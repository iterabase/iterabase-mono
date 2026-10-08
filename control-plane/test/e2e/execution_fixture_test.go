package e2e_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fluxInstallManifest = "https://github.com/fluxcd/flux2/releases/download/v2.4.0/install.yaml"

type toolFixture struct {
	name         string
	version      string
	digest       string
	bundle       []byte
	manifest     []byte
	bundlePath   string
	manifestPath string
}

func installExecutionFixtureStage(t *testing.T, state *deployedState) {
	t.Helper()
	installRuntimeBackend(t, state)
	state.kubectl(t, 3*time.Minute, "apply", "-f", fluxInstallManifest)
	state.kubectl(t, 4*time.Minute, "wait", "-n", "flux-system", "--for=condition=Available",
		"deployment/source-controller", "deployment/kustomize-controller", "--timeout=180s")

	v1 := writeToolGeneration(t, "1.0.0", false)
	v2 := writeToolGeneration(t, "2.0.0", true)
	state.toolDigests = map[string]string{}
	for _, tool := range v1 {
		state.toolDigests[tool.name] = tool.digest
	}
	state.toolV2 = v2
	installToolGitServer(t, state, v1)
	state.applyYAML(t, "tool-source.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: overlay
  namespace: flux-system
spec:
  interval: 2s
  url: ssh://git@tool-git.default.svc/git/repo
  ref: {branch: master}
  secretRef: {name: overlay-git-auth}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-tool-materializer
  namespace: flux-system
spec:
  podSelector:
    matchLabels: {app: source-controller}
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels: {kubernetes.io/metadata.name: iterabase-system}
          podSelector:
            matchLabels: {app.kubernetes.io/component: tool-runner}
      ports: [{protocol: TCP, port: 9090}]
`)
	state.kubectl(t, 4*time.Minute, "wait", "-n", "flux-system", "--for=condition=Ready", "gitrepository/overlay", "--timeout=180s")
	state.fluxRevision = state.kubectl(t, 30*time.Second, "get", "gitrepository", "overlay", "-n", "flux-system", "-o", "jsonpath={.status.artifact.revision}")
	state.fluxDigest = state.kubectl(t, 30*time.Second, "get", "gitrepository", "overlay", "-n", "flux-system", "-o", "jsonpath={.status.artifact.digest}")
	if state.fluxRevision == "" || !canonicalDigest(state.fluxDigest) {
		t.Fatalf("Flux did not publish an exact tool artifact: revision=%q digest=%q", state.fluxRevision, state.fluxDigest)
	}
	state.writeJSON(t, "execution-fixture-identity.json", map[string]any{
		"source_sha": os.Getenv("ITERABASE_E2E_SOURCE_SHA"), "flux_revision": state.fluxRevision, "flux_digest": state.fluxDigest,
		"images": []any{imageEvidence(state.harnessImage), imageEvidence(state.toolRunnerImage), imageEvidence(state.inferenceImage), imageEvidence(state.runtimeImage)},
	})
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
	// CI supplies every image as an exact archive imported before install.
	pullPolicy := "Never"
	values := map[string]any{
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
				"flux":                  map[string]any{"namespace": "flux-system", "sourceName": "overlay", "pollMs": 1000},
			},
			"artifact": map[string]any{"enabled": true},
			"ingress":  map[string]any{"enabled": false},
		},
	}
	state.installPlatformValues(t, "execution-platform-values.json", values, 18*time.Minute)
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
	state.captureBootstrapKeys(t)
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
	root := t.TempDir()
	out := make([]toolFixture, 0, len(definitions))
	for _, definition := range definitions {
		dir := filepath.Join(root, "tools", "product", strings.TrimPrefix(definition.name, "platform."))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create tool fixture: %v", err)
		}
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
		bundlePath := filepath.Join(dir, "index.mjs")
		manifestPath := filepath.Join(dir, "manifest.json")
		if err := os.WriteFile(bundlePath, []byte(definition.bundle), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifestPath, append(manifestJSON, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		out = append(out, toolFixture{
			name: definition.name, version: version, digest: digest,
			bundle: []byte(definition.bundle), manifest: append([]byte(nil), manifestJSON...),
			bundlePath: bundlePath, manifestPath: manifestPath,
		})
	}
	return out
}

func installToolGitServer(t *testing.T, state *deployedState, tools []toolFixture) {
	t.Helper()
	clientPublic, clientPrivate := sshKeyPair(t)
	hostPublic, hostPrivate := sshKeyPair(t)
	keys := t.TempDir()
	clientPrivatePath := filepath.Join(keys, "identity")
	knownHostsPath := filepath.Join(keys, "known_hosts")
	hostPrivatePath := filepath.Join(keys, "ssh_host_ed25519_key")
	clientPublicPath := filepath.Join(keys, "authorized_keys")
	mustWriteMode(t, clientPrivatePath, clientPrivate, 0o600)
	mustWriteMode(t, knownHostsPath, []byte("tool-git.default.svc "+strings.TrimSpace(string(hostPublic))+"\n"), 0o644)
	mustWriteMode(t, hostPrivatePath, hostPrivate, 0o600)
	mustWriteMode(t, clientPublicPath, clientPublic, 0o644)

	args := []string{"create", "configmap", "tool-repo-seed"}
	for _, tool := range tools {
		short := strings.TrimPrefix(tool.name, "platform.")
		args = append(args, "--from-file="+short+"-index.mjs="+tool.bundlePath, "--from-file="+short+"-manifest.json="+tool.manifestPath)
	}
	state.kubectl(t, 30*time.Second, args...)
	state.kubectl(t, 30*time.Second, "create", "configmap", "tool-git-authorized", "--from-file=authorized_keys="+clientPublicPath)
	state.kubectl(t, 30*time.Second, "create", "secret", "generic", "tool-git-host", "--from-file=ssh_host_ed25519_key="+hostPrivatePath)
	state.kubectl(t, 30*time.Second, "create", "secret", "generic", "overlay-git-auth", "-n", "flux-system",
		"--from-file=identity="+clientPrivatePath, "--from-file=known_hosts="+knownHostsPath)
	state.applyYAML(t, "tool-git.yaml", fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata: {name: tool-git, namespace: default}
spec:
  replicas: 1
  selector: {matchLabels: {app: tool-git}}
  template:
    metadata: {labels: {app: tool-git}}
    spec:
      containers:
        - name: git
          image: %s
          imagePullPolicy: Never
          securityContext: {runAsUser: 0, runAsGroup: 0}
          command: ["/bin/sh", "-ceu"]
          args:
            - |
              mkdir -p /git/repo/tools/product
              for manifest in /seed/*-manifest.json; do
                short=$(basename "$manifest" -manifest.json)
                mkdir -p "/git/repo/tools/product/$short"
                cp "$manifest" "/git/repo/tools/product/$short/manifest.json"
                cp "/seed/$short-index.mjs" "/git/repo/tools/product/$short/index.mjs"
              done
              cd /git/repo
              git init -b master
              git config user.name test
              git config user.email test@example.com
              git add . && git commit -m v1
              adduser -D -h /home/git -s /usr/bin/git-shell git
              passwd -d git
              chown -R git:git /git/repo
              mkdir -p /run/sshd
              exec /usr/sbin/sshd -D -e -o HostKey=/host/ssh_host_ed25519_key -o AuthorizedKeysFile=/auth/authorized_keys -o PasswordAuthentication=no -o PermitRootLogin=no -o StrictModes=no -o AllowUsers=git
          ports: [{containerPort: 22}]
          volumeMounts:
            - {name: seed, mountPath: /seed, readOnly: true}
            - {name: repo, mountPath: /git}
            - {name: host, mountPath: /host, readOnly: true}
            - {name: auth, mountPath: /auth, readOnly: true}
      volumes:
        - {name: seed, configMap: {name: tool-repo-seed}}
        - {name: repo, emptyDir: {}}
        - {name: host, secret: {secretName: tool-git-host, defaultMode: 256}}
        - {name: auth, configMap: {name: tool-git-authorized, defaultMode: 292}}
---
apiVersion: v1
kind: Service
metadata: {name: tool-git, namespace: default}
spec:
  selector: {app: tool-git}
  ports: [{port: 22, targetPort: 22}]
`, state.runtimeImage.reference()))
	state.kubectl(t, 3*time.Minute, "rollout", "status", "deployment/tool-git", "-n", "default", "--timeout=120s")
}

