# Microsoft 365 mailboxes

Authority: HOR-358 and its approved decisions `DES-HOR-358-01`..`05`, recorded on
the HOR-358 ticket. The code lives in
[`control-plane/internal/mailbox`](../../control-plane/internal/mailbox) (intake,
provider-neutral store and events), [`internal/mailbox/graph`](../../control-plane/internal/mailbox/graph)
(Microsoft Graph), and the product tools `platform.mailbox.read` and
`platform.mailbox.send` in [`control-plane/tool-runner/tools/product`](../../control-plane/tool-runner/tools/product).

An installation reads, watches and sends from the customer's Microsoft 365
mailboxes through one Entra app registration. The tool gateway is the only
process that holds its credentials. Workloads get a short-lived access token
per tool call and can only name a mailbox the installation watches.

## Boundary

| Surface | Contract |
| --- | --- |
| Mailbox list | `mailbox.sources` in Postgres is authoritative. The overlay `Mailbox` CRD writes Iterabase-managed rows (`owner = overlay`); Admin-added mailboxes are installation-owned (HOR-674). A name or address can belong to only one active mailbox. |
| Credential | One credential **slot** per Entra app, for example `microsoft365`. Its values live in the installation-owned Secret `iterabase-credential-<slot>` with keys `tenant_id`, `client_id`, `client_secret`. The Admin enters them in Settings → Integrations once HOR-652 ships; until then an operator seeds the Secret. Values are never shown, logged or stored in Postgres. |
| Intake | The gateway runs delta catch-up for every watched folder at the mailbox's `pollInterval` (default 60 s). Mail received before the mailbox was first watched is history and never becomes an event. Each new message is recorded once in `mailbox.messages`, deduplicated by its immutable Graph id. |
| Notifications | With `spec.notifications.url` set, the gateway also keeps a Graph `created` subscription per folder: created, renewed a day before expiry and recreated when Graph loses it. The receiver is HOR-356. Without a URL the mailbox is polling-only, which is the self-hosted default; no tunnel is needed. |
| Tools | `platform.mailbox.read` (read-only: message, attachment list, one attachment) and `platform.mailbox.send` (non-idempotent write: one email or reply). The gateway allows only an active mailbox whose slot is the one the tool's binding resolves. |
| Sends | Draft, then send (`DES-HOR-358-05`). The result carries `internetMessageId` and `conversationId`, known before the send. A failed send never retries; an uncertain one returns `outcome_unknown` with the draft identity. Confirmation and pre-approval are HOR-599. |
| Failures | A credential or permission failure is recorded per slot in `mailbox.credential_status` with a customer-safe reason (HOR-652 shows it as "needs attention") and retried after 5 minutes. Throttling honours `Retry-After`. |

## Customer onboarding

The customer's Microsoft 365 administrator does steps 1–4. Steps 5–7 are
Iterabase steps.

1. **Register the app.** In Entra admin center → App registrations → New
   registration: name `Iterabase`, single tenant, no redirect URI. Record the
   **Directory (tenant) ID** and **Application (client) ID**.
2. **Create a client secret.** Certificates & secrets → New client secret, 24
   months. Copy the value once; it goes only into Settings → Integrations.
3. **Do not grant Mail permissions in Entra.** Graph application permissions
   granted in Entra apply to every mailbox in the tenant, and they add to
   Exchange scoping rather than being narrowed by it. Leave API permissions
   without any `Mail.*` entry.
