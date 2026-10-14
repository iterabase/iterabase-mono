package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/artifacts"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/hosted"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/overlay"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/sshprovisioner"
	"github.com/nunocgoncalves/iterabase-mono/forge/internal/ssmsession"
)

// awsSSHUser is the Canonical Ubuntu image's default user (passwordless sudo).
const awsSSHUser = "ubuntu"

// resolveAWSTarget derives the host of a provisioned hosted environment from
// its Terraform state and wires SSH over SSM to it (DES-HOR-593-01).
func resolveAWSTarget(ctx context.Context, cfg *config.Cluster) (*target, error) {
	sess, err := hosted.Open(ctx, cfg.Metadata.Name, cfg.Spec.Provider.AWS)
	if err != nil {
		return nil, err
	}
	dir, err := environmentDir(cfg.Metadata.Name)
	if err != nil {
		return nil, err
	}
	outs, err := sess.Outputs(ctx, dir)
	if err != nil {
		return nil, err
	}
	return awsTarget(ctx, cfg, sess, dir, outs)
}

func environmentDir(name string) (string, error) {
	if err := artifacts.EnsureDir(name); err != nil {
		return "", err
	}
	return artifacts.Dir(name)
}

// awsTarget builds the resolved cluster: one host addressed by instance ID,
// the data volume's stable NVMe identity, and the kubelet provider ID the load
// balancer controller needs (DES-HOR-601-05). It also derives the hosted
// installation values and the DNS token Secret from the baseline contract.
func awsTarget(ctx context.Context, cfg *config.Cluster, sess *hosted.Session, dir string, outs *hosted.Outputs) (*target, error) {
	keyPath, trustPath, err := writeEnvironmentSSH(dir, outs)
	if err != nil {
		return nil, err
	}
	resolved := *cfg
	resolved.Spec.Hosts = []config.Host{{
		Address:      outs.InstanceID,
		SSHUser:      awsSSHUser,
		SSHKeyPath:   keyPath,
		SSHTrustFile: trustPath,
		Role:         config.RoleControlPlaneWorker,
		Labels:       map[string]string{},
		Taints:       []config.Taint{},
	}}
	resolved.Spec.DataStorage = config.DataStorage{Devices: []string{ebsDevice(outs.DataVolumeID)}}
	providerID := "--kubelet-arg=provider-id=aws:///" + outs.AvailabilityZone + "/" + outs.InstanceID
	resolved.Spec.K3s.ExtraArgs = append(withoutProviderID(cfg.Spec.K3s.ExtraArgs), providerID)
	if err := resolved.ValidateResolved(); err != nil {
		return nil, fmt.Errorf("resolved hosted environment: %w", err)
	}

	binDir, err := hosted.BinDir()
	if err != nil {
		return nil, err
	}
	plugin, err := ssmsession.EnsurePlugin(ctx, binDir)
	if err != nil {
		return nil, err
	}
	dialer := ssmsession.Dialer{API: ssm.NewFromConfig(sess.AWS), Region: sess.Contract.Region, Plugin: plugin}
	dial := func(ctx context.Context, _, addr string, sshCfg *ssh.ClientConfig) (*ssh.Client, error) {
		conn, err := dialer.Dial(ctx, outs.InstanceID)
		if err != nil {
			return nil, err
		}
		c, chans, reqs, err := ssh.NewClientConn(conn, addr, sshCfg)
		if err != nil {
			conn.Close()
			return nil, err
		}
		return ssh.NewClient(c, chans, reqs), nil
	}

	values, err := sess.Contract.InstallationValues(cfg.Metadata.Name, outs.SMTPUsername)
	if err != nil {
		return nil, err
	}
	mirror, err := sshprovisioner.ParseECRMirror(sess.Contract.ImageRegistryPrefix, sess.Contract.Region)
	if err != nil {
		return nil, err
	}
	t := &target{
		Prepare: func(ctx context.Context, p *sshprovisioner.SSHProvisioner) error {
			return p.EnsureECRMirror(ctx, mirror)
		},
		Cluster:    &resolved,
		SSHOptions: []sshprovisioner.Option{sshprovisioner.WithDial(dial)},
		ValueFiles: []overlay.ValueFile{{Name: "hosted-installation", Content: values}},
	}
	if ns := resolved.Spec.Chart.Namespace; ns != "" {
		secret, err := sess.Contract.CloudflareSecret(ns)
		if err != nil {
			return nil, err
		}
		nsManifest, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]string{"name": ns}})
		if err != nil {
			return nil, err
		}
		smtp, err := hosted.SMTPSecret(ns, outs.SMTPPassword)
		if err != nil {
			return nil, err
		}
		t.Manifests = []string{string(nsManifest), secret, smtp}
	}
	return t, nil
}

