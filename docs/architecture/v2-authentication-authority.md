# V2 local authentication, API access, and identity-authority contract

- **Status:** Approved design, amended by the founder design review of 2026-10-11/12 (section 1.1); implementation is owned by follow-on tickets.
- **Approval date:** 2026-08-23; amended 2026-10-12 ([HOR-663](https://linear.app/horizonshift/issue/HOR-663/docs-amend-the-v2-architecture-docs-to-the-2026-10-design-review))
- **Architecture ticket:** [HOR-451](https://linear.app/horizonshift/issue/HOR-451/v2-approve-local-authentication-browser-session-role-and-identity)
- **Product contract:** Obsidian `Platform V2 — Product Requirements` (2026-10-12), especially `REQ-010`–`REQ-014`, `REQ-033`, `REQ-036`, `REQ-037`, `REQ-040`, `REQ-060`, `SCN-001`, `SCN-002`, `SCN-018`, `SCN-021`, and `SCN-034`. The 2026-08-10 PRD is history.
- **Design handoff:** product design happens in code in the V2 app (HOR-459) and is reviewed on `pr-<N>` previews; the 2026-08 `Designs/Platform V2 — API Access Designer Brief` is reference only
- **Implementation owners:** HOR-453 and HOR-454 (delivered), HOR-582, HOR-585, HOR-314, HOR-248, HOR-513, HOR-514, plus the endpoint-owning tickets identified below

This record is the single repository authority for the V2 local-authentication, single-sign-on, browser-session, customer-role, customer API-credential, Inference Gateway authorization, attribution, and legacy-authority removal design. It does not implement the design. Where a 2026-08-23 decision below conflicts with section 1.1, section 1.1 wins.

## 1. Approved decisions

The founder approved these decisions for HOR-451. Product behavior changes require a new durable decision rather than an implementation-local interpretation.

| ID | Decision |
| --- | --- |
| `DES-HOR-451-01` | Personal API keys may remain human-owned; service identities must not remove durable human accountability. |
| `DES-HOR-451-02` | Every API key has a required accountable human owner and a request actor. Personal owner and actor are the same human; automation uses a distinct service actor. Service actors inherit no owner role. |
| `DES-HOR-451-03` | V2 browser authority is opaque server-backed sessions plus current database `operator|admin` roles. API keys use explicit actions. Delegated JWT/JWKS issuance, `POST /v1/token`, customer `IdentityMapping`/`PermissionPolicy` authority, and wildcard customer grants retire at cutover. *Amended by HOR-582: they are deleted, with no cutover (section 1.1).* |
| `DES-HOR-451-04` | A future verified external identity maps to the canonical human. *Delivered in V2 by HOR-314 SSO (section 1.1).* Trusted resource/tool boundaries re-evaluate the initiating human's current authority intersected with request-actor and workflow/agent capability. |
| `DES-HOR-451-05` | New customer-originated work persists initiating human, request actor, and executing workflow/agent as separate principals. |
| `DES-HOR-451-06` | Use the approved Argon2id password floor, opaque session cookie, bounded session lifetime, exact-origin/session-bound CSRF, persistent throttling, and live account/role checks. |
| `DES-HOR-451-07` | Active-session metadata is privacy-aware: trusted-proxy client derivation, server-only bounded raw IP, local advisory GeoIP, normalized UA labels, coalesced activity, caller-only listing, and immediate audited revocation. |
| `DES-HOR-451-08` | `identity.security_events` is the append-only identity security audit; independently purgeable network evidence is separate. Domain ledgers remain authoritative for work/runtime/tool effects. |
| `DES-HOR-451-09` | Authentication email uses a hash-only transactional outbox. Tokens are generated at send time, raw values are never persisted, SMTP uncertainty is explicit, and successful reset atomically changes password/revokes sessions without implicitly revoking API keys. *Hosted sender amended by HOR-582 (section 1.1).* |
| `DES-HOR-451-10` | Verified access-request state is separate from durable account state. Approval creates `setup_pending`; setup activates without creating a browser session. |
| `DES-HOR-451-11` | First Admin bootstrap and no-active-Admin recovery use the normal email setup channel and never print or create a bootstrap Admin API key. *First-Admin source amended by HOR-582 (section 1.1).* |
| `DES-HOR-451-12` | ~~The V2 authority epoch is an irreversible `expand → preflight/setup → maintenance cutover → verify/cleanup` transition. Post-epoch recovery is roll-forward or catastrophic full restore, never legacy-writer rollback.~~ *Superseded by HOR-582 (section 1.1 and section 15).* |
| `DES-HOR-451-13` | Customer API access uses the complete fixed action catalogue in this record. Security/credential lifecycle is browser-only; consequence-sensitive bearer access is deferred; workload/internal and admin/debug surfaces are never customer actions. Customer model listing/chat invocation have separate actions, current per-request authority, operator-managed model/rate policy, and payload-free attributable usage. Delivery is split into HOR-514 and HOR-513. |
| `DES-HOR-451-14` | Customer Inference Gateway authorization uses indexed live reads over its existing dedicated shared-PostgreSQL connection, with no request-authority cache. For the customer-authority surface, the existing role adds `SELECT` only on the bounded credential/catalogue projections and `INSERT` only on payload-free usage events; failures deny before backend invocation. The same role retains separately approved exact object-level routing/workload reads. A future Redis authorization-cache worker is deferred and requires a separate architecture decision preserving immediate revocation semantics. |

The full approval metadata for `DES-HOR-451-14` is durable in Obsidian `HOR-451 — Direct PostgreSQL inference authority decision`: Nuno Gonçalves approved it on 2026-08-23 for the customer inference request path, with the exact transport, grants, failure/isolation consequences, deferred alternative, HOR-451 link, and PR #58 review evidence.

### 1.1 Design review amendments (2026-10-11/12)

The founder design review of 2026-10-11/12 amended this record. Each amendment's detailed contract is the "Decisions (design review)" section of the ticket named in the right-hand column; the Obsidian `Platform V2 — Product Requirements` note maps requirements to those tickets.

| Amends | Amendment | Deciding ticket |
| --- | --- | --- |
| `DES-HOR-451-03`, `DES-HOR-451-12` | **One path, no cutover.** Every installation runs V2 authority from first boot. Sign-in is always on: there is no `auth.enabled: false` and no API-key board mode, and the work UI is reached only through a browser session. Legacy scoped keys (`scope=work\|admin\|token\|gateway`), `POST /v1/token`, `/.well-known/jwks.json`, the epoch/preflight/cutover lifecycle, the legacy-credential mapping manifest, the delegated-token drain window, and the `IdentityMapping`, `PermissionPolicy` and stale `EgressRoute` CRDs are deleted, not kept inert for a compatibility release. OPO1, the only existing installation, moves through one forward database migration, not a reinstall; work, artifact and attribution history is preserved, and rollback is roll-forward or catastrophic restore (`DES-HOR-582-02`, section 15). | [HOR-582](https://linear.app/horizonshift/issue/HOR-582) |
| `DES-HOR-451-11` | **First Admin from the overlay.** The customer overlay names the first Admin's email. Bootstrap runs whenever no bootstrap marker exists, not only on an empty installation: it creates that person as a `setup_pending` Admin, or converts an existing local user with that email, and emails a one-time setup link through the normal setup channel. Everyone else, including OPO1's other existing humans, uses the access-request flow (`DES-HOR-582-03`, section 12). | [HOR-582](https://linear.app/horizonshift/issue/HOR-582) |
| `DES-HOR-451-14` | **Gateway legacy lookup removed before HOR-513.** The Inference Gateway no longer loads the legacy `identity.active_api_keys` snapshot; its customer listener denies every bearer request until HOR-513 delivers live V2 credential lookup, while workload mTLS inference is unchanged. `permissions.effective_capabilities` grants the broad workload capability only to non-human identities (`DES-HOR-582-01`, approved 2026-10-12). | [HOR-582](https://linear.app/horizonshift/issue/HOR-582) |
| `DES-HOR-451-09` | **Hosted mail sender.** Hosted installations send authentication email through Amazon SES in the hosted AWS account (EU region), with one verified sending domain for all hosted installations and credentials per environment. Self-hosted installations configure their own SMTP. The hash-only outbox contract is unchanged. | [HOR-582](https://linear.app/horizonshift/issue/HOR-582), [HOR-603](https://linear.app/horizonshift/issue/HOR-603) |
| `DES-HOR-451-04`, section 2 non-goals | **Single sign-on is V2 scope, and Iterabase keeps authority.** Microsoft Entra ID and Google sign-in (OIDC) can be enabled per installation. SSO proves who someone is; Iterabase decides their access. A first-time SSO user goes through the normal access request, and the role is chosen at approval. A person is matched once by verified email and then bound to the IdP subject. Admins keep a break-glass local password. There is no group sync, JIT role provisioning, or SCIM (section 6.6). | [HOR-314](https://linear.app/horizonshift/issue/HOR-314) |
| Section 2 non-goals | **Teams linking uses the Entra identity.** A person signed in through Entra SSO is linked to Teams by Entra object ID automatically, with no separate confirmation step. Teams requires Entra SSO on the installation. Linking never creates or widens Iterabase access (section 6.7). | [HOR-248](https://linear.app/horizonshift/issue/HOR-248), [HOR-314](https://linear.app/horizonshift/issue/HOR-314) |
| Section 2 non-goals | **Managed hosting and managed inference are V2 scope.** Hosted single-tenant installations are the main offer, and managed inference goes through the central Iterabase gateway. That gateway authenticates installations by per-installation mTLS and meters credits centrally; its contract is owned by HOR-661 and is outside this record. This record still governs the installation's own customer inference API. | [HOR-593](https://linear.app/horizonshift/issue/HOR-593), [HOR-661](https://linear.app/horizonshift/issue/HOR-661) |

## 2. Scope and non-goals

### In scope

- Local access request, verification, setup, sign-in, password reset, sign-out, browser-session inspection/revocation, and password reauthentication.
- Exactly two customer roles: `operator` and `admin`.
- Human identities, service actors, personal API keys, accountable automation credentials, action selection, rate context, suspension, rotation, revocation, and ownership history.
- Complete customer HTTP endpoint classification for control-plane and Inference Gateway.
- Current-authority and three-principal attribution contracts for direct API starts and inference.
- Identity security audit, transactional authentication email, first-Admin bootstrap/recovery, chart/configuration prerequisites, and failure semantics.
- Microsoft Entra ID and Google OIDC sign-in, IdP-subject binding, and Teams person linking through the Entra identity (HOR-314, HOR-248).
- Removal of legacy local roles, broad grants, delegated JWT, API-key purposes, the auth-disabled mode, and identity/permission CRD customer authority, plus the OPO1 forward migration (HOR-582).

### Non-goals

- Runtime, API, schema, controller, UI, chart, or gateway implementation in HOR-451.
- Group or role sync, JIT role provisioning, SCIM, SAML, and identity providers other than Entra ID and Google.
- Scheduled starts.
- The central Iterabase inference gateway's installation authentication and credit metering (HOR-661).
- Customer permission/policy builders or per-user/per-workflow resource grants.
- Customer workflow, tool, AgentPool, capability, provider, backend, routing, model-exposure, rate-policy, or runtime-credential configuration.
- New OpenAI-compatible endpoints beyond customer model listing and chat completions.
- Bearer Chat, blocker approval, revision, cancellation, restart, consequence confirmation, extraction retry, password/session/key lifecycle, bootstrap, or recovery.

## 3. Authority and trust boundaries

### 3.1 V2 authorities

| Concern | Authoritative source | Never authoritative |
| --- | --- | --- |
| Human identity, email, role, account state | PostgreSQL `identity.identities` + `identity.local_users` | Removed `IdentityMapping`/`PermissionPolicy` CRDs, IdP claims, browser claims, UI state |
| SSO identity | Immutable binding of the canonical human to the IdP subject (Entra object ID or Google `sub`) + current local-user state | IdP email after binding, IdP groups or roles, any token claim beyond issuer/tenant/subject |
| Teams person link | Entra object ID from the person's SSO binding | Teams display name or email, Bot Framework claims alone |
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
- Audit, ownership, and historical principal evidence.
- IdP subject bindings and SSO client credentials.
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
- Upgrade operator or old binary reintroducing a removed legacy path or ambiguous grant.
- IdP-authenticated user from a non-allowed tenant or domain, or without an approved access request.
- Forged Teams Bot Framework request or a Teams user without an SSO-linked person.

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
| Legacy authority reintroduced | Legacy keys, token issuance, auth-disabled mode, and the identity/permission CRDs are deleted from code and chart (HOR-582). No compatibility reader exists. A static test fails on any unclassified customer route. |
| SSO bypasses approval | SSO proves identity only. A first-time subject enters the normal access request and receives a role only at Admin approval. No group sync, JIT provisioning, or SCIM. Tenant/domain allow-list and ID-token validation (HOR-314). |
| IdP email change or reuse | Match by verified email once, then bind to the immutable IdP subject; later sign-ins use the subject, never the email (HOR-314). |
| Teams link widens access | Link only by Entra object ID from an SSO binding. Linking creates no access. Every card action rechecks current authority and the content digest; forged Bot Framework requests are rejected (HOR-248). |
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
- Login always creates a new session/token and invalidates any supplied pre-login session token. Local and SSO sign-in create the same kind of session (HOR-314).
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

### 6.6 Single sign-on (HOR-314)

- **Providers:** Microsoft Entra ID and Google, using OIDC authorization code + PKCE with ID-token validation. An Admin enables SSO and its providers per installation. The customer's Entra app or Google client is configured in Settings → Integrations (HOR-652). Sign-in is restricted to the allowed tenant or domain.
- **Authority stays in Iterabase.** A successful SSO sign-in proves identity only. A first-time subject with no bound person enters the access-request flow in section 6.1, and an Admin chooses `operator|admin` at approval (HOR-585). There is no group sync, JIT role provisioning, or SCIM.
- **Binding:** an existing person is matched once by IdP-verified email, then bound to the IdP subject (Entra object ID or Google `sub`, scoped by issuer/tenant). Later sign-ins match the subject, not the email, so an email change at the IdP does not break the binding.
- **Sessions:** SSO sign-in creates the same opaque browser session as local sign-in (section 5.2), and the same current account/role checks apply on every request.
- **Break-glass:** local password sign-in remains for Admins, and for every user on installations without SSO. With SSO on, an Admin may disable local sign-in for non-Admins.
- **Deprovisioning:** an account disabled at the IdP cannot sign in through SSO. The Iterabase person stays until an Admin disables them; there is no sync.

### 6.7 Teams person linking (HOR-248)

- A person signed in through Entra SSO is linked to the Iterabase Teams apps by their Entra object ID automatically. There is no separate confirmation step.
- Teams requires Entra SSO on the installation, and a publicly reachable installation. Otherwise Teams is shown as unavailable.
- A Teams user without a linked person receives only a "sign in to Iterabase with your Microsoft account" message.
- Linking never creates or widens Iterabase access. Every Teams action (answers, confirmations on cards that show the exact content, cancellation, revision and restart per DES-HOR-663-01, and chief-of-staff DMs) is attributed to the linked person with channel `teams`. It goes through the same blocker, confirmation, and Chat services as the app, under the person's current authority (HOR-248, HOR-659).

## 7. Target data contract

Names below are the required logical contract. Migrations may preserve existing physical tables where compatible, but implementations must not create competing authorities.

### 7.1 `identity.identities`

Retain canonical UUIDs and actor kinds. Required V2 kinds include human, service, workflow/agent, and workload/system as already represented by the repository's identity domain. Human and service actor UUIDs are stable attribution anchors.

### 7.2 `identity.local_users`

Required fields/constraints:

- `identity_id` PK/FK to canonical human identity.
- canonical normalized email, unique for non-purged human accounts.
- original validated delivery email.
- display name and `en|pt` locale, or `NULL` to follow the organisation default (HOR-670, [`v2-localization.md`](v2-localization.md)).
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

### 7.11 `identity.authority_state` (removed)

Removed by HOR-582. There is no authority epoch: V2 authority is the only path from first boot, and no binary carries a legacy-writer configuration to refuse.

### 7.12 `identity.inference_api_credentials`

A bounded read projection for the dedicated Inference Gateway database role. It exposes only what a customer request needs:

- key hash/UUID/type/status/expiry.
- immutable action membership for the two inference actions.
- owner human UUID and current owner account state and role.
- actor UUID/kind and current personal actor role where applicable.
- effective suspension reason and credential/actor rate identifiers/limits.

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
- source `chat|api|mailbox` (mailbox starts per HOR-595) and channel `browser|api|chat|teams` (`REQ-014`), optional browser session/API key, and authorization snapshot references. A mailbox start records its mailbox binding and message identity as source evidence (HOR-595).

Active assignment resolves the run; callers cannot supply trusted principal UUIDs. Historical rows are immutable evidence. Current dynamic authorization is re-evaluated at the trusted boundary.

### 7.16 IdP subject bindings (HOR-314)

Logical contract; HOR-314 chooses the physical shape:

- canonical human identity, provider `entra|google`, issuer/tenant, and immutable IdP subject (Entra object ID or Google `sub`);
- unique `(provider, issuer, subject)`, and at most one binding per provider per human;
- bound time and binding evidence (first-sign-in verified-email match or access-request approval);
- no IdP tokens, refresh tokens, or group claims.

The Entra object ID from this binding is the only Teams link key (section 6.7).

## 8. Authentication and lifecycle APIs

Exact path spelling may follow repository conventions, but endpoint ownership and authentication mode are fixed.

### Public browser journey

- Request access and generic resend/status acknowledgement.
- Verify access-request email.
- Complete first-time password setup.
- Sign in.
- SSO sign-in start and callback for each enabled provider (HOR-314).
- Request/resend/complete password reset.

These endpoints accept only their submitted credential/token, use enumeration-resistant responses, and never accept an API key as substitute authority.

### Cookie-session customer APIs

- Current profile and locale (effective locale, own preference and organisation default; see [`v2-localization.md`](v2-localization.md)).
- Admin organisation default language (`/v1/organization/settings`, HOR-670).
- Password reauthentication/change.
- Session list/current/revoke/sign-out.
- Admin verified-access-request read/review.
- Admin role/account/session-security mutations.
- Admin SSO settings: enable or disable SSO, providers, and local sign-in policy (HOR-314).
- Personal API-key and Admin automation lifecycle.
- Chat, blocker response/confirmation, extraction retry, cancellation, revision, and restart according to their owning product contracts.

Unsafe calls require CSRF. Security/lifecycle calls requiring recent password proof reject stale proof.

### Bearer API resolution

1. Require exactly one supported Authorization bearer value.
2. Hash it using the API-key domain.
3. Resolve active key, owner, actor, current account/role, actions, and expiry/suspension.
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
| `/.well-known/jwks.json`, `POST /v1/token` | Removed (HOR-582) | Not served. No drain window and no V2 customer action. |
| SSO sign-in start/callback | Public browser journey | Only enabled providers and allowed tenant/domain; creates a browser session or an access request; never accepts an API key (HOR-314). |
| Teams bot messaging endpoints (one per agent) | Bot Framework authenticated | Not a customer key action. Acts only for an SSO-linked person, with channel `teams`, through the same blocker/confirmation/Chat services (HOR-248, HOR-659). |
| Workflow catalogue/summary/detail/process reads | Personal + automation | `workflows.read`; customer-safe projection only. |
| `POST /v1/work-items` | Personal + automation | `workflows.start`; mandatory idempotency and three-principal attribution. |
| Work list/detail/dashboard/attempt/node/timeline/consequence/blocker/feedback/link metadata and work-event SSE | Personal + automation | `work.read`. Value fields additionally require current Admin + `value.read`. Automation receives Operator-safe projection. |
| `POST /v1/work-items/{id}/feedback` | Personal only | `work.feedback.write`; saving starts no work. |
| Blocker response/confirmation | Cookie session, or a Teams card from an SSO-linked person (HOR-248); bearer deferred | Current body can confirm consequential invocation IDs. No V2 key action. |
| Revision/restart | Cookie session, or a Teams card from an SSO-linked person (DES-HOR-663-01); bearer deferred | Consequence-safe confirmation owned by HOR-463. No V2 key action. |
| Cancellation | Cookie session, or a Teams card from an SSO-linked person (DES-HOR-663-01); bearer deferred | Consequence-safe confirmation owned by HOR-464. No V2 key action. |
| Artifact list/search/recent/status/metadata/get/head/download | Personal + automation | `artifacts.read`; unavailable/quarantined bytes remain denied. |
| Artifact upload | Personal + automation | `artifacts.upload`; security/size/type policy applies. |
| Artifact delete | Personal current Admin only | Both `artifacts.read` and `artifacts.delete`; active-reference block/tombstone/audit; no UI, Operator, or automation. |
| Extraction results/status | Personal + automation | `artifacts.read`. |
| Extraction retry | Cookie session only | No V2 bearer processing-control action. |
| People/access-request reads | Personal current Admin only | `people.read`; bounded projection. |
| Access approve/decline, role/account/session-security mutations | Cookie + recent auth only | Bearer always denied. |
| Profile/password/session/key/automation lifecycle | Cookie (+ recent auth where specified) only | Bearer always denied. |
| Chat conversation/message/stream/tool/confirmation/memory | Cookie session, or a Teams DM from an SSO-linked person (HOR-659) | No V2 Chat bearer action. |
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

Rate counters live in the control-plane PostgreSQL (`ratelimit.counters`); Redis was removed from the platform by `DES-HOR-661-07`. Per `DES-HOR-451-14`, rate counters are not customer authorization authority: credential and catalogue state are read live from PostgreSQL on every customer request, and failed reads or accepted-event inserts deny before backend invocation. A future Redis authorization-cache worker is explicitly deferred; adopting one requires a separate architecture decision and proof that revocation, disablement, demotion, expiry, and suspension still take effect on the next request. Any other datastore/transport/isolation change likewise requires separate approval.

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
- SSO sign-in succeeded/refused (tenant, domain, or unapproved), IdP subject bound, SSO and local sign-in policy changed (HOR-314).

Core identity audit does not duplicate work/runtime/tool/inference payloads. Domain ledgers carry domain effects with principal snapshots.

### 11.2 Atomicity

- A successful security lifecycle mutation and its core event are one transaction; audit insert failure aborts the mutation.
- Failed login/throttle evidence uses a separate transaction so failure is recordable without a successful auth mutation.
- SMTP external acceptance is represented by outbox state/events and cannot be made database-atomic; uncertainty is explicit.
- The content-free accepted usage event must append after authorization/model/rate checks and before backend invocation; failure prevents the backend call. A terminal event is appended after completion/failure when possible. Terminal-event uncertainty may not leak payload or cause an automatic inference retry.

## 12. Bootstrap and recovery

### First-Admin bootstrap (`DES-HOR-582-03`)

A locked, RBAC-less `api bootstrap` init container:

1. Reads the first-Admin email (and locale) from the customer overlay value rendered by the chart (HOR-582).
2. Requires valid exact public HTTPS origin and verified-TLS SMTP configuration (Amazon SES on hosted installations).
3. Takes a database advisory lock.
4. Runs whenever no bootstrap marker exists, whether or not local users exist (so OPO1's first boot of this release qualifies). In one transaction it creates the overlay-named person as one canonical `setup_pending` Admin or, if a local user with that email already exists, converts that user to a `setup_pending` Admin (preserving their canonical UUID and history, clearing any old password, and revoking their sessions and owned keys); then it queues normal setup email, records the marker, and audits. Other existing users are not touched and use the access-request flow.
5. Never creates or prints an Admin API key or raw setup token.
6. Is a strict no-op once the marker exists; it never re-sends or re-converts on restart. A missing or invalid overlay email, or a marker without the Admin it recorded, fails closed.

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
- verified-TLS SMTP host/port/mode/from address and Secret-backed credentials: Amazon SES SMTP in the hosted AWS account for hosted installations, the customer's SMTP for self-hosted ones (HOR-582, HOR-603).
- first-Admin email/locale from the customer overlay for a fresh install; no synthetic default.
- SSO enablement and allowed tenant/domain; the IdP client credentials live in a Settings → Integrations credential slot (HOR-314, HOR-652).
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
| OPO1 forward migration fails | The migration transaction rolls back and OPO1 stays on its prior release until a corrected migration rolls forward; there is no reinstall path (`DES-HOR-582-02`). There is never a mixed authority (section 15). |
| IdP unavailable or ID token invalid | SSO sign-in fails closed. Admin break-glass local sign-in still works (HOR-314). |
| IdP account disabled | SSO sign-in is refused. The Iterabase person remains until an Admin disables them. |
| Teams request without an SSO-linked person or current authority | Refused; the user receives only the link-your-account message (HOR-248). |

## 15. Legacy removal and the OPO1 forward migration

The 2026-08-23 expand → preflight → token drain → maintenance cutover → verify/cleanup plan, and its one-release inert CRD compatibility, are superseded by HOR-582. The only existing installation is OPO1 (internal), so V2 removes the legacy authority outright.

### 15.1 Removed

- Legacy scoped keys (`scope=work|admin|token|gateway`) and the `identity.active_api_keys` customer lookup.
- `POST /v1/token`, `/.well-known/jwks.json`, and delegated-token validation.
- The epoch/preflight/cutover code and `identity.authority_state` (section 7.11).
- The auth-disabled mode and the API-key board mode.
- `POST|GET /v1/users*`, `/v1/api-keys*`, `GET /v1/permissions/identities/{id}`, and `POST /v1/value-models` on the customer surface.
- The `IdentityMapping`, `PermissionPolicy`, and stale `EgressRoute` CRDs, types, and reconcilers, from the chart and the manager in the same release.
- The broad `*/*` active-user behavior.

### 15.2 OPO1

OPO1 moves through one forward database migration, not a reinstall (`DES-HOR-582-02`, approved 2026-10-12):

- Preserve canonical UUIDs. Map role `admin→admin`, `user→operator`. Existing humans complete email setup rather than receiving invented passwords.
- Revoke every legacy key. Automation is reissued through HOR-514 with an active Admin owner and a distinct service actor.
- Replace the gateway's legacy schema-wide/default reads and `identity.active_api_keys` lookup with the exact grant union in section 7.12, in the same migration.
- Keep work, artifact, and attribution history readable.

The migration is rehearsed against a copy of OPO1's database before it runs.

### 15.3 Rollback boundary

- Before the migration commits: the prior release and database remain as they were.
- After it commits: roll forward, or catastrophic restore from backup (`DES-HOR-582-02`). There is no reinstall path, because it would lose work, artifact and attribution history. No binary restarts a legacy writer, wildcard grant, delegated issuance, or CRD authority.

### 15.4 Implementation (HOR-582)

- Migration `000031_v2_only_authority` is the OPO1 forward migration, following HOR-597's `000030_backend_auth_ref`. It also drops the legacy `scope` and `credential_epoch` columns, makes a live untyped key unrepresentable (`api_keys_v2_complete`), drops `identity.external_mappings` and `permissions.policies` (keeping an empty typed `permissions.effective_rate_limits`), and limits the broad capability to workload identities. `TestV2OnlyAuthorityMigrationUpgradesALegacyInstallation` proves it on an OPO1-shaped legacy database.
- Every control-plane route is registered through exactly one authority class: `probe`, `public`, `session` (cookie only, optionally current Admin and recent password), `action` (browser session or V2 credential holding the route's single action), or `bearer_action` (V2 credential only, e.g. Admin artifact deletion). A browser session holds the personal actions of the account's current role. `TestEveryRouteIsClassified` fails on an unclassified or reclassified route, a wildcard, or an unknown action.
- The chart renders no auth-disabled mode and fails closed without the public origin, verified-TLS SMTP (`auth.email.provider: smtp|ses`, optional `auth.email.caSecret` for a private relay CA), and the first-Admin email.
- First-Admin bootstrap is keyed only on the bootstrap marker (`DES-HOR-582-03`): with no marker it makes the overlay-named human a `setup_pending` Admin and emails the setup link, whatever other accounts exist. Other migrated humans join through the access-request flow; approving a passwordless `setup_pending` account keeps its canonical identity and applies the reviewed role. Any other existing account still blocks approval (`account_exists`).
- `POST /v1/work-items/{id}/wait/stop` (HOR-649) is a `session` route with the blocker-response authority (`DES-HOR-649-06`).
- Value timeline events (`value_credited`, `value_deducted`) are Admin-only data like the item and dashboard value fields: the timeline and work-event stream omit them without `value.read` and keep cursors resumable.
- Objects of the deleted CRDs keep their old reconciler finalizers; the operations runbook ([`control-plane/docs/browser-auth-operations.md`](../../control-plane/docs/browser-auth-operations.md)) releases them once on OPO1.

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
- Bootstrap keyed on the marker: empty install, existing install with no marker (create or convert the overlay-named user, other users untouched), restart no-op, and marker inconsistency; no-active-Admin recovery/last-Admin invariant.

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

### SSO and Teams linking tests

- Entra test-tenant and Google test-domain sign-in; a new user lands in an access request and an approved user gets the chosen role.
- Non-allowed tenant/domain refused; ID-token validation negatives; IdP email change keeps the subject binding.
- Admin break-glass local sign-in with SSO enabled; local sign-in disabled for Operators.
- An Entra-signed-in person is linked in Teams automatically; an unlinked Teams user or a forged Bot Framework request cannot act.
- Live portal login handoff (HOR-655) is the only operation with no Teams path; Teams links to the app for it.

### Migration/release tests

- Fresh install: no auth-disabled option, no legacy key path, no token issuance; the overlay's first Admin receives a setup link, signs in, and sees their work.
- Retired routes and removed CRDs absent from the chart and the manager.
- OPO1 forward migration against a copy of OPO1's database: history readable, legacy keys revoked, exact gateway grants.
- HOR-467 fresh install/upgrade with reference Admin/Operator, personal keys, automation actor, model filtering/rates/usage, and direct negative endpoint matrix.

## 17. Ticket ownership

| Ticket | Owns |
| --- | --- |
| HOR-451 | This approved architecture/decision contract only. |
| HOR-453 (delivered) | Access request, verification/setup/reset, local password, browser session/CSRF, recent password evidence, auth email/bootstrap packaging and journeys. |
| HOR-454 (delivered) | Role/People enforcement, browser-only identity mutations, target authority schema, key owner/actor/action substrate, inference credential projection, and exact grant union preserving routing/workload reads. |
| HOR-582 | One-path V2 authority: `requireAction` on every customer route, legacy removal (section 15.1), three principals from the authenticated boundary, overlay first Admin, SES configuration, OPO1 migration, static route-classification test. |
| HOR-585 | Admin People UI over the delivered People API. |
| HOR-314 | Entra ID and Google SSO, IdP-subject binding, SSO settings and local sign-in policy, Entra object ID for Teams linking. |
| HOR-248 / HOR-659 | Teams agents, person linking by Entra object ID, Teams card actions, and the chief-of-staff Teams DM. |
| HOR-652 | Settings → Integrations credential slots, including the SSO client configuration. |
| HOR-514 | Personal/automation Settings and browser-only credential lifecycle, presets, one-time reveal, rotate/revoke/transfer/suspension UI/API. |
| HOR-513 | Inference Gateway exact actions, `DES-HOR-451-14` live PostgreSQL lookup/bounded customer grants/no authority cache, model filter, mandatory rates, usage ledger, retained routing/workload regression, listener isolation, charts/tests. |
| HOR-452/HOR-456 | Cookie-session Chat contract/implementation (plus the HOR-659 Teams DM surface); no bearer Chat action. |
| HOR-455 | Confirmed Chat effects plus personal/automation `workflows.start` handoff/attribution. |
| HOR-459 | Routed shell, Settings slots, workflow/work/value role-safe projections. |
| HOR-461/HOR-462 | Artifact read/upload/delete and status/browser-retry boundaries. |
| HOR-463/HOR-464 | Consequence-safe restart/revision/cancellation from the browser session or Teams cards (DES-HOR-663-01); bearer remains denied. |
| HOR-467 | Complete shared release matrix and immutable evidence. |

## 18. Production and release classification

HOR-451 and HOR-663 are design-only and have no runtime/data impact. Follow-on implementation changes authentication/authorization authority, customer credentials, SSO, gateway database access, rate behavior, usage evidence, and the OPO1 forward migration. Their semantic publication is deferred to the founder-approved `release.yml` flow in `docs/release.md`; merge alone is not a release.
