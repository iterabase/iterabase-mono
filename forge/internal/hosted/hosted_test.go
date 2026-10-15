package hosted

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
)

// contractKeys are every key under ContractPath Forge consumes.
var contractKeys = []string{
	"region", "account-id", "vpc-id", "subnet-ids", "env-tag-key", "allowed-instance-types",
	"state-bucket", "state-prefix", "backup-bucket", "backup-prefix", "backup-kms-key-arn",
	"forge-role-arn", "node-role-path", "node-permissions-boundary-arn", "session-log-bucket",
	"session-log-group", "image-registry-prefix", "hosted-domain", "lb-controller-policy-arn",
	"origin-pull-ca-cert", "cloudflare-dns-token", "mail-domain", "mail-identity-arn",
	"mail-user-boundary-arn",
}

func contractValues() map[string]string {
	return map[string]string{
		"region":                        "eu-west-1",
		"account-id":                    "123456789012",
		"vpc-id":                        "vpc-0aaa",
		"subnet-ids":                    " subnet-0a , subnet-0b,,",
		"env-tag-key":                   "iterabase-env",
		"allowed-instance-types":        "m7i-flex.xlarge, g6.xlarge",
		"state-bucket":                  "iterabase-hosted-state",
		"state-prefix":                  "envs/",
		"backup-bucket":                 "iterabase-hosted-backups",
		"backup-prefix":                 "envs/",
		"backup-kms-key-arn":            "arn:aws:kms:eu-west-1:123456789012:key/k",
		"forge-role-arn":                "arn:aws:iam::123456789012:role/iterabase-forge",
		"node-role-path":                "/iterabase/env/",
		"node-permissions-boundary-arn": "arn:aws:iam::123456789012:policy/boundary",
		"session-log-bucket":            "iterabase-session-logs",
		"session-log-group":             "/iterabase/ssm",
		"image-registry-prefix":         "123456789012.dkr.ecr.eu-west-1.amazonaws.com/mirror",
		"hosted-domain":                 "iterabase.cloud",
		"lb-controller-policy-arn":      "arn:aws:iam::123456789012:policy/lbc",
		"origin-pull-ca-cert":           "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
		"cloudflare-dns-token":          "cf-token-value",
		"mail-domain":                   "email.iterabase.com",
		"mail-identity-arn":             "arn:aws:ses:eu-west-1:123456789012:identity/email.iterabase.com",
		"mail-user-boundary-arn":        "arn:aws:iam::123456789012:policy/iterabase/mail",
	}
}

func testContract(t *testing.T) *Contract {
	t.Helper()
	c, err := parseContract(contractValues())
	require.NoError(t, err)
	return c
}

func TestParseContract_AllKeys(t *testing.T) {
	values := contractValues()
	require.Len(t, values, len(contractKeys))
	c := testContract(t)
	assert.Equal(t, &Contract{
		Region:                     "eu-west-1",
		AccountID:                  "123456789012",
		VPCID:                      "vpc-0aaa",
		SubnetIDs:                  []string{"subnet-0a", "subnet-0b"},
		EnvTagKey:                  "iterabase-env",
		AllowedInstanceTypes:       []string{"m7i-flex.xlarge", "g6.xlarge"},
		StateBucket:                "iterabase-hosted-state",
		StatePrefix:                "envs/",
		BackupBucket:               "iterabase-hosted-backups",
		BackupPrefix:               "envs/",
		BackupKMSKeyARN:            "arn:aws:kms:eu-west-1:123456789012:key/k",
		ForgeRoleARN:               "arn:aws:iam::123456789012:role/iterabase-forge",
		NodeRolePath:               "/iterabase/env/",
		NodePermissionsBoundaryARN: "arn:aws:iam::123456789012:policy/boundary",
		SessionLogBucket:           "iterabase-session-logs",
		SessionLogGroup:            "/iterabase/ssm",
		ImageRegistryPrefix:        "123456789012.dkr.ecr.eu-west-1.amazonaws.com/mirror",
		HostedDomain:               "iterabase.cloud",
		LBControllerPolicyARN:      "arn:aws:iam::123456789012:policy/lbc",
		OriginPullCACert:           "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
		MailDomain:                 "email.iterabase.com",
		MailIdentityARN:            "arn:aws:ses:eu-west-1:123456789012:identity/email.iterabase.com",
		MailUserBoundaryARN:        "arn:aws:iam::123456789012:policy/iterabase/mail",
		CloudflareDNSToken:         "cf-token-value",
	}, c)
}

