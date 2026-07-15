package controllers

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/lightwell-tech/ahv-to-ove-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
)

// PrismClient は Prism Central REST API v3 クライアント
type PrismClient struct {
	baseURL  string
	username string
	password string
	http     *http.Client
	// pe は Prism Element 専用クライアント（電源操作に使用）。nil の場合はフォールバック動作。
	pe *PrismClient
}

// VMInfo は Prism API から取得した VM のメタデータ
type VMInfo struct {
	UUID       string
	Name       string
	NumCPUs    int64
	MemoryMB   int64
	DiskList   []DiskInfo
	NICList    []NICInfo
	IsUEFI     bool   // Prism の boot_config が UEFI、または EFI System パーティション検出時に true
}

// DiskInfo は個々のディスク情報
type DiskInfo struct {
	Index      int
	UUID       string // image UUID（image backing）または disk UUID（bare disk）
	SizeMB     int64
	DeviceType string // "DISK" or "CDROM"
	SourceURL  string // CDI DataVolume 用ダウンロード URL
	IsBare     bool   // true = image backing なし（直接プロビジョニングされた disk）
}

// NICInfo は NIC 情報
type NICInfo struct {
	NetworkName string
	MACAddress  string
}

// NewPrismClient は AHVMigration の spec から Prism クライアントを生成する。
// source.peEndpoint が指定されている場合は PE 専用サブクライアント（p.pe）も生成し、
// ShutdownVM 等の電源操作で PE v2 API を直接使用する。
func NewPrismClient(ctx context.Context, k8s client.Client, mig *migrationv1alpha1.AHVMigration) (*PrismClient, error) {
	secret := &corev1.Secret{}
	ns := mig.Namespace
	if err := k8s.Get(ctx, types.NamespacedName{Name: mig.Spec.Source.SecretRef.Name, Namespace: ns}, secret); err != nil {
		return nil, fmt.Errorf("secret %s not found: %w", mig.Spec.Source.SecretRef.Name, err)
	}

	user := string(secret.Data["user"])
	pass := string(secret.Data["password"])
	if user == "" || pass == "" {
		return nil, fmt.Errorf("secret %s must have 'user' and 'password' keys", mig.Spec.Source.SecretRef.Name)
	}

	tlsCfg := &tls.Config{}
	if mig.Spec.Source.Insecure {
		tlsCfg.InsecureSkipVerify = true
	}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}

	pc := &PrismClient{
		baseURL:  strings.TrimRight(mig.Spec.Source.Endpoint, "/"),
		username: user,
		password: pass,
		http:     httpClient,
	}

	// PE endpoint が指定されている場合、電源操作用サブクライアントを生成
	if mig.Spec.Source.PEEndpoint != "" {
		peUser, pePass := user, pass
		if mig.Spec.Source.PESecretRef != nil {
			peSecret := &corev1.Secret{}
			if err := k8s.Get(ctx, types.NamespacedName{Name: mig.Spec.Source.PESecretRef.Name, Namespace: ns}, peSecret); err != nil {
				return nil, fmt.Errorf("PE secret %s not found: %w", mig.Spec.Source.PESecretRef.Name, err)
			}
			peUser = string(peSecret.Data["user"])
			pePass = string(peSecret.Data["password"])
			if peUser == "" || pePass == "" {
				return nil, fmt.Errorf("PE secret %s must have 'user' and 'password' keys", mig.Spec.Source.PESecretRef.Name)
			}
		}
		pc.pe = &PrismClient{
			baseURL:  strings.TrimRight(mig.Spec.Source.PEEndpoint, "/"),
			username: peUser,
			password: pePass,
			http:     httpClient,
		}
	}

	return pc, nil
}

func (p *PrismClient) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(p.username, p.password)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Prism API %s %s: HTTP %d: %s", method, path, resp.StatusCode, string(data[:min(200, len(data))]))
	}
	return data, nil
}

