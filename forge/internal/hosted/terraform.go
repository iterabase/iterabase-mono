package hosted

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"
	"github.com/hashicorp/terraform-exec/tfexec"
)

// TerraformVersion is the pinned Terraform binary Forge downloads itself
// (hc-install verifies HashiCorp's signed checksums). It matches the version
// infra/ validates with (.github/inputs/remote-content.json ci_tools).
const TerraformVersion = "1.16.5"

//go:embed module/*.tf module/*.tftpl module/.terraform.lock.hcl
var moduleFS embed.FS

// ensureTerraform returns the pinned binary under binDir, installing it once.
func ensureTerraform(ctx context.Context, binDir string) (string, error) {
	dir := filepath.Join(binDir, "terraform-"+TerraformVersion)
	path := filepath.Join(dir, "terraform")
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	installer := &releases.ExactVersion{
		Product:    product.Terraform,
		Version:    version.Must(version.NewVersion(TerraformVersion)),
		InstallDir: dir,
	}
	installed, err := installer.Install(ctx)
	if err != nil {
		return "", fmt.Errorf("install terraform %s: %w", TerraformVersion, err)
	}
	return installed, nil
}

// writeModule materializes the embedded module (including its provider lock
// file) into a fresh working directory.
func writeModule(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return fs.WalkDir(moduleFS, "module", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("module", path)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := moduleFS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
}

// workspace is an initialized Terraform working directory for one environment.
type workspace struct {
	tf  *tfexec.Terraform
	dir string
}

// openWorkspace writes the module, points it at envs/<name>/terraform.tfstate,
// and initializes it with the Forge role's credentials.
func (s *Session) openWorkspace(ctx context.Context, stateDir string) (*workspace, error) {
	binDir, err := BinDir()
	if err != nil {
		return nil, err
	}
	bin, err := ensureTerraform(ctx, binDir)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(stateDir, "terraform")
	if err := writeModule(dir); err != nil {
		return nil, fmt.Errorf("write terraform module: %w", err)
	}
	tf, err := tfexec.NewTerraform(dir, bin)
	if err != nil {
		return nil, err
	}
	creds, err := s.AWS.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("retrieve the Forge role credentials: %w", err)
	}
	env := map[string]string{
		"AWS_ACCESS_KEY_ID":     creds.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY": creds.SecretAccessKey,
		"AWS_SESSION_TOKEN":     creds.SessionToken,
		"AWS_REGION":            s.Contract.Region,
		"TF_IN_AUTOMATION":      "1",
		"TF_PLUGIN_CACHE_DIR":   filepath.Join(binDir, "terraform-plugins"),
	}
	if err := os.MkdirAll(env["TF_PLUGIN_CACHE_DIR"], 0o700); err != nil {
		return nil, err
	}
	if err := tf.SetEnv(env); err != nil {
		return nil, err
	}
	if err := tf.Init(ctx,
		tfexec.BackendConfig("bucket="+s.Contract.StateBucket),
		tfexec.BackendConfig("key="+s.StateKey()),
		tfexec.BackendConfig("region="+s.Contract.Region),
		tfexec.Reconfigure(true),
	); err != nil {
		return nil, fmt.Errorf("terraform init: %w", err)
	}
	return &workspace{tf: tf, dir: dir}, nil
}

// StateKey is this environment's state object in the baseline state bucket.
func (s *Session) StateKey() string {
	return s.Contract.StatePrefix + s.Name + "/terraform.tfstate"
}

// varOpts are the module inputs: everything account-level from the contract,
// sizing from forge.yaml.
func (s *Session) vars(spec Spec) []string {
	c := s.Contract
	values := map[string]string{
		"name":                          s.Name,
		"region":                        c.Region,
		"env_tag_key":                   c.EnvTagKey,
		"vpc_id":                        c.VPCID,
		"subnet_id":                     c.SubnetIDs[0],
		"instance_type":                 spec.InstanceType,
		"root_volume_gib":               strconv.Itoa(spec.RootVolumeGiB),
		"data_volume_gib":               strconv.Itoa(spec.DataVolumeGiB),
		"node_role_path":                c.NodeRolePath,
		"node_permissions_boundary_arn": c.NodePermissionsBoundaryARN,
		"lb_controller_policy_arn":      c.LBControllerPolicyARN,
		"backup_bucket":                 c.BackupBucket,
		"backup_prefix":                 c.BackupPrefix,
		"backup_kms_key_arn":            c.BackupKMSKeyARN,
		"session_log_bucket":            c.SessionLogBucket,
		"session_log_group":             c.SessionLogGroup,
		"mail_identity_arn":             c.MailIdentityARN,
		"mail_user_boundary_arn":        c.MailUserBoundaryARN,
	}
	out := make([]string, 0, len(values))
	for k, v := range values {
		out = append(out, k+"="+v)
	}
	return out
}

func varOptions(vars []string) []tfexec.ApplyOption {
	opts := make([]tfexec.ApplyOption, 0, len(vars))
	for _, v := range vars {
		opts = append(opts, tfexec.Var(v))
	}
	return opts
}

func destroyVarOptions(vars []string) []tfexec.DestroyOption {
	opts := make([]tfexec.DestroyOption, 0, len(vars))
	for _, v := range vars {
		opts = append(opts, tfexec.Var(v))
	}
	return opts
}

// Outputs is what the environment module reports.
type Outputs struct {
	InstanceID       string
	AvailabilityZone string
	DataVolumeID     string
	HostPublicKey    string
	ClientPrivateKey string
	NodeRoleName     string
	SecurityGroupID  string
	SMTPUsername     string
	SMTPPassword     string
}

func (w *workspace) outputs(ctx context.Context) (*Outputs, error) {
	raw, err := w.tf.Output(ctx)
	if err != nil {
		return nil, fmt.Errorf("terraform output: %w", err)
	}
	str := func(key string) (string, error) {
		meta, ok := raw[key]
		if !ok {
			return "", fmt.Errorf("terraform output %q is missing", key)
		}
		var v string
		if err := json.Unmarshal(meta.Value, &v); err != nil || v == "" {
			return "", fmt.Errorf("terraform output %q is not a non-empty string", key)
		}
		return v, nil
	}
	o := &Outputs{}
	for key, dst := range map[string]*string{
		"instance_id":        &o.InstanceID,
		"availability_zone":  &o.AvailabilityZone,
		"data_volume_id":     &o.DataVolumeID,
		"host_public_key":    &o.HostPublicKey,
		"client_private_key": &o.ClientPrivateKey,
		"node_role_name":     &o.NodeRoleName,
		"security_group_id":  &o.SecurityGroupID,
		"smtp_username":      &o.SMTPUsername,
		"smtp_password":      &o.SMTPPassword,
	} {
		v, err := str(key)
		if err != nil {
			return nil, err
		}
		*dst = v
	}
	return o, nil
}
