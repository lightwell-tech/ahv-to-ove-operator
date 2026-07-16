package controllers

import (
	"testing"

	migrationv1alpha1 "github.com/lightwell-tech/ahv-to-ove-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ─── dvName ─────────────────────────────────────────────────────────────────

func TestDvName_Short(t *testing.T) {
	got := dvName("my-migration", 0, 1)
	want := "dv-my-migration-0-1"
	if got != want {
		t.Errorf("dvName() = %q, want %q", got, want)
	}
}

func TestDvName_TruncatesAt40(t *testing.T) {
	longName := "this-is-a-very-long-migration-name-that-exceeds-forty-characters"
	got := dvName(longName, 2, 3)
	// prefix は最大40文字
	if len(got) > len("dv-")+40+len("-2-3") {
		t.Errorf("dvName() too long: %q (len=%d)", got, len(got))
	}
	if got[:3] != "dv-" {
		t.Errorf("dvName() should start with 'dv-', got %q", got)
	}
}

// ─── targetNS ───────────────────────────────────────────────────────────────

func TestTargetNS_UsesTargetNamespace(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec:       migrationv1alpha1.AHVMigrationSpec{TargetNamespace: "vm-migration"},
	}
	if got := targetNS(mig); got != "vm-migration" {
		t.Errorf("targetNS() = %q, want %q", got, "vm-migration")
	}
}

func TestTargetNS_FallsBackToObjectNamespace(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ahv-to-ove-operator-system"},
	}
	if got := targetNS(mig); got != "ahv-to-ove-operator-system" {
		t.Errorf("targetNS() = %q, want %q", got, "ahv-to-ove-operator-system")
	}
}

// ─── needsGuestPrep ─────────────────────────────────────────────────────────

func TestNeedsGuestPrep_NoVMs(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{}
	if needsGuestPrep(mig) {
		t.Error("needsGuestPrep() = true, want false for empty VMs")
	}
}

func TestNeedsGuestPrep_NoGuestPrepMode(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		Spec: migrationv1alpha1.AHVMigrationSpec{
			VMs: []migrationv1alpha1.VMSpec{{Name: "vm1"}},
		},
	}
	if needsGuestPrep(mig) {
		t.Error("needsGuestPrep() = true, want false when guestPrepMode not set")
	}
}

func TestNeedsGuestPrep_SSHModeWithoutConfig(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		Spec: migrationv1alpha1.AHVMigrationSpec{
			VMs: []migrationv1alpha1.VMSpec{
				{Name: "vm1", GuestPrepMode: "ssh", GuestPrepConfig: nil},
			},
		},
	}
	if needsGuestPrep(mig) {
		t.Error("needsGuestPrep() = true, want false when GuestPrepConfig is nil")
	}
}

func TestNeedsGuestPrep_SSHModeWithConfig(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		Spec: migrationv1alpha1.AHVMigrationSpec{
			VMs: []migrationv1alpha1.VMSpec{
				{
					Name:          "vm1",
					GuestPrepMode: "ssh",
					GuestPrepConfig: &migrationv1alpha1.GuestPrepSSHConfig{
						SSHSecretRef:       corev1.LocalObjectReference{Name: "vm1-ssh"},
						PreMigrationScript: "dracut -f",
					},
				},
			},
		},
	}
	if !needsGuestPrep(mig) {
		t.Error("needsGuestPrep() = false, want true when ssh mode with config")
	}
}

func TestNeedsGuestPrep_MixedVMs_OnlyOneHasSSH(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		Spec: migrationv1alpha1.AHVMigrationSpec{
			VMs: []migrationv1alpha1.VMSpec{
				{Name: "vm1"},
				{
					Name:          "vm2",
					GuestPrepMode: "ssh",
					GuestPrepConfig: &migrationv1alpha1.GuestPrepSSHConfig{
						SSHSecretRef:       corev1.LocalObjectReference{Name: "vm2-ssh"},
						PreMigrationScript: "dracut -f",
					},
				},
			},
		},
	}
	if !needsGuestPrep(mig) {
		t.Error("needsGuestPrep() = false, want true when at least one VM has ssh mode")
	}
}

// ─── resolveNetwork ──────────────────────────────────────────────────────────

