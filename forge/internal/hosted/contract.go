// Package hosted provisions and tears down per-customer environments in the
// hosted AWS account (HOR-593). It reads the account-level baseline only through
// the versioned SSM contract (DES-HOR-603-03), acts as the baseline Forge role,
// runs the embedded Terraform module with a pinned binary, keeps state under
// envs/<name>/ in the baseline state bucket (DES-HOR-603-02), and proves
// teardown with a zero-result tag sweep.
package hosted

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// ContractPath is the baseline consumer contract (DES-HOR-603-03).
const ContractPath = "/iterabase/hosted/baseline/v1"

// Contract is the subset of the baseline contract Forge consumes.
type Contract struct {
	Region                     string
	AccountID                  string
	VPCID                      string
	SubnetIDs                  []string
	EnvTagKey                  string
	AllowedInstanceTypes       []string
	StateBucket                string
	StatePrefix                string
	BackupBucket               string
	BackupPrefix               string
	BackupKMSKeyARN            string
	ForgeRoleARN               string
	NodeRolePath               string
	NodePermissionsBoundaryARN string
	SessionLogBucket           string
	SessionLogGroup            string
	ImageRegistryPrefix        string
	HostedDomain               string
	LBControllerPolicyARN      string
	OriginPullCACert           string
	MailDomain                 string
	MailIdentityARN            string
	MailUserBoundaryARN        string
	// CloudflareDNSToken is the SecureString scoped to the hosted zone
	// (DES-HOR-601-03); it becomes the cluster's cloudflare-api-token Secret.
	CloudflareDNSToken string
}

// ParameterReader is the subset of the SSM client the contract reader uses.
type ParameterReader interface {
	GetParametersByPath(ctx context.Context, in *ssm.GetParametersByPathInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
}

// ReadContract reads every contract key (decrypting the SecureString token) and
// fails closed when any key Forge depends on is missing.
func ReadContract(ctx context.Context, api ParameterReader) (*Contract, error) {
	values := map[string]string{}
	var next *string
	for {
		out, err := api.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
			Path:           aws.String(ContractPath),
			WithDecryption: aws.Bool(true),
			NextToken:      next,
		})
		if err != nil {
			return nil, fmt.Errorf("read hosted baseline contract %s: %w", ContractPath, err)
		}
		for _, p := range out.Parameters {
			values[strings.TrimPrefix(aws.ToString(p.Name), ContractPath+"/")] = aws.ToString(p.Value)
		}
		if out.NextToken == nil {
			break
		}
		next = out.NextToken
	}
	return parseContract(values)
}

func parseContract(v map[string]string) (*Contract, error) {
	var missing []string
	get := func(key string) string {
		value := strings.TrimSpace(v[key])
		if value == "" {
			missing = append(missing, key)
		}
		return value
	}
	list := func(key string) []string {
		raw := strings.TrimSpace(v[key])
		var out []string
		for _, item := range strings.Split(raw, ",") {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
		if len(out) == 0 {
			missing = append(missing, key)
		}
		return out
	}
	c := &Contract{
		Region:                     get("region"),
		AccountID:                  get("account-id"),
		VPCID:                      get("vpc-id"),
		SubnetIDs:                  list("subnet-ids"),
		EnvTagKey:                  get("env-tag-key"),
		AllowedInstanceTypes:       list("allowed-instance-types"),
		StateBucket:                get("state-bucket"),
		StatePrefix:                get("state-prefix"),
		BackupBucket:               get("backup-bucket"),
		BackupPrefix:               get("backup-prefix"),
		BackupKMSKeyARN:            get("backup-kms-key-arn"),
		ForgeRoleARN:               get("forge-role-arn"),
		NodeRolePath:               get("node-role-path"),
		NodePermissionsBoundaryARN: get("node-permissions-boundary-arn"),
		SessionLogBucket:           get("session-log-bucket"),
		SessionLogGroup:            get("session-log-group"),
		ImageRegistryPrefix:        get("image-registry-prefix"),
		HostedDomain:               get("hosted-domain"),
		LBControllerPolicyARN:      get("lb-controller-policy-arn"),
		OriginPullCACert:           get("origin-pull-ca-cert"),
		MailDomain:                 get("mail-domain"),
		MailIdentityARN:            get("mail-identity-arn"),
		MailUserBoundaryARN:        get("mail-user-boundary-arn"),
		CloudflareDNSToken:         get("cloudflare-dns-token"),
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("hosted baseline contract %s is missing %s; apply the baseline that publishes them", ContractPath, strings.Join(missing, ", "))
	}
	return c, nil
}

// AllowsInstanceType reports whether the baseline lets the Forge role launch t.
func (c *Contract) AllowsInstanceType(t string) bool {
	for _, allowed := range c.AllowedInstanceTypes {
		if allowed == t {
			return true
		}
	}
	return false
}
