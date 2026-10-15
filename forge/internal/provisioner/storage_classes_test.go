package provisioner_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/provisioner"
)

// One Forge binary validates older and newer platforms: the signature class
// is part of the exact set only from the substrate version that renders it
// (DES-HOR-461-05).
func TestManagedStorageClassesFollowSubstrateVersion(t *testing.T) {
	two := []provisioner.ManagedStorageClass{
		{Name: "iterabase-agentpool-lvm-xfs", Shared: true},
		{Name: "iterabase-lvm-xfs", Shared: false},
	}
	three := append(append([]provisioner.ManagedStorageClass{}, two...),
		provisioner.ManagedStorageClass{Name: "iterabase-signatures-lvm-xfs", Shared: true})
	for version, want := range map[string][]provisioner.ManagedStorageClass{
		"0.5.0": two, "v0.5.9": two, "0.6.0": three, "0.7.1": three, "not-a-version": three,
	} {
		assert.Equal(t, want, provisioner.ManagedStorageClasses(version), version)
	}
}
