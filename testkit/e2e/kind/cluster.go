// Package kind owns fresh local Kind cluster lifecycle and kubeconfig isolation.
package kind

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
)

var (
	prefixPattern      = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Manager creates unique clusters through a testable process seam.
type Manager struct {
	Executor process.Executor
	TempRoot string
	Now      func() time.Time
	Random   io.Reader
}

// Cluster is an isolated Kind handle. Delete is idempotent.
type Cluster struct {
	Name       string
	Kubeconfig string

	executor     process.Executor
	tempDir      string
	owned        bool
	mu           sync.Mutex
	deleted      bool
	deleteErr    error
	lvmPrepared  bool
	lvmNode      string
	lvmNamespace string
	lvmRelease   string
	lvmContract  LVMStorageContract
}

// Create provisions a fresh cluster whose name and kubeconfig cannot collide
// with a stale local or CI run.
func (manager Manager) Create(ctx context.Context, prefix string) (*Cluster, error) {
	if manager.Executor == nil {
		return nil, fmt.Errorf("kind manager has no process executor")
	}
	name, err := manager.uniqueName(prefix)
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp(manager.TempRoot, "iterabase-e2e-kind-")
	if err != nil {
		return nil, err
	}
	kubeconfig, err := filepath.Abs(filepath.Join(tempDir, "kubeconfig.yaml"))
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, err
	}
	args := []string{"create", "cluster", "--name", name, "--kubeconfig", kubeconfig, "--wait", "120s"}
	config, err := registryAuthConfig(os.Getenv("DOCKERHUB_USERNAME"), os.Getenv("DOCKERHUB_TOKEN"))
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, err
	}
	if config != "" {
		configPath := filepath.Join(tempDir, "kind-config.yaml")
		if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
			_ = os.RemoveAll(tempDir)
			return nil, err
		}
		args = append(args, "--config", configPath)
	}
	_, err = manager.Executor.Run(ctx, process.Command{
		Name: "kind", Args: args,
		Timeout: 5 * time.Minute, OutputName: "kind-create-" + name + ".log",
	})
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_, deleteErr := manager.Executor.Run(cleanupCtx, process.Command{
			Name: "kind", Args: []string{"delete", "cluster", "--name", name},
			Timeout: 5 * time.Minute, OutputName: "kind-delete-after-create-failure-" + name + ".log",
		})
		_ = os.RemoveAll(tempDir)
		if deleteErr != nil {
			return nil, fmt.Errorf("create kind cluster %s: %w (best-effort delete also failed: %v)", name, err, deleteErr)
		}
		return nil, fmt.Errorf("create kind cluster %s: %w", name, err)
	}
	return &Cluster{Name: name, Kubeconfig: kubeconfig, executor: manager.Executor, tempDir: tempDir, owned: true}, nil
}

