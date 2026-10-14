# Artifact operations (HOR-399, HOR-461)

The installation is the tenant boundary. Artifact metadata, security attempts,
and lifecycle state are in Postgres. Immutable bytes are in the
`iterabase-artifacts` MinIO bucket, split into policy-separated zones
(`quarantine/`, `originals/`, `derived/`; DES-HOR-460-02). The contract is
[`docs/architecture/v2-artifact-processing.md`](../../docs/architecture/v2-artifact-processing.md).
Do not copy MinIO credentials into AgentPool, supervisor, runner, sandbox,
scanner, or updater configuration.

## Components and credentials

| Workload | Object-store credential | Other authority |
| --- | --- | --- |
| API, gateway | `<release>-minio-artifact-ingest` (quarantine write) and `<release>-minio-artifact-serve` (cleared read) | Customer/workload auth |
| `artifact-coordinator` | `<release>-minio-artifact-lifecycle` (promote and purge every zone) | Shared control-plane database credential (DES-HOR-461-04); mTLS client identity `spiffe://<trust-domain>/control-plane/artifact-coordinator` |
| `artifact-scanner` (adapter + `clamd`) | none | mTLS server identity `spiffe://<trust-domain>/control-plane/artifact-scanner`; read-only signature volume; zero egress |
| `artifact-signature-updater` (FreshClam) | none | Sole read-write mount of the signature volume; DNS and HTTP(S) egress to the update source only |

The MinIO NetworkPolicy admits only the API, gateway, and coordinator (plus
the provisioner and Prometheus). No processing workload mounts a
ServiceAccount token. Scanner and coordinator leaves come from the
`platform-spiffe-ca` ClusterIssuer, last 24 hours, and renew 8 hours early;
both processes re-read them on every handshake.

The upload path only admits bytes into quarantine. An artifact becomes
readable and linkable only after the coordinator records a clean verdict and
promotes the exact digest. Scanner outages leave uploads quarantined; they
never clear them.

## Install validation

```sh
kubectl -n <namespace> rollout status statefulset/<release>-minio
kubectl -n <namespace> wait --for=condition=complete job \
  -l app.kubernetes.io/component=artifact-provisioner --timeout=5m
kubectl -n <namespace> get secret <release>-minio-artifact-ingest \
  <release>-minio-artifact-serve <release>-minio-artifact-lifecycle
kubectl -n <namespace> rollout status deployment/<release>-control-plane-api
kubectl -n <namespace> rollout status deployment/<release>-control-plane-gateway
kubectl -n <namespace> rollout status deployment/<release>-control-plane-artifact-signature-updater
kubectl -n <namespace> rollout status deployment/<release>-control-plane-artifact-scanner --timeout=15m
kubectl -n <namespace> rollout status deployment/<release>-control-plane-artifact-coordinator
kubectl -n <namespace> get pvc <release>-control-plane-artifact-signatures
kubectl -n <namespace> rollout status daemonset/cert-manager-csi-driver
kubectl get csidriver csi.cert-manager.io
kubectl wait --for=condition=Ready clusterissuer/platform-spiffe-ca --timeout=2m
```

On a fresh install the scanner stays unready until the updater has downloaded,
validated, and published the first signature bundle, and `clamd` has loaded it
(a few minutes with the official databases).

The versioned artifact-provisioner is an ordinary Helm-owned Job, not a hook.
Its completed Job/Pod is deliberately retained without a TTL for the active
MinIO subchart revision so a later `helm upgrade --wait` or exact reapply can
continue to observe the same readiness resource. Treat its `Complete` condition,
Helm ownership labels, and UID as lifecycle evidence; do not delete or recreate
it while the release is active.

The workload ArtifactService is available only through the mandatory-mTLS
`<release>-control-plane-gateway:8090` Service. AgentPools use that Service as
their `toolGateway`, trust the chart-generated
`<release>-control-plane-gateway-ca`, and receive leaves from the
`platform-spiffe-ca` ClusterIssuer through the umbrella's pinned
`cert-manager-csi-driver`. Supervisors mount only their workload leaf and CA
chain.

