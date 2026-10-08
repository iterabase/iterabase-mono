package e2e_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/httpx"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/kube"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
	"gopkg.in/yaml.v3"
)

// Shared declarative lifecycle mechanics: Forge-equivalent CRD pre-apply,
// persisted-state seeding, retained Secret/PVC/workload identities, the
// Helm-owned artifact provisioner, and release-history assertions.

const (
	transitionFieldManager = "iterabase-chart-e2e"
	transitionMarker       = "hor-475-persisted-state"
)

type bundledCRDHeader struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
}

type bundledCRD struct {
	header   bundledCRDHeader
	manifest string
}

type lifecycleSnapshot struct {
	Secrets                map[string]string
	PVCs                   map[string]string
	Pods                   map[string]string
	ArtifactProvisionerJob artifactProvisionerJobIdentity
}

type artifactProvisionerJobIdentity struct {
	Name string
	UID  string
}

type helmHistoryEntry struct {
	Revision int    `json:"revision"`
	Status   string `json:"status"`
	Chart    string `json:"chart"`
}

func parsePublishedChartReference(reference string) (chart, repository, version string, err error) {
	separator := strings.LastIndexByte(reference, ':')
	if separator <= len("oci://") || separator == len(reference)-1 {
		return "", "", "", fmt.Errorf("published chart reference has no exact version: %q", reference)
	}
	repository, version = reference[:separator], reference[separator+1:]
	chart = filepath.Base(repository)
	if chart == "." || chart == "/" || strings.Contains(strings.ToLower(version), "latest") {
		return "", "", "", fmt.Errorf("invalid published chart reference %q", reference)
	}
	return chart, repository, version, nil
}

func verifyArchiveChecksum(path, expected string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	actual := hex.EncodeToString(digest[:])
	if actual != expected {
		return fmt.Errorf("archive checksum %s != expected %s", actual, expected)
	}
	return nil
}

func seedPersistedStateStage(t *testing.T, state *chartState) {
	t.Helper()
	state.kubectl(t, 2*time.Minute, "exec", "-n", testNamespace, "statefulset/"+testRelease+"-postgresql", "--",
		"psql", "-U", "controlplane", "-d", "controlplane", "-v", "ON_ERROR_STOP=1", "-c",
		"CREATE TABLE IF NOT EXISTS e2e_chart_transition (marker text PRIMARY KEY); INSERT INTO e2e_chart_transition(marker) VALUES ('"+transitionMarker+"') ON CONFLICT DO NOTHING;")
	state.kubectl(t, 30*time.Second, "exec", "-n", testNamespace, testRelease+"-minio-0", "--",
		"sh", "-c", "printf '%s' '"+transitionMarker+"' > /data/"+transitionMarker)
	assertPersistedState(t, state)
}

// assertPersistedState reads the MinIO marker from the StatefulSet's pod by
// name: the artifact-provisioner Job's pod also matches the StatefulSet's
// (immutable) selector, so `exec statefulset/...` can land in it instead.
func assertPersistedState(t *testing.T, state *chartState) {
	t.Helper()
	postgres := state.kubectl(t, 90*time.Second, "exec", "-n", testNamespace, "statefulset/"+testRelease+"-postgresql", "--",
		"psql", "-At", "-U", "controlplane", "-d", "controlplane", "-c",
		"SELECT marker FROM e2e_chart_transition WHERE marker='"+transitionMarker+"';")
	if postgres != transitionMarker {
		t.Fatalf("PostgreSQL persisted marker=%q want=%q", postgres, transitionMarker)
	}
	minio, err := state.kubectlOutput(30*time.Second, "exec", "-n", testNamespace, testRelease+"-minio-0", "--",
		"cat", "/data/"+transitionMarker)
	if err != nil || strings.TrimSpace(minio) != transitionMarker {
		t.Fatalf("MinIO persisted marker=%q want=%q (%v)\n%s", strings.TrimSpace(minio), transitionMarker, err, minioVolumeEvidence(state))
	}
}

