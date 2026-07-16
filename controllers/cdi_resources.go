package controllers

import (
	"fmt"

	migrationv1alpha1 "github.com/lightwell-tech/ahv-to-ove-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var dataVolumeGVR = schema.GroupVersionResource{
	Group:    "cdi.kubevirt.io",
	Version:  "v1beta1",
	Resource: "datavolumes",
}

// dvStorageSize はディスクサイズに 10% のオーバーヘッドを加えた PVC 要求サイズを返す。
// CDI の scratch PVC はターゲット PVC と同サイズで作られるため、要求サイズ＝ディスク実サイズだと
// ext4 のメタデータ・予約領域分が足りず "no space left on device" で転送が失敗する
func dvStorageSize(sizeMB int64) resource.Quantity {
	return resource.MustParse(fmt.Sprintf("%dMi", sizeMB*110/100))
}

// dvName は DataVolume の名前を生成する（DNS label 63文字制限対応）
func dvName(migName string, vmIdx, diskIdx int) string {
	prefix := migName
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	return fmt.Sprintf("dv-%s-%d-%d", prefix, vmIdx, diskIdx)
}

// buildCDIAuthSecret は Prism Basic Auth 用の CDI DataVolume secretRef 対応 Secret を生成する
// CDI は HTTP source の認証に accessKeyId / secretKey キーを使用する
func buildCDIAuthSecret(mig *migrationv1alpha1.AHVMigration, user, pass string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cdiAuthSecretName(mig),
			Namespace: targetNS(mig),
			Labels:    ownerLabelsTyped(mig),
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"accessKeyId": user,
			"secretKey":   pass,
		},
	}
}

func cdiAuthSecretName(mig *migrationv1alpha1.AHVMigration) string {
	return fmt.Sprintf("%s-prism-auth", mig.Name)
}

// buildDataVolume は CDI DataVolume (unstructured) を生成する
// CDI が Prism の HTTP endpoint からイメージをダウンロードして PVC に変換する
func buildDataVolume(mig *migrationv1alpha1.AHVMigration, vmIdx int, disk DiskInfo, sourceURL string) *unstructured.Unstructured {
	name := dvName(mig.Name, vmIdx, disk.Index)
	ns := targetNS(mig)

	storageClass, accessMode, volumeMode := resolveStorage(mig, "")
	storageSize := dvStorageSize(disk.SizeMB)

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cdi.kubevirt.io/v1beta1",
			"kind":       "DataVolume",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
				"labels":    ownerLabelsMap(mig),
				"annotations": map[string]interface{}{
					// CDI に TLS 検証をスキップさせる（自己署名証明書対応）
					"cdi.kubevirt.io/storage.bind.immediate.requested": "true",
				},
			},
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"http": map[string]interface{}{
						"url":           sourceURL,
						"secretRef":     cdiAuthSecretName(mig),
						"certConfigMap": "prism-ca-cert",
					},
				},
				"pvc": map[string]interface{}{
					"accessModes": []interface{}{string(accessMode)},
					"volumeMode":  string(volumeMode),
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{
							"storage": storageSize.String(),
						},
					},
					"storageClassName": storageClass,
				},
			},
		},
	}
	return obj
}

// resolveStorage は StorageMappings から storageClass / accessMode / volumeMode を解決する
func resolveStorage(mig *migrationv1alpha1.AHVMigration, containerName string) (storageClass string, accessMode corev1.PersistentVolumeAccessMode, volumeMode corev1.PersistentVolumeMode) {
	for _, sm := range mig.Spec.StorageMappings {
		if sm.Source == containerName || containerName == "" {
			sc := sm.TargetStorageClass
			am := sm.AccessMode
			if am == "" {
				am = corev1.ReadWriteMany
			}
			vm := sm.VolumeMode
			if vm == "" {
				vm = corev1.PersistentVolumeBlock
			}
			return sc, am, vm
		}
	}
	// デフォルト: OVE 向けの一般的な設定
	return "ocs-storagecluster-ceph-rbd", corev1.ReadWriteMany, corev1.PersistentVolumeBlock
}

func targetNS(mig *migrationv1alpha1.AHVMigration) string {
	if mig.Spec.TargetNamespace != "" {
		return mig.Spec.TargetNamespace
	}
	return mig.Namespace
}

func ownerLabelsTyped(mig *migrationv1alpha1.AHVMigration) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":    "ahv-to-ove-operator",
		"migration.lightwell.co.jp/owner": mig.Name,
	}
}

func ownerLabelsMap(mig *migrationv1alpha1.AHVMigration) map[string]interface{} {
	return map[string]interface{}{
		"app.kubernetes.io/managed-by":    "ahv-to-ove-operator",
		"migration.lightwell.co.jp/owner": mig.Name,
	}
}

// buildBlankDataVolume は bare disk 用の空 DataVolume を生成する（source: blank）
func buildBlankDataVolume(mig *migrationv1alpha1.AHVMigration, vmIdx int, disk DiskInfo) *unstructured.Unstructured {
	name := dvName(mig.Name, vmIdx, disk.Index)
	ns := targetNS(mig)
	storageClass, accessMode, volumeMode := resolveStorage(mig, "")
	storageSize := dvStorageSize(disk.SizeMB)

	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cdi.kubevirt.io/v1beta1",
			"kind":       "DataVolume",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
				"labels":    ownerLabelsMap(mig),
				"annotations": map[string]interface{}{
					"migration.lightwell.co.jp/bare-disk":        "true",
					"migration.lightwell.co.jp/source-disk-uuid": disk.UUID,
				},
			},
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"blank": map[string]interface{}{},
				},
				"pvc": map[string]interface{}{
					"accessModes": []interface{}{string(accessMode)},
					"volumeMode":  string(volumeMode),
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{
							"storage": storageSize.String(),
						},
					},
					"storageClassName": storageClass,
				},
			},
		},
	}
}

// isDVSucceeded は DataVolume の phase が Succeeded かを返す
func isDVSucceeded(dv *unstructured.Unstructured) bool {
	phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase")
	return phase == "Succeeded"
}

// dvProgress は DataVolume の進捗率 (0-100) を返す
func dvProgress(dv *unstructured.Unstructured) int32 {
	prog, _, _ := unstructured.NestedString(dv.Object, "status", "progress")
	if prog == "" {
		return 0
	}
	var pct int32
	fmt.Sscanf(prog, "%d", &pct)
	return pct
}