4. **Scope the app to the bound mailboxes with RBAC for Applications.** In
   Exchange Online PowerShell (`Connect-ExchangeOnline`):

   ```powershell
   # Once per tenant: custom management scopes need organization customization.
   # On a new tenant it can take a while to apply; retry the scope if it fails.
   Enable-OrganizationCustomization

   # One mail-enabled security group holds every mailbox Iterabase may use.
   New-DistributionGroup -Name "Iterabase mailboxes" -Type Security
   Add-DistributionGroupMember -Identity "Iterabase mailboxes" -Member quotes@customer.pt
   $group = (Get-DistributionGroup "Iterabase mailboxes").DistinguishedName
   New-ManagementScope -Name "Iterabase mailboxes" -RecipientRestrictionFilter "MemberOfGroup -eq '$group'"

   # The Exchange pointer to the Entra app (Enterprise applications → Object ID).
   New-ServicePrincipal -AppId <client-id> -ObjectId <enterprise-app-object-id> -DisplayName "Iterabase"
   New-ManagementRoleAssignment -App <client-id> -Role "Application Mail.ReadWrite" -CustomResourceScope "Iterabase mailboxes"
   New-ManagementRoleAssignment -App <client-id> -Role "Application Mail.Send" -CustomResourceScope "Iterabase mailboxes"

   # Proof: InScope is True for a bound mailbox and False for any other.
   Test-ServicePrincipalAuthorization -Identity <client-id> -Resource quotes@customer.pt
   Test-ServicePrincipalAuthorization -Identity <client-id> -Resource ceo@customer.pt
   ```

   Run each command on one line; PowerShell does not continue lines with `\`.
   On a newly customized tenant, `New-ManagementRoleAssignment` can fail with
   "You don't have access … delegating role assignment" for a while even for
   a Global Administrator, after scopes and service principals already work.
   Wait (hours, not minutes), reconnect and retry before changing anything.

   `Mail.ReadWrite` is needed because sends create a draft first
   (`DES-HOR-358-05`). Role assignments can take up to an hour to apply. To add a
   mailbox later, add it to the group; nothing in Entra changes.
5. **Enter the credential.** Settings → Integrations → Microsoft 365 (HOR-652).
   Until that ships, an operator seeds the Secret in the control-plane
   namespace:

   ```bash
   kubectl -n <namespace> create secret generic iterabase-credential-microsoft365 \
     --from-literal=tenant_id=<tenant-id> --from-literal=client_id=<client-id> \
     --from-file=client_secret=/dev/stdin
   ```

6. **Declare the mailbox in the overlay** (Iterabase-managed):

   ```yaml
   apiVersion: platform.iterabase.com/v1alpha1
   kind: Mailbox
   metadata:
     name: quotes
   spec:
     provider: microsoft365
     address: quotes@customer.pt
     credentialSlot: microsoft365
     folders: [inbox]
     # Hosted installs only, once HOR-356 is live:
     # notifications:
     #   url: https://<environment>.iterabase.app/hooks/mailbox/graph
   ```

   `kubectl get mailboxes` shows `READY`; `status.message` names a conflict or
   an invalid field.
7. **Bind the tools to the pool** that needs them. The binding must reference
   the slot's Secret: that is how the gateway ties the tool to the slot.

   ```yaml
   gatewayGrants:
     - {tool: platform.mailbox.read, maxEffectClass: read_only}
     - {tool: platform.mailbox.send, maxEffectClass: non_idempotent_write}
   credentialBindings:
     - toolName: platform.mailbox.read
       slot: microsoft365
       scheme: oauth_client_credentials
       oauth:
         clientId: <client-id>
         clientSecretRef: {name: iterabase-credential-microsoft365, key: client_secret}
         tokenUrl: https://login.microsoftonline.com/<tenant-id>/oauth2/v2.0/token
         scopes: [https://graph.microsoft.com/.default]
     # The same binding again for platform.mailbox.send.
   ```

The gateway reaches `graph.microsoft.com` and `login.microsoftonline.com`
directly; allow that egress where the installation restricts it.

## Test tenant and live CI (`DES-HOR-358-04`)

The `m365-live` job in `ci.yml` runs `make -C control-plane m365-live-test`
against the dedicated test tenant whenever `control-plane/internal/mailbox/**`
or a `tools/product/mailbox-*` tool changes, and in every full validation. It
proves: delta records a new message exactly once, polling-only intake picks it
up within the interval, the tenant and the gateway both deny the unbound
mailbox, a send's identifiers match the reply from another account, and no
token or secret reaches logs, results or the database. Notification delivery is
proven by HOR-356.

No secret is stored in GitHub: Entra trusts the job's GitHub OIDC token.

The founder provisions, once:

1. **Two shared mailboxes** in the test tenant (shared mailboxes need no
   licence): a bound mailbox, for example `ib-live-bound@<tenant>`, and another
   mailbox, for example `ib-live-other@<tenant>`.
2. **The test app** `iterabase-ci-mailbox`, set up as in steps 1, 3 and 4 above
   with its group containing only the bound mailbox. No client secret.
3. **The replier app** `iterabase-ci-replier`, the same way, with its own group
   and scope containing only the other mailbox. It plays the external sender.
4. **A federated credential on each app**: Certificates & secrets → Federated
   credentials → Add credential → **Other issuer**, issuer
   `https://token.actions.githubusercontent.com`, explicit subject identifier
   `repo:iterabase@338844113/iterabase-mono@1330311216:environment:m365-test`,
   audience `api://AzureADTokenExchange`, name `github-m365-test`. This
   repository issues GitHub's immutable OIDC subject (owner and repository ids),
   so the "GitHub Actions" scenario's `repo:iterabase/iterabase-mono:…` subject
   does not match; check the current prefix with
   `gh api repos/iterabase/iterabase-mono/actions/oidc/customization/sub`.
5. **The `m365-test` GitHub environment** with no required reviewers and
   deployment branches set to all branches (pull requests and the merge queue
   use it). Environment **variables**, not secrets:

   | Variable | Value |
   | --- | --- |
   | `M365_TEST_TENANT_ID` | tenant ID |
   | `M365_TEST_CLIENT_ID` | `iterabase-ci-mailbox` client ID |
   | `M365_TEST_REPLIER_CLIENT_ID` | `iterabase-ci-replier` client ID |
   | `M365_TEST_MAILBOX` | bound mailbox address |
   | `M365_TEST_OTHER_MAILBOX` | other mailbox address |

6. **Prove it**: `Test-ServicePrincipalAuthorization` shows each app in scope
   only for its own mailbox, then run the `m365-live` job.

A local run uses the client-secret product path instead of federation. Give
each app a short-lived secret, export the variables above plus
`M365_TEST_CLIENT_SECRET` and `M365_TEST_REPLIER_CLIENT_SECRET`, run
`make -C control-plane m365-live-test` (needs Docker), then delete the secrets.

Each run tags its messages with a unique `IB-LIVE-…` marker and deletes them
afterwards. Runs share one concurrency group, so they never overlap on the
tenant. A Microsoft outage or slow delivery (over 3 minutes) fails the job;
that is the accepted cost of a real-provider gate.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `status.ready` false with "already used by another mailbox" | Another active mailbox, possibly installation-owned, holds the name or address. |
| Slot status "The Microsoft 365 credentials are not set" | The Secret `iterabase-credential-<slot>` or one of its three keys is missing in the gateway's namespace. |
| "Microsoft 365 rejected the credentials" | Wrong tenant ID, client ID or secret, or the secret expired. |
| "Microsoft 365 denied access to …" | The mailbox is not in the app's RBAC for Applications scope, or the assignment has not applied yet (up to an hour). |
| Tool returns `permission_denied` naming the mailbox | It is not an active mailbox, or the tool's binding references a different Secret than the mailbox's slot. |
| Gateway logs "provider call failed … op=subscribe" | The notification URL did not answer Graph's validation request. Delta catch-up continues, so no mail is lost. |
