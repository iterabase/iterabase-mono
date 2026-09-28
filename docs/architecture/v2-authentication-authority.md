# V2 local authentication, API access, and identity-authority contract

- **Status:** Approved design; implementation is owned by follow-on tickets.
- **Approval date:** 2026-08-23
- **Architecture ticket:** [HOR-451](https://linear.app/horizonshift/issue/HOR-451/v2-approve-local-authentication-browser-session-role-and-identity)
- **Product contract:** Obsidian `Platform V2 — Managed Digital Workforce — Product Requirements`, especially `REQ-004`, `REQ-010`–`REQ-014`, `REQ-033`, `REQ-035`–`REQ-037`, `SCN-001`, `SCN-002`, `SCN-009`, `SCN-018`, and `SCN-021`
- **Design handoff:** Obsidian `Designs/Platform V2 — API Access Designer Brief`
- **Implementation owners:** HOR-453, HOR-454, HOR-513, HOR-514, plus the endpoint-owning tickets identified below

This record is the single repository authority for the V2 local-authentication, browser-session, customer-role, customer API-credential, Inference Gateway authorization, attribution, and legacy-authority migration design. It does not implement the design.

## 1. Approved decisions

The founder approved these decisions for HOR-451. Product behavior changes require a new durable decision rather than an implementation-local interpretation.

| ID | Decision |
| --- | --- |
| `DES-HOR-451-01` | Personal API keys may remain human-owned; service identities must not remove durable human accountability. |
| `DES-HOR-451-02` | Every API key has a required accountable human owner and a request actor. Personal owner and actor are the same human; automation uses a distinct service actor. Service actors inherit no owner role. |
| `DES-HOR-451-03` | V2 browser authority is opaque server-backed sessions plus current database `operator|admin` roles. API keys use explicit actions. Delegated JWT/JWKS issuance, `POST /v1/token`, customer `IdentityMapping`/`PermissionPolicy` authority, and wildcard customer grants retire at cutover. |
| `DES-HOR-451-04` | A future verified external identity maps to the canonical human. Trusted resource/tool boundaries re-evaluate the initiating human's current authority intersected with request-actor and workflow/agent capability. |
| `DES-HOR-451-05` | New customer-originated work persists initiating human, request actor, and executing workflow/agent as separate principals. |
| `DES-HOR-451-06` | Use the approved Argon2id password floor, opaque session cookie, bounded session lifetime, exact-origin/session-bound CSRF, persistent throttling, and live account/role checks. |
| `DES-HOR-451-07` | Active-session metadata is privacy-aware: trusted-proxy client derivation, server-only bounded raw IP, local advisory GeoIP, normalized UA labels, coalesced activity, caller-only listing, and immediate audited revocation. |
| `DES-HOR-451-08` | `identity.security_events` is the append-only identity security audit; independently purgeable network evidence is separate. Domain ledgers remain authoritative for work/runtime/tool effects. |
| `DES-HOR-451-09` | Authentication email uses a hash-only transactional outbox. Tokens are generated at send time, raw values are never persisted, SMTP uncertainty is explicit, and successful reset atomically changes password/revokes sessions without implicitly revoking API keys. |
| `DES-HOR-451-10` | Verified access-request state is separate from durable account state. Approval creates `setup_pending`; setup activates without creating a browser session. |
| `DES-HOR-451-11` | First Admin bootstrap and no-active-Admin recovery use the normal email setup channel and never print or create a bootstrap Admin API key. |
| `DES-HOR-451-12` | The V2 authority epoch is an irreversible `expand → preflight/setup → maintenance cutover → verify/cleanup` transition. Post-epoch recovery is roll-forward or catastrophic full restore, never legacy-writer rollback. |
| `DES-HOR-451-13` | Customer API access uses the complete fixed action catalogue in this record. Security/credential lifecycle is browser-only; consequence-sensitive bearer access is deferred; workload/internal and admin/debug surfaces are never customer actions. Customer model listing/chat invocation have separate actions, current per-request authority, operator-managed model/rate policy, and payload-free attributable usage. Delivery is split into HOR-514 and HOR-513. |
| `DES-HOR-451-14` | Customer Inference Gateway authorization uses indexed live reads over its existing dedicated shared-PostgreSQL connection, with no request-authority cache. For the customer-authority surface, the existing role adds `SELECT` only on the bounded credential/catalogue projections and `INSERT` only on payload-free usage events; failures deny before backend invocation. The same role retains separately approved exact object-level routing/workload reads. A future Redis authorization-cache worker is deferred and requires a separate architecture decision preserving immediate revocation semantics. |

The full approval metadata for `DES-HOR-451-14` is durable in Obsidian `HOR-451 — Direct PostgreSQL inference authority decision`: Nuno Gonçalves approved it on 2026-08-23 for the customer inference request path, with the exact transport, grants, failure/isolation consequences, deferred alternative, HOR-451 link, and PR #58 review evidence.

## 2. Scope and non-goals

### In scope

- Local access request, verification, setup, sign-in, password reset, sign-out, browser-session inspection/revocation, and password reauthentication.
- Exactly two customer roles: `operator` and `admin`.
- Human identities, service actors, personal API keys, accountable automation credentials, action selection, rate context, suspension, rotation, revocation, and ownership history.
- Complete customer HTTP endpoint classification for control-plane and Inference Gateway.
- Current-authority and three-principal attribution contracts for direct API starts and inference.
- Identity security audit, transactional authentication email, first-Admin bootstrap/recovery, chart/configuration prerequisites, and failure semantics.
- One-way migration from legacy local roles, broad grants, delegated JWT, API-key purposes, and identity/permission CRD customer authority.

### Non-goals

- Runtime, API, schema, controller, UI, chart, or gateway implementation in HOR-451.
- SSO, linked identities, Teams/Graph identity, notifications, or scheduled starts.
- Customer permission/policy builders or per-user/per-workflow resource grants.
- Customer workflow, tool, AgentPool, capability, provider, backend, routing, model-exposure, rate-policy, or runtime-credential configuration.
- New OpenAI-compatible endpoints beyond customer model listing and chat completions.
- Bearer Chat, blocker approval, revision, cancellation, restart, consequence confirmation, extraction retry, password/session/key lifecycle, bootstrap, or recovery.
- Managed hosting or hosted inference.

## 3. Authority and trust boundaries

### 3.1 Authorities after the V2 epoch

| Concern | Authoritative source | Non-authoritative after cutover |
| --- | --- | --- |
| Human identity, email, role, account state | PostgreSQL `identity.identities` + `identity.local_users` | `IdentityMapping`, `PermissionPolicy`, browser claims, UI state |
| Browser authentication | Hashed opaque `identity.browser_sessions` row + current local-user state | JWT browser cookie, cached role, user-supplied identity |
| Customer API credential | Hashed `identity.api_keys` row + current owner/actor/account/role + immutable actions + API catalogue | Key-supplied scope, owner role snapshot, wildcard grant |
| Automation accountability | Required active Admin owner + immutable service actor + permanent ownership history | Operator/inactive owner, service actor alone, mutable audit text |
| Customer inference models | Operator-managed `catalog.effective_api_catalog` | Key-selected provider/backend, raw gateway snapshot alone |
| Customer inference authority | Indexed live per-request PostgreSQL read from `identity.inference_api_credentials`; no request-authority cache | Stale authorization snapshot, successful request cache entry |
| Workflow/agent execution capability | Existing Iterabase-operated workflow/AgentPool/tool/credential policy | Customer role or API action alone |
| Work/tool effects | Work/runtime/tool ledgers | Identity security-event detail payload |

The installation is the tenant boundary. There is no cross-installation customer identity, session, key, artifact, work, or usage access.

### 3.2 Principals

- **Initiating human:** canonical accountable human who originated a customer request.
- **Request actor:** human for browser/personal-key requests; service identity for automation requests.
- **Executing identity:** workflow/agent/workload identity that performs delegated execution.
- **Credential:** browser session or API key used at the request boundary; it is evidence, not a principal that can assert another identity.

For a browser or personal key:

```text
initiating_human = request_actor = canonical human
executing_identity = selected workflow/agent when work is delegated
```

For an automation credential:

```text
initiating_human = accountable active Admin owner at request time
request_actor = immutable service actor
executing_identity = selected workflow/agent when work is delegated
```

An automation request is authorized only while its current owner remains an active Admin. Owner disablement or Admin→Operator demotion makes every authority projection return the credential as suspended before evaluating its immutable actions. Owner transfer changes future initiating-human accountability and appends permanent history. It never rewrites prior work, inference, audit, or ownership evidence.

### 3.3 Trusted request boundaries

1. Public auth endpoints validate only their specific opaque token or submitted credentials and return enumeration-resistant responses.
2. Cookie endpoints resolve the session hash, current user/account/role, exact origin, and session-bound CSRF before dispatch.
3. Bearer endpoints hash the supplied key, resolve current owner/actor/action state, and never fall back to a cookie.
4. Direct API workflow starts persist all three principals before execution is scheduled.
5. Resource/tool boundaries re-evaluate current initiating-human authorization, request-actor credential action, and workflow/agent capability for dynamic access.
6. Inference Gateway resolves the credential authoritatively for every customer request and independently applies exact endpoint action, model exposure, and mandatory rate policy.
7. Workload-mTLS and cluster-admin/debug listeners are isolated before customer bearer middleware.

## 4. Threat model

### 4.1 Protected assets

- Password hashes; setup/reset/session/API-key secret hashes.
- Human identity, email, role, account, session, key ownership, and service-actor authority.
- Customer work, artifacts, value data, exposed model aliases, inference capacity, and usage attribution.
- Authentication email links and SMTP credentials.
- Audit, ownership, migration-epoch, and historical principal evidence.
- Runtime/tool/provider credentials and infrastructure details that must never enter customer Settings or normal product responses.

### 4.2 Threat actors

- Unauthenticated remote attacker.
- Authenticated Operator probing Admin routes/fields.
- Active or formerly active user with a copied session/key.
- Admin attempting to create ownerless/wildcard automation authority or remove the last Admin.
- Automation client attempting to inherit its owner's role or multiply limits with extra keys.
- Browser origin attacker using CSRF, CORS, cookie fixation, or mixed cookie/bearer behavior.
- Reverse-proxy metadata spoofer.
- Compromised customer key attempting internal/debug/workload access.
- Compromised application/gateway process attempting broader database access.
- Upgrade/cutover operator accidentally restoring a legacy writer or ambiguous grant.

### 4.3 Abuse cases and required controls

| Abuse case | Required control/evidence |
| --- | --- |
| Credential stuffing/enumeration | Persistent per-source/per-canonical-account throttles, generic responses, no permanent lockout, append-only failed-auth evidence. |
| Password hash theft | Self-describing Argon2id: 64 MiB, 3 iterations, parallelism 1, 16-byte salt, 32-byte output; 12–128 Unicode chars; bundled common-password rejection. |
| Session fixation/theft | Login replaces any presented token; 256-bit opaque token stored only as hash; `__Host-iterabase_session`; current account/role read every request; immediate revocation. |
| Cross-site mutation | `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, no `Domain`; exact configured origin; session-bound `X-CSRF-Token`; no credentialed CORS. |
| Cookie/bearer privilege confusion | Authorization bearer path never falls back to cookies. Credential lifecycle/security routes reject bearer authentication even if a valid cookie is also present. |
| Stale role after demotion/disable | Resolve current account/role each request. Revoke browser sessions on role change/disable. Clip/suspend personal keys on the next API request; every automation projection fails closed unless its owner is currently an active Admin. |
| Service actor inherits Admin | Require an active Admin owner but evaluate only the automation action catalogue; owner role supplies accountability and eligibility, never service authority. |
| Extra keys multiply inference limits | Most restrictive actor/credential/model policy; actor-keyed counters. Limiter failure is fail closed. |
| Model/provider discovery | `/v1/models` returns only aliases in `effective_api_catalog`; responses/logs omit provider/backend/routing/credential details. |
| Key manages keys/security | Lifecycle and identity-authority mutations are cookie + CSRF + recent-auth only; bearer returns the normal unauthorized/forbidden contract. |
| Bearer approves consequential blocker/restart/cancel | Endpoint family remains bearer-denied; no corresponding V2 action exists. |
| Raw secret leaks | Random secret shown only once; hash-only persistence; no-store response; audit/log/analytics/crash-report exclusion; safe prefix only thereafter. |
| Spoofed session location/device | Trusted-proxy chain validation; local advisory GeoIP; normalized bounded UA; metadata never authorizes. |
| SMTP duplicate/unknown effect | Transactional hash-only outbox; leased sender; retry only before definite acceptance; explicit `outcome_unknown`; resend invalidates earlier unused tokens. |
| Last Admin removed | Product APIs reject disable/demotion leaving zero active Admins; recovery command only when no active Admin exists. |
| Legacy split-brain | Durable epoch, maintenance cutover, fingerprint preflight, one transaction, immediate writer/RBAC removal, inert compatibility only. |
| Gateway DB compromise blast radius | `DES-HOR-451-14` adds only the bounded customer credential/catalogue reads and content-free usage inserts. The same role retains separately approved exact object-level routing/workload reads, but no schema-wide/default read privilege, identity mutation grant, or customer-authority use of those internal objects. |
| Prompt/response usage surveillance | `usage.inference_events` contains identifiers/counts/outcome only, never request or response payload. |

## 5. Credential and session security envelope

### 5.1 Passwords and one-time links

- Password length: 12–128 Unicode characters.
- No composition rule.
- Reject a bundled common-password list.
- Store self-describing Argon2id hashes at the approved floor.
- Setup, reset, verification, session, and API-key raw secrets are independent random values; never reuse material across credential kinds.
- Setup/reset/session/API-key bearer material has at least 256 bits of CSPRNG entropy and is persisted only as a domain-separated SHA-256 hash. The secret prefix shown in metadata is non-authenticating.
- Configuration may increase password/hash cost or shorten time bounds, not lower the approved floor.

### 5.2 Browser sessions

- Cookie: `__Host-iterabase_session; Secure; HttpOnly; SameSite=Lax; Path=/` with no `Domain`.
- Idle lifetime: 12 hours.
- Absolute lifetime: 30 days.
- Unsafe methods require exact configured-origin validation and a session-bound `X-CSRF-Token` supplied outside the cookie.
- Login always creates a new session/token and invalidates any supplied pre-login session token.
- Password reset completion, account disablement, and role change revoke all browser sessions.
- Sign-out/revoke is idempotent and prevents the next authorization.
- An already-authorized in-flight side effect is not described as reversed.
- Password reauthentication updates session-bound recent-auth evidence. The implementation must use one bounded constant and action-specific checks; it may not treat account age, session age, key use, or an API key as recent password proof.

### 5.3 Session metadata

- Trust the direct peer unless it belongs to configured trusted-proxy CIDRs.
- For a trusted peer, walk exactly one configured forwarded-address chain right-to-left to the first untrusted hop; malformed input safely falls back.
- Raw created/last IP is server-only security data, retained while active and no more than 30 days after termination.
- Advisory location comes only from an operator-mounted local GeoIP-compatible database. No outbound lookup.
- Parse at most 512 bytes of User-Agent into bounded browser/OS/device labels; store/display no raw UA.
- Coalesce last-activity/IP/location updates to five minutes. A stream counts when established.
- List only the caller's active sessions. Derive the current-session marker server-side.

### 5.4 API-key lifecycle

- Personal and automation key create/list/detail/rotate/revoke/transfer endpoints accept cookie authentication only.
- Create, rotate, revoke, and transfer require valid CSRF and recent password proof.
- Action lists are immutable on a key version. Changing actions creates a new credential/version.
- Rotation generates a new secret and permits only one strictly bounded old-key overlap. HOR-514 must define one fixed overlap within the approved bounded contract and show the exact expiry; no extension endpoint exists.
- Expiry is mandatory and bounded by the product's offered choices. No never-expiring key may be created through V2 Settings.
- Disablement suspends every key owned by that human.
- Admin→Operator demotion suspends any key containing an Admin-only action; an Operator-only personal key may continue.
- Re-enable/promotion/catalogue growth never resumes or expands a suspended/existing key. Explicit recent-auth rotation/reissue is required.
- Automation creation and owner transfer require a current active Admin owner in the same locked lifecycle transaction.
- Every Product API and Inference Gateway authority projection derives an automation credential as suspended when its owner is disabled or no longer an Admin. Re-enable/promotion never resumes it automatically.
- Owner transfer is available only for automation credentials, only between active Admins, never changes actor/actions, and appends permanent history.

## 6. Identity state machines

### 6.1 Access request

```text
verification_pending
  ├─ verified ───────────────> approval_pending
  └─ verification expires ──> expired

approval_pending
  ├─ Admin approves ─────────> approved + create setup_pending local user
  └─ Admin declines ─────────> declined
```

Rules:

- One non-terminal request per canonical normalized email.
- Only verified requests are Admin-visible/actionable.
- Verification link lifetime: 24 hours.
- Terminal decline/expiry permits a new request.
- Terminal request personal data is purged after 180 days.
- Approval chooses exactly `operator|admin`, links the request, creates the canonical human/local user as `setup_pending`, queues setup email, and audits atomically.

### 6.2 Local user

```text
setup_pending --valid setup completion--> active <--> disabled
```

- Setup link lifetime: 7 days.
- Setup completion consumes the token, stores the password hash, invalidates other setup tokens, activates, and audits atomically.
- Setup completion creates no browser session; explicit sign-in follows.
- Disabled users cannot sign in, complete setup, or receive reset mail.
- Re-enable restores eligibility but creates no session and resumes no suspended key.
- Product mutations cannot leave zero active Admins.

### 6.3 Password reset

- Request/resend changes neither password nor browser sessions and returns a generic response.
- Reset link lifetime: 30 minutes.
- Successful completion atomically validates/consumes token, replaces password hash, invalidates other reset tokens, revokes browser sessions, and audits.
- Password reset does not implicitly revoke API keys.

### 6.4 Browser session

```text
active --> revoked | idle_expired | absolute_expired | account_revoked | password_revoked | role_revoked
```

Termination is final. Listing returns active rows only plus bounded safe metadata.

### 6.5 API key/version

```text
active --> retiring --> revoked
   ├────> expired
   └────> suspended --> revoked | explicit_reissue(new version)
```

A suspended/expired/revoked version never returns to active. Rotation/reissue produces a new version/secret and preserves safe historical metadata.

## 7. Target data contract

Names below are the required logical contract. Migrations may preserve existing physical tables where compatible, but implementations must not create competing authorities.

### 7.1 `identity.identities`

Retain canonical UUIDs and actor kinds. Required V2 kinds include human, service, workflow/agent, and workload/system as already represented by the repository's identity domain. Human and service actor UUIDs are stable attribution anchors.

### 7.2 `identity.local_users`

Required fields/constraints:

- `identity_id` PK/FK to canonical human identity.
- canonical normalized email, unique for non-purged human accounts.
- original validated delivery email.
- display name and `en|pt` locale.
- role exactly `operator|admin`.
- setup state exactly `setup_pending|active|disabled` according to transition rules.
- nullable self-describing password hash only while setup is pending.
- password/account/role change timestamps and actor/correlation evidence.
- no API-writable wildcard/capability column.

### 7.3 `identity.access_requests`

Contains canonical/original email, locale, state, verification timestamps, terminal reason, linked approved identity, reviewer/role/timestamps, and purge deadline. Enforce one non-terminal canonical email with a partial unique index.

### 7.4 `identity.auth_link_tokens`

Stores only token hash, purpose `verify_access|setup_password|reset_password`, subject/request/outbox linkage, issued/expiry/consumed/invalidated timestamps, and generation. Purpose and subject are immutable. Exactly one successful consumer wins under row locking/conditional update.

### 7.5 `identity.auth_email_outbox`

Stores secret-free intent: recipient identity/address, locale, template/purpose, subject linkage, lease/attempt state, accepted/unknown/failed outcome, timestamps, and bounded error class. It never stores a raw token or rendered link.

The API worker leases an intent, creates the raw token in memory, stores only its hash/expiry, renders EN/PT content in memory, and uses verified-TLS SMTP. Retry is allowed only before definite acceptance. Ambiguous acceptance becomes `outcome_unknown`; explicit resend invalidates prior unused tokens and creates a new intent.

### 7.6 `identity.browser_sessions`

Required fields:

- session UUID and unique token hash.
- human identity.
- created, last authorized/activity-coalesced, idle expiry, absolute expiry, and termination fields.
- session-bound CSRF secret hash/proof.
- recent password-authenticated timestamp/evidence.
- server-only created/last source IP and derived bounded location.
- normalized browser/OS/device labels only.

### 7.7 `identity.api_keys`

Each key/version requires:

- UUID and credential family/version linkage.
- type exactly `personal|automation`.
- unique token hash and non-authenticating display prefix.
- required `owner_user_identity_id` referencing a human local user; personal issuance requires an active Operator/Admin, while automation creation/transfer requires an active Admin.
- required `actor_identity_id`; equal to owner for personal, distinct service identity for automation.
- immutable name/purpose, action array/set, creation actor/time, expiry, last-used coalescing, rotation predecessor, retiring deadline, revocation/suspension state and reason.
- explicit credential-level rate policy reference/values; absence cannot mean unlimited for customer inference.
- no wildcard action and no owner-role inheritance switch.
- effective owner account/role state used by every authority projection; an automation version is eligible only when its owner is currently `active` with role `admin`.

Constraints enforce personal owner=actor and automation owner≠actor/service kind. Locked creation/transfer transactions enforce an active Admin automation owner; projections independently re-check that mutable invariant and return fail-closed suspension after owner demotion/disablement. Application code and database checks reject actions outside the applicable catalogue.

### 7.8 `identity.api_key_ownership_history`

Permanent append-only rows: credential, prior/new owner, actor performing transfer, reason, correlation, and timestamp. No update/delete application grant.

### 7.9 `identity.security_events`

Append-only core event containing stable event/outcome/reason, initiating human, request actor, actor-role snapshot, subject, optional access-request/session/key, key-owner snapshot, credential kind, request/correlation identifiers, time, and bounded secret-free details.

Successful security mutations and core audit append commit atomically and fail closed. Failed-login/throttle events use a separate transaction. Application access is insert-only. Core rows retain 180 days.

### 7.10 `identity.security_event_network`

One-to-one independently purgeable network evidence for security event: raw source IP and bounded derived location. Retain at most 30 days.

### 7.11 `identity.authority_state`

Singleton durable epoch and migration evidence:

- epoch `legacy|v2` (no transition back from `v2`).
- source fingerprint, cutover ID/time/operator/release.
- preflight/result metadata and verification completion.

Every V2 customer-authority writer/consumer checks the epoch contract. V2 binaries refuse a contradictory legacy-writer configuration.

### 7.12 `identity.inference_api_credentials`

A bounded read projection for the dedicated Inference Gateway database role. It exposes only what a customer request needs:

- key hash/UUID/type/status/expiry.
- immutable action membership for the two inference actions.
- owner human UUID and current owner account state and role.
- actor UUID/kind and current personal actor role where applicable.
- effective suspension reason and credential/actor rate identifiers/limits.
- authority epoch.

It exposes no email, password/session/reset hash, security-event detail, ownership history, runtime credential, provider/backend route, or non-inference action payload beyond boolean action membership. It is backed by indexed base-key lookup and live joins; it is not a stale gateway snapshot. An automation row is effective only when its owner is currently an active Admin; owner demotion/disablement yields a suspended result before action evaluation.

Per `DES-HOR-451-14`, every customer inference request reads this projection and the bounded model catalogue live through the gateway's existing dedicated shared-PostgreSQL connection. HOR-451 permits no successful-request, in-memory, Redis, or other request-authority cache. For this customer-authority surface, the existing gateway role adds minimum schema `USAGE` on `identity`, `catalog`, and `usage`, exactly `SELECT` on `identity.inference_api_credentials` and `catalog.effective_api_catalog`, and exactly `INSERT` on the payload-free `usage.inference_events` ledger. It receives no schema `CREATE`.

Those are not the role's complete grants. The gateway keeps its existing single PostgreSQL role/connection, minimum schema `USAGE` on `permissions`, `toolgateway`, and `runtime`, and these separately approved exact object-level reads for non-customer paths:

- `catalog.effective_catalog` for internal backend routing.
- `permissions.effective_capabilities` and `permissions.effective_rate_limits` for the workload scope's capability/rate pipeline.
- `toolgateway.pools`, `runtime.turns`, `runtime.workflow_runs`, `runtime.run_pool_assignments`, and `runtime.turn_assignments` for live workload-mTLS pool/turn/assignment authorization.

Customer bearer middleware cannot use those internal routing/workload objects as customer authority, and the internal data must not enter customer responses. The V2 grant migration revokes the legacy `identity.active_api_keys` customer lookup and schema-wide/default gateway read privileges, then materializes the exact union above. The role receives no identity mutation, broad raw-table select, update, or delete privilege. Workload-mTLS listener isolation and fail-closed authorization remain unchanged.

### 7.13 `catalog.effective_api_catalog`

Operator-managed installation-wide customer API model projection:

- exposed stable alias and customer-safe metadata.
- enabled state.
- required model-level rate policy identifiers/limits.
- no provider credentials.
- no customer key/user grants.

Operator reconciliation remains the only writer. Personal Operators/Admins and automation actors receive the same approved alias set; action and rate checks differ by credential/actor, not model visibility grants.

### 7.14 `usage.inference_events`

Append-only content-free usage evidence:

- event/request/correlation IDs and timestamps.
- phase exactly `accepted|completed|failed|canceled|outcome_unknown`; one accepted event precedes the backend attempt and one terminal event follows when its outcome is known.
- key UUID, owner human UUID, request actor UUID, credential kind.
- exact action and exposed model alias.
- status/outcome/error class, stream flag, timing, input/output token counts when known.
- no prompt, response, hidden model message, tool content, provider credential, or raw Authorization value.

The usage ledger is not the identity security audit and is not a billing promise. It remains durable under the installation's approved data/backup policy.

### 7.15 Work/runtime principal seam

New customer-originated work item and `runtime.workflow_run` retain:

- `initiating_human_identity_id`.
- `request_actor_identity_id`.
- existing `scope_identity_id` as executing workflow/agent.
- source `chat|api`, optional browser session/API key, and authorization snapshot references.

Active assignment resolves the run; callers cannot supply trusted principal UUIDs. Historical rows are immutable evidence. Current dynamic authorization is re-evaluated at the trusted boundary.

## 8. Authentication and lifecycle APIs

Exact path spelling may follow repository conventions, but endpoint ownership and authentication mode are fixed.

### Public browser journey

- Request access and generic resend/status acknowledgement.
- Verify access-request email.
- Complete first-time password setup.
- Sign in.
- Request/resend/complete password reset.

These endpoints accept only their submitted credential/token, use enumeration-resistant responses, and never accept an API key as substitute authority.

### Cookie-session customer APIs

- Current profile and locale.
- Password reauthentication/change.
- Session list/current/revoke/sign-out.
- Admin verified-access-request read/review.
- Admin role/account/session-security mutations.
- Personal API-key and Admin automation lifecycle.
- Chat, blocker response/confirmation, extraction retry, cancellation, revision, and restart according to their owning product contracts.

Unsafe calls require CSRF. Security/lifecycle calls requiring recent password proof reject stale proof.

### Bearer API resolution

1. Require exactly one supported Authorization bearer value.
2. Hash it using the API-key domain.
3. Resolve active key, owner, actor, current account/role, actions, expiry/suspension, and epoch.
4. Require the exact route action and any documented prerequisite action.
5. Apply current product resource/model policy and mandatory rate policy.
6. Record/coalesce last use without turning an audit failure into silent authorization.
7. Persist key/owner/actor on the domain effect/evidence.
8. Never fall back to cookie auth or legacy grants.

## 9. Complete action and endpoint catalogue

### 9.1 Customer actions

#### Personal key — active Operator or Admin

- `workflows.read`
- `workflows.start`
- `work.read`
- `work.feedback.write`
- `artifacts.read`
- `artifacts.upload`
- `inference.models.read`
- `inference.chat.invoke`

#### Personal key — current Admin only

- `people.read`
- `value.read`
- `artifacts.delete`

`value.read` also requires the relevant `work.read` or `workflows.read`. `artifacts.delete` also requires `artifacts.read`.

#### Automation credential

- `workflows.read`
- `workflows.start`
- `work.read`
- `artifacts.read`
- `artifacts.upload`
- `inference.models.read`
- `inference.chat.invoke`

No wildcard, custom string, implied child action, or arbitrary action is accepted.

### 9.2 Presets

Personal:

- Observe operations: `workflows.read`, `work.read`, `artifacts.read`.
- Start and review work: Observe + `workflows.start`, `work.feedback.write`, `artifacts.upload`.
- Inference client: `inference.models.read`, `inference.chat.invoke`.
- Admin People directory: `people.read`.
- Admin reporting: `workflows.read`, `work.read`, `value.read`.
- Admin artifact deletion: `artifacts.read`, `artifacts.delete`.

Automation:

- Workflow automation: `workflows.read`, `workflows.start`, `work.read`, `artifacts.read`, `artifacts.upload`.
- Inference client: `inference.models.read`, `inference.chat.invoke`.

Presets may be combined and reduced to supported subsets. Catalogue growth never mutates existing keys.

### 9.3 Control-plane endpoint classification

| Endpoint/family | Class | Required contract |
| --- | --- | --- |
| Product shell `/`, `/assets/*` | Browser/public shell | Not a key action. |
| `/healthz`, `/readyz`, metrics | Workload/internal | Probe/operations only. |
| `/.well-known/jwks.json`, `POST /v1/token` | Legacy/internal then removed | Bounded delegated-token drain only; no V2 customer action. |
| Workflow catalogue/summary/detail/process reads | Personal + automation | `workflows.read`; customer-safe projection only. |
| `POST /v1/work-items` | Personal + automation | `workflows.start`; mandatory idempotency and three-principal attribution. |
| Work list/detail/dashboard/attempt/node/timeline/consequence/blocker/feedback/link metadata and work-event SSE | Personal + automation | `work.read`. Value fields additionally require current Admin + `value.read`. Automation receives Operator-safe projection. |
| `POST /v1/work-items/{id}/feedback` | Personal only | `work.feedback.write`; saving starts no work. |
| Blocker response/confirmation | Cookie session; bearer deferred | Current body can confirm consequential invocation IDs. No V2 key action. |
| Revision/restart | Cookie session; bearer deferred | Consequence-safe confirmation owned by HOR-463. No V2 key action. |
| Cancellation | Cookie session; bearer deferred | Consequence-safe confirmation owned by HOR-464. No V2 key action. |
| Artifact list/search/recent/status/metadata/get/head/download | Personal + automation | `artifacts.read`; unavailable/quarantined bytes remain denied. |
| Artifact upload | Personal + automation | `artifacts.upload`; security/size/type policy applies. |
| Artifact delete | Personal current Admin only | Both `artifacts.read` and `artifacts.delete`; active-reference block/tombstone/audit; no UI, Operator, or automation. |
| Extraction results/status | Personal + automation | `artifacts.read`. |
| Extraction retry | Cookie session only | No V2 bearer processing-control action. |
| People/access-request reads | Personal current Admin only | `people.read`; bounded projection. |
| Access approve/decline, role/account/session-security mutations | Cookie + recent auth only | Bearer always denied. |
| Profile/password/session/key/automation lifecycle | Cookie (+ recent auth where specified) only | Bearer always denied. |
| Chat conversation/message/search/stream/tool/confirmation | Cookie session only | No V2 Chat bearer action. |
| Value projections/export | Personal current Admin only | `value.read` plus owning resource read. |
| Value-model writes | Iterabase/operator internal | No customer action. |
| Permission/identity debug | Cluster admin/debug | No customer action. |
| Runtime runs/turns/assignment, dispatch, tool/artifact RPC, CRDs/controllers | Workload/internal | No customer action. |

Ordinary list/get/detail/dashboard/status/search/download/SSE operations deliberately share the owning domain read action; scope proliferation is not allowed without a new product decision.

### 9.4 Inference Gateway endpoint classification

| Listener/endpoint | Class | Required contract |
| --- | --- | --- |
| Customer `/health`, `/readyz`, metrics | Workload/internal | No customer key action. |
| Customer `GET /v1/models` | Personal + automation | Exactly `inference.models.read`; only aliases in `effective_api_catalog`. |
| Customer `POST /v1/chat/completions` stream/non-stream | Personal + automation | Exactly `inference.chat.invoke`; authoritative key lookup, model/rate policy, usage event. |
| `/admin/v1/health`, `/admin/v1/snapshot` | Cluster admin/debug | Customer key always denied. |
| Workload-listener models/chat | Workload/internal | SPIFFE/mTLS + durable run/turn/model assignment; never customer bearer. |
| `responses`, `embeddings`, image/audio/file/fine-tuning/assistant/batch or any other OpenAI-compatible route | Unsupported/deferred | 404/deny until a separate action, data/retention contract, and ticket are approved. |

### 9.5 Inference request sequence

```mermaid
sequenceDiagram
  participant C as Customer client
  participant G as Inference Gateway
  participant I as identity.inference_api_credentials
  participant M as catalog.effective_api_catalog
  participant L as Existing limiter substrate
  participant B as Model backend
  participant U as usage.inference_events

  C->>G: Bearer request
  G->>I: Indexed hash lookup (every request)
  I-->>G: key + owner + actor + current authority + limits
  G->>M: Resolve exposed alias + model limits
  M-->>G: customer-safe alias policy
  G->>L: Enforce most restrictive actor/credential/model rates
  G->>U: Append content-free accepted event
  alt authority/catalogue/limiter/accepted-event unavailable or denied
    G-->>C: Fail closed; no backend call
  else allowed
    G->>B: One inference attempt
    B-->>G: response/stream or honest failure
    G->>U: Append content-free terminal event
    G-->>C: response/stream; never automatic retry
  end
```

The implementation may retain the repository's existing Redis limiter substrate solely for rate counters. Per `DES-HOR-451-14`, Redis is not customer authorization authority: credential and catalogue state are read live from PostgreSQL on every customer request, and failed reads or accepted-event inserts deny before backend invocation. A future Redis authorization-cache worker is explicitly deferred; adopting one requires a separate architecture decision and proof that revocation, disablement, demotion, expiry, and suspension still take effect on the next request. Any other datastore/transport/isolation change likewise requires separate approval.

## 10. Role, account, and credential transitions

| Transition | Browser sessions | Personal keys | Owned automation credentials |
| --- | --- | --- | --- |
| Operator → Admin | Revoke; sign in again under current role | Existing actions unchanged; no Admin action added | No change unless separately created as Admin |
| Admin → Operator | Revoke | Admin-action key suspended; Operator-only key may continue | Existing owned automation credentials are suspended until audited transfer/reissue by an active Admin |
| Active → Disabled | Revoke | Suspend all | Suspend all owned credentials |
| Disabled → Active | None created | Remain suspended | Remain suspended |
| Password reset completion | Revoke | No implicit revocation | No implicit revocation unless owner/account policy separately changes |
| Key rotation | No change | New version; old version bounded retiring overlap | Same |
| Automation owner transfer | No change | N/A | Only to an active Admin; future owner changes, actor/actions unchanged, history append |
| Key expiry/revocation | No change | Final for that version | Final for that version |

## 11. Identity security audit

### 11.1 Core event families

- access request created/verified/expired/approved/declined/purged.
- setup/reset requested/sent/unknown/completed/failed/resend.
- login success/failure/throttled, logout, session revoked/expired.
- password changed/reset.
- account enabled/disabled; role changed; last-Admin mutation denied.
- personal/automation key created/revealed-completion/rotated/expired/suspended/revoked.
- automation owner transferred.
- bootstrap/recovery attempted/completed/denied.
- authority preflight/cutover/verification/cleanup.

Core identity audit does not duplicate work/runtime/tool/inference payloads. Domain ledgers carry domain effects with principal snapshots.

### 11.2 Atomicity

- A successful security lifecycle mutation and its core event are one transaction; audit insert failure aborts the mutation.
- Failed login/throttle evidence uses a separate transaction so failure is recordable without a successful auth mutation.
- SMTP external acceptance is represented by outbox state/events and cannot be made database-atomic; uncertainty is explicit.
- The content-free accepted usage event must append after authorization/model/rate checks and before backend invocation; failure prevents the backend call. A terminal event is appended after completion/failure when possible. Terminal-event uncertainty may not leak payload or cause an automatic inference retry.

## 12. Bootstrap and recovery

### Fresh install

A locked, RBAC-less `api bootstrap` init container:

1. Reads first-Admin email/locale from an operator-created Secret.
2. Requires valid exact public HTTPS origin and verified-TLS SMTP configuration.
3. Takes a database advisory lock.
4. Only if no local users and no bootstrap marker exist, atomically creates one canonical `setup_pending` Admin, queues normal setup email, records marker, and audits.
5. Never creates or prints an Admin API key or raw setup token.
6. Is a strict no-op on a consistent restart; inconsistent user/marker state fails closed.

### Recovery

`bootstrap --recover-admin` is cluster-operator-only and permitted only when no active Admin exists. Under the same lock it creates or converts the named human to `setup_pending` Admin, clears old password, revokes sessions and owned keys, queues normal setup email, and audits. Product APIs preserve the last-active-Admin invariant.

## 13. Required configuration and secret boundary

### 13.1 Public customer API channels

The deployment operator, through repository-owned chart/configuration values, is the sole configuration owner for two independent non-secret channel records:

- **Product API:** `enabled`, absolute public HTTPS `base_url`, and customer-safe availability `available|temporarily_unavailable`.
- **OpenAI-compatible Inference API:** `enabled`, absolute public HTTPS `base_url`, and customer-safe availability `available|temporarily_unavailable`.

The logical field names above are contract names; chart keys may follow established repository nesting. These rules are fixed:

1. Either channel or both channels may be enabled. An enabled URL is absolute HTTPS, contains no userinfo/query/fragment, has one normalized base path, and routes only to that channel's public customer listener. It must not identify a health, metrics, workload, admin/debug, provider, backend, or database endpoint. The Product API may share the browser origin, but its base URL is still configured and validated independently.
2. A disabled channel is omitted from Settings and one-time reveal examples. Its actions are absent from new-key selection and denied by the current API-channel catalogue for existing keys; disabling does not mutate immutable action lists. Re-enabling never widens or automatically resumes an existing credential.
3. Chart/static validation and the owning component's startup validation reject an enabled record with an invalid URL, unknown availability value, or listener-class mismatch. Actual Internet reachability is not a startup requirement and a transient outage does not rewrite configuration.
4. The control-plane Settings API returns only enabled customer-safe records: channel label, normalized public base URL, and availability. Reveal examples are additionally filtered to the APIs required by the selected actions. It never returns internal service names, raw health/readiness, routing, credentials, or configuration provenance.
5. Availability is an operator-owned customer-safe channel state, not browser probing or an infrastructure-health feed. `temporarily_unavailable` keeps the URL visible with the approved “Requests to this base URL are failing right now. Nothing about your key has changed.” meaning. It neither revokes nor rotates a key; the owning public API returns its normal customer-safe unavailable response, while the other enabled channel and browser credential management continue independently.

This contract covers the approved both-enabled, Product-only, Inference-only, and temporarily-unavailable Settings states without introducing a customer configuration surface.

### 13.2 Other required configuration

Startup/chart validation must require or validate:

- exact public HTTPS browser origin, independently from the public API channel URLs.
- session/CSRF and API-key hashing domain configuration where required by implementation.
- trusted proxy CIDRs and one forwarded-address header/chain contract.
- optional operator-mounted local GeoIP database path/version behavior.
- verified-TLS SMTP host/port/mode/from address and Secret-backed credentials.
- first-Admin Secret email/locale for fresh install; no synthetic default.
- Inference Gateway dedicated database DSN/credentials with the `DES-HOR-451-14` customer-authority additions plus the retained exact routing/workload grants in section 7.12.
- mandatory installation/customer actor/credential/model rate policy; absence cannot imply unlimited inference.
- operator-managed customer API model exposure catalogue.
- retention workers/settings for identity network/core events and access-request purge.

Normal customer Settings and APIs never reveal secret or internal configuration values. Invalid security/email/model/rate/API-channel prerequisites fail the owning chart/startup/readiness boundary without flapping unrelated authenticated API health after a transient SMTP or public-channel outage.

## 14. Failure semantics

| Failure | Required behavior |
| --- | --- |
| Database unavailable during browser/key authorization | Deny; no stale authority fallback. |
| Inference credential/catalogue read fails | Deny before backend call. |
| Required inference limiter fails | Deny; never unlimited/fail-open. |
| Accepted usage insert fails | Deny before backend invocation; do not retry inference. |
| Enabled public API channel configuration is invalid | Reject chart/startup validation for the owning configuration; do not expose an unsafe or internal URL. |
| Enabled public API channel is temporarily unavailable | Keep its safe public URL visible with bounded unavailable copy; do not change the credential; return the owning customer-safe unavailable response and keep the other channel/browser lifecycle independent. |
| Terminal usage append is uncertain after backend/stream starts | Preserve the single accepted/inference attempt, emit bounded operational evidence/metric, return honest stream termination where possible, never retry. HOR-513 must make the exact phase sequence testable. |
| SMTP unavailable before acceptance | Durable outbox retry; auth/API readiness remains honest and unrelated authenticated traffic can continue. |
| SMTP acceptance ambiguous | `outcome_unknown`; no automatic resend; explicit resend rotates token. |
| Audit insert fails for security mutation | Roll back/fail closed. |
| GeoIP/UA parsing fails | Auth succeeds without location/labels; never widen/deny based on advisory metadata. |
| Session revoke races in-flight request | Subsequent authorization denied; already completed effect stands. |
| Role/account change races API request | The transactionally resolved current authority controls that request; next boundary observes committed change; audit retains decision snapshot. |
| Cutover preflight mismatch | Do not enter maintenance cutover; no partial epoch. |
| Post-epoch legacy binary/writer | Refuse startup/write; roll forward. |

## 15. One-way V2 authority migration

### 15.1 Expand

- Add target tables/columns/projections/indexes without switching authority.
- Add epoch state and compatibility readers.
- Deploy code capable of preflight but keep legacy writers authoritative.
- Rehearse backup/restore and migration against a production-shaped copy.

### 15.2 Preflight/setup

Block cutover on:

- canonical email collisions or non-deliverable/synthetic active Admin identity.
- no valid future active Admin/setup path.
- active key without an accountable human owner or valid actor.
- automation key whose owner is not a current active Admin, including an owner mapped or demoted to Operator.
- wildcard/unknown action or legacy admin/token key not marked for revocation.
- absent/malformed rate policy where required.
- a gateway grant plan that leaves schema-wide/default customer-authority reads or removes any required exact routing/workload grant from section 7.12.
- unresolved mapping into the fixed V2 action catalogue.
- delegated JWT lifetime beyond the bounded drain.
- source fingerprint drift.
- unrehearsed backup restore or failed compatibility verification.

Preserve canonical UUIDs. Map role `admin→admin`, `user→operator`. Existing humans complete email setup rather than receiving invented passwords. Do not invent unavailable historical role snapshots.

Legacy key disposition:

- legacy `admin` and `token` keys: revoke.
- legacy wildcard/unknown key: block until revoked or explicitly remapped.
- legacy personal `work` key: preserve human owner/actor and map only the approved fixed work/start subset supported by its verified use; never infer Admin/security actions.
- legacy service key: require an operator-supplied active Admin owner and distinct service-actor manifest.
- legacy `gateway` key: map only to `inference.models.read` and `inference.chat.invoke` with an explicit active Admin owner, distinct service actor, and materialized mandatory rates.
- when mapping is ambiguous, cutover blocks rather than widening.

### 15.3 Delegated JWT drain

- Clamp new delegated token lifetime to 15 minutes.
- Wait out previously longer valid tokens.
- Stop issuance.
- Permit only the final bounded validation/JWKS drain.
- Quiesce customer ingress/identity consumers before the epoch transaction.

### 15.4 Maintenance cutover

Under one advisory lock and source fingerprint:

1. Quiesce customer ingress and legacy identity consumers/writers.
2. Verify backup/restore evidence.
3. In one transaction, backfill V2 local roles/account state, API-key owners/actors/actions/rates/history, security audit, inert CR-derived evidence, retired-key revocations, removal of broad customer wildcard grants, and `identity.authority_state.epoch=v2`.
4. Replace the gateway's legacy schema-wide/default reads and `identity.active_api_keys` customer lookup with the exact customer-authority plus retained routing/workload grant union in section 7.12.
5. Start only V2 consumers and verify current-authority reads plus workload-mTLS model/chat authorization and routing.
6. Remove legacy reconcilers/finalizers/writer RBAC immediately.

There is no interval with two customer-authority writers.

### 15.5 Verify/cleanup

- Verify reference Admin/Operator login/session/role boundaries.
- Verify personal/service key mappings, revocation, and three-principal API start.
- Verify gateway current-authority, model filter, rate, and usage behavior.
- Verify legacy issuance/writers/grants are unavailable.
- Keep legacy CR objects/types inert and deprecated for one compatibility release.
- Remove served legacy CRDs/types in the next semantic release under the release process.

### 15.6 Rollback boundary

- Before epoch flip: restore previous release/database as rehearsed.
- After epoch flip: roll forward with V2-compatible binaries.
- Never restart legacy writers, wildcard grants, delegated issuance, or customer CR authority.
- Full pre-cutover database/release restore is catastrophic disaster recovery only, with explicit outage and loss of all post-cutover writes.

## 16. Validation plan

### Architecture and static traceability

- Every `REQ`/`SCN` cited at the top maps to a section/test owner.
- Enumerate every control-plane and inference-gateway customer HTTP route; every route is exactly customer-action, browser-only, deferred, workload/internal, or admin/debug.
- Static check forbids wildcard customer actions and unclassified new customer routes.
- Static copy/design check forbids runtime/provider credentials and V3 features in API Settings.

### Authentication/security tests

- Password Argon parameters/common-password/Unicode bounds.
- Verification/setup/reset expiry, reuse, resend, race, and enumeration resistance.
- Cookie attributes, fixation replacement, CSRF/origin, CORS, idle/absolute expiry, role/account/session revocation races.
- Trusted-proxy spoofing/malformed chains, bounded UA, GeoIP failure, activity coalescing, retention purge.
- Security mutation/audit atomicity and insert-only grants.
- SMTP accepted/definite failure/ambiguous acceptance and multi-replica leasing.
- Bootstrap restart/inconsistency and no-active-Admin recovery/last-Admin invariant.

### Role/key/endpoint tests

- Operator/Admin browser projection matrix and direct API negatives.
- Personal and automation action/preset creation; arbitrary/wildcard/browser/deferred/internal/debug injection rejected.
- Owner/actor constraints, active-Admin automation creation/transfer/migration invariants, fail-closed projection after owner disablement/demotion, ownership history, and no resume after re-enable/promotion.
- One-time reveal/no-store/log/analytics/crash-report exclusion.
- Rotation overlap, expiry, revocation, suspended finality, and no automatic widening/resumption.
- Every endpoint-family row in section 9, including prerequisites and no scope proliferation.
- Product/Inference public base-URL validation, both/either-enabled projection, action clipping, temporarily-unavailable copy/failure isolation, and no internal endpoint disclosure.
- Personal/automation API start idempotency and three-principal attribution.

### Inference tests

- Separate models/chat actions; action confusion denied.
- Current-authority next-request behavior after revoke/disable/demotion/expiry/suspension, using indexed live PostgreSQL reads with no request-authority cache.
- Exposed/non-exposed/unknown alias behavior with no provider/backend leak.
- Mandatory most-restrictive actor/credential/model rates; multiple keys cannot multiply actor limits.
- Authority/catalogue/limiter outage fail closed.
- Streaming/non-streaming disconnect, pre-header/mid-stream/backend uncertainty with exactly one inference attempt.
- Usage key/owner/actor/action/alias/tokens/outcome and payload absence.
- Exact database-grant regression: customer authority has only minimum schema `USAGE`, bounded projection reads, and payload-free ledger inserts with no schema `CREATE`; existing backend routing and workload-mTLS model/chat reads remain functional; legacy/default broad customer reads are absent.
- Workload-mTLS and admin/debug listener isolation regressions.
- Every unsupported OpenAI-compatible route denied.

### Migration/release tests

- Production-shaped preflight matrix and source-fingerprint drift.
- Email collision, owner/actor manifest, wildcard/unknown action, rate, JWT drain, and no-Admin blockers.
- Atomic epoch switch and failure injection at every phase.
- Inert CR compatibility then semantic-release removal.
- Pre-epoch rollback and post-epoch roll-forward/catastrophic-restore rehearsal.
- HOR-467 fresh install/upgrade with reference Admin/Operator, personal keys, automation actor, model filtering/rates/usage, and direct negative endpoint matrix.

## 17. Ticket ownership

| Ticket | Owns |
| --- | --- |
| HOR-451 | This approved architecture/decision contract only. |
| HOR-453 | Access request, verification/setup/reset, local password, browser session/CSRF, recent password evidence, auth email/bootstrap packaging and journeys. |
| HOR-454 | Role/People enforcement, browser-only identity mutations, target authority schema/migration, key owner/actor/action substrate, inference credential projection, and exact grant-union migration preserving routing/workload reads. |
| HOR-514 | Personal/automation Settings and browser-only credential lifecycle, presets, one-time reveal, rotate/revoke/transfer/suspension UI/API. |
| HOR-513 | Inference Gateway exact actions, `DES-HOR-451-14` live PostgreSQL lookup/bounded customer grants/no authority cache, model filter, mandatory rates, usage ledger, retained routing/workload regression, listener isolation, charts/tests. |
| HOR-452/HOR-456 | Cookie-session-only Chat contract/implementation; no bearer Chat action. |
| HOR-455 | Confirmed Chat effects plus personal/automation `workflows.start` handoff/attribution. |
| HOR-459 | Routed shell, Settings slots, workflow/work/value role-safe projections. |
| HOR-461/HOR-462 | Artifact read/upload/delete and status/browser-retry boundaries. |
| HOR-463/HOR-464 | Cookie-session consequence-safe restart/revision/cancellation; bearer remains denied. |
| HOR-467 | Complete shared release matrix and immutable evidence. |

## 18. Production and release classification

HOR-451 is design-only and has no runtime/data impact. Follow-on implementation changes authentication/authorization authority, customer credentials, gateway database access, rate behavior, usage evidence, and a one-way migration. Their semantic publication is deferred to the manual affected-target V2 release candidate and protected promotion flow in `docs/release.md`; merge alone is not a release.
