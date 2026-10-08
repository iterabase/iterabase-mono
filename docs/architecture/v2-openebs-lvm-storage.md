# Platform V2 OpenEBS LVM LocalPV storage

Status: current repository implementation contract for HOR-545/HOR-557 and approved `DES-HOR-545-01` / `DES-HOR-545-02` / `DES-HOR-545-03` / `DES-HOR-545-05` / `DES-HOR-545-07`, recorded canonically in Obsidian `Platform V2 — OpenEBS LVM LocalPV Storage`. The same-day pre-release `DES-HOR-545-04` snapshot amendment is superseded historical evidence only.

## Supported topology

Platform V2 supports exactly one schedulable K3s server node and one or more explicitly selected blank stable whole data disks. Forge prepares only physical volumes and one fixed thick volume group, `iterabase-data`. Chart-owned OpenEBS LVM LocalPV `1.10.0` creates every platform LV, XFS filesystem, mount, and claim lifecycle.

K3s local-path/default/root-backed storage, Longhorn, RWX, NFS/iSCSI, thin provisioning, shrink, claim-replacing resize, alternate/BYO classes, existing PV/VG/filesystem adoption, disk-set/VG extension or replacement, migration, and multi-node/HA are unsupported. Both managed classes support only grow-in-place online XFS/LVM expansion within real free extents already present in `iterabase-data`.

## Forge device and transaction contract

`spec.dataStorage.devices` is required, non-empty, unique, lexically ordered, immutable, and limited to `/dev/disk/by-id/...` whole-disk identities. Interactive init selects one or more; repeated `--data-storage-device`, comma-separated `FORGE_DATA_STORAGE_DEVICES`, and direct YAML represent the same canonical set. Explicit selection is the sole first-write authorization. No force, wipe, adopt, extend, “format anyway,” automatic candidate, or root fallback exists.

Before disk mutation and immediately before each `pvcreate`, Forge checks the complete set with bounded required probes:

- stable link, resolved device, model/serial/WWN, size, non-removable whole-device identity, and distinctness;
- no partition table/partition, unsupported mapper/loop/RAID/network device, foreign holder, mount, swap, system/root/boot/EFI/K3s backing, or raw process consumer;
- no recognized filesystem, LVM, RAID, crypt, or partition signature on a not-yet-created candidate;
- no read, parse, `/proc` enumeration, descriptor, replacement, topology, or identity uncertainty.

Forge never reads every device byte. Arbitrary non-signature bytes are not a secure-erasure proof and do not authorize adoption.

After read-only preflight, Forge installs/verifies `lvm2` and XFS tools without loading or persisting `dm-snapshot`; the superseded pre-release module configuration is converged safely to absence. A root-owned `0600`, atomic, file-and-directory-fsynced receipt at `/var/lib/iterabase/data-storage.receipt` binds contract `HOR-545/v2`, install, canonical paths, hardware/size, planned PV UUIDs, a unique Forge ownership tag, fixed VG name, completed PV count, observed VG UUID once created, and transaction stage. It also records each by-id link's current kernel path, but does not bind it: kernel names such as `nvme0n1` can change across reboots, so every run re-resolves the stable link and refuses only on hardware, size, PV/VG, tag, or membership drift (DES-HOR-616-01).

The receipt and ownership tag are durable before `pvcreate`. Planned PV UUIDs plus exact metadata make a crash after command success but before receipt advancement recognizable without adopting a foreign PV. Forge creates every PV with its planned UUID, then uses supported `vgcreate --addtag` to atomically create exactly one `iterabase-data` VG with the receipt-owned tag and its LVM-generated UUID. It verifies the exact tag/PV/member/name combination and fsyncs the observed UUID into the receipt before any later mutation or K3s/substrate/platform handoff. Reconcile, status, and purge accept only exact receipt tag/PV/VG UUID/name/membership. Forge reports bounded total/free capacity and never creates an LV, filesystem, mount, or fstab entry.

An installed K3s whose service does not include `--disable local-storage` is immutable incompatible state and requires clean destroy/rebuild before any data-storage mutation. Fresh installs always add that disablement.

## Companion and content authority

`lvm-storage-substrate` is a published companion with the same version as `iterabase-platform` and `cert-manager-substrate`. It wraps the direct reviewed archive:

- chart: OpenEBS `lvm-localpv` `1.10.0`;
- URL: `https://openebs.github.io/lvm-localpv/lvm-localpv-1.10.0.tgz`;
- SHA-256: `3ad766c56d4a0ab0f3f2baaeb726a4554d1f51bb485f1cef00846cf1d82a179d`.

`.github/inputs/remote-content.json` is authority for that archive and every runtime image digest. A fail-closed deterministic packaging transform removes the upstream CSI snapshot CRDs, snapshotter/controller templates, snapshot values, broad snapshot role, and all `LVMSnapshot` write authority before the dependency enters the companion. Per `DES-HOR-545-05`, it retains only the reviewed `lvmsnapshots.local.openebs.io` schema and driver `list`/`watch` needed by the pinned ordinary volume-deletion guard. The companion disables analytics, pins every required volume controller/node sidecar by digest, configures K3s's actual `/var/lib/kubelet` CSI registration/mount root, and renders the volume/node CRDs plus that inert schema. The wrapper replaces upstream combined controller RBAC with an exact volume role and the same read-only exception.