func publishToolGeneration(t *testing.T, state *deployedState, tools []toolFixture) {
	t.Helper()
	pod := state.kubectl(t, 30*time.Second, "get", "pods", "-n", "default", "-l", "app=tool-git", "-o", "jsonpath={.items[0].metadata.name}")
	if pod == "" {
		t.Fatal("tool Git server pod is unavailable")
	}
	for _, tool := range tools {
		short := strings.TrimPrefix(tool.name, "platform.")
		dir := filepath.Join(t.TempDir(), short)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create generation staging directory: %v", err)
		}
		bundlePath := filepath.Join(dir, "index.mjs")
		manifestPath := filepath.Join(dir, "manifest.json")
		mustWriteMode(t, bundlePath, tool.bundle, 0o644)
		mustWriteMode(t, manifestPath, append(append([]byte(nil), tool.manifest...), '\n'), 0o644)
		destination := "default/" + pod + ":/git/repo/tools/product/" + short + "/"
		state.kubectl(t, time.Minute, "cp", bundlePath, destination+"index.mjs")
		state.kubectl(t, time.Minute, "cp", manifestPath, destination+"manifest.json")
	}
	state.kubectl(t, time.Minute, "exec", "-n", "default", pod, "--", "/bin/sh", "-ceu",
		"git config --global --add safe.directory /git/repo; cd /git/repo; git add .; git commit -m v2; chown -R git:git /git/repo")
	state.kubectl(t, 30*time.Second, "annotate", "gitrepository/overlay", "-n", "flux-system",
		fmt.Sprintf("reconcile.fluxcd.io/requestedAt=%d", time.Now().UnixNano()), "--overwrite")
}

func sshKeyPair(t *testing.T) ([]byte, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "id_ed25519")
	output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "e2e", "-f", path).CombinedOutput()
	if err != nil {
		t.Fatalf("generate SSH key: %v\n%s", err, output)
	}
	public, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatalf("read SSH public key: %v", err)
	}
	private, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read SSH private key: %v", err)
	}
	return public, private
}

func mustWriteMode(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func (state *deployedState) applyYAML(t *testing.T, name, content string) {
	t.Helper()
	path := state.writeManifest(t, name, content)
	state.kubectl(t, 2*time.Minute, "apply", "-f", path)
}
