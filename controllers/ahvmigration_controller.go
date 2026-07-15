package controllers

import (
	"context"
	"fmt"
	"time"

	"strings"

	migrationv1alpha1 "github.com/lightwell-tech/ahv-to-ove-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const requeueInterval = 30 * time.Second

type AHVMigrationReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	DynamicClient dynamic.Interface
}

// +kubebuilder:rbac:groups=migration.lightwell.co.jp,resources=ahvmigrations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=migration.lightwell.co.jp,resources=ahvmigrations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=migration.lightwell.co.jp,resources=ahvmigrations/finalizers,verbs=update
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

func (r *AHVMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	mig := &migrationv1alpha1.AHVMigration{}
	if err := r.Get(ctx, req.NamespacedName, mig); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if mig.Status.Phase == migrationv1alpha1.PhaseCompleted ||
		mig.Status.Phase == migrationv1alpha1.PhaseFailed {
		return ctrl.Result{}, nil
	}

	logger.Info("Reconciling AHVMigration", "phase", mig.Status.Phase)

	switch mig.Status.Phase {
	case "":
		return r.handleInit(ctx, mig)
	case migrationv1alpha1.PhasePending:
		return r.handlePending(ctx, mig)
	case migrationv1alpha1.PhaseGuestPrepping:
		return r.handleGuestPrepping(ctx, mig)
	case migrationv1alpha1.PhasePreparingImages:
		return r.handlePrepareImages(ctx, mig)
	case migrationv1alpha1.PhaseWarmPreSync:
		return r.handleWarmPreSync(ctx, mig)
	case migrationv1alpha1.PhaseWarmSyncing:
		return r.handleWarmSyncing(ctx, mig)
	case migrationv1alpha1.PhaseReadyForCutover:
		return r.handleReadyForCutover(ctx, mig)
	case migrationv1alpha1.PhaseWarmCutover:
		return r.handleWarmCutover(ctx, mig)
	case migrationv1alpha1.PhaseWarmFinalSync:
		return r.handleWarmFinalSync(ctx, mig)
	case migrationv1alpha1.PhaseWarmDeltaSync:
		return r.handleWarmDeltaSync(ctx, mig)
	case migrationv1alpha1.PhaseWarmFinalDelta:
		return r.handleWarmFinalDelta(ctx, mig)
	case migrationv1alpha1.PhaseImportingDisks, migrationv1alpha1.PhaseWaitingForImport:
		return r.handleWaitImport(ctx, mig)
	case migrationv1alpha1.PhaseCreatingVMs:
		return r.handleCreateVMs(ctx, mig)
	case migrationv1alpha1.PhaseTestRunning:
		return r.handleTestRunning(ctx, mig)
	case migrationv1alpha1.PhaseTestPending:
		return r.handleTestPending(ctx, mig)
	case migrationv1alpha1.PhaseSwitchingNetwork:
		return r.handleSwitchingNetwork(ctx, mig)
	default:
		return ctrl.Result{}, nil
	}
}

