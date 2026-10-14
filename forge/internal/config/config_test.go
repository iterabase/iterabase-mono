package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// validCluster returns a config that passes validation.
func validCluster() *Cluster {
	return &Cluster{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "opo1"},
		Spec: Spec{
			Mode:        ModeSingleNode,
			DataStorage: DataStorage{Devices: []string{"/dev/disk/by-id/scsi-data-a", "/dev/disk/by-id/scsi-data-b"}},
			Hosts: []Host{{
				Address:      "10.20.0.10",
				SSHUser:      "forge",
				SSHKeyPath:   "~/.ssh/forge_ed25519",
				SSHTrustFile: "~/.forge/trust/opo1/10.20.0.10",
				Role:         RoleControlPlaneWorker,
				Labels:       map[string]string{"forge.horizonshift.io/env": "test"},
				Taints:       []Taint{{Key: "nvidia.com/gpu", Value: "true", Effect: "NoSchedule"}},
			}},
			K3s: K3s{
				Version:       "v1.31.5",
				ClusterCIDR:   "10.42.0.0/16",
				ServiceCIDR:   "10.43.0.0/16",
				DualStack:     true,
				ClusterCIDRv6: "fd42::/48",
				ServiceCIDRv6: "fd43::/112",
				Disable:       []string{"traefik", "servicelb"},
			},
		},
	}
}

// yamlFor marshals a (possibly mutated) valid cluster to YAML for parsing tests.
func yamlFor(t *testing.T, fn func(*Cluster)) []byte {
	t.Helper()
	c := validCluster()
	if fn != nil {
		fn(c)
	}
	out, err := yaml.Marshal(c)
	require.NoError(t, err)
	return out
}

func TestParse_Valid(t *testing.T) {
	c, err := Parse(yamlFor(t, nil))
	require.NoError(t, err)
	assert.Equal(t, APIVersion, c.APIVersion)
	assert.Equal(t, "opo1", c.Metadata.Name)
	require.Len(t, c.Spec.Hosts, 1)
	h := c.Spec.Hosts[0]
	assert.Equal(t, "10.20.0.10", h.Address)
	assert.Equal(t, "forge", h.SSHUser)
	assert.Equal(t, "~/.forge/trust/opo1/10.20.0.10", h.SSHTrustFile)
	assert.Equal(t, "test", h.Labels["forge.horizonshift.io/env"])
	require.Len(t, h.Taints, 1)
	assert.Equal(t, "NoSchedule", h.Taints[0].Effect)
	assert.True(t, c.Spec.K3s.DualStack)
	assert.Equal(t, []string{"traefik", "servicelb"}, c.Spec.K3s.Disable)
	assert.Equal(t, []string{"/dev/disk/by-id/scsi-data-a", "/dev/disk/by-id/scsi-data-b"}, c.Spec.DataStorage.Devices)
}

func TestParse_DualStackDisabledNoV6Required(t *testing.T) {
	c, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.K3s.DualStack = false
		cc.Spec.K3s.ClusterCIDRv6 = ""
		cc.Spec.K3s.ServiceCIDRv6 = ""
	}))
	require.NoError(t, err)
	assert.False(t, c.Spec.K3s.DualStack)
}

func TestParse_RejectsUnreviewedK3sVersion(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.K3s.Version = "v1.32.0" }))
	require.ErrorContains(t, err, "no repository-reviewed executable/runtime identity")
}

func TestParseRejectsK3sStorageAuthorityOverrides(t *testing.T) {
	for _, args := range [][]string{
		{"--disable="},
		{"--data-dir=/srv/k3s"},
		{"--kubelet-arg=root-dir=/srv/kubelet"},
		{"--kubelet-arg", "root-dir=/srv/kubelet"},
		{"--kubelet-arg"},
		{"--kubelet-arg", "--node-label=role=worker"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.K3s.ExtraArgs = args }))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "k3s.extraArgs")
		})
	}
}

func TestParseAcceptsReviewedNonRootKubeletArgForms(t *testing.T) {
	for _, args := range [][]string{
		{"--kubelet-arg=serialize-image-pulls=false"},
		{"--kubelet-arg", "serialize-image-pulls=false", "--node-label=role=worker"},
	} {
		_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.K3s.ExtraArgs = args }))
		require.NoError(t, err)
	}
}

func TestParse_BadAPIVersion(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.APIVersion = "bad" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apiVersion")
}

func TestParse_BadName(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Metadata.Name = "OPO 1" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata.name")
}

func TestParse_ReservedMode(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.Mode = ModeHA }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved")
}

