# Hosted AWS account operations

Authority: HOR-603 and its approved decisions `DES-HOR-603-01`..`09`, recorded on
the HOR-603 ticket. The Terraform lives in [`infra/hosted-aws/`](../../infra/hosted-aws)
and its invariants in [`infra/AGENTS.md`](../../infra/AGENTS.md). The CI account
is separate and documented in [`aws-ci.md`](aws-ci.md).

`iterabase-hosted` is the AWS account that runs hosted customer environments.
This runbook creates it, applies the shared baseline, and proves every
acceptance item. Per-customer environments are created later by the Forge AWS
provider (HOR-593), which reads the baseline only through its SSM contract.

## Boundary

| Surface | Contract |
| --- | --- |
| Account | A member account `iterabase-hosted` in the Organization created for CI (HOR-591), separate from `iterabase-ci`. Region `eu-west-1` only (`DES-HOR-603-04`). |
| Who applies | The founder, locally, with an IAM Identity Center admin session (`DES-HOR-603-02`). CI only runs `make infra-check` and holds no credentials for this account. |
| State | `iterabase-hosted-tfstate-<account-id>`, created by `bootstrap/` (local state). The baseline uses key `baseline/terraform.tfstate`; Forge uses `envs/<name>/`. S3-native locking (`use_lockfile`). |
| Consumer contract | SSM parameters under `/iterabase/hosted/baseline/v1/*` (`DES-HOR-603-03`). See [Contract keys](#contract-keys). |
| Forge role | `iterabase-hosted-forge`: tagged-only EC2 lifecycle in the shared subnets (allowed instance types, IMDSv2, Canonical or own AMIs by `aws:ResourceAccount`), `envs/*` state, backup-prefix teardown, node roles only under `/iterabase/env/` with the boundary (`DES-HOR-603-08`), SSH and port-forwarding sessions to tagged instances. |
| Support role | `iterabase-hosted-support`: interactive Session Manager shells (logged) and break-glass SSH over SSM to tagged instances. |
| Session audit | Interactive shells: content to `iterabase-hosted-session-logs-<account-id>` (`sessions/`) and `/iterabase/hosted/ssm-sessions`, 365 days. SSH and port forwarding: CloudTrail `StartSession`/`TerminateSession` events in `iterabase-hosted-audit-<account-id>` (`cloudtrail/`) and `/iterabase/hosted/cloudtrail`, 365 days. AWS does not log SSH session content (`DES-HOR-603-07`). |
| Network | `iterabase-hosted-shared` VPC `10.40.0.0/16`, one public `/20` per AZ tagged `kubernetes.io/role/elb=1`, S3 gateway endpoint, default security group with no rules. No inbound SSH anywhere; SSM uses the instances' internet egress, so no SSM interface endpoints. |
| Backups | `iterabase-hosted-backups-<account-id>`, prefix `envs/<name>/`, SSE-KMS with `alias/iterabase-hosted-backup`. Only administrators, the Forge role and `/iterabase/env/` node roles reach the bucket and key. |
| Image mirror | ECR pull-through cache prefix `ghcr` for `ghcr.io` (`DES-HOR-603-05`), 30 most recent images per repository. |
| Security | Multi-region CloudTrail with log-file validation, AWS Config recording every supported resource, GuardDuty with severity ≥ 7 findings to the alert topic. |
| Cost | `iterabase-hosted-monthly` gross cost budget (credits excluded), forecast alert at 80% and actual alert at 100% to the `iterabase-hosted-alerts` SNS topic. Resource tag keys `iterabase-hosted` and `iterabase-env` are unchanged; their Billing activation belongs to the Organization management account, not hosted Terraform (`DES-HOR-603-09`). |
| DNS and mail | Cloudflare zone (`PIVOT-ARCH-003`), SES identity for the mail domain with Easy DKIM, `bounce.<mail-domain>` MAIL FROM (SPF) and DMARC. A scoped Cloudflare token (DNS Write + Zone Read on the zone only) for external-dns and cert-manager, published as a SecureString in the contract. |

## Recorded identities

Fill this in while executing and copy the completed table to the HOR-603 ticket.

| Item | Value |
| --- | --- |
| Hosted account ID | |
| Identity Center admin permission set / role ARN pattern | |
| Support plan | |
| State bucket | |
| Forge role ARN | |
| Support role ARN | |
| Node boundary ARN | |
| ECR pull-through prefix | |
| SES identity / production access case ID | |
| Cloudflare zone ID / cluster token name | |
| Budget name / SNS subscription confirmed | |
| Management account / caller / dated proof that both cost-allocation keys are Active | |

## Part 1 — Console and out-of-band setup (founder)

1. **Create the account.** In the Organization's management account: AWS
   Organizations → Add an AWS account → Create, name `iterabase-hosted`. Record
   the account ID.
2. **Identity Center access.** Assign the founder's admin user the
   `AdministratorAccess` permission set on `iterabase-hosted`. Terraform and
   hosted-account validation run as that session (`aws sts get-caller-identity`
   must show the hosted account and an `AWSReservedSSO_AdministratorAccess_*`
   role), never as root. [Billing tag activation](#cost-allocation-tags--management-account-founder)
   is the explicit management-account exception, with its own verified identity
   and separate founder approval.
3. **Support plan.** Billing and Cost Management → Support plans: subscribe the
   plan budgeted in the financial model's AWS sheet (shared lines). Record it.
4. **Quotas** in `eu-west-1`: request "Running On-Demand Standard (A, C, D, H,
   I, M, R, T, Z) instances" (`L-1216C47A`) sized for the trial plan. Record the
   request IDs.
5. **GHCR credential** for the pull-through cache. Create a GitHub fine-grained
   or classic token with `read:packages` only, then:

   ```bash
   aws secretsmanager create-secret --region eu-west-1 \
     --name ecr-pullthroughcache/ghcr \
     --secret-string '{"username":"<github-user>","accessToken":"<token>"}'
   ```

   The value never enters Terraform state; the baseline only reads the
   secret's ARN.
6. **Cloudflare.** Make sure the hosted zone exists in the Cloudflare account
   (add it and move the registrar's name servers if not). Create a founder
   token for the apply with *Zone → DNS → Edit* on that zone and *Account →
   Account API Tokens → Edit*; export it as `CLOUDFLARE_API_TOKEN` for the
   apply only. Record the account ID and zone ID.

## Part 2 — Terraform (founder)

Use the pinned Terraform (`ci_tools` in `.github/inputs/remote-content.json`).

```bash
cd infra/hosted-aws/bootstrap
terraform init
terraform apply                 # creates iterabase-hosted-tfstate-<account-id>
terraform output state_bucket   # keep bootstrap/terraform.tfstate safe; it is not in Git

cd ../baseline
cp terraform.tfvars.example terraform.tfvars   # fill in; ignored by Git
terraform init -backend-config="bucket=iterabase-hosted-tfstate-<account-id>"
terraform plan -out baseline.tfplan
terraform apply baseline.tfplan
```

If any of these already exist in the account, import them instead of letting
the apply fail:

| Resource | Import |
| --- | --- |
| Session Manager preferences | `terraform import aws_ssm_document.session_preferences SSM-SessionManagerRunShell` |
| Config service-linked role | `terraform import aws_iam_service_linked_role.config arn:aws:iam::<account-id>:role/aws-service-role/config.amazonaws.com/AWSServiceRoleForConfig` |
| GuardDuty detector | `terraform import aws_guardduty_detector.hosted <detector-id>` |

After the first apply:

1. Confirm the SNS email subscription sent to `alert_email`.
2. **Leave the SES sandbox.** SES → Account dashboard → Request production
   access: transactional mail, the mail domain, bounce and complaint handling
   through the MAIL FROM domain. Record the case ID.
3. **Cost allocation tags.** Follow the separately approved
   [management-account procedure](#cost-allocation-tags--management-account-founder)
   below. No hosted-account apply activates these keys.
4. Revoke or narrow the founder Cloudflare token if it was created only for
   the apply.

### Cost allocation tags — management account (founder)

`DES-HOR-603-09` separates **resource tagging** from **Billing activation**.
Terraform keeps tagging baseline resources `iterabase-hosted=baseline`; HOR-593
keeps tagging customer resources `iterabase-env=<environment>`. Only the
Organization management/payer account can manage cost-allocation activation for
member accounts; [AWS documents this ownership](https://docs.aws.amazon.com/awsaccountbilling/latest/aboutv2/cost-alloc-tags.html).
Hosted administrator access cannot change it. Do not widen hosted IAM, add a
cross-account Terraform provider or silently switch mutation accounts.

1. Obtain separate founder approval for management-account activation. Source
   PR approval or a hosted saved-plan approval does not authorize this action.
2. Sign in through Identity Center to the **Organization management account**,
   not `iterabase-hosted` or `iterabase-ci`, and verify its identity against AWS
   Organizations. Use an explicit management-account profile for the optional
   read-only CLI checks below; never use root or pass these credentials to CI.
3. Billing and Cost Management → Cost allocation tags → User-defined cost
   allocation tags: find the exact keys `iterabase-hosted` and `iterabase-env`.
   Keys can take up to 24 hours to appear after the corresponding tagged
   resources exist. If a key is missing, leave this gate pending; do not launch
   an unapproved fixture, repeat a hosted apply or assume it is active.
4. Leave already-Active keys unchanged. Select only inactive keys from this
   pair and choose **Activate**. Activation is payer-wide, not a change to tag
   values or a replacement for the hosted account's budget. Do not deactivate
   other keys. [AWS's activation procedure](https://docs.aws.amazon.com/awsaccountbilling/latest/aboutv2/activating-tags.html)
   describes the discovery and activation delays; allow up to another 24 hours
   for activation to take effect rather than blindly resubmitting it.
5. Verify **both** exact keys are **Active** in the management account. Record
   account ID, caller, date and console/API evidence on HOR-603. Successful
   resource tagging, a hosted `AccessDenied` or static/mock checks are not
   activation proof. Until both keys are Active, acceptance remains pending.

Optional read-only verification, never run with the hosted profile:

```bash
aws sts get-caller-identity --profile <management-profile>
aws ce list-cost-allocation-tags --profile <management-profile> --region us-east-1 \
  --type UserDefined --tag-keys iterabase-hosted iterabase-env \
  --query 'CostAllocationTags[].{Key:TagKey,Status:Status}'
```

Cost Explorer's Billing API is queried in `us-east-1`; hosted infrastructure
remains in `eu-west-1`. The recorded management identity must match the payer,
and the readback must contain both keys, each with `Status=Active`.

**Upgrade safety:** remove the obsolete `activate_cost_allocation_tags` input
from private tfvars/environment overrides before the next baseline plan. The
hosted activation resource and toggle have been removed; no new hosted apply
is needed for Billing activation. If an older state contains
`aws_ce_cost_allocation_tag.hosted` instances, stop before applying a deletion:
that could deactivate payer-wide keys. Arrange a separately approved state-only
ownership handoff without changing remote activation, then require a reviewed
plan with no cost-tag deactivation. Never use `terraform destroy` to transfer
this ownership.

## Contract keys

`/iterabase/hosted/baseline/v1/<key>` — `region`, `account-id`, `vpc-id`,
`subnet-ids` (StringList), `env-tag-key`, `allowed-instance-types`
(StringList), `state-bucket`, `state-prefix`, `backup-bucket`, `backup-prefix`,
`backup-kms-key-arn`, `forge-role-arn`, `support-role-arn`, `node-role-path`,
`node-permissions-boundary-arn`, `session-log-bucket`, `session-log-group`,
`audit-log-group`, `image-registry-prefix`, `mail-domain`, `mail-identity-arn`,
`cloudflare-zone-id`, and `cloudflare-dns-token` (SecureString).

## Part 3 — Validation

Record each result on HOR-603. `A=<account-id>`, region `eu-west-1`.

### 1. Forge role and a throwaway environment

```bash
eval "$(aws configure export-credentials --format env)"   # admin session
# Public parameters are outside the Forge role's contract path (DES-HOR-603-03),
# so resolve the image while still the administrator.
ami=$(aws ssm get-parameter --name /aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id --query Parameter.Value --output text)
creds=$(aws sts assume-role --role-arn arn:aws:iam::$A:role/iterabase-hosted-forge \
  --role-session-name hor-603-smoke --query Credentials --output json)
export AWS_ACCESS_KEY_ID=$(jq -r .AccessKeyId <<<"$creds") \
  AWS_SECRET_ACCESS_KEY=$(jq -r .SecretAccessKey <<<"$creds") \
  AWS_SESSION_TOKEN=$(jq -r .SessionToken <<<"$creds")

aws ssm get-parameters-by-path --path /iterabase/hosted/baseline/v1 --with-decryption \
  --query 'Parameters[].Name'                                  # every contract key
aws s3api put-object --bucket iterabase-hosted-tfstate-$A --key envs/hor-603-smoke/probe --body /dev/null  # allowed
aws s3api put-object --bucket iterabase-hosted-tfstate-$A --key baseline/probe --body /dev/null           # AccessDenied
```

Throwaway environment, still as the Forge role (also proves `DES-HOR-603-08`):

```bash
boundary=$(aws ssm get-parameter --name /iterabase/hosted/baseline/v1/node-permissions-boundary-arn --query Parameter.Value --output text)
aws iam create-role --path /iterabase/env/ --role-name hor-603-smoke --permissions-boundary "$boundary" \
  --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
aws iam create-role --path /iterabase/env/ --role-name hor-603-no-boundary \
  --assume-role-policy-document '{"Version":"2012-10-17","Statement":[]}'      # AccessDenied
aws iam attach-role-policy --role-name hor-603-smoke --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
# Session transcripts and image pulls; the boundary caps both.
log_bucket=$(aws ssm get-parameter --name /iterabase/hosted/baseline/v1/session-log-bucket --query Parameter.Value --output text)
log_group=$(aws ssm get-parameter --name /iterabase/hosted/baseline/v1/session-log-group --query Parameter.Value --output text)
aws iam put-role-policy --role-name hor-603-smoke --policy-name smoke-node --policy-document "$(jq -n \
  --arg b "arn:aws:s3:::$log_bucket" --arg g "arn:aws:logs:eu-west-1:$A:log-group:$log_group" '{Version:"2012-10-17",Statement:[
    {Effect:"Allow",Action:["s3:PutObject"],Resource:"\($b)/sessions/*"},
    {Effect:"Allow",Action:["s3:GetEncryptionConfiguration"],Resource:$b},
    {Effect:"Allow",Action:["logs:CreateLogStream","logs:PutLogEvents","logs:DescribeLogStreams"],Resource:["\($g)","\($g):*"]},
    {Effect:"Allow",Action:["logs:DescribeLogGroups"],Resource:"*"},
    {Effect:"Allow",Action:["ecr:GetAuthorizationToken","ecr:BatchGetImage","ecr:GetDownloadUrlForLayer","ecr:BatchCheckLayerAvailability","ecr:BatchImportUpstreamImage","ecr:CreateRepository"],Resource:"*"}]}')"
aws iam create-instance-profile --path /iterabase/env/ --instance-profile-name hor-603-smoke
aws iam add-role-to-instance-profile --instance-profile-name hor-603-smoke --role-name hor-603-smoke

# Use the full ARN for the non-root-path instance profile.
profile_arn=$(aws iam get-instance-profile --instance-profile-name hor-603-smoke --query InstanceProfile.Arn --output text)
subnet=$(aws ssm get-parameter --name /iterabase/hosted/baseline/v1/subnet-ids --query Parameter.Value --output text | cut -d, -f1)
vpc=$(aws ssm get-parameter --name /iterabase/hosted/baseline/v1/vpc-id --query Parameter.Value --output text)
sg=$(aws ec2 create-security-group --group-name hor-603-smoke --description "no inbound" --vpc-id "$vpc" \
  --tag-specifications 'ResourceType=security-group,Tags=[{Key=iterabase-env,Value=hor-603-smoke}]' --query GroupId --output text)
aws ec2 run-instances --image-id "$ami" --instance-type m7i-flex.large --subnet-id "$subnet" \
  --security-group-ids "$sg" --iam-instance-profile Arn="$profile_arn" --metadata-options HttpTokens=required \
  --user-data file://smoke-user-data.yaml \
  --tag-specifications 'ResourceType=instance,Tags=[{Key=iterabase-env,Value=hor-603-smoke}]' \
                       'ResourceType=volume,Tags=[{Key=iterabase-env,Value=hor-603-smoke}]'
```

Before the real launch, use `--dry-run` with the **same** launch arguments.
Authorized requests return the expected **nonzero** `DryRunOperation` error;
`UnauthorizedOperation` is a failed preflight, not a reason to launch as admin.
IAM instance profiles can take time to propagate: retry an invalid-profile
parameter error for up to two minutes after creation, then stop if it persists.
Do not retry an authorization failure by widening the live policy.

A trusted-image authorization failure can be decoded as the administrator with
`aws sts decode-authorization-message --encoded-message <message>`. The image
condition uses [`aws:ResourceAccount`](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_condition-keys.html#condition-keys-resourceaccount)
to allow only Canonical (`099720109477`) and the hosted account. `ec2:Owner`
classifies public Canonical images as `amazon`, so comparing it to numeric
account IDs denies valid launches; allowing `amazon` instead would broaden the
approved trust boundary.

`smoke-user-data.yaml` adds your SSH public key to the `ubuntu` user. Also
record that an untagged `run-instances` and an `m7i.4xlarge` launch are denied.
`aws ec2 describe-security-groups --group-ids "$sg"` must show no ingress.

### 2. Support sessions and their audit

As the support role (assume `iterabase-hosted-support` the same way):

```bash
aws ssm start-session --target <instance-id>        # logged shell; run `id; hostname; exit`
ssh -o ProxyCommand='aws ssm start-session --target %h --document-name AWS-StartSSHSession --parameters portNumber=%p' \
  ubuntu@<instance-id> 'hostname'                    # break-glass SSH over SSM
```

Evidence, as the admin session:

```bash
aws s3 ls s3://iterabase-hosted-session-logs-$A/sessions/            # the shell transcript
aws logs filter-log-events --log-group-name /iterabase/hosted/ssm-sessions --filter-pattern hostname
aws logs filter-log-events --log-group-name /iterabase/hosted/cloudtrail \
  --filter-pattern '{ $.eventSource = "ssm.amazonaws.com" && ($.eventName = "StartSession" || $.eventName = "TerminateSession") }'
aws s3 ls s3://iterabase-hosted-audit-$A/cloudtrail/AWSLogs/$A/CloudTrail/eu-west-1/ --recursive | tail
```

The shell transcript appears in S3 and CloudWatch; the SSH session appears as
`StartSession` with `documentName=AWS-StartSSHSession` in both CloudTrail
destinations.

For both Forge and support, also keep a harmless SSH session open (`sleep 120`)
and explicitly terminate it **as the same caller**, using its session ID:

```bash
aws ssm terminate-session --session-id <own-live-session-id>
```

Match that successful `TerminateSession` and its `StartSession` by session ID in
**both** CloudTrail destinations. A normal SSH disconnect can close the session
without a `TerminateSession` API call; an attempt against an already-ended
session returning `ValidationException` is not an authorization pass. As a
different role, and as a different role-session name in the same role, trying to
terminate or resume the still-live session must return `AccessDenied`. Test
callers with prefix-colliding names too (for example `operator` and
`operator-other`); no wildcard identity match is permitted.

[SSM's AWS-supplied system owner tag](https://docs.aws.amazon.com/systems-manager/latest/userguide/getting-started-restrict-access-examples.html#restrict-access-example-user-sessions)
`aws:ssmmessages:session-id` contains the full assumed-role identity
`<role-id>:<role-session-name>`, matching `aws:userid`. The role policies require
an exact `StringEquals` match through
`ssm:resourceTag/aws:ssmmessages:session-id`, on session resources in this account
and region. Missing tags fail closed. Do not infer ownership from the displayed
session ARN's role-session-name prefix: it omits the role ID. The system tag is
supplied by AWS, not a custom tag callers choose; no new STS tag or source-identity
contract is needed. This check applies to terminate/resume permissions, not the
separate data-channel grants.

An authorization failure blocks the smoke: clean up the exact test environment
and correct the policy through a reviewed PR and founder-approved apply. Never
use administrator termination as acceptance evidence; it is cleanup only.

### 3. Budget, GuardDuty, Config

```bash
aws budgets describe-notifications-for-budget --account-id $A --budget-name iterabase-hosted-monthly
aws sns publish --topic-arn arn:aws:sns:eu-west-1:$A:iterabase-hosted-alerts --message "HOR-603 delivery proof"  # email arrives
aws guardduty create-sample-findings --detector-id <detector-id> --finding-types Backdoor:EC2/C\&CActivity.B
aws guardduty get-detector --detector-id <detector-id> --query Status            # ENABLED
aws configservice describe-configuration-recorder-status                          # recording: true, lastStatus SUCCESS
```

The sample finding (severity 8) reaches the alert email through EventBridge.

### 4. Image mirror

As the admin session, check the rule reaches GHCR with the stored credential
(node roles cannot, by the boundary):

```bash
aws ecr validate-pull-through-cache-rule --ecr-repository-prefix ghcr   # isValid: true
```

Then from the throwaway instance, whose `smoke-node` policy grants the pull:

```bash
prefix=$(aws ssm get-parameter --name /iterabase/hosted/baseline/v1/image-registry-prefix --query Parameter.Value --output text)
aws ecr get-login-password | sudo docker login --username AWS --password-stdin "${prefix%%/*}"
sudo docker pull "$prefix/iterabase/control-plane@sha256:<published digest>"
```

The pulled digest equals the GHCR digest.

### 5. Cloudflare token

```bash
token=$(aws ssm get-parameter --name /iterabase/hosted/baseline/v1/cloudflare-dns-token --with-decryption --query Parameter.Value --output text)
curl -sf -X POST -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
  "https://api.cloudflare.com/client/v4/zones/<zone-id>/dns_records" \
  --data '{"type":"TXT","name":"hor-603-smoke","content":"\"ok\"","ttl":60}'
```

Then delete the record with the same token, and record that a request against
any other zone returns an authorization error.

### 6. SES outside the sandbox

```bash
aws sesv2 get-account --query '{production:ProductionAccessEnabled,sending:SendingEnabled}'
aws sesv2 get-email-identity --email-identity <mail-domain> --query '{verified:VerifiedForSendingStatus,dkim:DkimAttributes.Status,mailFrom:MailFromAttributes.MailFromDomainStatus}'
aws sesv2 send-email --from-email-address noreply@<mail-domain> \
  --destination ToAddresses=<an address that is not verified in SES> \
  --content 'Simple={Subject={Data=HOR-603},Body={Text={Data=delivery proof}}}'
```

The received message shows `dkim=pass`, `spf=pass` (MAIL FROM) and `dmarc=pass`.

### 7. Restricted buckets and key

As the support role, and as an IAM principal outside the allowed patterns, each
of these returns `AccessDenied`:

```bash
aws s3 ls s3://iterabase-hosted-tfstate-$A/
aws s3 ls s3://iterabase-hosted-backups-$A/
aws kms encrypt --key-id alias/iterabase-hosted-backup --plaintext aGk=
```

As the Forge role, `envs/` in the state bucket works and `baseline/` is denied
(step 1); as the throwaway node role, `kms encrypt` succeeds once its role
policy grants it, and is denied for any role outside `/iterabase/env/`.

### Teardown of the throwaway environment

As the Forge role: terminate the instance, delete the security group, remove
the role from the instance profile, delete the instance profile, delete the
`smoke-node` inline policy, detach the managed policy, and delete the role.
The state bucket is versioned, so purge every version and delete marker under
the environment's prefix rather than deleting only the current objects:

```bash
bucket=iterabase-hosted-tfstate-$A
aws s3api list-object-versions --bucket "$bucket" --prefix envs/hor-603-smoke/ \
  --query '{Objects: [Versions, DeleteMarkers][][].{Key: Key, VersionId: VersionId}, Quiet: `true`}' \
  --output json > purge.json
aws s3api delete-objects --bucket "$bucket" --delete file://purge.json
aws s3api list-object-versions --bucket "$bucket" --prefix envs/hor-603-smoke/   # empty
aws s3api list-object-versions --bucket "$bucket" --prefix baseline/             # AccessDenied
```

Then confirm zero tagged leftovers:

```bash
aws resourcegroupstaggingapi get-resources --tag-filters Key=iterabase-env,Values=hor-603-smoke \
  --query 'ResourceTagMappingList[].ResourceARN'
```

## Operations

- **Changing the baseline:** a ticket-backed PR changes `infra/`; after merge
  the founder runs `terraform plan`/`apply` from `master` and records the plan
  summary on the ticket. Never apply from an unmerged branch.
- **Granting or revoking access:** change `forge_operator_principal_arns` or
  `support_principal_arns` and apply, or remove the Identity Center
  assignment. Support access to every environment is revoked in one place.
- **Lockout recovery:** a wrong `admin_principal_arns` denies administrators on
  the state and backup buckets and the backup key. Sign in as the hosted
  account root (MFA) and correct the bucket and key policies, then re-apply.
