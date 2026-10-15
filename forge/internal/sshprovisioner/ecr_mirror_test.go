package sshprovisioner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseECRMirror(t *testing.T) {
	m, err := ParseECRMirror("123456789012.dkr.ecr.eu-west-1.amazonaws.com/ghcr", "eu-west-1")
	require.NoError(t, err)
	assert.Equal(t, ECRMirror{Registry: "123456789012.dkr.ecr.eu-west-1.amazonaws.com", Prefix: "ghcr", Region: "eu-west-1"}, m)

	for _, bad := range []string{
		"ghcr",
		"example.com/ghcr",
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com/",
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com/gh cr",
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com/ghcr\"; rm -rf /",
	} {
		_, err := ParseECRMirror(bad, "eu-west-1")
		assert.Error(t, err, bad)
	}
	_, err = ParseECRMirror("123456789012.dkr.ecr.eu-west-1.amazonaws.com/ghcr", "eu-west-1; true")
	assert.Error(t, err)
}

func TestECRMirrorScript(t *testing.T) {
	s := ecrMirrorScript(ECRMirror{Registry: "123456789012.dkr.ecr.eu-west-1.amazonaws.com", Prefix: "ghcr", Region: "eu-west-1"})
	for _, want := range []string{
		`REGISTRY="123456789012.dkr.ecr.eu-west-1.amazonaws.com"`,
		`PREFIX="ghcr"`,
		`REGION="eu-west-1"`,
		`HOSTS_DIR="/var/lib/rancher/k3s/agent/etc/containerd/certs.d/ghcr.io"`,
		`--aws-sigv4 "aws:amz:${REGION}:ecr"`,
		`AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken`,
		`server = "https://ghcr.io"`,
		`override_path = true`,
		`Authorization = ["Basic ${TOKEN}"]`,
		`-K "$work/curl.conf"`,
		`f.write('user = "%s:%s"\n' % (c["AccessKeyId"], c["SecretAccessKey"]))`,
	} {
		assert.Contains(t, s, want)
	}
	assert.NotContains(t, s, "--user", "credentials never appear in curl's argument list")
	assert.Contains(t, ecrMirrorK3sDropIn, "ExecStartPost=-/usr/local/sbin/iterabase-ecr-mirror")
	assert.Contains(t, ecrMirrorTimer, "OnUnitActiveSec=6h")
}