func TestResolveNetwork_MatchingSource(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		Spec: migrationv1alpha1.AHVMigrationSpec{
			NetworkMappings: []migrationv1alpha1.NetworkMapping{
				{Source: "VLAN_100", Target: "example-bridge", TestTarget: "test-bridge-001", TargetNamespace: "vm-migration"},
			},
		},
	}
	got := resolveNetwork(mig, "VLAN_100", false)
	if got != "vm-migration/example-bridge" {
		t.Errorf("resolveNetwork(prod) = %q, want vm-migration/example-bridge", got)
	}
	got = resolveNetwork(mig, "VLAN_100", true)
	if got != "vm-migration/test-bridge-001" {
		t.Errorf("resolveNetwork(test) = %q, want vm-migration/test-bridge-001", got)
	}
}

func TestResolveNetwork_FallbackToFirstMapping(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		Spec: migrationv1alpha1.AHVMigrationSpec{
			NetworkMappings: []migrationv1alpha1.NetworkMapping{
				{Source: "VLAN_100", Target: "example-bridge", TestTarget: "test-bridge-001", TargetNamespace: "vm-migration"},
			},
		},
	}
	// source が一致しない場合は最初のマッピングで target/testTarget を使う
	got := resolveNetwork(mig, "UNKNOWN_VLAN", false)
	if got != "vm-migration/example-bridge" {
		t.Errorf("resolveNetwork(fallback prod) = %q, want vm-migration/example-bridge", got)
	}
	got = resolveNetwork(mig, "UNKNOWN_VLAN", true)
	if got != "vm-migration/test-bridge-001" {
		t.Errorf("resolveNetwork(fallback test) = %q, want vm-migration/test-bridge-001", got)
	}
}

func TestResolveNetwork_NoMappings(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{}
	got := resolveNetwork(mig, "VLAN_100", true)
	if got != "example-bridge" {
		t.Errorf("resolveNetwork(no mappings) = %q, want example-bridge", got)
	}
}

func TestResolveNetwork_TestTargetEmpty_FallsBackToProd(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		Spec: migrationv1alpha1.AHVMigrationSpec{
			NetworkMappings: []migrationv1alpha1.NetworkMapping{
				{Source: "VLAN_100", Target: "example-bridge", TargetNamespace: "vm-migration"},
			},
		},
	}
	// testTarget が空でも prod target を使う
	got := resolveNetwork(mig, "VLAN_100", true)
	if got != "vm-migration/example-bridge" {
		t.Errorf("resolveNetwork(test, no testTarget) = %q, want vm-migration/example-bridge", got)
	}
}

// ─── replacePrismURLWithProxy ────────────────────────────────────────────────

func TestReplacePrismURLWithProxy_Basic(t *testing.T) {
	sourceURL := "https://prism-central.example.com:9440/api/nutanix/v3/images/abc-123/file"
	endpoint := "https://prism-central.example.com:9440"
	proxy := "http://prism-proxy.vm-migration.svc:9440"

	got := replacePrismURLWithProxy(sourceURL, endpoint, proxy)
	want := "http://prism-proxy.vm-migration.svc:9440/api/nutanix/v3/images/abc-123/file"
	if got != want {
		t.Errorf("replacePrismURLWithProxy() = %q, want %q", got, want)
	}
}

func TestReplacePrismURLWithProxy_NoMatch(t *testing.T) {
	sourceURL := "https://other-host:9440/api/nutanix/v3/images/abc-123/file"
	endpoint := "https://prism-central.example.com:9440"
	proxy := "http://prism-proxy.vm-migration.svc:9440"

	got := replacePrismURLWithProxy(sourceURL, endpoint, proxy)
	if got != sourceURL {
		t.Errorf("replacePrismURLWithProxy() should not modify non-matching URL, got %q", got)
	}
}

func TestReplacePrismURLWithProxy_EmptyProxy(t *testing.T) {
	sourceURL := "https://prism-central.example.com:9440/api/nutanix/v3/images/abc-123/file"
	endpoint := "https://prism-central.example.com:9440"

	got := replacePrismURLWithProxy(sourceURL, endpoint, "")
	if got != sourceURL {
		t.Errorf("replacePrismURLWithProxy() should return original URL when proxy is empty, got %q", got)
	}
}