// minioVolumeEvidence tells a deleted file on the same filesystem apart from a
// volume recreated underneath an unchanged PVC.
func minioVolumeEvidence(state *chartState) string {
	var evidence strings.Builder
	for _, probe := range [][]string{
		{"get", "pod", testRelease + "-minio-0", "-n", testNamespace, "-o", "jsonpath={.metadata.uid} {.status.startTime} {.spec.containers[0].image}"},
		{"get", "pvc", "data-" + testRelease + "-minio-0", "-n", testNamespace, "-o", "jsonpath={.metadata.uid} {.spec.volumeName}"},
		{"exec", "-n", testNamespace, testRelease + "-minio-0", "--", "sh", "-c", "ls -la --time-style=full-iso /data; findmnt -no SOURCE,FSTYPE,OPTIONS /data"},
	} {
		out, err := state.kubectlOutput(30*time.Second, probe...)
		fmt.Fprintf(&evidence, "kubectl %s:\n%s(err=%v)\n", strings.Join(probe, " "), out, err)
	}
	node := testRelease
	if nodes, err := state.runner.Run(state.ctx, process.Command{Name: "kind", Args: []string{"get", "nodes", "--name", state.cluster.Name}, Timeout: 30 * time.Second}); err == nil {
		node = strings.TrimSpace(nodes.Output)
	}
	lvs, err := state.runner.Run(state.ctx, process.Command{Name: "docker", Args: []string{"exec", node, "lvs", "-o", "lv_name,lv_time,lv_size"}, Timeout: 30 * time.Second})
	fmt.Fprintf(&evidence, "lvs on %s:\n%s(err=%v)\n", node, lvs.Output, err)
	return evidence.String()
}

func captureLifecycleSnapshot(t *testing.T, state *chartState) lifecycleSnapshot {
	t.Helper()
	snapshot := lifecycleSnapshot{Secrets: map[string]string{}, PVCs: map[string]string{}, Pods: map[string]string{}}
	for _, name := range []string{
		testRelease + "-postgresql", testRelease + "-minio", testRelease + "-minio-artifacts",
		testRelease + "-control-plane-jwt", testRelease + "-gateway-admin",
	} {
		snapshot.Secrets[name] = secretDigest(t, state, name)
	}
	for _, name := range []string{"data-" + testRelease + "-postgresql-0", "data-" + testRelease + "-minio-0"} {
		snapshot.PVCs[name] = state.kubectl(t, 30*time.Second, "get", "pvc/"+name, "-n", testNamespace,
			"-o", "jsonpath={.metadata.uid}/{.spec.volumeName}")
	}
	for _, release := range []string{testRelease, testRelease + "-cert-manager"} {
		releaseSelector := "app.kubernetes.io/instance=" + release
		workloadData := state.kubectl(t, 30*time.Second, "get", "deployments,statefulsets,daemonsets", "-n", testNamespace,
			"-l", releaseSelector, "-o", "json")
		workloads, err := stableWorkloadSelectorsJSON([]byte(workloadData))
		if err != nil {
			t.Fatalf("discover stable workloads for release %s: %v", release, err)
		}
		for _, workload := range workloads {
			identities := strings.Fields(state.kubectl(t, 30*time.Second, "get", "pods", "-n", testNamespace,
				"-l", workload.Selector, "-o", `go-template={{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}={{.metadata.uid}}{{"\n"}}{{end}}{{end}}`))
			if len(identities) == 0 {
				t.Fatalf("%s/%s has no stable pod identity", release, workload.Key)
			}
			for _, identity := range identities {
				name, uid, ok := strings.Cut(identity, "=")
				if !ok || name == "" || uid == "" {
					t.Fatalf("%s/%s returned invalid pod identity %q", release, workload.Key, identity)
				}
				snapshot.Pods[release+"/"+workload.Key+"/"+name] = uid
			}
		}
	}
	return snapshot
}

type stableWorkloadSelector struct {
	Key      string
	Selector string
}