func TestParse_InvalidMode(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.Mode = "bogus" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid")
}

func TestParseDataStorageRequiresCanonicalStableUniqueSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		devices []string
		want    string
	}{
		{name: "missing", want: "at least one"},
		{name: "volatile", devices: []string{"/dev/sdb"}, want: "/dev/disk/by-id"},
		{name: "partition", devices: []string{"/dev/disk/by-id/scsi-data-part1"}, want: "whole disk"},
		{name: "nested", devices: []string{"/dev/disk/by-id/nested/device"}, want: "stable"},
		{name: "duplicate", devices: []string{"/dev/disk/by-id/scsi-a", "/dev/disk/by-id/scsi-a"}, want: "duplicate"},
		{name: "order drift", devices: []string{"/dev/disk/by-id/scsi-b", "/dev/disk/by-id/scsi-a"}, want: "canonical lexical order"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.DataStorage.Devices = tc.devices }))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestParse_NoHosts(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.Hosts = nil }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly 1 host")
}

func TestParse_TooManyHosts(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) {
		c.Spec.Hosts = append(c.Spec.Hosts, c.Spec.Hosts[0])
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly 1 host")
}

func TestParse_MissingSSHUser(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.Hosts[0].SSHUser = "" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sshUser")
}

func TestParse_SSHTrustFileIsRequired(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.Hosts[0].SSHTrustFile = "" }))
	require.ErrorContains(t, err, "sshTrustFile is required")

	cluster, err := Parse(yamlFor(t, func(c *Cluster) {
		c.Spec.Hosts[0].SSHTrustFile = "~/.forge/trust/opo1/10.20.0.10"
	}))
	require.NoError(t, err)
	assert.Equal(t, "~/.forge/trust/opo1/10.20.0.10", cluster.Spec.Hosts[0].SSHTrustFile)
}

func TestParse_RejectsLegacyInlineSSHHostKey(t *testing.T) {
	// The removed inline pin is detected by raw YAML so the migration error
	// never echoes the legacy material.
	legacy := []byte(`apiVersion: forge.horizonshift.io/v1alpha1
kind: Cluster
metadata:
  name: opo1
spec:
  mode: single-node
  dataStorage:
    devices: [/dev/disk/by-id/scsi-data-a]
  hosts:
    - address: 10.20.0.10
      sshUser: forge
      sshKeyPath: ~/.ssh/forge_ed25519
      sshHostKey: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixtureLegacySecretMaterial
      sshTrustFile: ~/.forge/trust/opo1/10.20.0.10
      role: control-plane+worker
  k3s:
    version: v1.31.5
    clusterCIDR: 10.42.0.0/16
    serviceCIDR: 10.43.0.0/16
`)
	_, err := Parse(legacy)
	require.ErrorContains(t, err, "sshHostKey is removed")
	require.NotContains(t, err.Error(), "AAAAIFixtureLegacySecretMaterial")
}

func TestParse_BadRole(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.Hosts[0].Role = RoleWorker }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "role")
}

func TestParse_InvalidTaintEffect(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.Hosts[0].Taints[0].Effect = "Maybe" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "effect")
}

func TestParse_InvalidCIDR(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.K3s.ClusterCIDR = "not-a-cidr" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "clusterCIDR")
}

func TestParse_DualStackMissingV6(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.K3s.ClusterCIDRv6 = "" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "clusterCIDRv6")
}

func TestParse_DualStackV6IsV4(t *testing.T) {
	_, err := Parse(yamlFor(t, func(c *Cluster) { c.Spec.K3s.ClusterCIDRv6 = "10.99.0.0/16" }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IPv6")
}

func TestLoad_NotFound(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	require.Error(t, err)
}

func TestLoad_FromFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "forge.yaml")
	require.NoError(t, os.WriteFile(p, yamlFor(t, nil), 0o600))
	c, err := Load(p)
	require.NoError(t, err)
	assert.Equal(t, "opo1", c.Metadata.Name)
}

func TestParse_ChartDefaults(t *testing.T) {
	c, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.Chart = Chart{Version: "0.1.0"}
	}))
	require.NoError(t, err)
	assert.Equal(t, "0.1.0", c.Spec.Chart.Version)
	assert.Equal(t, "oci://ghcr.io/iterabase/iterabase-charts/iterabase-platform", c.Spec.Chart.Repository)
	assert.Equal(t, "opo1", c.Spec.Chart.Release)
	assert.Equal(t, "iterabase-system", c.Spec.Chart.Namespace)
}