// GetVMByName は VM 名で検索して VMInfo を返す
func (p *PrismClient) GetVMByName(ctx context.Context, name string) (*VMInfo, error) {
	body := map[string]interface{}{
		"kind": "vm",
		"filter": fmt.Sprintf("vm_name==%s", name),
	}

	data, err := p.doRequest(ctx, "POST", "/api/nutanix/v3/vms/list", body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Entities []map[string]interface{} `json:"entities"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse VMs list: %w", err)
	}

	for _, ent := range result.Entities {
		spec, _ := ent["spec"].(map[string]interface{})
		metadata, _ := ent["metadata"].(map[string]interface{})
		vmName, _ := spec["name"].(string)
		if vmName != name {
			continue
		}
		uuid, _ := metadata["uuid"].(string)
		return p.GetVMByUUID(ctx, uuid)
	}

	return nil, fmt.Errorf("VM %q not found in Prism", name)
}

// GetVMByUUID は UUID で VM 情報を取得する
func (p *PrismClient) GetVMByUUID(ctx context.Context, uuid string) (*VMInfo, error) {
	data, err := p.doRequest(ctx, "GET", "/api/nutanix/v3/vms/"+uuid, nil)
	if err != nil {
		return nil, err
	}

	var vm map[string]interface{}
	if err := json.Unmarshal(data, &vm); err != nil {
		return nil, fmt.Errorf("parse VM: %w", err)
	}

	info := &VMInfo{UUID: uuid}

	spec, _ := vm["spec"].(map[string]interface{})
	info.Name, _ = spec["name"].(string)

	resources, _ := spec["resources"].(map[string]interface{})
	if v, ok := resources["num_vcpus_per_socket"].(float64); ok {
		info.NumCPUs = int64(v)
	}
	if sockets, ok := resources["num_sockets"].(float64); ok {
		info.NumCPUs *= int64(sockets)
	}
	if mem, ok := resources["memory_size_mib"].(float64); ok {
		info.MemoryMB = int64(mem)
	}
	if info.NumCPUs == 0 {
		info.NumCPUs = 1
	}
	if info.MemoryMB == 0 {
		info.MemoryMB = 2048
	}

	// ディスクリスト
	diskList, _ := resources["disk_list"].([]interface{})
	for _, d := range diskList {
		disk, _ := d.(map[string]interface{})
		dp, _ := disk["device_properties"].(map[string]interface{})
		deviceType, _ := dp["device_type"].(string)
		if deviceType == "CDROM" {
			continue
		}

		diskInfo := DiskInfo{
			Index:      len(info.DiskList), // CDROMを除いた連番（0,1,2...）
			DeviceType: deviceType,
		}

		// サイズ
		if sz, ok := disk["disk_size_mib"].(float64); ok {
			diskInfo.SizeMB = int64(sz)
		}
		if diskInfo.SizeMB == 0 {
			diskInfo.SizeMB = 20480 // デフォルト 20GB
		}

		// バッキングイメージ UUID（v3 images catalog から参照する場合）
		dataSourceRef, _ := disk["data_source_reference"].(map[string]interface{})
		if dataSourceRef != nil {
			imgUUID, _ := dataSourceRef["uuid"].(string)
			diskInfo.UUID = imgUUID
			if imgUUID != "" {
				// SourceURL は後でコントローラーが CDIProxyURL で上書きする
				diskInfo.SourceURL = fmt.Sprintf("%s/api/nutanix/v3/images/%s/file",
					p.baseURL, imgUUID)
			}
		}

		// Bare disk（image backing なし）: disk.uuid で v0.8 vdisk download URL を試みる
		// Nutanix v0.8 API: GET /api/nutanix/v0.8/vdisks/{diskUUID}
		if diskInfo.SourceURL == "" {
			diskUUID, _ := disk["uuid"].(string)
			if diskUUID != "" {
				diskInfo.UUID = diskUUID
				// vdisk UUID は v3 の disk.uuid。v0.8 の vdisk name は別形式だが
				// Prism Central 経由で "nfs://disk_uuid" 形式が使えることがある。
				// ここでは disk UUID をメタデータとして保持し、コントローラーが
				// image 化ステップを判断できるようにする。
				diskInfo.IsBare = true
			}
		}

		info.DiskList = append(info.DiskList, diskInfo)
	}

	// NIC リスト
	nicList, _ := resources["nic_list"].([]interface{})
	for _, n := range nicList {
		nic, _ := n.(map[string]interface{})
		subnetRef, _ := nic["subnet_reference"].(map[string]interface{})
		subnetName, _ := subnetRef["name"].(string)
		mac, _ := nic["mac_address"].(string)
		info.NICList = append(info.NICList, NICInfo{
			NetworkName: subnetName,
			MACAddress:  mac,
		})
	}

	// UEFI 自動検出: Prism v3 の spec.resources.boot_config.boot_type が "UEFI" なら true
	if bootCfg, ok := resources["boot_config"].(map[string]interface{}); ok {
		bootType, _ := bootCfg["boot_type"].(string)
		info.IsUEFI = strings.EqualFold(bootType, "UEFI")
	}

	return info, nil
}

// ChangedRegion は data/changed_regions API が返す変更リージョン
type ChangedRegion struct {
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Type   string `json:"type"` // REGULAR | ZEROED
}

// CreateVMSnapshot は v3 vm_snapshots で crash-consistent スナップショットを作成し、
// COMPLETE まで待って snapshot UUID を返す
func (p *PrismClient) CreateVMSnapshot(ctx context.Context, vmUUID, name string) (string, error) {
	body := map[string]interface{}{
		"spec": map[string]interface{}{
			"name":      name,
			"resources": map[string]interface{}{"entity_uuid": vmUUID},
		},
		"metadata": map[string]interface{}{"kind": "vm_snapshot"},
	}
	data, err := p.doRequest(ctx, "POST", "/api/nutanix/v3/vm_snapshots", body)
	if err != nil {
		return "", fmt.Errorf("CreateVMSnapshot %s: %w", vmUUID, err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	snapUUID, _, _ := unstructuredNestedString(resp, "metadata", "uuid")
	if snapUUID == "" {
		return "", fmt.Errorf("CreateVMSnapshot: no uuid in response: %s", string(data[:min(200, len(data))]))
	}
	// COMPLETE 待ち（10秒 × 60回）
	for attempt := 0; attempt < 60; attempt++ {
		data, err := p.doRequest(ctx, "GET", "/api/nutanix/v3/vm_snapshots/"+snapUUID, nil)
		if err != nil {
			return "", err
		}
		var snap map[string]interface{}
		if err := json.Unmarshal(data, &snap); err != nil {
			return "", err
		}
		state, _, _ := unstructuredNestedString(snap, "status", "state")
		if state == "COMPLETE" {
			return snapUUID, nil
		}
		if state == "ERROR" {
			return "", fmt.Errorf("vm_snapshot %s in ERROR state", snapUUID)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return "", fmt.Errorf("vm_snapshot %s not COMPLETE after 60 attempts", snapUUID)
}

// GetVMSnapshotPaths は snapshot の snapshot_file_path をディスク順で返す。
// v3 vm_snapshots の GET は vm_create_spec を含まないため、snapshot_file_list の各 vmdisk UUID を
// v2 virtual_disks で照会し「vmUUID にアタッチされた DISK」だけを disk_address (bus.index) 順に採用する。
// CDROM 由来や別 VM のファイル（snapshot_file_list に混入することがある）は自然に除外される。
// snapshot_file_path は data/changed_regions が受け付ける .snapshot/ 名前空間のパス
func (p *PrismClient) GetVMSnapshotPaths(ctx context.Context, snapUUID, vmUUID string) ([]string, error) {
	data, err := p.doRequest(ctx, "GET", "/api/nutanix/v3/vm_snapshots/"+snapUUID, nil)
	if err != nil {
		return nil, err
	}
	var snap struct {
		Status struct {
			SnapshotFileList []struct {
				FilePath         string `json:"file_path"`
				SnapshotFilePath string `json:"snapshot_file_path"`
			} `json:"snapshot_file_list"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("parse vm_snapshot %s: %w", snapUUID, err)
	}

	type diskPath struct {
		bus   string
		index int
		path  string
	}
	found := make([]diskPath, 0)
	for _, f := range snap.Status.SnapshotFileList {
		parts := strings.Split(f.FilePath, "/")
		if len(parts) == 0 {
			continue
		}
		vdiskUUID := parts[len(parts)-1]

		vdData, err := p.doRequest(ctx, "GET", "/api/nutanix/v2.0/virtual_disks/"+vdiskUUID, nil)
		if err != nil {
			// アタッチされていない vdisk（CDROM ISO 等）は照会に失敗する → スキップ
			continue
		}
		var vd struct {
			AttachedVMUUID string `json:"attached_vm_uuid"`
			DiskAddress    string `json:"disk_address"` // 例: "scsi.0"
		}
		if err := json.Unmarshal(vdData, &vd); err != nil {
			continue
		}
		if vd.AttachedVMUUID != vmUUID || vd.DiskAddress == "" {
			continue
		}
		bus, idxStr, ok := strings.Cut(vd.DiskAddress, ".")
		if !ok {
			continue
		}
		var idx int
		fmt.Sscanf(idxStr, "%d", &idx)
		found = append(found, diskPath{bus: bus, index: idx, path: f.SnapshotFilePath})
	}

	sort.SliceStable(found, func(i, j int) bool {
		if found[i].bus != found[j].bus {
			return found[i].bus < found[j].bus
		}
		return found[i].index < found[j].index
	})
	paths := make([]string, 0, len(found))
	for _, d := range found {
		paths = append(paths, d.path)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("vm_snapshot %s: no disk snapshot paths found for vm %s", snapUUID, vmUUID)
	}
	return paths, nil
}

