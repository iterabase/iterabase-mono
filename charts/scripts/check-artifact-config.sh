#!/usr/bin/env bash
# Static contract for the V2 artifact zones and processing pipeline
# (DES-HOR-460-02/05/07, HOR-461; docs/architecture/v2-artifact-processing.md
# sections 4 and 11). Renders the charts and asserts credential, mount,
# identity, and network isolation invariants on the manifests.
set -euo pipefail

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

helm template artifact-check charts/control-plane -f values-control-plane-auth.yaml \
  --set gateway.enabled=true --set dispatch.enabled=true \
  --set dispatch.defaultModel.id=m --set dispatch.defaultModel.api=a \
  --set artifact.enabled=true --set metrics.enabled=true >"$tmp/cp.yaml"
# Processing alone must still provision the shared workload CA/ClusterIssuer.
helm template artifact-check charts/control-plane -f values-control-plane-auth.yaml \
  --set artifact.enabled=true >"$tmp/cp-processing-only.yaml"
helm template artifact-check charts/control-plane -f values-control-plane-auth.yaml \
  --set artifact.enabled=true --set artifact.processing.signatures.official=false \
  --set-string 'artifact.processing.signatures.customDatabases.iterabase-e2e\.hdb=44d88612fea8a8f36de82e1278abb02f:68:Eicar-Test-Signature' \
  >"$tmp/cp-offline.yaml"
helm template artifact-check charts/minio >"$tmp/minio.yaml"
helm template artifact-check charts/iterabase-platform -f values-auth.yaml >"$tmp/platform.yaml"

expect_fail() {
  local why=$1
  shift
  if helm template artifact-check charts/control-plane -f values-control-plane-auth.yaml \
    --set artifact.enabled=true "$@" >/dev/null 2>&1; then
    echo "ERROR: render must fail: $why" >&2
    exit 1
  fi
}
expect_fail "signature refresh above the 2h bound" --set artifact.processing.signatures.refreshInterval=3h
expect_fail "signature volume on another StorageClass" --set artifact.processing.signatures.persistence.storageClassName=local-path
expect_fail "official signatures disabled without a custom database" --set artifact.processing.signatures.official=false
expect_fail "scan concurrency outside 1-8" --set artifact.processing.coordinator.scanConcurrency=9

python3 - "$tmp" charts/minio/files/policies <<'PY'
import json, pathlib, sys, yaml

tmp, policies = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
failures = []

def fail(msg):
    failures.append(msg)

def load(name):
    return [d for d in yaml.safe_load_all((tmp / name).read_text()) if d]

def find(docs, kind, name):
    hits = [d for d in docs if d["kind"] == kind and d["metadata"]["name"] == name]
    if len(hits) != 1:
        fail(f"expected one {kind}/{name}, found {len(hits)}")
        return {}
    return hits[0]

def pod(doc):
    spec = doc.get("spec", {})
    return spec.get("template", {}).get("spec", spec) if doc["kind"] != "Pod" else spec

def workloads(docs):
    return [d for d in docs if d["kind"] in ("Deployment", "StatefulSet", "Job", "DaemonSet")]

def containers(doc):
    p = pod(doc)
    return p.get("initContainers", []) + p.get("containers", [])

def secret_refs(container):
    refs = set()
    for e in container.get("env") or []:
        ref = (e.get("valueFrom") or {}).get("secretKeyRef")
        if ref:
            refs.add(ref["name"])
    return refs

def env(container):
    return {e["name"]: e for e in container.get("env") or []}

rel = "artifact-check"
ingest, serve, lifecycle = (f"{rel}-minio-artifact-{z}" for z in ("ingest", "serve", "lifecycle"))
cp = load("cp.yaml")

