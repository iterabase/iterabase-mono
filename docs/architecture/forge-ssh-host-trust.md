# Forge SSH host trust: enrollment, verification, and rotation

- **Status:** Approved design; implementation and evidence collection are owned by HOR-521.
- **Approval date:** 2026-09-24
- **Architecture ticket:** [HOR-521](https://linear.app/horizonshift/issue/HOR-521/forge-verify-pinned-ssh-host-keys-before-privileged-commands-and)
- **Decision:** `DES-HOR-521-01`
- **Publication classification:** Required — the `forge` semantic release is validated through the protected candidate/promotion flow and the resulting immutable version is deployed to OPO1. Merge is not acceptance.

This record is the repository authority for how Forge authenticates an SSH host's identity before user authentication, session creation, privileged bootstrap, remote command execution, or stdin transport. It replaces the optional inline `sshHostKey` Git pin and its insecure empty-value fallback (`ssh.InsecureIgnoreHostKey`) removed by HOR-521. A different trust source, representation, enrollment, algorithm policy, rotation model, or verification-timing contract requires a new approved architecture decision.

## 1. Approved design decision

### DES-HOR-521-01 — Forge SSH host trust: enrollment, verification, and rotation

- **Approved by:** Nuno Gonçalves
- **Approved on:** 2026-09-24
- **Scope:** The complete bounded bundle in section 2.
- **Consequences:** The config-schema break, fail-closed connection behavior, operator-owned rotation model, test-only seams, publication obligation, and OPO1 migration accepted in the bundle.
- **Evidence:** Founder approval is recorded durably in HOR-521. This repository record preserves the exact approved bundle and its operator, validation, and migration contract.

## 2. Exact approved bundle

1. **Trust source and representation (non-Git).** `spec.hosts[].sshTrustFile` is a required per-host path to a non-Git trust file on the operator machine. The file uses known_hosts-format lines and is the only authoritative host-key trust material. Inline `sshHostKey` is removed from the schema; a config still setting it fails validation with an actionable migration error and no connection attempt. Only the path ever appears in Git.
2. **Known_hosts representation, strict subset.** Only exact `address key-type key [comment]` lines for the configured address are trust material. Patterns, hashed host names, `[host]:port` forms, `@cert-authority`, and `@revoked` are never honoured; a CA/certificate marker addressing the configured host fails closed with an explicit unsupported-trust diagnostic. A populated `~/.ssh/known_hosts` may be referenced, but only under the same strict rules; the recommended path is the dedicated per-host file.
3. **Out-of-band enrollment.** `forge init` requires a founder-verified key line (interactive prompt or `--ssh-host-key`), writes it to the trust file with mode 0600 (directory 0700), and records only the path in `forge.yaml`. Forge never discovers, scans, or TOFU-learns host keys, and no Forge command mutates trust material at runtime.
4. **Host/address binding.** Each trust file is referenced by exactly one host entry and is trusted only for that entry's configured `address` at port 22, the only port Forge dials. No wildcard, multi-host, or CA/certificate trust in v1.
5. **Supported algorithms and key sets.** Accepted pin types are `ssh-ed25519`, `ecdsa-sha2-nistp256`, `ecdsa-sha2-nistp384`, `ecdsa-sha2-nistp521`, and RSA pins negotiated only as `rsa-sha2-256`/`rsa-sha2-512`. `ssh-dss`, SHA-1 `ssh-rsa` negotiation, certificate types, security-key/FIDO types, and unknown types are rejected. `HostKeyAlgorithms` is restricted to the algorithms derived from the pinned key set; exact key bytes must match one pinned key; the set is bounded to 8 keys for rotation overlap.
6. **File rules.** Regular file (symlinks rejected), owned by the invoking user, not group/other writable, containing 1–8 fully parsed keys with no duplicates. Missing, empty, malformed, or permission-violating trust fails closed before any TCP connection. Forge never falls back to `~/.ssh/known_hosts` or SSH client configuration implicitly.
7. **Verification timing and fail-closed effects.** Host-key verification is the `ssh.ClientConfig.HostKeyCallback` evaluated during SSH key exchange, before user authentication. Missing trust, an unknown key, or a mismatched key produces zero user-authentication attempts, zero session/exec requests, zero remote command execution, and zero stdin bytes. The single `ensureClient` seam constructs every connection, so ordinary commands, privileged/bootstrap commands, overlay-credential transport, and Kubernetes Secret-manifest transport cannot bypass verification. A failed verification is never cached or retried with a relaxed policy.
8. **Rotation, cutover, and failure behavior.** Planned rotation adds the new host public key to the trust file (overlap), re-keys the host, verifies with the new key, then removes the old key. Removing a key immediately stops trusting it and a server still presenting it fails closed. Emergency enrollment/recovery and rollback are deliberate operator edits made after out-of-band verification; rollback restores previous trust-file content and therefore still fails closed against an unknown server key.
9. **Diagnostics.** Errors name the address, failure stage, trust-file path, presented key type and SHA256 fingerprint, and the number and types of trusted keys. Diagnostics never contain raw trusted key bytes, private key material, overlay tokens, Secret manifests, command payloads, or stdin contents.
10. **Compatibility and test-only boundaries.** `spec.hosts[].sshHostKey` is removed with a migration error; no insecure fallback exists for empty or missing trust. No non-test build path uses `ssh.InsecureIgnoreHostKey`. Test-only injection stays behind the existing `WithSSHConfig` seam and ephemeral fake-SSH fixtures; the permanent CPU/GPU fixture E2E passes its founder-verified key through `FORGE_E2E_FIXTURE_SSH_HOST_KEY` into a generated per-run trust file, never Git, and no test-only exemption is reachable by a built Forge binary.
11. **OPO1 migration.** OPO1 removes the inline key from `iterabase-overlay-opo1/forge.yaml` and points `sshTrustFile` at a non-Git file created from the already founder-verified ed25519 key (no host re-keying). After the Forge release is promoted, OPO1 deployment evidence must prove a matching-key success and unknown/mismatch fail-closed behavior without exposing key material.
12. **Publication classification.** Forge semantic publication, protected promotion without rebuild, and deployment of the immutable version to OPO1 are required for ticket acceptance.
13. **Test and evidence obligations.** Fake-SSH integration tests instrument host-key exchange, user-authentication attempts, exec requests, and stdin receipt for matching, missing, unknown, and mismatched trust cases. Regression tests exercise command and secret-bearing `runStdin` paths with sentinel overlay-credential and Kubernetes Secret payloads and assert zero bytes on verification failure. The implementation must pass `make test`, `make lint`, `make fmt-check`, `make test-e2e-unit`, and any exact-head Release-candidate rehearsal required by the affected candidate-only boundary.

## 3. Normative contract

### 3.1 Trust file and address binding

The trust file is known_hosts-shaped. Forge reads it at provisioner construction, before dialing. A line is trust material only when its first field equals the configured `address` exactly:

```text
# founder-verified out of band; never committed to Git
10.177.10.10 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... operator-verified
```

Blank lines and `#` comment lines are ignored. Lines for other hosts, and lines using host patterns, hashing, or `[host]:port`, are not trust material for this config; if no exact entry exists the connection fails closed before dialing. Marker lines that address the configured host are rejected explicitly because Forge supports no SSH CA or certificate host authentication.

### 3.2 Permissions and ownership

The trust file must be a regular, non-symlink file owned by the invoking user with mode `&0600`-compatible semantics (`mode & 0022 == 0`, for example 0600 or 0644). A group- or world-writable trust file, a symlink, a non-regular file, an unreadable file, or a file containing only comments fails closed. `forge init` creates the default directory with 0700 and the file with 0600.

### 3.3 Verification and algorithms

Verification is the host-key callback executed during key exchange. The negotiated host key algorithm is restricted to the algorithms derived from the pinned set, and the presented key bytes must equal one pinned key. RSA pins are negotiated as `rsa-sha2-256`/`rsa-sha2-512` only; SHA-1 `ssh-rsa`, `ssh-dss`, certificate, and security-key types are rejected before dialing when they appear as pins, and are impossible to negotiate otherwise.

### 3.4 Fail-closed effects

A trust-load failure occurs before any TCP connection. A key-exchange failure occurs before user authentication; the client never opens a session, never sends an exec request, never runs a remote command, and never writes stdin. Sentinel overlay-credential and Kubernetes Secret payloads must be provably absent from the server side in those cases.

### 3.5 Rotation

Forge never mutates trust material. Rotation is an operator operation:

1. verify the new host key out of band (console/provider/physical access);
2. add the new key line to the trust file (overlap window; both keys trusted, set size ≤ 8);
3. re-key the host;
4. confirm `forge status` (or the affected workflow) succeeds with the new key;
5. remove the old key line.

Removing a key is immediate cutover. A server still presenting a removed key fails closed during key exchange, with a mismatch diagnostic. Emergency recovery uses the same out-of-band verification before editing the file. Rollback restores the previous file content only if the operator still trusts that key; it never reintroduces insecure verification.

### 3.6 Diagnostics

The mismatch/unknown diagnostic reports: the address, the key-exchange stage, the presented key type and `SHA256:` fingerprint, the number and types of trusted keys, and the trust-file path. It never prints raw key bytes or transported values. Missing/malformed/permission failures name the trust file and the violated rule without echoing file contents. Audit output retains the existing Forge behavior and gains only the verification outcome.

### 3.7 Compatibility and migration

- `spec.hosts[].sshHostKey` is removed. Configs that set it fail validation with a migration error that names `sshTrustFile` and this record; the legacy value is never echoed.
- `spec.hosts[].sshTrustFile` is required. A missing value fails validation; a missing file fails provisioning before dialing.
- `forge init --ssh-host-key` now writes the verified key to the trust file instead of `forge.yaml`. `--ssh-trust-file` selects the path; the default is `~/.forge/trust/<install-name>/<address>`.
- Test-only seams: `WithSSHConfig` remains for fake-SSH unit tests; the permanent fixture E2E writes its env-supplied key into a per-run trust file. No test-only insecure callback exists in any non-test build path.

## 4. Operator procedures

### 4.1 First enrollment

1. Obtain the host's SSH host public key out of band (provider console serial access, physical console, or provider-published metadata) and verify its `SHA256:` fingerprint against the expected host.
2. Run `forge init` and supply the verified `ssh-ed25519 ...` line (prompt or `--ssh-host-key`), or write the trust file yourself with mode 0600.
3. Confirm the generated `forge.yaml` contains only `sshTrustFile` (no key material).

### 4.2 Planned rotation

Follow 3.5. Use `forge status` between steps as the verification probe. Keep the trust set within 8 keys.

### 4.3 Mismatch / emergency recovery

A mismatch or unknown-key failure is a stop condition. Do not disable verification. Re-verify the host identity through an independent channel, then add the verified key (emergency) or complete the planned rotation. If the host was rebuilt, remove the stale key only after the new key is verified.

### 4.4 Rollback

Restore the prior trust-file content from a backup made before the rotation. Rollback does not relax verification; if the host now presents a different key, the workflow fails closed until enrollment is completed.

### 4.5 OPO1 operation

OPO1 runs `forge apply --config forge.yaml` from the OPO1 overlay checkout. The checkout keeps only `sshTrustFile`; the key material lives in the operator-controlled trust file outside Git. After the Forge release is promoted, OPO1 must demonstrate a successful matching-key operation and a sanitized negative check (sentinel trust file) that fails closed with zero authentication attempts, exec requests, and stdin bytes.

## 5. Validation and evidence obligations

- Unit tests cover file loading, permissions/ownership/symlink rejection, known_hosts strict-subset parsing, exact address binding, key-type allowlist, algorithm derivation, key-set bounds, duplicate rejection, and diagnostics content.
- Fake-SSH integration tests cover matching, missing, unknown, and mismatched trust, and assert zero authentication attempts, exec requests, and stdin bytes on failure, including sentinel overlay-credential and Secret payloads on `runStdin`.
- A scan of the built-module source must show no `ssh.InsecureIgnoreHostKey` in non-test paths.
- `make test`, `make lint`, `make fmt-check`, and `make test-e2e-unit` must pass; any affected Release-candidate-only behavior requires an exact-head candidate rehearsal per `docs/release.md`.
- OPO1 migration evidence is sanitized: version, fingerprints, outcomes, and counts only — never key material or secret payloads.

## 6. Traceability

| Surface | Contract |
| --- | --- |
| `spec.hosts[].sshTrustFile` | Required non-Git per-host trust file; exact `address` binding. |
| `forge/internal/sshprovisioner` | Single verified connection seam for commands, privileged bootstrap, overlay credentials, and Secret manifests. |
| `forge/internal/config` | Legacy `sshHostKey` rejected with migration error; `sshTrustFile` required. |
| `forge init` | Enrollment writes trust material outside Git with 0700/0600 permissions. |
| Permanent fixture E2E | Founder-verified key through `FORGE_E2E_FIXTURE_SSH_HOST_KEY` into a generated trust file; no Git material. |
| Release | `forge` candidate validation and protected promotion; OPO1 deployment of the immutable version. |