func stableWorkloadSelectorsJSON(workloadData []byte) ([]stableWorkloadSelector, error) {
	var workloads struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Selector struct {
					MatchLabels      map[string]string `json:"matchLabels"`
					MatchExpressions []json.RawMessage `json:"matchExpressions"`
				} `json:"selector"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(workloadData, &workloads); err != nil {
		return nil, fmt.Errorf("decode stable workloads: %w", err)
	}
	if len(workloads.Items) == 0 {
		return nil, errors.New("release has no stable workloads")
	}
	selectors := make([]stableWorkloadSelector, 0, len(workloads.Items))
	for _, workload := range workloads.Items {
		if workload.Metadata.Name == "" || len(workload.Spec.Selector.MatchLabels) == 0 || len(workload.Spec.Selector.MatchExpressions) != 0 {
			return nil, fmt.Errorf("%s/%s requires an exact matchLabels selector", workload.Kind, workload.Metadata.Name)
		}
		labels := make([]string, 0, len(workload.Spec.Selector.MatchLabels))
		for key, value := range workload.Spec.Selector.MatchLabels {
			labels = append(labels, key+"="+value)
		}
		sort.Strings(labels)
		selectors = append(selectors, stableWorkloadSelector{
			Key: strings.ToLower(workload.Kind) + "/" + workload.Metadata.Name, Selector: strings.Join(labels, ","),
		})
	}
	sort.Slice(selectors, func(i, j int) bool { return selectors[i].Key < selectors[j].Key })
	return selectors, nil
}

func secretDigest(t *testing.T, state *chartState, name string) string {
	t.Helper()
	result, err := state.runner.Run(state.ctx, process.Command{
		Name: "bash", Args: []string{"-o", "pipefail", "-c", `kubectl --kubeconfig "$KUBECONFIG_PATH" get secret "$SECRET_NAME" -n "$SECRET_NAMESPACE" -o go-template='{{range $key, $value := .data}}{{$key}}={{$value}}{{"\n"}}{{end}}' | sha256sum | awk '{print $1}'`},
		Env:     map[string]string{"KUBECONFIG_PATH": state.cluster.Kubeconfig, "SECRET_NAME": name, "SECRET_NAMESPACE": testNamespace},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("hash Secret %s without retaining its values: %v", name, err)
	}
	digest := strings.TrimSpace(result.Stdout)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
		t.Fatalf("Secret %s returned invalid digest %q", name, digest)
	}
	return digest
}

func assertRetainedState(t *testing.T, before, after lifecycleSnapshot, includePods bool) {
	t.Helper()
	if err := retainedStateError(before, after, includePods); err != nil {
		t.Fatal(err)
	}
}

func retainedStateError(before, after lifecycleSnapshot, includePods bool) error {
	if !mapsEqual(before.Secrets, after.Secrets) {
		return fmt.Errorf("generated Secret digests changed: before=%v after=%v", before.Secrets, after.Secrets)
	}
	if !mapsEqual(before.PVCs, after.PVCs) {
		return fmt.Errorf("PVC identities changed: before=%v after=%v", before.PVCs, after.PVCs)
	}
	if includePods && !mapsEqual(before.Pods, after.Pods) {
		return fmt.Errorf("idempotent reapply rolled workloads: before=%v after=%v", before.Pods, after.Pods)
	}
	if includePods && before.ArtifactProvisionerJob != after.ArtifactProvisionerJob {
		return fmt.Errorf("idempotent reapply replaced the completed artifact-provisioner Job: before=%+v after=%+v", before.ArtifactProvisionerJob, after.ArtifactProvisionerJob)
	}
	return nil
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func assertSchemaOwnership(t *testing.T, state *chartState) {
	t.Helper()
	manifest := state.process(t, 90*time.Second, "helm", "get", "manifest", testRelease, "-n", testNamespace,
		"--kubeconfig", state.cluster.Kubeconfig)
	for _, crd := range []string{"agentpools.platform.iterabase.com", "workflows.platform.iterabase.com"} {
		if !strings.Contains(manifest, "name: "+crd) {
			t.Fatalf("Helm release does not retain declarative ownership of %s", crd)
		}
		managedBy := state.kubectl(t, 30*time.Second, "get", "crd/"+crd,
			"-o", "jsonpath={.metadata.labels.app\\.kubernetes\\.io/managed-by}")
		instance := state.kubectl(t, 30*time.Second, "get", "crd/"+crd,
			"-o", "jsonpath={.metadata.labels.app\\.kubernetes\\.io/instance}")
		storedVersion := state.kubectl(t, 30*time.Second, "get", "crd/"+crd, "-o", "jsonpath={.status.storedVersions[0]}")
		if managedBy != "Helm" || instance != testRelease || storedVersion != "v1alpha1" {
			t.Fatalf("%s ownership/schema managed-by=%q instance=%q storedVersion=%q", crd, managedBy, instance, storedVersion)
		}
	}
}

func artifactProvisionerJobName(manifest []byte, release string) (string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(string(manifest)))
	var names []string
	for {
		var document struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name        string            `yaml:"name"`
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
		}
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("decode Helm release manifest: %w", err)
		}
		if document.Kind != "Job" || document.Metadata.Labels["app.kubernetes.io/component"] != "artifact-provisioner" {
			continue
		}
		if document.Metadata.Name == "" {
			return "", errors.New("artifact-provisioner Job has no name")
		}
		if managedBy := document.Metadata.Labels["app.kubernetes.io/managed-by"]; managedBy != "Helm" {
			return "", fmt.Errorf("artifact-provisioner Job managed-by=%q want Helm", managedBy)
		}
		if instance := document.Metadata.Labels["app.kubernetes.io/instance"]; instance != release {
			return "", fmt.Errorf("artifact-provisioner Job instance=%q want %q", instance, release)
		}
		if hook := document.Metadata.Annotations["helm.sh/hook"]; hook != "" {
			return "", fmt.Errorf("artifact-provisioner Job unexpectedly renders as Helm hook %q", hook)
		}
		names = append(names, document.Metadata.Name)
	}
	if len(names) != 1 {
		return "", fmt.Errorf("Helm release manifest has %d artifact-provisioner Jobs, want 1: %v", len(names), names)
	}
	return names[0], nil
}

func currentArtifactProvisionerJobIdentity(t *testing.T, state *chartState) artifactProvisionerJobIdentity {
	t.Helper()
	// Helm release manifests contain Secret data whose key names can look
	// credential-shaped to text redaction. Keep the exact manifest in a private
	// temporary file so redaction cannot make its YAML invalid before parsing.
	manifestPath := filepath.Join(t.TempDir(), "platform-manifest.yaml")
	if err := os.WriteFile(manifestPath, nil, 0o600); err != nil {
		t.Fatalf("create private Helm manifest file: %v", err)
	}
	if _, err := state.runner.Run(state.ctx, process.Command{
		Name: "bash", Args: []string{"-o", "pipefail", "-c", `helm get manifest "$RELEASE" -n "$NAMESPACE" --kubeconfig "$KUBECONFIG_PATH" > "$MANIFEST_OUTPUT"`},
		Env: map[string]string{
			"RELEASE": testRelease, "NAMESPACE": testNamespace, "KUBECONFIG_PATH": state.cluster.Kubeconfig, "MANIFEST_OUTPUT": manifestPath,
		},
		Timeout: 60 * time.Second,
	}); err != nil {
		t.Fatalf("read exact Helm release manifest: %v", err)
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read private Helm manifest file: %v", err)
	}
	name, err := artifactProvisionerJobName(manifest, testRelease)
	if err != nil {
		t.Fatal(err)
	}
	state.kubectl(t, 4*time.Minute, "wait", "--for=condition=Complete", "job/"+name, "-n", testNamespace, "--timeout=3m")
	identity, err := readArtifactProvisionerJobIdentity(state, name)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func readArtifactProvisionerJobIdentity(state *chartState, name string) (artifactProvisionerJobIdentity, error) {
	raw, err := state.kubectlOutput(30*time.Second, "get", "job/"+name, "-n", testNamespace, "-o", "json")
	if err != nil {
		return artifactProvisionerJobIdentity{}, fmt.Errorf("read artifact-provisioner Job %s: %w", name, err)
	}
	var job struct {
		Metadata struct {
			Name   string            `json:"name"`
			UID    string            `json:"uid"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		return artifactProvisionerJobIdentity{}, fmt.Errorf("decode artifact-provisioner Job %s: %w", name, err)
	}
	if job.Metadata.Name != name || job.Metadata.UID == "" {
		return artifactProvisionerJobIdentity{}, fmt.Errorf("artifact-provisioner Job identity name=%q uid=%q want name=%q and a UID", job.Metadata.Name, job.Metadata.UID, name)
	}
	if managedBy := job.Metadata.Labels["app.kubernetes.io/managed-by"]; managedBy != "Helm" {
		return artifactProvisionerJobIdentity{}, fmt.Errorf("live artifact-provisioner Job managed-by=%q want Helm", managedBy)
	}
	if instance := job.Metadata.Labels["app.kubernetes.io/instance"]; instance != testRelease {
		return artifactProvisionerJobIdentity{}, fmt.Errorf("live artifact-provisioner Job instance=%q want %q", instance, testRelease)
	}
	complete := false
	for _, condition := range job.Status.Conditions {
		if condition.Type == "Complete" && condition.Status == "True" {
			complete = true
			break
		}
	}
	if !complete {
		return artifactProvisionerJobIdentity{}, fmt.Errorf("artifact-provisioner Job %s is not Complete", name)
	}
	return artifactProvisionerJobIdentity{Name: name, UID: job.Metadata.UID}, nil
}