# API and gateway: ingest + serve only; never lifecycle or the legacy names.
for name, container in ((f"{rel}-control-plane-api", "api"), (f"{rel}-control-plane-gateway", "gateway")):
    dep = find(cp, "Deployment", name)
    c = next((c for c in pod(dep).get("containers", []) if c["name"] == container), {}) if dep else {}
    e = env(c)
    for key, secret in (("ARTIFACT_INGEST_ACCESS_KEY", ingest), ("ARTIFACT_INGEST_SECRET_KEY", ingest),
                        ("ARTIFACT_SERVE_ACCESS_KEY", serve), ("ARTIFACT_SERVE_SECRET_KEY", serve)):
        got = ((e.get(key) or {}).get("valueFrom") or {}).get("secretKeyRef", {}).get("name")
        if got != secret:
            fail(f"{name}: {key} must come from {secret}, got {got}")
    for banned in ("ARTIFACT_LIFECYCLE_ACCESS_KEY", "ARTIFACT_LIFECYCLE_SECRET_KEY", "ARTIFACT_ACCESS_KEY",
                   "ARTIFACT_SECRET_KEY", "ARTIFACT_MAX_SIZE_BYTES", "ARTIFACT_PENDING_TTL"):
        if banned in e:
            fail(f"{name}: must not render {banned}")
    if e.get("ARTIFACT_QUARANTINE_MAX_BYTES", {}).get("value") != "2147483648":
        fail(f"{name}: ARTIFACT_QUARANTINE_MAX_BYTES must render the decimal default")

# The lifecycle credential is mounted only in the coordinator; the scanner and
# updater hold no Secret-backed environment at all.
coordinator = find(cp, "Deployment", f"{rel}-control-plane-artifact-coordinator")
scanner = find(cp, "Deployment", f"{rel}-control-plane-artifact-scanner")
updater = find(cp, "Deployment", f"{rel}-control-plane-artifact-signature-updater")
for doc in workloads(cp):
    for c in containers(doc):
        refs = secret_refs(c)
        if lifecycle in refs and not (doc is coordinator and c["name"] == "coordinator"):
            fail(f"{doc['metadata']['name']}/{c['name']}: holds the lifecycle credential")
for doc in (scanner, updater):
    for c in containers(doc):
        if secret_refs(c):
            fail(f"{doc['metadata']['name']}/{c['name']}: must hold no Secret-backed credential")
        if any(k in env(c) for k in ("DATABASE_URL", "PGPASSWORD")):
            fail(f"{doc['metadata']['name']}/{c['name']}: must hold no database credential")
coord = next((c for c in pod(coordinator).get("containers", []) if c["name"] == "coordinator"), {})
if lifecycle not in secret_refs(coord):
    fail("coordinator: lifecycle credential missing")
if secret_refs(coord) & {ingest, serve}:
    fail("coordinator: must not hold the ingest/serve credentials")
dispatch = find(cp, "Deployment", f"{rel}-control-plane-dispatch")
dispatch_c = next((c for c in pod(dispatch).get("containers", []) if c["name"] == "dispatch"), {})
if env(coord).get("DATABASE_URL") != env(dispatch_c).get("DATABASE_URL"):
    fail("coordinator: DATABASE_URL must equal the dispatch control-plane credential (DES-HOR-461-04)")

# No Kubernetes token for any processing workload.
for doc in (coordinator, scanner, updater):
    name = doc["metadata"]["name"]
    if pod(doc).get("automountServiceAccountToken") is not False:
        fail(f"{name}: automountServiceAccountToken must be false")
    sa = find(cp, "ServiceAccount", pod(doc).get("serviceAccountName", ""))
    if sa.get("automountServiceAccountToken") is not False:
        fail(f"{name}: ServiceAccount must disable token automount")
    for kind in ("RoleBinding", "ClusterRoleBinding"):
        for b in (d for d in cp if d["kind"] == kind):
            if any(s.get("name") == pod(doc).get("serviceAccountName") for s in b.get("subjects", [])):
                fail(f"{name}: ServiceAccount must have no RBAC binding")
    for c in containers(doc):
        sc = c.get("securityContext", {})
        if sc.get("readOnlyRootFilesystem") is not True or sc.get("allowPrivilegeEscalation") is not False \
                or sc.get("capabilities", {}).get("drop") != ["ALL"]:
            fail(f"{name}/{c['name']}: container hardening incomplete")
    psc = pod(doc).get("securityContext", {})
    if psc.get("runAsNonRoot") is not True or psc.get("seccompProfile", {}).get("type") != "RuntimeDefault":
        fail(f"{name}: pod must be non-root with RuntimeDefault seccomp")
    for host in ("hostNetwork", "hostPID", "hostIPC"):
        if pod(doc).get(host):
            fail(f"{name}: must not use {host}")

