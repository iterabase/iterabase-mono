package cli

import (
	"context"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/overlay"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/sshprovisioner"
)

// target is the host a forge.yaml resolves to and how Forge reaches it. For an
// operator-supplied host it is forge.yaml as written. For provider.aws it is
// derived from the provisioned environment: the host and data disk, the SSH
// over SSM transport, and the hosted installation-fact Helm values.
type target struct {
	Cluster    *config.Cluster
	SSHOptions []sshprovisioner.Option
	ValueFiles []overlay.ValueFile
	Manifests  []string // Forge-derived objects applied before the chart
	// Prepare runs host-level setup before the lifecycle apply (for example
	// the hosted ECR mirror, DES-HOR-593-07).
	Prepare func(context.Context, *sshprovisioner.SSHProvisioner) error
	close   func()
}

func (t *target) Close() {
	if t.close != nil {
		t.close()
	}
}

func resolveTarget(ctx context.Context, cfg *config.Cluster) (*target, error) {
	if cfg.Spec.Provider.AWS != nil {
		return resolveAWSTarget(ctx, cfg)
	}
	return &target{Cluster: cfg}, nil
}

// connect resolves cfg's target and opens an SSH provisioner to its host. The
// caller closes both.
func connect(ctx context.Context, cfg *config.Cluster) (*target, *sshprovisioner.SSHProvisioner, error) {
	t, err := resolveTarget(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	p, err := sshprovisioner.New(t.Cluster.Spec.Hosts[0], t.SSHOptions...)
	if err != nil {
		t.Close()
		return nil, nil, err
	}
	return t, p, nil
}