func TestParseContract_MissingKeysFailClosed(t *testing.T) {
	_, err := parseContract(map[string]string{})
	require.Error(t, err)
	for _, key := range contractKeys {
		assert.Contains(t, err.Error(), key)
	}

	values := contractValues()
	delete(values, "vpc-id")
	values["cloudflare-dns-token"] = "   "
	_, err = parseContract(values)
	require.ErrorContains(t, err, "is missing vpc-id, cloudflare-dns-token;")
	assert.NotContains(t, err.Error(), "cf-token-value")
}

type fakeParameters struct {
	pages [][]ssmtypes.Parameter
	calls []*ssm.GetParametersByPathInput
	err   error
}

func (f *fakeParameters) GetParametersByPath(_ context.Context, in *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return nil, f.err
	}
	i := len(f.calls) - 1
	out := &ssm.GetParametersByPathOutput{Parameters: f.pages[i]}
	if i+1 < len(f.pages) {
		out.NextToken = aws.String("page-" + string(rune('1'+i)))
	}
	return out, nil
}

func TestReadContract_PagesAndDecrypts(t *testing.T) {
	var params []ssmtypes.Parameter
	for k, v := range contractValues() {
		params = append(params, ssmtypes.Parameter{Name: aws.String(ContractPath + "/" + k), Value: aws.String(v)})
	}
	sort.Slice(params, func(i, j int) bool { return *params[i].Name < *params[j].Name })
	api := &fakeParameters{pages: [][]ssmtypes.Parameter{params[:10], params[10:]}}
	c, err := ReadContract(context.Background(), api)
	require.NoError(t, err)
	assert.Equal(t, testContract(t), c)
	require.Len(t, api.calls, 2)
	for _, call := range api.calls {
		assert.Equal(t, ContractPath, aws.ToString(call.Path))
		assert.True(t, aws.ToBool(call.WithDecryption))
	}
	assert.Nil(t, api.calls[0].NextToken)
	assert.Equal(t, "page-1", aws.ToString(api.calls[1].NextToken))

	_, err = ReadContract(context.Background(), &fakeParameters{err: assert.AnError})
	require.ErrorContains(t, err, "read hosted baseline contract /iterabase/hosted/baseline/v1")
}

func TestAllowsInstanceType(t *testing.T) {
	c := testContract(t)
	assert.True(t, c.AllowsInstanceType("m7i-flex.xlarge"))
	assert.True(t, c.AllowsInstanceType("g6.xlarge"))
	assert.False(t, c.AllowsInstanceType("g6"))
	assert.False(t, c.AllowsInstanceType("M7I-FLEX.XLARGE"))
	assert.False(t, c.AllowsInstanceType(""))
}

func TestStateKey(t *testing.T) {
	s := &Session{Name: "acme", Contract: testContract(t)}
	assert.Equal(t, "envs/acme/terraform.tfstate", s.StateKey())
}

// moduleVariables lists every variable the embedded module declares.
func moduleVariables(t *testing.T) []string {
	t.Helper()
	data, err := moduleFS.ReadFile("module/variables.tf")
	require.NoError(t, err)
	var names []string
	for _, m := range regexp.MustCompile(`(?m)^variable "([a-z_]+)"`).FindAllStringSubmatch(string(data), -1) {
		names = append(names, m[1])
	}
	sort.Strings(names)
	require.NotEmpty(t, names)
	return names
}