# Signature volume: fixed class, RWO; one read-write mount (the updater);
# scanner mounts read-only at both the claim and every container mount.
claim = f"{rel}-control-plane-artifact-signatures"
pvc = find(cp, "PersistentVolumeClaim", claim)
if pvc.get("spec", {}).get("storageClassName") != "iterabase-lvm-xfs" or pvc.get("spec", {}).get("accessModes") != ["ReadWriteOnce"]:
    fail("signature PVC must be iterabase-lvm-xfs ReadWriteOnce")
rw = []
for doc in workloads(cp):
    vols = {v["name"]: v for v in pod(doc).get("volumes", []) if (v.get("persistentVolumeClaim") or {}).get("claimName") == claim}
    for vname, vol in vols.items():
        for c in containers(doc):
            for m in c.get("volumeMounts", []):
                if m["name"] == vname and not (m.get("readOnly") or vol["persistentVolumeClaim"].get("readOnly")):
                    rw.append(f"{doc['metadata']['name']}/{c['name']}")
        if doc is scanner and not vol["persistentVolumeClaim"].get("readOnly"):
            fail("scanner: signature claim must be readOnly")
if rw != [f"{rel}-control-plane-artifact-signature-updater/freshclam"]:
    fail(f"signature PVC must have exactly one read-write mount (the updater), got {rw}")
for c in pod(scanner).get("containers", []):
    for m in c.get("volumeMounts", []):
        if m["name"] == "signatures" and m.get("readOnly") is not True:
            fail(f"scanner/{c['name']}: signature mount must be readOnly")
clamd = next((c for c in pod(scanner).get("containers", []) if c["name"] == "clamd"), {})
if any(v.get("secret") for v in pod(scanner).get("volumes", []) if v["name"] in {m["name"] for m in clamd.get("volumeMounts", [])}):
    fail("scanner/clamd: the engine container must mount no Secret")
if "@sha256:" not in clamd.get("image", ""):
    fail("scanner/clamd: ClamAV image must be digest-pinned")

# Identities: coordinator client-auth leaf, scanner server-auth leaf, both from
# the shared workload CA; scanner accepts exactly the coordinator SPIFFE ID.
coord_cert = find(cp, "Certificate", f"{rel}-control-plane-artifact-coordinator")
scan_cert = find(cp, "Certificate", f"{rel}-control-plane-artifact-scanner")
if coord_cert.get("spec", {}).get("usages") != ["client auth"] or \
        coord_cert.get("spec", {}).get("uris") != ["spiffe://iterabase.local/control-plane/artifact-coordinator"]:
    fail("coordinator Certificate must be a client-auth leaf for its exact SPIFFE ID")
if scan_cert.get("spec", {}).get("usages") != ["server auth"] or \
        scan_cert.get("spec", {}).get("uris") != ["spiffe://iterabase.local/control-plane/artifact-scanner"]:
    fail("scanner Certificate must be a server-auth leaf for its exact SPIFFE ID")
for cert in (coord_cert, scan_cert):
    if cert.get("spec", {}).get("issuerRef", {}).get("name") != "platform-spiffe-ca":
        fail(f"{cert.get('metadata', {}).get('name')}: must be issued by platform-spiffe-ca")
