# Permanent CPU/GPU E2E fixture operations

Authority: `DES-HOR-540-02`, `DES-HOR-545-02`, and `DES-HOR-545-03`. These hosts are dedicated,
reimageable CI fixtures; they contain no customer data. Work is serialized per
fixture by `iterabase-permanent-fixture-<capacity>` with cancellation disabled
and FIFO `queue: max` pending runs; independent CPU and GPU hosts may run
concurrently.

## Active evidence identities

The active compiled scenario IDs are exactly:

- `forge/permanent-fixture-cpu`
- `forge/permanent-fixture-cpu-workspace`
- `forge/permanent-fixture-gpu`

PR execution retains grouped F3 evidence as
`e2e-result-permanent-fixture-<capacity>` and
`e2e-diagnostics-permanent-fixture-<capacity>`. Candidate execution uses
`candidate-result-permanent-fixture-<capacity>` and
`candidate-diagnostics-permanent-fixture-<capacity>`. Per-scenario JSON files
derive from the provider-neutral IDs, for example
`forge-permanent-fixture-gpu.json`. Dated acceptance records keep their exact
legacy artifact names as immutable historical evidence; those names are not
active selector, catalogue, plan, result, or release authority.

## Security and ownership boundary

- Actions receives one fixture-scoped SSH key per host. It receives no provider
  account/API credential.
- Repository-controlled code has root-equivalent authority on only these
  dedicated hosts. This is not a malicious-same-repository-code isolation
  guarantee.
- The founder alone provisions, quarantines, power-cycles, rescues, reimages,
  replaces, or deletes fixtures through the provider.
- The CPU and GPU Forge data-storage disks contain disposable test state. The GPU
  model-cache disk contains only the reviewed public model below. No customer
  secrets or data may be placed on any fixture disk.

## Required host baseline

Provision one Ubuntu 24.04 CPU host and one Ubuntu 24.04 NVIDIA GPU host. For
each host:

1. Create a dedicated `forge`-style user with passwordless sudo and a unique
   Ed25519 public key. Do not reuse a personal, overlay, provider-account, or
   other fixture key.
2. Attach one non-root whole data disk. Record its stable
   `/dev/disk/by-id/...` identity and leave it blank. Forge receipt-binds its PV
   and membership in the fixed thick `iterabase-data` VG; chart-owned OpenEBS,
   not Forge, owns each claim's LV/XFS/mount lifecycle.
3. Confirm the selected disk does not back `/`, `/boot`, `/boot/efi`, `/var`,
   swap, `/var/lib/rancher/k3s`, or `/var/lib/kubelet` and has no partitions,
   holders, mounts, or signatures.
4. Install the baseline packages required by Forge and the lifecycle probe:
   `curl`, `git`, `psmisc` (`fuser`), `util-linux`, `fail2ban`, and the applicable
   ext4/XFS tools. Enable the `sshd` fail2ban jail (`maxretry=5`,
   `findtime=10m`, `bantime=1d`) and set SSH `MaxStartups 100:30:200` plus
   `LoginGraceTime 30`; public fixture addresses otherwise receive enough
   unauthenticated scanning to starve rapid reboot/reconnect probes. The GPU
   host must also satisfy Forge's NVIDIA/Ubuntu preflight. Forge reports the
   matching kernel build Makefile plus `dkms`, `gcc`, and `make`, then every GPU
   apply idempotently installs `linux-headers-$(uname -r)`, `build-essential`,
   and `dkms` and verifies that exact build surface before installing the GPU
   Operator. The running kernel's exact header package must still be available
   from the configured Ubuntu archives because the driver container resolves it
   independently; upgrade and reboot a stale HWE baseline before qualification.
   Do not rely on an out-of-band build-dependency install as qualification
   evidence.
5. Obtain the host public key through a trusted provider console or first-boot
   channel. Compare it independently before recording the exact one-line
   OpenSSH public key. Do not trust an unauthenticated first `ssh-keyscan` result.
6. Do not pre-create or remove
   `/etc/sysctl.d/90-iterabase-k3s-inotify.conf` and do not set a conflicting
   `fs.inotify.max_user_instances` in a higher-precedence drop-in. Forge owns that
   file on every apply, and the permanent CPU fixture asserts the exact `8192`
   live and persisted value (HOR-569).