// DeleteVMSnapshot は v3 vm_snapshot を削除する（best-effort 用途）
func (p *PrismClient) DeleteVMSnapshot(ctx context.Context, snapUUID string) error {
	_, err := p.doRequest(ctx, "DELETE", "/api/nutanix/v3/vm_snapshots/"+snapUUID, nil)
	return err
}

// ChangedRegions は 2 つの snapshot_file_path 間の変更リージョンを返す。
// refPath が空の場合は全割当マップ（ZEROED 含む）を返す。
// 戻り値: リージョンリスト, ディスク実サイズ(bytes), error
func (p *PrismClient) ChangedRegions(ctx context.Context, path, refPath string) ([]ChangedRegion, int64, error) {
	body := map[string]interface{}{"snapshot_file_path": path}
	if refPath != "" {
		body["reference_snapshot_file_path"] = refPath
	}
	data, err := p.doRequest(ctx, "POST", "/api/nutanix/v3/data/changed_regions", body)
	if err != nil {
		return nil, 0, fmt.Errorf("ChangedRegions %s: %w", path, err)
	}
	var resp struct {
		FileSize   int64           `json:"file_size"`
		RegionList []ChangedRegion `json:"region_list"`
		NextOffset int64           `json:"next_offset"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, 0, fmt.Errorf("parse changed_regions: %w", err)
	}
	// next_offset によるページングは AOS 7.5 では未確認。返ってきた場合は
	// 取りこぼし防止のためエラーにする（上位でフルコピーへフォールバック判断）
	if resp.NextOffset > 0 {
		return nil, 0, fmt.Errorf("changed_regions returned next_offset=%d (pagination not supported)", resp.NextOffset)
	}
	return resp.RegionList, resp.FileSize, nil
}

// GetImageSizeBytes は v3 images API から image の実サイズ (bytes) を取得する
func (p *PrismClient) GetImageSizeBytes(ctx context.Context, imageUUID string) (int64, error) {
	data, err := p.doRequest(ctx, "GET", "/api/nutanix/v3/images/"+imageUUID, nil)
	if err != nil {
		return 0, err
	}
	var img struct {
		Status struct {
			Resources struct {
				SizeBytes int64 `json:"size_bytes"`
			} `json:"resources"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &img); err != nil {
		return 0, fmt.Errorf("parse image %s: %w", imageUUID, err)
	}
	if img.Status.Resources.SizeBytes == 0 {
		return 0, fmt.Errorf("image %s has no size_bytes", imageUUID)
	}
	return img.Status.Resources.SizeBytes, nil
}

