package kind

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
)

type fakeExecutor struct {
	mu        sync.Mutex
	commands  []process.Command
	failNext  bool
	outputFor func(process.Command) string
}

func (executor *fakeExecutor) Run(_ context.Context, command process.Command) (process.Result, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.commands = append(executor.commands, command)
	if executor.failNext {
		executor.failNext = false
		return process.Result{}, errors.New("forced create failure")
	}
	result := process.Result{}
	if executor.outputFor != nil {
		result.Output = executor.outputFor(command)
	}
	return result, nil
}

func TestKindDataVolumeGroupFitsLargestThickClaimSet(t *testing.T) {
	const (
		largestChartClaimSet = 70 << 30
		fixtureHeadroom      = 20 << 30
	)
	if kindDataVolumeGroupSizeBytes < largestChartClaimSet+fixtureHeadroom {
		t.Fatalf("Kind data VG = %d bytes, want at least %d for thick chart claims plus fixture headroom", kindDataVolumeGroupSizeBytes, largestChartClaimSet+fixtureHeadroom)
	}
}

func TestLVMStorageHelmArgsUseTheKindKubeletRegistrationPath(t *testing.T) {
	args := lvmStorageHelmArgs("storage-lvm-storage", "/tmp/chart", "/tmp/kubeconfig", "iterabase-system")
	if !slices.Contains(args, "lvm-localpv.global.kubeletDir=/var/lib/kubelet") {
		t.Fatalf("LVM storage Helm args do not override the K3s kubelet path for Kind: %v", args)
	}
	if !slices.Contains(args, "agentpool.authorizedManagerIdentity=system:serviceaccount:iterabase-system:storage-control-plane-manager") {
		t.Fatalf("LVM storage Helm args do not carry the exact control-plane manager SA identity for admission (DES-HOR-545-01): %v", args)
	}
	for _, arg := range args {
		if strings.Contains(arg, "/var/lib/rancher/k3s") {
			t.Fatalf("Kind Helm args retained the K3s kubelet registration path: %v", args)
		}
	}
}

