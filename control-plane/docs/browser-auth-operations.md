# Authentication operations (HOR-453, HOR-582)

- **Status:** implemented; sign-in is always on for every installation
- **Authority:** [`docs/architecture/v2-authentication-authority.md`](../../docs/architecture/v2-authentication-authority.md) (HOR-451 `DES-HOR-451-01`–`14`, HOR-582 `DES-HOR-582-01`–`03`)
- **Component:** `control-plane` (`cmd/api` `serve`/`bootstrap`), `charts/charts/control-plane`
- **Tickets:** [HOR-453](https://linear.app/horizonshift/issue/HOR-453/v2-deliver-local-access-request-verification-password-and-browser), [HOR-582](https://linear.app/horizonshift/issue/HOR-582/v2-v2-only-authority-browser-sessions-reach-work-legacy-keyscrds)

This runbook covers the V2 local access-request, verification, password, and
browser-session journey, first-Admin bootstrap, recovery, and the one-step
upgrade of an existing installation. API-credential Settings and Inference
Gateway enforcement are owned by HOR-514 and HOR-513.

## Configuration

There is no auth-disabled mode. Every installation's deployment overlay
supplies the following through chart values and Secrets; rendering and startup
fail closed without them.

| Input | Chart value | Notes |
| --- | --- | --- |
| Exact public browser origin | `auth.publicOrigin` | Absolute HTTPS origin, no path. Used for email links and exact-origin CSRF checks. |
| Transactional SMTP | `auth.email.*` + `auth.email.existingSecret` | Verified TLS only (`starttls` or `tls`); `mode: plain` fails chart render and startup validation. |
| Hosted mail (SES) | `auth.email.provider: ses`, `auth.email.ses.region` | Derives `email-smtp.<region>.amazonaws.com`; the per-environment SES SMTP credential (`username` + `existingSecret`) is required (HOR-603). Self-hosted installs keep `provider: smtp` with their own relay. |
| Private relay CA | `auth.email.caSecret` / `auth.email.caKey` | Optional PEM bundle trusted in addition to the system roots. Verification is never disabled. |
| First Admin | `auth.bootstrap.adminEmail`, `auth.bootstrap.adminLocale`, `auth.bootstrap.existingSecret` | Named by the overlay; read from a Secret; never printed. |
| Trusted proxies | `auth.trustedProxies` | Exact CIDRs. The direct peer is trusted otherwise. |
| Advisory GeoIP | `auth.geoip.existingClaim` / `auth.geoip.databasePath` | Optional local database; failures only remove location labels. |
| Session bounds | `auth.session.idleTTL` / `absoluteTTL` / `recentAuthTTL` | May only tighten the approved 12 h / 30 d / 15 min maximums. |

## First Admin bootstrap

`api bootstrap` runs as the RBAC-less init container under an advisory lock,
keyed on `identity.bootstrap_marker` (`DES-HOR-582-03`):

1. With no marker, it makes the overlay-named email a `setup_pending` Admin —
   converting an existing local account with that email or creating one —
   clears any password, revokes that human's sessions and credentials, queues a
   one-time setup link through the configured mail relay, and records the
   marker. Other accounts, including any other Admin, are untouched.
2. Every later run is a strict no-op. A marker with no local users fails closed
   (`ErrBootstrapInconsistent`).
3. It never creates or prints an API key, setup token, or password.

The Admin completes first-time setup from the emailed link, which activates
the account without creating a browser session; sign-in is explicit. Everyone
else joins through the access-request flow: request, verify the emailed link,
Admin approval as Operator or Admin, emailed setup, and sign-in.

## Upgrading an existing installation (OPO1)

Migration `000031_v2_only_authority` moves an installation forward in one step
(`DES-HOR-582-02`), after `000030_backend_auth_ref` (HOR-597). It rewrites `user` roles to `operator`, revokes every
legacy scoped key, drops the authority epoch, the `IdentityMapping` binding
table, `PermissionPolicy` storage, and the `identity.active_api_keys` lookup,
limits the broad capability to workload identities, and applies the exact
DES-HOR-451-14 gateway grant union. Work, artifact, attribution, and security
history are untouched. The transition is one-way: recover by rolling forward
or by catastrophic full restore.

1. **Rehearse on a copy first (required).** Restore a fresh `pg_dump` of the
   installation into a scratch Postgres, run the release image's
   `api migrate up` against it with `DATABASE_URL` pointing at the copy, and
   confirm that it finishes clean and that history is still readable:

   ```sql
   SELECT count(*) FROM work.work_items;          -- unchanged from the source
   SELECT count(*) FROM work.timeline_events;     -- unchanged from the source
   SELECT count(*) FROM artifact.artifacts;       -- unchanged from the source
   SELECT role, status, count(*) FROM identity.local_users GROUP BY 1, 2;  -- admin|operator only
   SELECT count(*) FROM identity.api_keys WHERE status <> 'revoked' AND key_type IS NULL;  -- 0
   ```

   Record the run (source dump time, migration result, the query outputs) on
   the ticket before releasing. Do not apply the release without it. An earlier
   rehearsal ending at migration 30 is historical evidence, not proof for this
   combined 31-migration release.
2. Set the `auth.*` values above in the overlay, naming the first Admin.
3. Apply the release. The migration runs in the `migrate` init container, then
   `bootstrap` emails the first Admin a setup link.
4. The chart no longer renders the `IdentityMapping`, `PermissionPolicy`, or
   `EgressRoute` CRDs, so Helm deletes them. Objects created by the retired
   reconcilers still carry their finalizers and hold the CRDs in `Terminating`.
   Release them once. All three resources are namespaced, so patch each object
   in its own namespace, then confirm the CRDs are gone:

   ```bash
   for kind in identitymappings permissionpolicies egressroutes; do
     kubectl get "$kind" -A \
       -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' \
       | while read -r ns name; do
           kubectl patch "$kind" "$name" -n "$ns" --type=merge \
             -p '{"metadata":{"finalizers":null}}'
         done
   done
   kubectl get crd identitymappings.platform.iterabase.com permissionpolicies.platform.iterabase.com egressroutes.platform.iterabase.com
   # Expect: NotFound for all three.
   ```

5. Existing humans without a password are `setup_pending`. They request
   access; the Admin's approval keeps their existing identity (and so their
   work and attribution history), applies the reviewed role, and emails a fresh
   setup link. Legacy API keys do not survive; automation moves to
   V2 credentials (HOR-514). The Inference Gateway's customer listener refuses
   bearer requests until HOR-513 (`DES-HOR-582-01`); workload inference is
   unaffected.

