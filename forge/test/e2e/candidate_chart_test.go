package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
)

const (
	platformChartArchiveEnv   = "FORGE_E2E_PLATFORM_CHART_ARCHIVE"
	substrateChartArchiveEnv  = "FORGE_E2E_SUBSTRATE_CHART_ARCHIVE"
	lvmStorageChartArchiveEnv = "FORGE_E2E_LVM_STORAGE_CHART_ARCHIVE"
)

type importedRuntimeIdentity struct {
	ConfigDigest   string
	ManifestDigest string
}

// prepareCandidateChart transfers the exact Actions-retained platform and
// companion archives to the fixture host. Forge then gives remote Helm those
// extracted directories, so real-machine validation consumes the candidate
// bytes without publishing a persistent candidate package.
func prepareCandidateImages(t *testing.T, ip, keyPath string) map[string]importedRuntimeIdentity {
	t.Helper()
	runtimeDigests := make(map[string]importedRuntimeIdentity)
	type imageInput struct {
		name       string
		prefix     string
		archiveEnv string
	}
	inputs := []imageInput{
		{name: "control-plane", prefix: "CONTROL_PLANE", archiveEnv: "FORGE_E2E_CONTROL_PLANE_IMAGE_ARCHIVE"},
		{name: "harness", prefix: "HARNESS", archiveEnv: "FORGE_E2E_HARNESS_IMAGE_ARCHIVE"},
		{name: "tool-runner", prefix: "TOOL_RUNNER", archiveEnv: "FORGE_E2E_TOOL_RUNNER_IMAGE_ARCHIVE"},
		{name: "inference-gateway", prefix: "INFERENCE_GATEWAY", archiveEnv: "FORGE_E2E_INFERENCE_IMAGE_ARCHIVE"},
		{name: "runtime-fixture", prefix: "FORGE_E2E_RUNTIME", archiveEnv: "FORGE_E2E_RUNTIME_IMAGE_ARCHIVE"},
	}
	selected := make([]imageInput, 0, len(inputs))
	for _, input := range inputs {
		if os.Getenv(input.archiveEnv) == "" && os.Getenv(input.prefix+"_IMAGE_REPO") == "" &&
			os.Getenv(input.prefix+"_IMAGE_TAG") == "" && os.Getenv(input.prefix+"_IMAGE_CONFIG_DIGEST") == "" {
			continue
		}
		selected = append(selected, input)
	}
	if len(selected) == 0 {
		return runtimeDigests
	}
	client, err := sshDial(ip, keyPath)
	if err != nil {
		t.Fatalf("dial candidate host to transfer images: %v", err)
	}
	defer client.Close()
	for _, input := range selected {
		archive := os.Getenv(input.archiveEnv)
		repository := os.Getenv(input.prefix + "_IMAGE_REPO")
		tag := os.Getenv(input.prefix + "_IMAGE_TAG")
		configDigest := os.Getenv(input.prefix + "_IMAGE_CONFIG_DIGEST")
		if repository == "" || tag == "" || !isCanonicalSHA256Digest(configDigest) {
			t.Fatalf("composed %s image has incomplete repository/tag/config-digest identity", input.name)
		}
		reference := repository + ":" + tag
		source, err := os.Open(archive)
		if err != nil {
			t.Fatalf("open candidate image archive %s: %v", archive, err)
		}
		remote := filepath.Join("/tmp", filepath.Base(archive))
		session, err := client.NewSession()
		if err != nil {
			source.Close()
			t.Fatal(err)
		}
		session.Stdin = source
		output, transferErr := session.CombinedOutput("cat > " + candidateShellQuote(remote))
		session.Close()
		source.Close()
		if transferErr != nil {
			t.Fatalf("transfer candidate image %s: %v\n%s", archive, transferErr, output)
		}
		if output, err := sshOutput(client, "sudo k3s ctr images import "+candidateShellQuote(remote)+" && rm -f "+candidateShellQuote(remote)); err != nil {
			t.Fatalf("import candidate image %s: %v\n%s", archive, err, output)
		}
		inspection, err := sshOutput(client, "sudo k3s crictl inspecti "+candidateShellQuote(reference))
		if err != nil {
			t.Fatalf("inspect imported candidate image %s: %v\n%s", reference, err, inspection)
		}
		repoTag, labels, err := importedRuntimeImageConfig([]byte(inspection), configDigest)
		if err != nil {
			t.Fatalf("verify imported candidate image %s config: %v", reference, err)
		}
		manifest, err := sshOutput(client, "sudo k3s ctr images list")
		if err != nil {
			t.Fatalf("inspect imported candidate image %s manifest: %v\n%s", reference, err, manifest)
		}
		runtimeDigest, err := importedRuntimeManifestDigest([]byte(manifest), repoTag)
		if err != nil {
			t.Fatalf("verify imported candidate image %s manifest: %v", reference, err)
		}
		if sourceSHA := os.Getenv(input.prefix + "_IMAGE_SOURCE_SHA"); sourceSHA != "" && labels["org.opencontainers.image.revision"] != sourceSHA {
			t.Fatalf("imported %s image revision label=%q want=%q", input.name, labels["org.opencontainers.image.revision"], sourceSHA)
		}
		runtimeDigests[input.prefix] = importedRuntimeIdentity{
			ConfigDigest: configDigest, ManifestDigest: runtimeDigest,
		}
		artifact := map[string]string{
			"CONTROL_PLANE": "control-plane-image", "HARNESS": "harness-image", "TOOL_RUNNER": "tool-runner-image",
			"INFERENCE_GATEWAY": "inference-gateway-image", "FORGE_E2E_RUNTIME": "runtime-fixture-image",
		}[input.prefix]
		if err := sharede2e.RecordRuntimeImageIdentity(artifact, runtimeDigest); err != nil {
			t.Fatalf("record imported %s runtime identity: %v", input.name, err)
		}
	}
	return runtimeDigests
}

