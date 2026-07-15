package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ahvmig
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="VMs",type="integer",JSONPath=".status.totalVMs"
// +kubebuilder:printcolumn:name="Completed",type="integer",JSONPath=".status.completedVMs"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AHVMigration は Nutanix AHV から OpenShift Virtualization (OVE) への VM 移行を表す
type AHVMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AHVMigrationSpec   `json:"spec,omitempty"`
	Status AHVMigrationStatus `json:"status,omitempty"`
}

type AHVMigrationSpec struct {
	// Source は移行元 AHV (Prism Central) の接続情報
	Source AHVSourceSpec `json:"source"`

	// VMs は移行対象 VM のリスト
	VMs []VMSpec `json:"vms"`

	// NetworkMappings は AHV ネットワーク → OVE NAD のマッピング
	NetworkMappings []NetworkMapping `json:"networkMappings,omitempty"`

	// StorageMappings は AHV ストレージ → OVE StorageClass のマッピング
	StorageMappings []StorageMapping `json:"storageMappings,omitempty"`

	// TargetNamespace は OVE 側の移行先 Namespace（省略時: AHVMigration と同じ ns）
	TargetNamespace string `json:"targetNamespace,omitempty"`
}

type AHVSourceSpec struct {
	// Endpoint は Prism Central の URL (例: https://prism-central.example.com:9440)
	Endpoint string `json:"endpoint"`

	// SecretRef は Prism Central の認証情報を持つ Secret の参照
	// Secret には user/password キーが必要
	SecretRef corev1.LocalObjectReference `json:"secretRef"`

	// Insecure が true の場合、TLS 証明書を検証しない
	// +optional
	Insecure bool `json:"insecure,omitempty"`

	// CDIProxyURL は CDI DataVolume 用の HTTP プロキシ URL
	// Prism の TLS 証明書が IP SAN なしの場合に使用する
	// 例: "http://prism-proxy.vm-migration.svc:9440"
	// +optional
	CDIProxyURL string `json:"cdiProxyURL,omitempty"`

	// PEEndpoint は Prism Element の URL (例: https://prism-element.example.com:9440)
	// 指定時は電源操作（ShutdownVM）に Prism Element v2 API を使用する。
	// PC v3 PUT が使えない場合（PE VM Put request not supported）に有効。
	// 省略時は endpoint への v3 PUT → 405 時に v2 フォールバックで対応する。
	// +optional
	PEEndpoint string `json:"peEndpoint,omitempty"`

	// PESecretRef は Prism Element の認証情報を持つ Secret の参照
	// Secret には user/password キーが必要。省略時は SecretRef を使用する。
	// +optional
	PESecretRef *corev1.LocalObjectReference `json:"peSecretRef,omitempty"`

	// ShutdownBeforeMigration が true の場合、移行前に VM を ACPI シャットダウンする
	// Bare disk（image backing なし）を正確にコピーするために必要
	// +optional
	ShutdownBeforeMigration bool `json:"shutdownBeforeMigration,omitempty"`

	// WarmMigration が true の場合、VM を起動したまま初回 image 化+CDI インポートを行い
	// 完了後に ACPI シャットダウンして VM を作成する（ダウンタイム最小化）
	// shutdownBeforeMigration より優先される
	// +optional
	WarmMigration bool `json:"warmMigration,omitempty"`

	// PauseBeforeCutover が true の場合、WarmSyncing 完了後に ReadyForCutover フェーズで停止する
	// 再開するには annotation: migration.lightwell.co.jp/cutover-approved=true を付与する
	// +optional
	PauseBeforeCutover bool `json:"pauseBeforeCutover,omitempty"`

	// WarmFinalFullSync が true（または省略）の場合、warm 移行の cutover（ソースVM停止）後に
	// ディスクを image 化し直してフルコピーをやり直す（WarmFinalSync フェーズ）。
	// プリコピー image 作成以降のゲスト書き込みを取りこぼさないための再同期（RPO=0 担保）。
	// false にするとプリコピーのみで VM を作成する（image 化以降の書き込みは失われる）。
	// cbt.enabled=true の場合は差分同期（WarmDeltaSync/WarmFinalDelta）が優先され、この設定は無視される。
	// +optional
	WarmFinalFullSync *bool `json:"warmFinalFullSync,omitempty"`

	// CBT は warm 移行の CBT（Changed Block Tracking）差分同期設定。
	// 有効にすると v3 vm_snapshots + data/changed_regions を使って差分だけを転送し、
	// cutover 時の停止時間を「最終差分の転送時間」まで短縮する。
	// +optional
	CBT *CBTConfig `json:"cbt,omitempty"`
}