Validate through the artifact API rather than MinIO:

```sh
printf 'artifact round trip\n' > /tmp/artifact.csv
curl -fsS -H "Authorization: Bearer $WORK_API_KEY" \
  -H 'Content-Type: text/csv' -H 'X-Artifact-Filename: round-trip.csv' \
  -H "Idempotency-Key: install-check-$(date +%s)" \
  --data-binary @/tmp/artifact.csv \
  https://<control-plane>/v1/artifacts > /tmp/upload.json
UPLOAD_ID=$(jq -r .uploadId /tmp/upload.json)
# Poll until the scanner clears it; the status then carries the artifact ref.
curl -fsS -H "Authorization: Bearer $WORK_API_KEY" \
  "https://<control-plane>/v1/artifacts/$UPLOAD_ID/status"
```

Record the chart revision, upload/artifact IDs, canonical SHA-256 digest,
signature version, and command results as deployment evidence. Never record
API keys or MinIO credentials.

## Legacy credential revocation (upgrade from HOR-399)

The HOR-399 chart generated one broad bucket credential,
`<release>-minio-artifacts`, attached to the `artifact-service` policy. The
upgrade replaces it:

1. The MinIO subchart generates the three zone Secrets and stops rendering
   `<release>-minio-artifacts` (Helm deletes it).
2. The new provisioner Job creates the zone users and the `artifact-ingest`,
   `artifact-serve`, and `artifact-lifecycle` policies, then finds every user
   still attached to `artifact-service`, detaches and removes it, and removes
   the policy. Rerunning it is a no-op.
3. API and gateway pods roll onto the zone credentials. Until they do,
   artifact calls on old pods fail closed; that is the cutover window
   section 19 accepts.

Verify that nothing broad remains:

```sh
kubectl -n <namespace> logs job/<release>-minio-artifact-provisioner-<chart-version>
kubectl -n <namespace> exec statefulset/<release>-minio -- sh -c \
  'mc alias set local http://localhost:9000 "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null &&
   mc admin policy list local && mc admin user list local'
```

The output must list exactly the three `artifact-*` zone policies (plus MinIO's
built-ins) and no `artifact-service` policy or user.

## Scanner readiness and signature freshness

The scanner adapter is ready only while `clamd` detects the EICAR self-test on
a bundle whose `checkedAt` is at most 24 hours old (DES-HOR-460-05). Unready
scanners issue no verdicts; pending artifacts wait and retry.

```sh
kubectl -n <namespace> get pods -l app.kubernetes.io/component=artifact-scanner
kubectl -n <namespace> logs deploy/<release>-control-plane-artifact-scanner -c adapter
kubectl -n <namespace> logs deploy/<release>-control-plane-artifact-scanner -c clamd
kubectl -n <namespace> exec deploy/<release>-control-plane-artifact-scanner -c clamd -- \
  sh -c 'cat /signatures/state.json; readlink /signatures/current; cat /signatures/previous'
```

`control_plane_artifact_scanner_ready` and
`control_plane_artifact_signature_age_seconds` back the
`IterabaseArtifactScannerUnready`, `IterabaseArtifactSignaturesAging` (20 h),
and `IterabaseArtifactSignaturesStale` (24 h) alerts.

## Signature updater

The updater runs FreshClam every `signatures.refreshInterval` (default and
maximum 2h). Each check:

1. downloads into pod-lifetime staging (`/staging/db`; seeded from the
   published bundle after a restart so updates stay incremental);
2. validates every `.cvd`/`.cld`/`.cud` with `sigtool --info` (header and
   Cisco digital signature) and rejects empty custom databases;
3. derives the bundle version as the SHA-256 of the sorted per-file SHA-256
   list;
4. when the version changed, copies the bundle into
   `versions/<version>/` (verified against its checksum list, then renamed into
   place) and switches the `current` symlink with an atomic rename;
5. atomically replaces `state.json` (`{"version","checkedAt"}`) after every
   successful check, changed or not, and keeps only the current and previous
   versions.