// DiskImageURL はディスクのダウンロード URL を返す（CDI 用）
func (p *PrismClient) DiskImageURL(imageUUID string) string {
	return fmt.Sprintf("%s/api/nutanix/v3/images/%s/file", p.baseURL, imageUUID)
}

// Credentials は CDI の secretRef 用 Basic Auth 情報を返す
func (p *PrismClient) Credentials() (user, pass string) {
	return p.username, p.password
}

// ShutdownVM は VM の電源を切る。
// pe サブクライアントがある場合（peEndpoint 指定時）: PE v2 set_power_state を直接使用。
// pe がない場合: PC v3 intent PUT → 405 時に v2 set_power_state へフォールバック。
func (p *PrismClient) ShutdownVM(ctx context.Context, vmUUID string) error {
	state, err := p.GetVMPowerState(ctx, vmUUID)
	if err != nil {
		return fmt.Errorf("ShutdownVM: GetVMPowerState %s: %w", vmUUID, err)
	}
	if state == "OFF" {
		return nil
	}

	// PE クライアントが明示設定されている場合は v2 API を直接使用
	if p.pe != nil {
		body := map[string]string{"transition": "ACPI_SHUTDOWN"}
		if _, err := p.pe.doRequest(ctx, "POST", "/api/nutanix/v2.0/vms/"+vmUUID+"/set_power_state", body); err != nil {
			return fmt.Errorf("ShutdownVM %s: PE v2 set_power_state failed: %w", vmUUID, err)
		}
		return nil
	}

	// PC v3: intent document を GET して power_state=OFF で PUT
	data, err := p.doRequest(ctx, "GET", "/api/nutanix/v3/vms/"+vmUUID, nil)
	if err != nil {
		return fmt.Errorf("ShutdownVM: GET VM %s: %w", vmUUID, err)
	}
	var vm map[string]interface{}
	if err := json.Unmarshal(data, &vm); err != nil {
		return fmt.Errorf("ShutdownVM: parse VM: %w", err)
	}
	delete(vm, "status")
	spec, _ := vm["spec"].(map[string]interface{})
	if spec == nil {
		spec = map[string]interface{}{}
		vm["spec"] = spec
	}
	resources, _ := spec["resources"].(map[string]interface{})
	if resources == nil {
		resources = map[string]interface{}{}
		spec["resources"] = resources
	}
	resources["power_state"] = "OFF"

	_, putErr := p.doRequest(ctx, "PUT", "/api/nutanix/v3/vms/"+vmUUID, vm)
	if putErr == nil {
		return nil
	}

	// v3 PUT 未対応（PE を endpoint に指定している場合など）→ v2 フォールバック
	if strings.Contains(putErr.Error(), "405") || strings.Contains(putErr.Error(), "REQUEST_NOT_SUPPORTED") {
		body := map[string]string{"transition": "ACPI_SHUTDOWN"}
		if _, err2 := p.doRequest(ctx, "POST", "/api/nutanix/v2.0/vms/"+vmUUID+"/set_power_state", body); err2 != nil {
			return fmt.Errorf("ShutdownVM %s: v2 set_power_state failed: %w", vmUUID, err2)
		}
		return nil
	}
	return fmt.Errorf("ShutdownVM %s: PUT failed: %w", vmUUID, putErr)
}