func assertReleaseMechanics(t *testing.T, state *chartState) {
	t.Helper()
	hooks := state.process(t, 60*time.Second, "helm", "get", "hooks", testRelease+"-cert-manager", "-n", testNamespace,
		"--kubeconfig", state.cluster.Kubeconfig)
	if !strings.Contains(hooks, "startupapicheck") || !strings.Contains(hooks, "helm.sh/hook: post-install") {
		t.Fatalf("certificate substrate does not retain its startup API hook: %s", stateSafeBody([]byte(hooks)))
	}
	_ = currentArtifactProvisionerJobIdentity(t, state)
}

func assertLifecycleHealth(t *testing.T, state *chartState) {
	t.Helper()
	for _, workload := range []string{
		"statefulset/" + testRelease + "-postgresql",
		"statefulset/" + testRelease + "-minio",
		"deployment/" + testRelease + "-redis",
		"deployment/" + testRelease + "-control-plane-api",
		"deployment/" + testRelease + "-control-plane-manager",
		"deployment/" + testRelease + "-gateway",
	} {
		state.kubectl(t, 6*time.Minute, "rollout", "status", workload, "-n", testNamespace, "--timeout=5m")
	}
	client, err := httpx.Client(15 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	controlPlane := state.forward(t, "svc/"+testRelease+"-control-plane-api", 8080, "http")
	requireHTTP(t, client, http.MethodGet, controlPlane.URL+"/healthz", nil, http.StatusOK)
	state.stopForward(t, controlPlane)
	gateway := state.forward(t, "svc/"+testRelease+"-gateway", 8080, "http")
	if err := waitHTTPReady(state.ctx, client, gateway.URL+"/readyz", 2*time.Minute); err != nil {
		t.Fatalf("gateway snapshot did not recover after the database rollout: %v", err)
	}
	body := requireHTTP(t, client, http.MethodGet, gateway.URL+"/readyz", nil, http.StatusOK)
	if !strings.Contains(string(body), `"fresh":true`) {
		t.Fatalf("gateway snapshot is not fresh: %s", stateSafeBody(body))
	}
	state.stopForward(t, gateway)
}

func selectBundledCRDs(raw string) (string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	selected := make(map[string]bundledCRD)
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("decode bundled CRDs: %w", err)
		}
		var header bundledCRDHeader
		if err := document.Decode(&header); err != nil {
			return "", fmt.Errorf("decode bundled CRD header: %w", err)
		}
		if header.APIVersion != "apiextensions.k8s.io/v1" || header.Kind != "CustomResourceDefinition" {
			continue
		}
		if header.Metadata.Name == "" {
			return "", errors.New("bundled CRD is missing metadata.name")
		}
		var resource any
		if err := document.Decode(&resource); err != nil {
			return "", fmt.Errorf("decode bundled CRD %s: %w", header.Metadata.Name, err)
		}
		encoded, err := yaml.Marshal(resource)
		if err != nil {
			return "", fmt.Errorf("encode bundled CRD %s: %w", header.Metadata.Name, err)
		}
		candidate := bundledCRD{header: header, manifest: strings.TrimSpace(string(encoded))}
		if existing, duplicate := selected[header.Metadata.Name]; duplicate {
			candidate, err = selectBundledCRD(existing, candidate)
			if err != nil {
				return "", err
			}
		}
		selected[header.Metadata.Name] = candidate
	}
	if len(selected) == 0 {
		return "", errors.New("exact current platform archive contains no CRDs")
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	manifests := make([]string, 0, len(names))
	for _, name := range names {
		manifests = append(manifests, selected[name].manifest)
	}
	return strings.Join(manifests, "\n---\n") + "\n", nil
}