// CBTConfig は CBT 差分同期の設定
type CBTConfig struct {
	// Enabled が true の場合、warm 移行で CBT 差分同期を行う
	Enabled bool `json:"enabled"`

	// DeltaSyncThresholdMB は「差分がこの値未満になったら収束」とみなす閾値（MB）。
	// 収束後は cutover 待ち（pauseBeforeCutover=true なら承認待ち）に進む。省略時 512。
	// +optional
	DeltaSyncThresholdMB int64 `json:"deltaSyncThresholdMB,omitempty"`

	// MaxDeltaRounds は cutover 前の差分同期ループの最大回数。省略時 10。
	// 上限到達時は差分が閾値超のままでも収束扱いにする（最終差分で必ず整合するため安全）。
	// +optional
	MaxDeltaRounds int32 `json:"maxDeltaRounds,omitempty"`

	// NFSServer は差分読み出しで snapshot のストレージコンテナを NFS マウントするサーバ。
	// 差分データは Prism v3 images/file が HTTP Range 非対応のため、snapshot_file_path の
	// コンテナを直接 NFS(RO) マウントして os.ReadAt で該当オフセットだけ読む。
	// 省略時は PEEndpoint（無ければ Endpoint）のホストを使う。対象コンテナの
	// Filesystem Whitelist に OCP ノードのサブネット登録が前提。
	// +optional
	NFSServer string `json:"nfsServer,omitempty"`
}

type VMSpec struct {
	// Name は AHV 上の VM 名
	Name string `json:"name"`

	// TargetName は OVE 側の VM 名（省略時は Name と同じ）
	// +optional
	TargetName string `json:"targetName,omitempty"`

	// GuestPrepMode は移行前のゲストOS準備方式
	// none:  ディスクコピーのみ (デフォルト)
	// ssh:   SSH経由で preMigrationScript を実行してドライバーを注入する（Linux向け）
	// winrm: WinRM経由で PowerShell スクリプトを実行してドライバーを注入する（Windows向け）
	// +kubebuilder:validation:Enum=none;ssh;winrm
	// +optional
	GuestPrepMode string `json:"guestPrepMode,omitempty"`

	// GuestPrepConfig は GuestPrepMode: ssh の設定
	// +optional
	GuestPrepConfig *GuestPrepSSHConfig `json:"guestPrepConfig,omitempty"`

	// GuestPrepWinRMConfig は GuestPrepMode: winrm の設定
	// +optional
	GuestPrepWinRMConfig *GuestPrepWinRMConfig `json:"guestPrepWinRMConfig,omitempty"`

	// UEFI は OVE 側 VM を UEFI ファームウェアで起動するかどうかを指定する。
	// nil (省略) の場合はオペレーターが Prism のディスクパーティションを自動検出する。
	// +optional
	UEFI *bool `json:"uefi,omitempty"`

	// KeepMAC は移行後の OVE VM で AHV の MAC アドレスを引き継ぐかを制御する。
	// nil または true の場合、AHV の MAC アドレスを KubeVirt VM に設定する（デフォルト）。
	// false の場合、KubeVirt に MAC アドレスを自動割り当てさせる（IP も変わる）。
	// +optional
	KeepMAC *bool `json:"keepMAC,omitempty"`

	// DiskBus は OVE 側 VM のディスクバスタイプを指定する。省略時は scsi。
	// scsi:   virtio-scsi。AHV は Linux/Windows とも virtio-scsi ネイティブのため推奨（デフォルト）
	// virtio: virtio-blk。高速だが viostor サービスが必要（AHV Windows には存在せず INACCESSIBLE_BOOT_DEVICE になる）
	// sata:   ドライバー不要のフォールバック（性能は劣る）
	// +kubebuilder:validation:Enum=virtio;sata;scsi
	// +optional
	DiskBus string `json:"diskBus,omitempty"`

	// NICModel は OVE 側 VM の NIC モデルを指定する。
	// virtio: 高速だが virtio-net ドライバーが必要（Linux デフォルト）
	// e1000e: ドライバー不要（Windows 移行時に推奨）
	// +kubebuilder:validation:Enum=virtio;e1000e;rtl8139
	// +optional
	NICModel string `json:"nicModel,omitempty"`
}