var registryCredential = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// registryAuthConfig authenticates the Kind nodes' own containerd to Docker
// Hub. Chart images are pulled inside the nodes, so a docker login on the
// runner does not cover them, and anonymous pulls share the runner IP's rate
// limit. Without credentials the cluster is created unchanged.
func registryAuthConfig(username, token string) (string, error) {
	if username == "" && token == "" {
		return "", nil
	}
	if !registryCredential.MatchString(username) || !registryCredential.MatchString(token) {
		return "", fmt.Errorf("DOCKERHUB_USERNAME and DOCKERHUB_TOKEN must both be set to plain credential strings")
	}
	return fmt.Sprintf(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri".registry.configs."registry-1.docker.io".auth]
      username = %q
      password = %q
`, username, token), nil
}

// Use wraps an exact existing kubeconfig without taking lifecycle ownership.
func Use(name, kubeconfig string, executor process.Executor) (*Cluster, error) {
	absolute, err := filepath.Abs(kubeconfig)
	if err != nil {
		return nil, err
	}
	if name == "" || executor == nil {
		return nil, fmt.Errorf("existing cluster requires a name and executor")
	}
	return &Cluster{Name: name, Kubeconfig: absolute, executor: executor}, nil
}

// LoadImage loads an explicitly named local image into every Kind node.
func (cluster *Cluster) LoadImage(ctx context.Context, image string) error {
	if image == "" {
		return fmt.Errorf("kind image identity is empty")
	}
	_, err := cluster.executor.Run(ctx, process.Command{
		Name: "kind", Args: []string{"load", "docker-image", "--name", cluster.Name, image},
		Timeout: 10 * time.Minute, OutputName: "kind-load-" + safeFileName(image) + ".log",
	})
	return err
}

// ImportedImageIdentity binds the supplied archive identity to the immutable
// manifest digest that CRI reports after Kind import.
type ImportedImageIdentity struct {
	ConfigDigest  string
	RuntimeDigest string
	Labels        map[string]string
}

// ImportImageArchive restores one supplied image archive into the runner
// daemon after cluster creation, transports that exact reference into every
// Kind node, and proves that each node retained the expected image config. The
// returned runtime digest is the immutable manifest identity CRI later reports
// for Pods; it is distinct from a docker config or multi-platform index digest.
func (cluster *Cluster) ImportImageArchive(ctx context.Context, archive, image, expectedConfigDigest string) (ImportedImageIdentity, error) {
	if image == "" {
		return ImportedImageIdentity{}, fmt.Errorf("kind image identity is empty")
	}
	if !imageDigestPattern.MatchString(expectedConfigDigest) {
		return ImportedImageIdentity{}, fmt.Errorf("resolved image %s has invalid config digest %q", image, expectedConfigDigest)
	}
	info, err := os.Stat(archive)
	if err != nil {
		return ImportedImageIdentity{}, fmt.Errorf("resolved image archive %q: %w", archive, err)
	}
	if !info.Mode().IsRegular() {
		return ImportedImageIdentity{}, fmt.Errorf("resolved image archive %q is not a regular file", archive)
	}
	if _, err := cluster.executor.Run(ctx, process.Command{
		Name: "docker", Args: []string{"load", "-i", archive},
		Timeout: 10 * time.Minute, OutputName: "docker-load-" + safeFileName(image) + ".log",
	}); err != nil {
		return ImportedImageIdentity{}, fmt.Errorf("restore resolved image %s: %w", image, err)
	}
	if err := cluster.LoadImage(ctx, image); err != nil {
		return ImportedImageIdentity{}, fmt.Errorf("import resolved image %s into Kind: %w", image, err)
	}
	nodesResult, err := cluster.executor.Run(ctx, process.Command{
		Name: "kind", Args: []string{"get", "nodes", "--name", cluster.Name},
		Timeout: 30 * time.Second, OutputName: "kind-nodes-" + safeFileName(image) + ".log",
	})
	if err != nil {
		return ImportedImageIdentity{}, fmt.Errorf("resolve Kind nodes after importing %s: %w", image, err)
	}
	if strings.TrimSpace(nodesResult.Output) == "" {
		return ImportedImageIdentity{}, fmt.Errorf("resolve Kind nodes after importing %s: no nodes returned", image)
	}

	identity := ImportedImageIdentity{ConfigDigest: expectedConfigDigest}
	for _, node := range strings.Fields(nodesResult.Output) {
		inspection, inspectErr := cluster.executor.Run(ctx, process.Command{
			Name: "docker", Args: []string{"exec", node, "crictl", "inspecti", image},
			Timeout: 30 * time.Second, OutputName: "kind-inspect-" + safeFileName(node+"-"+image) + ".json",
		})
		if inspectErr != nil {
			return ImportedImageIdentity{}, fmt.Errorf("inspect imported image %s on %s: %w", image, node, inspectErr)
		}
		var runtimeImage struct {
			Status struct {
				ID       string   `json:"id"`
				RepoTags []string `json:"repoTags"`
			} `json:"status"`
			Info struct {
				ImageSpec struct {
					Config struct {
						Labels map[string]string `json:"Labels"`
					} `json:"config"`
				} `json:"imageSpec"`
			} `json:"info"`
		}
		if err := json.Unmarshal([]byte(inspection.Output), &runtimeImage); err != nil {
			return ImportedImageIdentity{}, fmt.Errorf("decode imported image %s on %s: %w", image, node, err)
		}
		if runtimeImage.Status.ID != expectedConfigDigest {
			return ImportedImageIdentity{}, fmt.Errorf("imported image %s on %s config digest %q != %q", image, node, runtimeImage.Status.ID, expectedConfigDigest)
		}
		if len(runtimeImage.Status.RepoTags) != 1 {
			return ImportedImageIdentity{}, fmt.Errorf("imported image %s on %s has ambiguous runtime tags: %v", image, node, runtimeImage.Status.RepoTags)
		}
		manifest, manifestErr := cluster.executor.Run(ctx, process.Command{
			Name: "docker", Args: []string{"exec", node, "ctr", "-n", "k8s.io", "images", "list"},
			Timeout: 30 * time.Second, OutputName: "kind-manifest-" + safeFileName(node+"-"+image) + ".txt",
		})
		if manifestErr != nil {
			return ImportedImageIdentity{}, fmt.Errorf("list imported manifests for %s on %s: %w", image, node, manifestErr)
		}
		runtimeDigests := make(map[string]struct{})
		for _, line := range strings.Split(manifest.Output, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[0] == runtimeImage.Status.RepoTags[0] && imageDigestPattern.MatchString(fields[2]) {
				runtimeDigests[fields[2]] = struct{}{}
			}
		}
		if len(runtimeDigests) != 1 {
			return ImportedImageIdentity{}, fmt.Errorf("imported image %s on %s has ambiguous manifest identity", image, node)
		}
		var runtimeDigest string
		for digest := range runtimeDigests {
			runtimeDigest = digest
		}
		if identity.RuntimeDigest != "" && identity.RuntimeDigest != runtimeDigest {
			return ImportedImageIdentity{}, fmt.Errorf("imported image %s runtime digest differs across Kind nodes", image)
		}
		identity.RuntimeDigest = runtimeDigest
		if identity.Labels == nil {
			identity.Labels = runtimeImage.Info.ImageSpec.Config.Labels
		}
	}
	return identity, nil
}

// Delete tears down owned infrastructure and removes its temporary kubeconfig.
// Best-effort LVM cleanup and mandatory Kind deletion receive independent
// bounded contexts so one exhausted phase cannot suppress the next. A failed
// Kind teardown remains retryable; terminal deletion is recorded only after the
// cluster and temporary state are actually gone.
func (cluster *Cluster) Delete(ctx context.Context) error {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	if cluster.deleted {
		return nil
	}
	base := context.WithoutCancel(ctx)
	if cluster.owned {
		if cluster.lvmPrepared {
			cleanupCtx, cancelCleanup := context.WithTimeout(base, 10*time.Minute)
			_ = cluster.cleanupLVMStorage(cleanupCtx)
			cancelCleanup()
		}
		deleteCtx, cancelDelete := context.WithTimeout(base, 6*time.Minute)
		_, deleteErr := cluster.executor.Run(deleteCtx, process.Command{
			Name: "kind", Args: []string{"delete", "cluster", "--name", cluster.Name},
			Timeout: 5 * time.Minute, OutputName: "kind-delete-" + cluster.Name + ".log",
		})
		cancelDelete()
		if deleteErr != nil {
			cluster.deleteErr = deleteErr
			return deleteErr
		}
	}
	if cluster.tempDir != "" {
		if err := os.RemoveAll(cluster.tempDir); err != nil {
			cluster.deleteErr = err
			return err
		}
	}
	cluster.deleted = true
	cluster.deleteErr = nil
	return nil
}

func (manager Manager) uniqueName(prefix string) (string, error) {
	if !prefixPattern.MatchString(prefix) {
		return "", fmt.Errorf("kind cluster prefix %q must be lowercase DNS-safe", prefix)
	}
	now := manager.Now
	if now == nil {
		now = time.Now
	}
	random := manager.Random
	if random == nil {
		random = rand.Reader
	}
	var entropy [4]byte
	if _, err := io.ReadFull(random, entropy[:]); err != nil {
		return "", err
	}
	suffix := fmt.Sprintf("-%x-%s", now().UnixNano(), hex.EncodeToString(entropy[:]))
	maxPrefix := 63 - len(suffix)
	if len(prefix) > maxPrefix {
		prefix = strings.TrimRight(prefix[:maxPrefix], "-")
	}
	return prefix + suffix, nil
}

func safeFileName(value string) string {
	value = strings.NewReplacer("/", "-", ":", "-", "@", "-").Replace(value)
	if len(value) > 80 {
		value = value[:80]
	}
	return value
}