func TestReplacePrismURLWithProxy_TrailingSlash(t *testing.T) {
	sourceURL := "https://prism-central.example.com:9440/api/path"
	endpoint := "https://prism-central.example.com:9440"
	proxy := "http://proxy.svc:9440/"

	got := replacePrismURLWithProxy(sourceURL, endpoint, proxy)
	want := "http://proxy.svc:9440/api/path"
	if got != want {
		t.Errorf("replacePrismURLWithProxy() = %q, want %q", got, want)
	}
}

// ─── resolveStorage ──────────────────────────────────────────────────────────

func TestResolveStorage_ExplicitMapping(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		Spec: migrationv1alpha1.AHVMigrationSpec{
			StorageMappings: []migrationv1alpha1.StorageMapping{
				{
					Source:             "",
					TargetStorageClass: "my-storageclass",
					AccessMode:         corev1.ReadWriteOnce,
					VolumeMode:         corev1.PersistentVolumeFilesystem,
				},
			},
		},
	}
	sc, am, vm := resolveStorage(mig, "")
	if sc != "my-storageclass" {
		t.Errorf("storageClass = %q, want %q", sc, "my-storageclass")
	}
	if am != corev1.ReadWriteOnce {
		t.Errorf("accessMode = %q, want ReadWriteOnce", am)
	}
	if vm != corev1.PersistentVolumeFilesystem {
		t.Errorf("volumeMode = %q, want Filesystem", vm)
	}
}

func TestResolveStorage_DefaultAccessModeAndVolumeMode(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		Spec: migrationv1alpha1.AHVMigrationSpec{
			StorageMappings: []migrationv1alpha1.StorageMapping{
				{Source: "", TargetStorageClass: "test-sc"},
			},
		},
	}
	_, am, vm := resolveStorage(mig, "")
	if am != corev1.ReadWriteMany {
		t.Errorf("accessMode default = %q, want ReadWriteMany", am)
	}
	if vm != corev1.PersistentVolumeBlock {
		t.Errorf("volumeMode default = %q, want Block", vm)
	}
}

func TestResolveStorage_FallbackWhenNoMappings(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{}
	sc, am, vm := resolveStorage(mig, "")
	if sc != "ocs-storagecluster-ceph-rbd" {
		t.Errorf("fallback storageClass = %q, want ocs-storagecluster-ceph-rbd", sc)
	}
	if am != corev1.ReadWriteMany {
		t.Errorf("fallback accessMode = %q, want ReadWriteMany", am)
	}
	if vm != corev1.PersistentVolumeBlock {
		t.Errorf("fallback volumeMode = %q, want Block", vm)
	}
}

// ─── isDVSucceeded ────────────────────────────────────────────────────────────

func TestIsDVSucceeded_True(t *testing.T) {
	dv := makeDV("Succeeded", "100")
	if !isDVSucceeded(dv) {
		t.Error("isDVSucceeded() = false, want true for Succeeded phase")
	}
}

func TestIsDVSucceeded_False(t *testing.T) {
	dv := makeDV("ImportInProgress", "50")
	if isDVSucceeded(dv) {
		t.Error("isDVSucceeded() = true, want false for ImportInProgress")
	}
}

func TestIsDVSucceeded_Pending(t *testing.T) {
	dv := makeDV("Pending", "")
	if isDVSucceeded(dv) {
		t.Error("isDVSucceeded() = true, want false for Pending")
	}
}

// ─── dvProgress ──────────────────────────────────────────────────────────────

func TestDvProgress_Parses(t *testing.T) {
	dv := makeDV("ImportInProgress", "75")
	if got := dvProgress(dv); got != 75 {
		t.Errorf("dvProgress() = %d, want 75", got)
	}
}

func TestDvProgress_EmptyIsZero(t *testing.T) {
	dv := makeDV("Pending", "")
	if got := dvProgress(dv); got != 0 {
		t.Errorf("dvProgress() = %d, want 0 for empty progress", got)
	}
}

func TestDvProgress_IgnoresPercentSign(t *testing.T) {
	dv := makeDV("ImportInProgress", "58")
	if got := dvProgress(dv); got != 58 {
		t.Errorf("dvProgress() = %d, want 58", got)
	}
}

// ─── buildCDIAuthSecret ──────────────────────────────────────────────────────

