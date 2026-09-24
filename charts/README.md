# Iterabase charts

> Canonical source: [`iterabase-mono/charts`](https://github.com/nunocgoncalves/iterabase-mono/tree/master/charts). The former standalone source repository is historical and read-only; the existing `ghcr.io/nunocgoncalves/iterabase-charts` package namespace remains the stable artifact identity.

Helm charts for the [iterabase](https://iterabase.com) platform. The `cert-manager-substrate` release establishes certificate authority before the platform. The same-version `lvm-storage-substrate` release establishes content-pinned OpenEBS LVM LocalPV `1.10.0`, its volume authority, the inert `LVMSnapshot` deletion-safety schema with read-only driver access and deny-all creation, and the two managed non-default thick XFS/RWO classes. CSI and user snapshot surfaces remain disabled. [Forge](https://github.com/nunocgoncalves/iterabase-mono/tree/master/forge) prepares only the exact receipt-bound `iterabase-data` VG and disables K3s local storage before either chart runs.

## Charts

| Chart | Description | Released individually |
|---|---|---|
| `cert-manager-substrate` | Ordered certificate operator, CRDs, webhook, and CSI substrate | ✅, alongside platform |
| `lvm-storage-substrate` | Pinned OpenEBS LVM LocalPV volume authority, inert deletion-safety schema, and exact managed storage classes | ✅, alongside platform |
| `iterabase-platform` | Application umbrella — composes all platform components | ✅ |
| `inference-gateway` | Model-access service | ✅ |
| `control-plane` | Durable workflow/control APIs, operator, and immutable artifact service | ✅ |
| `postgresql` | Self-contained Postgres on the official image | bundled only |
| `redis` | Self-contained Redis (hot-path cache) | bundled only |
| `minio` | Self-contained MinIO object storage | bundled only |
| `cert-issuers` | cert-manager ClusterIssuers (Let's Encrypt DNS-01/Cloudflare + self-signed) | bundled only |
| `metallb-config` | MetalLB IPAddressPool + L2Advertisement (L2 edge for bare-metal/kind/OPO1) | bundled only |
| `observability` | Prometheus + Grafana + Loki + Alertmanager (kube-prometheus-stack + loki) + default alert rules + GPU (DCGM) scraping | bundled only |

control-plane ships standalone and is enabled in the umbrella by default (it provides the shared pgvector Postgres + the schemas the gateway reads).

## Install

Install the same-version certificate substrate first and wait for its webhook;
install the same-version LVM storage substrate and wait for volume-only CSI plus
`iterabase-data` discovery; then install the platform. The gateway is the only public endpoint, served over
HTTPS by the platform edge (ingress-nginx + cert-manager-issued leaves). The
edge is always a **LoadBalancer** Service —
no hostNetwork. The LB implementation is pluggable:

- **kind/dev** — MetalLB L2 with a pool in the kind docker-bridge subnet. Clone
  `iterabase-mono`, change into its `charts/` directory, and use the
  `values-kind.yaml` preset:
  ```sh
  helm install iterabase-cert-manager charts/cert-manager-substrate \
    -n iterabase-system --create-namespace --wait
  helm install iterabase-lvm-storage charts/lvm-storage-substrate \
    -n iterabase-system --wait
  helm install iterabase charts/iterabase-platform -n iterabase-system \
    -f values-kind.yaml --set control-plane.toolRunner.enabled=false --wait
  ```
  then verify the self-signed edge against its generated certificate and DNS identity:
  ```sh
  LB_IP=$(kubectl get svc -n iterabase-system -l app.kubernetes.io/name=ingress-nginx \
    -o jsonpath='{.items[0].status.loadBalancer.ingress[0].ip}')
  kubectl get secret iterabase-gateway-tls -n iterabase-system \
    -o jsonpath='{.data.tls\.crt}' | base64 -d >/tmp/iterabase-gateway.crt
  curl --cacert /tmp/iterabase-gateway.crt \
    --resolve gateway.iterabase.local:443:"$LB_IP" https://gateway.iterabase.local/health
  ```
- **bare-metal/OPO1** — MetalLB L2 with a real pool (e.g. a VLAN range); see the
  prod overlay below.
- **cloud** — leave MetalLB disabled and set provider annotations on
  `ingress-nginx.controller.service` so the cloud LB provisions the Service.

Get the generated gateway admin API key:

```sh
kubectl get secret iterabase-gateway-admin -n iterabase-system \
  -o jsonpath='{.data.adminApiKey}' | base64 -d
```

Production (OPO1, IPv6-only origin + Cloudflare-proxied dual-stack) — override in
your values/overlay:

```sh
helm install iterabase-cert-manager \
  oci://ghcr.io/nunocgoncalves/iterabase-charts/cert-manager-substrate \
  --version 0.4.5 -n iterabase-system --create-namespace --wait
helm install iterabase-lvm-storage \
  oci://ghcr.io/nunocgoncalves/iterabase-charts/lvm-storage-substrate \
  --version 0.4.5 -n iterabase-system --wait
helm install iterabase oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform \
  --version 0.4.5 -n iterabase-system \
  --set inference-gateway.ingress.host=gateway.opo1.example.com \
  --set inference-gateway.ingress.tls.clusterIssuer=letsencrypt-prod \
  --set ingress-nginx.controller.service.ipFamilyPolicy=SingleStack \
  --set ingress-nginx.controller.service.ipFamilies[0]=IPv6 \
  --set metallb.enabled=true \
  --set metallb-config.enabled=true \
  --set metallb-config.addresses[0]=2001:db8:30::/64 \
  --set cert-issuers.letsencrypt.enabled=true \
  --set cert-issuers.letsencrypt.email=you@example.com \
  --set external-dns.enabled=true \
  --set external-dns.domainFilters[0]=opo1.example.com.
```

(The Cloudflare API-token Secret shared by cert-issuers + external-dns must be
provisioned out-of-band — see the umbrella `values.yaml` comments. For IPv4-first
clients, set `ipFamilies[0]=IPv4` and an IPv4 `metallb-config.addresses` pool.)

### Private observability ingress

A deployment can expose Grafana, Prometheus, Alertmanager, and Loki through a
dedicated ingress plane without publishing those routes through the public
ingress plane. Reachability, DNS, and authorization are customer/deployment
policy owned by the overlay; the reusable chart does not prescribe a universal
exposure model. This private-network example enables the aliased
`internal-ingress-nginx` dependency, adds a named MetalLB pool, and binds the
private controller's IPv4 LoadBalancer Service to that pool explicitly:

```yaml
metallb-config:
  additionalPools:
    - name: internal
      addresses: ["10.0.20.200-10.0.20.215"]
      autoAssign: false
      interfaces: [eth0]
internal-ingress-nginx:
  enabled: true
  controller:
    service:
      annotations:
        metallb.io/address-pool: <release>-internal
        metallb.io/loadBalancerIPs: 10.0.20.200
```

Set each upstream observability ingress to `ingressClassName: nginx-internal`.
Use cert-manager's DNS-01 issuer for publicly trusted leaves and annotate each
Ingress with `external-dns.alpha.kubernetes.io/cloudflare-proxied: "false"` so
the resulting `A` record points directly at the private address. Prometheus,
Alertmanager, and Grafana serve internal-CA HTTPS; their Ingresses must use the
`backend-protocol`, `proxy-ssl-secret`, `proxy-ssl-verify`, and
`proxy-ssl-name` annotations shown in [`values-internal-observability.yaml`](values-internal-observability.yaml).
Loki's Ingress targets its HTTP gateway, which independently verifies the
internal-CA HTTPS hop to Loki.

For this example policy, the trusted private network/VPN is the authorization
boundary for the direct Prometheus, Alertmanager, and Loki APIs; Grafana keeps
its own login. A customer deployment must choose and approve its own network
reachability and authentication policy in the overlay. Do not use this recipe
on an untrusted LAN, and do not place these routes on the public `nginx` class.
The complete documentation-address fixture is rendered and validated by
`make check-internal-observability`.

The default Loki gateway has one replica and retains required same-component
pod anti-affinity. It therefore uses `Recreate`, accepting a brief gateway/log-
ingest interruption so a one-node upgrade cannot deadlock waiting for a second
schedulable node. The Loki backend and persisted data remain intact. An HA
or object-storage overlay may explicitly restore `RollingUpdate` only when its
replica and topology contract can satisfy the anti-affinity rule. When upgrading
an existing legacy `RollingUpdate` Deployment, a lookup-gated pre-upgrade hook
uses the digest-pinned Kubernetes `v1.31.0` kubectl image to replace the strategy
before Helm server-side applies the changed pod template. The hook is gated on
the desired strategy remaining `Recreate`; an explicit `RollingUpdate` overlay
is never migrated. The patch uses Helm's field-manager identity, so it does not
create a competing owner that blocks the explicit rollback below.

Ingress admission remains fail-closed. On first enablement, ingress-nginx's
pre-hook creates a stable serving Secret and its post-hook patches the validating
webhook CA. If Helm fails while waiting for workloads before that post-hook, run
the exact same upgrade again: the platform detects the existing internal webhook
and executes a pre-upgrade recovery hook after Secret creation, repairing the CA
before normal resources are reapplied. Do not patch the CA manually or weaken
`failurePolicy: Fail`.

A rollback to a revision before `0.3.19` restores that revision's legacy
`RollingUpdate` gateway manifest. On a one-node deployment, make the downtime
explicit by scaling the gateway to zero before rolling back the platform, then
roll back the same-version substrate after the platform:

```sh
kubectl scale deployment/<release>-loki-gateway -n <namespace> --replicas=0
kubectl wait -n <namespace> --for=delete pod \
  -l app.kubernetes.io/name=loki,app.kubernetes.io/component=gateway \
  --timeout=5m
helm rollback <release> <platform-revision> -n <namespace> --wait
helm rollback <release>-cert-manager <substrate-revision> -n <namespace> --wait
```

`make test-e2e-observability-ingress-recovery` exercises the checksum-pinned
`0.3.12` baseline, bypass of migration for an explicit `RollingUpdate` override,
a changed one-replica gateway, an injected failure before admission post-hooks,
fail-closed reapply, the explicit legacy rollback above, and current forward
recovery.

### Production OpenEBS LVM LocalPV storage

Platform V2 has no chart-selectable storage backend. Forge requires one or more
explicit stable blank whole data disks, repeats complete-set fail-closed checks,
and crash-resumably creates only receipt-bound PVs plus one fixed thick VG named
`iterabase-data`. Forge creates no platform LV, filesystem, mount, or fstab
entry. K3s's `local-storage` component is disabled; no local-path provisioner,
default class, or root-backed fallback is supported.

The same-platform-version `lvm-storage-substrate` companion wraps the reviewed
OpenEBS LVM LocalPV `1.10.0` archive (SHA-256
`3ad766c56d4a0ab0f3f2baaeb726a4554d1f51bb485f1cef00846cf1d82a179d`),
derives a fail-closed volume-only dependency by removing upstream CSI/user
snapshot surfaces and snapshot write authority while retaining only the inert
`lvmsnapshots.local.openebs.io` CRD plus exact driver `list`/`watch` required by
ordinary volume deletion, pins every required runtime image by digest, disables
analytics, configures K3s's actual `/var/lib/kubelet` CSI registration/mount
root, and creates exactly:

- `iterabase-lvm-xfs`: `shared: no`, used explicitly by every chart-generated
  platform data PVC;
- `iterabase-agentpool-lvm-xfs`: `shared: yes`, authorized only for one
  Filesystem/RWO claim per AgentPool so multiple same-pool workers can mount it
  on the one supported node.

Both classes use `local.csi.openebs.io`, `storage: lvm`,
`vgpattern: ^iterabase-data$`, XFS, explicit thick `thinProvision: no`,
`ReadWriteOnce`, `WaitForFirstConsumer`, `Delete`, grow-only expansion enabled,
and false default annotations. PostgreSQL, MinIO, persistent observability components,
and every other chart-generated data claim use only the general class.

The claim-authority admission policy permits only the exact control-plane
manager to raise an AgentPool request, and only upward; shrink and class,
ownership, access-mode, or volume-mode changes remain denied. The AgentPool
controller accepts initial unbound `WaitForFirstConsumer` state, then requires
the exact CSI driver, XFS, VG attribute, OpenEBS LVMVolume Ready identity and
capacity, no-thin/shared settings, LVMNode VG identity, local node topology, and
a fresh post-resize mounted-filesystem observation.
Trusted root-supervisor access, stable isolated child UID=GID and permissions,
workload-key/mTLS validation, fencing, and no-replay behavior remain unchanged.
Capacity warning/gating is per AgentPool PVC (25% warning, <=20% all-fresh-credit
gate, >=25% reopen); OpenEBS `lvm_vg_*` metrics and chart alerts separately
report aggregate VG pressure and pending-claim exhaustion.

Direct Helm installation does not replace Forge's receipt/VG safety gate. The
companion must observe the exact `iterabase-data` VG before the platform creates
claims. Multi-node/HA, RWX, default/BYO classes, Longhorn, local-path, thin
provisioning, shrink, identity-replacing resize, disk/VG extension, adoption,
and migration remain unsupported. A
fail-closed admission policy denies every `LVMSnapshot` creation and readiness
requires zero instances. Apart from that inert schema and driver `list`/`watch`,
every CSI/user snapshot CRD/class/controller/sidecar/RBAC/image/lifecycle remains
unsupported and absent. OPP-005 solely owns any future recovery mechanism.

### Private control-plane ingress

The control-plane Ingress owns one same-origin host for the Dashboard, static
assets, authentication, and every `/v1/*` API. Selecting a private class makes
that complete host private; it does not leave a separate public API route.
Deployments choose the exposure boundary. For a VPN-only host on the private
plane above:

```yaml
control-plane:
  ingress:
    enabled: true
    className: nginx-internal
    host: app.private.example.com
    tls:
      enabled: true
      clusterIssuer: letsencrypt-prod
    externalDns:
      enabled: true
      target: 10.0.20.200
      cloudflareProxied: "false"
```

The chart derives a dedicated edge Secret named
`<release>-control-plane-api-ingress-tls`; it never reuses the internal API leaf
`<release>-control-plane-api-tls`. When `global.internalTLS.enabled=true`, the
Ingress uses that non-CA API leaf Secret as its proxy TLS material: the
Secret's `ca.crt` verifies the backend at the exact
`<release>-control-plane-api.<namespace>.svc` identity. The internal root CA
signing Secret and private key are never passed to ingress-nginx. An explicit
edge Secret that collides with the internal leaf fails rendering.

#### Pre-`0.4.12` edge certificate migration

Upgrading from control-plane chart `0.4.11` or earlier changes the default edge
Secret name. A single Helm upgrade cannot guarantee that ingress-shim issues
that new Secret before ingress-nginx observes the changed Ingress. Pre-issue it
while the existing Ingress still references the old Secret, then switch in a
second phase (with Flux/reconcilers suspended when that is the deployment's
normal maintenance procedure):

```sh
release=iterabase
namespace=iterabase-system
host=app.example.com
issuer=letsencrypt-prod
edge_secret="${release}-control-plane-api-ingress-tls"
staging_certificate="${edge_secret}-preissue"

# Phase 1: issue the replacement without changing the live Ingress.
cat <<EOF | kubectl apply -f -
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ${staging_certificate}
  namespace: ${namespace}
spec:
  secretName: ${edge_secret}
  dnsNames:
    - ${host}
  issuerRef:
    name: ${issuer}
    kind: ClusterIssuer
EOF
kubectl wait -n "$namespace" --for=condition=Ready \
  "certificate/$staging_certificate" --timeout=10m
kubectl get secret "$edge_secret" -n "$namespace"
kubectl get secret "$edge_secret" -n "$namespace" \
  -o jsonpath='{.data.tls\.crt}' | base64 -d | \
  openssl x509 -noout -checkhost "$host"

# Preserve the issued Secret but remove the temporary Certificate so
# ingress-shim can become its sole owner after the upgrade.
kubectl delete certificate "$staging_certificate" -n "$namespace" \
  --cascade=orphan
kubectl get secret "$edge_secret" -n "$namespace"

# Phase 2: run the normal upgrade with the deployment's complete values. Update
# any explicit ingress.tls.secretName override to $edge_secret first.
helm upgrade "$release" \
  oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform \
  --version <target-version> -n "$namespace" -f /path/to/values.yaml --wait
kubectl wait -n "$namespace" --for=condition=Ready \
  "certificate/$edge_secret" --timeout=10m
kubectl get ingress "${release}-control-plane-api" -n "$namespace" \
  -o jsonpath='{.spec.tls[0].secretName}{"\n"}'
```

Do not delete `<release>-control-plane-api-tls` after the switch: with internal
TLS it remains the API backend's serving leaf. Resume reconciliation only after
the new edge Certificate is Ready and the route passes trusted TLS validation.
The fixture and negative collision checks run through
`make check-private-control-plane`.

MetalLB pools and advertisements are **ordinary kept resources**, not Helm
hooks (DES-HOR-511-01/05): `IPAddressPool`/`L2Advertisement` carry
`helm.sh/resource-policy: keep`, so upgrades, exact reapplies, uninstall, and
rollback never delete them or the LoadBalancer VIP they assign. The nine
MetalLB 0.16.1 CRDs are owned as umbrella template resources gated by
`metallb.enabled` (DES-HOR-511-03). A direct (`Forge`-less) `helm install` of
the umbrella with `metallb.enabled=true` therefore needs the same bounded
pre-apply/bootstrap Forge performs automatically, in order, before Helm can
REST-map the pools:

1. Render the **exact target chart archive** with the release namespace and
   extract its `CustomResourceDefinition` documents (`helm template -n
   <namespace>` → keep the `.metallb.io` ones).
2. Mark those nine CRDs Helm-adoptable for the incoming release
   (`meta.helm.sh/release-name`/`-namespace` + `app.kubernetes.io/managed-by:
   Helm`), apply them server-side with `kubectl apply --server-side
   --force-conflicts`, and wait for every one to become `Established`.
3. `helm install` with `--set metallb.crds.validationFailurePolicy=Ignore`, wait
   until the MetalLB controller Deployment is Available and
   `metallb-webhook-service` has ready endpoints, then run the same upgrade
   again without the override so the admission webhook converges to its
   steady-state `Fail` policy. Forge performs this bootstrap; a direct operator
   must repeat it and verify the live webhook policy is `Fail`.

### Historical pre-0.4 upgrade and rollback ownership procedures

> These procedures preserve certificate/MetalLB history and test non-storage
> transitions only. They do not authorize upgrading a local-path installation
> into platform `0.4.0`. `DES-HOR-545-01` requires a clean host/OPO1 rebuild,
> explicit blank disks, a new receipt-bound VG, and fresh OpenEBS claims.

**Upgrading from a hook-era predecessor (0.3.19 and earlier, whose pools were
Helm hooks).** Before running step 1 above, transfer the live hook-created pools
and advertisements into the incoming release (DES-HOR-511-01) so the upgrade
preserves their UIDs instead of Helm rejecting the new ordinary resources:

```sh
pools=$(kubectl get ipaddresspool,l2advertisement -n <namespace> \
  -l app.kubernetes.io/instance=<release> -o name)
kubectl annotate --overwrite $pools \
  meta.helm.sh/release-name=<release> \
  meta.helm.sh/release-namespace=<namespace> \
  'helm.sh/hook-' 'helm.sh/hook-weight-'  # strip the hook metadata
```

This ownership/hook-metadata transfer is exactly what the transition scenario
asserts: the pools keep their `meta.helm.sh` owner and lose their `helm.sh/hook`
markers, so a subsequent upgrade adopts them rather than recreating them.

**Rollback to a hook-era predecessor.** A `helm rollback <release> <previous>
-n <namespace> --wait` to a pre-DES-HOR-511 revision is safe and **does not tear
down the pools or the LoadBalancer VIP**: because they carry
`helm.sh/resource-policy: keep` and were Helm-adopted, the rollback leaves their
UIDs, desired specs, and ownership/hook metadata intact and the wire route stays
healthy. This predecessor-pool restoration is **proven** (not merely claimed) by
the `metallb-upgrade-reapply` transition: it captures the predecessor desired
specs before the rollback, then after the rollback **re-queries the live objects**
and asserts their exact `meta.helm.sh/release-name` and
`meta.helm.sh/release-namespace` ownership, `helm.sh/resource-policy: keep`, and
the absence of **both** `helm.sh/hook` and `helm.sh/hook-weight` markers (plus
UID, desired-spec, VIP, and route) — for the tested edge and internal pools and
their advertisements — never asserting metadata cached before the rollback.
During every blocking upgrade, exact reapply, rollback, and forward recovery, the blocking operation itself runs under a concurrent **fail-closed** observation
of the Service LoadBalancer identity and route: any kubectl read error, empty or
changed VIP, or route failure fails the scenario rather than being skipped.

**Safe predecessor reapply / forward recovery.** The supported recovery after
rolling back to a hook-era predecessor is a forward re-upgrade to the current
chart, which re-owns the kept resources under continuous observed VIP and route
continuity and restores the steady-state `Fail` admission policy. Re-running the
hook-era chart's own raw resources is **not** a supported regression path: that
revision's hooks would try to recreate objects that the kept release still owns.
Always recover via forward re-upgrade rather than re-applying the predecessor's
raw manifests.

### Internal CA authority (HOR-528)

`global.internalTLS.ca.commonName` and `global.internalTLS.ca.duration` are the
single internal-CA identity contract. The ordered `cert-manager-substrate`
companion and the platform's `cert-issuers` subchart both render the root
Certificate from those values, so there is exactly one definition of the
authority no matter which release applies first.

The companion's bootstrap hook creates the platform-owned bootstrap
ClusterIssuer, root Certificate, and `internal-ca` ClusterIssuer **only when they
are absent**, then verifies the live specs and fails closed on any drift; its
RBAC grants no `patch`/`update` on those objects. The platform chart owns and may
re-render the same objects afterwards, which is what makes the platform's first
apply the only writer of an existing root. Cert-manager re-issues (rotates) the
root whenever its spec changes, which would invalidate every leaf already signed
by the current key, so:

- a per-chart override (`cert-issuers.internal.ca.commonName`/`duration`) fails
the render instead of diverging from the bootstrapped authority;
- `make check-certificate-substrate` proves the hook manifest and the platform
chart render identical CA specs for the default and shared overrides, and that
the hook remains create-only;
- `make test-e2e-internal-tls` and `make test-e2e-observability-tls` assert one
root authority on its first revision, prove every issued workload leaf chains to
the mounted root, and (internal TLS) prove a reconcile leaves the root key
material unchanged.

Changing the shared identity on a running installation rotates the authority.
Intentional rotation is not implemented; perform it as an explicit migration
rather than by editing these values in place.

### Historical certificate ownership handoff from platform 0.2.2 or earlier

This is retained evidence for the 0.3 line, not a supported path into 0.4.0.

Platform 0.2.2 bundled cert-manager and the CSI driver in the platform Helm
release. **Do not install the companion substrate first** on an existing
installation: its resources intentionally have the same names, and Helm will
reject the second release's ownership metadata. Use this staged hand-off, with
the deployment's normal values arguments in both platform upgrades:

```sh
platform_values=(-f /path/to/current-values.yaml) # add the deployment's usual --set options too

# 1. Upgrade the old owner first. Existing certificate Secrets remain valid;
#    defer the newly introduced runner until the substrate is restored.
helm upgrade iterabase \
  oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform \
  --version 0.3.0 -n iterabase-system "${platform_values[@]}" \
  --set control-plane.toolRunner.enabled=false --wait

# 2. The old chart keeps its CRDs. Transfer those six retained objects to the
#    companion release, then install the new owner.
cert_crds=$(kubectl get crd -l app.kubernetes.io/name=cert-manager -o name)
kubectl annotate --overwrite $cert_crds \
  meta.helm.sh/release-name=iterabase-cert-manager \
  meta.helm.sh/release-namespace=iterabase-system
helm install iterabase-cert-manager \
  oci://ghcr.io/nunocgoncalves/iterabase-charts/cert-manager-substrate \
  --version 0.3.0 -n iterabase-system --wait

# 3. Reconcile the intended values now that the operator and CSI driver are Ready.
helm upgrade iterabase \
  oci://ghcr.io/nunocgoncalves/iterabase-charts/iterabase-platform \
  --version 0.3.0 -n iterabase-system "${platform_values[@]}" --wait
```

Forge detects a pre-0.3 platform release and performs this same platform-first
hand-off before returning to the normal substrate-first order. The companion
release name must be `<platform-release>-cert-manager`; the examples therefore
pair `iterabase` with `iterabase-cert-manager`.

Rollback uses the inverse hand-off. First uninstall the companion release, then
rollback the platform to its pre-0.3 revision; a direct rollback collides with
the still-owned companion resources:

```sh
helm uninstall iterabase-cert-manager -n iterabase-system --wait
cert_crds=$(kubectl get crd -l app.kubernetes.io/name=cert-manager -o name)
kubectl annotate --overwrite $cert_crds \
  meta.helm.sh/release-name=iterabase \
  meta.helm.sh/release-namespace=iterabase-system
helm rollback iterabase <pre-0.3-revision> -n iterabase-system --wait
```

The chart-owned compiled Kind scenario exercises both directions against the
released 0.2.2 chart via `make test-e2e-certificate-migration`.

### Enabling an operator-backed dependency during upgrade

Helm does not install CRDs from a dependency that was disabled on the original
release. Before enabling observability—or another operator-backed dependency—
extract CRDs from the **exact target chart archive**, select the authoritative
schema for duplicate CRDs using its operator-owned version annotation, apply
those CRDs server-side, and wait for every CRD to become `Established` before
running `helm upgrade`. Forge performs this sequence automatically. Direct Helm
operators must perform the same ordered operation; applying the regular custom
resources first can fail during REST mapping before any chart hook executes.

The chart-owned `test/e2e/transition-baselines.json` preserves local F0 fixture
inputs only. Required planning obtains the exact platform/substrate `0.3.12` and
MetalLB hook predecessor `0.3.19` OCI/archive identities from the one pinned
complete release-baseline snapshot; the source file is not baseline authority.
The DES-HOR-511 transition still covers the hook→ordinary pool/VIP preservation
path through upgrade and reapply. The supported inverse boundary is current → the declared
predecessor within the post-0.3 companion-ownership model, followed by a current
forward recovery. Roll back the platform release before the companion substrate.
CRDs, generated Secrets, and PVCs are retained. The separate pre-0.3 ownership
handoff above remains mandatory; arbitrary-version rollback safety is not
claimed.

Run `make test-e2e-feature-enable` for the absent-CRD path,
`make test-e2e-observability-ingress-recovery` for the interrupted single-node
private-ingress path, and `make test-e2e-reapply-rollback` for idempotent reapply
plus the general inverse/forward recovery evidence.

## Flux-backed gateway tool runner

The control-plane chart can deploy the HOR-397 Node 24 runner as a two-container
pod. Its materializer receives a projected, read-only Kubernetes token limited
to `get` on the configured Flux `GitRepository`; the runner receives the mTLS
SPIFFE leaf but no Kubernetes or Git credential. The materializer verifies the
exact Flux artifact digest and writes immutable generation directories. The
runner mounts those files read-only, validates all manifests/bundles atomically,
and connects outbound to the tool gateway. It exposes no inbound execution API;
when `control-plane.metrics.enabled=true` (or the narrower
`control-plane.toolRunner.metrics.enabled=true`), a metrics-only ServiceMonitor
scrapes separate materializer and runner `/metrics` endpoints, matching the inference
gateway observability pattern.

Configure exact dotted tool-name namespaces through
`control-plane.toolRunner.allowedToolNamespaces`; wildcards are not supported.
Defaults retain at most eight generations / 512 MiB and drain old pins for up to
24 hours. Invalid revisions leave the last valid generation serving. Product and
client bundle authoring is documented in the overlay's `tools/README.md`.

A valid, Ready Flux `GitRepository` named `overlay` must exist before installing
with the runner enabled because readiness intentionally requires a validated
first generation. Forge establishes and gates on the exact source revision and
digest before Helm. Generic chart-only CI disables the runner because it has no
cross-repository runner image or Flux source; the dedicated kind+Flux contract
covers the enabled runtime path.

**Dispatch (Work server).** The control-plane chart deploys the durable Work
bidi-stream gRPC server (HOR-249) at `control-plane.dispatch.*`, enabled by
default in the umbrella. AgentPool/warm-agent workers connect over mTLS, receive
durable graph-node assignments and return completion events. Its serving leaf is
signed by the shared platform workload CA (`platform-spiffe-ca`) with the
Dispatch Service name as a SAN, so a worker's `controlPlane.serverName` matches
it and its client CA verifies the worker's SPIFFE leafs. The default model
permission (`control-plane.dispatch.defaultModel.id/api`) is
**customer/overlay-specific** and is set by the overlay — a dispatch enabled
without one fails closed at startup (it never emits an empty model permission),
so the shared umbrella does not hardcode a model.

## Immutable artifacts

The MinIO chart provisions `iterabase-artifacts` plus a dedicated bucket-scoped
credential consumed only by the control-plane API/gateway. Its versioned,
ordinary Helm provisioner Job remains `Complete` and Helm-owned without TTL
cleanup for the active subchart revision, so later blocking upgrades and exact
reapplies observe the same Job identity. Sandboxes and tool runners have no
object-store credential or direct route. Retention is indefinite
unless `control-plane.artifact.defaultRetention` is configured. See
[`docs/artifact-operations.md`](docs/artifact-operations.md) for round-trip and
explicit deletion validation.

## Develop

```sh
make check                  # Helm lint/template + kubeconform + static contracts
make check-tls              # TLS presets, including observability + TLS together
make test-e2e-unit          # compiled suite + intentional break fixtures (no cluster)
make test-e2e-install       # current clean OpenEBS LVM install + claim lifecycle
make test-e2e-observability
make test-e2e-observability-tls
make test-e2e-internal-tls
# Historical 0.3-line non-storage transition evidence; never 0.4 migration authority:
make test-e2e-certificate-migration
make test-e2e-upgrade
make test-e2e-feature-enable
make test-e2e-observability-ingress-recovery
make test-e2e-reapply-rollback
```

The runtime targets are chart-owned typed Go scenarios built on `testkit/e2e`.
Each current target creates exactly one isolated Kind cluster, runs once without retries,
and collects shared redacted diagnostics on failure. See
[`test/e2e/README.md`](test/e2e/README.md) for scenario and fixture contracts.

Requires `helm` and `kubeconform`; do not add or trust mutable Helm repository indexes. `make build-deps` reads `.github/inputs/remote-content.json`, downloads each reviewed external archive directly, verifies its exact SHA-256 before packaging, and vendors local dependencies recursively. A byte mismatch is terminal; bounded retries cover transport only. The same authority pins every rendered third-party runtime image by digest. The companion substrate contains exact `cert-manager` and `cert-manager-csi-driver` archives and no cert-manager custom resources, so Helm can establish and wait for the operator before the platform release submits issuers and Certificates.

## Observability (HOR-408)

The optional `observability` subchart (disabled by default) deploys Prometheus +
Grafana + Loki + Alertmanager via `kube-prometheus-stack` + `loki`, with the
Prometheus Operator CRDs (`ServiceMonitor` / `PodMonitor` / `PrometheusRule`),
PV-backed storage, default alert rules, and a Loki Grafana datasource. Enable it
with the preset:

```sh
helm install iterabase charts/iterabase-platform -n iterabase-system --create-namespace \
  -f values-kind.yaml -f values-observability.yaml --wait
```

The preset flips the stack on **and** every component's `metrics.enabled` knob,
so Prometheus scrapes inference-gateway; control-plane API, manager, gateway,
dispatch, harness workers, tool runner, and materializer; vLLM model-backend
pods; Postgres/Redis exporters; MinIO; and enabled upstream substrate targets.
Dedicated metrics listeners keep customer ingress and mandatory-mTLS workload
listeners isolated.
GPU metrics: set `observability.dcgmExporter.enabled=true` (gpu-operator must be
installed out-of-band). The default monitor matches GPU Operator's exact
`app=nvidia-dcgm-exporter` Service label and `gpu-metrics` named port across
namespaces; deployments with a supported renamed contract may override both.
The Grafana namespace variable includes discovered DCGM exporter jobs so the
out-of-band namespace remains selectable in the shipped GPU panels.
Alertmanager **email routing is overlay-owned** — the
chart ships a null-receiver default; set
`observability.kube-prometheus-stack.alertmanager.config` in the prod overlay
(HOR-408: the OPO1 overlay carries the email receiver).

Grafana provisions the immutable seven-dashboard Iterabase production suite
under `Iterabase`, focused component dashboards under `Infrastructure` and
`Observability`, and bundled kube-prometheus-stack dashboards under `Kubernetes`.
Dashboard source, stable UIDs, ordering, and provenance live
under `charts/observability/dashboards`; runtime Internet imports are forbidden.
The chart also ships bounded recording rules, invariant production alerts, and
runbook links. `up == 0` coverage is paired with explicit per-component absence
checks under `observability.alerts.expectedTargets`, so a vanished Pod, Service
endpoint, or monitor still alerts. Disable expectations for intentionally omitted
core components, and enable the `harness` / `modelBackend` expectations when an
overlay creates those operator-managed workloads. Workload-specific performance
alerts remain off until an overlay sets accepted thresholds under
`observability.alerts.performance`.

The vLLM `PodMonitor` selects operator-created model-backend pods by their stable
label and named serving port. GPU panels require the optional DCGM target.

## Release

Raw tags do not publish. `Chart.yaml` is the chart version authority. Dispatch
the root **Release candidate** workflow with an explicit affected-target set
and an exact master SHA. It packages the selected chart archives once, builds
any selected component/Forge artifacts, and validates the coherent bundle
before retaining it as Actions artifacts. Then dispatch **Promote release**
with that successful run ID.
After founder approval in the protected `release` environment, the unchanged
archive is published. See [`../docs/release.md`](../docs/release.md).