func TestParse_ChartEmptySkipsDefaults(t *testing.T) {
	c, err := Parse(yamlFor(t, nil))
	require.NoError(t, err)
	assert.Empty(t, c.Spec.Chart.Version)
	assert.Empty(t, c.Spec.Chart.Repository)
}

func TestParse_GPUDefaults(t *testing.T) {
	c, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.GPU = GPU{Enabled: true, Driver: GPUDriver{Version: "570.186", SHA256: strings.Repeat("a", 64)}}
	}))
	require.NoError(t, err)
	assert.True(t, c.Spec.GPU.Enabled)
	assert.Equal(t, defaultGPUOperatorVersion, c.Spec.GPU.Operator.Version)
	assert.Equal(t, defaultGPUOperatorRepository, c.Spec.GPU.Operator.Repository)
	assert.Equal(t, defaultGPUOperatorChart, c.Spec.GPU.Operator.Chart)
	assert.Equal(t, "opo1-gpu-operator", c.Spec.GPU.Operator.Release)
	assert.Equal(t, defaultGPUOperatorNamespace, c.Spec.GPU.Operator.Namespace)
	assert.Equal(t, defaultGPUOperatorSHA256, c.Spec.GPU.Operator.SHA256)
}

func TestParse_GPUDisabledNoDefaults(t *testing.T) {
	c, err := Parse(yamlFor(t, nil))
	require.NoError(t, err)
	assert.False(t, c.Spec.GPU.Enabled)
	assert.Empty(t, c.Spec.GPU.Operator.Version)
}

func TestParse_GPUDriverRequiresCompleteContentIdentity(t *testing.T) {
	for _, driver := range []GPUDriver{
		{},
		{Version: "570.186"},
		{Version: "570.186", SHA256: "invalid"},
	} {
		_, err := Parse(yamlFor(t, func(cc *Cluster) {
			cc.Spec.GPU = GPU{Enabled: true, Driver: driver}
		}))
		require.Error(t, err)
	}

	digest := strings.Repeat("a", 64)
	c, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.GPU = GPU{Enabled: true, Driver: GPUDriver{Version: "570.186", SHA256: digest}}
	}))
	require.NoError(t, err)
	assert.Equal(t, "570.186", c.Spec.GPU.Driver.Version)
	assert.Equal(t, digest, c.Spec.GPU.Driver.SHA256)
}

func TestGPUValidate_RequiresSingleNode(t *testing.T) {
	err := GPU{Enabled: true}.validate(ModeHA)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "single-node")
}

func TestGPUValidate_DisabledRejectsDriverVersion(t *testing.T) {
	// HOR-401: a non-empty driver pin with gpu.enabled: false is invalid — the
	// pin is inert when no operator runs, so it must not be silently ignored.
	err := GPU{Enabled: false, Driver: GPUDriver{Version: "570.186"}}.validate(ModeSingleNode)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gpu.driver identity")
	assert.Contains(t, err.Error(), "gpu.enabled is false")

	// Disabled with no driver pin remains valid.
	require.NoError(t, GPU{Enabled: false}.validate(ModeSingleNode))
}

func TestParse_OverlayDefaults(t *testing.T) {
	c, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.Overlay = Overlay{Path: "../overlays/customers/acme"}
	}))
	require.NoError(t, err)
	assert.Equal(t, "../overlays/customers/acme", c.Spec.Overlay.Path)
	assert.Equal(t, []string{"values.yaml"}, c.Spec.Overlay.ValueFiles)
	assert.Equal(t, []string{"tools"}, c.Spec.Overlay.ToolDirs)
}

func TestParse_OverlayExplicitListsKept(t *testing.T) {
	c, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.Overlay = Overlay{
			Path:       "customers/acme",
			ValueFiles: []string{"../../bases/hosted/values.yaml", "values.yaml"},
			ToolDirs:   []string{"../../components/support/tools", "tools"},
		}
	}))
	require.NoError(t, err)
	assert.Equal(t, []string{"../../bases/hosted/values.yaml", "values.yaml"}, c.Spec.Overlay.ValueFiles, "order is apply order and parent-relative shared bases are allowed")
	assert.Equal(t, []string{"../../components/support/tools", "tools"}, c.Spec.Overlay.ToolDirs)
}

func TestParse_OverlayEmptySkips(t *testing.T) {
	c, err := Parse(yamlFor(t, nil))
	require.NoError(t, err)
	assert.Empty(t, c.Spec.Overlay.Path)
	assert.Empty(t, c.Spec.Overlay.ValueFiles, "no defaults without overlay.path")
	assert.Empty(t, c.Spec.Overlay.ToolDirs)
}

