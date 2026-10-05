# AWS CI fixture account operations

Authority: `DES-HOR-591-01` (HOR-591, approved 2026-09-27) and D1/D2 of
`Areas/ho/Delivery/Fast Validation Pipeline — Engineering Plan.md`. The GitHub
workflows this runbook sets up are `.github/workflows/aws-ci-smoke.yml` and
`.github/workflows/aws-ci-reaper.yml`; the contract and its enforcement live in
`.github/scripts/aws_ci.py`.

The `iterabase-ci` account is a disposable CI substrate created once by hand. It
holds no customer data and no product artifact. There is no infrastructure as
code here: the resources are few, long-lived, and applied once, and the smoke
workflow is the executable proof that the account is still correct.

## Boundary

| Surface | Contract |
| --- | --- |
| Credentials | GitHub OIDC only, no static keys anywhere, no GitHub environment. Trust covers `aud=sts.amazonaws.com` and `sub=repo:nunocgoncalves@64640406/iterabase-mono@1330311216:*` — the immutable subject form GitHub issues for repositories created after 2026-07-15, which pins the owner and repository IDs so a rename or a recreated repository cannot inherit trust; fork pull requests cannot request OIDC tokens. |
| Launch | `RunInstances` only for `m6i.xlarge` (CPU) and `g5.xlarge` (GPU), only from AMIs owned by the CI account, and only with no instance profile — each enforced twice, as an allow condition and as an explicit deny. The mandatory tags are enforced at tag-on-create (`ec2:CreateTags` requires the marker and run tags through `aws:RequestTag`), because the launch action's own authorization context carries no request-tag keys. |
| Tags | `iterabase-ci=true` (marker, mandatory), `iterabase-ci-run=<github run id>` (mandatory), `iterabase-ci-scenario=<identity>` (mandatory), optional `iterabase-ci-deadline=<RFC3339 UTC>`, `Name=iterabase-ci-<run>-<scenario>`. The marker is the IAM condition key and the reaper's only selection criterion. |
| Lifecycle | Terminate, volume, snapshot, and AMI actions are scoped to resources carrying `iterabase-ci=true`; tag-on-create is bound to the creating action. |
| Reaper | `iterabase-ci-deadline` when present, otherwise `LaunchTime + 180 minutes`. Untagged or foreign instances are reported and never touched; the role cannot terminate them. |
| Unreachable by the role | `iam:*`, `organizations:*`, `s3:*`, `ssm:*`, and `sts:AssumeRole` (no `iam:PassRole`, so no instance profile can ever be attached). |
| Region | Three allowed regions in preference order: `eu-west-1` (primary: default VPC, bootstrap AMI, CPU fixtures, cheapest `g5`), `eu-central-1`, `eu-north-1`. A GPU fixture walks regions, then approved types, then offered AZs, because `eu-west-1` offers `g5` without live capacity and does not offer the `g6`/L4 family at all. The policy's ARNs carry a wildcard region (listing three regions would exceed the 6144-character managed-policy limit), so the region set is a service contract, and cost misuse in another region is bounded by the budget alarm. |
| Budget | `$250/month` cost budget `iterabase-ci-monthly` on the member account, forecast alert at 80% and actual alert at 100%. The budget measures gross usage cost (`IncludeCredit=false`), so startup credits cannot hide runaway spend. |

`Describe*` actions cannot be resource-scoped by AWS design; the policy grants
them account-wide, read-only. The cost boundary is the budget alarm, not a hard
cap. CI runs as CI-only code in a dedicated account, which is the isolation
boundary that matters.

## Recorded identities

Fill this table while executing. HOR-591 acceptance requires it: complete the
values in this file on the ticket branch and record the validation run ids and
observed outcomes on the ticket. Do not record secret material here.