func selectMetalLBCRDs(raw string) (string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	var crds []string
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("decode MetalLB bundled CRDs: %w", err)
		}
		var header bundledCRDHeader
		if err := document.Decode(&header); err != nil {
			return "", fmt.Errorf("decode MetalLB bundled CRD header: %w", err)
		}
		if header.APIVersion != "apiextensions.k8s.io/v1" || header.Kind != "CustomResourceDefinition" {
			continue
		}
		if !strings.HasSuffix(header.Metadata.Name, ".metallb.io") {
			continue
		}
		var resource any
		if err := document.Decode(&resource); err != nil {
			return "", fmt.Errorf("decode MetalLB bundled CRD: %w", err)
		}
		encoded, err := yaml.Marshal(resource)
		if err != nil {
			return "", fmt.Errorf("encode MetalLB bundled CRD %s: %w", header.Metadata.Name, err)
		}
		crds = append(crds, strings.TrimSpace(string(encoded)))
	}
	// Filter only the MetalLB CRDs; every other CRD the chart ships in `crds/`
	// directories or renders as an ordinary template is left entirely to Helm's
	// own install path, which owns it correctly on a fresh install. Pre-applying
	// those without Helm ownership makes a fresh `helm install` fail to import
	// them ("invalid ownership metadata"), so only the MetalLB set is established
	// before Helm (mirrors Forge's selectMetalLBCRDs, DES-HOR-511-03/04).
	if len(crds) == 0 {
		return "", nil
	}
	return strings.Join(crds, "\n---\n") + "\n", nil
}

func selectBundledCRD(existing, candidate bundledCRD) (bundledCRD, error) {
	if existing.manifest == candidate.manifest {
		return existing, nil
	}
	const versionAnnotation = "operator.prometheus.io/version"
	existingVersion := existing.header.Metadata.Annotations[versionAnnotation]
	candidateVersion := candidate.header.Metadata.Annotations[versionAnnotation]
	if existingVersion == "" && candidateVersion == "" {
		return bundledCRD{}, fmt.Errorf("conflicting duplicate bundled CRD %q has no authoritative version annotation", existing.header.Metadata.Name)
	}
	if existingVersion == "" {
		return candidate, nil
	}
	if candidateVersion == "" {
		return existing, nil
	}
	comparison, err := compareNumericVersions(existingVersion, candidateVersion)
	if err != nil {
		return bundledCRD{}, fmt.Errorf("compare duplicate bundled CRD %q versions: %w", existing.header.Metadata.Name, err)
	}
	if comparison < 0 {
		return candidate, nil
	}
	if comparison > 0 {
		return existing, nil
	}
	return bundledCRD{}, fmt.Errorf("conflicting duplicate bundled CRD %q has equal authoritative version %q", existing.header.Metadata.Name, existingVersion)
}