func TestVars_SetEveryModuleVariable(t *testing.T) {
	s := &Session{Name: "acme", Contract: testContract(t)}
	got := map[string]string{}
	for _, kv := range s.vars(Spec{InstanceType: "g6.xlarge", RootVolumeGiB: 80, DataVolumeGiB: 500}) {
		k, v, ok := strings.Cut(kv, "=")
		require.True(t, ok, kv)
		_, dup := got[k]
		require.False(t, dup, k)
		got[k] = v
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, moduleVariables(t), keys, "every module variable is set, and nothing else")
	assert.Equal(t, map[string]string{
		"name":                          "acme",
		"region":                        "eu-west-1",
		"env_tag_key":                   "iterabase-env",
		"vpc_id":                        "vpc-0aaa",
		"subnet_id":                     "subnet-0a",
		"instance_type":                 "g6.xlarge",
		"root_volume_gib":               "80",
		"data_volume_gib":               "500",
		"node_role_path":                "/iterabase/env/",
		"node_permissions_boundary_arn": "arn:aws:iam::123456789012:policy/boundary",
		"lb_controller_policy_arn":      "arn:aws:iam::123456789012:policy/lbc",
		"mail_identity_arn":             "arn:aws:ses:eu-west-1:123456789012:identity/email.iterabase.com",
		"mail_user_boundary_arn":        "arn:aws:iam::123456789012:policy/iterabase/mail",
		"backup_bucket":                 "iterabase-hosted-backups",
		"backup_prefix":                 "envs/",
		"backup_kms_key_arn":            "arn:aws:kms:eu-west-1:123456789012:key/k",
		"session_log_bucket":            "iterabase-session-logs",
		"session_log_group":             "/iterabase/ssm",
	}, got)
	assert.Len(t, varOptions(s.vars(Spec{})), len(got))
	assert.Len(t, destroyVarOptions(s.vars(Spec{})), len(got))
}

func TestPurgePrefix_RefusesBroadPrefixes(t *testing.T) {
	for _, prefix := range []string{"", "x", "envs/", "envs/acme", "/", "envs"} {
		n, err := purgePrefix(context.Background(), nil, "bucket", prefix)
		require.ErrorContains(t, err, "refusing to purge broad prefix", prefix)
		assert.Zero(t, n)
	}
}

func TestSpecFrom(t *testing.T) {
	s := Spec{InstanceType: "g6.xlarge", RootVolumeGiB: 50, DataVolumeGiB: 100}
	assert.Equal(t, s, SpecFrom(&config.AWSProvider{Region: "eu-west-1", InstanceType: "g6.xlarge", RootVolumeGiB: 50, DataVolumeGiB: 100}))
}

func TestInstallationValues(t *testing.T) {
	c := testContract(t)
	raw, err := c.InstallationValues("acme", "AKIAMAILUSER")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(raw), "# Generated by forge"))
	var got map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &got))
	assert.Equal(t, map[string]any{
		"global": map[string]any{
			"originPull": map[string]any{"caCert": "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"},
		},
		"aws-load-balancer-controller": map[string]any{
			"region":      "eu-west-1",
			"clusterName": "acme",
			"vpcId":       "vpc-0aaa",
			"defaultTags": map[string]any{"iterabase-env": "acme"},
		},
		"external-dns": map[string]any{
			"domainFilters": []any{"iterabase.cloud."},
		},
		"control-plane": map[string]any{
			"ingress": map[string]any{"host": "acme.iterabase.cloud"},
			"auth": map[string]any{
				"publicOrigin": "https://acme.iterabase.cloud",
				"email": map[string]any{
					"provider":       "ses",
					"ses":            map[string]any{"region": "eu-west-1"},
					"from":           "Iterabase <no-reply@email.iterabase.com>",
					"username":       "AKIAMAILUSER",
					"existingSecret": "iterabase-smtp",
					"passwordKey":    "smtp-password",
				},
			},
		},
		"inference-gateway": map[string]any{
			"ingress": map[string]any{"host": "acme-gateway.iterabase.cloud"},
		},
	}, got)
	assert.NotContains(t, string(raw), "cf-token-value", "the DNS token never reaches Helm values")
	assert.Equal(t, "acme.iterabase.cloud", c.AppHost("acme"))
	assert.Equal(t, "acme-gateway.iterabase.cloud", c.GatewayHost("acme"))
}

func TestCloudflareSecret(t *testing.T) {
	doc, err := testContract(t).CloudflareSecret("iterabase-system")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(doc), &got))
	assert.Equal(t, map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata": map[string]any{
			"name":      "cloudflare-api-token",
			"namespace": "iterabase-system",
			"labels":    map[string]any{"app.kubernetes.io/managed-by": "forge"},
		},
		"stringData": map[string]any{"api-token": "cf-token-value"},
	}, got)
}

