# Forge hosted environments and the customer overlay

- **Status:** Approved design; implemented by HOR-593.
- **Approval date:** 2026-10-14
- **Ticket:** [HOR-593](https://linear.app/horizonshift/issue/HOR-593)
- **Decisions:** `DES-HOR-593-01` to `DES-HOR-593-07`. Canonical record: Obsidian `Areas/ho/Architecture/HOR-593 — Forge AWS provider design decisions.md`, mirrored in the ticket.
- **Inherits:** `DES-HOR-603-02`/`-03`/`-04`/`-05`/`-07`/`-08` (hosted baseline), `DES-HOR-601-01`/`-02`/`-03`/`-05` (hosted ingress), `DES-HOR-661-02`/`-05`/`-09` (central gateway identity and entitlements), `DES-HOR-521-01` (SSH host trust).

This record is the repository authority for how Forge provisions a hosted customer environment, how it reaches it, and how a customer overlay reaches an installation. Changing any decision below requires a new approved decision.

## 1. Approved decisions

| ID | Decision | Consequences |
| --- | --- | --- |
| `DES-HOR-593-01` | Forge's own SSH to hosted instances runs over an SSM `AWS-StartSSHSession` stream. Forge calls `ssm:StartSession` with the AWS SDK and hands the session to AWS's session-manager-plugin, which it downloads at a pinned, checksum-verified version (registered in `.github/inputs/remote-content.json`). | Amends the Forge "no local shell-out" invariant for this transport only. Plugin bumps are reviewed pin changes. Operators install nothing. |
| `DES-HOR-593-02` | For Forge-launched hosts, the ed25519 SSH host key is generated with the environment on the operator machine, installed by cloud-init user-data (replacing every stock key), and enrolled in the non-Git trust file before the first connection. | A new enrolment source under `DES-HOR-521-01` for Forge-launched hosts only. The private host key lives in user-data and the environment's Terraform state, both readable only by the Forge role and administrators. Re-provisioning a replaced instance re-keys it. |
| `DES-HOR-593-03` | Customer overlays are authored in one private repository (`iterabase/overlays`: `bases/`, `components/`, `customers/<name>/`, `customers/_template/`), templated with Kustomize bases and components plus layered Helm values. Forge renders one customer's overlay on the operator machine and ships only the result. Every overlay object carries `iterabase.com/owner=overlay`; Forge never overwrites or prunes an object without it. | No Git credential or other customer's content ever reaches an installation. Installation-owned objects (Admins; later the Chief of Staff) are never touched; name clashes are refused; promotion into the overlay is an export PR (future). |
| `DES-HOR-593-04` | `forge apply` is the only overlay delivery path. Flux is removed from Forge and from installations, including OPO1. `forge fleet apply` rolls a shared change to every customer. | No automatic apply on merge. Supersedes HOR-292's Flux GitOps and `DES-HOR-632-01`. A future pull model would replace push as the single path. |
| `DES-HOR-593-05` | Customer tools reach the tool runner as an immutable ConfigMap `overlay-tools-<sha256>` (`binaryData` `tools.tar.gz`) and the pointer ConfigMap `overlay-tools` (`digest`, `archive`, `revision`) in namespace `iterabase-overlay`. Forge writes them before Helm and prunes superseded archives after; the materializer reads them with a namespaced get-only Role and verifies the exact digest. | Platform chart 0.6.0 or later; Forge refuses 0.3.x–0.5.x charts. Archives are capped below the 1 MiB object limit. Installation-owned tools later fit the same shape as a third layer. |
| `DES-HOR-593-06` | Each hosted environment has its own SES SMTP credential: an IAM user `iterabase-env-<name>-mail` under `/iterabase/env/`, capped by the baseline `iterabase-hosted-mail-user-boundary` (only `ses:SendRawEmail` from the hosted identity), created by Forge's module; its SMTP password becomes the `iterabase-smtp` Secret and the chart's `auth.email` uses provider `ses`. | Needs the HOR-603 baseline change in this PR (Forge role may create mail users only under the path and boundary). A static credential in the cluster, revocable per customer and deleted at teardown; no pod identity required. External delivery still waits for SES production access. |
| `DES-HOR-593-07` | Hosted nodes pull `ghcr.io` images through the baseline ECR pull-through cache: containerd's `ghcr.io` `hosts.toml` mirrors to `<registry>/v2/<prefix>` (`override_path`) with an ECR token from the node role, refreshed every 6 hours by a Forge-installed timer and after every k3s start; `ghcr.io` stays the fallback. | No GHCR credential reaches customer nodes, so private packages need no hosted change. Chart image references are unchanged. The k3s-regenerated registry directory is rewritten by Forge's hook. |

## 2. Overlay contract

`forge.yaml` (inside `customers/<name>/`):

```yaml
spec:
  chart:
    version: 0.6.0                     # the platform release pin
  overlay:
    path: .                            # this directory
    valueFiles: [../../bases/hosted/values.yaml, values.yaml]
    toolDirs: [tools]
```

| Path in `customers/<name>/` | Content | Owner |
| --- | --- | --- |
| `forge.yaml` | Substrate, provider, release pin, overlay pointer | HOR-593 |
| `kustomization.yaml` | Composes `../../components/*` and local object files | HOR-593 |
| `values.yaml` | Customer Helm values (layered after shared bases) | HOR-593 |
| `workflows/` | `Workflow` instances | HOR-593 / workflow tickets |
| `tools/<tool>/` | Customer tools (`manifest.json` + bundle) | HOR-651 |
| `secrets.yaml` | Secret declarations; values from operator env vars | HOR-364 |
| `skills/` | Reserved: overlay Skills pinned by digest | HOR-423 |
| `credential-slots/` | Reserved: Integrations credential slots | HOR-582 / HOR-599 |
| `agents/` | Reserved: agent identities | HOR-248 |
| `mailboxes/` | Reserved: mailbox bindings | HOR-358 |
| `baselines/` | Reserved: value baselines (hours saved per workflow) | HOR-658 |

The hosted plan record is not in the overlay: it lives in the central gateway registry and changes through `forge central plan` / `extend-trial` (`DES-HOR-661-05`).

Rendering rules: value files and tool directories must resolve inside the overlays repository; kustomizations reachable from `overlay.path` must be local (no remote bases, no Helm inflation); Secret manifests are refused; hidden files in tools are not shipped.

## 3. Hosted environment

| Concern | Contract |
| --- | --- |
| Inputs | SSM contract `/iterabase/hosted/baseline/v1/*` only; forge.yaml supplies sizing. |
| Identity | Operator credentials read the contract and assume the baseline Forge role; every mutation runs as that role. |
| State | `s3://<state-bucket>/envs/<name>/terraform.tfstate`, S3-native locking. |
| Resources | Instance (Canonical Ubuntu 24.04, IMDSv2, hop limit 1, encrypted gp3 root), separate gp3 data volume, no-inbound security group, node role under `/iterabase/env/` with the node boundary, `AmazonSSMManagedInstanceCore`, `iterabase-hosted-lb-controller` and an inline policy for session logs, ECR pulls and the environment's own backup prefix. Everything tagged `iterabase-env=<name>`. |
| Host derivation | Host = instance ID over SSM; data disk = `/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_<vol-id>`; kubelet `provider-id=aws:///<az>/<instance-id>`. |
| Installation facts | Load balancer controller cluster name, tags, region and VPC; hosts `<name>.<hosted-domain>` and `<name>-gateway.<hosted-domain>`; external-dns domain filter; origin-pull CA; auth public origin; SES SMTP sender settings; `cloudflare-api-token` and `iterabase-smtp` Secrets. Applied after the overlay's values. |
| Images | `ghcr.io` through the ECR pull-through cache (`DES-HOR-603-05`, `DES-HOR-593-07`); other registries directly. |
| Access | SSH over SSM for Forge; `forge tunnel` forwards the Kubernetes API; support uses logged Session Manager shells (`DES-HOR-603-07`). |
| Teardown | LoadBalancer Services → wait for controller cleanup → `terraform destroy` → backup prefix purge → central certificate revocation → state prefix purge → zero-result tag sweep. |

## 4. Traceability

| Surface | Contract |
| --- | --- |
| `forge/internal/overlay` | Local render, labels, tools archive and ConfigMap manifests |
| `forge/internal/lifecycle/overlay.go` | Delivery order, ownership apply and prune |
| `forge/internal/hosted` | Contract, role assumption, embedded module, teardown, sweep |
| `forge/internal/ssmsession` | Pinned plugin and SSH-over-SSM stream |
| `control-plane/tool-runner/src/materializer.ts` | ConfigMap tool source |
| `charts/charts/control-plane/templates/tool-runner.yaml` | `overlayTools` Role and environment |
| [`docs/runbooks/hosted-environments.md`](../runbooks/hosted-environments.md) | Operator procedures |