func compareNumericVersions(left, right string) (int, error) {
	parse := func(version string) ([]int, error) {
		parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid numeric version %q", version)
		}
		values := make([]int, len(parts))
		for index, part := range parts {
			value, err := strconv.Atoi(part)
			if err != nil || value < 0 {
				return nil, fmt.Errorf("invalid numeric version %q", version)
			}
			values[index] = value
		}
		return values, nil
	}
	leftParts, err := parse(left)
	if err != nil {
		return 0, err
	}
	rightParts, err := parse(right)
	if err != nil {
		return 0, err
	}
	for index := 0; index < max(len(leftParts), len(rightParts)); index++ {
		var leftPart, rightPart int
		if index < len(leftParts) {
			leftPart = leftParts[index]
		}
		if index < len(rightParts) {
			rightPart = rightParts[index]
		}
		if leftPart < rightPart {
			return -1, nil
		}
		if leftPart > rightPart {
			return 1, nil
		}
	}
	return 0, nil
}

// bundledCRDNames returns the sorted, de-duplicated CRD names in raw YAML
// (selecting the authoritative candidate on duplicate names), mirroring
// selectBundledCRDs but returning names rather than manifests.
func bundledCRDNames(raw string) ([]string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	seen := make(map[string]struct{})
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode CRD names: %w", err)
		}
		var header bundledCRDHeader
		if err := document.Decode(&header); err != nil {
			return nil, fmt.Errorf("decode CRD header: %w", err)
		}
		if header.APIVersion != "apiextensions.k8s.io/v1" || header.Kind != "CustomResourceDefinition" || header.Metadata.Name == "" {
			continue
		}
		seen[header.Metadata.Name] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// markRenderedCRDsOwned injects the incoming release's Helm ownership metadata
// into every rendered (template) CustomResourceDefinition so a fresh `helm
// install` can adopt them (mirrors Forge's markHelmAdoptableCRDs, DES-HOR-511-04).
// Scope is strict: only the rendered template MetalLB CRDs are marked for the
// incoming release/namespace; crds/-directory CRDs install via Helm's own path.
// Idempotent; preserves existing metadata.
func markRenderedCRDsOwned(rendered, release, namespace string) (string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(rendered))
	var manifests []string
	for {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("decode rendered CRD for helm adoption: %w", err)
		}
		var header bundledCRDHeader
		if err := document.Decode(&header); err != nil {
			return "", fmt.Errorf("decode rendered CRD header: %w", err)
		}
		if header.APIVersion != "apiextensions.k8s.io/v1" || header.Kind != "CustomResourceDefinition" {
			continue
		}
		// Strict founder scope (DES-HOR-511-04): only the nine MetalLB CRDs are
		// marked Helm-adoptable; other rendered template CRDs are still included in
		// the pre-apply set (established before Helm) but left unmarked.
		if strings.HasSuffix(header.Metadata.Name, ".metallb.io") {
			injectHelmOwnership(document.Content[0], release, namespace)
		}
		var resource any
		if err := document.Decode(&resource); err != nil {
			return "", fmt.Errorf("decode rendered CRD for helm adoption: %w", err)
		}
		manifest, err := yaml.Marshal(resource)
		if err != nil {
			return "", fmt.Errorf("encode rendered CRD for helm adoption: %w", err)
		}
		manifests = append(manifests, strings.TrimSpace(string(manifest)))
	}
	return strings.Join(manifests, "\n---\n") + "\n", nil
}

// injectHelmOwnership sets release-ownership annotations + managed-by label on a
// CRD's metadata node, preserving existing metadata (idempotent).
func injectHelmOwnership(root *yaml.Node, release, namespace string) {
	var meta *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "metadata" {
			meta = root.Content[i+1]
			break
		}
	}
	if meta == nil {
		meta = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "metadata"}, meta)
	}
	ensureYAMLMapKeys(meta, "annotations", map[string]string{
		"meta.helm.sh/release-name":      release,
		"meta.helm.sh/release-namespace": namespace,
	})
	ensureYAMLMapKeys(meta, "labels", map[string]string{
		"app.kubernetes.io/managed-by": "Helm",
	})
}