// WaitVMOff は VM の power_state が OFF になるまでポーリングする
func (p *PrismClient) WaitVMOff(ctx context.Context, vmUUID string, maxWait int) error {
	for i := 0; i < maxWait; i++ {
		data, err := p.doRequest(ctx, "GET", "/api/nutanix/v3/vms/"+vmUUID, nil)
		if err != nil {
			return err
		}
		var vm map[string]interface{}
		if err := json.Unmarshal(data, &vm); err != nil {
			return err
		}
		state, _, _ := unstructuredNestedString(vm, "status", "resources", "power_state")
		if state == "OFF" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// 10秒待機
		}
	}
	return fmt.Errorf("VM %s did not power off after %d attempts", vmUUID, maxWait)
}

// GetVMPowerState は VM の現在の電源状態を返す（"ON" / "OFF"）
func (p *PrismClient) GetVMPowerState(ctx context.Context, vmUUID string) (string, error) {
	data, err := p.doRequest(ctx, "GET", "/api/nutanix/v3/vms/"+vmUUID, nil)
	if err != nil {
		return "", err
	}
	var vm map[string]interface{}
	if err := json.Unmarshal(data, &vm); err != nil {
		return "", err
	}
	state, _, _ := unstructuredNestedString(vm, "status", "resources", "power_state")
	return state, nil
}

