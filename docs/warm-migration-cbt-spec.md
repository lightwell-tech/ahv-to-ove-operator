# Warm Migration with CBT Delta Sync — Specification (as-built)

Status: **Implemented & E2E-validated** (2026-07-14)
Applies to: `ahv-to-ove-operator` v0.1.x / AOS 7.5.x / OpenShift Virtualization

This document specifies the CBT (Changed Region Tracking) based **delta sync** used by
warm migration to minimize cutover downtime. It is the "as-built" contract.

---

## 1. Goal

Warm migration copies a running VM's disks while it stays online (pre-copy), then does a
short cutover. CBT delta sync reduces the cutover copy to **only the blocks that changed**
since the last sync, so downtime approaches "final delta transfer time" instead of a full
re-copy.

## 2. Key constraint that shapes the design

Nutanix Prism **`GET /api/nutanix/v3/images/{uuid}/file` does NOT support HTTP Range**
(verified on AOS 7.5.1.2, all image types, PE-direct and via proxy): a `Range` header is
ignored and the endpoint returns `HTTP 200` streaming the **entire** file. There is no
partial-read HTTP API on PE (no `/data/read`, no `v0.8 vdisks`, no v4 API on PE).

Therefore the "read only the changed blocks" half of CBT **cannot** be done over HTTP.
It is done instead by mounting the snapshot's storage container over **NFS (read-only)**
and issuing random `pread` (`os.ReadAt`) at the changed offsets — the Nutanix analog of
VMware VDDK/NBD random block reads that MTV relies on.

## 3. Architecture

```
                 ┌──────────────── operator (reconciler) ────────────────┐
 changed_regions │  1. snapshot the running VM (v3 vm_snapshots)          │
 = WHAT changed  │  2. changed_regions(newSnap, baseSnap) → region list   │
                 │  3. spawn delta-sync Job per disk                      │
                 └───────────────────────────┬───────────────────────────┘
                                             │ regions (offset,len,type)
        NFS (RO) mount of snapshot container │      ┌── delta-sync Job (runner) ──┐
 os.ReadAt = READ changed blocks  ───────────┼─────►│  ReadAt(src @off) → WriteAt  │
        Prism v3 images/file is NOT used     │      │  (PVC disk.img @off)         │
                                             │      └──────────────────────────────┘
```

- **What changed**: `POST /api/nutanix/v3/data/changed_regions` with the snapshot
  `snapshot_file_path` (base + reference). Returns `region_list[]{offset,length,type}`
  where `type ∈ {REGULAR, ZEROED}`.
- **Read changed blocks**: the delta-sync Job NFS-mounts the snapshot's container
  read-only and `pread`s each REGULAR region; ZEROED regions are written as zeros.
- **Write target**: the CDI-created target PVC (`disk.img` in Filesystem mode, or the raw
  block device in Block mode), written at the same offsets (`WriteAt`).

## 4. CRD

```yaml
spec:
  source:
    endpoint:  "https://<PE>:9440"     # Prism Element
    peEndpoint: "https://<PE>:9440"
    warmMigration: true
    cbt:
      enabled: true                    # turn on CBT delta sync
      deltaSyncThresholdMB: 512        # converge when a round's delta < this (default 512)
      maxDeltaRounds: 10               # cap on pre-cutover rounds (default 10)
      nfsServer: "<host>"              # optional; default = PEEndpoint (else Endpoint) host
```

`nfsServer` is the NFS export host for the snapshot container. Omit it to derive the host
from `peEndpoint`/`endpoint`. Only override when NFS is served on a different address.

## 5. State machine

```
WarmPreSync → WarmSyncing → WarmDeltaSync ⟲ → WarmCutover → WarmFinalDelta → CreatingVMs → Completed
   snap0        full         (loop until          stop         final delta      build VM
   + image      pre-copy      converged)          source       (RPO=0)
   + CDI        (CDI)
```

| Phase | CBT behavior |
|-------|--------------|
| `WarmPreSync` | Create base snapshot `snap0` **before** imaging; full pre-copy via CDI (HTTP, unchanged). |
| `WarmSyncing` | CDI import of the full pre-copy image completes. |
| `WarmDeltaSync` | Loop: new snapshot → `changed_regions(new, base)` → **delta-sync Job (NFS)** → promote new snapshot as base. Converge when `delta < deltaSyncThresholdMB` or `maxDeltaRounds` reached. |
| `WarmCutover` | Shut the source VM down. |
| `WarmFinalDelta` | One final snapshot + delta (NFS) after shutdown → **RPO=0**; consistent with the powered-off state. |
| `CreatingVMs` → `Completed` | Build the KubeVirt VirtualMachine and finish. |

### 5.1 Per-VM delta round (`runDeltaRound`)

A round is a two-step per-VM state machine keyed on `status.vms[].pendingSnapshotUUID`:

1. **`pendingSnapshotUUID == ""` (round start)**
   - Create snapshot `snapN`; fetch its `snapshot_file_path`s.
   - For each disk: `changed_regions(snapN[d], base[d])`, sum bytes.
   - **Convergence (pre-cutover only)**: if `Σbytes ≤ threshold` **or** `deltaRounds ≥ max`,
     delete old base, promote `snapN` as base, mark `DeltaConverged`, proceed to cutover.
   - Otherwise: merge regions, write a ConfigMap, and spawn one delta-sync Job per disk.
     Record `pendingSnapshotUUID = snapN`, `syncJobRefs`, phase `DeltaSyncing`.
