package e2e_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func installObservabilityToolSourceStage(t *testing.T, state *chartState) {
	t.Helper()
	archive, digest, err := buildObservabilityToolArchive()
	if err != nil {
		t.Fatalf("build observability tool archive: %v", err)
	}
	archivePath := filepath.Join(t.TempDir(), "artifact.tgz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatalf("write observability tool archive: %v", err)
	}

	state.kubectl(t, 2*time.Minute, "apply", "-f", state.writeManifest(t, "observability-tool-source-api.yaml", `apiVersion: v1
kind: Namespace
metadata:
  name: flux-system
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: gitrepositories.source.toolkit.fluxcd.io
spec:
  group: source.toolkit.fluxcd.io
  scope: Namespaced
  names:
    plural: gitrepositories
    singular: gitrepository
    kind: GitRepository
    listKind: GitRepositoryList
  versions:
    - name: v1
      served: true
      storage: true
      subresources: {status: {}}
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
`))
	state.kubectl(t, 2*time.Minute, "wait", "--for=condition=Established", "crd/gitrepositories.source.toolkit.fluxcd.io", "--timeout=90s")
	state.kubectl(t, 30*time.Second, "create", "configmap", "observability-tool-artifact", "-n", observabilityToolServerNamespace,
		"--from-file=artifact.tgz="+archivePath)
	state.kubectl(t, 2*time.Minute, "apply", "-f", state.writeManifest(t, "observability-tool-source-server.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: observability-tool-source
  namespace: flux-system
spec:
  replicas: 1
  selector:
    matchLabels: {app: observability-tool-source}
  template:
    metadata:
      labels: {app: observability-tool-source}
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: server
          image: busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0
          command: [httpd, -f, -p, "8080", -h, /www]
          ports: [{name: http, containerPort: 8080}]
          readinessProbe: {tcpSocket: {port: http}}
          securityContext:
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
          resources:
            requests: {cpu: 5m, memory: 8Mi}
            limits: {cpu: 100m, memory: 32Mi}
          volumeMounts: [{name: artifact, mountPath: /www, readOnly: true}]
      volumes:
        - name: artifact
          configMap: {name: observability-tool-artifact}
---
apiVersion: v1
kind: Service
metadata:
  name: source-controller
  namespace: flux-system
spec:
  selector: {app: observability-tool-source}
  ports: [{name: http, port: 80, targetPort: http}]
`))
	state.kubectl(t, 3*time.Minute, "rollout", "status", "deployment/observability-tool-source", "-n", observabilityToolServerNamespace, "--timeout=2m")
	state.kubectl(t, 30*time.Second, "apply", "-f", state.writeManifest(t, "observability-tool-source.yaml", fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: %s
  namespace: %s
spec: {}
`, observabilityToolSourceName, testNamespace)))
	status := fmt.Sprintf(`{"status":{"artifact":{"revision":"main@sha1:0000000000000000000000000000000000000000","digest":%q,"url":"http://source-controller.flux-system.svc/artifact.tgz"}}}`, digest)
	state.kubectl(t, 30*time.Second, "patch", "gitrepository/"+observabilityToolSourceName, "-n", testNamespace,
		"--subresource=status", "--type=merge", "-p", status)
	if got := state.kubectl(t, 30*time.Second, "get", "gitrepository/"+observabilityToolSourceName, "-n", testNamespace,
		"-o", "jsonpath={.status.artifact.digest}"); got != digest {
		t.Fatalf("observability tool source digest=%q want=%q", got, digest)
	}
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
		{name: "tools/product/observability-fixture/index.mjs", data: bundle},
		{name: "tools/product/observability-fixture/manifest.json", data: manifest},
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
	bundle := entries["tools/product/observability-fixture/index.mjs"]
	manifestBytes := entries["tools/product/observability-fixture/manifest.json"]
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

func TestUnitObservabilityCandidateUsesMaterializableToolSource(t *testing.T) {
	data, err := os.ReadFile(strings.TrimPrefix(observabilityToolRunnerValues, "test/e2e/"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		ControlPlane struct {
			ToolRunner struct {
				Enabled bool `yaml:"enabled"`
				Flux    struct {
					Namespace  string `yaml:"namespace"`
					SourceName string `yaml:"sourceName"`
				} `yaml:"flux"`
			} `yaml:"toolRunner"`
		} `yaml:"control-plane"`
	}
	if err := yaml.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}
	toolRunner := values.ControlPlane.ToolRunner
	if !toolRunner.Enabled || toolRunner.Flux.Namespace != testNamespace || toolRunner.Flux.SourceName != observabilityToolSourceName {
		t.Fatalf("candidate tool source=%+v", toolRunner)
	}
	declared := observabilityCandidateValues(observabilityPlatform, false, true)
	if !slices.Contains(declared, observabilityToolRunnerValues) {
		t.Fatalf("tool-runner candidate values do not include %s: %v", observabilityToolRunnerValues, declared)
	}
}