Forge applies certificate substrate, LVM substrate, then platform. It waits boundedly for the volume/node and inert `LVMSnapshot` CRDs, deny-all creation admission guard, zero `LVMSnapshot` instances, exact driver `list`/`watch`, volume controller, node DaemonSet, `local.csi.openebs.io`, the exact one-node CSINode registration and topology keys, both StorageClasses, CSI/user snapshot-surface absence, and one LVMNode reporting receipt-matching `iterabase-data`. Any local-path/default class, missing/extra class, broader snapshot authority, nonzero `LVMSnapshot` set, CSI/user snapshot surface, contradictory VG, or unavailable volume controller/node/CSI authority fails closed.

## Exact StorageClasses

Both classes are non-default, grow-only-expandable, `ReadWriteOnce`, `WaitForFirstConsumer`, `Delete`, XFS, and thick. Their provisioner is `local.csi.openebs.io` and parameters include:

```yaml
storage: lvm
vgpattern: ^iterabase-data$
fsType: xfs
thinProvision: "no"
```

- `iterabase-lvm-xfs` adds `shared: "no"` and is explicit on every chart-generated data PVC: PostgreSQL, MinIO, persistent observability components, and any future enabled chart data claim.
- `iterabase-agentpool-lvm-xfs` adds `shared: "yes"` and is authorized only for one claim per AgentPool. `shared: yes` allows multiple pods on the same node to mount the RWO filesystem; it does not create RWX or cross-node behavior.

A provisioned claim may change only `spec.resources.requests.storage`, and only upward. Growth preserves PVC/PV/OpenEBS `LVMVolume`/LV/filesystem identity, topology, owner, and bytes. It never authorizes claim recreation, copy/cutover, disk-set/VG mutation, thin overcommit, alternate classes, or root fallback.

## ModelBackend managed serving storage

`ModelBackend.spec.persistentVolumes[]` is the reusable serving-storage declaration. Every entry requires a unique DNS-label name, unique canonical absolute mount path, explicit `iterabase-lvm-xfs`, and positive desired size. The controller creates one deterministic owner-referenced RWO/Filesystem PVC per entry and refuses to adopt an unrelated deterministic-name claim. A claim-set digest plus per-claim declaration annotations make names, paths, class, access mode, and volume mode immutable after provisioning; the only supported mutation patches the same request upward.

Pending `WaitForFirstConsumer` claims remain schedulable so the first serving pod can bind them. ModelBackend/catalogue health stays false while a required claim, class, PVC/PV/OpenEBS identity, capacity, or resize condition is missing or unconverged. Safe online resize retains the serving pod so Kubelet can grow mounted XFS, but the gateway receives no healthy route until all claims converge. A declared `/data/hf-cache` backs the existing `HF_HOME`; `/cache` is a generic mount. Without a managed HF declaration the controller uses an ephemeral `emptyDir`, never a hostPath. Explicit raw volumes remain available for unrelated file overlays and may not collide with managed names/paths; the existing disposable `container-tmp` hostPath remains allowed and is never represented as HF/LMCache persistence. Owned claims retain ordinary owner-GC plus StorageClass `Delete` reclamation when the ModelBackend is deleted.

Managed serving claims are single-replica in this release. Multi-replica/HA, retention, adoption, migration, copy/cutover, backup, restore, and first-class model-cache administration remain unsupported.

## Inert provider dependency; snapshot lifecycle absent

Platform V2 installs no `dm-snapshot` module/configuration, CSI VolumeSnapshot CRD, `VolumeSnapshotClass`, snapshot controller, CSI snapshotter sidecar/image, user/operator snapshot authority, or supported snapshot lifecycle. `DES-HOR-545-05` permits exactly one inert dependency: the `lvmsnapshots.local.openebs.io` CRD and `list`/`watch` for the controller and node service accounts that host the pinned OpenEBS driver. A fail-closed admission policy denies every `LVMSnapshot` creation; neither service account has `get`, `create`, `update`, `patch`, or `delete`; and readiness requires zero instances. Static packaging, runtime readiness, claim deletion, reapply, and real-machine evidence prove that exact boundary. OPP-005 remains the sole authority for any future recovery mechanism.

## AgentPool validation and isolation

The API fixes `spec.sandbox.storageClassName` to `iterabase-agentpool-lvm-xfs`, access to `ReadWriteOnce`, and the generated claim to explicit Filesystem mode. Size is desired grow-only thick capacity, not local-path planning metadata.

The controller validates the class before creating a claim. An unbound `WaitForFirstConsumer` claim remains mount-capable so the first worker can schedule. Once bound, readiness requires:

