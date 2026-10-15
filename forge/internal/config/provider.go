package config

import "fmt"

// Provider selects who provisions the host. With no provider the operator
// supplies an existing host (spec.hosts) reached over SSH. With provider.aws,
// Forge provisions a dedicated environment in the hosted AWS account, reaches it
// with SSH over SSM, and derives the host and data disk from that environment
// (HOR-593).
type Provider struct {
	AWS *AWSProvider `yaml:"aws,omitempty"`
}

// AWSProvider is the per-customer hosted environment. Everything account-level
// (VPC, subnets, roles, buckets, image mirror, DNS zone) comes from the hosted
// baseline contract /iterabase/hosted/baseline/v1/* (DES-HOR-603-03), never from
// forge.yaml.
type AWSProvider struct {
	// Region is the hosted account region (DES-HOR-603-04). Default eu-west-1.
	Region string `yaml:"region"`
	// Profile is an optional AWS shared-config profile for the operator identity
	// that assumes the baseline Forge role. Empty uses the SDK default chain
	// (AWS_PROFILE, environment credentials, ...).
	Profile string `yaml:"profile"`
	// InstanceType must be one of the contract's allowed-instance-types.
	// Default m7i-flex.xlarge (HOR-602 hosted baseline).
	InstanceType string `yaml:"instanceType"`
	// RootVolumeGiB is the encrypted gp3 root disk. Default 50.
	RootVolumeGiB int `yaml:"rootVolumeGiB"`
	// DataVolumeGiB is the separate encrypted gp3 volume used as the blank
	// whole disk for the iterabase-data volume group. Default 100.
	DataVolumeGiB int `yaml:"dataVolumeGiB"`
}

// Hosted-environment defaults.
const (
	DefaultAWSRegion        = "eu-west-1"
	DefaultAWSInstanceType  = "m7i-flex.xlarge"
	DefaultAWSRootVolumeGiB = 50
	DefaultAWSDataVolumeGiB = 100
)

func (p *Provider) applyDefaults() {
	a := p.AWS
	if a == nil {
		return
	}
	if a.Region == "" {
		a.Region = DefaultAWSRegion
	}
	if a.InstanceType == "" {
		a.InstanceType = DefaultAWSInstanceType
	}
	if a.RootVolumeGiB == 0 {
		a.RootVolumeGiB = DefaultAWSRootVolumeGiB
	}
	if a.DataVolumeGiB == 0 {
		a.DataVolumeGiB = DefaultAWSDataVolumeGiB
	}
}

func (p Provider) validate() error {
	a := p.AWS
	if a == nil {
		return nil
	}
	if a.RootVolumeGiB < 20 || a.RootVolumeGiB > 1024 {
		return fmt.Errorf("provider.aws.rootVolumeGiB must be between 20 and 1024, got %d", a.RootVolumeGiB)
	}
	if a.DataVolumeGiB < 20 || a.DataVolumeGiB > 16384 {
		return fmt.Errorf("provider.aws.dataVolumeGiB must be between 20 and 16384, got %d", a.DataVolumeGiB)
	}
	return nil
}

// KubeAPIHost is the address written into fetched kubeconfigs. A hosted AWS
// environment exposes no inbound ports, so its API server is reached on
// localhost through an SSM port-forward (forge tunnel); any other host is
// reached directly at its address.
func (c *Cluster) KubeAPIHost() string {
	if c.Spec.Provider.AWS != nil {
		return "127.0.0.1"
	}
	return c.Spec.Hosts[0].Address
}

// ValidateResolved validates a provider.aws config after Forge has derived its
// host and data disk from the provisioned environment: the same rules as an
// operator-supplied host.
func (c *Cluster) ValidateResolved() error {
	resolved := *c
	resolved.Spec.Provider = Provider{}
	return resolved.Validate()
}