func TestParse_OverlayListsRequirePath(t *testing.T) {
	for _, ov := range []Overlay{
		{ValueFiles: []string{"values.yaml"}},
		{ToolDirs: []string{"tools"}},
	} {
		_, err := Parse(yamlFor(t, func(cc *Cluster) { cc.Spec.Overlay = ov }))
		require.ErrorContains(t, err, "require overlay.path")
	}
}

func TestParse_OverlayRelativePaths(t *testing.T) {
	for _, tt := range []struct {
		name string
		ov   Overlay
		want string
	}{
		{name: "absolute value file", ov: Overlay{Path: "acme", ValueFiles: []string{"/etc/values.yaml"}}, want: "overlay.valueFiles[0]"},
		{name: "blank value file", ov: Overlay{Path: "acme", ValueFiles: []string{"values.yaml", "  "}}, want: "overlay.valueFiles[1]: must be non-empty"},
		{name: "absolute tool dir", ov: Overlay{Path: "acme", ToolDirs: []string{"/srv/tools"}}, want: "overlay.toolDirs[0]"},
		{name: "blank tool dir", ov: Overlay{Path: "acme", ToolDirs: []string{""}}, want: "overlay.toolDirs[0]: must be non-empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(yamlFor(t, func(cc *Cluster) { cc.Spec.Overlay = tt.ov }))
			require.ErrorContains(t, err, tt.want)
		})
	}
	_, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.Overlay = Overlay{Path: "/srv/overlays/customers/acme"}
	}))
	require.NoError(t, err, "overlay.path itself may be absolute")
}

func TestParse_RejectsLegacyGitOverlay(t *testing.T) {
	for _, legacy := range []string{
		"overlay:\n  repo: https://github.com/example/iterabase-overlay.git\n",
		"overlay:\n  repo: file:///srv/overlay\n  ref: main\n",
		"overlay:\n  path: customers/acme\n  ref: main\n",
	} {
		_, err := Parse(withSpec(t, legacy))
		require.ErrorContains(t, err, "overlay.repo and overlay.ref are removed", legacy)
		require.ErrorContains(t, err, "set overlay.path", legacy)
	}
}

func TestParse_RejectsFlux(t *testing.T) {
	for _, legacy := range []string{
		"flux:\n  enabled: true\n  version: v2.4.0\n",
		"flux:\n  enabled: false\n",
		"flux: {}\n",
	} {
		_, err := Parse(withSpec(t, legacy))
		require.ErrorContains(t, err, "flux is removed", legacy)
		require.ErrorContains(t, err, "DES-HOR-593-04", legacy)
		require.ErrorContains(t, err, "delete the flux block", legacy)
	}
}

// withSpec appends raw keys under spec of the valid cluster YAML.
func withSpec(t *testing.T, raw string) []byte {
	t.Helper()
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(yamlFor(t, nil), &doc))
	var extra map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(raw), &extra))
	spec := doc["spec"].(map[string]any)
	for k, v := range extra {
		spec[k] = v
	}
	out, err := yaml.Marshal(doc)
	require.NoError(t, err)
	return out
}

func TestLoad_SetsConfigDir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "forge.yaml")
	require.NoError(t, os.WriteFile(p, yamlFor(t, func(cc *Cluster) {
		cc.Spec.Overlay = Overlay{Path: "overlay"}
	}), 0o600))
	c, err := Load(p)
	require.NoError(t, err)
	assert.Equal(t, dir, c.Dir)
	assert.True(t, filepath.IsAbs(c.Dir))

	// A relative config path still records an absolute directory.
	t.Chdir(dir)
	c, err = Load("forge.yaml")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(c.Dir))
	assert.Equal(t, filepath.Base(dir), filepath.Base(c.Dir))

	parsed, err := Parse(yamlFor(t, nil))
	require.NoError(t, err)
	assert.Empty(t, parsed.Dir, "Parse has no file location")
	out, err := yaml.Marshal(c)
	require.NoError(t, err)
	assert.NotContains(t, string(out), dir, "Dir is never serialized")
}

func awsCluster(fn func(*AWSProvider)) func(*Cluster) {
	return func(cc *Cluster) {
		cc.Spec.Hosts = nil
		cc.Spec.DataStorage = DataStorage{}
		cc.Spec.Provider.AWS = &AWSProvider{}
		if fn != nil {
			fn(cc.Spec.Provider.AWS)
		}
	}
}

