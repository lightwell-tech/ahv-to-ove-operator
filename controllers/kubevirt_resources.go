package controllers

import (
	"fmt"

	migrationv1alpha1 "github.com/lightwell-tech/ahv-to-ove-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var virtualMachineGVR = schema.GroupVersionResource{
	Group:    "kubevirt.io",
	Version:  "v1",
	Resource: "virtualmachines",
}

// vmTargetName は OVE 側の VM 名を返す（targetName 指定があればそちらを優先）
func vmTargetName(vmSpec migrationv1alpha1.VMSpec) string {
	if vmSpec.TargetName != "" {
		return vmSpec.TargetName
	}
	return vmSpec.Name
}

// buildVirtualMachine は DataVolume PVC を利用する KubeVirt VirtualMachine を生成する
func buildVirtualMachine(
	mig *migrationv1alpha1.AHVMigration,
	vmSpec migrationv1alpha1.VMSpec,
	vmIdx int,
	vmInfo *VMInfo,
) *unstructured.Unstructured {
	name := vmTargetName(vmSpec)
	ns := targetNS(mig)

	// ディスクとボリュームの定義
	disks := make([]interface{}, 0, len(vmInfo.DiskList))
	volumes := make([]interface{}, 0, len(vmInfo.DiskList))

	// AHV は Linux/Windows とも virtio-scsi ネイティブのため、ソース VM のドライバー構成を
	// そのまま活かせる scsi をデフォルトにする。virtio (virtio-blk) は Windows で
	// viostor サービス不在により INACCESSIBLE_BOOT_DEVICE になる（E2E実証: 2026-07-12）
	diskBus := vmSpec.DiskBus
	if diskBus == "" {
		diskBus = "scsi"
	}
	nicModel := vmSpec.NICModel
	if nicModel == "" {
		nicModel = "virtio"
	}

	for i := range vmInfo.DiskList {
		diskName := fmt.Sprintf("disk%d", i)
		dvn := dvName(mig.Name, vmIdx, i)

		disks = append(disks, map[string]interface{}{
			"name": diskName,
			"disk": map[string]interface{}{
				"bus": diskBus,
			},
		})
		volumes = append(volumes, map[string]interface{}{
			"name": diskName,
			"dataVolume": map[string]interface{}{
				"name": dvn,
			},
		})
	}

	// ネットワーク設定（Multus NAD）
	networks := make([]interface{}, 0)
	interfaces := make([]interface{}, 0)

	// keepMAC のデフォルトは true（AHV の MAC を引き継ぐ）
	keepMAC := vmSpec.KeepMAC == nil || *vmSpec.KeepMAC

	if len(vmInfo.NICList) > 0 {
		for i, nic := range vmInfo.NICList {
			ifName := fmt.Sprintf("nic%d", i)
			nadName := resolveNetwork(mig, nic.NetworkName, false)

			iface := map[string]interface{}{
				"name":   ifName,
				"bridge": map[string]interface{}{},
				"model":  nicModel,
			}
			if keepMAC && nic.MACAddress != "" {
				iface["macAddress"] = nic.MACAddress
			}
			interfaces = append(interfaces, iface)

			net := map[string]interface{}{
				"name": ifName,
				"multus": map[string]interface{}{
					"networkName": nadName,
				},
			}
			networks = append(networks, net)
		}
	} else {
		// NIC 情報なし → デフォルト Pod ネットワーク
		interfaces = append(interfaces, map[string]interface{}{
			"name":       "default",
			"masquerade": map[string]interface{}{},
		})
		networks = append(networks, map[string]interface{}{
			"name": "default",
			"pod":  map[string]interface{}{},
		})
	}

	// DataVolume のテンプレート参照
	dvTemplates := make([]interface{}, 0, len(vmInfo.DiskList))
	for i := range vmInfo.DiskList {
		dvn := dvName(mig.Name, vmIdx, i)
		storageClass, accessMode, volumeMode := resolveStorage(mig, "")
		dvTemplates = append(dvTemplates, map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": dvn,
			},
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"pvc": map[string]interface{}{
						"name":      dvn,
						"namespace": ns,
					},
				},
				"pvc": map[string]interface{}{
					"accessModes":      []interface{}{string(accessMode)},
					"volumeMode":       string(volumeMode),
					"storageClassName": storageClass,
				},
			},
		})
	}

	// UEFI 判定: 手動指定 > Prism 自動検出
	useUEFI := vmInfo.IsUEFI
	if vmSpec.UEFI != nil {
		useUEFI = *vmSpec.UEFI
	}

	domain := map[string]interface{}{
		"cpu": map[string]interface{}{
			"cores": int64(vmInfo.NumCPUs),
		},
		"resources": map[string]interface{}{
			"requests": map[string]interface{}{
				"memory": fmt.Sprintf("%dMi", vmInfo.MemoryMB),
			},
		},
		"devices": map[string]interface{}{
			"disks":      disks,
			"interfaces": interfaces,
		},
	}
	if useUEFI {
		domain["firmware"] = map[string]interface{}{
			"bootloader": map[string]interface{}{
				"efi": map[string]interface{}{
					"secureBoot": false,
				},
			},
		}
	}

	vm := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
				"labels":    ownerLabelsMap(mig),
				"annotations": map[string]interface{}{
					"migration.lightwell.co.jp/source-vm": vmSpec.Name,
					"migration.lightwell.co.jp/uefi":      fmt.Sprintf("%v", useUEFI),
				},
			},
			"spec": map[string]interface{}{
				"running": false,
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels": map[string]interface{}{
							"kubevirt.io/vm": name,
						},
					},
					"spec": map[string]interface{}{
						"domain":   domain,
						"networks": networks,
						"volumes":  volumes,
					},
				},
			},
		},
	}

	return vm
}