The source/candidate fixture-only Helm values keep the real thick PostgreSQL and
MinIO claims enabled at 5 GiB each so the smallest 25 GiB disposable data VG
retains headroom for AgentPool and lifecycle proofs. They never override the
fixed classes, VG, provisioner, filesystem, access mode, or production chart
default sizes; Kind owner scenarios still exercise the production defaults.

The permanent GPU fixture does not require provider ingress to K3s port 6443.
The harness rewrites only the fetched kubeconfig transport endpoint and carries
all client-go and `kubectl` API traffic through a fixture-scoped pinned SSH
tunnel to host-local `127.0.0.1:6443`, while retaining the original API server
identity for TLS verification.

Do not repair GPU readiness by manually labeling the node. Forge keeps GPU
Operator v26.3.3 but overrides its embedded NFD 0.18.3 subchart through supported
values to run NFD v0.19.0 and set master `resyncPeriod=30s`. NFD v0.19.0 retains
the embedded chart's NodeFeature CRDs and command surface; its periodic full
reconcile bounds recovery when master misses a fresh NodeFeature event. The
subchart already supplies worker `POD_UID`, and topology updater remains disabled,
so the v0.19.0 topology-updater RBAC migration does not apply. Every live GPU run
verifies that the rendered master/worker image is
`registry.k8s.io/nfd/node-feature-discovery:v0.19.0` and the master argument is
`-resync-period=30s` before accepting the scenario.

The GPU host additionally receives a second non-root whole disk, physically and
logically distinct from the Forge data-storage disk:

```bash
# Example only: substitute the founder-verified model-cache by-id device.
DEVICE=/dev/disk/by-id/<gpu-model-cache>
sudo mkfs.ext4 -F -L iterabase-model-cache "$DEVICE"
sudo install -d -o root -g root -m 0755 /data/hf-cache
UUID=$(sudo blkid -p -s UUID -o value "$DEVICE")
printf 'UUID=%s /data/hf-cache ext4 nodev,nosuid 0 2\n' "$UUID" | sudo tee -a /etc/fstab
sudo mount /data/hf-cache
```

Populate only the repository-pinned public model:

```bash
python3 -m pip install --user 'huggingface_hub[cli]'
huggingface-cli download Qwen/Qwen3.5-0.8B \
  --revision 2fc06364715b967f1860aea9cf38778875588b17 \
  --cache-dir /data/hf-cache
sha256sum /data/hf-cache/models--Qwen--Qwen3.5-0.8B/snapshots/\
2fc06364715b967f1860aea9cf38778875588b17/\
model.safetensors-00001-of-00001.safetensors
# must equal 04b1c301231dd422b8860db31311ab2721511346a32cb1e079c4c4e5f1fe4696
```

Do not place this disk's by-id identity in `spec.dataStorage.devices`. Forge
data-storage purge never targets `/data/hf-cache`. This disk is harness-owned,
read-only seed input: the GPU scenario first verifies its device/UUID/revision/
hash, copies the pinned cache into a controller-owned `iterabase-lvm-xfs` PVC,
and then starts the ModelBackend. The serving pod mounts only the managed PVC
at `/data/hf-cache`; it never mounts this fixture disk or another hostPath.

## GitHub repository configuration

Set these repository **variables** from founder-verified values:

| CPU | GPU |
| --- | --- |
| `FORGE_E2E_CPU_ADDRESS` | `FORGE_E2E_GPU_ADDRESS` |
| `FORGE_E2E_CPU_SSH_USER` | `FORGE_E2E_GPU_SSH_USER` |
| `FORGE_E2E_CPU_SSH_HOST_KEY` | `FORGE_E2E_GPU_SSH_HOST_KEY` |
| `FORGE_E2E_CPU_DATA_STORAGE_DEVICE` | `FORGE_E2E_GPU_DATA_STORAGE_DEVICE` |
| — | `FORGE_E2E_GPU_MODEL_CACHE_DEVICE` |
| — | `FORGE_E2E_GPU_MODEL_CACHE_UUID` |

Set two repository **secrets**:

- `FORGE_E2E_CPU_SSH_KEY`
- `FORGE_E2E_GPU_SSH_KEY`