func TestParse_AWSProviderDefaults(t *testing.T) {
	c, err := Parse(yamlFor(t, awsCluster(nil)))
	require.NoError(t, err)
	require.NotNil(t, c.Spec.Provider.AWS)
	assert.Equal(t, AWSProvider{
		Region:        DefaultAWSRegion,
		InstanceType:  DefaultAWSInstanceType,
		RootVolumeGiB: DefaultAWSRootVolumeGiB,
		DataVolumeGiB: DefaultAWSDataVolumeGiB,
	}, *c.Spec.Provider.AWS)
	assert.Equal(t, "eu-west-1", DefaultAWSRegion)
	assert.Equal(t, "127.0.0.1", c.KubeAPIHost(), "hosted API is reached through the SSM port-forward")
}

func TestParse_AWSProviderFromYAML(t *testing.T) {
	c, err := Parse([]byte(`apiVersion: ` + APIVersion + `
kind: ` + Kind + `
metadata:
  name: acme
spec:
  mode: single-node
  provider:
    aws:
      region: eu-central-1
      profile: iterabase-hosted
      instanceType: g6.xlarge
      rootVolumeGiB: 80
      dataVolumeGiB: 500
  k3s:
    version: v1.31.5
    clusterCIDR: 10.42.0.0/16
    serviceCIDR: 10.43.0.0/16
`))
	require.NoError(t, err)
	assert.Equal(t, AWSProvider{Region: "eu-central-1", Profile: "iterabase-hosted", InstanceType: "g6.xlarge", RootVolumeGiB: 80, DataVolumeGiB: 500}, *c.Spec.Provider.AWS)
	assert.Empty(t, c.Spec.Hosts)
}

func TestParse_AWSProviderBounds(t *testing.T) {
	for _, tt := range []struct {
		name string
		fn   func(*AWSProvider)
		want string
	}{
		{name: "root too small", fn: func(a *AWSProvider) { a.RootVolumeGiB = 19 }, want: "rootVolumeGiB must be between 20 and 1024, got 19"},
		{name: "root too large", fn: func(a *AWSProvider) { a.RootVolumeGiB = 1025 }, want: "rootVolumeGiB must be between 20 and 1024"},
		{name: "root negative", fn: func(a *AWSProvider) { a.RootVolumeGiB = -1 }, want: "rootVolumeGiB"},
		{name: "data too small", fn: func(a *AWSProvider) { a.DataVolumeGiB = 19 }, want: "dataVolumeGiB must be between 20 and 16384, got 19"},
		{name: "data too large", fn: func(a *AWSProvider) { a.DataVolumeGiB = 16385 }, want: "dataVolumeGiB must be between 20 and 16384"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(yamlFor(t, awsCluster(tt.fn)))
			require.ErrorContains(t, err, tt.want)
		})
	}
	for _, edge := range []func(*AWSProvider){
		func(a *AWSProvider) { a.RootVolumeGiB, a.DataVolumeGiB = 20, 20 },
		func(a *AWSProvider) { a.RootVolumeGiB, a.DataVolumeGiB = 1024, 16384 },
	} {
		_, err := Parse(yamlFor(t, awsCluster(edge)))
		require.NoError(t, err)
	}
}

func TestParse_AWSProviderRejectsHostsAndDataStorage(t *testing.T) {
	_, err := Parse(yamlFor(t, func(cc *Cluster) {
		cc.Spec.Provider.AWS = &AWSProvider{} // hosts and dataStorage kept from validCluster
	}))
	require.ErrorContains(t, err, "provider.aws derives hosts and dataStorage")

	_, err = Parse(yamlFor(t, func(cc *Cluster) {
		awsCluster(nil)(cc)
		cc.Spec.Hosts = validCluster().Spec.Hosts
	}))
	require.ErrorContains(t, err, "remove them from forge.yaml")

	_, err = Parse(yamlFor(t, func(cc *Cluster) {
		awsCluster(nil)(cc)
		cc.Spec.DataStorage = validCluster().Spec.DataStorage
	}))
	require.ErrorContains(t, err, "remove them from forge.yaml")
}

func TestParse_NoProviderStillRequiresHostAndDataStorage(t *testing.T) {
	_, err := Parse(yamlFor(t, func(cc *Cluster) { cc.Spec.Hosts = nil }))
	require.ErrorContains(t, err, "requires exactly 1 host")
	c, err := Parse(yamlFor(t, nil))
	require.NoError(t, err)
	assert.Nil(t, c.Spec.Provider.AWS)
	assert.Equal(t, "10.20.0.10", c.KubeAPIHost())
}