// writeEnvironmentSSH writes the client key and the trust file from the
// environment's IAM-protected state. The host key was generated with the
// environment and installed by user-data, so trust never comes from the
// network peer (DES-HOR-593-02).
func writeEnvironmentSSH(dir string, outs *hosted.Outputs) (keyPath, trustPath string, err error) {
	sshDir := filepath.Join(dir, "ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return "", "", err
	}
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(outs.HostPublicKey))
	if err != nil {
		return "", "", fmt.Errorf("environment host public key is malformed: %w", err)
	}
	fields := strings.Fields(string(ssh.MarshalAuthorizedKey(hostKey)))
	keyPath = filepath.Join(sshDir, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte(outs.ClientPrivateKey), 0o600); err != nil {
		return "", "", err
	}
	trustPath = filepath.Join(sshDir, "known_hosts")
	line := outs.InstanceID + " " + fields[0] + " " + fields[1] + " forge-generated\n"
	if err := os.WriteFile(trustPath, []byte(line), 0o600); err != nil {
		return "", "", err
	}
	return keyPath, trustPath, nil
}

// ebsDevice is the stable whole-disk identity of an EBS volume on Nitro
// instances: the NVMe serial is the volume ID without its hyphen.
func ebsDevice(volumeID string) string {
	return "/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_" + strings.Replace(volumeID, "-", "", 1)
}

func withoutProviderID(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if !strings.HasPrefix(a, "--kubelet-arg=provider-id=") {
			out = append(out, a)
		}
	}
	return out
}

// runAWSInit provisions (or converges) the environment, then bootstraps it
// over SSH-over-SSM and applies the release and overlay. Re-running it is
// idempotent.
func runAWSInit(cmd *cobra.Command, cfg *config.Cluster) error {
	ctx := context.Background()
	log := newLogger(cmd)
	sess, err := hosted.Open(ctx, cfg.Metadata.Name, cfg.Spec.Provider.AWS)
	if err != nil {
		return err
	}
	dir, err := environmentDir(cfg.Metadata.Name)
	if err != nil {
		return err
	}
	log.Info("provisioning hosted environment", "name", cfg.Metadata.Name, "state", "s3://"+sess.Contract.StateBucket+"/"+sess.StateKey())
	outs, err := sess.Provision(ctx, dir, hosted.SpecFrom(cfg.Spec.Provider.AWS))
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "environment %s: instance %s (%s), data volume %s\n", cfg.Metadata.Name, outs.InstanceID, outs.AvailabilityZone, outs.DataVolumeID)
	if err := waitForSSMAgent(ctx, cmd, sess, outs.InstanceID); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "app: https://%s  gateway: https://%s\n", sess.Contract.AppHost(cfg.Metadata.Name), sess.Contract.GatewayHost(cfg.Metadata.Name))
	return applyInstallation(ctx, cmd, cfg, applyOptsFromFlags(cmd), false)
}

