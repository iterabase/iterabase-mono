# forge

> Canonical source: [`iterabase-mono/forge`](https://github.com/nunocgoncalves/iterabase-mono/tree/master/forge). The former standalone repository is historical and read-only.

`forge` is the installer for the Horizonshift / Iterabase platform. It bootstraps a production-ready single-node [k3s](https://k3s.io) cluster on a VM or host over SSH, with dual-stack networking and prod-ready defaults, then applies an exact platform release and the customer's overlay.

> Per-customer, fully isolated. forge takes an existing VM/host (SSH), or, with `provider.aws`, provisions a dedicated environment in the hosted AWS account and reaches it with SSH over SSM (HOR-593). It does **not** provision bare metal, Proxmox, or network appliances.

## Status

Single-node k3s bootstrap, the platform chart and its substrates, GPU node readiness (HOR-240), the locally rendered customer overlay (DES-HOR-593-03), and the hosted AWS provider (HOR-593). The vLLM/SGLang backend is CRD-driven by the control-plane (HOR-306).

## Install

Pre-built binaries are published from the monorepo's protected release workflow on the [GitHub Releases](https://github.com/nunocgoncalves/iterabase-mono/releases) page (linux/darwin × amd64/arm64), with checksums, an SBOM, provenance, compatibility, and validation evidence. Raw tags do not publish; see [`../docs/release.md`](../docs/release.md).

> Homebrew tap: deferred — `goreleaser` deprecated its `brews` section; the tap will return once the replacement stabilizes.

Build from source:

```sh
make build      # -> bin/forge
```

## Quickstart

```sh
forge init               # generate forge.yaml (interactive)
forge apply --dry-run    # preflight the target host and print the plan (read-only)
forge apply              # provision / reconcile the cluster
forge kubeconfig         # fetch (or refresh) the kubeconfig
forge status             # cluster health + drift
forge upgrade --to v1.34.10+k3s1 # upgrade to the current repository-reviewed k3s release
forge destroy            # customer-safe: uninstall k3s; preserve iterabase-data
forge destroy --purge-data-storage --reboot --yes # explicit empty-VG/PV decommission lifecycle

# Overlays repository (DES-HOR-593-03/04)
forge overlay new acme --root ~/src/overlays            # scaffold customers/acme from customers/_template
forge fleet apply --root ~/src/overlays [--only acme]   # forge apply for every customer

# Hosted AWS environments (provider.aws, HOR-593)
forge init --provider aws --config customers/acme/forge.yaml   # provision, bootstrap over SSH over SSM, apply
forge apply --config customers/acme/forge.yaml                 # later releases and overlay changes
forge tunnel --config customers/acme/forge.yaml                # Kubernetes API on 127.0.0.1:6443 over SSM
forge destroy --config customers/acme/forge.yaml               # ordered teardown + zero-result tag sweep
```

`forge` SSHes to the host as a sudoer user (key auth) and installs k3s with flags derived from `forge.yaml`. `spec.hosts[].sshTrustFile` is required and names a non-Git known_hosts-format file holding the founder-verified host public key(s); every production SSH path verifies the host during key exchange and fails closed on missing, unknown, or mismatched trust before authentication, sessions, commands, or stdin. See [`docs/architecture/forge-ssh-host-trust.md`](../docs/architecture/forge-ssh-host-trust.md). Forge verifies the repository-reviewed K3s executable and airgap image archive before placing either in a privileged path, then runs the pinned K3s service installer with downloads disabled. Helm is likewise installed only from a reviewed archive after both archive and extracted-executable verification. Unsupported tool versions and changed bytes fail before execution or extraction, and retries cover transport only. These identities are recorded in `.github/inputs/remote-content.json`. The kubeconfig is fetched, rewritten to the host address, and stored at `~/.forge/<install>/kubeconfig.yaml`.

`apply` is **idempotent**: it reconciles from the live system — installs if absent, skips if in sync, refuses immutable changes (`cluster-cidr`/`service-cidr`/`dualStack` → `destroy` + `apply`), and routes version changes to `upgrade`.

A Helm apply that fails part-way is recovered by fixing its cause and running `apply` again; no manual Kubernetes repair is needed. Helm computes an upgrade's deletions from the previous revision's manifest even when that revision failed, so objects a failed revision created can outlive every later successful revision. After each successful Helm apply, Forge therefore deletes the objects its failed revisions rendered that the release no longer renders, limited to objects whose live Helm ownership annotations still name the release and that do not carry `helm.sh/resource-policy: keep` (DES-HOR-677-02).

### Overlay (DES-HOR-593-03, -04, -05)

`spec.overlay` is a local directory, normally `customers/<name>/` in the private
overlays repository. Forge renders it **on the operator machine** and ships only
the result, so one repository can hold every customer and no installation ever
receives another customer's content or a Git credential:

| `forge.yaml` field | Meaning |
| --- | --- |
| `overlay.path` | Overlay directory, relative to `forge.yaml`. Its `kustomization.yaml` (built in-process; remote bases and Helm inflation are refused) yields the object instances: Workflows, Models, ModelBackends, AgentPools. |
| `overlay.valueFiles` | Helm value files relative to `path`, in order, later wins (default `[values.yaml]`). They may reach shared bases inside the repository, for example `../../bases/hosted/values.yaml`. |
| `overlay.toolDirs` | Directories holding one subdirectory per customer tool (default `[tools]`). |
| `secrets.yaml` | Optional declarations of Secrets whose values come from operator environment variables; Secret manifests in the overlay are refused. |

On every apply Forge:

1. stages the value files on the host and materializes declared Secrets;
2. writes the customer tools as an immutable ConfigMap `overlay-tools-<sha256>`
   plus the `overlay-tools` pointer in namespace `iterabase-overlay`, which the
   tool runner reads (DES-HOR-593-05; platform chart 0.6.0 or later);
3. applies the platform chart with the value files;
4. applies the object instances labelled `iterabase.com/owner=overlay`, refusing
   to overwrite any live object without that label;
5. prunes overlay-owned objects and tool archives the overlay no longer renders.
   Objects created inside the installation never carry the overlay label and are
   never touched. Removing the overlay from `forge.yaml` removes its objects.

`forge apply` is the only delivery path; there is no in-cluster GitOps
controller (DES-HOR-593-04). Use `forge fleet apply` to roll a shared change out
to every customer.

### Hosted AWS environments (`provider.aws`, HOR-593)

```yaml
spec:
  mode: single-node
  provider:
    aws:
      instanceType: m7i-flex.xlarge   # must be in the baseline's allowed-instance-types
      dataVolumeGiB: 100
  # no hosts or dataStorage: Forge derives them from the environment
```

Forge reads the account-level baseline only through the SSM contract
`/iterabase/hosted/baseline/v1/*` (DES-HOR-603-03), assumes the baseline Forge
role, and runs its embedded Terraform module with a pinned Terraform binary.
State lives in the baseline state bucket under `envs/<name>/`. The module
creates the instance (IMDSv2, hop limit 1), a separate gp3 data volume used as
the blank `iterabase-data` disk, a security group with no inbound rules, and a
node role under `/iterabase/env/` capped by the baseline boundary with the load
balancer controller policy attached; every resource carries
`iterabase-env=<name>`. The instance's SSH host key is generated with the
environment and installed by user-data, so trust exists before the first
connection (DES-HOR-593-02). Forge's SSH runs over an SSM
`AWS-StartSSHSession` stream through a pinned, checksum-verified
session-manager-plugin it downloads itself (DES-HOR-593-01); port 22 is never
opened. The hosted installation facts (load balancer controller identity, VPC,
hostnames under the hosted domain, origin-pull CA, DNS token Secret) are derived
from the contract and applied after the overlay's values.

`forge destroy` deletes every LoadBalancer Service and waits for the controller
to remove the NLB, then runs `terraform destroy`, purges the backup and state
prefixes, and succeeds only when the tag sweep for `iterabase-env=<name>` finds
zero resources. See [`docs/runbooks/hosted-environments.md`](../docs/runbooks/hosted-environments.md).

When GPU support is enabled, the default GPU Operator chart archive is bound to its repository-reviewed SHA-256; a custom operator repository/version requires an explicit `gpu.operator.sha256`. `gpu.driver.version` and `gpu.driver.sha256` are both required, and Forge composes that exact driver tag-plus-digest together with repository-pinned operator, validator, toolkit, device-plugin, DCGM exporter, MIG manager, driver manager, and NFD identities. Forge verifies the downloaded chart archive before Helm inspects CRDs, templates, or applies it. Preflight then reports the NVIDIA PCI device and the complete driver-build surface: `/lib/modules/$(uname -r)/build/Makefile`, `dkms`, `gcc`, and `make`. Before GPU Operator reconciliation, `apply` idempotently installs the matching `linux-headers-$(uname -r)`, `build-essential`, and `dkms` packages, verifies the exact surface, and refuses a stale running kernel whose matching headers are no longer available from the configured Ubuntu archives because the driver container must independently resolve that package. Apply then accepts readiness only from one coherent operator/node observation: the ClusterPolicy must expose `Ready=True` and `Error=False`, its configured driver must match any requested pin, and the single node must be Ready, schedulable, outside an active upgrade, and report that exact loaded driver. The GPU Operator's legacy `status.state=notReady` remains visible evidence but is not the sole authority because v26.3.3 writes it separately from conditions and can lose that status update to a resource-version conflict. Forge accepts that one documented contradiction only when every stronger current signal agrees; other missing or contradictory evidence keeps apply blocked, and `upgrade-failed` terminates it immediately. GPU Operator v26.3.3 embeds the NFD 0.18.3 subchart, but Forge pins its compatible runtime image to NFD v0.19.0 and sets the supported master `resyncPeriod` to 30 seconds. NFD v0.19.0 makes that interval drive a periodic full reconcile, so a missed fresh-NodeFeature event cannot defer the required NVIDIA node label until the operator readiness timeout. The embedded chart retains identical NodeFeature CRDs, supplies the default worker pod identity needed by v0.19.0, and leaves the topology updater (whose v0.19.0 image-only upgrade needs additional RBAC) disabled.

Before each Helm apply, forge reads the CRDs bundled in the exact pinned chart artifact, server-side applies them, and waits for them to become `Established`. This permits an existing release to enable an operator-backed dependency later (for example, enabling observability adds the Prometheus Operator CRDs) despite Helm's limitation that `crds/` are installed only during a release's initial install. Charts without bundled CRDs are unchanged. CRDs are intentionally retained on rollback/uninstall to protect custom resources and their data.

Every single-node config requires canonical `spec.dataStorage.devices`: a
non-empty lexically ordered immutable list of stable `/dev/disk/by-id/...`
blank whole-disk identities. Interactive init selects one or more devices;
repeated `--data-storage-device`, comma-separated
`FORGE_DATA_STORAGE_DEVICES`, and direct YAML express the same set. Duplicate,
conflicting, reordered persisted, missing, replaced, or changed sets fail
closed. Selecting that complete set is the sole first-write authorization;
there is no force, wipe, adopt, extend, replacement, or root fallback.

Before any disk mutation and again immediately before each `pvcreate`, Forge
performs bounded complete-set stable-identity, hardware/size, whole-device,
topology, system/root/boot/EFI/swap/K3s-backing, mount, holder,
LVM/RAID/crypt, partition/signature, and `/proc` raw-consumer checks. Missing
probes, read errors, descriptor races, ambiguity, or drift fail before K3s,
charts, workloads, or claims. Forge never scans every byte.

After a read-only preflight, a fresh install disables every active host
swap device with `swapoff --all` and atomically comments every uncommented
`/etc/fstab` entry whose filesystem type is `swap` (including Ubuntu's default
`/swap.img`). The rewrite preserves every unrelated/commented line plus the
file's owner and mode and leaves inactive swap files in place. The content
rewrite is skipped when already converged, but every run re-verifies metadata
and syncs the current fstab plus its parent directory so an interruption after
atomic replacement is retry-safe. Forge immediately re-reads `/proc/swaps` and
`/etc/fstab` and fails before package, disk, or k3s artifact/service mutation
unless both are swap-free. Dry-run, installed-cluster reapply, and upgrade remain
read-only with respect to this fresh-install hardening operation.

Every non-dry-run `apply` and `upgrade` also reconciles the managed-host
`fs.inotify.max_user_instances` ceiling before downstream install, upgrade, or
overlay mutation. Kubernetes CRI log following opens one fsnotify watcher per
followed container log, so exhausting the per-user inotify **instance** ceiling
fails with `EMFILE` (`failed to create fsnotify watcher: too many open files`)
even when process and global file-descriptor limits are healthy; the OPO1
incident exhausted Ubuntu's default of `128` with K3s, containerd shims, and
root containers. Forge owns the stable drop-in
`/etc/sysctl.d/90-iterabase-k3s-inotify.conf` (`root:root`, mode `0644`,
`fs.inotify.max_user_instances = 8192`): a conforming persisted and effective
value is a no-op, drift converges through a staged, fsynced atomic replacement
that never removes the existing drop-in first and never lowers an already
sufficient live value, a lower live value is raised with `sysctl -w`, and Forge
fails closed unless the read-back proves both the persistent and effective
setting. `forge apply --dry-run` and `forge status` stay read-only and report
the effective value, drift, and drop-in state.

Forge then installs/verifies `lvm2` and XFS tooling, converges the superseded
pre-release `dm-snapshot` module/configuration to absence, and uses a root-owned
fsynced staged receipt to
bind exact planned-UUID PVs, a unique ownership tag, and the fixed thick VG name
`iterabase-data` before mutation. Forge creates the VG atomically with that tag
and LVM-generated UUID, then fsyncs the observed UUID before any handoff.
Reconcile, status, and purge accept only the receipt-matching tag/device/PV/VG
set. Purge records durable intent/completion around VG, each PV, and receipt
removal so interruption resumes exactly. Forge creates no platform LV,
filesystem, mount, or fstab entry; those belong to chart-owned OpenEBS LVM
LocalPV dynamic PVC lifecycle. Storage snapshots remain fully disabled. The sole
inert provider dependency is the `LVMSnapshot` CRD plus exact driver
`list`/`watch`, protected by deny-all creation and a zero-instance readiness
gate; every CSI/user snapshot surface remains absent. OPP-005 solely owns any
future recovery mechanism.

K3s is installed with `local-storage` disabled. Forge installs the same-version
`cert-manager-substrate` and `lvm-storage-substrate` companions before the
platform, then waits for the pinned OpenEBS `1.10.0` volume CRDs, inert
`LVMSnapshot` CRD, deny-all creation policy, zero instances, exact read-only
driver authority, controller, node plugin, CSI registration, exactly two managed
non-default XFS/RWO classes, and receipt-matching VG discovery. No
local-path/default/root storage fallback is reconciled.

Ordinary `forge destroy` uninstalls platform/K3s but preserves the receipt,
PVs, VG, LVs, and bytes. `--purge-data-storage` is separate explicit
decommissioning: after consumers and LVs are gone it revalidates the exact
receipt/device/PV/VG identities before removing only that VG and its PV
signatures. It refuses foreign, mounted, in-use, non-empty, missing, or drifted
state and is not secure erase. `--reboot` runs only after successful cleanup.
The non-interactive explicit form is
`forge destroy --purge-data-storage --reboot --yes`. See
[`../docs/architecture/v2-openebs-lvm-storage.md`](../docs/architecture/v2-openebs-lvm-storage.md).

See `forge.example.yaml` for the full substrate config schema.

## Development

```sh
make test           # unit + fake-SSH integration tests
make test-e2e       # forge/cpu on a fresh per-run CPU host (CI supplies the environment)
make test-e2e-workspace # forge/cpu-workspace: PV/VG/OpenEBS thick-XFS RWO gate on a fresh CPU host
make test-e2e-gpu   # forge/gpu on a fresh per-run GPU host
make test-e2e-unit  # compile + unit-test the separate E2E harness module
make lint           # golangci-lint
make fmt-check      # gofmt check
make install-hooks  # wire .githooks/ via core.hooksPath
```

Architecture invariants and v1 boundaries are documented in `AGENTS.md`.

## Layout

- `cmd/forge/` — entrypoint
- `internal/cli/` — Cobra command tree
- `internal/config/` — `forge.yaml` schema + loader
- `internal/provisioner/` — provisioner interface (the testability seam)
- `internal/sshprovisioner/` — SSH implementation
- `internal/k3s/` — k3s install-arg builder
- `internal/kubeconfig/` — kubeconfig fetch + server-URL rewrite
- `internal/lifecycle/` — phase orchestration + reconcile
- `internal/artifacts/` — local state dir (`~/.forge/<install>/`)
- `internal/version/` — build version
- `test/e2e/` — real-machine CPU/GPU runner on fresh per-run EC2 hosts (separate module; see `test/e2e/DESIGN.md`)

## License

Proprietary — Horizonshift. All rights reserved.
