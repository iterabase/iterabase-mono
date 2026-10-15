package hosted

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	tagging "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	taggingtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/config"
)

// Spec is the environment sizing from forge.yaml.
type Spec struct {
	InstanceType  string
	RootVolumeGiB int
	DataVolumeGiB int
}

// SpecFrom reads the sizing from provider.aws.
func SpecFrom(p *config.AWSProvider) Spec {
	return Spec{InstanceType: p.InstanceType, RootVolumeGiB: p.RootVolumeGiB, DataVolumeGiB: p.DataVolumeGiB}
}

// BinDir holds the pinned binaries Forge downloads (Terraform, the SSM plugin).
func BinDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".forge", "bin"), nil
}

// Provision creates or converges the environment's resources. Re-running it
// is idempotent: Terraform applies only differences, and the instance is never
// replaced for a newer image (the module ignores AMI and user-data drift).
func (s *Session) Provision(ctx context.Context, stateDir string, spec Spec) (*Outputs, error) {
	if !s.Contract.AllowsInstanceType(spec.InstanceType) {
		return nil, fmt.Errorf("provider.aws.instanceType %s is not in the baseline allowed-instance-types %v", spec.InstanceType, s.Contract.AllowedInstanceTypes)
	}
	w, err := s.openWorkspace(ctx, stateDir)
	if err != nil {
		return nil, err
	}
	if err := w.tf.Apply(ctx, varOptions(s.vars(spec))...); err != nil {
		return nil, fmt.Errorf("terraform apply: %w", err)
	}
	return w.outputs(ctx)
}

// ErrNotProvisioned means the environment has no state: forge init
// --provider aws has not run, or the environment was destroyed.
var ErrNotProvisioned = errors.New("hosted environment is not provisioned")

// Outputs reads the environment's current outputs from its state.
func (s *Session) Outputs(ctx context.Context, stateDir string) (*Outputs, error) {
	exists, err := s.stateExists(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s has no state at s3://%s/%s; run forge init --provider aws", ErrNotProvisioned, s.Name, s.Contract.StateBucket, s.StateKey())
	}
	w, err := s.openWorkspace(ctx, stateDir)
	if err != nil {
		return nil, err
	}
	return w.outputs(ctx)
}

// DestroyResources runs terraform destroy. The load balancer the in-cluster
// controller created must already be gone (HOR-593 teardown order), because
// Terraform does not know about it.
func (s *Session) DestroyResources(ctx context.Context, stateDir string, spec Spec) error {
	w, err := s.openWorkspace(ctx, stateDir)
	if err != nil {
		return err
	}
	if err := w.tf.Destroy(ctx, destroyVarOptions(s.vars(spec))...); err != nil {
		return fmt.Errorf("terraform destroy: %w", err)
	}
	return nil
}

func (s *Session) stateExists(ctx context.Context) (bool, error) {
	out, err := s3.NewFromConfig(s.AWS).ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.Contract.StateBucket),
		Prefix: aws.String(s.StateKey()),
	})
	if err != nil {
		return false, fmt.Errorf("check state s3://%s/%s: %w", s.Contract.StateBucket, s.StateKey(), err)
	}
	for _, o := range out.Contents {
		if aws.ToString(o.Key) == s.StateKey() {
			return true, nil
		}
	}
	return false, nil
}

// PurgeState deletes every version and delete marker of this environment's
// state prefix. It runs only after a successful destroy.
func (s *Session) PurgeState(ctx context.Context) (int, error) {
	return purgePrefix(ctx, s3.NewFromConfig(s.AWS), s.Contract.StateBucket, s.Contract.StatePrefix+s.Name+"/")
}

// PurgeBackups deletes every version and delete marker of this environment's
// backup prefix (HOR-604 data).
func (s *Session) PurgeBackups(ctx context.Context) (int, error) {
	return purgePrefix(ctx, s3.NewFromConfig(s.AWS), s.Contract.BackupBucket, s.Contract.BackupPrefix+s.Name+"/")
}

// purgePrefix removes all object versions under prefix. The bucket is
// versioned, so deleting only current objects would leave the data behind.
func purgePrefix(ctx context.Context, api *s3.Client, bucket, prefix string) (int, error) {
	if !strings.HasSuffix(prefix, "/") || strings.Count(prefix, "/") < 2 {
		return 0, fmt.Errorf("refusing to purge broad prefix %q", prefix)
	}
	deleted := 0
	pager := s3.NewListObjectVersionsPaginator(api, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return deleted, fmt.Errorf("list s3://%s/%s: %w", bucket, prefix, err)
		}
		var ids []s3types.ObjectIdentifier
		for _, v := range page.Versions {
			ids = append(ids, s3types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, m := range page.DeleteMarkers {
			ids = append(ids, s3types.ObjectIdentifier{Key: m.Key, VersionId: m.VersionId})
		}
		for start := 0; start < len(ids); start += 1000 {
			end := min(start+1000, len(ids))
			out, err := api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(bucket),
				Delete: &s3types.Delete{Objects: ids[start:end], Quiet: aws.Bool(true)},
			})
			if err != nil {
				return deleted, fmt.Errorf("delete s3://%s/%s: %w", bucket, prefix, err)
			}
			if len(out.Errors) > 0 {
				return deleted, fmt.Errorf("delete s3://%s/%s: %s: %s", bucket, prefix, aws.ToString(out.Errors[0].Key), aws.ToString(out.Errors[0].Message))
			}
			deleted += end - start
		}
	}
	return deleted, nil
}