// resolveNetwork は AHV ネットワーク名 → OVE NAD 名 に変換する
// useTestTarget=true の場合は testTarget を優先する（テスト移行フェーズ用）
// source が一致しない場合は最初のマッピングをフォールバックとして使用する
func resolveNetwork(mig *migrationv1alpha1.AHVMigration, ahvNetwork string, useTestTarget bool) string {
	pickTarget := func(nm migrationv1alpha1.NetworkMapping) string {
		target := nm.Target
		if useTestTarget && nm.TestTarget != "" {
			target = nm.TestTarget
		}
		if nm.TargetNamespace != "" {
			return nm.TargetNamespace + "/" + target
		}
		return target
	}

	for _, nm := range mig.Spec.NetworkMappings {
		if nm.Source == ahvNetwork {
			return pickTarget(nm)
		}
	}
	// source 名が一致しない場合は最初のマッピングをフォールバック
	if len(mig.Spec.NetworkMappings) > 0 {
		return pickTarget(mig.Spec.NetworkMappings[0])
	}
	return "example-bridge"
}

// hasTestTarget はいずれかの NetworkMapping に testTarget が設定されているか確認する
func hasTestTarget(mig *migrationv1alpha1.AHVMigration) bool {
	for _, nm := range mig.Spec.NetworkMappings {
		if nm.TestTarget != "" {
			return true
		}
	}
	return false
}

// buildVirtualMachineWithNetwork は testTarget 指定有無を考慮して VM を構築する
func buildVirtualMachineWithNetwork(
	mig *migrationv1alpha1.AHVMigration,
	vmSpec migrationv1alpha1.VMSpec,
	vmIdx int,
	vmInfo *VMInfo,
	useTestTarget bool,
) *unstructured.Unstructured {
	// 元の buildVirtualMachine の networks を差し替える版
	// ネットワーク解決だけ差し替えて他は同じロジック
	vm := buildVirtualMachine(mig, vmSpec, vmIdx, vmInfo)
	if !useTestTarget {
		return vm
	}

	// テスト VLAN 用にネットワーク設定を上書き
	nicModel := vmSpec.NICModel
	if nicModel == "" {
		nicModel = "virtio"
	}
	keepMAC := vmSpec.KeepMAC == nil || *vmSpec.KeepMAC

	networks := make([]interface{}, 0)
	interfaces := make([]interface{}, 0)

	for i, nic := range vmInfo.NICList {
		ifName := fmt.Sprintf("nic%d", i)
		nadName := resolveNetwork(mig, nic.NetworkName, true)

		iface := map[string]interface{}{
			"name":   ifName,
			"bridge": map[string]interface{}{},
			"model":  nicModel,
		}
		if keepMAC && nic.MACAddress != "" {
			iface["macAddress"] = nic.MACAddress
		}
		interfaces = append(interfaces, iface)
		networks = append(networks, map[string]interface{}{
			"name": ifName,
			"multus": map[string]interface{}{
				"networkName": nadName,
			},
		})
	}

	// VMI テンプレートの networks/interfaces を差し替え
	spec, _, _ := unstructured.NestedMap(vm.Object, "spec", "template", "spec")
	spec["networks"] = networks
	domain, _, _ := unstructured.NestedMap(vm.Object, "spec", "template", "spec", "domain")
	devices, _, _ := unstructured.NestedMap(domain, "devices")
	devices["interfaces"] = interfaces
	domain["devices"] = devices
	spec["domain"] = domain
	_ = unstructured.SetNestedMap(vm.Object, spec, "spec", "template", "spec")

	return vm
}

// patchVMNetworkToProd は TestRunning の VM の NIC を本番 NAD に切り替えるパッチを返す
func buildNetworkPatchToProd(mig *migrationv1alpha1.AHVMigration, vmInfo *VMInfo, vmSpec migrationv1alpha1.VMSpec) ([]interface{}, []interface{}) {
	nicModel := vmSpec.NICModel
	if nicModel == "" {
		nicModel = "virtio"
	}
	keepMAC := vmSpec.KeepMAC == nil || *vmSpec.KeepMAC

	networks := make([]interface{}, 0)
	interfaces := make([]interface{}, 0)

	for i, nic := range vmInfo.NICList {
		ifName := fmt.Sprintf("nic%d", i)
		nadName := resolveNetwork(mig, nic.NetworkName, false) // 本番 NAD

		iface := map[string]interface{}{
			"name":   ifName,
			"bridge": map[string]interface{}{},
			"model":  nicModel,
		}
		if keepMAC && nic.MACAddress != "" {
			iface["macAddress"] = nic.MACAddress
		}
		interfaces = append(interfaces, iface)
		networks = append(networks, map[string]interface{}{
			"name": ifName,
			"multus": map[string]interface{}{
				"networkName": nadName,
			},
		})
	}
	return networks, interfaces
}
