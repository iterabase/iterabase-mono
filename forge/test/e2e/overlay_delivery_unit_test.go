package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseOverlayApplyResult(t *testing.T) {
	revision, tools := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	out := "apply complete\n  action:     skip\n  overlay:        /tmp/overlay\n  overlay revision: " + revision +
		"\n  overlay tools:  " + tools + "\n  overlay pruned: ConfigMap/ns/a\n  overlay pruned: Workflow/ns/b\n"
	delivery, err := parseOverlayApplyResult(out)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.revision != revision || delivery.toolsDigest != tools || !slices.Equal(delivery.pruned, []string{"ConfigMap/ns/a", "Workflow/ns/b"}) {
		t.Fatalf("parsed %+v", delivery)
	}
	if _, err := parseOverlayApplyResult("apply complete\n  action:     skip\n"); err == nil {
		t.Fatal("an apply without overlay lines must not count as an overlay delivery")
	}
}

// packTestTools mirrors the delivery contract: sorted regular-file entries in a
// gzip tar.
func packTestTools(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(gz)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func toolsConfigMaps(t *testing.T, data []byte, revision string) (overlayDelivery, []liveConfigMap) {
	t.Helper()
	digest := "sha256:" + sha256BytesHex(data)
	labels := map[string]string{overlayOwnerLabel: "overlay", overlayManagedByLabel: "forge", overlayPartLabel: "tools"}
	immutable := true
	var archive, pointer liveConfigMap
	archive.Metadata.Name, archive.Metadata.Labels = overlayToolsPrefix+strings.TrimPrefix(digest, "sha256:"), labels
	archive.Immutable = &immutable
	archive.BinaryData = map[string]string{overlayToolsKey: base64.StdEncoding.EncodeToString(data)}
	pointer.Metadata.Name, pointer.Metadata.Labels = overlayToolsPointer, labels
	pointer.Data = map[string]string{"digest": digest, "archive": archive.Metadata.Name, "revision": revision}
	return overlayDelivery{revision: revision, toolsDigest: digest}, []liveConfigMap{pointer, archive}
}

func TestCheckOverlayToolsAcceptsOnlyTheExactDelivery(t *testing.T) {
	files, err := expectedOverlayToolFiles(candidateOverlayFixture, candidateOverlayToolDirs)
	if err != nil {
		t.Fatal(err)
	}
	revision := "sha256:" + strings.Repeat("c", 64)
	delivery, items := toolsConfigMaps(t, packTestTools(t, files), revision)
	if err := checkOverlayTools(items, delivery, files); err != nil {
		t.Fatalf("exact delivery rejected: %v", err)
	}

	stale, staleItems := toolsConfigMaps(t, packTestTools(t, map[string][]byte{"tools/client/old/manifest.json": []byte("{}")}), revision)
	if err := checkOverlayTools(append(items, staleItems[1]), delivery, files); err == nil {
		t.Fatalf("a superseded archive %s left beside the current one was accepted", stale.toolsDigest)
	}
	changed := maps.Clone(files)
	changed["tools/client/validation-echo/NOTES.md"] = []byte("extra")
	if err := checkOverlayTools(items, delivery, changed); err == nil {
		t.Fatal("an archive missing an overlay file was accepted")
	}
	wrongPointer := slices.Clone(items)
	wrongPointer[0].Data = map[string]string{"digest": delivery.toolsDigest, "archive": items[1].Metadata.Name, "revision": "sha256:" + strings.Repeat("d", 64)}
	if err := checkOverlayTools(wrongPointer, delivery, files); err == nil {
		t.Fatal("a pointer with another revision was accepted")
	}
	mutable := slices.Clone(items)
	no := false
	mutable[1].Immutable = &no
	if err := checkOverlayTools(mutable, delivery, files); err == nil {
		t.Fatal("a mutable archive ConfigMap was accepted")
	}
	unlabelled := slices.Clone(items)
	unlabelled[0].Metadata.Labels = map[string]string{overlayOwnerLabel: "overlay"}
	if err := checkOverlayTools(unlabelled, delivery, files); err == nil {
		t.Fatal("a pointer without the full ownership labels was accepted")
	}
}

func TestCheckStagedValueFiles(t *testing.T) {
	valueFiles := []string{"values.yaml", "values.client.yaml", "values.e2e.yaml"}
	for _, ok := range [][]string{
		{"00-overlay.yaml", "01-overlay.yaml", "02-overlay.yaml"},
		{"00-overlay.yaml", "01-overlay.yaml", "02-overlay.yaml", "03-forge.yaml"},
	} {
		if err := checkStagedValueFiles(ok, valueFiles); err != nil {
			t.Fatalf("%v rejected: %v", ok, err)
		}
	}
	for _, bad := range [][]string{
		{"00-overlay.yaml", "01-overlay.yaml"},
		{"00-overlay.yaml", "01-overlay.yaml", "02-overlay.yaml", ".git"},
		{"00-overlay.yaml", "01-overlay.yaml", "02-overlay.yaml", "values.yaml"},
		{"00-forge.yaml", "01-overlay.yaml", "02-overlay.yaml"},
	} {
		if err := checkStagedValueFiles(bad, valueFiles); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
}

func TestExpectedClientLayerListingMatchesSha256sum(t *testing.T) {
	listing := expectedClientLayerListing(map[string][]byte{
		"tools/client/b/index.mjs":     []byte("b"),
		"tools/client/a/manifest.json": []byte("a"),
	})
	want := sha256BytesHex([]byte("a")) + "  ./a/manifest.json\n" + sha256BytesHex([]byte("b")) + "  ./b/index.mjs"
	if listing != want {
		t.Fatalf("listing = %q, want %q", listing, want)
	}
}

func TestLogHasEvent(t *testing.T) {
	logs := "starting\n" +
		`{"event":"generation_materialized","digest":"sha256:g1","clientDigest":"sha256:c1"}` + "\n" +
		`{"event":"generation_activated","digest":"sha256:g1","tools":1}` + "\n"
	if !logHasEvent(logs, "generation_materialized", map[string]string{"digest": "sha256:g1", "clientDigest": "sha256:c1"}) {
		t.Fatal("matching materialization not found")
	}
	if logHasEvent(logs, "generation_materialized", map[string]string{"clientDigest": "sha256:c2"}) {
		t.Fatal("materialization of another archive matched")
	}
	if logHasEvent(logs, "generation_activated", map[string]string{"digest": "sha256:g2"}) {
		t.Fatal("activation of another generation matched")
	}
}

// TestFixtureOverlayMatchesLocalRenderContract keeps the versioned fixture in
// the shape Forge renders (DES-HOR-593-03/05) and preview.py consumes.
func TestFixtureOverlayMatchesLocalRenderContract(t *testing.T) {
	root, err := os.ReadFile(filepath.Join(candidateOverlayFixture, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("fixture overlay has no root kustomization: %v", err)
	}
	if !strings.Contains(string(root), "resources:\n  - crds/client\n") {
		t.Fatalf("root kustomization must compose crds/client:\n%s", root)
	}
	client, err := os.ReadFile(filepath.Join(candidateOverlayFixture, "crds", "client", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(client), "  - ../base\n  - overlay-probe.yaml\n") {
		t.Fatalf("crds/client must compose ../base and the overlay probe:\n%s", client)
	}
	probe, err := os.ReadFile(filepath.Join(candidateOverlayFixture, "crds", "client", "overlay-probe.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: Namespace\nmetadata:\n  name: " + overlayProbeNamespace, "kind: ConfigMap\nmetadata:\n  name: " + overlayProbeConfigMap + "\n  namespace: " + overlayProbeNamespace} {
		if !strings.Contains(string(probe), want) {
			t.Fatalf("overlay probe lacks %q:\n%s", want, probe)
		}
	}
	if strings.Contains(string(probe), overlayOwnerLabel) {
		t.Fatal("the fixture must not set ownership labels itself; Forge sets them")
	}
	for _, file := range []string{"secrets.yaml", "tools/product"} {
		if _, err := os.Stat(filepath.Join(candidateOverlayFixture, file)); err == nil {
			t.Fatalf("fixture overlay must not carry %s", file)
		}
	}
	err = filepath.WalkDir(candidateOverlayFixture, func(path string, _ os.DirEntry, err error) error {
		if err != nil || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		contents, err := os.ReadFile(path) //nolint:gosec // versioned fixture file
		if err != nil {
			return err
		}
		if strings.Contains(string(contents), "kind: Secret") {
			t.Fatalf("%s declares a Secret manifest; Forge rejects them (use secrets.yaml)", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, valueFile := range []string{"values.yaml", "values.client.yaml"} {
		if _, err := os.Stat(filepath.Join(candidateOverlayFixture, valueFile)); err != nil {
			t.Fatalf("fixture overlay lacks value file %s: %v", valueFile, err)
		}
	}
}

// TestFixtureToolManifestsCarryTheirDigest recomputes each fixture tool's
// digest as the tool runner does (canonical manifest without digest, NUL,
// bundle), so a manifest edit cannot ship a tool the runner rejects.
func TestFixtureToolManifestsCarryTheirDigest(t *testing.T) {
	files, err := expectedOverlayToolFiles(candidateOverlayFixture, candidateOverlayToolDirs)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]bool{}
	for name := range files {
		tools[strings.Split(strings.TrimPrefix(name, overlayToolsRoot+"/"), "/")[0]] = true
	}
	if len(tools) == 0 {
		t.Fatal("fixture overlay has no client tools")
	}
	for tool := range tools {
		raw := files[overlayToolsRoot+"/"+tool+"/manifest.json"]
		var manifest map[string]any
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatalf("%s manifest: %v", tool, err)
		}
		claimed, _ := manifest["digest"].(string)
		bundleName, _ := manifest["bundle"].(string)
		delete(manifest, "digest")
		var canonical bytes.Buffer
		encoder := json.NewEncoder(&canonical)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(manifest); err != nil {
			t.Fatal(err)
		}
		bundle, ok := files[overlayToolsRoot+"/"+tool+"/"+bundleName]
		if !ok {
			t.Fatalf("%s bundle %q is missing", tool, bundleName)
		}
		hash := sha256.New()
		hash.Write(bytes.TrimSuffix(canonical.Bytes(), []byte("\n")))
		hash.Write([]byte{0})
		hash.Write(bundle)
		if actual := "sha256:" + hex.EncodeToString(hash.Sum(nil)); actual != claimed {
			t.Fatalf("%s manifest digest %s, actual %s", tool, claimed, actual)
		}
	}
}