// ensureYAMLMapKeys sets scalar key/value pairs on a mapping node, preserving
// the existing node structure and other entries.
func ensureYAMLMapKeys(mapNode *yaml.Node, key string, values map[string]string) {
	var sub *yaml.Node
	for i := 0; i+1 < len(mapNode.Content); i += 2 {
		if mapNode.Content[i].Value == key {
			sub = mapNode.Content[i+1]
		}
	}
	if sub == nil {
		sub = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		mapNode.Content = append(mapNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, sub)
	}
	if sub.Kind != yaml.MappingNode {
		sub = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	for k, v := range values {
		var set bool
		for i := 0; i+1 < len(sub.Content); i += 2 {
			if sub.Content[i].Value == k {
				sub.Content[i+1].Value = v
				if sub.Content[i+1].Kind != yaml.ScalarNode {
					sub.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
				}
				set = true
				break
			}
		}
		if !set {
			sub.Content = append(sub.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
		}
	}
}

func currentChartVersion(t *testing.T, state *chartState, chart kube.Chart) string {
	t.Helper()
	// Keep the Helm input selection identical to installation rather than
	// trusting a parallel version constant.
	metadata := state.process(t, 60*time.Second, "helm", "show", "chart", chart.LocalPath)
	for _, line := range strings.Split(metadata, "\n") {
		if version, ok := strings.CutPrefix(line, "version:"); ok {
			return strings.Trim(strings.TrimSpace(version), `"'`)
		}
	}
	t.Fatalf("chart metadata has no version: %s", metadata)
	return ""
}

func assertReleaseChartVersion(t *testing.T, state *chartState, release, chart, version string) {
	t.Helper()
	entry, err := currentHelmHistoryEntry([]byte(state.process(t, 60*time.Second, "helm", "history", release,
		"--namespace", testNamespace, "--kubeconfig", state.cluster.Kubeconfig, "--output", "json")))
	if err != nil {
		t.Fatal(err)
	}
	want := chart + "-" + version
	if entry.Chart != want || entry.Status != "deployed" {
		t.Fatalf("%s current history chart=%q status=%q want chart=%q deployed", release, entry.Chart, entry.Status, want)
	}
}

func assertReleaseRevision(t *testing.T, state *chartState, release string, want int) {
	t.Helper()
	entry, err := currentHelmHistoryEntry([]byte(state.process(t, 60*time.Second, "helm", "history", release,
		"--namespace", testNamespace, "--kubeconfig", state.cluster.Kubeconfig, "--output", "json")))
	if err != nil {
		t.Fatal(err)
	}
	if entry.Revision != want || entry.Status != "deployed" {
		t.Fatalf("%s revision=%d status=%q want revision=%d deployed", release, entry.Revision, entry.Status, want)
	}
}

func assertRollbackReleaseHistory(t *testing.T, state *chartState, release, chart, version string, revision int) {
	t.Helper()
	entry, err := currentHelmHistoryEntry([]byte(state.process(t, 60*time.Second, "helm", "history", release,
		"--namespace", testNamespace, "--kubeconfig", state.cluster.Kubeconfig, "--output", "json")))
	if err != nil {
		t.Fatal(err)
	}
	if err := rollbackHistoryError(entry, chart, version, revision); err != nil {
		t.Fatalf("%s rollback boundary: %v", release, err)
	}
}

func rollbackHistoryError(entry helmHistoryEntry, chart, version string, revision int) error {
	wantChart := chart + "-" + version
	if entry.Revision != revision || entry.Status != "deployed" || entry.Chart != wantChart {
		return fmt.Errorf("revision=%d status=%q chart=%q want revision=%d status=deployed chart=%q",
			entry.Revision, entry.Status, entry.Chart, revision, wantChart)
	}
	return nil
}

func currentHelmHistoryEntry(data []byte) (helmHistoryEntry, error) {
	var history []helmHistoryEntry
	if err := json.Unmarshal(data, &history); err != nil {
		return helmHistoryEntry{}, fmt.Errorf("decode Helm history: %w", err)
	}
	if len(history) == 0 {
		return helmHistoryEntry{}, fmt.Errorf("Helm history is empty")
	}
	sort.Slice(history, func(i, j int) bool { return history[i].Revision < history[j].Revision })
	return history[len(history)-1], nil
}

func TestUnitBundledCRDsSelectAuthoritativeOperatorSchema(t *testing.T) {
	authoritative := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: servicemonitors.monitoring.coreos.com
  annotations:
    operator.prometheus.io/version: 0.93.0
spec:
  group: monitoring.coreos.com`
	stale := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: servicemonitors.monitoring.coreos.com
spec:
  group: stale.example.com`
	selected, err := selectBundledCRDs(stale + "\n---\n" + authoritative)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(selected, "group: monitoring.coreos.com") || strings.Contains(selected, "stale.example.com") {
		t.Fatalf("authoritative operator schema was not selected:\n%s", selected)
	}
	ambiguous := strings.ReplaceAll(stale, "stale.example.com", "first.example.com") + "\n---\n" + strings.ReplaceAll(stale, "stale.example.com", "second.example.com")
	if _, err := selectBundledCRDs(ambiguous); err == nil {
		t.Fatal("ambiguous duplicate bundled CRDs passed")
	}
}

func TestUnitArtifactProvisionerManifestRequiresOrdinaryHelmOwnership(t *testing.T) {
	valid := `apiVersion: batch/v1
kind: Job
metadata:
  name: iterabase-minio-artifact-provisioner-0-2-3
  labels:
    app.kubernetes.io/component: artifact-provisioner
    app.kubernetes.io/instance: iterabase
    app.kubernetes.io/managed-by: Helm
`
	if name, err := artifactProvisionerJobName([]byte(valid), "iterabase"); err != nil || name != "iterabase-minio-artifact-provisioner-0-2-3" {
		t.Fatalf("valid ordinary provisioner rejected: name=%q err=%v", name, err)
	}
	for name, changed := range map[string]string{
		"wrong owner": strings.Replace(valid, "managed-by: Helm", "managed-by: controller", 1),
		"hook":        strings.Replace(valid, "  labels:\n", "  annotations:\n    helm.sh/hook: post-install\n  labels:\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := artifactProvisionerJobName([]byte(changed), "iterabase"); err == nil {
				t.Fatal("invalid artifact-provisioner ownership passed")
			}
		})
	}
}

