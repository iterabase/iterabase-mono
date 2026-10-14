package e2e_test

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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The overlay tools contract Forge writes (DES-HOR-593-05): a dedicated
// namespace, an immutable digest-named archive ConfigMap, and a pointer.
const (
	overlayToolsNamespace     = "iterabase-overlay"
	overlayToolsPointer       = "overlay-tools"
	overlayToolsArchivePrefix = "overlay-tools-"
	overlayToolsArchiveKey    = "tools.tar.gz"
)

type toolFixture struct {
	name     string
	version  string
	digest   string
	bundle   []byte
	manifest []byte
}

func installExecutionFixtureStage(t *testing.T, state *deployedState) {
	t.Helper()
	installRuntimeBackend(t, state)
	// The platform installs product-only first (HOR-651): no overlay
	// namespace, no overlay source, no client tools. enableClientToolLayerStage adds them later.
	state.toolDigests = map[string]string{productDiagnosticTool: productToolDigest(t, "diagnostic")}
	state.toolV2 = writeToolGeneration(t, "2.0.0", true)
}

// enableClientToolLayerStage turns the product-only install into the
// customer shape: the overlay's tools/client layer is written as Forge writes
// it and a platform upgrade enables the runner's overlay source.
func enableClientToolLayerStage(t *testing.T, state *deployedState) {
	t.Helper()
	v1 := writeToolGeneration(t, "1.0.0", false)
	publishToolGeneration(t, state, "e2e-overlay-v1", v1)
	state.writeJSON(t, "execution-fixture-identity.json", map[string]any{
		"source_sha": os.Getenv("ITERABASE_E2E_SOURCE_SHA"), "overlay_revision": state.overlayRevision, "overlay_digest": state.overlayDigest,
		"images": []any{imageEvidence(state.harnessImage), imageEvidence(state.toolRunnerImage), imageEvidence(state.inferenceImage), imageEvidence(state.runtimeImage)},
	})

	state.installPlatformValues(t, "execution-platform-client-values.json", executionPlatformValues(state, true), 18*time.Minute)
	state.kubectl(t, 7*time.Minute, "rollout", "status", "deployment/iterabase-tool-runner", "-n", controlPlaneNamespace, "--timeout=6m")
	for _, tool := range v1 {
		state.toolDigests[tool.name] = tool.digest
	}
	for name, digest := range state.toolDigests {
		query := fmt.Sprintf("SELECT count(*) FROM toolgateway.runner_registrations WHERE tool_name='%s' AND tool_digest='%s' AND active AND accepting_new", name, digest)
		waitForDatabaseValue(t, state, query, "1", 3*time.Minute)
	}
}

func imageEvidence(image deployedImage) map[string]string {
	return map[string]string{"name": image.name, "reference": image.reference(), "digest": image.digest}
}

func canonicalDigest(value string) bool {
	return len(value) == 71 && strings.HasPrefix(value, "sha256:") && onlyHex(value[7:])
}

func onlyHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}

