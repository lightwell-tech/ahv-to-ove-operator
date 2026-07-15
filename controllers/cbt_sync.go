package controllers

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	migrationv1alpha1 "github.com/lightwell-tech/ahv-to-ove-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// defaultDeltaSyncThresholdMB は差分収束判定のデフォルト閾値
	defaultDeltaSyncThresholdMB = 512
	// defaultMaxDeltaRounds は cutover 前差分ループのデフォルト上限
	defaultMaxDeltaRounds = 10
	// regionMergeGap は隣接リージョンを結合する最大ギャップ（bytes）。
	// ギャップ部分は未変更データを余分に読むだけで正しさに影響しない
	regionMergeGap = int64(4 * 1024 * 1024)
	// maxRegionsPerJob は ConfigMap(1MiB) に収める上限。超えたらギャップを広げて再結合する
	maxRegionsPerJob = 20000
	// deltaSyncServiceAccount は delta-sync Job 用の ServiceAccount（anyuid SCC 付与済み前提）
	deltaSyncServiceAccount = "ahv-delta-sync"
)

// cbtEnabled は CBT 差分同期が有効かを返す
func cbtEnabled(mig *migrationv1alpha1.AHVMigration) bool {
	return mig.Spec.Source.CBT != nil && mig.Spec.Source.CBT.Enabled
}

func deltaSyncThresholdBytes(mig *migrationv1alpha1.AHVMigration) int64 {
	mb := int64(defaultDeltaSyncThresholdMB)
	if mig.Spec.Source.CBT != nil && mig.Spec.Source.CBT.DeltaSyncThresholdMB > 0 {
		mb = mig.Spec.Source.CBT.DeltaSyncThresholdMB
	}
	return mb * 1024 * 1024
}

func maxDeltaRounds(mig *migrationv1alpha1.AHVMigration) int32 {
	if mig.Spec.Source.CBT != nil && mig.Spec.Source.CBT.MaxDeltaRounds > 0 {
		return mig.Spec.Source.CBT.MaxDeltaRounds
	}
	return defaultMaxDeltaRounds
}

// nfsServerHost は差分読み出しで snapshot コンテナを NFS マウントするサーバホストを返す。
// source.cbt.nfsServer 指定があればそれを、無ければ PEEndpoint（無ければ Endpoint）のホストを使う。
func nfsServerHost(mig *migrationv1alpha1.AHVMigration) string {
	if mig.Spec.Source.CBT != nil && mig.Spec.Source.CBT.NFSServer != "" {
		return mig.Spec.Source.CBT.NFSServer
	}
	ep := mig.Spec.Source.PEEndpoint
	if ep == "" {
		ep = mig.Spec.Source.Endpoint
	}
	if u, err := url.Parse(ep); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	// スキーム無し等のフォールバック（host:port/path をざっくり削る）
	ep = strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	if i := strings.IndexByte(ep, '/'); i >= 0 {
		ep = ep[:i]
	}
	if i := strings.IndexByte(ep, ':'); i >= 0 {
		ep = ep[:i]
	}
	return ep
}

// splitSnapshotPath は snapshot_file_path をコンテナ名とコンテナ内相対パスに分解する。
// 例: "/default-container-x/.snapshot/47/.../vmdisk/uuid"
//   → container="default-container-x", rel=".snapshot/47/.../vmdisk/uuid"
// Nutanix ADSF は各ストレージコンテナを "/<container>" として NFS エクスポートするため、
// コンテナをマウントし rel を pread する。
func splitSnapshotPath(p string) (container, rel string) {
	p = strings.TrimPrefix(p, "/")
	i := strings.IndexByte(p, '/')
	if i < 0 {
		return p, ""
	}
	return p[:i], p[i+1:]
}

// operatorImage は delta-sync Job に使うイメージ（= operator 自身のイメージ）を返す
func operatorImage() string {
	if img := os.Getenv("OPERATOR_IMAGE"); img != "" {
		return img
	}
	return "ghcr.io/lightwell-tech/ahv-to-ove-operator:v0.1.0"
}