func importedRuntimeImageConfig(data []byte, expectedConfigDigest string) (string, map[string]string, error) {
	var image struct {
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
	if err := json.Unmarshal(data, &image); err != nil {
		return "", nil, fmt.Errorf("decode CRI image identity: %w", err)
	}
	if image.Status.ID != expectedConfigDigest {
		return "", nil, fmt.Errorf("CRI config digest %q != %q", image.Status.ID, expectedConfigDigest)
	}
	if len(image.Status.RepoTags) != 1 {
		return "", nil, fmt.Errorf("CRI runtime tags are ambiguous: %v", image.Status.RepoTags)
	}
	return image.Status.RepoTags[0], image.Info.ImageSpec.Config.Labels, nil
}

func importedRuntimeManifestDigest(data []byte, repoTag string) (string, error) {
	digests := make(map[string]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == repoTag && isCanonicalSHA256Digest(fields[2]) {
			digests[fields[2]] = struct{}{}
		}
	}
	if len(digests) != 1 {
		return "", fmt.Errorf("imported runtime manifest identity is ambiguous")
	}
	for digest := range digests {
		return digest, nil
	}
	panic("unreachable")
}

func TestImportedRuntimeImageIdentityKeepsConfigAndManifestDigestsDistinct(t *testing.T) {
	configDigest := "sha256:" + strings.Repeat("a", 64)
	runtimeDigest := "sha256:" + strings.Repeat("b", 64)
	configData := []byte(fmt.Sprintf(
		`{"status":{"id":%q,"repoTags":["docker.io/iterabase-e2e/control-plane:exact-head"]},"info":{"imageSpec":{"config":{"Labels":{"org.opencontainers.image.revision":"exact-head"}}}}}`,
		configDigest,
	))
	repoTag, labels, err := importedRuntimeImageConfig(configData, configDigest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := importedRuntimeManifestDigest([]byte(
		"REF TYPE DIGEST SIZE PLATFORMS LABELS\n"+
			"docker.io/iterabase-e2e/control-plane:exact-head application/vnd.oci.image.manifest.v1+json "+runtimeDigest+" 1B linux/amd64 -\n",
	), repoTag)
	if err != nil {
		t.Fatal(err)
	}
	if repoTag != "docker.io/iterabase-e2e/control-plane:exact-head" || got != runtimeDigest || labels["org.opencontainers.image.revision"] != "exact-head" {
		t.Fatalf("repoTag=%s runtime identity=%s labels=%v", repoTag, got, labels)
	}
	if _, _, err := importedRuntimeImageConfig(configData, "sha256:"+strings.Repeat("c", 64)); err == nil {
		t.Fatal("mismatched composer config digest unexpectedly passed remote import verification")
	}
}

func candidateShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func resetCandidateChartRootCommand(root string) string {
	quoted := candidateShellQuote(root)
	return "rm -rf -- " + quoted + " && install -d -m 0700 -- " + quoted
}

func prepareCandidateChart(t *testing.T, ip, keyPath string) {
	t.Helper()
	platform := os.Getenv(platformChartArchiveEnv)
	substrate := os.Getenv(substrateChartArchiveEnv)
	lvmStorage := os.Getenv(lvmStorageChartArchiveEnv)
	if platform == "" || substrate == "" || lvmStorage == "" {
		t.Fatalf("exact platform validation requires %s, %s, and %s", platformChartArchiveEnv, substrateChartArchiveEnv, lvmStorageChartArchiveEnv)
	}

	root := fmt.Sprintf("/tmp/iterabase-release-charts-%d", os.Getpid())
	client, err := sshDial(ip, keyPath)
	if err != nil {
		t.Fatalf("dial candidate host to transfer charts: %v", err)
	}
	defer client.Close()
	if output, err := sshOutput(client, resetCandidateChartRootCommand(root)); err != nil {
		t.Fatalf("prepare remote candidate chart directory: %v\n%s", err, output)
	}

	for _, archive := range []string{platform, substrate, lvmStorage} {
		source, err := os.Open(archive)
		if err != nil {
			t.Fatalf("open exact candidate chart %s: %v", archive, err)
		}
		remote := filepath.Join(root, filepath.Base(archive))
		session, err := client.NewSession()
		if err != nil {
			source.Close()
			t.Fatalf("create candidate chart transfer session: %v", err)
		}
		session.Stdin = source
		output, transferErr := session.CombinedOutput("cat > " + candidateShellQuote(remote))
		session.Close()
		source.Close()
		if transferErr != nil {
			t.Fatalf("transfer exact candidate chart %s: %v\n%s", archive, transferErr, output)
		}
		if output, err := sshOutput(client, "tar -xzf "+candidateShellQuote(remote)+" -C "+candidateShellQuote(root)); err != nil {
			t.Fatalf("extract exact candidate chart %s: %v\n%s", archive, err, output)
		}
	}

	t.Setenv("FORGE_E2E_CHART_REPOSITORY", filepath.Join(root, "iterabase-platform"))
	t.Cleanup(func() {
		cleanup, err := sshDial(ip, keyPath)
		if err != nil {
			t.Logf("remove remote candidate charts: dial host: %v", err)
			return
		}
		defer cleanup.Close()
		if output, err := sshOutput(cleanup, "rm -rf "+candidateShellQuote(root)); err != nil {
			t.Logf("remove remote candidate charts: %v\n%s", err, output)
		}
	})
	t.Log("transferred exact platform, certificate-substrate, and LVM-storage candidate archives to the real-machine host")
}

func TestCandidateChartTransferRecreatesAnEmptyPrivateRoot(t *testing.T) {
	root := "/tmp/iterabase-release-charts-123"
	want := "rm -rf -- '/tmp/iterabase-release-charts-123' && install -d -m 0700 -- '/tmp/iterabase-release-charts-123'"
	if got := resetCandidateChartRootCommand(root); got != want {
		t.Fatalf("candidate chart reset command = %q, want %q", got, want)
	}
}