// handleInit: 初回 → Pending に遷移して CDI 認証 Secret を作成する
func (r *AHVMigrationReconciler) handleInit(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Initializing AHVMigration")

	patch := mig.DeepCopy()
	patch.Status.Phase = migrationv1alpha1.PhasePending
	patch.Status.TotalVMs = int32(len(mig.Spec.VMs))
	now := metav1.Now()
	patch.Status.StartTime = &now

	if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// handlePending: Prism から VM 情報取得 → CDI Secret 作成
// shutdownBeforeMigration=true の場合: PreparingImages へ
// false の場合: イメージバッキングありの disk のみ DataVolume 作成 → ImportingDisks へ
func (r *AHVMigrationReconciler) handlePending(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Fetching VM info from Prism")

	prism, err := NewPrismClient(ctx, r.Client, mig)
	if err != nil {
		return r.failMigration(ctx, mig, fmt.Sprintf("Prism client error: %v", err))
	}

	// CDI 認証 Secret（Prism Basic Auth）を作成
	user, pass := prism.Credentials()
	authSecret := buildCDIAuthSecret(mig, user, pass)
	if err := r.ensureSecret(ctx, authSecret); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure CDI auth secret: %w", err)
	}

	vmStatuses := make([]migrationv1alpha1.VMStatus, 0, len(mig.Spec.VMs))

	for _, vmSpec := range mig.Spec.VMs {
		vmInfo, err := prism.GetVMByName(ctx, vmSpec.Name)
		if err != nil {
			return r.failMigration(ctx, mig, fmt.Sprintf("VM %q: %v", vmSpec.Name, err))
		}
		vmStatuses = append(vmStatuses, migrationv1alpha1.VMStatus{
			Name:    vmSpec.Name,
			AHVUUID: vmInfo.UUID,
			Phase:   "Pending",
		})
	}

	patch := mig.DeepCopy()
	patch.Status.VMs = vmStatuses
	setCondition(patch, migrationv1alpha1.ConditionVMInfoFetched, metav1.ConditionTrue, "Fetched", "VM info fetched from Prism")

	// いずれかのVMにSSH GuestPrepが設定されていればGuestPreppingフェーズへ
	if needsGuestPrep(mig) {
		patch.Status.Phase = migrationv1alpha1.PhaseGuestPrepping
		setCondition(patch, "GuestPrepComplete", metav1.ConditionFalse, "Pending", "Running pre-migration guest preparation scripts")
	} else if mig.Spec.Source.WarmMigration {
		// VM 起動中に image 化 → CDI import → 完了後にシャットダウン（ダウンタイム最小化）
		patch.Status.Phase = migrationv1alpha1.PhaseWarmPreSync
		setCondition(patch, migrationv1alpha1.ConditionImagesReady, metav1.ConditionFalse, "WarmPreSync", "Creating disk images from running VMs")
	} else if mig.Spec.Source.ShutdownBeforeMigration {
		// VM 停止 → Prism image 化が必要 → PreparingImages フェーズへ
		patch.Status.Phase = migrationv1alpha1.PhasePreparingImages
		setCondition(patch, migrationv1alpha1.ConditionImagesReady, metav1.ConditionFalse, "Preparing", "Shutting down VMs and creating disk images")
	} else {
		// イメージバッキングありの disk のみ DataVolume 作成 → ImportingDisks へ
		if err := r.createDataVolumes(ctx, mig, prism, vmStatuses); err != nil {
			return ctrl.Result{}, err
		}
		patch.Status.Phase = migrationv1alpha1.PhaseImportingDisks
		patch.Status.VMs = vmStatuses
		setCondition(patch, migrationv1alpha1.ConditionDisksImported, metav1.ConditionFalse, "Importing", "DataVolumes are being imported")
	}

	if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// handlePrepareImages: VM を停止して disk → Prism image 化し、完了したら ImportingDisks へ
func (r *AHVMigrationReconciler) handlePrepareImages(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	prism, err := NewPrismClient(ctx, r.Client, mig)
	if err != nil {
		return r.failMigration(ctx, mig, fmt.Sprintf("Prism client error: %v", err))
	}

	newVMStatuses := make([]migrationv1alpha1.VMStatus, len(mig.Status.VMs))
	copy(newVMStatuses, mig.Status.VMs)
	allReady := true

	for i, vmSt := range newVMStatuses {
		if vmSt.AHVUUID == "" {
			continue
		}

		// Step1: VM 停止
		powerState, err := prism.GetVMPowerState(ctx, vmSt.AHVUUID)
		if err != nil {
			logger.Error(err, "GetVMPowerState failed", "vm", vmSt.Name)
			allReady = false
			continue
		}

		if powerState == "ON" {
			logger.Info("Shutting down VM", "vm", vmSt.Name)
			if err := prism.ShutdownVM(ctx, vmSt.AHVUUID); err != nil {
				logger.Error(err, "ShutdownVM failed", "vm", vmSt.Name)
			}
			allReady = false
			newVMStatuses[i].Phase = "ShuttingDown"
			continue
		}

		// Step2: 停止済み → disk → image 変換（まだやっていない disk のみ）
		vmInfo, err := prism.GetVMByName(ctx, vmSt.Name)
		if err != nil {
			return r.failMigration(ctx, mig, fmt.Sprintf("VM %q: %v", vmSt.Name, err))
		}

		if len(vmSt.TempImageRefs) == 0 {
			// image 化がまだ → 一括で起動
			tempImages := make([]string, 0)
			for _, disk := range vmInfo.DiskList {
				if disk.UUID == "" {
					continue
				}
				imageName := fmt.Sprintf("mig-%s-%s-d%d", mig.Name[:min(20, len(mig.Name))], vmSt.Name[:min(10, len(vmSt.Name))], disk.Index)
				logger.Info("Creating Prism image from disk", "vm", vmSt.Name, "disk", disk.Index, "imageName", imageName)
				imgUUID, err := prism.CreateImageFromDisk(ctx, disk.UUID, imageName)
				if err != nil {
					logger.Error(err, "CreateImageFromDisk failed", "disk", disk.UUID)
					allReady = false
					continue
				}
				tempImages = append(tempImages, imgUUID)
				logger.Info("Image created", "imageUUID", imgUUID)
			}
			newVMStatuses[i].TempImageRefs = tempImages
			newVMStatuses[i].Phase = "CreatingImages"
			allReady = false
			continue
		}

		// Step3: image が COMPLETE になるまで待機
		allImagesReady := true
		for _, imgUUID := range vmSt.TempImageRefs {
			if err := prism.WaitImageReady(ctx, imgUUID); err != nil {
				logger.Info("Image not ready yet", "imageUUID", imgUUID)
				allImagesReady = false
				allReady = false
				break
			}
		}
		if allImagesReady {
			newVMStatuses[i].Phase = "ImagesReady"
		}
	}

	patch := mig.DeepCopy()
	patch.Status.VMs = newVMStatuses

	if allReady {
		// 全 VM の image 化完了 → DataVolume 作成 → ImportingDisks
		logger.Info("All images ready, creating DataVolumes")
		if err := r.createDataVolumesFromTempImages(ctx, mig, prism, newVMStatuses); err != nil {
			return ctrl.Result{}, err
		}
		patch.Status.Phase = migrationv1alpha1.PhaseImportingDisks
		setCondition(patch, migrationv1alpha1.ConditionImagesReady, metav1.ConditionTrue, "Ready", "All disk images created in Prism")
		setCondition(patch, migrationv1alpha1.ConditionDisksImported, metav1.ConditionFalse, "Importing", "DataVolumes are being imported")
	}

	if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// createDataVolumes は通常フロー（image backing あり disk のみ）で DataVolume を作成する
func (r *AHVMigrationReconciler) createDataVolumes(ctx context.Context, mig *migrationv1alpha1.AHVMigration, prism *PrismClient, vmStatuses []migrationv1alpha1.VMStatus) error {
	logger := log.FromContext(ctx)
	ns := targetNS(mig)

	for vmIdx, vmSpec := range mig.Spec.VMs {
		vmInfo, err := prism.GetVMByName(ctx, vmSpec.Name)
		if err != nil {
			return fmt.Errorf("VM %q: %w", vmSpec.Name, err)
		}

		dvRefs := make([]string, 0)
		for _, disk := range vmInfo.DiskList {
			var dv *unstructured.Unstructured
			if disk.SourceURL != "" {
				sourceURL := disk.SourceURL
				if mig.Spec.Source.CDIProxyURL != "" {
					sourceURL = replacePrismURLWithProxy(disk.SourceURL, mig.Spec.Source.Endpoint, mig.Spec.Source.CDIProxyURL)
				}
				dv = buildDataVolume(mig, vmIdx, disk, sourceURL)
			} else if disk.IsBare {
				logger.Info("Bare disk without shutdown mode: creating blank DV", "vm", vmSpec.Name, "disk", disk.Index)
				dv = buildBlankDataVolume(mig, vmIdx, disk)
			} else {
				continue
			}

			dvRefs = append(dvRefs, dv.GetName())
			existing := &unstructured.Unstructured{}
			existing.SetAPIVersion("cdi.kubevirt.io/v1beta1")
			existing.SetKind("DataVolume")
			err := r.Get(ctx, types.NamespacedName{Name: dv.GetName(), Namespace: ns}, existing)
			if apierrors.IsNotFound(err) {
				if err := r.Create(ctx, dv); err != nil {
					return fmt.Errorf("create DataVolume %s: %w", dv.GetName(), err)
				}
			} else if err != nil {
				return err
			}
		}
		vmStatuses[vmIdx].DataVolumeRefs = dvRefs
		vmStatuses[vmIdx].Phase = "ImportingDisks"
	}
	return nil
}

// createDataVolumesFromTempImages は PreparingImages フェーズで作成した一時 image から DataVolume を作成する
func (r *AHVMigrationReconciler) createDataVolumesFromTempImages(ctx context.Context, mig *migrationv1alpha1.AHVMigration, prism *PrismClient, vmStatuses []migrationv1alpha1.VMStatus) error {
	logger := log.FromContext(ctx)
	ns := targetNS(mig)

	for vmIdx := range vmStatuses {
		dvRefs := make([]string, 0)

		for diskIdx, imgUUID := range vmStatuses[vmIdx].TempImageRefs {
			sourceURL := prism.DiskImageURL(imgUUID)
			if mig.Spec.Source.CDIProxyURL != "" {
				sourceURL = replacePrismURLWithProxy(sourceURL, mig.Spec.Source.Endpoint, mig.Spec.Source.CDIProxyURL)
			}

			// image の実サイズを取得して DiskInfo を構築（取得失敗時は 50GB デフォルト）
			sizeMB := int64(51200)
			if sizeBytes, err := prism.GetImageSizeBytes(ctx, imgUUID); err != nil {
				logger.Error(err, "GetImageSizeBytes failed, using default 50GB", "imageUUID", imgUUID)
			} else {
				sizeMB = (sizeBytes + 1024*1024 - 1) / (1024 * 1024)
			}
			disk := DiskInfo{Index: diskIdx, UUID: imgUUID, SizeMB: sizeMB}
			dv := buildDataVolume(mig, vmIdx, disk, sourceURL)
			dvRefs = append(dvRefs, dv.GetName())

			existing := &unstructured.Unstructured{}
			existing.SetAPIVersion("cdi.kubevirt.io/v1beta1")
			existing.SetKind("DataVolume")
			err := r.Get(ctx, types.NamespacedName{Name: dv.GetName(), Namespace: ns}, existing)
			if apierrors.IsNotFound(err) {
				logger.Info("Creating DataVolume from temp image", "name", dv.GetName(), "imageUUID", imgUUID)
				if err := r.Create(ctx, dv); err != nil {
					return fmt.Errorf("create DataVolume %s: %w", dv.GetName(), err)
				}
			} else if err != nil {
				return err
			}
		}
		vmStatuses[vmIdx].DataVolumeRefs = dvRefs
		vmStatuses[vmIdx].Phase = "ImportingDisks"
	}
	return nil
}

// handleWarmPreSync: VM 起動中に全 disk を Prism image 化して DataVolume 作成 → WarmSyncing へ
func (r *AHVMigrationReconciler) handleWarmPreSync(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("WarmPreSync: creating disk images from running VMs")

	prism, err := NewPrismClient(ctx, r.Client, mig)
	if err != nil {
		return r.failMigration(ctx, mig, fmt.Sprintf("Prism client error: %v", err))
	}

	newVMStatuses := make([]migrationv1alpha1.VMStatus, len(mig.Status.VMs))
	copy(newVMStatuses, mig.Status.VMs)

	for i, vmSt := range newVMStatuses {
		if len(vmSt.TempImageRefs) > 0 {
			// すでに image 化済み → スキップ
			continue
		}

		vmInfo, err := prism.GetVMByName(ctx, vmSt.Name)
		if err != nil {
			return r.failMigration(ctx, mig, fmt.Sprintf("VM %q: %v", vmSt.Name, err))
		}

		// CBT: プリコピーの基準となる snapshot (snap0) を image 化より先に作成する。
		// 以降の差分は snap0 起点の changed_regions で追跡する
		if cbtEnabled(mig) && newVMStatuses[i].LastSnapshotUUID == "" {
			snapName := fmt.Sprintf("cbt-base-%s-%s", mig.Name[:min(20, len(mig.Name))], vmSt.Name[:min(10, len(vmSt.Name))])
			snapUUID, err := prism.CreateVMSnapshot(ctx, vmInfo.UUID, snapName)
			if err != nil {
				return r.failMigration(ctx, mig, fmt.Sprintf("CBT base snapshot %q: %v", vmSt.Name, err))
			}
			paths, err := prism.GetVMSnapshotPaths(ctx, snapUUID, vmInfo.UUID)
			if err != nil {
				return r.failMigration(ctx, mig, fmt.Sprintf("CBT base snapshot paths %q: %v", vmSt.Name, err))
			}
			newVMStatuses[i].LastSnapshotUUID = snapUUID
			newVMStatuses[i].SnapshotPaths = paths
			logger.Info("CBT: base snapshot created", "vm", vmSt.Name, "snapshot", snapUUID)
		}

		tempImages := make([]string, 0)
		for _, disk := range vmInfo.DiskList {
			if disk.UUID == "" {
				continue
			}
			imageName := fmt.Sprintf("warm-%s-%s-d%d",
				mig.Name[:min(20, len(mig.Name))],
				vmSt.Name[:min(10, len(vmSt.Name))],
				disk.Index)
			logger.Info("Creating Prism image from running disk", "vm", vmSt.Name, "disk", disk.Index)
			imgUUID, err := prism.CreateImageFromDisk(ctx, disk.UUID, imageName)
			if err != nil {
				return r.failMigration(ctx, mig, fmt.Sprintf("CreateImageFromDisk %q disk%d: %v", vmSt.Name, disk.Index, err))
			}
			tempImages = append(tempImages, imgUUID)
		}
		newVMStatuses[i].TempImageRefs = tempImages
		newVMStatuses[i].AHVUUID = vmInfo.UUID
		newVMStatuses[i].Phase = "WarmPreSync"
	}

	// DataVolume 作成
	if err := r.createDataVolumesFromTempImages(ctx, mig, prism, newVMStatuses); err != nil {
		return ctrl.Result{}, err
	}

	patch := mig.DeepCopy()
	patch.Status.VMs = newVMStatuses
	patch.Status.Phase = migrationv1alpha1.PhaseWarmSyncing
	setCondition(patch, migrationv1alpha1.ConditionImagesReady, metav1.ConditionTrue, "Created", "Disk images created from running VMs")
	setCondition(patch, migrationv1alpha1.ConditionDisksImported, metav1.ConditionFalse, "WarmSyncing", "CDI importing disks (VM still running)")

	if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// handleWarmSyncing: CDI インポート完了待ち（VM は起動中）→ 完了したら WarmCutover へ
func (r *AHVMigrationReconciler) handleWarmSyncing(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ns := targetNS(mig)

	allReady := true
	newVMStatuses := make([]migrationv1alpha1.VMStatus, len(mig.Status.VMs))
	copy(newVMStatuses, mig.Status.VMs)

	for i, vmSt := range newVMStatuses {
		allDVsDone := true
		var totalProgress int32

		for _, dvRef := range vmSt.DataVolumeRefs {
			dv := &unstructured.Unstructured{}
			dv.SetAPIVersion("cdi.kubevirt.io/v1beta1")
			dv.SetKind("DataVolume")
			if err := r.Get(ctx, types.NamespacedName{Name: dvRef, Namespace: ns}, dv); err != nil {
				allDVsDone = false
				allReady = false
				continue
			}
			if !isDVSucceeded(dv) {
				allDVsDone = false
				allReady = false
				totalProgress += dvProgress(dv)
			} else {
				totalProgress += 100
			}
		}
		if len(vmSt.DataVolumeRefs) > 0 {
			newVMStatuses[i].Progress = totalProgress / int32(len(vmSt.DataVolumeRefs))
		}
		if allDVsDone {
			newVMStatuses[i].Phase = "WarmSyncDone"
		}
	}

	patch := mig.DeepCopy()
	patch.Status.VMs = newVMStatuses

	if allReady {
		if cbtEnabled(mig) {
			logger.Info("WarmSyncing complete, starting CBT delta sync loop")
			patch.Status.Phase = migrationv1alpha1.PhaseWarmDeltaSync
			setCondition(patch, migrationv1alpha1.ConditionWarmSyncDone, metav1.ConditionTrue, "Done", "Pre-copy sync complete, delta sync loop starting")
		} else if mig.Spec.Source.PauseBeforeCutover {
			logger.Info("WarmSyncing complete, pausing at ReadyForCutover (manual approval required)")
			patch.Status.Phase = migrationv1alpha1.PhaseReadyForCutover
			setCondition(patch, migrationv1alpha1.ConditionWarmSyncDone, metav1.ConditionTrue, "Done", "Pre-copy sync complete. Add annotation cutover-approved=true to proceed")
		} else {
			logger.Info("WarmSyncing complete, transitioning to WarmCutover")
			patch.Status.Phase = migrationv1alpha1.PhaseWarmCutover
			setCondition(patch, migrationv1alpha1.ConditionWarmSyncDone, metav1.ConditionTrue, "Done", "Pre-copy sync complete, ready for cutover")
		}
	} else {
		logger.Info("WarmSyncing: waiting for DataVolumes", "progress", func() int32 {
			var t int32
			for _, v := range newVMStatuses {
				t += v.Progress
			}
			if len(newVMStatuses) > 0 {
				return t / int32(len(newVMStatuses))
			}
			return 0
		}())
	}

	if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// warmFinalSyncEnabled は warm 移行の cutover 後再同期（WarmFinalSync）が有効かを返す。
// warmFinalFullSync 未指定時は有効（デフォルト true）。
func warmFinalSyncEnabled(mig *migrationv1alpha1.AHVMigration) bool {
	if mig.Spec.Source.WarmFinalFullSync != nil {
		return *mig.Spec.Source.WarmFinalFullSync
	}
	return true
}

// handleWarmFinalSync: cutover（ソースVM停止）後の再同期。
// プリコピー image は VM 稼働中に作成されているため、そのままでは image 化以降の書き込みが失われる。
// 停止済みディスクから image を再作成し、DataVolume を作り直してフルコピーし直すことで RPO=0 を担保する
// （Phase A: CBT 差分同期までのつなぎ。設計: docs/warm-migration-cbt-design.md）。
// 完了後は ImportingDisks に戻し、既存の handleWaitImport → CreatingVMs の機構に乗せる。
func (r *AHVMigrationReconciler) handleWarmFinalSync(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ns := targetNS(mig)

	prism, err := NewPrismClient(ctx, r.Client, mig)
	if err != nil {
		return r.failMigration(ctx, mig, fmt.Sprintf("Prism client error: %v", err))
	}

	newVMStatuses := make([]migrationv1alpha1.VMStatus, len(mig.Status.VMs))
	copy(newVMStatuses, mig.Status.VMs)
	allStaged := true

	for i := range newVMStatuses {
		vmSt := &newVMStatuses[i]
		if vmSt.AHVUUID == "" {
			continue
		}
		switch vmSt.Phase {
		case "FinalSyncImaged":
			// 旧 DV の削除完了（同名 DV を作り直すため PVC 解放待ち）
			for _, dvRef := range vmSt.DataVolumeRefs {
				dv := &unstructured.Unstructured{}
				dv.SetAPIVersion("cdi.kubevirt.io/v1beta1")
				dv.SetKind("DataVolume")
				if err := r.Get(ctx, types.NamespacedName{Name: dvRef, Namespace: ns}, dv); err == nil {
					allStaged = false
				} else if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}
			}
		default:
			// stage 1: 停止済みディスクから image 再作成 → 旧 DV / 旧プリコピー image を破棄。
			// 注意: image 作成後・status 反映前に reconcile が落ちると image が重複しうる（best-effort 掃除対象）
			vmInfo, err := prism.GetVMByName(ctx, vmSt.Name)
			if err != nil {
				return r.failMigration(ctx, mig, fmt.Sprintf("FinalSync: VM %q: %v", vmSt.Name, err))
			}
			oldImages := vmSt.TempImageRefs
			newImages := make([]string, 0)
			for _, disk := range vmInfo.DiskList {
				if disk.UUID == "" {
					continue
				}
				imageName := fmt.Sprintf("final-%s-%s-d%d",
					mig.Name[:min(20, len(mig.Name))],
					vmSt.Name[:min(10, len(vmSt.Name))],
					disk.Index)
				logger.Info("FinalSync: creating image from stopped disk", "vm", vmSt.Name, "disk", disk.Index)
				imgUUID, err := prism.CreateImageFromDisk(ctx, disk.UUID, imageName)
				if err != nil {
					return r.failMigration(ctx, mig, fmt.Sprintf("FinalSync: CreateImageFromDisk %q disk%d: %v", vmSt.Name, disk.Index, err))
				}
				newImages = append(newImages, imgUUID)
			}
			// 旧 DV を削除（PVC ごと作り直す）
			for _, dvRef := range vmSt.DataVolumeRefs {
				dv := &unstructured.Unstructured{}
				dv.SetAPIVersion("cdi.kubevirt.io/v1beta1")
				dv.SetKind("DataVolume")
				dv.SetName(dvRef)
				dv.SetNamespace(ns)
				if err := r.Delete(ctx, dv); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("FinalSync: delete DataVolume %s: %w", dvRef, err)
				}
			}
			// 旧プリコピー image を削除（best-effort）
			for _, img := range oldImages {
				if err := prism.DeleteImage(ctx, img); err != nil {
					logger.Error(err, "FinalSync: failed to delete pre-copy image (continuing)", "image", img)
				}
			}
			vmSt.TempImageRefs = newImages
			vmSt.Phase = "FinalSyncImaged"
			allStaged = false
		}
	}

	patch := mig.DeepCopy()
	patch.Status.VMs = newVMStatuses

	if allStaged {
		// 旧 DV は全て消えた → 新 image から DV を作り直して既存インポート機構に乗せる
		if err := r.createDataVolumesFromTempImages(ctx, mig, prism, newVMStatuses); err != nil {
			return ctrl.Result{}, err
		}
		patch.Status.VMs = newVMStatuses
		patch.Status.Phase = migrationv1alpha1.PhaseImportingDisks
		setCondition(patch, migrationv1alpha1.ConditionDisksImported, metav1.ConditionFalse, "FinalSyncing", "Re-importing disks from post-shutdown images (final sync)")
		logger.Info("FinalSync: DataVolumes recreated from post-shutdown images, re-importing")
	}

	if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// handleReadyForCutover: カットオーバー手動承認待ち
// annotation migration.lightwell.co.jp/cutover-approved=true が付いたら WarmCutover へ進む
func (r *AHVMigrationReconciler) handleReadyForCutover(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	const approvalAnnotation = "migration.lightwell.co.jp/cutover-approved"
	if mig.Annotations[approvalAnnotation] == "true" {
		logger.Info("Cutover approved, transitioning to WarmCutover")
		patch := mig.DeepCopy()
		patch.Status.Phase = migrationv1alpha1.PhaseWarmCutover
		if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	logger.Info("ReadyForCutover: waiting for manual approval",
		"hint", "oc annotate ahvmigration "+mig.Name+" migration.lightwell.co.jp/cutover-approved=true -n "+mig.Namespace)
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// handleWarmCutover: VM を ACPI 停止してそのまま CreatingVMs へ（PVC はプリコピー済み）
func (r *AHVMigrationReconciler) handleWarmCutover(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	prism, err := NewPrismClient(ctx, r.Client, mig)
	if err != nil {
		return r.failMigration(ctx, mig, fmt.Sprintf("Prism client error: %v", err))
	}

	newVMStatuses := make([]migrationv1alpha1.VMStatus, len(mig.Status.VMs))
	copy(newVMStatuses, mig.Status.VMs)
	allOff := true

	for i, vmSt := range newVMStatuses {
		if vmSt.AHVUUID == "" {
			continue
		}
		powerState, err := prism.GetVMPowerState(ctx, vmSt.AHVUUID)
		if err != nil {
			logger.Error(err, "GetVMPowerState failed", "vm", vmSt.Name)
			allOff = false
			continue
		}
		if powerState == "ON" {
			logger.Info("WarmCutover: shutting down VM", "vm", vmSt.Name)
			if err := prism.ShutdownVM(ctx, vmSt.AHVUUID); err != nil {
				logger.Error(err, "ShutdownVM failed", "vm", vmSt.Name)
			}
			newVMStatuses[i].Phase = "ShuttingDown"
			allOff = false
		}
	}

	patch := mig.DeepCopy()
	patch.Status.VMs = newVMStatuses

	if allOff {
		if cbtEnabled(mig) {
			logger.Info("WarmCutover: all VMs off, moving to WarmFinalDelta (CBT final delta sync)")
			patch.Status.Phase = migrationv1alpha1.PhaseWarmFinalDelta
			setCondition(patch, migrationv1alpha1.ConditionDisksImported, metav1.ConditionFalse, "FinalDelta", "VMs shut down; syncing final delta regions")
		} else if warmFinalSyncEnabled(mig) {
			logger.Info("WarmCutover: all VMs off, moving to WarmFinalSync (re-sync after shutdown)")
			patch.Status.Phase = migrationv1alpha1.PhaseWarmFinalSync
			setCondition(patch, migrationv1alpha1.ConditionDisksImported, metav1.ConditionFalse, "FinalSyncing", "VMs shut down; re-syncing disks to capture writes since pre-copy")
		} else {
			logger.Info("WarmCutover: all VMs off, moving to CreatingVMs")
			patch.Status.Phase = migrationv1alpha1.PhaseCreatingVMs
			setCondition(patch, migrationv1alpha1.ConditionDisksImported, metav1.ConditionTrue, "Imported", "Pre-copy complete, VMs shut down")
		}
	}

	if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// handleWaitImport: 全 DataVolume が Succeeded になるまでポーリング
func (r *AHVMigrationReconciler) handleWaitImport(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ns := targetNS(mig)

	allReady := true
	newVMStatuses := make([]migrationv1alpha1.VMStatus, len(mig.Status.VMs))
	copy(newVMStatuses, mig.Status.VMs)

	for i, vmSt := range newVMStatuses {
		allDVsDone := true
		var totalProgress int32

		for _, dvRef := range vmSt.DataVolumeRefs {
			dv := &unstructured.Unstructured{}
			dv.SetAPIVersion("cdi.kubevirt.io/v1beta1")
			dv.SetKind("DataVolume")

			if err := r.Get(ctx, types.NamespacedName{Name: dvRef, Namespace: ns}, dv); err != nil {
				logger.Error(err, "DataVolume not found", "name", dvRef)
				allDVsDone = false
				allReady = false
				continue
			}

			if !isDVSucceeded(dv) {
				allDVsDone = false
				allReady = false
				totalProgress += dvProgress(dv)
			} else {
				totalProgress += 100
			}
		}

		if len(vmSt.DataVolumeRefs) > 0 {
			newVMStatuses[i].Progress = totalProgress / int32(len(vmSt.DataVolumeRefs))
		}
		if allDVsDone {
			newVMStatuses[i].Phase = "ReadyForVM"
		}
	}

	patch := mig.DeepCopy()
	patch.Status.VMs = newVMStatuses

	if allReady {
		logger.Info("All DataVolumes Succeeded, moving to CreatingVMs")
		patch.Status.Phase = migrationv1alpha1.PhaseCreatingVMs
		setCondition(patch, migrationv1alpha1.ConditionDisksImported, metav1.ConditionTrue, "Imported", "All DataVolumes imported successfully")
	} else {
		logger.Info("Waiting for DataVolumes to complete")
	}

	if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// handleCreateVMs: 各 VM の KubeVirt VirtualMachine を作成する
// testTarget が設定されている場合はテスト VLAN で起動 → TestRunning へ
// そうでなければ本番 NAD で起動 → Completed へ
func (r *AHVMigrationReconciler) handleCreateVMs(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ns := targetNS(mig)

	prism, err := NewPrismClient(ctx, r.Client, mig)
	if err != nil {
		return r.failMigration(ctx, mig, fmt.Sprintf("Prism client error: %v", err))
	}

	useTestTarget := hasTestTarget(mig)
	newVMStatuses := make([]migrationv1alpha1.VMStatus, len(mig.Status.VMs))
	copy(newVMStatuses, mig.Status.VMs)

	for vmIdx, vmSpec := range mig.Spec.VMs {
		if vmIdx >= len(newVMStatuses) {
			logger.Info("VMStatus entry missing, skipping", "vmIdx", vmIdx, "vm", vmSpec.Name)
			continue
		}
		vmInfo, err := prism.GetVMByName(ctx, vmSpec.Name)
		if err != nil {
			newVMStatuses[vmIdx].Error = fmt.Sprintf("Prism API error: %v", err)
			continue
		}

		var vmObj *unstructured.Unstructured
		if useTestTarget {
			vmObj = buildVirtualMachineWithNetwork(mig, vmSpec, vmIdx, vmInfo, true)
		} else {
			vmObj = buildVirtualMachine(mig, vmSpec, vmIdx, vmInfo)
		}
		vmName := vmObj.GetName()

		existing := &unstructured.Unstructured{}
		existing.SetAPIVersion("kubevirt.io/v1")
		existing.SetKind("VirtualMachine")

		err = r.Get(ctx, types.NamespacedName{Name: vmName, Namespace: ns}, existing)
		if apierrors.IsNotFound(err) {
			logger.Info("Creating VirtualMachine", "name", vmName, "testVLAN", useTestTarget)
			if err := r.Create(ctx, vmObj); err != nil {
				return ctrl.Result{}, fmt.Errorf("create VirtualMachine %s: %w", vmName, err)
			}
		} else if err != nil {
			return ctrl.Result{}, err
		}

		newVMStatuses[vmIdx].VMRef = vmName
		newVMStatuses[vmIdx].Phase = "VMCreated"
		newVMStatuses[vmIdx].Progress = 100
	}

	patch := mig.DeepCopy()
	patch.Status.VMs = newVMStatuses
	setCondition(patch, migrationv1alpha1.ConditionVMsCreated, metav1.ConditionTrue, "Created", "All VirtualMachines created")

	if useTestTarget {
		// テスト移行フェーズへ: VM を起動して動作確認を待つ
		logger.Info("VMs created with test VLAN, transitioning to TestRunning")
		patch.Status.Phase = migrationv1alpha1.PhaseTestRunning
		setCondition(patch, migrationv1alpha1.ConditionTestApproved, metav1.ConditionFalse, "Pending",
			"VMs running on test VLAN. Start VMs and verify, then add annotation: "+migrationv1alpha1.AnnotationTestApproved+"=true")
	} else {
		now := metav1.Now()
		patch.Status.Phase = migrationv1alpha1.PhaseCompleted
		patch.Status.CompletionTime = &now
		patch.Status.CompletedVMs = int32(len(mig.Spec.VMs))
		setCondition(patch, migrationv1alpha1.ConditionCompleted, metav1.ConditionTrue, "Completed",
			fmt.Sprintf("Migration completed: %d VMs created", len(mig.Spec.VMs)))
		logger.Info("Migration completed", "vms", len(mig.Spec.VMs))
	}

	if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// handleTestRunning: テスト VLAN で VM 起動中。VM を running=true にして確認を促す。
// annotation test-approved=true が付いたら TestPending へ
func (r *AHVMigrationReconciler) handleTestRunning(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ns := targetNS(mig)

	// VM を起動していなければ running=true にする
	for _, vmSt := range mig.Status.VMs {
		if vmSt.VMRef == "" {
			continue
		}
		vm := &unstructured.Unstructured{}
		vm.SetAPIVersion("kubevirt.io/v1")
		vm.SetKind("VirtualMachine")
		if err := r.Get(ctx, types.NamespacedName{Name: vmSt.VMRef, Namespace: ns}, vm); err != nil {
			logger.Error(err, "VirtualMachine not found", "name", vmSt.VMRef)
			continue
		}
		running, _, _ := unstructured.NestedBool(vm.Object, "spec", "running")
		if !running {
			patch := vm.DeepCopy()
			if err := unstructured.SetNestedField(patch.Object, true, "spec", "running"); err == nil {
				logger.Info("Starting VM for test validation", "name", vmSt.VMRef)
				_ = r.Patch(ctx, patch, client.MergeFrom(vm))
			}
		}
	}

	// test-approved アノテーション確認
	if mig.Annotations[migrationv1alpha1.AnnotationTestApproved] == "true" {
		logger.Info("Test approved, moving to TestPending (will switch to prod VLAN)")
		patch := mig.DeepCopy()
		patch.Status.Phase = migrationv1alpha1.PhaseTestPending
		if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	logger.Info("TestRunning: waiting for test approval",
		"hint", "oc annotate ahvmigration "+mig.Name+" "+migrationv1alpha1.AnnotationTestApproved+"=true -n "+mig.Namespace)
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// handleTestPending: VM を停止して本番 VLAN に切り替える準備
func (r *AHVMigrationReconciler) handleTestPending(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ns := targetNS(mig)

	// VM を停止（running=false）
	allStopped := true
	for _, vmSt := range mig.Status.VMs {
		if vmSt.VMRef == "" {
			continue
		}
		vm := &unstructured.Unstructured{}
		vm.SetAPIVersion("kubevirt.io/v1")
		vm.SetKind("VirtualMachine")
		if err := r.Get(ctx, types.NamespacedName{Name: vmSt.VMRef, Namespace: ns}, vm); err != nil {
			continue
		}

		running, _, _ := unstructured.NestedBool(vm.Object, "spec", "running")
		if running {
			patch := vm.DeepCopy()
			if err := unstructured.SetNestedField(patch.Object, false, "spec", "running"); err == nil {
				logger.Info("Stopping VM before network switch", "name", vmSt.VMRef)
				_ = r.Patch(ctx, patch, client.MergeFrom(vm))
			}
			allStopped = false
		}

		// VMI が消えるまで待つ
		vmi := &unstructured.Unstructured{}
		vmi.SetAPIVersion("kubevirt.io/v1")
		vmi.SetKind("VirtualMachineInstance")
		if err := r.Get(ctx, types.NamespacedName{Name: vmSt.VMRef, Namespace: ns}, vmi); err == nil {
			allStopped = false
		}
	}

	if !allStopped {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	patch := mig.DeepCopy()
	patch.Status.Phase = migrationv1alpha1.PhaseSwitchingNetwork
	if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// handleSwitchingNetwork: 停止した VM の NIC を本番 NAD に切り替えて再起動
func (r *AHVMigrationReconciler) handleSwitchingNetwork(ctx context.Context, mig *migrationv1alpha1.AHVMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	ns := targetNS(mig)

	prism, err := NewPrismClient(ctx, r.Client, mig)
	if err != nil {
		return r.failMigration(ctx, mig, fmt.Sprintf("Prism client error: %v", err))
	}

	for vmIdx, vmSpec := range mig.Spec.VMs {
		if vmIdx >= len(mig.Status.VMs) {
			logger.Info("VMStatus entry missing, skipping", "vmIdx", vmIdx, "vm", vmSpec.Name)
			continue
		}
		vmSt := mig.Status.VMs[vmIdx]
		if vmSt.VMRef == "" {
			continue
		}

		vmInfo, err := prism.GetVMByName(ctx, vmSpec.Name)
		if err != nil {
			return r.failMigration(ctx, mig, fmt.Sprintf("VM %q Prism error: %v", vmSpec.Name, err))
		}

		vm := &unstructured.Unstructured{}
		vm.SetAPIVersion("kubevirt.io/v1")
		vm.SetKind("VirtualMachine")
		if err := r.Get(ctx, types.NamespacedName{Name: vmSt.VMRef, Namespace: ns}, vm); err != nil {
			return ctrl.Result{}, err
		}

		networks, interfaces := buildNetworkPatchToProd(mig, vmInfo, vmSpec)

		patchVM := vm.DeepCopy()
		_ = unstructured.SetNestedSlice(patchVM.Object, networks, "spec", "template", "spec", "networks")
		_ = unstructured.SetNestedSlice(patchVM.Object, interfaces, "spec", "template", "spec", "domain", "devices", "interfaces")
		_ = unstructured.SetNestedField(patchVM.Object, true, "spec", "running")

		logger.Info("Switching VM to production VLAN", "name", vmSt.VMRef)
		if err := r.Patch(ctx, patchVM, client.MergeFrom(vm)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patch VM %s network: %w", vmSt.VMRef, err)
		}
	}

	now := metav1.Now()
	patch := mig.DeepCopy()
	patch.Status.Phase = migrationv1alpha1.PhaseCompleted
	patch.Status.CompletionTime = &now
	patch.Status.CompletedVMs = int32(len(mig.Spec.VMs))
	setCondition(patch, migrationv1alpha1.ConditionTestApproved, metav1.ConditionTrue, "Approved", "Test validation passed")
	setCondition(patch, migrationv1alpha1.ConditionCompleted, metav1.ConditionTrue, "Completed",
		fmt.Sprintf("Migration completed after test validation: %d VMs on production VLAN", len(mig.Spec.VMs)))

	if err := r.Status().Patch(ctx, patch, client.MergeFrom(mig)); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Migration completed with test validation, VMs switched to production VLAN")
	return ctrl.Result{}, nil
}

// failMigration は移行全体を Failed フェーズに遷移させる
func (r *AHVMigrationReconciler) failMigration(ctx context.Context, mig *migrationv1alpha1.AHVMigration, reason string) (ctrl.Result, error) {
	log.FromContext(ctx).Error(fmt.Errorf(reason), "Migration failed")
	patch := mig.DeepCopy()
	patch.Status.Phase = migrationv1alpha1.PhaseFailed
	setCondition(patch, migrationv1alpha1.ConditionCompleted, metav1.ConditionFalse, "Failed", reason)
	return ctrl.Result{}, r.Status().Patch(ctx, patch, client.MergeFrom(mig))
}

// ensureSecret は Secret が存在しない場合のみ作成する
func (r *AHVMigrationReconciler) ensureSecret(ctx context.Context, secret *corev1.Secret) error {
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, secret)
	}
	return err
}

// replacePrismURLWithProxy は Prism API URL の host:port 部分を proxy ベース URL に置き換える
// 例: https://prism-central.example.com:9440/api/... → http://proxy.svc:9440/api/...
func replacePrismURLWithProxy(sourceURL, prismEndpoint, proxyURL string) string {
	// prismEndpoint から scheme://host:port を抽出
	if len(prismEndpoint) == 0 || len(proxyURL) == 0 {
		return sourceURL
	}
	// https://prism-central.example.com:9440/api/nutanix/v3/images/UUID/file
	// prismEndpoint: https://prism-central.example.com:9440
	// proxyURL: http://prism-proxy.vm-migration.svc:9440
	// → http://prism-proxy.vm-migration.svc:9440/api/nutanix/v3/images/UUID/file
	if !strings.HasPrefix(sourceURL, prismEndpoint) {
		return sourceURL
	}
	return strings.TrimRight(proxyURL, "/") + sourceURL[len(prismEndpoint):]
}

func setCondition(mig *migrationv1alpha1.AHVMigration, condType string, status metav1.ConditionStatus, reason, msg string) {
	now := metav1.Now()
	for i, c := range mig.Status.Conditions {
		if c.Type == condType {
			mig.Status.Conditions[i].Status = status
			mig.Status.Conditions[i].Reason = reason
			mig.Status.Conditions[i].Message = msg
			mig.Status.Conditions[i].LastTransitionTime = now
			return
		}
	}
	mig.Status.Conditions = append(mig.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
	})
}

// needsGuestPrep はいずれかのVMにゲストプリセット（SSH/WinRM）が設定されているか確認する
func needsGuestPrep(mig *migrationv1alpha1.AHVMigration) bool {
	for _, vm := range mig.Spec.VMs {
		if vm.GuestPrepMode == "ssh" && vm.GuestPrepConfig != nil {
			return true
		}
		if vm.GuestPrepMode == "winrm" && vm.GuestPrepWinRMConfig != nil {
			return true
		}
	}
	return false
}

func (r *AHVMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&migrationv1alpha1.AHVMigration{}).
		Complete(r)
}