func TestBuildCDIAuthSecret_NameAndNamespace(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "test-mig", Namespace: "ahv-to-ove-operator-system"},
		Spec:       migrationv1alpha1.AHVMigrationSpec{TargetNamespace: "vm-migration"},
	}
	secret := buildCDIAuthSecret(mig, "admin", "P@ssw0rd")

	if secret.Name != "test-mig-prism-auth" {
		t.Errorf("secret name = %q, want %q", secret.Name, "test-mig-prism-auth")
	}
	if secret.Namespace != "vm-migration" {
		t.Errorf("secret namespace = %q, want %q", secret.Namespace, "vm-migration")
	}
}

func TestBuildCDIAuthSecret_Credentials(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: "default"},
	}
	secret := buildCDIAuthSecret(mig, "admin", "secret123")

	if secret.StringData["accessKeyId"] != "admin" {
		t.Errorf("accessKeyId = %q, want admin", secret.StringData["accessKeyId"])
	}
	if secret.StringData["secretKey"] != "secret123" {
		t.Errorf("secretKey = %q, want secret123", secret.StringData["secretKey"])
	}
}

// ─── buildBlankDataVolume ────────────────────────────────────────────────────

func TestBuildBlankDataVolume_HasBlankSource(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "test-mig", Namespace: "default"},
		Spec: migrationv1alpha1.AHVMigrationSpec{
			StorageMappings: []migrationv1alpha1.StorageMapping{
				{Source: "", TargetStorageClass: "test-sc"},
			},
		},
	}
	disk := DiskInfo{Index: 1, UUID: "disk-uuid-abc", SizeMB: 40960}
	dv := buildBlankDataVolume(mig, 0, disk)

	spec, ok := dv.Object["spec"].(map[string]interface{})
	if !ok {
		t.Fatal("spec not found in DataVolume")
	}
	src, ok := spec["source"].(map[string]interface{})
	if !ok {
		t.Fatal("source not found in spec")
	}
	if _, ok := src["blank"]; !ok {
		t.Error("blank source not found — expected source.blank to be set")
	}
}

func TestBuildBlankDataVolume_Name(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "test-mig", Namespace: "default"},
	}
	disk := DiskInfo{Index: 1, UUID: "disk-uuid", SizeMB: 40960}
	dv := buildBlankDataVolume(mig, 0, disk)

	if got := dv.GetName(); got != "dv-test-mig-0-1" {
		t.Errorf("DataVolume name = %q, want %q", got, "dv-test-mig-0-1")
	}
}

func TestBuildBlankDataVolume_SourceDiskAnnotation(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
	}
	disk := DiskInfo{Index: 0, UUID: "my-disk-uuid", SizeMB: 1024}
	dv := buildBlankDataVolume(mig, 0, disk)

	annotations := dv.GetAnnotations()
	if annotations["migration.lightwell.co.jp/source-disk-uuid"] != "my-disk-uuid" {
		t.Errorf("source-disk-uuid annotation = %q, want %q",
			annotations["migration.lightwell.co.jp/source-disk-uuid"], "my-disk-uuid")
	}
}

// ─── buildDataVolume ─────────────────────────────────────────────────────────

func TestBuildDataVolume_HasHTTPSource(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "mig", Namespace: "default"},
		Spec: migrationv1alpha1.AHVMigrationSpec{
			StorageMappings: []migrationv1alpha1.StorageMapping{
				{Source: "", TargetStorageClass: "ceph-rbd"},
			},
		},
	}
	disk := DiskInfo{Index: 0, UUID: "uuid-1", SizeMB: 10240}
	sourceURL := "http://prism-proxy.vm-migration.svc:9440/api/nutanix/v3/images/uuid-1/file"

	dv := buildDataVolume(mig, 0, disk, sourceURL)

	spec := dv.Object["spec"].(map[string]interface{})
	src := spec["source"].(map[string]interface{})
	http, ok := src["http"].(map[string]interface{})
	if !ok {
		t.Fatal("http source not found")
	}
	if http["url"] != sourceURL {
		t.Errorf("http.url = %q, want %q", http["url"], sourceURL)
	}
}

func TestBuildDataVolume_Name(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "mymig", Namespace: "default"},
	}
	disk := DiskInfo{Index: 2, UUID: "u", SizeMB: 1024}
	dv := buildDataVolume(mig, 1, disk, "http://example.com/disk")

	if got := dv.GetName(); got != "dv-mymig-1-2" {
		t.Errorf("DataVolume name = %q, want %q", got, "dv-mymig-1-2")
	}
}