adapter = next((c for c in pod(scanner).get("containers", []) if c["name"] == "adapter"), {})
if env(adapter).get("SCANNER_ALLOWED_CLIENT", {}).get("value") != "spiffe://iterabase.local/control-plane/artifact-coordinator":
    fail("scanner adapter must accept exactly the coordinator SPIFFE ID")
if any(d["kind"] == "Ingress" and "artifact" in d["metadata"]["name"] for d in cp):
    fail("processing Services must have no Ingress")
only = load("cp-processing-only.yaml")
if not any(d["kind"] == "ClusterIssuer" and d["metadata"]["name"] == "platform-spiffe-ca" for d in only):
    fail("artifact processing alone must provision the platform-spiffe-ca ClusterIssuer")

# Network policy: scanner zero egress; updater no ingress and DNS/HTTPS only;
# coordinator no customer ingress.
np_scanner = find(cp, "NetworkPolicy", f"{rel}-control-plane-artifact-scanner")
if np_scanner.get("spec", {}).get("policyTypes") != ["Ingress", "Egress"] or np_scanner.get("spec", {}).get("egress") != []:
    fail("scanner NetworkPolicy must deny all egress")
ing = np_scanner.get("spec", {}).get("ingress", [])
if not ing or ing[0]["from"][0]["podSelector"]["matchLabels"].get("app.kubernetes.io/component") != "artifact-coordinator" \
        or [p["port"] for p in ing[0]["ports"]] != [8443]:
    fail("scanner NetworkPolicy must admit only the coordinator on 8443")
np_updater = find(cp, "NetworkPolicy", f"{rel}-control-plane-artifact-signature-updater")
if np_updater.get("spec", {}).get("ingress") != [] or "Ingress" not in np_updater.get("spec", {}).get("policyTypes", []):
    fail("updater NetworkPolicy must deny all ingress")
ports = sorted({p["port"] for rule in np_updater.get("spec", {}).get("egress", []) for p in rule.get("ports", [])})
if ports != [53, 80, 443]:
    fail(f"updater egress must be DNS and HTTP(S) only, got {ports}")
v4 = [t["ipBlock"] for r in np_updater.get("spec", {}).get("egress", []) for t in r["to"] if t.get("ipBlock", {}).get("cidr") == "0.0.0.0/0"]
if not v4 or not {"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"} <= set(v4[0].get("except", [])):
    fail("updater egress must exclude private, link-local, and cluster ranges")
np_coord = find(cp, "NetworkPolicy", f"{rel}-control-plane-artifact-coordinator")
if [p["port"] for r in np_coord.get("spec", {}).get("ingress", []) for p in r["ports"]] != [8092]:
    fail("coordinator NetworkPolicy must admit only metrics scraping on 8092")

# Offline/E2E signature source selected purely by values.
off = load("cp-offline.yaml")
cfg = find(off, "ConfigMap", f"{rel}-control-plane-artifact-signature-updater").get("data", {}).get("freshclam.conf", "")
for line in ("ExcludeDatabase main", "ExcludeDatabase daily", "ExcludeDatabase bytecode",
             "DatabaseCustomURL file:///custom/iterabase-e2e.hdb"):
    if line not in cfg.splitlines():
        fail(f"offline freshclam.conf missing {line!r}")
custom = find(off, "ConfigMap", f"{rel}-control-plane-artifact-custom-signatures")
if "iterabase-e2e.hdb" not in custom.get("data", {}):
    fail("offline render must ship the inline custom database ConfigMap")

# MinIO: three stable zone Secrets, canonical policies, versioned retained
# provisioner with legacy revocation, and an exact admission list.
mi = load("minio.yaml")
for name in (ingest, serve, lifecycle):
    find(mi, "Secret", name)