// CreateImageFromDisk は VM ディスク UUID から Prism image を作成し、image UUID を返す
// diskUUID: VM の disk_list[i].uuid（bare disk の vdisk UUID）
func (p *PrismClient) CreateImageFromDisk(ctx context.Context, diskUUID, imageName string) (string, error) {
	body := map[string]interface{}{
		"metadata": map[string]interface{}{
			"kind": "image",
		},
		"spec": map[string]interface{}{
			"name": imageName,
			"resources": map[string]interface{}{
				"image_type": "DISK_IMAGE",
				"data_source_reference": map[string]interface{}{
					"kind": "vm_disk",
					"uuid": diskUUID,
				},
			},
		},
	}
	data, err := p.doRequest(ctx, "POST", "/api/nutanix/v3/images", body)
	if err != nil {
		return "", fmt.Errorf("CreateImageFromDisk %s: %w", diskUUID, err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	// レスポンスは task_uuid 形式で非同期
	taskUUID, _, _ := unstructuredNestedString(resp, "status", "execution_context", "task_uuid")
	if taskUUID == "" {
		// metadata.uuid で返ってくることもある
		taskUUID, _, _ = unstructuredNestedString(resp, "metadata", "uuid")
	}
	if taskUUID == "" {
		return "", fmt.Errorf("CreateImageFromDisk: no task_uuid in response: %s", string(data[:min(200, len(data))]))
	}
	// タスク完了を待ち image UUID を取得
	return p.waitTaskAndGetImageUUID(ctx, taskUUID)
}

// waitTaskAndGetImageUUID はタスク完了を待ち、生成された image の UUID を返す
func (p *PrismClient) waitTaskAndGetImageUUID(ctx context.Context, taskUUID string) (string, error) {
	for attempt := 0; attempt < 60; attempt++ {
		data, err := p.doRequest(ctx, "GET", "/api/nutanix/v3/tasks/"+taskUUID, nil)
		if err != nil {
			return "", err
		}
		var task map[string]interface{}
		if err := json.Unmarshal(data, &task); err != nil {
			return "", err
		}
		status, _, _ := unstructuredNestedString(task, "status")
		if status == "SUCCEEDED" {
			// entity_reference_list[0].uuid が image UUID
			entities, _, _ := unstructuredNestedSlice(task, "entity_reference_list")
			for _, e := range entities {
				ent, ok := e.(map[string]interface{})
				if !ok {
					continue
				}
				if kind, _, _ := unstructuredNestedString(ent, "kind"); kind == "image" {
					imgUUID, _, _ := unstructuredNestedString(ent, "uuid")
					if imgUUID != "" {
						return imgUUID, nil
					}
				}
			}
			return "", fmt.Errorf("task succeeded but no image entity found")
		}
		if status == "FAILED" {
			msg, _, _ := unstructuredNestedString(task, "error_detail")
			return "", fmt.Errorf("image creation task failed: %s", msg)
		}
		// RUNNING / QUEUED → 10秒待ちリトライ
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
	}
	return "", fmt.Errorf("image creation timed out after 60 attempts (task: %s)", taskUUID)
}

// WaitImageReady は image の state が COMPLETE になるまでポーリングする
func (p *PrismClient) WaitImageReady(ctx context.Context, imageUUID string) error {
	for attempt := 0; attempt < 60; attempt++ {
		data, err := p.doRequest(ctx, "GET", "/api/nutanix/v3/images/"+imageUUID, nil)
		if err != nil {
			return err
		}
		var img map[string]interface{}
		if err := json.Unmarshal(data, &img); err != nil {
			return err
		}
		state, _, _ := unstructuredNestedString(img, "status", "state")
		if state == "COMPLETE" {
			return nil
		}
		if state == "ERROR" {
			msg, _, _ := unstructuredNestedString(img, "status", "message_list", "0", "message")
			return fmt.Errorf("image %s in ERROR state: %s", imageUUID, msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	return fmt.Errorf("image %s not ready after 60 attempts", imageUUID)
}

// DeleteImage は作成した一時イメージを削除する
func (p *PrismClient) DeleteImage(ctx context.Context, imageUUID string) error {
	_, err := p.doRequest(ctx, "DELETE", "/api/nutanix/v3/images/"+imageUUID, nil)
	return err
}

// unstructuredNestedString は map 階層から string 値を取得する
func unstructuredNestedString(obj map[string]interface{}, fields ...string) (string, bool, error) {
	cur := interface{}(obj)
	for _, f := range fields {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return "", false, nil
		}
		cur = m[f]
	}
	s, ok := cur.(string)
	return s, ok, nil
}

// unstructuredNestedSlice は map 階層から slice を取得する
func unstructuredNestedSlice(obj map[string]interface{}, fields ...string) ([]interface{}, bool, error) {
	cur := interface{}(obj)
	for _, f := range fields {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false, nil
		}
		cur = m[f]
	}
	s, ok := cur.([]interface{})
	return s, ok, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
