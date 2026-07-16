# AHV to OVE Migration Operator

[![CI](https://github.com/lightwell-tech/ahv-to-ove-operator/actions/workflows/ci.yaml/badge.svg)](https://github.com/lightwell-tech/ahv-to-ove-operator/actions/workflows/ci.yaml)
[![Go Report Card](https://goreportcard.com/badge/github.com/lightwell-tech/ahv-to-ove-operator)](https://goreportcard.com/report/github.com/lightwell-tech/ahv-to-ove-operator)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Operator Version](https://img.shields.io/badge/version-0.1.0-green)](https://github.com/lightwell-tech/ahv-to-ove-operator/releases)

A Kubernetes Operator that automates virtual machine migration from **Nutanix AHV** (Prism Central) to **OpenShift Virtualization** (KubeVirt/OVE).

> **Status**: Alpha — suitable for lab and PoC environments. Production use requires additional testing.

---

## Features

- **Three migration modes** to fit different workloads and risk tolerances
- **Warm migration** — disk copy while the source VM keeps running, brief downtime only at cutover
- **CBT delta sync** — optional Changed Region Tracking so warm cutover transfers only the changed blocks (not a full re-copy), reading them over NFS since Prism's image download has no HTTP Range support — see [docs/warm-migration-cbt-spec.md](docs/warm-migration-cbt-spec.md)
- **GuestPrep / SSH** — injects virtio drivers into the source VM's initramfs before migration, preventing boot failures caused by hypervisor differences
- **Console Plugin** — built-in OpenShift Console UI with card layout, phase stepper, per-VM progress bars, and a guided creation form
- **Cutover control** — optional `pauseBeforeCutover` for manual approval before the final switchover

---

## Migration Modes

| Mode | Downtime | Data Consistency | Use Case |
|------|----------|-----------------|----------|
| **Warm** (`warmMigration: true`) | Seconds (cutover only) | Good | Production VMs that must stay online during copy |
| **Shutdown** (`shutdownBeforeMigration: true`) | Full copy duration | Highest | Non-critical VMs, highest data integrity |
| **GuestPrep / SSH** (`guestPrepMode: ssh`) | Seconds (cutover only) | Good | VMs missing virtio drivers in initramfs |

### Why GuestPrep?

When migrating from AHV to KubeVirt, the disk controller changes from AHV's virtio-scsi to KubeVirt's virtio-blk (or virtio-scsi with different UUIDs). If the source VM's initramfs was built without the target driver, the VM will drop into emergency shell on first boot after migration.

Which combinations boot:

| Disk on AHV | Drivers in guest initramfs | Disk bus on KubeVirt | Boots? |
|---|---|---|---|
| SCSI (qemu-scsi / pvscsi) | `ahci` / `ata_piix` | `virtio` (virtio-blk) | No |
| SCSI (qemu-scsi / pvscsi) | `ahci` / `ata_piix` | `scsi` (virtio-scsi) | No — `virtio_scsi` module absent |
| virtio-scsi | `virtio_scsi` | `scsi` (virtio-scsi) | Yes |

So copying the disk alone is not enough — the guest needs the target driver in its initramfs.
(Nutanix Move and Red Hat MTV solve the same problem with `virt-v2v`.)

GuestPrep solves this by SSH-ing into the source VM _before_ copying the disk and running:

```bash
dracut --add-drivers "virtio virtio_blk virtio_scsi" -f
```

---

## Architecture

```
User applies AHVMigration CR
        │
        ▼
┌─────────────────────────────────────────────┐
│           AHVMigration Reconciler            │
│                                             │
│  Pending ──► [GuestPrepping] ──►            │
│  WarmPreSync ──► WarmSyncing ──►            │
│  [ReadyForCutover] ──► WarmCutover ──►      │
│  ImportingDisks ──► CreatingVMs ──►         │
│  Completed                                  │
│                        └──► Failed          │
└─────────────────────────────────────────────┘
        │                       │
        ▼                       ▼
  Prism Central v3 API    CDI DataVolume
  (CreateImageFromDisk)   (HTTP import)
        │                       │
        └───────────────────────┘
                    │
                    ▼
          KubeVirt VirtualMachine
```

**Key components:**
- **Prism Client** — calls Prism Central v3 API to fetch VM info, power control, and create disk images
- **CDI DataVolume** — uses Containerized Data Importer to HTTP-stream disk images into PVCs
- **KubeVirt VM** — creates `VirtualMachine` objects from the imported PVCs
- **Console Plugin** — dynamic OCP console plugin served as a separate deployment

---

## Prerequisites

| Component | Minimum Version | Notes |
|-----------|----------------|-------|
| OpenShift | 4.13+ | OCP or OKD (matches `minKubeVersion: 1.26`) |
| OpenShift Virtualization | 4.13+ | KubeVirt operator |
| CDI (Containerized Data Importer) | 1.58 | Usually bundled with OCP Virt |
| Nutanix Prism Central | 2022.4+ | v3 API must be reachable from cluster |
| Go (for local build) | 1.23 | |
| Node.js (for plugin build) | 18 | |

---

## Installation

### Option A — OLM / OperatorHub

> **Not available yet.** The operator is not published to OperatorHub as of v0.1.0 — use **Option B** below.
> Once listed, install it from the cluster's OperatorHub UI, or with a Subscription against the
> `community-operators` catalog:

```bash
# Create a Subscription (after the operator is listed in OperatorHub)
kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: ahv-to-ove-operator
  namespace: ahv-to-ove-operator-system
spec:
  channel: alpha
  name: ahv-to-ove-operator
  source: community-operators
  sourceNamespace: openshift-marketplace
EOF
```

### Option B — Manual (Kustomize / kubectl)

```bash
# 1. Install CRD and RBAC
make deploy

# 2. Build and run the operator (local)
make run

# OR build a container image and deploy
make docker-build docker-push IMG=quay.io/<your-org>/ahv-to-ove-operator:latest
```

---

## Quick Start

### 1. Create a Prism Central credentials Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: prism-credentials
  namespace: ahv-to-ove-operator-system
stringData:
  user: admin
  password: YOUR_PRISM_PASSWORD
```

```bash
kubectl apply -f prism-secret.yaml
```

### 2. Create an AHVMigration resource

```yaml
apiVersion: migration.lightwell.co.jp/v1alpha1
kind: AHVMigration
metadata:
  name: my-first-migration
  namespace: ahv-to-ove-operator-system
spec:
  source:
    endpoint: https://prism-central.example.com:9440
    secretRef:
      name: prism-credentials
    insecure: true                   # set false if using a valid TLS certificate
    warmMigration: true              # copy disk while VM runs
    pauseBeforeCutover: false        # set true for manual cutover approval

  vms:
    - name: my-vm-on-ahv            # VM name as shown in Prism
      targetName: my-vm-on-ove      # desired name in OpenShift (optional)

  networkMappings:
    - source: VLAN_100              # AHV network/VLAN name
      target: ovs-bridge            # OCP NetworkAttachmentDefinition name

  storageMappings:
    - source: ""                    # AHV storage container (empty = default)
      targetStorageClass: ocs-storagecluster-ceph-rbd
      accessMode: ReadWriteMany
      volumeMode: Block

  targetNamespace: default          # namespace where migrated VMs are created
```

```bash
kubectl apply -f my-migration.yaml
kubectl get ahvmigration -n ahv-to-ove-operator-system -w
```

### 3. Monitor progress

```bash
# Phase and VM count
kubectl get ahvmigration my-first-migration -n ahv-to-ove-operator-system

# Per-VM progress
kubectl get ahvmigration my-first-migration -n ahv-to-ove-operator-system -o jsonpath='{.status.vms}' | jq .

# DataVolume import progress
kubectl get datavolume -n default
```

---

## Migration Mode Examples

### Warm Migration (minimize downtime)

```yaml
spec:
  source:
    warmMigration: true
    pauseBeforeCutover: true    # wait for manual approval at ReadyForCutover phase
```

Approve cutover from the OpenShift Console or via annotation:

```bash
kubectl annotate ahvmigration my-migration \
  migration.lightwell.co.jp/cutover-approved=true \
  -n ahv-to-ove-operator-system
```

### Shutdown Migration (highest consistency)

```yaml
spec:
  source:
    warmMigration: false
    shutdownBeforeMigration: true
```

### GuestPrep / SSH (virtio driver injection)

Use this when the source VM might be missing virtio drivers in its initramfs — common for VMs originally installed on bare metal or VMware and later moved to AHV.

```yaml
spec:
  source:
    warmMigration: true
  vms:
    - name: my-vm-on-ahv
      targetName: my-vm-on-ove
      guestPrepMode: ssh
      guestPrepConfig:
        sshSecretRef:
          name: my-vm-ssh-secret    # Secret with host/user/password or privateKey
        preMigrationScript: |
          set -e
          dracut --add-drivers "virtio virtio_blk virtio_scsi" -f
          grubby --args="rd.driver.pre=virtio_blk rd.driver.pre=virtio_scsi" --update-kernel=ALL
        timeoutSeconds: 300
```

SSH Secret format:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-vm-ssh-secret
  namespace: ahv-to-ove-operator-system
stringData:
  host: "192.168.1.10:22"
  user: root
  password: "vm-root-password"
  # privateKey: |
  #   -----BEGIN OPENSSH PRIVATE KEY-----
  #   ...
```

---

## CRD Reference

### AHVMigration Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `source.endpoint` | string | ✅ | Prism Central HTTPS URL (e.g. `https://pc.example.com:9440`) |
| `source.secretRef.name` | string | ✅ | Secret with `user` and `password` keys |
| `source.insecure` | bool | | Skip TLS verification (default: false) |
| `source.cdiProxyURL` | string | | Proxy URL for CDI to reach Prism (useful when Prism is not directly reachable from CDI pods) |
| `source.peEndpoint` | string | | Prism **Element** HTTPS URL. Used for power operations (v2 API) when the PC v3 PUT is unsupported |
| `source.peSecretRef.name` | string | | Secret for Prism Element (defaults to `source.secretRef`) |
| `source.warmMigration` | bool | | Copy disk while VM is running (default: false) |
| `source.shutdownBeforeMigration` | bool | | Power off VM before copying (default: false) |
| `source.pauseBeforeCutover` | bool | | Pause at ReadyForCutover phase for manual approval (default: false) |
| `source.warmFinalFullSync` | bool | | Warm only: after cutover, re-image and full-copy again so writes made during pre-copy are not lost — RPO=0 (default: **true**). Ignored when `source.cbt.enabled` is true |
| `source.cbt.enabled` | bool | | Enable CBT delta sync: transfer only changed blocks instead of a full re-copy at cutover. See [CBT spec](docs/warm-migration-cbt-spec.md) and its extra prerequisites (default: false) |
| `source.cbt.deltaSyncThresholdMB` | int | | Converge once a round's delta is below this (default: 512) |
| `source.cbt.maxDeltaRounds` | int | | Max pre-cutover delta rounds (default: 10) |
| `source.cbt.nfsServer` | string | | NFS host exporting the snapshot's storage container (default: host of `peEndpoint`, else `endpoint`) |
| `vms[].name` | string | ✅ | Source VM name in Prism |
| `vms[].targetName` | string | | Target VM name in OpenShift (defaults to source name) |
| `vms[].guestPrepMode` | string | | `none`, `ssh`, or `winrm` (default: none) |
| `vms[].guestPrepConfig.sshSecretRef.name` | string | | Secret with SSH credentials (`host`, `user`, `privateKey` or `password`) |
| `vms[].guestPrepConfig.preMigrationScript` | string | | Shell script to run on source VM before migration |
| `vms[].guestPrepConfig.timeoutSeconds` | int | | SSH script timeout (default: 300) |
| `vms[].guestPrepWinRMConfig.winrmSecretRef.name` | string | | Secret with WinRM credentials (`host`, `username`, `password`) |
| `vms[].guestPrepWinRMConfig.preMigrationScript` | string | | PowerShell script (default: built-in virtio-win driver install) |
| `vms[].guestPrepWinRMConfig.virtIOWinISOPath` | string | | virtio-win ISO path inside the VM (default: `C:\virtio-win.iso`) |
| `vms[].guestPrepWinRMConfig.timeoutSeconds` | int | | Script timeout (default: 600) |
| `vms[].guestPrepWinRMConfig.useHTTPS` | bool | | Use WinRM over HTTPS / port 5986 (default: false) |
| `vms[].uefi` | bool | | Boot the target VM with UEFI firmware. Omit to auto-detect from the Prism disk partitioning |
| `vms[].keepMAC` | bool | | Carry the AHV MAC address over to the KubeVirt VM (default: true). `false` lets KubeVirt assign one |
| `vms[].diskBus` | string | | `scsi` (default, recommended), `virtio`, or `sata`. AHV guests are virtio-scsi native; `virtio` (virtio-blk) can fail to boot Windows |
| `vms[].nicModel` | string | | `virtio` (default for Linux), `e1000e` (recommended for Windows), or `rtl8139` |
| `networkMappings[].source` | string | ✅ | Source AHV network name |
| `networkMappings[].target` | string | ✅ | Target OCP NetworkAttachmentDefinition name |
| `networkMappings[].testTarget` | string | | NAD to boot on first for a test run (TestRunning phase); after approval the VM switches to `target`. Omit to skip the test phase |
| `networkMappings[].targetNamespace` | string | | Namespace of the NAD |
| `storageMappings[].source` | string | | Source AHV storage container name |
| `storageMappings[].targetStorageClass` | string | ✅ | Target Kubernetes StorageClass |
| `storageMappings[].accessMode` | string | | PVC access mode (default: ReadWriteMany) |
| `storageMappings[].volumeMode` | string | | PVC volume mode (default: Block) |
| `targetNamespace` | string | | Namespace for created VMs (default: operator namespace) |

### AHVMigration Status

| Field | Description |
|-------|-------------|
| `phase` | Current migration phase (see Phase Reference below) |
| `totalVMs` | Total number of VMs in this migration |
| `completedVMs` | Number of successfully migrated VMs |
| `failedVMs` | Number of failed VMs |
| `startTime` | Migration start timestamp (RFC3339) |
| `completionTime` | Migration completion timestamp (RFC3339) |
| `vms[].name` | VM name |
| `vms[].phase` | Per-VM phase |
| `vms[].progress` | Per-VM import progress (0–100) |
| `vms[].vmRef` | Created VirtualMachine name in OCP |
| `vms[].error` | Error message if VM migration failed |

### Phase Reference

| Phase | Description |
|-------|-------------|
| `Pending` | CR accepted, fetching VM info from Prism |
| `GuestPrepping` | Running pre-migration script on source VM via SSH |
| `WarmPreSync` | Creating Prism disk images from running VMs |
| `WarmSyncing` | CDI importing disk images (warm path) |
| `WarmDeltaSync` | (CBT) Looping snapshot → changed_regions → NFS delta sync until converged |
| `ReadyForCutover` | Waiting for cutover approval (when `pauseBeforeCutover: true`) |
| `WarmCutover` | Performing final cutover (shutting down source, final sync) |
| `WarmFinalDelta` | (CBT) Final post-shutdown delta sync for RPO=0 |
| `ImportingDisks` | CDI importing disk images (shutdown path) |
| `CreatingVMs` | Creating KubeVirt VirtualMachine objects |
| `Completed` | All VMs migrated successfully |
| `Failed` | Migration failed — see `status.vms[].error` for details |

---

## Console Plugin

The operator ships an OpenShift Console dynamic plugin that provides:

- **Migration list** with card layout, colored phase indicators, and summary badges
- **Migration detail** with phase stepper, per-VM progress bars, and cutover approval button
- **Create form** with guided wizard (mode selection, VM list, network/storage mapping)

> **Note:** the Console Plugin is **not** shipped in the v0.1.x OLM bundle. Install it manually with
> `console-plugin/config/deploy.yaml` (see that file), then enable it on the `Console` CR. The operator is
> fully usable without it via the OLM-generated form under *Installed Operators* or plain YAML/`oc`.

---

## Directory Structure

```
ahv-to-ove-operator/
├── main.go                              # Operator entry point + `delta-sync` subcommand dispatch
├── deltasync.go                         # delta-sync runner (NFS ReadAt → PVC WriteAt)
├── api/v1alpha1/
│   ├── ahvmigration_types.go            # CRD type definitions
│   ├── groupversion_info.go
│   └── zz_generated.deepcopy.go
├── controllers/
│   ├── ahvmigration_controller.go       # Main reconciler / phase state machine
│   ├── cbt_sync.go                      # CBT delta rounds + delta-sync Job builder
│   ├── cdi_resources.go                 # DataVolume / CDI resources
│   ├── kubevirt_resources.go            # KubeVirt VM manifest builder
│   ├── guest_prep.go                    # SSH / WinRM GuestPrep
│   ├── prism_client.go                  # Prism v3/v2 API client
│   └── helpers_test.go                  # Unit tests
├── console-plugin/                      # OpenShift Console dynamic plugin (not bundled in v0.1.x)
│   └── src/components/
│       ├── MigrationListPage.tsx
│       ├── MigrationDetailPage.tsx
│       └── MigrationCreatePage.tsx
├── bundle/                              # OLM bundle (OperatorHub)
│   ├── manifests/
│   │   ├── ahv-to-ove-operator.clusterserviceversion.yaml
│   │   └── migration.lightwell.co.jp_ahvmigrations.yaml
│   └── metadata/annotations.yaml
├── config/
│   ├── crd/bases/                       # CRD YAML
│   ├── manager/deployment.yaml          # Operator Deployment
│   ├── rbac/                            # RBAC + delta-sync ServiceAccount
│   └── samples/                         # Example AHVMigration resources
└── docs/
    └── warm-migration-cbt-spec.md       # Warm migration + CBT delta sync specification
```

---

## Development

```bash
# Run locally against a live cluster
make run

# Build binary
make build

# Run tests
go test ./...

# Build and push container image
make docker-build docker-push IMG=<registry>/<image>:<tag>

# Build OLM bundle
make bundle

# Validate OLM bundle (requires operator-sdk)
make bundle-validate
```

---

## Troubleshooting

### VM boots into dracut emergency shell after migration

**Symptom:** `/dev/XXX/root does not exist`, empty `/sys/block/`

**Cause:** The source VM's initramfs does not contain the virtio driver required by KubeVirt.

**Fix:** Use `guestPrepMode: ssh` with a dracut rebuild script before migrating:

```bash
dracut --add-drivers "virtio virtio_blk virtio_scsi" -f
```

### Migration stuck at WarmSyncing

Check CDI DataVolume progress:

```bash
kubectl get datavolume -n <target-namespace>
kubectl describe datavolume <dv-name> -n <target-namespace>
```

Common causes:
- Prism image creation still in progress (Prism can take several minutes)
- Network connectivity between CDI pod and Prism endpoint
- `cdiProxyURL` needed when Prism is not directly reachable from CDI pods

### Operator not processing new migrations

The operator must be running. Check:

```bash
# If deployed via OLM
kubectl get pods -n ahv-to-ove-operator-system

# If running locally
make run
```

---

## Contributing

Contributions are welcome! Please read our [Contributing Guide](CONTRIBUTING.md) and submit pull requests to the `main` branch.

Areas especially looking for contributions:
- **Unit tests** for reconciler logic
- **`virt-v2v` conversion mode** — automated driver conversion for guests that cannot be reached over SSH/WinRM
- **Console Plugin packaging** — ship the UI in the OLM bundle (it is manual-install in v0.1.x)
- **iSCSI transport** for CBT delta reads, as an alternative to NFS

---

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
