package hosted

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
)

// Session is an authenticated view of the hosted account as the baseline Forge
// role. The operator's own identity is used only to read the contract and to
// assume that role; every mutation runs as the role, so the baseline's
// tag-scoped IAM is what bounds Forge (DES-HOR-603-08).
type Session struct {
	Name     string // environment name (forge.yaml metadata.name)
	Contract *Contract
	AWS      aws.Config // credentials of the assumed Forge role
}

// Open assumes the Forge role named by the contract and re-reads the contract
// as that role.
func Open(ctx context.Context, name string, p *config.AWSProvider) (*Session, error) {
	loadOpts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(p.Region)}
	if p.Profile != "" {
		loadOpts = append(loadOpts, awsconfig.WithSharedConfigProfile(p.Profile))
	}
	base, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS operator credentials: %w", err)
	}
	bootstrap, err := ReadContract(ctx, newSSM(base))
	if err != nil {
		return nil, err
	}
	if bootstrap.Region != p.Region {
		return nil, fmt.Errorf("provider.aws.region %s does not match the hosted baseline region %s", p.Region, bootstrap.Region)
	}

	roleCfg := base.Copy()
	roleCfg.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(base), bootstrap.ForgeRoleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = "forge-" + name
	}))
	contract, err := ReadContract(ctx, newSSM(roleCfg))
	if err != nil {
		return nil, fmt.Errorf("assume %s: %w", bootstrap.ForgeRoleARN, err)
	}
	return &Session{Name: name, Contract: contract, AWS: roleCfg}, nil
}

func newSSM(cfg aws.Config) *ssm.Client { return ssm.NewFromConfig(cfg) }
