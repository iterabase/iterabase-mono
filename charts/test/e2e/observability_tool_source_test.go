package e2e_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// installObservabilityToolSourceStage writes exactly what Forge writes for an
// overlay's tools/client layer (DES-HOR-593-05): the dedicated overlay
// namespace, the immutable digest-named archive ConfigMap, and the
// overlay-tools pointer naming it.
func installObservabilityToolSourceStage(t *testing.T, state *chartState) {
	t.Helper()
	archive, digest, err := buildObservabilityToolArchive()
	if err != nil {
		t.Fatalf("build observability tool archive: %v", err)
	}
	state.kubectl(t, 2*time.Minute, "apply", "-f", state.writeManifest(t, "observability-tool-source.yaml", observabilityOverlayManifest(archive, digest)))
	if got := state.kubectl(t, 30*time.Second, "get", "configmap/"+observabilityOverlayPointer, "-n", observabilityOverlayNamespace,
		"-o", "jsonpath={.data.digest}"); got != digest {
		t.Fatalf("observability overlay pointer digest=%q want=%q", got, digest)
	}
}

// observabilityOverlayManifest renders the overlay namespace, archive, and
// pointer ConfigMaps for one exact archive.
func observabilityOverlayManifest(archive []byte, digest string) string {
	name := observabilityOverlayArchivePrefix + strings.TrimPrefix(digest, "sha256:")
	labels := "\n    app.kubernetes.io/managed-by: forge\n    iterabase.com/owner: overlay"
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: %[2]s
  namespace: %[1]s
  labels:%[3]s
immutable: true
binaryData:
  %[4]s: %[5]s
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: %[6]s
  namespace: %[1]s
  labels:%[3]s
data:
  digest: %[7]q
  archive: %[2]q
  revision: "observability-e2e"
`, observabilityOverlayNamespace, name, labels, observabilityOverlayArchiveKey, base64.StdEncoding.EncodeToString(archive), observabilityOverlayPointer, digest)
}

func buildObservabilityToolArchive() ([]byte, string, error) {
	bundle := []byte(`export const identity={name:"platform.observability_fixture",version:"1.0.0"};export async function invoke(_context,args){return {result:args};}
`)
	projection := map[string]any{
		"apiVersion": "iterabase.io/tool/v1",
		"name":       "platform.observability_fixture", "version": "1.0.0",
		"description": "Observability runtime fixture", "bundle": "index.mjs",
		"inputSchema": map[string]any{"type": "object"}, "effectClass": "read_only", "timeoutMs": 1000,
	}
	canonical, err := json.Marshal(projection)
	if err != nil {
		return nil, "", fmt.Errorf("marshal canonical tool projection: %w", err)
	}
	toolHash := sha256.New()
	_, _ = toolHash.Write(canonical)
	_, _ = toolHash.Write([]byte{0})
	_, _ = toolHash.Write(bundle)
	projection["digest"] = fmt.Sprintf("sha256:%x", toolHash.Sum(nil))
	manifest, err := json.Marshal(projection)
	if err != nil {
		return nil, "", fmt.Errorf("marshal tool manifest: %w", err)
	}

	var output bytes.Buffer
	compressor, err := gzip.NewWriterLevel(&output, gzip.BestCompression)
	if err != nil {
		return nil, "", fmt.Errorf("create gzip writer: %w", err)
	}
	compressor.ModTime = time.Unix(0, 0)
	archive := tar.NewWriter(compressor)
	files := []struct {
		name string
		data []byte
	}{
		{name: "tools/client/observability-fixture/index.mjs", data: bundle},
		{name: "tools/client/observability-fixture/manifest.json", data: manifest},
	}
	for _, file := range files {
		header := &tar.Header{Name: file.name, Mode: 0o444, Size: int64(len(file.data)), ModTime: time.Unix(0, 0)}
		if err := archive.WriteHeader(header); err != nil {
			return nil, "", fmt.Errorf("write archive header %s: %w", file.name, err)
		}
		if _, err := archive.Write(file.data); err != nil {
			return nil, "", fmt.Errorf("write archive file %s: %w", file.name, err)
		}
	}
	if err := archive.Close(); err != nil {
		return nil, "", fmt.Errorf("close tar archive: %w", err)
	}
	if err := compressor.Close(); err != nil {
		return nil, "", fmt.Errorf("close gzip archive: %w", err)
	}
	digest := sha256.Sum256(output.Bytes())
	return output.Bytes(), fmt.Sprintf("sha256:%x", digest), nil
}

func TestUnitObservabilityToolFixturePublishesCanonicalArchive(t *testing.T) {
	archive, digest, err := buildObservabilityToolArchive()
	if err != nil {
		t.Fatal(err)
	}
	actual := sha256.Sum256(archive)
	if got := fmt.Sprintf("sha256:%x", actual); got != digest {
		t.Fatalf("archive digest=%q want=%q", got, digest)
	}
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	entries := map[string][]byte{}
	tarReader := tar.NewReader(reader)
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		data, readErr := io.ReadAll(tarReader)
		if readErr != nil {
			t.Fatal(readErr)
		}
		entries[header.Name] = data
	}
	bundle := entries["tools/client/observability-fixture/index.mjs"]
	manifestBytes := entries["tools/client/observability-fixture/manifest.json"]
	if len(bundle) == 0 || len(manifestBytes) == 0 {
		t.Fatalf("tool archive entries=%v", entries)
	}
	var manifest map[string]any
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	claimed, _ := manifest["digest"].(string)
	delete(manifest, "digest")
	canonical, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, _ = hash.Write(canonical)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(bundle)
	if want := fmt.Sprintf("sha256:%x", hash.Sum(nil)); claimed != want {
		t.Fatalf("tool digest=%q want=%q", claimed, want)
	}
}

func TestUnitObservabilityOverlayManifestFollowsForgeContract(t *testing.T) {
	archive, digest, err := buildObservabilityToolArchive()
	if err != nil {
		t.Fatal(err)
	}
	if len(archive) >= 1<<20 {
		t.Fatalf("archive is %d bytes; Forge caps it below the 1 MiB ConfigMap limit", len(archive))
	}
	type object struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name      string            `yaml:"name"`
			Namespace string            `yaml:"namespace"`
			Labels    map[string]string `yaml:"labels"`
		} `yaml:"metadata"`
		Immutable  bool              `yaml:"immutable"`
		Data       map[string]string `yaml:"data"`
		BinaryData map[string]string `yaml:"binaryData"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(observabilityOverlayManifest(archive, digest)))
	objects := map[string]object{}
	for {
		var next object
		if err := decoder.Decode(&next); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		objects[next.Kind+"/"+next.Metadata.Name] = next
	}
	if _, ok := objects["Namespace/"+observabilityOverlayNamespace]; !ok || len(objects) != 3 {
		t.Fatalf("overlay objects=%v", objects)
	}
	pointer := objects["ConfigMap/"+observabilityOverlayPointer]
	archiveName := pointer.Data["archive"]
	stored := objects["ConfigMap/"+archiveName]
	decoded, err := base64.StdEncoding.DecodeString(stored.BinaryData[observabilityOverlayArchiveKey])
	if err != nil {
		t.Fatal(err)
	}
	actual := fmt.Sprintf("sha256:%x", sha256.Sum256(decoded))
	if pointer.Data["digest"] != actual || archiveName != observabilityOverlayArchivePrefix+strings.TrimPrefix(actual, "sha256:") ||
		!stored.Immutable || pointer.Data["revision"] == "" || !bytes.Equal(decoded, archive) {
		t.Fatalf("pointer=%+v archive=%+v digest=%s", pointer, stored, actual)
	}
	for _, configMap := range []object{pointer, stored} {
		if configMap.Metadata.Namespace != observabilityOverlayNamespace ||
			configMap.Metadata.Labels["app.kubernetes.io/managed-by"] != "forge" || configMap.Metadata.Labels["iterabase.com/owner"] != "overlay" {
			t.Fatalf("overlay ConfigMap metadata=%+v", configMap.Metadata)
		}
	}
}

func TestUnitObservabilityCandidateUsesMaterializableToolSource(t *testing.T) {
	data, err := os.ReadFile(strings.TrimPrefix(observabilityToolRunnerValues, "test/e2e/"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		ControlPlane struct {
			ToolRunner struct {
				Enabled      bool `yaml:"enabled"`
				OverlayTools struct {
					Enabled   bool   `yaml:"enabled"`
					Namespace string `yaml:"namespace"`
				} `yaml:"overlayTools"`
			} `yaml:"toolRunner"`
		} `yaml:"control-plane"`
	}
	if err := yaml.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}
	toolRunner := values.ControlPlane.ToolRunner
	if !toolRunner.Enabled || !toolRunner.OverlayTools.Enabled || toolRunner.OverlayTools.Namespace != observabilityOverlayNamespace {
		t.Fatalf("candidate tool source=%+v", toolRunner)
	}
	declared := observabilityCandidateValues(observabilityPlatform, false, true)
	if !slices.Contains(declared, observabilityToolRunnerValues) {
		t.Fatalf("tool-runner candidate values do not include %s: %v", observabilityToolRunnerValues, declared)
	}
}