// mergeRegions は REGULAR リージョンを offset 順にギャップ gap 以下で結合する。
// ZEROED は差分同期では転送不要（変更が ZEROED になった場合も REGULAR 扱いで返る想定だが、
// 明示 ZEROED はゼロ書き込みが必要なため type を保持したまま残す）
func mergeRegions(regions []ChangedRegion, gap int64) []ChangedRegion {
	if len(regions) == 0 {
		return regions
	}
	merged := make([]ChangedRegion, 0, len(regions))
	cur := regions[0]
	for _, r := range regions[1:] {
		// type が同じで近接していれば結合
		if r.Type == cur.Type && r.Offset <= cur.Offset+cur.Length+gap {
			end := r.Offset + r.Length
			if end > cur.Offset+cur.Length {
				cur.Length = end - cur.Offset
			}
			continue
		}
		merged = append(merged, cur)
		cur = r
	}
	merged = append(merged, cur)
	return merged
}

// prepareRegions は転送対象リージョンを結合し、ConfigMap に収まる件数まで圧縮する
func prepareRegions(regions []ChangedRegion) []ChangedRegion {
	gap := regionMergeGap
	merged := mergeRegions(regions, gap)
	for len(merged) > maxRegionsPerJob && gap < 1024*1024*1024 {
		gap *= 4
		merged = mergeRegions(regions, gap)
	}
	return merged
}

func totalRegionBytes(regions []ChangedRegion) int64 {
	var t int64
	for _, r := range regions {
		t += r.Length
	}
	return t
}

// renderRegions は delta-sync ランナーに渡す "offset length type" 行形式を生成する
func renderRegions(regions []ChangedRegion) string {
	var b strings.Builder
	for _, r := range regions {
		fmt.Fprintf(&b, "%d %d %s\n", r.Offset, r.Length, r.Type)
	}
	return b.String()
}

func deltaSyncName(migName string, vmIdx, diskIdx int, round int32) string {
	prefix := migName
	if len(prefix) > 30 {
		prefix = prefix[:30]
	}
	return fmt.Sprintf("delta-%s-%d-%d-r%d", prefix, vmIdx, diskIdx, round)
}

// buildDeltaSyncConfigMap はリージョンリストを保持する ConfigMap を生成する
func buildDeltaSyncConfigMap(mig *migrationv1alpha1.AHVMigration, vmIdx, diskIdx int, round int32, regions []ChangedRegion) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deltaSyncName(mig.Name, vmIdx, diskIdx, round),
			Namespace: targetNS(mig),
			Labels:    ownerLabelsTyped(mig),
		},
		Data: map[string]string{
			"regions": renderRegions(regions),
		},
	}
}

// buildDeltaSyncJob は PVC に差分リージョンを書き込む Job を生成する。
// snapshot のストレージコンテナを NFS(RO) でマウントし、operator 自身のイメージを
// delta-sync サブコマンドで起動して snapshot vmdisk から差分だけを ReadAt→WriteAt する。
func buildDeltaSyncJob(mig *migrationv1alpha1.AHVMigration, vmIdx, diskIdx int, round int32, snapshotPath, nfsServer, pvcName string) *batchv1.Job {
	name := deltaSyncName(mig.Name, vmIdx, diskIdx, round)
	ns := targetNS(mig)
	_, _, volumeMode := resolveStorage(mig, "")

	containerName, rel := splitSnapshotPath(snapshotPath)

	container := corev1.Container{
		Name:  "delta-sync",
		Image: operatorImage(),
		Args: []string{
			"delta-sync",
			"--source-file=/src/" + rel,
			"--regions=/config/regions",
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "regions", MountPath: "/config", ReadOnly: true},
			{Name: "src", MountPath: "/src", ReadOnly: true},
		},
		SecurityContext: &corev1.SecurityContext{
			// NFS 上の snapshot を read、CDI が作る uid/gid 107 所有の disk.img を write する。
			// root(0) なら DAC_OVERRIDE で両対応（anyuid SCC 付き SA が前提。ケーパビリティは
			// 既定のまま = DAC_OVERRIDE を残すため Drop はしない）。
			RunAsUser:  ptr.To(int64(0)),
			RunAsGroup: ptr.To(int64(0)),
		},
	}

	volumes := []corev1.Volume{
		{Name: "regions", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: name}}}},
		{Name: "src", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{
			Server: nfsServer, Path: "/" + containerName, ReadOnly: true}}},
	}

	if volumeMode == corev1.PersistentVolumeBlock {
		container.Args = append(container.Args, "--target=/dev/delta-target", "--block")
		container.VolumeDevices = []corev1.VolumeDevice{{Name: "target", DevicePath: "/dev/delta-target"}}
	} else {
		container.Args = append(container.Args, "--target=/target/disk.img")
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "target", MountPath: "/target"})
	}
	volumes = append(volumes, corev1.Volume{Name: "target", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: pvcName}}})

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    ownerLabelsTyped(mig),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(3)),
			TTLSecondsAfterFinished: ptr.To(int32(3600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: ownerLabelsTyped(mig)},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: deltaSyncServiceAccount,
					Containers:         []corev1.Container{container},
					Volumes:            volumes,
				},
			},
		},
	}
}