func TestUnitRetainedStateRejectsSecretPVCReapplyRolloutAndProvisionerChanges(t *testing.T) {
	baseline := lifecycleSnapshot{
		Secrets: map[string]string{"secret": "a"}, PVCs: map[string]string{"pvc": "b"}, Pods: map[string]string{"pods": "c"},
		ArtifactProvisionerJob: artifactProvisionerJobIdentity{Name: "provisioner", UID: "job-uid"},
	}
	changes := []lifecycleSnapshot{
		{Secrets: map[string]string{"secret": "changed"}, PVCs: baseline.PVCs, Pods: baseline.Pods, ArtifactProvisionerJob: baseline.ArtifactProvisionerJob},
		{Secrets: baseline.Secrets, PVCs: map[string]string{"pvc": "changed"}, Pods: baseline.Pods, ArtifactProvisionerJob: baseline.ArtifactProvisionerJob},
		{Secrets: baseline.Secrets, PVCs: baseline.PVCs, Pods: map[string]string{"pods": "changed"}, ArtifactProvisionerJob: baseline.ArtifactProvisionerJob},
		{Secrets: baseline.Secrets, PVCs: baseline.PVCs, Pods: baseline.Pods, ArtifactProvisionerJob: artifactProvisionerJobIdentity{Name: "provisioner", UID: "replacement-uid"}},
	}
	for index, changed := range changes {
		if err := retainedStateError(baseline, changed, true); err == nil {
			t.Fatalf("intentional retained-state break %d passed", index)
		}
	}
}

func TestUnitRollbackBoundaryRejectsIncorrectHelmHistory(t *testing.T) {
	entry, err := currentHelmHistoryEntry([]byte(`[{"revision":3,"status":"superseded","chart":"iterabase-platform-0.3.15"},{"revision":4,"status":"deployed","chart":"iterabase-platform-0.3.12"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if err := rollbackHistoryError(entry, "iterabase-platform", "0.3.12", 4); err != nil {
		t.Fatalf("valid rollback history rejected: %v", err)
	}
	for name, changed := range map[string]helmHistoryEntry{
		"wrong revision": {Revision: 3, Status: "deployed", Chart: "iterabase-platform-0.3.12"},
		"wrong status":   {Revision: 4, Status: "failed", Chart: "iterabase-platform-0.3.12"},
		"wrong chart":    {Revision: 4, Status: "deployed", Chart: "iterabase-platform-0.3.15"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := rollbackHistoryError(changed, "iterabase-platform", "0.3.12", 4); err == nil {
				t.Fatal("intentional rollback history break passed")
			}
		})
	}
}

func TestUnitStableWorkloadSnapshotIncludesEveryController(t *testing.T) {
	workloads := []byte(`{"items":[
		{"kind":"Deployment","metadata":{"name":"manager"},"spec":{"selector":{"matchLabels":{"component":"manager","app":"control-plane"}}}},
		{"kind":"DaemonSet","metadata":{"name":"csi"},"spec":{"selector":{"matchLabels":{"app":"csi"}}}}
	]}`)
	selectors, err := stableWorkloadSelectorsJSON(workloads)
	if err != nil {
		t.Fatal(err)
	}
	want := []stableWorkloadSelector{
		{Key: "daemonset/csi", Selector: "app=csi"},
		{Key: "deployment/manager", Selector: "app=control-plane,component=manager"},
	}
	if !slices.Equal(selectors, want) {
		t.Fatalf("stable workload selectors=%v want=%v", selectors, want)
	}
}