| Value | Command that produces it | Recorded |
| --- | --- | --- |
| Management account ID | `aws sts get-caller-identity --query Account --output text` (management) | `712493797687` |
| CI account ID (`iterabase-ci`) | `aws sts get-caller-identity --query Account --output text` (member) | `024378233802` |
| Identity Center portal | Identity Center console → Settings → AWS access portal URL | `https://ssoins-72239e45c8505198.portal.us-east-1.app.aws` |
| Identity Center primary Region | Identity Center console → Settings → Region (single-Region home Region; record what you chose) | `us-east-1` (single-Region) |
| Identity Center admin role ARN | `aws sts get-caller-identity --query Arn --output text` in a `iterabase-ci` portal session | `arn:aws:sts::024378233802:assumed-role/AWSReservedSSO_AdministratorAccess_79e2306e90704f7b/nuno` |
| Root MFA and root access keys | `aws iam get-account-summary --query 'SummaryMap.{RootMFAEnabled:AccountMFAEnabled,RootAccessKeysPresent:AccountAccessKeysPresent,RootPasswordPresent:AccountPasswordPresent}'` (1 = yes, 0 = no; console is authoritative) | RootMFAEnabled `1`, RootAccessKeysPresent `0` |
| OIDC provider ARN | `aws iam list-open-id-connect-providers --query 'OpenIDConnectProviderList[].Arn' --output text` | `arn:aws:iam::024378233802:oidc-provider/token.actions.githubusercontent.com` |
| CI role ARN | `arn:aws:iam::<CI account ID>:role/iterabase-ci-role` | `arn:aws:iam::024378233802:role/iterabase-ci-role` (one attached policy, no inline policy) |
| CI policy ARN | `aws iam list-policies --scope Local --query "Policies[?PolicyName=='iterabase-ci-role-policy'].Arn" --output text` | `arn:aws:iam::024378233802:policy/iterabase-ci-role-policy` (default version `v3`, identical to the committed renderer) |
| Security group IDs | `aws ec2 describe-security-groups --filters Name=group-name,Values=iterabase-ci-ssh --query 'SecurityGroups[0].GroupId' --output text`, per region | `eu-west-1`: `sg-0c1c48f5483c349bb`; `eu-central-1`: *pending*; `eu-north-1`: *pending* (same `iterabase-ci-ssh` rule in each region's default VPC) |
| Default VPC IDs | `aws ec2 describe-vpcs --filters Name=isDefault,Values=true --query 'Vpcs[0].VpcId' --output text`, per region | `eu-west-1`: `vpc-0cedec86f4a3d5022` (IGW `igw-05e32f993a7978aab`, subnets `subnet-05087e89ffe206577`/1a, `subnet-09132b1b93f433ee2`/1b, `subnet-00be394acf0f860c9`/1c); `eu-central-1`: `vpc-0cd6e9809118b596d`; `eu-north-1`: `vpc-07688f0d38e7944a9` |
| G/VT quota | `aws service-quotas get-service-quota --service-code ec2 --quota-code L-DB2E81BA --query 'Quota.Value' --output text` | `0.0` -> `16.0` **approved** 2026-10-05 (request `f1b9e4046ac34aec8be6679c1c3f85d324nx64fO`, case `179053976300776`, resolved `CASE_CLOSED` after the appeal): four concurrent `g5.xlarge` hosts |
| G/VT quota `eu-central-1` / `eu-north-1` | same command with `--region` | *pending*: requested `0` -> `16` for both, so a GPU fixture can land where capacity exists |
| Standard quota `eu-central-1` / `eu-north-1` | same command with `--region` | *pending*: requested `0` -> `32` for both, so a relocating fixture is never CPU-starved |
| Standard quota | `aws service-quotas get-service-quota --service-code ec2 --quota-code L-1216C47A --query 'Quota.Value' --output text` | `5.0` -> `32.0` **approved** 2026-09-27 (request `fe65c092e8c8414b81e2b927ce38b50cpJbBVGkh`, case `179053976300908`; the request status stayed `CASE_CLOSED`) |
| AZs offering `g5.xlarge` | step 13 below | `eu-west-1a`, `eu-west-1b`, `eu-west-1c` (offered, but live probes found no capacity in any of them) |
| AZs offering `g6.xlarge` (L4) | step 13 below | not offered in `eu-west-1`; offered in `eu-central-1` and `eu-north-1` |
| AZs offering `m6i.xlarge` | step 13 below | `eu-west-1a`, `eu-west-1b`, `eu-west-1c` |
| Budget name | `iterabase-ci-monthly` | `iterabase-ci-monthly` — $250/month, forecast >80%, actual >100%, subscribers `nuno+ci@iterabase.com` (email) and the SNS topic below |
| SNS alerts topic | `aws sns list-topics --region us-east-1 --query 'Topics[].TopicArn' --output text` | `arn:aws:sns:us-east-1:024378233802:iterabase-ci-budget-alerts` (display name `iterabase-ci budget`; Budgets publish allowed by the topic policy; `nuno+ci@iterabase.com` confirmed as `arn:aws:sns:us-east-1:024378233802:iterabase-ci-budget-alerts:96796240-e89e-4cb7-bdbe-78fde1fa8493`; delivery proven 2026-09-27 by test message `5e578566-6a21-546c-86dc-e655f25b0d4d`: delivered 1, failed 0) |

## Part 1 — Account setup (console, founder)

Everything in this part is done once in the management account (except the
budget and quotas, which apply to the member account).

Before you start, know which account and identity your shell is using:

```bash
aws sts get-caller-identity --query '{Account:Account,Arn:Arn}' --output table
aws organizations describe-organization \
  --query 'Organization.{ManagementAccountId:MasterAccountId,FeatureSet:FeatureSet}'
```

- The ARN says which identity you are: `arn:aws:iam::<id>:root` is the root
  user, `arn:aws:iam::<id>:user/<name>` is an IAM user, and
  `arn:aws:sts::<id>:assumed-role/AWSReservedSSO_.../<name>` is an Identity
  Center role.
- The management account is the account that created the organization; it is the
  same account as the root sign-in, and the root user is an identity inside it,
  not a separate account. Compare `Account` with `ManagementAccountId`.
- Organization administration commands — `organizations list-accounts`,
  `describe-organization`, `create-account` — succeed only in the management
  account. A member account gets `AccessDeniedException`, and an account without
  an organization gets `AWSOrganizationsNotInUseException`.
- Sign in as root at <https://signin.aws.amazon.com/console> (*Root user* → the
  account's email address, then MFA). Root is required for organization
  creation and is the reliable identity for billing pages until Identity Center
  is enabled in step 5; use the access portal for everything else from then on.
- If the CLI itself is authenticated as root — a CloudShell session opened as
  root, root access keys, or a cached `aws login` console session — do not run
  the resource steps with it. None of those create an IAM access key, so
  `RootAccessKeysPresent` stays `0`, but the session still carries root
  authority until it expires. Use it for the checks below and for root-only
  tasks, move to the Identity Center admin session (Part 2) for everything else,
  and clear the cached root session with `aws logout` when you are done. Check
  that cached session with `aws sts get-caller-identity --profile default`, not
  with a bare call: a bare call follows `AWS_PROFILE`, so once you have exported
  the admin profile it answers as the admin. The default profile returns the root
  ARN while the cache is live and fails once it is gone.
  `aws logout --profile default` clears only that session; `--all` also clears the
  Identity Center tokens, which then need `aws sso login` again. A process that
  already loaded the access token can keep using it for its remaining lifetime
  (up to 15 minutes); the CLI refreshes from the cache, which is now empty.

1. **Secure root.** Sign in as root, open *Account* → *Security credentials*, and
   confirm: MFA is assigned, there are **no** root access keys, and root is used
   only for billing, account creation, and quota/limit changes that require it.
   The console page is authoritative; the account summary is the scriptable
   check, and its fields describe the root user, not your current IAM principal:
   ```bash
   aws iam get-account-summary --output json | jq '.SummaryMap | {RootMFAEnabled: .AccountMFAEnabled, RootAccessKeysPresent: .AccountAccessKeysPresent, RootPasswordPresent: .AccountPasswordPresent}'
   # expect RootMFAEnabled 1 and RootAccessKeysPresent 0
   ```
2. **Create the organization.**
   ```bash
   aws organizations create-organization --feature-set ALL
   aws organizations describe-organization --query 'Organization.{Id:Id,FeatureSet:FeatureSet}'
   ```
   Or *Organizations* → *Create an organization* → *All features*.
3. **Create the `iterabase-ci` member account.** Use an email address that only
   this account will ever use. Account creation is asynchronous and takes a few
   minutes:
   ```bash
   aws organizations create-account --email <ci-account-email> --account-name iterabase-ci \
     --query 'CreateAccountStatus.{RequestId:Id,State:State}'

   # Poll until State is SUCCEEDED; FAILED reports the reason.
   aws organizations describe-create-account-status --create-account-request-id <request-id> \
     --query 'CreateAccountStatus.{State:State,AccountId:AccountId,FailureReason:FailureReason}'

   aws organizations list-accounts \
     --query 'Accounts[].{Id:Id,Name:Name,Status:Status,Email:Email}' --output table
   ```
   `list-accounts` always includes the management account; the member account
   appears once creation succeeds. JMESPath is case-sensitive, so the query keys
   must match the API response exactly (`Accounts`, `Id`, `Name`, `Status`,
   `Email`); a lowercase query silently prints nothing. The created account
   carries `OrganizationAccountAccessRole`; Identity Center replaces it for
   day-to-day use.
4. **Check that the startup credits apply to the consolidated bill.** In the
   management account open *Billing and Cost Management* → *Credits* and confirm
   the startup credit is listed and not expired. Because the member account's
   usage rolls into the management account's consolidated bill, the credit must
   appear there. Record what the console shows. Billing pages are reachable as
   root immediately; if an Identity Center `AdministratorAccess` role cannot open
   them later, enable *Account settings* → *IAM user and role access to Billing
   information*.
5. **Enable IAM Identity Center** (in the management account): *IAM Identity
   Center* → *Enable*. Choose an **organization instance**, not an account
   instance: only an organization instance can assign access across the
   organization, and an account instance cannot be converted. Then:
   - **Region and configuration.** Use the **Single-Region** configuration. The
     primary Region cannot be changed after creation, so choose it deliberately:
     it is independent of the `eu-west-1` fixture region, because IAM and STS are
     global and the EC2 resources stay in `eu-west-1` regardless. It does become
     the `sso_region` of any CLI profile you configure, so record it. Multi-Region
     replication is not used: it requires a customer managed multi-Region KMS key
     (AWS KMS charges apply), and it only improves access resilience and regional
     application deployment for the AWS access portal. CI never touches Identity
     Center — GitHub Actions authenticates through the OIDC provider and STS only
     — so portal resilience is not on the pipeline's critical path, and one
     founder uses one directory. Additional Regions can still be added later from
     *Settings*, at the cost of introducing that KMS key then.
   - *Users* → *Add user*: the founder's admin user with an email address.
   - *Permission sets* → *Create permission set* → *Predefined permission set* →
     `AdministratorAccess`, session duration 8 hours.
   - *AWS accounts* → select `iterabase-ci` (and the management account) →
     *Assign users or groups* → assign the founder user with that permission set.
   - Record the primary Region and the AWS access portal URL from *Settings* in the
     table above. The portal URL is instance-wide, not per user: every Identity
     Center user signs in at the same URL with their own username. Copy exactly
     what the console *Settings* page shows; it is either
     `https://<directory-id>.awsapps.com/start` or the current
     `https://ssoins-….portal.<region>.app.aws` form. Verify the instance, the
     user, and the assignment from the CLI in the Identity Center home Region
     (root or any read-capable principal is fine):
     ```bash
     aws sso-admin list-instances --region "$IDC_REGION" \
       --query 'Instances[].{InstanceArn:InstanceArn,IdentityStoreId:IdentityStoreId,Name:Name,PrimaryRegion:PrimaryRegion}' \
       --output table
     ID_STORE=$(aws sso-admin list-instances --region "$IDC_REGION" --query 'Instances[0].IdentityStoreId' --output text)
     INSTANCE_ARN=$(aws sso-admin list-instances --region "$IDC_REGION" --query 'Instances[0].InstanceArn' --output text)
     aws identitystore list-users --identity-store-id "$ID_STORE" --region "$IDC_REGION" \
       --query 'Users[].{Id:UserId,UserName:UserName,Email:Emails[0].Value}' --output table
     aws sso-admin list-permission-sets --instance-arn "$INSTANCE_ARN" --region "$IDC_REGION" \
       --query 'PermissionSets[]' --output text
     aws sso-admin list-account-assignments --instance-arn "$INSTANCE_ARN" \
       --account-id "$CI_ACCOUNT_ID" --permission-set-arn "$PERMISSION_SET_ARN" --region "$IDC_REGION" \
       --query 'AccountAssignments[].{Principal:PrincipalId,Type:PrincipalType}' --output table
     ```
     An *account instance* supports none of the multi-account commands above; if
     `list-instances` is empty or the assignment is missing, the instance was
     enabled as an account instance and must be replaced.
   From here on, use the access portal for everything below. Never root.
6. **Set the budget alarm on `iterabase-ci`** while signed in to that member
   account (Identity Center access portal → `iterabase-ci`). Budgets are global,
   so the region does not matter.
   ```bash
   export CI_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

   # Explicit recurring period: starts this month, no end date. Without it the
   # coverage window depends on API defaults.
   MONTH_START=$(date -u +%Y-%m-01T00:00:00Z)
   cat > /tmp/iterabase-ci-budget.json <<JSON
   {
     "BudgetName": "iterabase-ci-monthly",
     "BudgetLimit": {"Amount": "250", "Unit": "USD"},
     "TimeUnit": "MONTHLY",
     "TimePeriod": {"Start": "${MONTH_START}"},
     "BudgetType": "COST",
     "CostTypes": {"IncludeCredit": false, "IncludeRefund": false, "UseBlended": false}
   }
   JSON

   cat > /tmp/iterabase-ci-notifications.json <<'JSON'
   [
     {
       "Notification": {
         "NotificationType": "FORECASTED",
         "ComparisonOperator": "GREATER_THAN",
         "Threshold": 80,
         "ThresholdType": "PERCENTAGE"
       },
       "Subscribers": [{"SubscriptionType": "EMAIL", "Address": "<founder-email>"}]
     },
     {
       "Notification": {
         "NotificationType": "ACTUAL",
         "ComparisonOperator": "GREATER_THAN",
         "Threshold": 100,
         "ThresholdType": "PERCENTAGE"
       },
       "Subscribers": [{"SubscriptionType": "EMAIL", "Address": "<founder-email>"}]
     }
   ]
   JSON

   aws budgets create-budget \
     --account-id "$CI_ACCOUNT_ID" \
     --budget file:///tmp/iterabase-ci-budget.json \
     --notifications-with-subscribers file:///tmp/iterabase-ci-notifications.json

   aws budgets describe-budget --account-id "$CI_ACCOUNT_ID" --budget-name iterabase-ci-monthly \
     --query 'Budget.{Name:BudgetName,Limit:BudgetLimit,Type:BudgetType,Unit:TimeUnit}'
   aws budgets describe-notifications-for-budget --account-id "$CI_ACCOUNT_ID" \
     --budget-name iterabase-ci-monthly \
     --query 'Notifications[].{Type:NotificationType,Threshold:Threshold,Operator:ComparisonOperator,State:NotificationState}' --output table
   ```
   `describe-notifications-for-budget` does not echo `ThresholdType`, so the
   percentages below appear without a type even though each notification was
   created with `"ThresholdType": "PERCENTAGE"`; the console shows them as
   percentages. `IncludeCredit=false` keeps the alarm honest: it fires on gross
   usage cost, so credits cannot hide a runaway. Both notifications must be
   listed after that command with `State: OK`, and both email subscribers must
   **confirm their subscription**: AWS sends an *AWS Notification - Subscription
   Confirmation* email to each address, and until it is confirmed the 80%/100%
   alerts are never delivered. The Budgets console shows the subscriber as
   pending until then. Do not look for the subscription in the account's SNS
   console: for an email subscriber added through Budgets, no topic or
   subscription appears in the account (verified: zero topics and zero
   subscriptions in both `us-east-1` and `eu-west-1`). To force a fresh
   confirmation email, delete and re-create the **notification**, never just its
   last subscriber: `delete-subscriber` removes the whole notification once it
   has no subscribers left (verified — both notifications silently disappeared),
   which leaves the budget without alerts. Alert routing is the founder's
   decision too: never add an address the founder has not named — a
   plausible-looking address from a git author or a chat profile is not consent.
   Use the exact address the founder confirmed they read:
   ```bash
   aws budgets delete-notification --account-id "$CI_ACCOUNT_ID" --budget-name iterabase-ci-monthly \
     --notification '{"NotificationType":"FORECASTED","ComparisonOperator":"GREATER_THAN","Threshold":80,"ThresholdType":"PERCENTAGE"}'
   aws budgets create-notification --account-id "$CI_ACCOUNT_ID" --budget-name iterabase-ci-monthly \
     --notification '{"NotificationType":"FORECASTED","ComparisonOperator":"GREATER_THAN","Threshold":80,"ThresholdType":"PERCENTAGE"}' \
     --subscriber 'SubscriptionType=EMAIL,Address=<address>'
   ```
   Re-run `describe-notifications-for-budget` after any subscriber change: two
   notifications with `State: OK` must still be listed. Verify the subscribers
   with:
   ```bash
   aws budgets describe-subscribers-for-notification --account-id "$CI_ACCOUNT_ID" \
     --budget-name iterabase-ci-monthly \
     --notification '{"NotificationType":"FORECASTED","ComparisonOperator":"GREATER_THAN","Threshold":80,"ThresholdType":"PERCENTAGE"}' \
     --output table
   ```
   Then prove the path end-to-end instead of assuming it — configuration alone is
   not evidence:
   ```bash
   aws sns publish --topic-arn "$TOPIC_ARN" --region us-east-1 \
     --subject "iterabase-ci alert path test" --message "alert path test"
   aws cloudwatch get-metric-statistics --namespace AWS/SNS --metric-name NumberOfNotificationsDelivered \
     --dimensions Name=TopicName,Value=iterabase-ci-budget-alerts --region us-east-1 \
     --start-time "$(python3 -c 'import datetime;print((datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(minutes=15)).strftime("%Y-%m-%dT%H:%M:%SZ"))')" \
     --end-time "$(python3 -c 'import datetime;print(datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))')" \
     --period 60 --statistics Sum --query 'Datapoints[].Sum' --output text
   ```
   A delivered count above zero with `NumberOfNotificationsFailed` at zero, plus the
   test message arriving in the mailbox, is the acceptance evidence for the alarm
   being active **and** verified.
   If the confirmation email never arrives, stop deleting subscribers and use a
   customer-owned topic instead. Budgets' own email subscription is managed
   service-side (nothing shows in the account's SNS console), so a topic is the
   only channel whose subscription you can see and resend. The topic must live in
   the same account as the budget (no cross-account), and it must let
   `budgets.amazonaws.com` publish with `aws:SourceAccount`/`aws:SourceArn`
   conditions; otherwise Budgets reports "AWS Budgets doesn't have access to the
   SNS topic". One SNS subscriber may sit alongside up to ten email subscribers
   per notification:
   ```bash
   TOPIC_ARN=$(aws sns create-topic --name iterabase-ci-budget-alerts --region us-east-1 \
     --query 'TopicArn' --output text)
   aws sns subscribe --topic-arn "$TOPIC_ARN" --protocol email \
     --notification-endpoint '<address>' --region us-east-1        # then confirm that email
   aws budgets create-subscriber --account-id "$CI_ACCOUNT_ID" --budget-name iterabase-ci-monthly \
     --notification '{"NotificationType":"FORECASTED","ComparisonOperator":"GREATER_THAN","Threshold":80,"ThresholdType":"PERCENTAGE"}' \
     --subscriber "SubscriptionType=SNS,Address=$TOPIC_ARN"
   ```
   Re-subscribing an address re-sends its confirmation email, which is the API's
   only equivalent of the console's *Request confirmation*. An unwanted pending
   subscription cannot be removed — `unsubscribe` rejects the
   `PendingConfirmation` placeholder ARN ("an ARN must have at least 6 elements")
   — but it is inert: it delivers nothing until confirmed and expires on its own
   after a few days.
   A confirmation email that never arrives is a provider or a sender problem,
   and guessing between them is expensive. Work it in this order:

   1. Search the mailbox itself, with a control. `in:anywhere
      from:no-reply-aws@amazon.com` must find the support mail that demonstrably
      arrived; then `in:anywhere from:sns.amazonaws.com` decides whether the
      confirmation was merely filed outside the inbox (Spam, Trash, All Mail) or
      never arrived. If the control search finds nothing either, the mailbox
      being searched is not the mailbox that receives AWS mail — fix that before
      anything else. On HOR-591 this was the answer: both confirmation emails were
      sitting in Spam, and no admin log ever showed them. After confirming, mark
      the sender as not spam **and** add it to the Workspace *Email allowlist*, or
      the 80%/100% alerts themselves will land in Spam and be missed.
   2. Do **not** rely on the provider's *Email log search* to settle it. On
      HOR-591 it returned nothing even for support mail that had arrived, so an
      empty result is inconclusive and proves neither provider-side dropping nor
      AWS-side failure.
   3. If the message was accepted somewhere but never reaches the recipient, try
      the same mailbox without its plus-alias: plus-addressing is the most common
      innocent difference between an address that receives AWS mail and one that
      does not. Then add the sender to the Workspace *Email allowlist* (Apps →
      Google Workspace → Gmail → Spam, Phishing and Malware) and check
      *Compliance* → blocked senders and any routing rule for a drop.
   4. If nothing distinguishes the two sides, stop re-subscribing: the channel is
      the fix, not the subscription. Move the alert channel (AWS Chatbot to
      Slack/Teams needs an OAuth click, not an email confirmation) and treat the
      email subscriber as best-effort. A budget action (for example an IAM-deny
      policy applied at 100% actual) needs no notification channel at all and is
      the fallback when the alarm must be verifiably effective rather than
      verifiably delivered.

   SNS documents no email suppression list and exposes no API for one (the CLI
   only has SMS opt-out), so there is nothing on the AWS side to inspect or lift.
   That is also why the budget's own email subscriber can be green but deaf:
   `describe-notifications-for-budget` reports `NotificationState: OK` and lists
   the address whether or not anyone ever confirmed the subscription, and there is
   no API field for the confirmation state. A topic you own is observable by
   design — subscription status, request-confirmation, publish/delivery metrics —
   so treat the topic as the primary channel and the budget's email subscriber as
   a convenience. Budgets allows one SNS subscriber alongside up to ten email
   subscribers per notification.
7. **Request the quota increases in every allowed region** (member account; quota
   requests are per region, and a new region starts at `0` vCPU for both families, so
   the fallback regions need their own requests before they can host anything). Confirm the quota codes first, then request and verify. A
   brand-new account starts at `0` vCPU for G/VT and `5` vCPU for Standard, so
   both requests are real increases. AWS decides these cases asynchronously: the
   request moves from `PENDING` to `CASE_OPENED` with a support case id and the
   quota keeps its current value until that case is resolved. Track it with
   `get-requested-service-quota-change`, and follow the case through the AWS
   emails and the Support Center, since this account has Basic support: the
   Support API answers `SubscriptionRequiredException`, so cases cannot be read
   or replied to from the CLI. Two traps when reading the result: the API is
   eventually consistent (a granted increase showed a stale pre-increase value
   in later reads, so read it repeatedly a few minutes apart before concluding),
   and a granted increase left the request status at `CASE_CLOSED` instead of
   `APPROVED` — the quota value is the authority, not the request status. When
   AWS asks for a use case, the true one is that the GPU host is a CI fixture
   running one `g5.xlarge` at a time. A refusal is usually an invitation to
   appeal: the text says *reopen this case and provide as detailed a use case as
   possible*. Reply on the same case thread with the text below, and ask for
   exactly one GPU host (`4` vCPU) rather than a ceiling — Basic Support covers
   service quota increases, so the case route is in scope on that plan. Keep the
   escalation order: (1) appeal with the detailed use case and a `4` vCPU ask;
   (2) if it is refused again, ask the startup-program contact or move to
   Business Support+ (from about $29/month per account); (3) retry once the
   account has real usage and a paid invoice, since the automated checks weigh
   account history. Do not work around it by moving fixtures to another Region
   (the `0` vCPU start value applies to every Region) or into the management
   account (that breaks the account boundary in `DES-HOR-591-01`).

   ```text
   Thank you for the review. We would like to appeal this decision and provide
   the detailed use case you asked for.

   Account: 024378233802 ("iterabase-ci"), a dedicated member account of our AWS
   Organization. It is a CI sandbox: no customer data, no production traffic, no
   human users — only GitHub Actions workflows.

   Workload: automated end-to-end validation of our Kubernetes platform
   installer before each merge. The GPU leg boots a g5.xlarge (A10G, 4 vCPU,
   16 GiB) in eu-west-1, runs the GPU stack for about 20 minutes, then shuts
   down. The GPU part is not optional: the inference stack we validate (vLLM with
   a bf16 model, FlashAttention-2) needs sm_86, and the smaller g4dn (T4) is
   sm_75 and cannot run the validated stack.

   Why we are asking for 16 vCPU (four concurrent hosts):
   - The GPU work arrives from three sources that can coincide: pull-request
     validation, the merge-queue run of the exact merge commit, and the nightly
     full validation plus the nightly GPU image bake. Each takes its own fresh
     instance by design, and they must not queue behind each other.
   - Queueing is the problem we are fixing. Today every GPU job serializes behind
     a single permanent host: the median end-to-end validation is 90 minutes, and
     roughly half of that is waiting rather than running. Our target is 10-20
     minutes.
   - Headroom also covers failure. A capacity error or a failed host has to be
     replaced while the previous instance is still draining. With a single-host
     quota, one failure blocks the whole GPU pipeline until that instance
     terminates.

   Requested quota: 16 vCPU (four g5.xlarge). If 16 cannot be granted, we would
   rather start at 4 vCPU — exactly one host — than wait, and we will ask again
   once the account has usage history. The quota is a ceiling, not projected
   spend: our estimated GPU usage is about 36 USD/month at the current run
   frequency, whatever the ceiling is.

   Billing-limit safeguards already in this account:
   - Instances launch with "terminate on instance-initiated shutdown", and every
     fixture shuts itself down when its run ends.
   - Each host runs an on-host shutdown timer at the scenario timeout plus a
     margin.
   - A scheduled GitHub Actions reaper terminates any tagged instance older than
     three hours, and the CI role may only create or terminate resources
     carrying the CI tag, so an orphan cannot survive.
   - A $250/month AWS Budget with alerts at 80% forecast and 100% actual covers
     this account, and no static credentials are used anywhere.
   - The same role is limited to two approved instance types (m6i.xlarge and
     g5.xlarge) and to CI-owned AMIs, so both the launch surface and the
     instance count are bounded by policy rather than convention.

   For reference, On-Demand Standard (L-1216C47A) in the same Region is already
   at 32 vCPU for the CPU fixtures and was approved earlier.

   Please let us know if anything else is needed to re-assess.
   ```
   ```bash
   aws service-quotas list-service-quotas --service-code ec2 --region eu-west-1 \
     --query "Quotas[?contains(QuotaName,'On-Demand')].{Code:QuotaCode,Name:QuotaName,Value:Value}" \
     --output table

   aws service-quotas get-service-quota --service-code ec2 --quota-code L-DB2E81BA --region eu-west-1 \
     --query 'Quota.{Name:QuotaName,Value:Value}'
   aws service-quotas get-service-quota --service-code ec2 --quota-code L-1216C47A --region eu-west-1 \
     --query 'Quota.{Name:QuotaName,Value:Value}'

   aws service-quotas request-service-quota-increase --service-code ec2 \
     --quota-code L-DB2E81BA --desired-value 16 --region eu-west-1
   aws service-quotas request-service-quota-increase --service-code ec2 \
     --quota-code L-1216C47A --desired-value 32 --region eu-west-1

   aws service-quotas list-requested-service-quota-change-history-by-quota --service-code ec2 \
     --quota-code L-DB2E81BA --region eu-west-1 --output table
   aws service-quotas list-requested-service-quota-change-history-by-quota --service-code ec2 \
     --quota-code L-1216C47A --region eu-west-1 --output table
   ```
   - `L-DB2E81BA` — *Running On-Demand G and VT instances*. 16 vCPUs cover four
     concurrent GPU hosts (4 vCPU each) and were granted in `eu-west-1`; the same
     request is submitted for `eu-central-1` and `eu-north-1`.
   - `L-1216C47A` — *Running On-Demand Standard (A, C, D, H, I, M, R, T, Z)
     instances*. 32 vCPUs cover eight concurrent `m6i.xlarge` hosts.
   Wait until both requests show `APPROVED` and re-read the quotas. Until then
   nothing can launch `g5.xlarge` at all (G/VT is `0` in a new account) and only
   one `m6i.xlarge` fits at a time (Standard is `5`), so the smoke workflow fails
   closed on the GPU leg rather than queueing on capacity.

## Part 2 — CLI resources

Run this part in the **member account** (`iterabase-ci`) with administrative
credentials, from a checkout of this repository at the ticket head so the
renderer is the exact committed version. Two equivalent ways to get there:

- **CloudShell (simplest).** Switch to `iterabase-ci` in the access portal, open
  CloudShell, and run the commands there — it uses the console's
  `AdministratorAccess` role and needs no local credentials at all.
- **Local CLI through Identity Center SSO.** Short-lived credentials, no static
  keys:
  ```bash
  aws configure sso --profile iterabase-ci-admin --region eu-west-1
  # prompts: SSO session name: iterabase
  #          SSO start URL: <access portal URL>
  #          SSO region: <Identity Center home Region, for example us-east-1>
  #          account: iterabase-ci   role: AdministratorAccess
  aws sso login --profile iterabase-ci-admin
  export AWS_PROFILE=iterabase-ci-admin
  aws sts get-caller-identity --query '{Account:Account,Arn:Arn}' --output table
  ```
  The profile deliberately holds two different Regions: `sso_region` is the
  Identity Center home Region and `region` is `eu-west-1` for the EC2 work. A
  mismatch between them surfaces as an SSO token error, not as a permission
  error.

```bash
export AWS_PAGER=""
# Step 6 onwards runs as the member account's Identity Center admin session,
# never as root: `aws sts get-caller-identity` must return the CI account ID.
export REPO=nunocgoncalves/iterabase-mono
export VPC_ID=$(aws ec2 describe-vpcs --region eu-west-1 --filters Name=isDefault,Values=true \
  --query 'Vpcs[0].VpcId' --output text)
export CI_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
printf 'member account %s in default VPC %s\n' "$CI_ACCOUNT_ID" "$VPC_ID"
```

8. **Confirm the default VPC is usable, in every allowed region.** The fixtures need a
   public IPv4 address, so each region's default VPC must have an internet gateway and
   at least one subnet in every AZ you intend to use (run the commands below with
   `--region eu-west-1`, `--region eu-central-1`, and `--region eu-north-1`):
   ```bash
   aws ec2 describe-internet-gateways --region eu-west-1 \
     --filters Name=attachment.vpc-id,Values="$VPC_ID" \
     --query 'InternetGateways[].InternetGatewayId' --output text
   aws ec2 describe-subnets --region eu-west-1 --filters Name=vpc-id,Values="$VPC_ID" \
     --query 'Subnets[].{Subnet:SubnetId,AZ:AvailabilityZone,PublicIP:MapPublicIpOnLaunch,Free:AvailableIpAddressCount}' \
     --output table
   ```
   Both must be non-empty. The smoke workflow passes
   `--associate-public-ip-address` explicitly, so it fails closed rather than
   launching an unreachable host.
9. **Create the GitHub OIDC identity provider.**
   ```bash
   aws iam create-open-id-connect-provider \
     --url https://token.actions.githubusercontent.com \
     --client-id-list sts.amazonaws.com \
     --tags Key=iterabase-ci,Value=true
   aws iam list-open-id-connect-providers --query 'OpenIDConnectProviderList[].Arn' --output text
   ```
   No `--thumbprint-list` is needed: AWS manages the GitHub thumbprint. If the
   provider already exists in this account, reuse it and confirm its client ID
   list contains `sts.amazonaws.com`.
10. **Create the CI role policy** from the committed renderer, never by hand:
    ```bash
    python3 .github/scripts/aws_ci.py render-policy --account-id "$CI_ACCOUNT_ID" \
      --output /tmp/iterabase-ci-policy.json
    aws iam create-policy --policy-name iterabase-ci-role-policy \
      --policy-document file:///tmp/iterabase-ci-policy.json \
      --description "DES-HOR-591-01 iterabase CI fixture boundary" \
      --tags Key=iterabase-ci,Value=true
    export POLICY_ARN=$(aws iam list-policies --scope Local \
      --query "Policies[?PolicyName=='iterabase-ci-role-policy'].Arn" --output text)
    printf '%s\n' "$POLICY_ARN"
    ```
    The renderer refuses any region other than `eu-west-1` and any account id
    that is not twelve digits. The policy is 18 statements, and its shape follows
    what EC2 actually puts in each authorization context (every step below was
    proven by decoding `UnauthorizedOperation` from real dispatches, not inferred):

    - Read-only describes; the approved launch surface; tag-scoped lifecycle
      actions; and a deny for the privilege and data surface.
    - Regions. Resource ARNs carry a wildcard region (`arn:aws:ec2:*:<account>:…`)
      because three regions are allowed and listing each would exceed the 6144-character
      managed-policy limit; the allowed set is enforced by the driver (`CI_REGIONS`) and
      covered by the budget alarm.
    - Types. The launch allow lists every approved type — `m6i.xlarge` for CPU and
      `g6.xlarge`, `g5.xlarge`, `g6.2xlarge`, `g5.2xlarge`, `g5.4xlarge` for GPU (all
      24 GiB and at least sm_86; `g4dn`/T4 is sm_75 and cannot run the validated stack).
    - Launch constraints. `ec2:InstanceType` and the absence of an instance profile
      are allow conditions on the launch statement, with `DenyUnapprovedInstanceType`
      and `DenyInstanceProfile` as explicit denies. `DenyNonCiOwnedAmi` is scoped to
      image ARNs, because an unscoped `StringNotEquals` on `ec2:Owner` denies every
      launch — the instance being created is an `aws:ResourceBeingCreated` context
      that carries `ec2:InstanceType` but neither `ec2:Owner` nor any request-tag
      key, and `StringNotEquals` is true when its key is absent.
    - Mandatory tags. They are enforced at tag-on-create, not on the creating
      action: `TagCiResourcesOnCreate` and `TagCopiedImagesOnCreate` require
      `aws:RequestTag/iterabase-ci=true` and a present `aws:RequestTag/iterabase-ci-run`
      through `ec2:CreateTags`. `aws:RequestTag` is populated for `ec2:CreateTags`
      (the bootstrap copy's tagging passes through these statements) and absent for
      `RunInstances`, so a tag condition — allow or deny — on the launch statement
      could never match.
    - `ec2:CopyImage` carries its own unconditional allow: EC2 authorizes a copy
      against the **source image and its source snapshot** — for a public Canonical
      image both are empty-account ARNs that the account-scoped patterns cannot match
      — and against destination wildcard ARNs whose context carries no request-tag
      keys. A copy always lands in the CI account, and its tags are enforced by
      `TagCopiedImagesOnCreate`, whose `ec2:CreateAction` list is `CopyImage`.
    - Copies carry the empty-account ARN form, so both forms are allowed wherever a
      copied AMI is evaluated: `RemoveCiImagesAndSnapshots` (EC2 reports the copied
      image's own ARN with an empty account segment even with its `ec2:ResourceTag`
      keys present) and `LaunchFromCiOwnedAmi` (the same form appears for the launch's
      image evaluation). The `ec2:Owner` condition keeps launches to AMIs this account
      owns, and the marker tag condition still scopes removal to CI resources.

    `RunInstances` stays restricted to CI-owned AMIs, so the launch boundary is
    unchanged.
    (`iam:*`, `organizations:*`, `s3:*`, `ssm:*`, `sts:AssumeRole`).
11. **Create the CI role** with the GitHub OIDC trust policy:
    ```bash
    python3 .github/scripts/aws_ci.py render-trust-policy --account-id "$CI_ACCOUNT_ID" \
      --output /tmp/iterabase-ci-trust.json
    aws iam create-role --role-name iterabase-ci-role \
      --assume-role-policy-document file:///tmp/iterabase-ci-trust.json \
      --description "GitHub OIDC CI role for the iterabase fixtures (DES-HOR-591-01)" \
      --max-session-duration 3600 \
      --tags Key=iterabase-ci,Value=true
    aws iam attach-role-policy --role-name iterabase-ci-role --policy-arn "$POLICY_ARN"
    export ROLE_ARN=arn:aws:iam::"$CI_ACCOUNT_ID":role/iterabase-ci-role
    aws iam list-attached-role-policies --role-name iterabase-ci-role
    aws iam list-role-policies --role-name iterabase-ci-role
    ```
    Exactly one attached policy and zero inline policies must be listed. No
    instance profile is created for this role and none may ever be created.
12. **Create the tagged CI security group** in each allowed region's default VPC
    (`--region eu-west-1`, `--region eu-central-1`, `--region eu-north-1`); the launch
    logic resolves it per region by name and marker tag. SSH is open to
    the internet by design; authentication is the per-run key and the host key is
    pinned by the runner, so there is no CIDR allowlist to maintain:
    ```bash
    aws ec2 create-security-group --region eu-west-1 --group-name iterabase-ci-ssh \
      --description "iterabase CI fixtures: per-run key SSH only (DES-HOR-591-01)" \
      --vpc-id "$VPC_ID" \
      --tag-specifications 'ResourceType=security-group,Tags=[{Key=iterabase-ci,Value=true},{Key=Name,Value=iterabase-ci-ssh}]'
    export SG_ID=$(aws ec2 describe-security-groups --region eu-west-1 \
      --filters Name=group-name,Values=iterabase-ci-ssh Name=tag:iterabase-ci,Values=true \
      --query 'SecurityGroups[0].GroupId' --output text)
    aws ec2 authorize-security-group-ingress --region eu-west-1 --group-id "$SG_ID" \
      --protocol tcp --port 22 --cidr 0.0.0.0/0
    aws ec2 describe-security-groups --region eu-west-1 --group-ids "$SG_ID" \
      --query 'SecurityGroups[0].{Id:GroupId,Ingress:IpPermissions,Vpc:VpcId,Tags:Tags}'
    ```
    The `iterabase-ci=true` tag on the group is required: the role may only
    launch with a security group carrying the marker.
13. **Record the AZ offerings** the launch logic will try, per region and in order
    (repeat with `--region` for each allowed region; `g6.xlarge` is absent from
    `eu-west-1`, which is why the search spans regions):
    ```bash
    aws ec2 describe-instance-type-offerings --region eu-west-1 --location-type availability-zone \
      --filters Name=instance-type,Values=g5.xlarge,m6i.xlarge \
      --query 'InstanceTypeOfferings[].{AZ:Location,Type:InstanceType}' --output table
    ```
    An AZ offering *both* types is the preferred target; the smoke workflow tries
    every AZ that offers the requested type and records any
    `InsufficientInstanceCapacity` it meets. Record the two lists in the table
    above.
14. **Set the repository variables** (from a machine with `gh` authenticated as
    a repository admin):
    ```bash
    gh variable set AWS_CI_ROLE_ARN --repo "$REPO" --body "$ROLE_ARN"
    gh variable set AWS_CI_REGION --repo "$REPO" --body eu-west-1
    gh variable list --repo "$REPO"
    ```
    These two variables are the complete GitHub-side configuration. No secrets
    are stored, and there is no GitHub environment.

## Part 3 — Validation

GitHub only dispatches a `workflow_dispatch` workflow when its file exists on
the default branch, so the first dispatches below happen **after** this ticket's
pull request merges to `master`. Later dispatches may target a branch ref
(`--ref HOR-591-aws-ci-substrate`).

15. **Dispatch the smoke workflow and record the run id.**
    ```bash
    gh workflow run aws-ci-smoke.yml --repo "$REPO" --ref master
    gh run list --repo "$REPO" --workflow aws-ci-smoke.yml --limit 3 \
      --json databaseId,headSha,status,conclusion,createdAt
    gh run view --repo "$REPO" --log <run-id>
    ```
    Required evidence in the run summary:
    - the assumed role ARN, account ID, and `eu-west-1`;
    - the CI-owned bootstrap AMI copied from Canonical Ubuntu 24.04;
    - one `m6i.xlarge` (**smoke cpu**) and one `g5.xlarge` (**smoke gpu**) row per
      host with a matching pinned/remote host-key fingerprint, the rejected wrong
      pinned key, a by-id device that ends in the EBS volume id, the device size,
      `shutdown behavior = terminate`, and `final state = terminated`;
    - the denied-case table with five `denied` rows and the denied action for
      each (`ec2:RunInstances`, or `iam:PassRole` for the profile case);
    - a cleanup job that removed the AMI, its snapshot, and any leftover resource
      tagged with this run id.
16. **Create the throwaway instance profile** the profile denial case
    references, then re-dispatch if step 15 ran before it existed:
    ```bash
    aws iam create-role --role-name iterabase-ci-denied-profile-role \
      --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"}]}' \
      --tags Key=iterabase-ci,Value=true
    aws iam create-instance-profile --instance-profile-name iterabase-ci-denied-profile \
      --tags Key=iterabase-ci,Value=true
    aws iam add-role-to-instance-profile --instance-profile-name iterabase-ci-denied-profile \
      --role-name iterabase-ci-denied-profile-role
    ```
    The role assumes nobody; it exists only so EC2 accepts the profile name and
    the RunInstances denial is the boundary under test. Remove it when HOR-591 is
    accepted:
    ```bash
    aws iam remove-role-from-instance-profile --instance-profile-name iterabase-ci-denied-profile \
      --role-name iterabase-ci-denied-profile-role
    aws iam delete-instance-profile --instance-profile-name iterabase-ci-denied-profile
    aws iam delete-role --role-name iterabase-ci-denied-profile-role
    ```
17. **Plant the reaper fixtures** with the founder's admin identity (the CI role
    cannot create untagged resources, which is the point of the control):
    ```bash
    export PUBLIC_AMI=$(aws ec2 describe-images --region eu-west-1 --owners 099720109477 \
      --filters "Name=name,Values=ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*" \
        Name=state,Values=available Name=architecture,Values=x86_64 \
      --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text)
    export SUBNET_ID=$(aws ec2 describe-subnets --region eu-west-1 \
      --filters Name=vpc-id,Values="$VPC_ID" --query 'Subnets[0].SubnetId' --output text)

    export FIXTURE_ID=$(aws ec2 run-instances --region eu-west-1 \
      --image-id "$PUBLIC_AMI" --instance-type m6i.xlarge --count 1 \
      --subnet-id "$SUBNET_ID" --security-group-ids "$SG_ID" \
      --associate-public-ip-address --instance-initiated-shutdown-behavior terminate \
      --tag-specifications 'ResourceType=instance,Tags=[{Key=iterabase-ci,Value=true},{Key=iterabase-ci-run,Value=reaper-validation},{Key=iterabase-ci-scenario,Value=reaper-fixture},{Key=iterabase-ci-deadline,Value=2000-01-01T00:00:00Z},{Key=Name,Value=iterabase-ci-reaper-fixture}]' \
      --query 'Instances[0].InstanceId' --output text)

    export CONTROL_ID=$(aws ec2 run-instances --region eu-west-1 \
      --image-id "$PUBLIC_AMI" --instance-type m6i.xlarge --count 1 \
      --subnet-id "$SUBNET_ID" --security-group-ids "$SG_ID" \
      --associate-public-ip-address --instance-initiated-shutdown-behavior terminate \
      --query 'Instances[0].InstanceId' --output text)
    ```
18. **Dispatch the reaper with the control instance** and record the run id:
    ```bash
    gh workflow run aws-ci-reaper.yml --repo "$REPO" --ref master \
      -f control_instance_id="$CONTROL_ID"
    gh run list --repo "$REPO" --workflow aws-ci-reaper.yml --limit 3 \
      --json databaseId,status,conclusion,createdAt
    gh run view --repo "$REPO" --log <run-id>
    ```
    Required evidence: `terminated: <FIXTURE_ID>`, the control listed under
    *foreign (untagged) left alone*, an `untagged control denial: denied
    ec2:TerminateInstances` line, and no other instance touched. Confirm with:
    ```bash
    aws ec2 describe-instances --region eu-west-1 --instance-ids "$FIXTURE_ID" "$CONTROL_ID" \
      --query 'Reservations[].Instances[].{Id:InstanceId,State:State.Name,Reason:StateReason.Code}' --output table
    aws ec2 terminate-instances --region eu-west-1 --instance-ids "$CONTROL_ID"
    ```
19. **Remove any remaining fixture resources and record the cost.**
    ```bash
    aws ec2 describe-instances --region eu-west-1 \
      --filters 'Name=tag:iterabase-ci,Values=true' 'Name=instance-state-name,Values=pending,running,stopping,stopped' \
      --query 'Reservations[].Instances[].{Id:InstanceId,Type:InstanceType,State:State.Name,Name:Tags[?Key==`Name`]|[0].Value}' \
      --output table
    aws ec2 describe-volumes --region eu-west-1 --filters 'Name=tag:iterabase-ci,Values=true' \
      --query 'Volumes[].{Id:VolumeId,State:State,Size:Size}' --output table
    aws ec2 describe-images --region eu-west-1 --owners self \
      --query 'Images[].{Id:ImageId,Name:Name,State:State}' --output table
    aws ec2 describe-snapshots --region eu-west-1 --owner-ids self \
      --query 'Snapshots[].{Id:SnapshotId,Size:VolumeSize,State:State}' --output table
    aws budgets describe-budget --account-id "$CI_ACCOUNT_ID" --budget-name iterabase-ci-monthly \
      --query 'Budget.{Limit:BudgetLimit,Actual:CalculatedSpend.ActualSpend,Forecast:CalculatedSpend.ForecastedSpend}'
    ```
    Nothing tagged `iterabase-ci=true` may remain except the security group, and
    no self-owned AMI or snapshot may remain. Record the actual and forecasted
    spend, the run ids, and the observed outcomes on the HOR-591 ticket.

## Operations

- **Reaper.** Runs hourly on `master` at minute 17. To inspect without acting:
  `gh workflow run aws-ci-reaper.yml --ref master -f dry_run=true`. To shorten
  the maximum age for a bounded cleanup, dispatch with
  `-f max_age_minutes=<minutes>`; do not use it while a legitimate run is in
  flight.
- **Runaway resource.** Terminate it with the founder's Identity Center
  credentials. The role can only touch tagged resources, and the budget alarm
  fires on gross spend, so a stuck host cannot hide.
- **Kill switch.** To stop all CI AWS activity immediately, detach the policy:
  `aws iam detach-role-policy --role-name iterabase-ci-role --policy-arn "$POLICY_ARN"`.
  Re-attach it with `attach-role-policy` when the incident is over. Instance
  cleanup during a kill switch needs the founder's admin identity.
- **Changing the boundary.** Any change to tags, instance types, or permissions
  is a policy change: update `.github/scripts/aws_ci.py`, its tests in
  `.github/scripts/test_aws_ci.py`, this runbook, and the recorded
  `DES-HOR-591-01` decision together, then re-render and re-apply the policy:
  ```bash
  python3 .github/scripts/aws_ci.py render-policy --account-id "$CI_ACCOUNT_ID" \
    --output /tmp/iterabase-ci-policy.json
  aws iam create-policy-version --policy-arn "$POLICY_ARN" \
    --policy-document file:///tmp/iterabase-ci-policy.json --set-as-default
  ```
- **What HOR-590 inherits.** The tag scheme, the rendered policy, the security
  group, the reaper, and this runbook. HOR-590 replaces the throwaway bootstrap
  AMI with Packer-built `cpu-base`/`gpu-base`/`cpu-baseline`/`gpu-baseline` AMIs,
  moves E2E execution onto these hosts, and rewrites `docs/ci.md`. It also owns
  the on-host `shutdown -h` timer at the scenario timeout plus margin: this
  ticket proves that instance-initiated shutdown terminates the instance, and
  HOR-590 installs the per-stage timer that triggers it. The smoke workflow stays
  the substrate proof.
- **Residual risks.** Instance user-data carries the per-run host private key and
  is readable by anything holding `ec2:DescribeInstanceAttribute` in the account,
  so an in-account attacker could impersonate a running fixture; user
  authentication still requires the per-run client key, and the account holds no
  product data. Host-key pinning therefore protects against network attackers and
  against a host that is not the one the runner launched, not against a
  compromised account principal. `Describe*` is account-wide read-only by AWS
  design. The budget is an alarm, not a hard spend cap.