type GuestPrepSSHConfig struct {
	// SSHSecretRef は SSH認証情報を持つ Secret 名
	// Secret に必要なキー: host (例: "192.168.1.10:22"), user, privateKey または password
	SSHSecretRef corev1.LocalObjectReference `json:"sshSecretRef"`

	// PreMigrationScript はSSH経由で実行する bash スクリプト
	PreMigrationScript string `json:"preMigrationScript"`

	// TimeoutSeconds はスクリプト実行タイムアウト（デフォルト: 300）
	// +optional
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// GuestPrepWinRMConfig は Windows VM の WinRM 経由ゲスト準備設定
type GuestPrepWinRMConfig struct {
	// WinRMSecretRef は WinRM 認証情報を持つ Secret 名
	// Secret に必要なキー: host (例: "192.168.1.10"), username, password
	WinRMSecretRef corev1.LocalObjectReference `json:"winrmSecretRef"`

	// PreMigrationScript は WinRM 経由で実行する PowerShell スクリプト。
	// 省略時はデフォルトの virtio-win ドライバーインストールスクリプトを使用する。
	// スクリプトは AHV VM に影響を与えずドライバーを追加インストールする。
	// VM のシャットダウンは Operator が ShutdownBeforeMigration で行う。
	// +optional
	PreMigrationScript string `json:"preMigrationScript,omitempty"`

	// VirtIOWinISOPath は VM 内での virtio-win ISO のパス（デフォルト: C:\virtio-win.iso）
	// +optional
	VirtIOWinISOPath string `json:"virtIOWinISOPath,omitempty"`

	// TimeoutSeconds はスクリプト実行タイムアウト（デフォルト: 600）
	// +optional
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	// UseHTTPS が true の場合、WinRM HTTPS（ポート 5986）を使用する
	// +optional
	UseHTTPS bool `json:"useHTTPS,omitempty"`
}

type NetworkMapping struct {
	// Source は AHV 上のネットワーク名（VLAN 名等）
	Source string `json:"source"`

	// Target は OVE 側の本番 NetworkAttachmentDefinition 名
	Target string `json:"target"`

	// TestTarget はテスト移行時に使う OVE 側の NAD 名。
	// 指定した場合、VM は最初にこの NAD で起動（TestRunning フェーズ）し、
	// 手動承認後に Target の本番 NAD に切り替わる（SwitchingNetwork フェーズ）。
	// 省略するとテストフェーズをスキップして直接本番 NAD で起動する。
	// +optional
	TestTarget string `json:"testTarget,omitempty"`

	// TargetNamespace は NAD の Namespace
	// +optional
	TargetNamespace string `json:"targetNamespace,omitempty"`
}

type StorageMapping struct {
	// Source は AHV 上のストレージコンテナ名
	Source string `json:"source"`

	// TargetStorageClass は OVE 側の StorageClass 名
	TargetStorageClass string `json:"targetStorageClass"`

	// AccessMode は PVC の accessMode（省略時: ReadWriteMany）
	// +optional
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`

	// VolumeMode は PVC の volumeMode（省略時: Block）
	// +optional
	VolumeMode corev1.PersistentVolumeMode `json:"volumeMode,omitempty"`
}

// AHVMigrationStatus は移行の現在状態を表す
type AHVMigrationStatus struct {
	// Phase は移行全体のフェーズ
	// +kubebuilder:validation:Enum=Pending;GuestPrepping;FetchingVMInfo;PreparingImages;ImportingDisks;WaitingForImport;WarmPreSync;WarmSyncing;ReadyForCutover;WarmCutover;WarmFinalSync;WarmDeltaSync;WarmFinalDelta;CreatingVMs;TestRunning;TestPending;SwitchingNetwork;Completed;Failed
	Phase AHVMigrationPhase `json:"phase,omitempty"`

	// Conditions は詳細な状態条件
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// StartTime は移行開始時刻
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime は移行完了時刻
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// TotalVMs は移行対象 VM 総数
	TotalVMs int32 `json:"totalVMs,omitempty"`

	// CompletedVMs は移行完了 VM 数
	CompletedVMs int32 `json:"completedVMs,omitempty"`

	// FailedVMs は移行失敗 VM 数
	FailedVMs int32 `json:"failedVMs,omitempty"`

	// VMs は VM ごとの移行状態
	VMs []VMStatus `json:"vms,omitempty"`
}

type VMStatus struct {
	// Name は VM 名（AHV 側）
	Name string `json:"name"`

	// Phase はこの VM の移行フェーズ
	Phase string `json:"phase,omitempty"`

	// Progress は進捗率 (0-100)
	// +optional
	Progress int32 `json:"progress,omitempty"`

	// DataVolumeRefs は作成した DataVolume 名のリスト
	// +optional
	DataVolumeRefs []string `json:"dataVolumeRefs,omitempty"`

	// TempImageRefs は Prism 上に一時作成した image UUID のリスト（移行完了後に削除）
	// +optional
	TempImageRefs []string `json:"tempImageRefs,omitempty"`

	// LastSnapshotUUID は CBT 差分同期の基準となる直近の vm_snapshot UUID
	// +optional
	LastSnapshotUUID string `json:"lastSnapshotUUID,omitempty"`

	// SnapshotPaths は基準 snapshot の disk ごとの snapshot_file_path（DiskList Index 順）
	// +optional
	SnapshotPaths []string `json:"snapshotPaths,omitempty"`

	// DeltaRounds は実行済みの差分同期ラウンド数
	// +optional
	DeltaRounds int32 `json:"deltaRounds,omitempty"`

	// LastDeltaBytes は直近ラウンドの差分バイト数（全ディスク合計）
	// +optional
	LastDeltaBytes int64 `json:"lastDeltaBytes,omitempty"`

	// SyncJobRefs は実行中の delta-sync Job 名のリスト
	// +optional
	SyncJobRefs []string `json:"syncJobRefs,omitempty"`

	// PendingSnapshotUUID は同期中ラウンドの新 snapshot UUID（完了後に LastSnapshotUUID へ昇格）
	// +optional
	PendingSnapshotUUID string `json:"pendingSnapshotUUID,omitempty"`

	// PendingSnapshotPaths は同期中ラウンドの snapshot_file_path（完了後に SnapshotPaths へ昇格）
	// +optional
	PendingSnapshotPaths []string `json:"pendingSnapshotPaths,omitempty"`

	// DeltaImageRefs は差分読み出し用に一時作成した image UUID（ラウンド終了時に削除）
	// +optional
	DeltaImageRefs []string `json:"deltaImageRefs,omitempty"`

	// AHVUUID は AHV 上の VM UUID（停止・電源操作用）
	// +optional
	AHVUUID string `json:"ahvUUID,omitempty"`

	// VMRef は作成した KubeVirt VirtualMachine 名
	// +optional
	VMRef string `json:"vmRef,omitempty"`

	// Error は失敗時のエラーメッセージ
	// +optional
	Error string `json:"error,omitempty"`
}

type AHVMigrationPhase string

const (
	PhasePending           AHVMigrationPhase = "Pending"
	PhaseGuestPrepping     AHVMigrationPhase = "GuestPrepping"
	PhaseFetchingVMInfo    AHVMigrationPhase = "FetchingVMInfo"
	PhasePreparingImages   AHVMigrationPhase = "PreparingImages"  // VM停止 + disk→image変換
	PhaseImportingDisks    AHVMigrationPhase = "ImportingDisks"
	PhaseWaitingForImport  AHVMigrationPhase = "WaitingForImport"
	PhaseWarmPreSync       AHVMigrationPhase = "WarmPreSync"       // VM起動中にimage化
	PhaseWarmSyncing       AHVMigrationPhase = "WarmSyncing"       // VM起動中にCDIインポート待機
	PhaseReadyForCutover   AHVMigrationPhase = "ReadyForCutover"   // カットオーバー待機（手動承認）
	PhaseWarmCutover       AHVMigrationPhase = "WarmCutover"       // インポート完了→VMシャットダウン
	PhaseWarmFinalSync     AHVMigrationPhase = "WarmFinalSync"     // 停止後にimage再作成→フルコピーやり直し（RPO=0担保）
	PhaseWarmDeltaSync     AHVMigrationPhase = "WarmDeltaSync"     // CBT: 稼働中の差分同期ループ
	PhaseWarmFinalDelta    AHVMigrationPhase = "WarmFinalDelta"    // CBT: 停止後の最終差分同期
	PhaseCreatingVMs       AHVMigrationPhase = "CreatingVMs"
	PhaseTestRunning       AHVMigrationPhase = "TestRunning"       // テストVLANでVM起動中
	PhaseTestPending       AHVMigrationPhase = "TestPending"       // テスト確認の手動承認待ち
	PhaseSwitchingNetwork  AHVMigrationPhase = "SwitchingNetwork"  // 本番VLANへNIC切り替え中
	PhaseCompleted         AHVMigrationPhase = "Completed"
	PhaseFailed            AHVMigrationPhase = "Failed"
)

const (
	ConditionVMInfoFetched  = "VMInfoFetched"
	ConditionImagesReady    = "ImagesReady"
	ConditionDisksImported  = "DisksImported"
	ConditionVMsCreated     = "VMsCreated"
	ConditionCompleted      = "Completed"
	ConditionWarmSyncDone   = "WarmSyncDone"
	ConditionTestApproved   = "TestApproved"

	// アノテーション定数
	AnnotationCutoverApproved = "migration.lightwell.co.jp/cutover-approved"
	AnnotationTestApproved    = "migration.lightwell.co.jp/test-approved"
	AnnotationRollback        = "migration.lightwell.co.jp/rollback"
)

// +kubebuilder:object:root=true

type AHVMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AHVMigration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AHVMigration{}, &AHVMigrationList{})
}