func TestWriteModule_WritesEveryEmbeddedFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "terraform")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stale.tf"), []byte("x"), 0o600))
	require.NoError(t, writeModule(dir))

	var embedded []string
	require.NoError(t, fs.WalkDir(moduleFS, "module", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel("module", path)
		embedded = append(embedded, rel)
		want, err := moduleFS.ReadFile(path)
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(dir, rel))
		require.NoError(t, err, rel)
		assert.Equal(t, want, got, rel)
		info, err := os.Stat(filepath.Join(dir, rel))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), rel)
		return nil
	}))
	assert.Contains(t, embedded, ".terraform.lock.hcl", "the provider lock file is embedded (all: prefix)")
	assert.Contains(t, embedded, "main.tf")
	assert.Contains(t, embedded, "variables.tf")

	_, err := os.Stat(filepath.Join(dir, "stale.tf"))
	assert.True(t, os.IsNotExist(err), "the working directory is replaced, not merged")
}

// fakeTerraform is a script answering `terraform output -no-color -json`.
func fakeTerraform(t *testing.T, outputJSON string) *workspace {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "output.json")
	require.NoError(t, os.WriteFile(out, []byte(outputJSON), 0o600))
	bin := filepath.Join(dir, "terraform")
	script := "#!/bin/sh\nif [ \"$1\" = output ]; then cat '" + out + "'; exit 0; fi\nexit 1\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o700))
	tf, err := tfexec.NewTerraform(dir, bin)
	require.NoError(t, err)
	return &workspace{tf: tf, dir: dir}
}

func outputJSON(values map[string]any) string {
	m := map[string]any{}
	for k, v := range values {
		m[k] = map[string]any{"sensitive": k == "client_private_key" || k == "smtp_password", "type": "string", "value": v}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func completeOutputs() map[string]any {
	return map[string]any{
		"instance_id":        "i-0abc",
		"availability_zone":  "eu-west-1a",
		"data_volume_id":     "vol-0def",
		"host_public_key":    "ssh-ed25519 AAAAhost",
		"client_private_key": "-----BEGIN OPENSSH PRIVATE KEY-----\n...",
		"node_role_name":     "iterabase-env-acme",
		"security_group_id":  "sg-0123",
		"smtp_username":      "AKIAMAILUSER",
		"smtp_password":      "smtp-secret",
	}
}

func TestOutputs_Parse(t *testing.T) {
	w := fakeTerraform(t, outputJSON(completeOutputs()))
	o, err := w.outputs(context.Background())
	require.NoError(t, err)
	assert.Equal(t, &Outputs{
		InstanceID:       "i-0abc",
		AvailabilityZone: "eu-west-1a",
		DataVolumeID:     "vol-0def",
		HostPublicKey:    "ssh-ed25519 AAAAhost",
		ClientPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\n...",
		NodeRoleName:     "iterabase-env-acme",
		SecurityGroupID:  "sg-0123",
		SMTPUsername:     "AKIAMAILUSER",
		SMTPPassword:     "smtp-secret",
	}, o)
}

func TestOutputs_RejectMissingOrInvalid(t *testing.T) {
	missing := completeOutputs()
	delete(missing, "data_volume_id")
	_, err := fakeTerraform(t, outputJSON(missing)).outputs(context.Background())
	require.ErrorContains(t, err, `terraform output "data_volume_id" is missing`)

	empty := completeOutputs()
	empty["instance_id"] = ""
	_, err = fakeTerraform(t, outputJSON(empty)).outputs(context.Background())
	require.ErrorContains(t, err, `terraform output "instance_id" is not a non-empty string`)

	typed := completeOutputs()
	typed["security_group_id"] = []string{"sg-1"}
	_, err = fakeTerraform(t, outputJSON(typed)).outputs(context.Background())
	require.ErrorContains(t, err, `terraform output "security_group_id" is not a non-empty string`)
}

func TestSMTPSecret(t *testing.T) {
	doc, err := SMTPSecret("iterabase-system", "smtp-secret")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(doc), &got))
	assert.Equal(t, "iterabase-smtp", got["metadata"].(map[string]any)["name"])
	assert.Equal(t, "iterabase-system", got["metadata"].(map[string]any)["namespace"])
	assert.Equal(t, map[string]any{"smtp-password": "smtp-secret"}, got["stringData"])
}