No address, host key, device, or key may come from `workflow_dispatch` input.
No legacy provider API token or other provider credential may exist in
repository Actions secrets after cutover.

Audit without exposing values:

```bash
gh variable list --repo nunocgoncalves/iterabase-mono
gh secret list --repo nunocgoncalves/iterabase-mono
```

## Pinned image cache

The shared platform image set pinned in `.github/inputs/remote-content.json` is
cached on each fixture host so the platform applies never pull those images from
public registries, whose per-IP anonymous quotas can fail a run mid-apply
(`401`/rate-limit responses). GPU-only pins (`nvcr.io`, `nvidia/*`, vLLM) are
not cached: they remain per-run registry pulls on the GPU capacity until that
fixture has a dedicated cache volume.

- Layout: `/var/lib/iterabase-e2e/image-cache/<capacity>/<generation>/` holds
  `generation.json` (emitted by
  `.github/scripts/fixture_image_cache.py manifest --capacity <capacity>`, plus
  the per-image config digest recorded at seed time) and `images/<archive>.tar`,
  one digest-pinned image per archive.
- Capacity sets: both capacities cache the same shared set (49 images at this
  head, ~4.5 GB). GPU-only registries/repositories (`nvcr.io`, `nvidia/*`,
  vLLM) are excluded because the GPU fixture's 96 GB root disk cannot hold the
  full ~46 GB cache plus its imported copy.
- Seeding: dispatch the `Fixture image cache` workflow with `capacity` and the
  repository `ref` whose pinned-list generation is required. The job shares the
  `iterabase-permanent-fixture-<capacity>` lock, pulls each image by digest with
  the reviewed `crane` binary directly on the host, stages
  `.staging-<generation>`, writes `generation.json`, swaps it into place, and
  only then prunes superseded generations. Seeding is required again only when
  the pinned list changes (new generation); a matching generation is left
  untouched.
- Failed seeds: staging happens before any prune, so a seed that fails partway
  leaves the previous generation intact and usable. Re-run the workflow to
  retry; the next run discards the failed staging directory.
- Per run: real-machine PR and candidate jobs derive the generation from
  `remote-content.json`, and the harness imports and verifies every reference —
  including the recorded config digest, not just the tag — before the first
  apply. A missing or mismatched generation or image fails the scenario with an
  actionable message instead of falling back to registries.
- Retention: superseded generations are pruned only after a new generation is
  finalized; keep the generation referenced by the checked-out source. The seed
  workflow reports the retained generation sizes.
- The cache is a derived artifact of `remote-content.json`; never hand-edit it
  and never place credentials or customer data in it.
- MinIO provenance: `ghcr.io/nunocgoncalves/iterabase-third-party/minio:RELEASE.2025-09-07T16-13-09Z`
  (`sha256:786c852164a4fab14fd194fdbe6b4ed6f34934fcf6e4556aa9368432081719e9`)
  republishes the exact archived upstream multi-arch content (linux/amd64,
  linux/arm64) because the upstream registries no longer serve it anonymously.
  Its embedded `mc` (`RELEASE.2025-08-13T08-35-41Z`) serves the
  artifact-provisioner Job, so the chart pins that single image for both roles.

## Normal lifecycle

Every selected scenario performs this lifecycle before apply and again after
diagnostics, regardless of success, failure, or interruption recovery:

```bash
forge destroy --config forge.yaml --purge-data-storage --reboot --yes
```

Expected evidence:

1. existing Flux/platform/GPU/K3s cleanup completes;
2. the exact Forge receipt/ownership-tag/device/PV/VG identity and empty-LV predicate are revalidated;
3. the receipt-matching `iterabase-data` VG and selected PV signatures are removed;
4. reboot is requested only after successful purge;
5. SSH disconnects, then reconnects under the same pinned host key;
6. `/proc/sys/kernel/random/boot_id` changes;
7. `cloud-init status` reaches `done` and the final pinned readiness session is
   closed before a short quiet handoff to Forge; SSH becoming reachable alone is
   not post-reboot readiness;
8. data-storage receipt/VG/PV signatures, K3s, run-scoped overlays,
   transferred artifacts, and stale test processes are absent;