- one exact RWO/Filesystem/Delete PV with `local.csi.openebs.io`, `fsType: xfs`, non-empty volume handle, `openebs.io/volgroup: iterabase-data`, and exact OpenEBS local-node affinity;
- one unique Ready LVMVolume matching that handle, `volGroup`, `vgPattern`, `shared: yes`, `thinProvision: no`, and owner node;
- the owner LVMNode reporting `iterabase-data` with a non-empty UUID, no missing PVs, and no thin pool.

Class, access/volume mode, CSI, VG, OpenEBS identity, or topology drift withdraws readiness and quiesces workers without automatic turn/effect replay. A size increase instead patches only the same PVC request and records a resize timestamp. While PVC request/capacity, PV capacity, Ready LVMVolume capacity, filesystem growth, or the required post-resize mounted `statfs` observation has not converged, `StorageReady` and durable fresh-credit authorization remain false while mounted workers and active turns are retained. Shrink is refused without replacing the claim or deleting workers. Transient or unknown StorageClass/PV/LVMVolume/LVMNode observations likewise withdraw readiness/credit but retain healthy workers; only positively observed unsafe identity/topology drift authorizes destructive quiescence. The manager records storage authorization independently of PVC-capacity hysteresis, and dispatch checks it durably before consuming an idle credit; a request alone cannot reopen authority.

All trusted root supervisors in one pool may access that whole claim; separate pools receive separate PVC/PV/LVMVolume identities. Disposable children retain stable distinct session UID=GID, cleared groups/capabilities, `no_new_privs`, umask `0077`, root-owned `0711` pool root, session-owned `0700` trees, sibling denial, and path containment. `DES-HOR-538-03` remains authoritative for constrained cert-manager CSI AtomicWriter key validation, supervisor mTLS, and child `EACCES`.

## Capacity and failure

The harness measures and performs real write/fsync/rename/unlink health against its pool PVC. A claim-local state file and `runtime.workspace_capacity_state` row keyed by materialized pool preserve hysteresis:

- warn below 25% free;
- withhold all fresh credits for that pool at or below 20%;
- reopen only at or above 25%;
- never abort an active turn solely for threshold crossing.

Different AgentPool PVCs remain independent. Real I/O/fsync/mount/ownership failure fences without replay. Dispatch metrics carry a bounded `pool` label and manager conditions query only the matching pool.

The OpenEBS node metric endpoint supplies `lvm_vg_free_size_bytes{name="iterabase-data"}` and `lvm_vg_total_size_bytes{name="iterabase-data"}`. Chart-owned monitoring reports aggregate VG pressure independently and alerts on a managed PVC that remains Pending. Thick-capacity exhaustion for a new claim or resize must fail visibly and actionably while preserving the existing claim/volume identity, current usable bytes, mounted observer, and unrelated claims. There is no capacity manufacture, disk/VG extension, thin overcommit, alternate class, or root fallback. Soft-deleting a pool removes its durable capacity row, process-local hysteresis entry, and every per-pool Prometheus label series; a same-UUID revival starts fail-closed until its new PVC is observed.

## Claim deletion, destroy, and purge

A deleted claim uses `Delete` and must remove its PV, LVMVolume, and LV after consumers release it. Platform uninstall and ordinary `forge destroy` preserve the receipt, selected PVs, VG, LVs, and bytes; no snapshot deletion lifecycle exists.

`forge destroy --purge-data-storage --reboot --yes` is the explicit fixture/decommission path. Purge refuses unless the receipt, ownership tag, set/order, resolved hardware, PV UUIDs, VG UUID/name/membership, system safety, mounts, raw consumers, and empty LV set all agree. A fsynced receipt records intent and completion around VG removal, every receipt PV removal, and receipt removal so interruption resumes only the same exact decommission transaction. It has no force path and does not securely erase media.

## Release and acceptance

The affected semantic target set is `control-plane`, `forge`, `control-plane-chart`, and `iterabase-platform-chart`; the platform target contains both same-version companions. HOR-538 artifacts remain immutable historical evidence.

Required evidence includes focused owners, generated CRDs, static/render and runtime proof of both expandable classes plus the inert CRD/read-only RBAC/deny-all/zero-instance boundary and CSI/user snapshot absence, manager-only grow admission, ModelBackend deterministic multi-claim/WFFC/no-adoption behavior, real mounted general/ModelBackend/AgentPool growth with stable PVC/PV/LVMVolume/LV/filesystem identities and bytes, shrink and insufficient-VG refusal, safe interruption/reboot/reapply convergence, same-pool concurrency/isolation and active-turn continuity, fresh-statfs 20/25 behavior, worker and serving-pod replacement, lifecycle reclamation, exact-head CI/E2E, an exact-head explicit four-target candidate rehearsal, exact-source candidate, protected promotion, and fresh bare-metal Ubuntu 24.04 LTS OPO1 acceptance. The permanent GPU fixture copies its separately verified pinned model from the harness cache disk into a managed general-class PVC before serving; the serving pod mounts only that PVC. Active F3 evidence uses only the provider-neutral `forge/permanent-fixture-cpu`, `forge/permanent-fixture-cpu-workspace`, and `forge/permanent-fixture-gpu` identities. Merge and publication do not complete HOR-557 or HOR-545's later OPO1 gate.