## Recovery

`api bootstrap --recover-admin --admin-email <email>` is the cluster-operator
path and succeeds only while **no active Admin exists**:

- Creates or converts the named human to `setup_pending` Admin.
- Clears the old password, revokes every browser session and API credential
  the human acts as or owns, invalidates outstanding setup/reset links, queues a
  normal setup email, and audits (`recover_admin_completed`).
- Product mutations must never leave zero active Admins; this command is the
  only recovery path once no Admin can sign in.

## Revocation and retention

| Action | Effect |
| --- | --- |
| Sign out this device | Terminates the current session; the next authenticated request is denied. |
| Revoke one session | Own-session revocation, idempotent, no recent-password step. |
| Revoke all other sessions | Keeps the current session; API keys are unchanged. |
| Password reset completion | Replaces the password, consumes all reset links, revokes every browser session, keeps API keys. |
| Account disablement / role change | Revokes sessions and suspends affected credentials on the next request. |

Retention: raw session IP is server-only and purged no later than 30 days after
termination; access-request personal data is purged 180 days after a terminal
state; `identity.security_events` is append-only. Network evidence is stored
separately in `identity.security_event_network` so it can be purged
independently.

## Failure semantics

| Failure | Behavior |
| --- | --- |
| Database unavailable | Session and credential authorization denies; no stale authority fallback. |
| Missing auth configuration | Chart render and API/bootstrap startup fail closed; routes report `auth_unavailable`. |
| SMTP unavailable before acceptance | Durable outbox retry with bounded attempts; readiness stays honest. |
| SMTP acceptance ambiguous | Recorded as `outcome_unknown`; no automatic resend, explicit resend rotates the link. |
| Audit insert fails | The security mutation rolls back. |
| GeoIP/User-Agent parsing fails | Authentication succeeds without location/client labels. |
| Malformed trusted-proxy chain | Falls back to the direct peer. |
| Role/account changes race a request | The transactionally resolved current authority controls that request. |

## Observability

Security evidence lives in `identity.security_events` (core, 180 days) and
`identity.security_event_network` (bounded network evidence, ≤ 30 days).
Operational diagnosis should use those bounded rows; never query or log raw
passwords, one-time tokens, session tokens, or SMTP credentials.