2. **`pendingSnapshotUUID != ""` (waiting)**
   - Poll the Job(s). On failure → fail migration. On success → delete Job/ConfigMap,
     delete the old base snapshot, promote `snapN` as the new base, `deltaRounds++`, and
     (pre-cutover) start the next round; (final) mark done.

Region merge: adjacent REGULAR regions within a 4 MiB gap are merged (reading a little
unchanged data is harmless and reduces request count). If the merged list would exceed the
ConfigMap size budget, the gap is widened (correctness unchanged).

`WarmFinalDelta` runs exactly one round with no convergence check (always syncs once so the
target is byte-consistent with the powered-off source → RPO=0).

## 6. delta-sync Job

Per disk, per round the operator creates:

- **ConfigMap** `delta-<mig>-<vmIdx>-<diskIdx>-r<round>` — the region list, one
  `"<offset> <length> <REGULAR|ZEROED>"` per line, mounted at `/config/regions`.
- **Job** of the same name running the operator image's `delta-sync` subcommand.

Job pod spec (essentials):

```yaml
serviceAccountName: ahv-delta-sync
securityContext (container):
  runAsUser: 107          # qemu: owns the CDI-created disk.img; snapshot files are world-readable
  runAsGroup: 107
  allowPrivilegeEscalation: false
  capabilities: {drop: [ALL]}
volumes:
  - name: regions  (configMap: <job name>)                        # mounted /config (RO)
  - name: src      (nfs: server=<nfsServer> path=/<container> RO)  # snapshot container
  - name: target   (pvc: <DataVolume of disk d>)                   # mounted /target (or block device)
args:
  delta-sync
  --source-file=/src/<snapshot_file_path minus container prefix>
  --regions=/config/regions
  --target=/target/disk.img         # or: --target=/dev/delta-target --block
backoffLimit: 3
ttlSecondsAfterFinished: 3600
```

### 6.1 Runner I/O contract (`deltasync.go`, subcommand `delta-sync`)

| Flag | Meaning |
|------|---------|
| `--source-file` | NFS-mounted snapshot vmdisk file (required) |
| `--regions` | region file path (default `/config/regions`) |
| `--target` | target file or block device (default `/target/disk.img`) |
| `--block` | target is a block device |
| `--workers` | concurrent region copies (default 4) |

Behavior: open source RO and target WO (no truncate). For each region — REGULAR ⇒
chunked `ReadAt(src, off)` → `WriteAt(out, off)` (4 MiB chunks); ZEROED ⇒ zero-write.
`*os.File.ReadAt/WriteAt` are offset-explicit and safe for the concurrent workers. Each
region retries up to 3× (idempotent). Final `fsync`. Exit non-zero on any region failure.

## 7. Prerequisites (operational)

1. **NFS whitelist** — the snapshot's storage container must allow the OCP node subnet.
   Non-whitelisted clients are **silently dropped** (mount hangs → Job `FailedMount`).
   Example (v2 `storage_containers` PUT): add `198.51.100.0/255.255.255.0` to
   `nfs_whitelist` (this sets `nfs_whitelist_inherited=false` on that container).
2. **SCC** — apply the dedicated `ahv-delta-sync` SCC and grant it to the ServiceAccount:
   ```bash
   oc apply -f config/rbac/delta-sync-scc.yaml
   oc adm policy add-scc-to-user ahv-delta-sync -z ahv-delta-sync -n <ns>
   ```
   This is required: the default `restricted-v2` and `anyuid` SCCs do **not** allow the
   `nfs` volume type, so the Job's Pods are never created (`FailedCreate` — the Job stalls
   silently with no Pod to inspect). The dedicated SCC allows only `nfs` + uid 107; it does
   not grant hostPath or privilege escalation (unlike `hostmount-anyuid`, which also works
   but is broader than necessary).
3. **Image pull** — `system:image-puller` RoleBinding so the Job (in the target ns) can
   pull the operator image from the internal registry (see `config/rbac/delta-sync-sa.yaml`).

## 8. Limitations / future work

- **Container-scoped whitelist.** Adding the node subnet flips the container to
  `inherited=false`; changes to the global whitelist no longer propagate to it.
- **PE-only.** CBT here uses PE v3 APIs (no Prism Central). The v4 Data Protection / CRT
  API is PC-only and was not required.
- **NFS transport.** iSCSI (Acropolis Block Services) is an alternative random-read
  transport (3260 reachable in the validated environment) if NFS is undesirable.

## 9. Validation

E2E on `rhel8-cbt` (AOS 7.5.1.2, `deltaSyncThresholdMB: 1`, `maxDeltaRounds: 3`):
`WarmPreSync → WarmSyncing (20 GB import) → WarmDeltaSync (1 round, NFS Job) → WarmCutover
→ WarmFinalDelta (NFS Job, ~1.9 MB, 7 s) → CreatingVMs → Completed`. Migrated VM booted:
IP assigned, `guestOSInfo=Red Hat Enterprise Linux`, `AgentConnected=True`, `Ready=True`.

## 10. References

- Code: `deltasync.go` (runner), `controllers/cbt_sync.go` (rounds/Job), `api/v1alpha1` (CRD)