func installRuntimeBackend(t *testing.T, state *deployedState) {
	t.Helper()
	state.applyYAML(t, "runtime-backend.yaml", fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: deterministic-model
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels: {app: deterministic-model}
  template:
    metadata:
      labels: {app: deterministic-model}
    spec:
      containers:
        - name: model
          image: %s
          imagePullPolicy: Never
          ports: [{name: http, containerPort: 8080}]
          readinessProbe: {httpGet: {path: /health, port: http}}
          securityContext:
            runAsNonRoot: true
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
---
apiVersion: v1
kind: Service
metadata:
  name: deterministic-model
  namespace: default
spec:
  selector: {app: deterministic-model}
  ports: [{name: http, port: 8080, targetPort: http}]
`, state.runtimeImage.reference()))
	state.kubectl(t, 3*time.Minute, "rollout", "status", "deployment/deterministic-model", "-n", "default", "--timeout=120s")
}

func installExecutionPlatformStage(t *testing.T, state *deployedState) {
	t.Helper()
	state.installPlatformValues(t, "execution-platform-values.json", executionPlatformValues(state, false), 18*time.Minute)
}

// executionPlatformValues renders the execution install. clientTools=false is
// the fresh product-only install: the tool runner serves only the product
// layer from its image and the chart renders no overlay source or credential.
func executionPlatformValues(state *deployedState, clientTools bool) map[string]any {
	// CI supplies every image as an exact archive imported before install.
	pullPolicy := "Never"
	return map[string]any{
		"global":         map[string]any{"internalTLS": map[string]any{"enabled": true}},
		"external-dns":   map[string]any{"enabled": false},
		"redis":          map[string]any{"enabled": true},
		"minio":          map[string]any{"enabled": true, "artifactService": map[string]any{"enabled": true}},
		"ingress-nginx":  map[string]any{"enabled": false},
		"metallb":        map[string]any{"enabled": false},
		"metallb-config": map[string]any{"enabled": false},
		"reloader":       map[string]any{"enabled": false},
		"inference-gateway": map[string]any{
			"enabled":  true,
			"image":    map[string]any{"repository": state.inferenceImage.repository, "tag": state.inferenceImage.tag, "pullPolicy": pullPolicy},
			"ingress":  map[string]any{"enabled": false},
			"workload": map[string]any{"enabled": true},
		},
		"control-plane": map[string]any{
			"image":    map[string]any{"repository": state.imageRepo, "tag": state.imageTag, "pullPolicy": pullPolicy},
			"gateway":  map[string]any{"enabled": true},
			"dispatch": map[string]any{"enabled": true, "defaultModel": map[string]any{"id": "e2e-model", "api": "openai-completions"}},
			"toolRunner": map[string]any{
				"enabled":               true,
				"image":                 map[string]any{"repository": state.toolRunnerImage.repository, "tag": state.toolRunnerImage.tag, "pullPolicy": pullPolicy},
				"allowedToolNamespaces": []string{"platform"},
				"overlayTools":          map[string]any{"enabled": clientTools, "namespace": overlayToolsNamespace, "pollMs": 1000},
			},
			"artifact": map[string]any{"enabled": true},
			"ingress":  map[string]any{"enabled": false},
			"auth":     authValues(),
		},
	}
}

func assertExecutionPlatformReadyStage(t *testing.T, state *deployedState) {
	t.Helper()
	for _, workload := range []string{
		"deployment/iterabase-control-plane-api", "deployment/iterabase-control-plane-manager",
		"deployment/iterabase-control-plane-dispatch", "deployment/iterabase-control-plane-gateway",
		"deployment/iterabase-gateway", "deployment/iterabase-tool-runner",
	} {
		state.kubectl(t, 7*time.Minute, "rollout", "status", workload, "-n", controlPlaneNamespace, "--timeout=6m")
	}
	state.kubectl(t, 7*time.Minute, "rollout", "status", "statefulset/iterabase-postgresql", "-n", controlPlaneNamespace, "--timeout=6m")
	state.kubectl(t, 7*time.Minute, "rollout", "status", "deployment/iterabase-redis", "-n", controlPlaneNamespace, "--timeout=6m")
	state.kubectl(t, 7*time.Minute, "rollout", "status", "statefulset/iterabase-minio", "-n", controlPlaneNamespace, "--timeout=6m")
	assertComponentImage(t, state, "app.kubernetes.io/name=control-plane", state.imageDigest, []string{"api", "manager", "dispatch", "gateway", "migrate", "bootstrap"})
	assertComponentImage(t, state, "app.kubernetes.io/name=inference-gateway", state.inferenceImage.digest, []string{"gateway"})
	assertComponentImage(t, state, "app.kubernetes.io/component=tool-runner", state.toolRunnerImage.digest, []string{"materializer", "runner"})
	state.openAPI(t)
	state.ensurePrincipals(t)
	assertWorkloadRejectsAnonymousCaller(t, state)
}

func assertComponentImage(t *testing.T, state *deployedState, selector, digest string, names []string) {
	t.Helper()
	lines := state.kubectl(t, 30*time.Second, "get", "pods", "-n", controlPlaneNamespace, "-l", selector, "-o",
		`jsonpath={range .items[*].status.containerStatuses[*]}{.name}={.imageID}{"\n"}{end}{range .items[*].status.initContainerStatuses[*]}{.name}={.imageID}{"\n"}{end}`)
	found := map[string]bool{}
	for _, line := range strings.Split(lines, "\n") {
		name, imageID, ok := strings.Cut(line, "=")
		if !ok || !strings.Contains(imageID, digest) {
			continue
		}
		found[name] = true
	}
	for _, name := range names {
		if !found[name] {
			t.Fatalf("%s container %s does not run exact digest %s: %s", selector, name, digest, lines)
		}
	}
}

func writeToolGeneration(t *testing.T, version string, v2 bool) []toolFixture {
	t.Helper()
	readLabel := "v1"
	if v2 {
		readLabel = "v2"
	}
	definitions := []struct {
		name, effect, bundle string
		extra                map[string]any
	}{
		{
			name: "platform.fixture_read", effect: "read_only",
			bundle: fmt.Sprintf(`export const identity={name:"platform.fixture_read",version:%q};
export async function invoke(context,args){
 const credential=context.credentials.fixture_token;
 if(!credential||credential.scheme!=="bearer"||credential.value!=="synthetic-e2e-token") throw new Error("credential mismatch");
 const bytes=new TextEncoder().encode("%s:"+String(args.message||""));
 async function* chunks(){yield bytes;}
 const ref=await context.artifacts.write({mimeType:"text/plain",expectedSizeBytes:bytes.length,bytes:chunks()});
 return {result:{generation:%q,credential:"resolved"},artifactRefs:[ref]};
}
`, version, readLabel, readLabel),
			extra: map[string]any{
				"credentialSlots":      []any{map[string]any{"name": "fixture_token", "scheme": "bearer", "required": true}},
				"artifactCapabilities": map[string]any{"writesArtifacts": true, "acceptedMimeTypes": []string{"text/plain"}},
			},
		},
		{
			name: "platform.fixture_barrier", effect: "read_only",
			bundle: fmt.Sprintf(`export const identity={name:"platform.fixture_barrier",version:%q};
const waiters=[];
export async function invoke(){
 return await new Promise((resolve,reject)=>{
  const entry={resolve,reject,timer:null};
  entry.timer=setTimeout(()=>{const i=waiters.indexOf(entry);if(i>=0)waiters.splice(i,1);reject(new Error("concurrency barrier timeout"));},8000);
  waiters.push(entry);
  if(waiters.length>=2){
   const batch=waiters.splice(0,waiters.length);
   for(const waiter of batch){clearTimeout(waiter.timer);waiter.resolve({result:{simultaneous:true,participants:batch.length}});}
  }
 });
}
`, version),
			extra: map[string]any{"timeoutMs": 10000},
		},
		{
			name: "platform.fixture_upsert", effect: "idempotent_write",
			bundle: fmt.Sprintf(`export const identity={name:"platform.fixture_upsert",version:%q};
export async function invoke(context,args){
 const bytes=new TextEncoder().encode("upsert:"+context.idempotencyKey);
 async function* chunks(){yield bytes;}
 const ref=await context.artifacts.write({mimeType:"text/plain",expectedSizeBytes:bytes.length,bytes:chunks()});
 return {result:{idempotencyKey:context.idempotencyKey,args},artifactRefs:[ref]};
}
`, version),
			extra: map[string]any{
				"artifactCapabilities":       map[string]any{"writesArtifacts": true, "acceptedMimeTypes": []string{"text/plain"}},
				"idempotencyProof":           map[string]any{"strategy": "upstream_key", "description": "The fixture persists the gateway key.", "upstreamKeyHeader": "Idempotency-Key"},
				"consequenceSummaryTemplate": map[string]any{"localizedTemplates": map[string]string{"en": "Update the synthetic record", "pt": "Atualizar o registo sintético"}, "argumentPaths": map[string]string{}},
			},
		},
		{
			name: "platform.fixture_write", effect: "non_idempotent_write",
			bundle: fmt.Sprintf(`export const identity={name:"platform.fixture_write",version:%q};
export async function invoke(context,args){
 const bytes=new TextEncoder().encode("effect:"+String(args.target||"unknown"));
 async function* chunks(){yield bytes;}
 const ref=await context.artifacts.write({mimeType:"text/plain",expectedSizeBytes:bytes.length,bytes:chunks()});
 if(args.mode==="crash") process.exit(23);
 return {result:{written:true,target:args.target},artifactRefs:[ref]};
}
`, version),
			extra: map[string]any{
				"artifactCapabilities":       map[string]any{"writesArtifacts": true, "acceptedMimeTypes": []string{"text/plain"}},
				"consequenceSummaryTemplate": map[string]any{"localizedTemplates": map[string]string{"en": "Write synthetic target {{target}}", "pt": "Escrever destino sintético {{target}}"}, "argumentPaths": map[string]string{"target": "/target"}},
			},
		},
	}
	if v2 {
		definitions = definitions[:1]
	}
	out := make([]toolFixture, 0, len(definitions))
	for _, definition := range definitions {
		projection := map[string]any{
			"apiVersion": "iterabase.io/tool/v1", "name": definition.name, "version": version,
			"description": "Deterministic deployed E2E fixture", "bundle": "index.mjs",
			"inputSchema": map[string]any{"type": "object"}, "effectClass": definition.effect, "timeoutMs": 3000,
		}
		for key, value := range definition.extra {
			projection[key] = value
		}
		canonical, err := json.Marshal(projection)
		if err != nil {
			t.Fatalf("marshal tool projection: %v", err)
		}
		hash := sha256.New()
		_, _ = hash.Write(canonical)
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(definition.bundle))
		digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
		manifest := map[string]any{}
		for key, value := range projection {
			manifest[key] = value
		}
		manifest["digest"] = digest
		manifestJSON, _ := json.Marshal(manifest)
		out = append(out, toolFixture{
			name: definition.name, version: version, digest: digest,
			bundle: []byte(definition.bundle), manifest: append(manifestJSON, '\n'),
		})
	}
	return out
}

// productDiagnosticTool is the reference release-versioned product tool
// (HOR-651). It ships inside the tool-runner image under test, never in the
// overlay fixture, so its digest comes from the same source tree.
const productDiagnosticTool = "platform.diagnostic"

func productToolDigest(t *testing.T, short string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "tool-runner", "tools", "product", short, "manifest.json"))
	if err != nil {
		t.Fatalf("read product tool manifest %s: %v", short, err)
	}
	var manifest struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil || !canonicalDigest(manifest.Digest) {
		t.Fatalf("product tool manifest %s has no canonical digest: %v", short, err)
	}
	return manifest.Digest
}

// publishToolGeneration does what `forge apply` does for an overlay's
// tools/client tree: it updates the cumulative client layer, packs it into one
// deterministic gzip tar, writes the immutable digest-named archive ConfigMap,
// then moves the pointer to it. The archive precedes the pointer, so the
// materializer never observes a pointer to a missing archive.
func publishToolGeneration(t *testing.T, state *deployedState, revision string, tools []toolFixture) {
	t.Helper()
	if state.clientTools == nil {
		state.clientTools = map[string]toolFixture{}
	}
	for _, tool := range tools {
		state.clientTools[strings.TrimPrefix(tool.name, "platform.")] = tool
	}
	archive, digest, err := overlayToolsArchive(state.clientTools)
	if err != nil {
		t.Fatalf("build overlay tools archive: %v", err)
	}
	name := overlayToolsArchivePrefix + strings.TrimPrefix(digest, "sha256:")
	labels := map[string]string{"app.kubernetes.io/managed-by": "forge", "iterabase.com/owner": "overlay"}
	objects := []map[string]any{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": overlayToolsNamespace}},
		{
			"apiVersion": "v1", "kind": "ConfigMap", "immutable": true,
			"metadata":   map[string]any{"name": name, "namespace": overlayToolsNamespace, "labels": labels},
			"binaryData": map[string]string{overlayToolsArchiveKey: base64.StdEncoding.EncodeToString(archive)},
		},
	}
	pointer := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": overlayToolsPointer, "namespace": overlayToolsNamespace, "labels": labels},
		"data":     map[string]string{"digest": digest, "archive": name, "revision": revision},
	}
	for index, object := range append(objects, pointer) {
		raw, err := json.Marshal(object)
		if err != nil {
			t.Fatalf("marshal overlay object: %v", err)
		}
		path := state.writeManifest(t, fmt.Sprintf("overlay-tools-%s-%d.json", revision, index), string(raw))
		state.kubectl(t, time.Minute, "apply", "--server-side", "--field-manager=forge-e2e", "-f", path)
	}
	got := state.kubectl(t, 30*time.Second, "get", "configmap", overlayToolsPointer, "-n", overlayToolsNamespace, "-o", "jsonpath={.data.digest}")
	if got != digest || !canonicalDigest(got) {
		t.Fatalf("overlay pointer digest=%q want=%q", got, digest)
	}
	state.overlayRevision, state.overlayDigest = revision, digest
}

// overlayToolsArchive packs tools/client/<tool>/{index.mjs,manifest.json} in
// sorted order with fixed metadata, so equal layers yield equal digests.
func overlayToolsArchive(tools map[string]toolFixture) ([]byte, string, error) {
	var output bytes.Buffer
	compressor := gzip.NewWriter(&output)
	compressor.ModTime = time.Unix(0, 0)
	archive := tar.NewWriter(compressor)
	for _, short := range slices.Sorted(maps.Keys(tools)) {
		for _, file := range []struct {
			name string
			data []byte
		}{{"index.mjs", tools[short].bundle}, {"manifest.json", tools[short].manifest}} {
			header := &tar.Header{Name: "tools/client/" + short + "/" + file.name, Mode: 0o444, Size: int64(len(file.data)), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}
			if err := archive.WriteHeader(header); err != nil {
				return nil, "", err
			}
			if _, err := archive.Write(file.data); err != nil {
				return nil, "", err
			}
		}
	}
	if err := archive.Close(); err != nil {
		return nil, "", err
	}
	if err := compressor.Close(); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(output.Bytes())
	return output.Bytes(), "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (state *deployedState) applyYAML(t *testing.T, name, content string) {
	t.Helper()
	path := state.writeManifest(t, name, content)
	state.kubectl(t, 2*time.Minute, "apply", "-f", path)
}

// overlayArchiveEntries lists the entry names of a base64 overlay archive.
func overlayArchiveEntries(encoded string) ([]string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	var names []string
	entries := tar.NewReader(reader)
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("archive entry %s is not a regular file", header.Name)
		}
		names = append(names, header.Name)
	}
}

func TestOverlayToolsArchiveFollowsForgeLayout(t *testing.T) {
	tools := map[string]toolFixture{}
	for _, tool := range writeToolGeneration(t, "1.0.0", false) {
		tools[strings.TrimPrefix(tool.name, "platform.")] = tool
	}
	archive, digest, err := overlayToolsArchive(tools)
	if err != nil {
		t.Fatal(err)
	}
	again, againDigest, err := overlayToolsArchive(maps.Clone(tools))
	if err != nil || againDigest != digest || !bytes.Equal(again, archive) || !canonicalDigest(digest) {
		t.Fatalf("archive is not deterministic: %s vs %s (%v)", digest, againDigest, err)
	}
	if len(archive) >= 1<<20 {
		t.Fatalf("archive is %d bytes; Forge caps it below the 1 MiB ConfigMap limit", len(archive))
	}
	names, err := overlayArchiveEntries(base64.StdEncoding.EncodeToString(archive))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "tools/client/") {
			t.Fatalf("archive entry %s is outside tools/client", name)
		}
	}
	if len(names) != 2*len(tools) || !slices.IsSorted(names) {
		t.Fatalf("archive entries=%v", names)
	}
}