// jobFinished は Job の完了状態を返す (done, succeeded)
func jobFinished(job *batchv1.Job) (bool, bool) {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true, true
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true, false
		}
	}
	return false, false
}

// handleWarmDeltaSync は cutover 前の差分同期ループ（収束するまで snap→diff→sync を繰り返す）
func (r *AHVMigrationReconciler) handleWarmDeltaSync(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	return r.runDeltaRound(ctx, mig, false)
}

// handleWarmFinalDelta は cutover（VM停止）後の最終差分同期（必ず1ラウンド実行して整合させる）
func (r *AHVMigrationReconciler) handleWarmFinalDelta(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	return r.runDeltaRound(ctx, mig, true)
}

// runDeltaRound は CBT 差分同期の 1 ラウンドを進める per-VM ステートマシン。
//   PendingSnapshotUUID == "" : 新 snapshot 作成 → changed_regions → (収束判定) → sync Job 起動
//   PendingSnapshotUUID != "" : Job 完了待ち → 後片付け → 基準 snapshot を昇格
// final=false: 差分が閾値未満 or ラウンド上限で "DeltaConverged" → cutover へ
// final=true : 収束判定なしで必ず 1 ラウンド同期し "FinalDeltaDone" → CreatingVMs へ
func (r *AHVMigrationReconciler) runDeltaRound(ctx context.Context, mig *migrationv1alpha1.AHVMigration, final bool) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ns := targetNS(mig)

	prism, err := NewPrismClient(ctx, r.Client, mig)
	if err != nil {
		return r.failMigration(ctx, mig, fmt.Sprintf("Prism client error: %v", err))
	}

	newVMStatuses := make([]migrationv1alpha1.VMStatus, len(mig.Status.VMs))
	copy(newVMStatuses, mig.Status.VMs)
	allDone := true

	doneMark := "DeltaConverged"
	snapPrefix := "cbt-delta"
	if final {
		doneMark = "FinalDeltaDone"
		snapPrefix = "cbt-final"
	}

	for i := range newVMStatuses {
		vmSt := &newVMStatuses[i]
		if vmSt.AHVUUID == "" || vmSt.Phase == doneMark {
			continue
		}
		if len(vmSt.SnapshotPaths) == 0 {
			return r.failMigration(ctx, mig, fmt.Sprintf("CBT: VM %q has no base snapshot paths", vmSt.Name))
		}

		if vmSt.PendingSnapshotUUID == "" {
			// ── ラウンド開始: 新 snapshot + changed_regions ──
			round := vmSt.DeltaRounds + 1
			snapName := fmt.Sprintf("%s-%s-r%d", snapPrefix, vmSt.Name[:min(10, len(vmSt.Name))], round)
			snapUUID, err := prism.CreateVMSnapshot(ctx, vmSt.AHVUUID, snapName)
			if err != nil {
				return r.failMigration(ctx, mig, fmt.Sprintf("CBT snapshot %q: %v", vmSt.Name, err))
			}
			paths, err := prism.GetVMSnapshotPaths(ctx, snapUUID, vmSt.AHVUUID)
			if err != nil {
				return r.failMigration(ctx, mig, fmt.Sprintf("CBT snapshot paths %q: %v", vmSt.Name, err))
			}
			if len(paths) != len(vmSt.SnapshotPaths) {
				return r.failMigration(ctx, mig, fmt.Sprintf("CBT: disk count changed for %q (%d -> %d)", vmSt.Name, len(vmSt.SnapshotPaths), len(paths)))
			}

			diskRegions := make([][]ChangedRegion, len(paths))
			var totalBytes int64
			for d := range paths {
				regions, _, err := prism.ChangedRegions(ctx, paths[d], vmSt.SnapshotPaths[d])
				if err != nil {
					return r.failMigration(ctx, mig, fmt.Sprintf("CBT changed_regions %q disk%d: %v", vmSt.Name, d, err))
				}
				diskRegions[d] = regions
				totalBytes += totalRegionBytes(regions)
			}
			vmSt.LastDeltaBytes = totalBytes
			logger.Info("CBT: delta computed", "vm", vmSt.Name, "round", round, "bytes", totalBytes, "final", final)

			// 収束判定（cutover 前のみ）: 閾値未満 or ラウンド上限 → 同期せず新 snapshot を基準に昇格
			if !final && (totalBytes <= deltaSyncThresholdBytes(mig) || vmSt.DeltaRounds >= maxDeltaRounds(mig)) {
				if err := prism.DeleteVMSnapshot(ctx, vmSt.LastSnapshotUUID); err != nil {
					logger.Error(err, "CBT: failed to delete old base snapshot (continuing)", "snapshot", vmSt.LastSnapshotUUID)
				}
				vmSt.LastSnapshotUUID = snapUUID
				vmSt.SnapshotPaths = paths
				vmSt.Phase = doneMark
				logger.Info("CBT: delta converged, ready for cutover", "vm", vmSt.Name, "bytes", totalBytes)
				continue
			}

			// 最終ラウンドで差分ゼロなら Job 不要でそのまま完了
			if final && totalBytes == 0 {
				vmSt.PendingSnapshotUUID = snapUUID
				vmSt.PendingSnapshotPaths = paths
				vmSt.Phase = doneMark
				logger.Info("CBT: final delta is zero, nothing to sync", "vm", vmSt.Name)
				continue
			}

			// ── 差分転送 Job 起動（disk ごと）──
			// snapshot vmdisk を NFS 直読みするので image 化は不要（v3 images/file が
			// Range 非対応のため）。paths[d] = 新 snapshot の snapshot_file_path をマウントして読む。
			nfsServer := nfsServerHost(mig)
			jobRefs := make([]string, 0)
			for d := range paths {
				merged := prepareRegions(diskRegions[d])
				if len(merged) == 0 {
					continue
				}
				if d >= len(vmSt.DataVolumeRefs) {
					return r.failMigration(ctx, mig, fmt.Sprintf("CBT: no DataVolume for disk %d of %q", d, vmSt.Name))
				}
				pvcName := vmSt.DataVolumeRefs[d]

				cm := buildDeltaSyncConfigMap(mig, i, d, round, merged)
				if err := r.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
					return ctrl.Result{}, fmt.Errorf("create delta ConfigMap: %w", err)
				}
				job := buildDeltaSyncJob(mig, i, d, round, paths[d], nfsServer, pvcName)
				if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
					return ctrl.Result{}, fmt.Errorf("create delta Job: %w", err)
				}
				jobRefs = append(jobRefs, job.Name)
				logger.Info("CBT: delta sync job created", "job", job.Name, "regions", len(merged), "bytes", totalRegionBytes(merged), "src", paths[d])
			}
			vmSt.PendingSnapshotUUID = snapUUID
			vmSt.PendingSnapshotPaths = paths
			vmSt.SyncJobRefs = jobRefs
			vmSt.Phase = "DeltaSyncing"
			allDone = false
		} else {
			// ── Job 完了待ち → 後片付け ──
			jobsDone := true
			for _, jobName := range vmSt.SyncJobRefs {
				job := &batchv1.Job{}
				if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job); err != nil {
					if apierrors.IsNotFound(err) {
						continue // TTL で消えた = 完了扱い
					}
					return ctrl.Result{}, err
				}
				done, ok := jobFinished(job)
				if !done {
					jobsDone = false
					continue
				}
				if !ok {
					return r.failMigration(ctx, mig, fmt.Sprintf("CBT: delta sync job %s failed", jobName))
				}
			}
			if !jobsDone {
				allDone = false
				continue
			}

			// ラウンド後処理: CM/Job/delta image/旧基準 snapshot を削除して基準を昇格
			for _, jobName := range vmSt.SyncJobRefs {
				job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns}}
				propagation := metav1.DeletePropagationBackground
				_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation})
				cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns}}
				_ = r.Delete(ctx, cm)
			}
			// NFS 直読み方式では差分用 image を作らないため image 削除は不要
			if err := prism.DeleteVMSnapshot(ctx, vmSt.LastSnapshotUUID); err != nil {
				logger.Error(err, "CBT: failed to delete old base snapshot (continuing)", "snapshot", vmSt.LastSnapshotUUID)
			}
			vmSt.LastSnapshotUUID = vmSt.PendingSnapshotUUID
			vmSt.SnapshotPaths = vmSt.PendingSnapshotPaths
			vmSt.PendingSnapshotUUID = ""
			vmSt.PendingSnapshotPaths = nil
			vmSt.SyncJobRefs = nil
			vmSt.DeltaImageRefs = nil
			vmSt.DeltaRounds++
			if final {
				vmSt.Phase = doneMark
			} else {
				vmSt.Phase = "DeltaIdle" // 次の reconcile で新ラウンド開始
				allDone = false
			}
			logger.Info("CBT: delta round complete", "vm", vmSt.Name, "rounds", vmSt.DeltaRounds, "final", final)
		}
	}

	patch := mig.DeepCopy()
	patch.Status.VMs = newVMStatuses

	if allDone {
		if final {
			// 最終差分完了: 残スナップショット・プリコピー image を掃除して VM 作成へ
			for i := range newVMStatuses {
				vmSt := &newVMStatuses[i]
				for _, snap := range []string{vmSt.LastSnapshotUUID, vmSt.PendingSnapshotUUID} {
					if snap == "" {
						continue
					}
					if err := prism.DeleteVMSnapshot(ctx, snap); err != nil {
						logger.Error(err, "CBT: failed to delete snapshot (continuing)", "snapshot", snap)
					}
				}
				for _, img := range vmSt.TempImageRefs {
					if err := prism.DeleteImage(ctx, img); err != nil {
						logger.Error(err, "CBT: failed to delete pre-copy image (continuing)", "image", img)
					}
				}
			}
			patch.Status.VMs = newVMStatuses
			patch.Status.Phase = migrationv1alpha1.PhaseCreatingVMs
			setCondition(patch, migrationv1alpha1.ConditionDisksImported, metav1.ConditionTrue, "Imported", "CBT final delta synced, disks consistent with shutdown state")
			logger.Info("CBT: final delta sync complete, moving to CreatingVMs")
		} else {
			if mig.Spec.Source.PauseBeforeCutover {
				patch.Status.Phase = migrationv1alpha1.PhaseReadyForCutover
				setCondition(patch, migrationv1alpha1.ConditionWarmSyncDone, metav1.ConditionTrue, "Converged", "Delta sync converged. Add annotation cutover-approved=true to proceed")
			} else {
				patch.Status.Phase = migrationv1alpha1.PhaseWarmCutover
				setCondition(patch, migrationv1alpha1.ConditionWarmSyncDone, metav1.ConditionTrue, "Converged", "Delta sync converged, cutting over")
			}
			logger.Info("CBT: all VMs converged")
		}
	}

	if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}
