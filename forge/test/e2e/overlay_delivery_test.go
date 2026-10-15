package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// The overlay delivery contract Forge's built binary must satisfy on a real
// host (DES-HOR-593-03/05). These literals are the cross-component contract
// with the tool-runner materializer; the E2E module deliberately restates them
// instead of importing Forge internals.
const (
	overlayOwnerLabel     = "iterabase.com/owner"
	overlayManagedByLabel = "app.kubernetes.io/managed-by"
	overlayPartLabel      = "iterabase.com/overlay-part"
	overlayToolsNamespace = "iterabase-overlay"
	overlayToolsPointer   = "overlay-tools"
	overlayToolsPrefix    = "overlay-tools-"
	overlayToolsKey       = "tools.tar.gz"
	overlayToolsRoot      = "tools/client"
	overlayStageRoot      = "/var/lib/forge/overlay/"

	// The fixture's inert overlay objects (overlay/crds/client/overlay-probe.yaml).
	overlayProbeNamespace = "forge-e2e-overlay"
	overlayProbeConfigMap = "overlay-probe"
	overlayChangeProbe    = "overlay-change-probe"
	overlayClashProbe     = "overlay-clash-probe"

	overlayOwnershipRefusal = "refusing to overwrite objects not owned by the overlay"
)

// overlayDelivery is what one forge apply reported delivering.
type overlayDelivery struct {
	revision    string
	toolsDigest string
	pruned      []string
}

// parseOverlayApplyResult reads Forge's apply summary overlay lines.
func parseOverlayApplyResult(out string) (overlayDelivery, error) {
	var delivery overlayDelivery
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "overlay revision:"); ok {
			delivery.revision = strings.TrimSpace(value)
		} else if value, ok := strings.CutPrefix(line, "overlay tools:"); ok {
			delivery.toolsDigest = strings.TrimSpace(value)
		} else if value, ok := strings.CutPrefix(line, "overlay pruned:"); ok {
			delivery.pruned = append(delivery.pruned, strings.TrimSpace(value))
		}
	}
	if !isCanonicalSHA256Digest(delivery.revision) || !isCanonicalSHA256Digest(delivery.toolsDigest) {
		return delivery, fmt.Errorf("apply output reports no exact overlay delivery (revision %q, tools %q)", delivery.revision, delivery.toolsDigest)
	}
	return delivery, nil
}

