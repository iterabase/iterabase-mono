# Legacy Moby test-dependency risk

- **Status:** mitigated by non-reachability
- **Owner:** control-plane
- **Last reviewed:** 2026-09-28
- **Tracking:** HOR-499
- **Related:** HOR-586 (split `github.com/moby/go-archive` bump)

## Decision

Keep `github.com/docker/docker` pinned at `v28.5.2+incompatible` until the
legacy module publishes an import-compatible version that clears one or more
applicable advisories, or `golang-migrate`/`dktest` publish an import-compatible
upgrade or remove the legacy dependency. Validate any available exit against the
control-plane database and integration tests before changing the pin. The pin is
not a production Docker client. It exists because Go's module graph includes
tests of the imported golang-migrate PostgreSQL driver; neither control-plane
binaries nor control-plane database tests compile the legacy module.

Do not replace the pin with `github.com/moby/moby/v2` or the
`docker-v29.3.1` source tag. Moby v2 has a different module/import path, and the
Docker Engine tag is not a version of the legacy `github.com/docker/docker` Go
module. Such a replacement would not be an upstream-supported upgrade for the
consumers retaining the old imports.

The directly used testcontainers packages already use the split
`github.com/moby/moby/api` and `github.com/moby/moby/client` modules. They do not
introduce the legacy edge. Updating testcontainers alone cannot remove
golang-migrate's dependency-test requirement, so HOR-499 does not broaden into
an unrelated cross-workspace dependency upgrade.

## Split-module go-archive advisory

HOR-586 advanced the directly required split module
`github.com/moby/go-archive` from v0.2.0 to v0.3.0 in `control-plane/go.mod` and
`inference-gateway/go.mod`, closing GHSA-hfg8-hc9c-6c3h, where a crafted tar
archive could create or overwrite files outside the extraction directory through
`Unpack`, `UnpackLayer`, `Untar`, `UntarUncompressed`, and the `ApplyLayer`
helpers. The split module is a required indirect dependency reached only through
the testcontainers Docker client, and repository source invokes no archive
extraction path of its own. The legacy `github.com/docker/docker` pin, its
advisory dispositions, and the re-entry triggers below are unchanged by that
bump.

## Reachability evidence

Run from `control-plane/` with `GOWORK=off` so another workspace module cannot
contribute graph edges:

```text
$ GOWORK=off go mod why -m github.com/docker/docker
# github.com/docker/docker
github.com/nunocgoncalves/iterabase-mono/control-plane/internal/database
github.com/golang-migrate/migrate/v4/database/postgres
github.com/golang-migrate/migrate/v4/database/postgres.test
github.com/dhui/dktest
github.com/docker/docker/api/types/container
```

Both compiled-dependency checks return no package:

```bash
GOWORK=off go list -deps ./cmd/... | grep '^github.com/docker/docker'
GOWORK=off go list -deps -test ./internal/database | grep '^github.com/docker/docker'
```

The production commands import no Docker SDK. Database integration tests use
repository-defined `pgvector/pgvector:pg16` fixtures through testcontainers on
an isolated local or CI daemon. Repository source does not invoke `docker cp`,
the container archive endpoints, Docker AuthZ plugins, or Docker plugin
installation. The Docker daemon used by tests is an external test prerequisite,
not code supplied by this Go module pin.

## Advisory review

GitHub's advisory records were refreshed on 2026-08-18.

| Advisory | Severity | Upstream status for the legacy module | Required vulnerable path | Repository mitigation |
| --- | --- | --- | --- | --- |
| [GHSA-rg2x-37c3-w2rh](https://github.com/advisories/GHSA-rg2x-37c3-w2rh) | High | Affects `<= 28.5.2`; no patched `github.com/docker/docker` version is published. Moby v2 first patches it in `2.0.0-beta.14`. | A malicious running container races a volume path while an operator calls `docker cp` into it or `PUT`/`HEAD /containers/{id}/archive`. | The module is not compiled; tests accept no untrusted image and perform no archive operation. |
| [GHSA-vp62-88p7-qqf5](https://github.com/advisories/GHSA-vp62-88p7-qqf5) | Medium | Affects `<= 28.5.2`; no patched legacy-module version is published. Moby v2 first patches it in `2.0.0-beta.14`. | A malicious running container races mountpoint creation during the same copy/archive workflow. | The module is not compiled; tests accept no untrusted image and perform no archive operation. |
| [GHSA-x86f-5xw2-fm2r](https://github.com/advisories/GHSA-x86f-5xw2-fm2r) | High | Affects `<= 28.5.2`; no patched legacy-module version is published. Moby v2 first patches it in `2.0.0-beta.14`. | An operator uploads a compressed archive to a malicious image through `docker cp -` or `PUT /containers/{id}/archive`. | The module is not compiled; tests accept no untrusted image and upload no archive. |
| [GHSA-x744-4wpc-v9h2](https://github.com/advisories/GHSA-x744-4wpc-v9h2) | High | Docker Engine 29.3.1 and Moby v2 `2.0.0-beta.8` contain the fix, but no v29 legacy Go-module version exists. | A Docker AuthZ plugin relies on request-body inspection. | Tests configure no AuthZ plugin and expose no Docker API to untrusted callers. |
| [GHSA-pxq6-2prw-chj9](https://github.com/advisories/GHSA-pxq6-2prw-chj9) | Medium | Docker Engine 29.3.1 and Moby v2 `2.0.0-beta.8` contain the fix, but no v29 legacy Go-module version exists. | A user installs a malicious Docker plugin and relies on its privilege-approval flow. | Tests do not install or use Docker plugins. |

The latest published `golang-migrate` release remains v4.19.1. Its current
upstream `go.mod` retains `dktest` v0.4.6 and the legacy Docker module, while
current dktest retains `github.com/docker/docker` v28.3.3. The highest version
reported by `go list -m -versions github.com/docker/docker` is v28.5.2.
Consequently, upgrading the legacy pin cannot currently close these alerts
without an unsupported module-path substitution.

After this decision is reviewed and merged, the five Dependabot alerts may be
dismissed with Dependabot reason `not_used` ("Vulnerable code is not actually
used"), referencing HOR-499 and this document. The dismissal must be revisited
under any trigger below.

## Re-entry triggers

Reopen this decision immediately if any of the following occurs:

- `github.com/docker/docker` appears in `go list -deps ./cmd/...` or the compiled
  dependencies of a control-plane test;
- repository tests accept an untrusted image, compressed archive, or archive
  destination;
- a repository-owned test exposes its Docker API to an untrusted caller or
  configures Docker AuthZ/legacy plugins;
- `github.com/docker/docker` publishes an import-compatible legacy-module
  version that clears any applicable advisory;
- golang-migrate/dktest removes the edge or publishes an import-compatible
  patched dependency; or
- GitHub publishes corrected legacy-module patch metadata.