func TestValidateLVMCSINodeRegistrationFailsClosed(t *testing.T) {
	contract := testLVMStorageContract()
	valid := func() map[string]any {
		return map[string]any{"spec": map[string]any{"drivers": []any{
			map[string]any{"name": "other.csi.example", "nodeID": "node-a", "topologyKeys": []string{"example.com/node"}},
			map[string]any{"name": contract.Provisioner, "nodeID": "node-a", "topologyKeys": []string{contract.NodeTopologyKey, "kubernetes.io/hostname"}},
		}}}
	}
	marshal := func(value map[string]any) []byte {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if err := ValidateLVMCSINodeRegistration(marshal(valid()), "node-a", contract); err != nil {
		t.Fatalf("valid OpenEBS LVM CSINode registration failed: %v", err)
	}

	for name, mutate := range map[string]func(map[string]any){
		"missing": func(value map[string]any) {
			value["spec"].(map[string]any)["drivers"] = []any{}
		},
		"wrong node": func(value map[string]any) {
			value["spec"].(map[string]any)["drivers"].([]any)[1].(map[string]any)["nodeID"] = "node-b"
		},
		"missing driver topology": func(value map[string]any) {
			value["spec"].(map[string]any)["drivers"].([]any)[1].(map[string]any)["topologyKeys"] = []string{"kubernetes.io/hostname"}
		},
		"extra topology": func(value map[string]any) {
			value["spec"].(map[string]any)["drivers"].([]any)[1].(map[string]any)["topologyKeys"] = []string{contract.NodeTopologyKey, "kubernetes.io/hostname", "example.com/zone"}
		},
		"duplicate": func(value map[string]any) {
			drivers := value["spec"].(map[string]any)["drivers"].([]any)
			value["spec"].(map[string]any)["drivers"] = append(drivers, map[string]any{"name": contract.Provisioner, "nodeID": "node-a", "topologyKeys": []string{contract.NodeTopologyKey, "kubernetes.io/hostname"}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateLVMCSINodeRegistration(marshal(func() map[string]any { value := valid(); mutate(value); return value }()), "node-a", contract); err == nil {
				t.Fatalf("%s CSINode drift unexpectedly passed", name)
			}
		})
	}
}

func TestCreateFailureStillAttemptsClusterDeletion(t *testing.T) {
	t.Parallel()
	executor := &fakeExecutor{failNext: true}
	manager := Manager{
		Executor: executor, TempRoot: t.TempDir(),
		Now: func() time.Time { return time.Unix(0, 42) }, Random: bytes.NewReader([]byte("abcd")),
	}
	if _, err := manager.Create(context.Background(), "failed-e2e"); err == nil {
		t.Fatal("forced create failure unexpectedly passed")
	}
	if len(executor.commands) != 2 || executor.commands[1].Args[0] != "delete" {
		t.Fatalf("create failure did not attempt delete: %+v", executor.commands)
	}
}

func TestDownloadedRuntimeArtifactRequiresPostCreateClusterImport(t *testing.T) {
	t.Parallel()
	configDigest := "sha256:" + strings.Repeat("a", 64)
	runtimeDigest := "sha256:" + strings.Repeat("b", 64)
	executor := &fakeExecutor{outputFor: func(command process.Command) string {
		if command.Name == "kind" && slices.Equal(command.Args, []string{"get", "nodes", "--name", "charts"}) {
			return "charts-control-plane\n"
		}
		if command.Name == "docker" && len(command.Args) > 2 && command.Args[0] == "exec" && command.Args[2] == "crictl" {
			return fmt.Sprintf(`{"status":{"id":%q,"repoTags":["docker.io/iterabase-e2e/control-plane:exact-head"]},"info":{"imageSpec":{"config":{"Labels":{"org.opencontainers.image.revision":"exact-head"}}}}}`, configDigest)
		}
		if command.Name == "docker" && len(command.Args) > 2 && command.Args[0] == "exec" && command.Args[2] == "ctr" {
			return "REF TYPE DIGEST SIZE PLATFORMS LABELS\n" +
				"docker.io/iterabase-e2e/control-plane:exact-head application/vnd.oci.image.manifest.v1+json " + runtimeDigest + " 1B linux/amd64 -\n"
		}
		return ""
	}}
	cluster, err := Use("charts", filepath.Join(t.TempDir(), "kubeconfig"), executor)
	if err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(t.TempDir(), "artifacts", "e2e-runtime-control-plane-image")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(artifactDir, "control-plane-image.tar")
	if err := os.WriteFile(archive, []byte("exact downloaded archive bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	identity, err := cluster.ImportImageArchive(context.Background(), archive, "iterabase-e2e/control-plane:exact-head", configDigest)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ConfigDigest != configDigest || identity.RuntimeDigest != runtimeDigest || identity.Labels["org.opencontainers.image.revision"] != "exact-head" {
		t.Fatalf("imported identity = %+v", identity)
	}
	if len(executor.commands) != 5 {
		t.Fatalf("import commands = %d, want docker restore + Kind transport + config/manifest identity inspection", len(executor.commands))
	}
	if got := executor.commands[0]; got.Name != "docker" || !slices.Equal(got.Args, []string{"load", "-i", archive}) {
		t.Fatalf("first import command = %+v, want exact downloaded archive restore", got)
	}
	if got := executor.commands[1]; got.Name != "kind" || !slices.Equal(got.Args, []string{"load", "docker-image", "--name", "charts", "iterabase-e2e/control-plane:exact-head"}) {
		t.Fatalf("second import command = %+v, want post-create Kind transport", got)
	}
	if got := executor.commands[3]; got.Name != "docker" || !slices.Equal(got.Args, []string{"exec", "charts-control-plane", "crictl", "inspecti", "iterabase-e2e/control-plane:exact-head"}) {
		t.Fatalf("config inspection command = %+v, want exact node config identity", got)
	}
	if got := executor.commands[4]; got.Name != "docker" || !slices.Equal(got.Args, []string{"exec", "charts-control-plane", "ctr", "-n", "k8s.io", "images", "list"}) {
		t.Fatalf("manifest inspection command = %+v, want exact imported runtime identity", got)
	}
}

func TestValidateManagedLVMStorageClassFailsClosed(t *testing.T) {
	t.Parallel()
	contract := testLVMStorageContract()
	valid := map[string]any{
		"metadata": map[string]any{
			"name":        contract.StorageClasses[1].Name,
			"annotations": map[string]any{defaultClassAnnotation: "false", betaDefaultClassAnnotation: "false"},
		},
		"provisioner": contract.Provisioner, "reclaimPolicy": "Delete",
		"volumeBindingMode": "WaitForFirstConsumer", "allowVolumeExpansion": true,
		"parameters": map[string]any{
			"storage": "lvm", "vgpattern": "^" + contract.DataVolumeGroupName + "$", "fsType": "xfs", "thinProvision": "no", "shared": "yes",
		},
	}
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	name, err := ValidateManagedLVMStorageClass(data, contract)
	if err != nil || name != contract.StorageClasses[1].Name {
		t.Fatalf("valid class=%q err=%v", name, err)
	}

	mutations := map[string]func(map[string]any){
		"default": func(value map[string]any) {
			value["metadata"].(map[string]any)["annotations"].(map[string]any)[defaultClassAnnotation] = "true"
		},
		"provisioner": func(value map[string]any) { value["provisioner"] = "rancher.io/local-path" },
		"reclaim":     func(value map[string]any) { value["reclaimPolicy"] = "Retain" },
		"binding":     func(value map[string]any) { value["volumeBindingMode"] = "Immediate" },
		"expansion":   func(value map[string]any) { value["allowVolumeExpansion"] = false },
		"filesystem":  func(value map[string]any) { value["parameters"].(map[string]any)["fsType"] = "ext4" },
		"thin":        func(value map[string]any) { value["parameters"].(map[string]any)["thinProvision"] = "yes" },
		"unshared":    func(value map[string]any) { value["parameters"].(map[string]any)["shared"] = "no" },
		"vg":          func(value map[string]any) { value["parameters"].(map[string]any)["vgpattern"] = ".*" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(valid)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err := json.Unmarshal(data, &value); err != nil {
				t.Fatal(err)
			}
			mutate(value)
			data, err = json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateManagedLVMStorageClass(data, contract); err == nil {
				t.Fatalf("%s contract drift unexpectedly passed", name)
			}
		})
	}
}

// testLVMStorageContract returns a representative owner-supplied LVM storage
// contract used to exercise the shared, parameterized validation mechanics in
// testkit itself. The exact expectations are the owning suite's responsibility.
func testLVMStorageContract() LVMStorageContract {
	return LVMStorageContract{
		DataVolumeGroupName: "iterabase-data",
		Provisioner:         "local.csi.openebs.io",
		NodeTopologyKey:     "openebs.io/nodename",
		StorageClasses: []StorageClassExpectation{
			{Name: "iterabase-lvm-xfs", Shared: false},
			{Name: "iterabase-agentpool-lvm-xfs", Shared: true},
		},
	}
}

func TestValidateLVMSnapshotDependencyBoundary(t *testing.T) {
	t.Parallel()
	deployment := func(name, image string) string {
		return fmt.Sprintf(`{"items":[{"spec":{"template":{"spec":{"containers":[{"name":%q,"image":%q}]}}},"status":{"availableReplicas":1}}]}`, name, image)
	}
	bounded := func(command process.Command) string {
		args := strings.Join(command.Args, " ")
		switch {
		case strings.Contains(args, "get crd lvmsnapshots.local.openebs.io"):
			return "customresourcedefinition.apiextensions.k8s.io/lvmsnapshots.local.openebs.io\n"
		case strings.Contains(args, "get deployment"):
			return deployment("openebs-lvm-plugin", "docker.io/openebs/lvm-driver:1.10.0@sha256:exact")
		default:
			return ""
		}
	}
	for name, output := range map[string]func(process.Command) string{
		"bounded volume only": bounded,
		"missing inert CRD": func(command process.Command) string {
			if strings.Contains(strings.Join(command.Args, " "), "get deployment") {
				return deployment("openebs-lvm-plugin", "docker.io/openebs/lvm-driver:1.10.0@sha256:exact")
			}
			return ""
		},
		"forbidden instance": func(command process.Command) string {
			if strings.Contains(strings.Join(command.Args, " "), "get lvmsnapshots.local.openebs.io") {
				return "lvmsnapshot.local.openebs.io/forbidden\n"
			}
			return bounded(command)
		},
		"forbidden CSI CRD": func(command process.Command) string {
			if strings.Contains(strings.Join(command.Args, " "), "get crd volumesnapshots.snapshot.storage.k8s.io") {
				return "customresourcedefinition.apiextensions.k8s.io/volumesnapshots.snapshot.storage.k8s.io\n"
			}
			return bounded(command)
		},
		"forbidden container": func(command process.Command) string {
			if strings.Contains(strings.Join(command.Args, " "), "get deployment") {
				return deployment("csi-snapshotter", "registry.k8s.io/sig-storage/csi-snapshotter:v8.2.0")
			}
			return bounded(command)
		},
	} {
		t.Run(name, func(t *testing.T) {
			executor := &fakeExecutor{outputFor: output}
			cluster, err := Use("charts", filepath.Join(t.TempDir(), "kubeconfig"), executor)
			if err != nil {
				t.Fatal(err)
			}
			err = cluster.validateLVMSnapshotDependencyBoundary(context.Background(), "iterabase-system")
			if name == "bounded volume only" && err != nil {
				t.Fatalf("bounded volume authority failed: %v", err)
			}
			if name != "bounded volume only" && err == nil {
				t.Fatalf("%s boundary unexpectedly passed", name)
			}
		})
	}
}

func TestValidateLVMSnapshotDependencyBoundaryFailsOnObservationError(t *testing.T) {
	t.Parallel()
	executor := &fakeExecutor{failNext: true}
	cluster, err := Use("charts", filepath.Join(t.TempDir(), "kubeconfig"), executor)
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.validateLVMSnapshotDependencyBoundary(context.Background(), "iterabase-system"); err == nil {
		t.Fatal("LVMSnapshot dependency observation error unexpectedly passed")
	}
}

func TestConfigureLVMStorageRejectsMissingChartBeforeMutation(t *testing.T) {
	t.Parallel()
	executor := &fakeExecutor{}
	cluster, err := Use("charts", filepath.Join(t.TempDir(), "kubeconfig"), executor)
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.ConfigureLVMStorage(context.Background(), filepath.Join(t.TempDir(), "missing"), "iterabase-system", "iterabase-lvm-storage", testLVMStorageContract()); err == nil {
		t.Fatal("missing pinned LVM chart unexpectedly reached infrastructure mutation")
	}
	if len(executor.commands) != 0 {
		t.Fatalf("missing chart executed commands: %+v", executor.commands)
	}
}

func TestLVMStorageCleanupDeletesPlatformResourcesBeforeHelmUninstall(t *testing.T) {
	t.Parallel()
	executor := &fakeExecutor{}
	cluster, err := Use("charts", filepath.Join(t.TempDir(), "kubeconfig"), executor)
	if err != nil {
		t.Fatal(err)
	}
	cluster.lvmNamespace = "iterabase-system"
	cluster.lvmRelease = "iterabase-lvm-storage"
	cluster.lvmNode = "charts-control-plane"
	cluster.lvmContract = testLVMStorageContract()
	if err := cluster.cleanupLVMStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(executor.commands) != 1 || executor.commands[0].Name != "bash" || len(executor.commands[0].Args) < 2 {
		t.Fatalf("cleanup commands = %+v", executor.commands)
	}
	script := executor.commands[0].Args[1]
	platformDelete := strings.Index(script, "api-resources --api-group=platform.iterabase.com")
	helmUninstall := strings.Index(script, "helm uninstall")
	if platformDelete < 0 || helmUninstall < 0 || platformDelete >= helmUninstall {
		t.Fatalf("cleanup does not release platform CRs before Helm uninstall:\n%s", script)
	}
}

func TestMissingDownloadedRuntimeArtifactCannotReachClusterImport(t *testing.T) {
	t.Parallel()
	executor := &fakeExecutor{}
	cluster, err := Use("charts", filepath.Join(t.TempDir(), "kubeconfig"), executor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.ImportImageArchive(context.Background(), filepath.Join(t.TempDir(), "missing.tar"), "iterabase-e2e/control-plane:exact-head", "sha256:"+strings.Repeat("a", 64)); err == nil {
		t.Fatal("missing downloaded runtime artifact unexpectedly reached install transport")
	}
	if len(executor.commands) != 0 {
		t.Fatalf("missing archive executed import commands: %+v", executor.commands)
	}
}

func TestDeleteUsesIndependentBoundedContextsAndRetriesFailedKindTeardown(t *testing.T) {
	t.Parallel()
	executor := &teardownExecutor{}
	tmp := t.TempDir()
	cluster := &Cluster{
		Name: "retryable", Kubeconfig: filepath.Join(tmp, "kubeconfig"), executor: executor,
		owned: true, lvmPrepared: true, tempDir: filepath.Join(tmp, "state"),
	}
	if err := os.MkdirAll(cluster.tempDir, 0o700); err != nil {
		t.Fatal(err)
	}
	caller, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cluster.Delete(caller); err == nil {
		t.Fatal("first Kind teardown failure was not returned")
	}
	if cluster.deleted {
		t.Fatal("failed teardown was cached as terminal deletion")
	}
	if err := cluster.Delete(caller); err != nil {
		t.Fatalf("retry Kind teardown: %v", err)
	}
	if !cluster.deleted {
		t.Fatal("successful retry did not mark terminal deletion")
	}
	if executor.cleanupCalls != 2 || executor.kindCalls != 2 {
		t.Fatalf("cleanup/kind calls = %d/%d, want 2/2", executor.cleanupCalls, executor.kindCalls)
	}
	if err := cluster.Delete(caller); err != nil {
		t.Fatal(err)
	}
	if executor.kindCalls != 2 {
		t.Fatal("terminal deletion executed Kind again")
	}
}

type teardownExecutor struct {
	cleanupCalls int
	kindCalls    int
}

func (executor *teardownExecutor) Run(ctx context.Context, command process.Command) (process.Result, error) {
	if err := ctx.Err(); err != nil {
		return process.Result{}, fmt.Errorf("phase received canceled context: %w", err)
	}
	switch command.Name {
	case "bash":
		executor.cleanupCalls++
		return process.Result{}, errors.New("best-effort LVM cleanup failed")
	case "kind":
		executor.kindCalls++
		if executor.kindCalls == 1 {
			return process.Result{}, errors.New("transient Kind teardown failure")
		}
		return process.Result{}, nil
	default:
		return process.Result{}, fmt.Errorf("unexpected command %s", command.Name)
	}
}

func TestClusterLifecycleUsesUniqueNamesAndIsolatedKubeconfigs(t *testing.T) {
	t.Parallel()
	executor := &fakeExecutor{}
	manager := Manager{
		Executor: executor,
		TempRoot: t.TempDir(),
		Now:      func() time.Time { return time.Unix(0, 42) },
		Random:   bytes.NewReader([]byte("abcdefgh")),
	}
	first, err := manager.Create(context.Background(), "charts-e2e")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Create(context.Background(), "charts-e2e")
	if err != nil {
		t.Fatal(err)
	}
	if first.Name == second.Name || first.Kubeconfig == second.Kubeconfig {
		t.Fatalf("clusters collided: first=%+v second=%+v", first, second)
	}
	if err := first.LoadImage(context.Background(), "control-plane:test"); err != nil {
		t.Fatal(err)
	}
	if err := first.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(executor.commands); got != 4 {
		t.Fatalf("commands = %d, want two creates + load + one delete", got)
	}
	if executor.commands[0].Args[0] != "create" || executor.commands[3].Args[0] != "delete" {
		t.Fatalf("unexpected lifecycle commands: %+v", executor.commands)
	}
}

func TestRegistryAuthConfig(t *testing.T) {
	config, err := registryAuthConfig("", "")
	if err != nil || config != "" {
		t.Fatalf("no credentials must leave the cluster config unchanged: %q, %v", config, err)
	}
	config, err = registryAuthConfig("iterabase", "dckr_pat_abc-123")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`[plugins."io.containerd.grpc.v1.cri".registry.configs."registry-1.docker.io".auth]`,
		`username = "iterabase"`, `password = "dckr_pat_abc-123"`,
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("kind config is missing %q:\n%s", want, config)
		}
	}
	for _, bad := range [][2]string{{"iterabase", ""}, {"", "token"}, {"user\"", "token"}, {"user", "tok\nen"}} {
		if _, err := registryAuthConfig(bad[0], bad[1]); err == nil {
			t.Fatalf("credentials %q were accepted", bad)
		}
	}
}
