# Hosted customer environments (HOR-593)

How an Iterabase operator provisions, connects to, converts and tears down a
dedicated customer environment in the hosted AWS account. Design authority:
[`docs/architecture/forge-hosted-environments.md`](../architecture/forge-hosted-environments.md).
The account-level baseline these environments consume is
[`aws-hosted.md`](aws-hosted.md).

Every step below creates, changes or deletes real customer infrastructure. Run
it only with the founder's approval for that environment and step.

## Prerequisites

- The hosted baseline is applied and its contract is complete, including the
  HOR-601 keys (`hosted-domain`, `lb-controller-policy-arn`,
  `origin-pull-ca-cert`). Forge fails closed and names any missing key.
- An Identity Center session whose principal matches the baseline's
  `forge_operator_principal_arns`, for example
  `aws sso login --sso-session iterabase` then `export AWS_PROFILE=iterabase-hosted-admin`
  (or set `provider.aws.profile`). Forge reads the contract with it and does
  everything else as the baseline Forge role.
- A checkout of the private overlays repository `iterabase/overlays`.
- A Forge release that includes HOR-593. Forge downloads its own pinned
  Terraform and session-manager-plugin into `~/.forge/bin/`.

## Provision

1. Scaffold and edit the customer overlay, then review and merge it:

   ```sh
   cd ~/src/overlays
   forge overlay new acme
   $EDITOR customers/acme/{forge.yaml,values.yaml,kustomization.yaml}
   ```

   `forge.yaml` pins the platform release (`spec.chart.version`, 0.6.0 or later)
   and sizes the environment under `provider.aws`. Names are DNS labels of at
   most 32 characters.

2. Provision, bootstrap and apply:

   ```sh
   forge init --provider aws --config customers/acme/forge.yaml
   ```

   Forge assumes the Forge role, runs the embedded Terraform module (state
   `envs/acme/terraform.tfstate`), waits for the SSM agent, then runs the normal
   apply over SSH over SSM: k3s, data storage on the separate EBS volume,
   substrates, the platform release, the hosted installation values, the DNS
   token Secret, and the overlay. It prints the app and gateway hostnames under
   the hosted domain.

3. Re-running the same command is idempotent: Terraform reports no changes and
   apply skips what is in sync.

Later changes use `forge apply --config customers/acme/forge.yaml`, or
`forge fleet apply` from the repository root for every customer.

## Connect

| Who | How | Audit |
| --- | --- | --- |
| Forge | SSH over an SSM `AWS-StartSSHSession` stream, every apply | CloudTrail `StartSession`/`TerminateSession` (`DES-HOR-603-07`) |
| Operator, kubectl | `forge tunnel --config customers/acme/forge.yaml` and `forge kubeconfig --config ... --out kubeconfig.yaml` (server `https://127.0.0.1:6443`) | CloudTrail session events |
| Support engineer | `aws ssm start-session --target <instance-id>` as the support role (logged interactive shell) | Session content in S3 and CloudWatch, 365 days |

No inbound port is ever opened. Revoking access is an IAM change in the
baseline, in one place.

## Convert or extend a trial

The plan lives in the central gateway, not the overlay (`DES-HOR-661-05`):

```sh
forge central plan --installation acme --plan starter
forge central extend-trial --installation acme --days 7
```

The environment, hostnames and data are unchanged; no `forge apply` is needed.

## Tear down

```sh
forge destroy --config customers/acme/forge.yaml
```

Forge, in order:

1. deletes every `LoadBalancer` Service and waits until no load balancer,
   target group or controller security group tagged `iterabase-env=acme`
   remains;
2. runs `terraform destroy`;
3. purges every version under the backup prefix `envs/acme/`;
4. revokes the installation's central-gateway certificate (HOR-661);
5. purges every version under the state prefix `envs/acme/`;
6. succeeds only when the tag sweep for `iterabase-env=acme` finds zero
   resources, including the environment's IAM role and instance profile.

If a step fails, fix the cause and run `forge destroy` again: every step is
idempotent, and a missing state skips straight to the purges and the sweep.
Then remove `customers/acme/` from the overlays repository.

## Orphan check

Run at any time, as the Forge role or an administrator:

```sh
aws resourcegroupstaggingapi get-resources --tag-filters Key=iterabase-env,Values=acme
aws iam get-role --role-name iterabase-env-acme            # NoSuchEntity
aws s3api list-object-versions --bucket iterabase-hosted-tfstate-<account> --prefix envs/acme/
aws s3api list-object-versions --bucket iterabase-hosted-backups-<account> --prefix envs/acme/
```

The tagging API keeps reporting terminated instances for a while; their state
is `terminated`. Anything else is an orphan.