// TaggedResources lists every live resource carrying this environment's tag:
// the tagging API (EC2, ELB, ...) with terminated instances removed, plus the
// environment's IAM role and instance profile, which that API does not cover.
func (s *Session) TaggedResources(ctx context.Context) ([]string, error) {
	var arns []string
	pager := tagging.NewGetResourcesPaginator(tagging.NewFromConfig(s.AWS), &tagging.GetResourcesInput{
		TagFilters: []taggingtypes.TagFilter{{Key: aws.String(s.Contract.EnvTagKey), Values: []string{s.Name}}},
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("tag sweep: %w", err)
		}
		for _, m := range page.ResourceTagMappingList {
			arns = append(arns, aws.ToString(m.ResourceARN))
		}
	}
	live, err := s.withoutTerminatedInstances(ctx, arns)
	if err != nil {
		return nil, err
	}
	iamLeftovers, err := s.iamLeftovers(ctx)
	if err != nil {
		return nil, err
	}
	live = append(live, iamLeftovers...)
	sort.Strings(live)
	return live, nil
}

// withoutTerminatedInstances drops instances EC2 still reports for a while
// after termination; any other state counts as live.
func (s *Session) withoutTerminatedInstances(ctx context.Context, arns []string) ([]string, error) {
	var ids []string
	byID := map[string]string{}
	var out []string
	for _, arn := range arns {
		if i := strings.Index(arn, ":instance/"); i >= 0 {
			id := arn[i+len(":instance/"):]
			ids = append(ids, id)
			byID[id] = arn
			continue
		}
		out = append(out, arn)
	}
	if len(ids) == 0 {
		return out, nil
	}
	resp, err := ec2.NewFromConfig(s.AWS).DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
	if err != nil {
		return nil, fmt.Errorf("tag sweep instances: %w", err)
	}
	seen := map[string]bool{}
	for _, r := range resp.Reservations {
		for _, inst := range r.Instances {
			id := aws.ToString(inst.InstanceId)
			seen[id] = true
			if inst.State != nil && inst.State.Name == ec2types.InstanceStateNameTerminated {
				continue
			}
			out = append(out, byID[id])
		}
	}
	for _, id := range ids {
		if !seen[id] {
			out = append(out, byID[id])
		}
	}
	return out, nil
}

func (s *Session) iamLeftovers(ctx context.Context) ([]string, error) {
	api := iam.NewFromConfig(s.AWS)
	name := "iterabase-env-" + s.Name
	var left []string
	if out, err := api.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)}); err == nil {
		left = append(left, aws.ToString(out.Role.Arn))
	} else if !isNoSuchEntity(err) {
		return nil, fmt.Errorf("tag sweep role %s: %w", name, err)
	}
	if out, err := api.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(name)}); err == nil {
		left = append(left, aws.ToString(out.InstanceProfile.Arn))
	} else if !isNoSuchEntity(err) {
		return nil, fmt.Errorf("tag sweep instance profile %s: %w", name, err)
	}
	return left, nil
}

func isNoSuchEntity(err error) bool {
	var nse *iamtypes.NoSuchEntityException
	return errors.As(err, &nse)
}

// WaitForNoLoadBalancers waits until no load balancer, target group or
// security group other than the Terraform-managed node group (nodeSG) is tagged
// for this environment. The AWS Load Balancer Controller removes its NLB,
// target groups and security groups after its LoadBalancer Service is deleted.
func (s *Session) WaitForNoLoadBalancers(ctx context.Context, nodeSG string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		left, err := s.TaggedResources(ctx)
		if err != nil {
			return err
		}
		var lbs []string
		for _, arn := range left {
			controllerSG := strings.Contains(arn, ":security-group/") && !strings.HasSuffix(arn, "/"+nodeSG)
			if strings.Contains(arn, ":elasticloadbalancing:") || controllerSG {
				lbs = append(lbs, arn)
			}
		}
		if len(lbs) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("load balancer resources still present after %s: %s", timeout, strings.Join(lbs, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// Sweep waits until the tag sweep finds nothing; that zero result is the
// deletion evidence. The tagging API can lag deletions briefly.
func (s *Session) Sweep(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		left, err := s.TaggedResources(ctx)
		if err != nil {
			return err
		}
		if len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tag sweep for %s=%s still finds %d resources: %s", s.Contract.EnvTagKey, s.Name, len(left), strings.Join(left, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}