A failed check logs, leaves the published bundle and `state.json` untouched,
and retries after `signatures.retryInterval` (default 10m). The scanner adapter
reloads `clamd` when the published version changes and re-runs its self-test.

Signature sources (all under `control-plane.artifact.processing.signatures`):

| Value | Effect |
| --- | --- |
| `official: true` (default) | Official `main`, `daily`, `bytecode` from `database.clamav.net` |
| `mirrorURL: https://mirror.example` | FreshClam `PrivateMirror` (no DNS version lookup); add the mirror's ranges to `networkPolicy.updaterMirrorCIDRs` when private |
| `official: false` | Excludes all three official databases; requires a custom database |
| `customDatabases: {name.hdb: content}` | Chart renders a ConfigMap; each key is fetched with `DatabaseCustomURL file:///custom/<name>` |
| `customDatabasesConfigMap: <name>` + `customDatabaseFiles: [..]` | Use an existing ConfigMap and publish exactly the listed keys |

The offline E2E bundle is `official: false` plus
`customDatabases: {"iterabase-e2e.hdb": "44d88612fea8a8f36de82e1278abb02f:68:Eicar-Test-Signature\n"}`,
which is enough for the adapter's EICAR self-test.

### Mirror outage and validated rollback

During an update-source or mirror outage the scanner keeps serving the last
validated bundle until it is 24 hours old, then fails closed. Restore the
source (or set `mirrorURL`) and restart the updater to force an immediate check.

To roll back a known-bad signature release, point `current` back at the
retained previous version and record the evidence; the updater keeps it until
two newer versions are published, and new verdicts record the exact version:

```sh
kubectl -n <namespace> exec deploy/<release>-control-plane-artifact-signature-updater -- sh -c \
  'prev=$(cat /signatures/previous) && test -d "/signatures/versions/$prev" &&
   ln -s "versions/$prev" /signatures/.current.tmp && mv -T /signatures/.current.tmp /signatures/current &&
   printf "{\"version\":\"%s\",\"checkedAt\":\"%s\"}\n" "$prev" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > /signatures/.state.json.tmp &&
   mv -f /signatures/.state.json.tmp /signatures/state.json'
```

Before the next check (at most `refreshInterval` later), pin a corrected
`mirrorURL` or scale the updater to zero, or it republishes the bad release.
With the updater stopped, the rolled-back bundle ages normally and the scanner
fails closed at 24 hours. Never copy or restore an unverified bundle onto the volume; recovery
always refetches and validates.

## Coordinator, queues, and quarantine

```sh
kubectl -n <namespace> logs deploy/<release>-control-plane-artifact-coordinator
```

Watch `control_plane_artifact_security_queue_depth`,
`control_plane_artifact_security_queue_oldest_seconds`,
`control_plane_artifact_quarantine_bytes` against
`control_plane_artifact_quarantine_limit_bytes` (`artifact.quarantineMaxBytes`,
default 2 GiB), and `control_plane_artifact_security_attempts_total{outcome}`.
At the quarantine limit, uploads fail closed with
`503 processing_capacity_unavailable`. Attempts are leased and fenced in
Postgres, so restarting or scaling the coordinator never duplicates or skips a
verdict.

## Component and security updates

The ClamAV engine image is pinned by digest at
`control-plane.artifact.processing.scanner.image`; the scanner adapter and
coordinator ship in the control-plane image. Engine updates are reviewed
release changes (section 10); signature data updates independently through the
updater.

## Explicit deletion

Explicit deletion is admin-only. It immediately makes reads unavailable, the
coordinator purges the bytes from every zone, and a metadata tombstone and
historical work links remain.

```sh
curl -fsS -X DELETE -H "Authorization: Bearer $ADMIN_API_KEY" \
  "https://<control-plane>/v1/artifacts/$ARTIFACT_ID"
```

A subsequent work-scope `GET` must return `410 Gone`. Retention expiry uses the
same lifecycle automatically. `retention_until = NULL` means indefinite.
`IterabaseArtifactDeletionStuck` fires when a purge has not finished within an
hour.