// waitForSSMAgent waits until the instance's SSM agent is online; SSH over
// SSM cannot start before it.
func waitForSSMAgent(ctx context.Context, cmd *cobra.Command, sess *hosted.Session, instanceID string) error {
	api := ssm.NewFromConfig(sess.AWS)
	deadline := time.Now().Add(10 * time.Minute)
	for {
		out, err := api.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{
			Filters: []ssmtypes.InstanceInformationStringFilter{{Key: aws.String("InstanceIds"), Values: []string{instanceID}}},
		})
		if err != nil {
			return fmt.Errorf("ssm agent status: %w", err)
		}
		for _, info := range out.InstanceInformationList {
			if info.PingStatus == ssmtypes.PingStatusOnline {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ssm agent on %s is not online after 10m", instanceID)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "waiting for the SSM agent on %s...\n", instanceID)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

// runAWSDestroy tears the environment down in the HOR-593 order and proves it
// with a zero-result tag sweep:
//  1. delete every LoadBalancer Service so the controller removes its NLB,
//     target groups and security groups, and wait until they are gone;
//  2. terraform destroy;
//  3. purge the environment's backup prefix;
//  4. revoke the central-gateway certificate (HOR-661);
//  5. purge the environment's state prefix;
//  6. tag sweep: zero remaining tagged resources.
func runAWSDestroy(cmd *cobra.Command, cfg *config.Cluster) error {
	ctx := context.Background()
	yes, _ := cmd.Flags().GetBool("yes")
	if !yes && !confirm(cmd, fmt.Sprintf("DESTROY hosted environment %s: its instance, data volume, load balancer, backups and state? This cannot be undone.", cfg.Metadata.Name)) {
		return fmt.Errorf("aborted")
	}
	out := cmd.OutOrStdout()
	sess, err := hosted.Open(ctx, cfg.Metadata.Name, cfg.Spec.Provider.AWS)
	if err != nil {
		return err
	}
	dir, err := environmentDir(cfg.Metadata.Name)
	if err != nil {
		return err
	}
	spec := hosted.SpecFrom(cfg.Spec.Provider.AWS)

	outs, err := sess.Outputs(ctx, dir)
	switch {
	case err == nil:
		if err := deleteLoadBalancers(ctx, cfg, sess, dir, outs); err != nil {
			return err
		}
		fmt.Fprintln(out, "load balancer removed")
		if err := sess.DestroyResources(ctx, dir, spec); err != nil {
			return err
		}
		fmt.Fprintln(out, "terraform destroy complete")
	case errors.Is(err, hosted.ErrNotProvisioned):
		fmt.Fprintln(out, "no environment state; continuing with prefix purge and sweep")
	default:
		return err
	}

	n, err := sess.PurgeBackups(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "backup prefix purged (%d object versions)\n", n)
	if err := revokeInstallationCertificate(ctx, cfg); err != nil {
		return err
	}
	n, err = sess.PurgeState(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "state prefix purged (%d object versions)\n", n)
	if err := sess.Sweep(ctx, 10*time.Minute); err != nil {
		return err
	}
	fmt.Fprintf(out, "tag sweep %s=%s: 0 resources\n", sess.Contract.EnvTagKey, cfg.Metadata.Name)
	_ = os.RemoveAll(dir)
	fmt.Fprintln(out, "destroyed")
	return nil
}

// deleteLoadBalancers removes every LoadBalancer Service and waits until the
// controller has removed the NLB, target groups and its security groups.
func deleteLoadBalancers(ctx context.Context, cfg *config.Cluster, sess *hosted.Session, dir string, outs *hosted.Outputs) error {
	t, err := awsTarget(ctx, cfg, sess, dir, outs)
	if err != nil {
		return err
	}
	defer t.Close()
	p, err := sshprovisioner.New(t.Cluster.Spec.Hosts[0], t.SSHOptions...)
	if err != nil {
		return err
	}
	defer p.Close()
	if _, err := p.DeleteLoadBalancerServices(ctx); err != nil {
		return err
	}
	return sess.WaitForNoLoadBalancers(ctx, outs.SecurityGroupID, 10*time.Minute)
}

// revokeInstallationCertificate is completed by the HOR-661 integration.
func revokeInstallationCertificate(context.Context, *config.Cluster) error { return nil }

func newTunnelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Forward a hosted environment's Kubernetes API to localhost over SSM",
		Long: `tunnel opens an SSH-over-SSM connection to a provider.aws environment and
forwards 127.0.0.1:<port> to its Kubernetes API, which the kubeconfig from
'forge kubeconfig' points at. No inbound port is ever opened on the instance.`,
		RunE: runTunnel,
	}
	cmd.Flags().Int("port", 6443, "local port")
	return cmd
}

func runTunnel(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	if cfg.Spec.Provider.AWS == nil {
		return fmt.Errorf("forge tunnel is for provider.aws environments; other hosts expose their API directly")
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	t, p, err := connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer t.Close()
	defer p.Close()
	port, _ := cmd.Flags().GetInt("port")
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	defer ln.Close()
	fmt.Fprintf(cmd.OutOrStdout(), "forwarding 127.0.0.1:%d -> %s:6443 over SSM; Ctrl-C to stop\n", port, t.Cluster.Spec.Hosts[0].Address)
	return p.ForwardLocal(ctx, ln, "127.0.0.1:6443")
}