// expectedOverlayToolFiles is the tools/client layer the overlay at root must
// deliver: every regular, non-hidden file under each tool directory, keyed by
// its archive path tools/client/<tool>/<file>.
func expectedOverlayToolFiles(root string, toolDirs []string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, dir := range toolDirs {
		base := filepath.Join(root, dir)
		entries, err := os.ReadDir(base)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			err := filepath.WalkDir(filepath.Join(base, entry.Name()), func(file string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if strings.HasPrefix(d.Name(), ".") {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if d.IsDir() {
					return nil
				}
				rel, err := filepath.Rel(base, file)
				if err != nil {
					return err
				}
				content, err := os.ReadFile(file) //nolint:gosec // run-private overlay copy
				if err != nil {
					return err
				}
				name := path.Join(overlayToolsRoot, filepath.ToSlash(rel))
				if _, dup := files[name]; dup {
					return fmt.Errorf("tool file %s is defined twice", name)
				}
				files[name] = content
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	return files, nil
}

// readToolsArchive decodes one gzip tar and returns its regular files.
func readToolsArchive(data []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("archive entry %s is not a regular file (type %q)", header.Name, header.Typeflag)
		}
		if _, dup := files[header.Name]; dup {
			return nil, fmt.Errorf("archive entry %s is duplicated", header.Name)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		files[header.Name] = content
	}
}

func sha256BytesHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type liveConfigMap struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Immutable  *bool             `json:"immutable"`
	Data       map[string]string `json:"data"`
	BinaryData map[string]string `json:"binaryData"`
}

// checkOverlayTools verifies the tools ConfigMaps Forge wrote: exactly the
// pointer and the one immutable digest-named archive it names, holding exactly
// the expected tool files.
func checkOverlayTools(items []liveConfigMap, delivery overlayDelivery, expected map[string][]byte) error {
	archiveName := overlayToolsPrefix + strings.TrimPrefix(delivery.toolsDigest, "sha256:")
	byName := map[string]liveConfigMap{}
	for _, item := range items {
		byName[item.Metadata.Name] = item
		if err := checkOverlayLabels(item.Metadata.Labels, "tools"); err != nil {
			return fmt.Errorf("ConfigMap %s: %w", item.Metadata.Name, err)
		}
	}
	names := slices.Sorted(maps.Keys(byName))
	if !slices.Equal(names, sortedStrings(archiveName, overlayToolsPointer)) {
		return fmt.Errorf("overlay tools ConfigMaps = %v, want exactly the pointer and %s (superseded archives must be pruned)", names, archiveName)
	}
	pointer := byName[overlayToolsPointer]
	if pointer.Data["digest"] != delivery.toolsDigest || pointer.Data["archive"] != archiveName || pointer.Data["revision"] != delivery.revision {
		return fmt.Errorf("pointer data = %v, want digest %s, archive %s, revision %s", pointer.Data, delivery.toolsDigest, archiveName, delivery.revision)
	}
	archive := byName[archiveName]
	if archive.Immutable == nil || !*archive.Immutable {
		return fmt.Errorf("archive ConfigMap %s is not immutable", archiveName)
	}
	encoded, ok := archive.BinaryData[overlayToolsKey]
	if !ok || len(archive.BinaryData) != 1 || len(archive.Data) != 0 {
		return fmt.Errorf("archive ConfigMap %s must hold only binaryData %s", archiveName, overlayToolsKey)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode archive %s: %w", archiveName, err)
	}
	if actual := "sha256:" + sha256BytesHex(data); actual != delivery.toolsDigest {
		return fmt.Errorf("archive %s bytes hash to %s, pointer says %s", archiveName, actual, delivery.toolsDigest)
	}
	files, err := readToolsArchive(data)
	if err != nil {
		return fmt.Errorf("read archive %s: %w", archiveName, err)
	}
	if got, want := slices.Sorted(maps.Keys(files)), slices.Sorted(maps.Keys(expected)); !slices.Equal(got, want) {
		return fmt.Errorf("archive %s entries = %v, want %v", archiveName, got, want)
	}
	for name, content := range expected {
		if !bytes.Equal(files[name], content) {
			return fmt.Errorf("archive %s entry %s differs from the overlay", archiveName, name)
		}
	}
	return nil
}

func checkOverlayLabels(labels map[string]string, part string) error {
	if labels[overlayOwnerLabel] != "overlay" || labels[overlayManagedByLabel] != "forge" || labels[overlayPartLabel] != part {
		return fmt.Errorf("labels %v lack %s=overlay, %s=forge, %s=%s", labels, overlayOwnerLabel, overlayManagedByLabel, overlayPartLabel, part)
	}
	return nil
}

// checkStagedValueFiles verifies the host staging directory holds exactly the
// overlay value files, in forge.yaml order, then Forge's own value files.
func checkStagedValueFiles(names []string, valueFiles []string) error {
	if len(names) < len(valueFiles) {
		return fmt.Errorf("staged value files = %v, want the %d overlay files first", names, len(valueFiles))
	}
	for i := range valueFiles {
		if want := fmt.Sprintf("%02d-overlay.yaml", i); names[i] != want {
			return fmt.Errorf("staged value file %d = %q, want %q (staged: %v)", i, names[i], want, names)
		}
	}
	for i, name := range names[len(valueFiles):] {
		if want := fmt.Sprintf("%02d-forge.yaml", len(valueFiles)+i); name != want {
			return fmt.Errorf("unexpected staged file %q, want %q: the host receives only rendered value files (staged: %v)", name, want, names)
		}
	}
	return nil
}

// assertOverlayDelivered proves exactly what one apply delivered: the staged
// value files, the tools ConfigMaps, owner-labelled fixture objects, and no
// Flux or Git overlay machinery.
func assertOverlayDelivered(t *testing.T, sc *ssh.Client, installName string, plan candidateOverlayPlan, out string) overlayDelivery {
	t.Helper()
	delivery, err := parseOverlayApplyResult(out)
	if err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}

	stageDir := overlayStageRoot + installName
	names := strings.Fields(mustSSHOutput(t, sc, "sudo ls -1A "+candidateShellQuote(stageDir)))
	if err := checkStagedValueFiles(names, plan.valueFiles); err != nil {
		t.Fatal(err)
	}
	for i, file := range plan.valueFiles {
		local, err := os.ReadFile(filepath.Join(plan.path, file)) //nolint:gosec // run-private overlay copy
		if err != nil {
			t.Fatalf("read overlay value file %s: %v", file, err)
		}
		staged := strings.Fields(mustSSHOutput(t, sc, "sudo sha256sum "+candidateShellQuote(fmt.Sprintf("%s/%02d-overlay.yaml", stageDir, i))))
		if len(staged) == 0 || staged[0] != sha256BytesHex(local) {
			t.Fatalf("staged value file %02d-overlay.yaml does not match overlay %s", i, file)
		}
	}

	var tools struct {
		Items []liveConfigMap `json:"items"`
	}
	raw := mustSSHOutput(t, sc, fmt.Sprintf("sudo k3s kubectl get configmaps -n %s -l %s=overlay,%s=tools -o json", overlayToolsNamespace, overlayOwnerLabel, overlayPartLabel))
	if err := json.Unmarshal([]byte(raw), &tools); err != nil {
		t.Fatalf("decode overlay tools ConfigMaps: %v", err)
	}
	expected, err := expectedOverlayToolFiles(plan.path, plan.toolDirs)
	if err != nil {
		t.Fatalf("read overlay tools: %v", err)
	}
	if err := checkOverlayTools(tools.Items, delivery, expected); err != nil {
		t.Fatal(err)
	}

	assertOverlayOwned(t, sc, "namespace", "", overlayProbeNamespace)
	assertOverlayOwned(t, sc, "configmap", overlayProbeNamespace, overlayProbeConfigMap)

	absent := mustSSHOutput(t, sc, "sudo k3s kubectl get namespace flux-system --ignore-not-found -o name; sudo k3s kubectl get crd gitrepositories.source.toolkit.fluxcd.io kustomizations.kustomize.toolkit.fluxcd.io --ignore-not-found -o name")
	if strings.TrimSpace(absent) != "" {
		t.Fatalf("Forge no longer installs Flux (DES-HOR-593-04), but the cluster has:\n%s", absent)
	}
	t.Logf("overlay revision %s delivered tools %s (%d files)", delivery.revision, delivery.toolsDigest, len(expected))
	return delivery
}

// assertOverlayOwned requires one live object to carry the overlay owner labels.
func assertOverlayOwned(t *testing.T, sc *ssh.Client, kind, namespace, name string) {
	t.Helper()
	command := fmt.Sprintf("sudo k3s kubectl get %s %s -o json", kind, candidateShellQuote(name))
	if namespace != "" {
		command += " -n " + candidateShellQuote(namespace)
	}
	var object struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(mustSSHOutput(t, sc, command)), &object); err != nil {
		t.Fatalf("decode %s/%s: %v", kind, name, err)
	}
	if err := checkOverlayLabels(object.Metadata.Labels, "objects"); err != nil {
		t.Fatalf("overlay object %s %s/%s: %v", kind, namespace, name, err)
	}
}