if any(d["kind"] == "Secret" and d["metadata"]["name"] == f"{rel}-minio-artifacts" for d in mi):
    fail("legacy broad <release>-minio-artifacts Secret must no longer render")
policy = find(mi, "ConfigMap", f"{rel}-minio-artifact-policy").get("data", {})
for zone in ("ingest", "serve", "lifecycle"):
    want = json.loads((policies / f"artifact-{zone}.json").read_text().replace("__BUCKET__", "iterabase-artifacts"))
    if json.loads(policy.get(f"artifact-{zone}.json", "null")) != want:
        fail(f"provisioner policy artifact-{zone} must equal the canonical file")
jobs = [d for d in mi if d["kind"] == "Job"]
if len(jobs) != 1 or not jobs[0]["metadata"]["name"].startswith(f"{rel}-minio-artifact-provisioner-"):
    fail("expected one versioned artifact-provisioner Job")
else:
    job = jobs[0]
    if job["spec"].get("ttlSecondsAfterFinished") is not None or "helm.sh/hook" in (job["metadata"].get("annotations") or {}):
        fail("artifact-provisioner must remain an ordinary retained Helm Job")
    script = job["spec"]["template"]["spec"]["containers"][0]["args"][0]
    for snippet in ('mc mb --ignore-existing "local/$ARTIFACT_BUCKET"',
                    'mc admin user add local "$key" "$secret"',
                    'mc admin policy create local "artifact-$zone" "/policy/artifact-$zone.json"',
                    'mc admin policy attach local "artifact-$zone" --user "$key"',
                    "for zone in ingest serve lifecycle; do",
                    "mc admin policy entities local --policy artifact-service --json",
                    'mc admin policy detach local artifact-service --user "$user"',
                    'mc admin user remove local "$user"',
                    "mc admin policy remove local artifact-service"):
        if snippet not in script:
            fail(f"provisioner script missing: {snippet}")
np_minio = find(mi, "NetworkPolicy", f"{rel}-minio")
admitted = set()
for peer in np_minio.get("spec", {}).get("ingress", [{}])[0].get("from", []):
    for expr in peer.get("podSelector", {}).get("matchExpressions", []):
        if expr["key"] == "app.kubernetes.io/component":
            admitted |= set(expr["values"])
if admitted != {"api", "gateway", "artifact-coordinator"}:
    fail(f"MinIO must admit exactly api, gateway, artifact-coordinator; got {sorted(admitted)}")

# Umbrella: both sides of the cross-chart Secret-name contract agree.
pl = load("platform.yaml")
rendered = {d["metadata"]["name"] for d in pl if d["kind"] == "Secret"}
for name in (f"artifact-check-minio-artifact-{z}" for z in ("ingest", "serve", "lifecycle")):
    if name not in rendered:
        fail(f"umbrella: {name} not generated")
used = set().union(*(secret_refs(c) for d in workloads(pl) for c in containers(d)))
for name in (f"artifact-check-minio-artifact-{z}" for z in ("ingest", "serve", "lifecycle")):
    if name not in used:
        fail(f"umbrella: {name} generated but not consumed")

if failures:
    print("FAIL: artifact contract violations:", file=sys.stderr)
    for f in failures:
        print(f"  {f}", file=sys.stderr)
    sys.exit(1)
print("OK: zone credentials, legacy revocation, processing identities, read-only signatures, sole updater writer, and isolation policies")
PY

substrate=$(helm template artifact-cert-manager charts/cert-manager-substrate)
grep -q '^kind: CSIDriver$' <<<"$substrate"
grep -q '^  name: csi.cert-manager.io$' <<<"$substrate"
grep -q 'image: "quay.io/jetstack/cert-manager-csi-driver@sha256:49c82164a9b4ae13eb2ab74f57d07efe13edfbf40766975f062bcce48230ce87"' <<<"$substrate"
echo "OK: certificate substrate renders the pinned CSI driver for AgentPool leaves"