9. on GPU, `/data/hf-cache` still resolves to its distinct by-id device/UUID and
   its pinned model file still matches revision and SHA-256 authority.

Ordinary `forge destroy` remains data-preserving. `--reboot`, CI mode,
environment, or prior fixture state never implies `--purge-data-storage`.

## Key rotation and host-key replacement

Perform client-key rotation only while fixture-backed dispatch is stopped and no
job holds the global concurrency group.

1. Quarantine the target fixture.
2. Add the new fixture-scoped client public key through the trusted provider
   channel.
3. Verify a direct session against the unchanged pinned host key, then replace
   only the matching GitHub private-key secret.
4. Remove the old authorized client key and run one full lifecycle cycle.
5. Record the rotation date and validating run in the operational ticket.

A changed SSH **host** key is not routine client-key rotation. Do not accept the
new endpoint key, use `ssh-keyscan` as authority, rotate/update the repository
pin, or dispatch another fixture job until the founder explicitly approves
recovery. Use this independently verifiable procedure:

1. Stop fixture-backed dispatch and quarantine the address.
2. Through the authenticated provider account and trusted console—not SSH to the
   suspect endpoint—confirm the intended fixture identity and run:

   ```bash
   sudo cat /etc/ssh/ssh_host_ed25519_key.pub
   sudo ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256
   ```

3. Independently fingerprint the currently configured repository-variable value
   from a trusted administrator workstation with `ssh-keygen -lf - -E sha256`.
   Compare algorithm, base64 public-key body, and SHA-256 fingerprint. Do not
   infer trust from the address or an unauthenticated network observation.
4. If the change was not an expected founder-authorized rebuild, treat the host
   as replaced or compromised and reimage it through the provider console.
   Re-establish the full Ubuntu/package/user/disk/model-cache baseline and repeat
   the console fingerprint capture.
5. Record the old and console-verified new fingerprints, provider resource
   identity, rebuild reason, disk assignments, and approval in the operational
   ticket. Only after explicit founder approval update the matching
   `FORGE_E2E_<CAPACITY>_SSH_HOST_KEY` repository variable.
6. Prove one direct session with `StrictHostKeyChecking=yes` against that
   approved exact public key. The fixture runner materializes the same approved
   key into a per-run non-Git trust file referenced by
   `spec.hosts[].sshTrustFile` (see [`../architecture/forge-ssh-host-trust.md`](../architecture/forge-ssh-host-trust.md));
   the key never enters Git, forge.yaml, command arguments, logs, or evidence.
   Then run one full required reset/apply/assert/reset lifecycle. Only that green
   lifecycle ends quarantine.

## Failure, quarantine, and manual provider recovery

If SSH remains healthy, leave the failed run red. The next capacity-serialized
preflight executes the same purge/reboot and may recover interrupted state.
Never rerun a failed assertion to launder it into a pass; qualification streaks
reset on any failed or incomplete cycle.

If SSH, purge, or reboot cannot recover the host:

1. Stop fixture-backed workflow dispatch and let the active job fail.
2. Mark the fixture quarantined in the active Linear incident/ticket.
3. Use the provider console manually to inspect power/network/disk state. Actions
   has no authority here.
4. Prefer reimage over ad-hoc repair when identity or residual-state confidence
   is lost.
5. Restore the full baseline, stable by-id assignments, fixture user/key, pinned
   host key, and (GPU) separately mounted/cache-verified public model.
6. Run the next selected required PR or candidate lifecycle for that capacity
   before resuming unrelated changes. Record source SHA, workflow/job, boot IDs,
   data-storage identity, and model revision/hash in the operational ticket.

Never attach customer disks, restore customer snapshots, or copy customer data
to a fixture.

## Rollback

Rollback first stops fixture-backed execution, lets or forces no new holder of
either capacity lock, and runs/verifies cleanup where pinned SSH remains
healthy. Quarantine both fixtures while reverting source/workflow behavior.

Do **not** restore a provider account token to Actions, re-enable dynamic
provisioning/reaping, weaken host-key checks, share a key across fixtures, point
Forge purge at the model cache, or make ordinary destroy destructive. A rollback
that needs provider-side action is founder-operated. Resume only with an
explicit approved corrective change and a fresh lifecycle qualification record.