// toolRunnerPodsCommand prints the tool runner pod names of one release.
func toolRunnerPodsCommand(namespace, release string) string {
	return fmt.Sprintf("sudo k3s kubectl get pods -n %s -l app.kubernetes.io/name=control-plane,app.kubernetes.io/instance=%s,app.kubernetes.io/component=tool-runner -o jsonpath='{.items[*].metadata.name}'",
		candidateShellQuote(namespace), candidateShellQuote(release))
}

// expectedClientLayerListing is the sha256sum listing of the client layer the
// materializer must publish, as `find . -type f | sort | sha256sum` prints it
// from inside tools/client.
func expectedClientLayerListing(files map[string][]byte) string {
	names := slices.Sorted(maps.Keys(files))
	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, sha256BytesHex(files[name])+"  ./"+strings.TrimPrefix(name, overlayToolsRoot+"/"))
	}
	return strings.Join(lines, "\n")
}

// logHasEvent reports whether a JSON log stream has one event with every
// given field value.
func logHasEvent(logs, event string, fields map[string]string) bool {
	for _, line := range strings.Split(logs, "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &record) != nil || record["event"] != event {
			continue
		}
		match := true
		for key, want := range fields {
			if got, _ := record[key].(string); got != want {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// assertOverlayToolsLoaded proves every tool runner pod of the release
// materialized exactly the delivered client layer and activated it with the
// gateway.
func assertOverlayToolsLoaded(t *testing.T, sc *ssh.Client, namespace, release string, files map[string][]byte, toolsDigest string, timeout time.Duration) {
	t.Helper()
	mustSSHOutput(t, sc, fmt.Sprintf("sudo k3s kubectl rollout status -n %s deployment/%s-tool-runner --timeout=300s", candidateShellQuote(namespace), release))
	pods := strings.Fields(mustSSHOutput(t, sc, toolRunnerPodsCommand(namespace, release)))
	if len(pods) == 0 {
		t.Fatalf("release %s has no tool runner pods", release)
	}
	want := expectedClientLayerListing(files)
	for _, pod := range pods {
		exec := fmt.Sprintf("sudo k3s kubectl exec -n %s %s -c runner -- ", candidateShellQuote(namespace), candidateShellQuote(pod))
		deadline := time.Now().Add(timeout)
		var current struct {
			ArtifactDigest string `json:"artifactDigest"`
			Directory      string `json:"directory"`
		}
		var listing string
		for {
			raw := strings.TrimSpace(mustSSHOutput(t, sc, exec+"sh -c 'if test -f /control/current.json; then cat /control/current.json; fi'"))
			if raw != "" {
				if err := json.Unmarshal([]byte(raw), &current); err != nil {
					t.Fatalf("decode %s current generation: %v\n%s", pod, err, raw)
				}
				listing = strings.TrimSpace(mustSSHOutput(t, sc, exec+candidateShellQuote("sh")+" -c "+candidateShellQuote(
					`cd "$1/tools/client" && find . -type f | LC_ALL=C sort | while IFS= read -r f; do sha256sum "$f"; done`)+" sh "+candidateShellQuote(current.Directory)))
				if listing == want {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("tool runner pod %s did not publish the delivered client layer within %s\ncurrent: %s\nlisting:\n%s\nwant:\n%s", pod, timeout, raw, listing, want)
			}
			time.Sleep(5 * time.Second)
		}
		materializer := mustSSHOutput(t, sc, fmt.Sprintf("sudo k3s kubectl logs -n %s %s -c materializer", candidateShellQuote(namespace), candidateShellQuote(pod)))
		if !logHasEvent(materializer, "generation_materialized", map[string]string{"digest": current.ArtifactDigest, "clientDigest": toolsDigest}) {
			t.Fatalf("materializer in %s never materialized generation %s from client archive %s:\n%s", pod, current.ArtifactDigest, toolsDigest, materializer)
		}
		for {
			runner := mustSSHOutput(t, sc, fmt.Sprintf("sudo k3s kubectl logs -n %s %s -c runner", candidateShellQuote(namespace), candidateShellQuote(pod)))
			if logHasEvent(runner, "generation_activated", map[string]string{"digest": current.ArtifactDigest}) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("tool runner %s never activated generation %s within %s:\n%s", pod, current.ArtifactDigest, timeout, runner)
			}
			time.Sleep(5 * time.Second)
		}
		t.Logf("tool runner %s activated generation %s with client archive %s", pod, current.ArtifactDigest, toolsDigest)
	}
}

// assertOverlayToolsLoadedStage proves the platform's tool runner loads the
// fixture's client tool from the archive the first apply delivered.
func assertOverlayToolsLoadedStage(t *testing.T, state *cpuFixtureState) {
	t.Helper()
	if state.overlay.toolsDigest == "" {
		t.Fatal("no overlay delivery was recorded by the install stage")
	}
	files, err := expectedOverlayToolFiles(candidateOverlayFixture, candidateOverlayToolDirs)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[overlayToolsRoot+"/validation-echo/manifest.json"]; !ok {
		t.Fatalf("fixture lost its client tool canary: %v", slices.Sorted(maps.Keys(files)))
	}
	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer sc.Close()
	assertOverlayToolsLoaded(t, sc, "iterabase-system", state.runID, files, state.overlay.toolsDigest, 5*time.Minute)
}

// addOverlayObject writes one extra object file into a run-private overlay
// copy and lists it in crds/client.
func addOverlayObject(t *testing.T, plan candidateOverlayPlan, file, manifest string) {
	t.Helper()
	client := filepath.Join(plan.path, "crds", "client")
	if err := os.WriteFile(filepath.Join(client, file), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	kustomization := filepath.Join(client, "kustomization.yaml")
	content, err := os.ReadFile(kustomization) //nolint:gosec // run-private overlay copy
	if err != nil {
		t.Fatal(err)
	}
	const anchor = "  - overlay-probe.yaml\n"
	if !bytes.Contains(content, []byte(anchor)) {
		t.Fatalf("fixture crds/client/kustomization.yaml no longer lists overlay-probe.yaml:\n%s", content)
	}
	content = bytes.Replace(content, []byte(anchor), []byte(anchor+"  - "+file+"\n"), 1)
	if err := os.WriteFile(kustomization, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func overlayProbeConfigMapManifest(name, owner string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: %s\ndata:\n  owner: %s\n", name, overlayProbeNamespace, owner)
}

// reapplyDeliversOverlayChangeStage proves a forge apply re-run is how an
// overlay change reaches the installation: a changed tools layer becomes a new
// archive the runner loads (and the superseded archive is pruned), a new
// object is applied with the owner labels, and removing it from the overlay
// prunes it live. The chart is not re-applied; it already runs.
func reapplyDeliversOverlayChangeStage(t *testing.T, state *cpuFixtureState) {
	t.Helper()
	if state.overlay.toolsDigest == "" {
		t.Fatal("no overlay delivery was recorded by the install stage")
	}
	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer sc.Close()

	changed := prepareCandidateOverlay(t)
	addOverlayObject(t, changed, overlayChangeProbe+".yaml", overlayProbeConfigMapManifest(overlayChangeProbe, "overlay"))
	notes := filepath.Join(changed.path, "tools", "client", "validation-echo", "NOTES.md")
	if err := os.WriteFile(notes, []byte("Forge E2E overlay change "+state.runID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := applyOnceArgs(t, state.forgeBin, state.forgeHome,
		writeCurrentOverlayForgeConfig(t, state.runID, state.ip, state.privKeyPath, state.chartVersion, changed), "--skip-chart")
	t.Logf("changed overlay apply output:\n%s", out)
	delivery := assertOverlayDelivered(t, sc, state.runID, changed, out)
	if delivery.toolsDigest == state.overlay.toolsDigest {
		t.Fatalf("a changed tools layer kept the archive digest %s", delivery.toolsDigest)
	}
	if len(delivery.pruned) != 0 {
		t.Fatalf("adding overlay content pruned objects: %v", delivery.pruned)
	}
	assertOverlayOwned(t, sc, "configmap", overlayProbeNamespace, overlayChangeProbe)
	files, err := expectedOverlayToolFiles(changed.path, changed.toolDirs)
	if err != nil {
		t.Fatal(err)
	}
	assertOverlayToolsLoaded(t, sc, "iterabase-system", state.runID, files, delivery.toolsDigest, 5*time.Minute)

	original := prepareCandidateOverlay(t)
	out = applyOnceArgs(t, state.forgeBin, state.forgeHome,
		writeCurrentOverlayForgeConfig(t, state.runID, state.ip, state.privKeyPath, state.chartVersion, original), "--skip-chart")
	t.Logf("restored overlay apply output:\n%s", out)
	restored := assertOverlayDelivered(t, sc, state.runID, original, out)
	if restored.toolsDigest != state.overlay.toolsDigest {
		t.Fatalf("the unchanged fixture tools packed to %s, first delivery was %s: the archive is not deterministic", restored.toolsDigest, state.overlay.toolsDigest)
	}
	wantPruned := []string{"ConfigMap/" + overlayProbeNamespace + "/" + overlayChangeProbe}
	if !slices.Equal(restored.pruned, wantPruned) {
		t.Fatalf("removing %s from the overlay pruned %v, want %v", overlayChangeProbe, restored.pruned, wantPruned)
	}
	if live := strings.TrimSpace(mustSSHOutput(t, sc, fmt.Sprintf("sudo k3s kubectl get configmap %s -n %s --ignore-not-found -o name", overlayChangeProbe, overlayProbeNamespace))); live != "" {
		t.Fatalf("pruned overlay object still exists: %s", live)
	}
	originalFiles, err := expectedOverlayToolFiles(original.path, original.toolDirs)
	if err != nil {
		t.Fatal(err)
	}
	assertOverlayToolsLoaded(t, sc, "iterabase-system", state.runID, originalFiles, restored.toolsDigest, 5*time.Minute)
}

// refuseInstallationOwnedClashStage proves an overlay object never overwrites
// a live object it does not own: apply fails, names the object, and leaves it
// unchanged.
func refuseInstallationOwnedClashStage(t *testing.T, state *cpuFixtureState) {
	t.Helper()
	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer sc.Close()
	mustSSHOutput(t, sc, fmt.Sprintf("sudo k3s kubectl create configmap %s -n %s --from-literal=owner=installation", overlayClashProbe, overlayProbeNamespace))

	clash := prepareCandidateOverlay(t)
	addOverlayObject(t, clash, overlayClashProbe+".yaml", overlayProbeConfigMapManifest(overlayClashProbe, "overlay"))
	out, applyErr := runForgeE(state.forgeBin, state.forgeHome, "apply", "--config",
		writeCurrentOverlayForgeConfig(t, state.runID, state.ip, state.privKeyPath, state.chartVersion, clash), "--skip-chart")
	if applyErr == nil {
		t.Fatalf("forge apply overwrote an installation-owned object:\n%s", out)
	}
	if !strings.Contains(out, overlayOwnershipRefusal) || !strings.Contains(out, "ConfigMap/"+overlayProbeNamespace+"/"+overlayClashProbe) {
		t.Fatalf("ownership refusal did not name the installation-owned object:\n%s", out)
	}
	var live liveConfigMap
	if err := json.Unmarshal([]byte(mustSSHOutput(t, sc, fmt.Sprintf("sudo k3s kubectl get configmap %s -n %s -o json", overlayClashProbe, overlayProbeNamespace))), &live); err != nil {
		t.Fatalf("decode installation-owned ConfigMap: %v", err)
	}
	if live.Data["owner"] != "installation" || live.Metadata.Labels[overlayOwnerLabel] != "" {
		t.Fatalf("refused apply changed the installation-owned object: data=%v labels=%v", live.Data, live.Metadata.Labels)
	}
	mustSSHOutput(t, sc, fmt.Sprintf("sudo k3s kubectl delete configmap %s -n %s --wait=true --timeout=1m", overlayClashProbe, overlayProbeNamespace))
}

func sortedStrings(values ...string) []string {
	slices.Sort(values)
	return values
}