// ─── helper ──────────────────────────────────────────────────────────────────

func makeDV(phase, progress string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetName("test-dv")
	obj.Object["status"] = map[string]interface{}{
		"phase":    phase,
		"progress": progress,
	}
	return obj
}

func TestWarmFinalSyncEnabled(t *testing.T) {
	f := false
	tr := true
	cases := []struct {
		name string
		val  *bool
		want bool
	}{
		{"未指定はデフォルト有効", nil, true},
		{"明示true", &tr, true},
		{"明示false", &f, false},
	}
	for _, c := range cases {
		mig := &migrationv1alpha1.AHVMigration{}
		mig.Spec.Source.WarmFinalFullSync = c.val
		if got := warmFinalSyncEnabled(mig); got != c.want {
			t.Errorf("%s: warmFinalSyncEnabled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDVStorageSize_Overhead(t *testing.T) {
	// 10%オーバーヘッド: 51200Mi (50GiB) → 56320Mi = 55Gi に正規化される
	q := dvStorageSize(51200)
	if q.String() != "55Gi" {
		t.Errorf("dvStorageSize(51200) = %s, want 55Gi", q.String())
	}
}

func TestMergeRegions(t *testing.T) {
	regions := []ChangedRegion{
		{Offset: 0, Length: 1024, Type: "REGULAR"},
		{Offset: 2048, Length: 1024, Type: "REGULAR"},            // gap 1024 → 結合される
		{Offset: 10 * 1024 * 1024, Length: 512, Type: "REGULAR"}, // gap大 → 別リージョン
		{Offset: 20 * 1024 * 1024, Length: 4096, Type: "ZEROED"}, // type違い → 別リージョン
	}
	merged := mergeRegions(regions, 4*1024*1024)
	if len(merged) != 3 {
		t.Fatalf("merged len = %d, want 3: %+v", len(merged), merged)
	}
	if merged[0].Offset != 0 || merged[0].Length != 3072 {
		t.Errorf("merged[0] = %+v, want {0 3072 REGULAR}", merged[0])
	}
	if merged[2].Type != "ZEROED" {
		t.Errorf("merged[2].Type = %s, want ZEROED", merged[2].Type)
	}
}

func TestPrepareRegions_CompressesToLimit(t *testing.T) {
	// 4MiB 超のギャップで並ぶ 30000 リージョン → ギャップ拡大で 20000 以下に圧縮される
	regions := make([]ChangedRegion, 30000)
	for i := range regions {
		regions[i] = ChangedRegion{Offset: int64(i) * 8 * 1024 * 1024, Length: 4096, Type: "REGULAR"}
	}
	merged := prepareRegions(regions)
	if len(merged) > maxRegionsPerJob {
		t.Errorf("prepareRegions len = %d, want <= %d", len(merged), maxRegionsPerJob)
	}
	// 総カバレッジは元リージョンを全て包含していること（先頭と末尾で確認）
	first, last := merged[0], merged[len(merged)-1]
	if first.Offset > 0 {
		t.Errorf("first.Offset = %d, want 0", first.Offset)
	}
	wantEnd := regions[len(regions)-1].Offset + regions[len(regions)-1].Length
	if last.Offset+last.Length < wantEnd {
		t.Errorf("coverage end = %d, want >= %d", last.Offset+last.Length, wantEnd)
	}
}

func TestCBTEnabled(t *testing.T) {
	mig := &migrationv1alpha1.AHVMigration{}
	if cbtEnabled(mig) {
		t.Error("cbtEnabled = true for nil CBT, want false")
	}
	mig.Spec.Source.CBT = &migrationv1alpha1.CBTConfig{Enabled: true}
	if !cbtEnabled(mig) {
		t.Error("cbtEnabled = false, want true")
	}
	if deltaSyncThresholdBytes(mig) != 512*1024*1024 {
		t.Errorf("default threshold = %d, want 512MiB", deltaSyncThresholdBytes(mig))
	}
	if maxDeltaRounds(mig) != 10 {
		t.Errorf("default maxRounds = %d, want 10", maxDeltaRounds(mig))
	}
}
