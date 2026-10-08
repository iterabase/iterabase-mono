package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	sharede2e "github.com/nunocgoncalves/iterabase-mono/testkit/e2e"
	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/poll"
)

// freshInstallPlatform is the platform values the fresh-install scenario
// installs, layered before the runtime image and Kind address overlay.
var freshInstallPlatform = platformValues{platformValuesBase, "test/e2e/values/fresh-install.yaml"}

func freshInstallScenario() sharede2e.Definition {
	diagnostics, cleanup := scenarioHooks()
	metadata := chartScenarioMetadata(
		"fresh-install",
		"Installs ordered certificate and pinned OpenEBS LVM volume-only substrates plus class-isolated public/private ingress planes, then proves exact classes, claims, the inert LVMSnapshot deletion-safety boundary, CSI/user snapshot absence, manager, issuer, workload identity, fixed private allocation, route isolation, and verified gateway readiness.",
		"test-e2e-install", 45,
		[]string{"HOR-408", "HOR-414", "HOR-416", "HOR-475", "HOR-545", "HOR-557", "HOR-590", "DES-HOR-545-01", "DES-HOR-545-05", "DES-HOR-545-07"},
		append(substrateRenders("iterabase"), freshInstallPlatform.render()),
	)
	// The suite's single smoke scenario: CI-only changes run exactly this one.
	metadata.Smoke = true
	return sharede2e.Define(sharede2e.Scenario[*chartState]{
		Metadata: metadata,
		NewState: newChartState,
		Stages: []sharede2e.Stage[*chartState]{
			{Name: "create-kind", Run: createKindStage},
			{Name: "import-runtime-images", DependsOn: []string{"create-kind"}, Run: importRuntimeImagesStage},
			{Name: "install-certificate-substrate", DependsOn: []string{"import-runtime-images"}, Run: installCertificateSubstrateStage},
			{Name: "install-lvm-storage-substrate", DependsOn: []string{"install-certificate-substrate"}, Run: installLVMStorageStage},
			{Name: "install-minimal-platform-edge", DependsOn: []string{"install-lvm-storage-substrate"}, Run: installMinimalPlatformEdgeStage},
			{Name: "assert-openebs-lvm-claims", DependsOn: []string{"install-minimal-platform-edge"}, Run: assertOpenEBSLVMClaimsStage},
			{Name: "assert-storage-snapshot-boundary", DependsOn: []string{"assert-openebs-lvm-claims"}, Run: assertStorageSnapshotBoundaryStage},
			{Name: "assert-manager-contract", DependsOn: []string{"assert-openebs-lvm-claims"}, Run: assertManagerContractStage},
			{Name: "assert-certificate-issuer", DependsOn: []string{"install-minimal-platform-edge"}, Run: assertCertificateIssuerStage},
			{Name: "assert-workload-identity", DependsOn: []string{"assert-certificate-issuer"}, Run: assertWorkloadIdentityStage},
			{Name: "assert-verified-edge", DependsOn: []string{"install-minimal-platform-edge", "assert-certificate-issuer"}, Run: assertVerifiedEdgeStage},
			{Name: "assert-private-ingress-plane", DependsOn: []string{"install-minimal-platform-edge", "assert-certificate-issuer"}, Run: assertPrivateIngressPlaneStage},
		},
		Diagnostics: diagnostics,
		Cleanup:     cleanup,
	})
}

func installCertificateSubstrateStage(t *testing.T, state *chartState) {
	state.installSubstrate(t)
}

func assertStorageSnapshotBoundaryStage(t *testing.T, state *chartState) {
	t.Helper()
	if got := strings.TrimSpace(state.kubectl(t, 30*time.Second, "get", "crd/lvmsnapshots.local.openebs.io", "-o", "name")); got != "customresourcedefinition.apiextensions.k8s.io/lvmsnapshots.local.openebs.io" {
		t.Fatalf("inert LVMSnapshot CRD identity = %q", got)
	}
	if got := strings.TrimSpace(state.kubectl(t, 30*time.Second, "get", "lvmsnapshots.local.openebs.io", "-A", "-o", "name")); got != "" {
		t.Fatalf("inert LVMSnapshot API contains forbidden instances: %s", got)
	}
	for _, name := range []string{
		"volumesnapshotclasses.snapshot.storage.k8s.io",
		"volumesnapshotcontents.snapshot.storage.k8s.io",
		"volumesnapshots.snapshot.storage.k8s.io",
	} {
		if got := strings.TrimSpace(state.kubectl(t, 30*time.Second, "get", "crd/"+name, "--ignore-not-found=true", "-o", "name")); got != "" {
			t.Fatalf("volume-only runtime exposes forbidden CSI snapshot CRD: %s", got)
		}
	}
	for _, resource := range []string{"clusterrole/openebs-lvm-snapshotter-role", "clusterrolebinding/openebs-lvm-snapshotter-binding"} {
		if got := strings.TrimSpace(state.kubectl(t, 30*time.Second, "get", resource, "--ignore-not-found=true", "-o", "name")); got != "" {
			t.Fatalf("volume-only runtime exposes forbidden broad snapshot RBAC: %s", got)
		}
	}
	policy := state.kubectl(t, 30*time.Second, "get", "validatingadmissionpolicy/iterabase-lvmsnapshot-create-deny", "-o", `jsonpath={.spec.failurePolicy}|{.spec.matchConstraints.resourceRules[0].apiGroups[0]}|{.spec.matchConstraints.resourceRules[0].apiVersions[0]}|{.spec.matchConstraints.resourceRules[0].operations[0]}|{.spec.matchConstraints.resourceRules[0].resources[0]}|{.spec.validations[0].expression}`)
	if policy != "Fail|local.openebs.io|v1alpha1|CREATE|lvmsnapshots|false" {
		t.Fatalf("LVMSnapshot deny policy contract = %q", policy)
	}
	binding := state.kubectl(t, 30*time.Second, "get", "validatingadmissionpolicybinding/iterabase-lvmsnapshot-create-deny", "-o", `jsonpath={.spec.policyName}|{.spec.validationActions[0]}`)
	if binding != "iterabase-lvmsnapshot-create-deny|Deny" {
		t.Fatalf("LVMSnapshot deny binding contract = %q", binding)
	}
	blocked := `apiVersion: local.openebs.io/v1alpha1
kind: LVMSnapshot
metadata:
  name: forbidden-lvmsnapshot
  namespace: ` + testNamespace + `
spec:
  ownerNodeID: forbidden
  volGroup: iterabase-data
status: {}
`
	blockedPath := state.writeManifest(t, "forbidden-lvmsnapshot.yaml", blocked)
	out, err := state.kubectlOutput(30*time.Second, "create", "-f", blockedPath)
	if err == nil || !strings.Contains(err.Error(), "LVMSnapshot creation is disabled by DES-HOR-545-05") {
		t.Fatalf("LVMSnapshot CREATE was not denied by the exact admission policy: err=%v output=%s", err, out)
	}
	if got := strings.TrimSpace(state.kubectl(t, 30*time.Second, "get", "lvmsnapshots.local.openebs.io", "-A", "-o", "name")); got != "" {
		t.Fatalf("denied LVMSnapshot CREATE left instances: %s", got)
	}
	containers := strings.ToLower(state.kubectl(t, 30*time.Second, "get", "deployment", "-n", testNamespace, "-l", "app=openebs-lvm-controller", "-o", `jsonpath={range .items[*].spec.template.spec.containers[*]}{.name}{" "}{.image}{"\n"}{end}`))
	if strings.Contains(containers, "snapshot") {
		t.Fatalf("volume-only runtime exposes forbidden CSI snapshot container: %s", containers)
	}
	node := strings.TrimSpace(state.process(t, 30*time.Second, "kind", "get", "nodes", "--name", state.cluster.Name))
	state.process(t, 30*time.Second, "docker", "exec", node, "bash", "-ceu", `if grep -q '^dm_snapshot ' /proc/modules; then exit 42; fi`)
}

func installMinimalPlatformEdgeStage(t *testing.T, state *chartState) {
	t.Helper()
	inspect := state.process(t, 30*time.Second, "docker", "network", "inspect", "kind", "-f", `{{range .IPAM.Config}}{{.Subnet}}{{"\n"}}{{end}}`)
	var subnet string
	for _, candidate := range strings.Fields(inspect) {
		if strings.Contains(candidate, ".") {
			subnet = strings.SplitN(candidate, "/", 2)[0]
			break
		}
	}
	parts := strings.Split(subnet, ".")
	if len(parts) != 4 {
		t.Fatalf("Kind network has no IPv4 subnet: %q", inspect)
	}
	pool := fmt.Sprintf("%s.%s.255.200-%s.%s.255.250", parts[0], parts[1], parts[0], parts[1])
	state.internalIngressIP = fmt.Sprintf("%s.%s.255.180", parts[0], parts[1])
	internalPool := fmt.Sprintf("%s-%s.%s.255.190", state.internalIngressIP, parts[0], parts[1])
	// Replace only the declared representative addresses with the observed Kind
	// subnet. Helm replaces lists wholesale, so the pool entry is restated.
	values := runtimeImageValues(t)
	values["metallb-config"] = map[string]any{
		"addresses": []string{pool},
		"additionalPools": []any{map[string]any{
			"name": "internal", "addresses": []string{internalPool}, "autoAssign": false,
		}},
	}
	values["internal-ingress-nginx"] = map[string]any{
		"controller": map[string]any{"service": map[string]any{"annotations": map[string]any{
			"metallb.io/address-pool":    testRelease + "-internal",
			"metallb.io/loadBalancerIPs": state.internalIngressIP,
		}}},
	}
	files := append(freshInstallPlatform.files(state), state.writeValues(t, "fresh-install-runtime", values))
	state.installPlatform(t, 15*time.Minute, files...)
	assertCandidateImages(t, state)
}

func assertOpenEBSLVMClaimsStage(t *testing.T, state *chartState) {
	t.Helper()
	classes := strings.Fields(state.kubectl(t, 30*time.Second, "get", "storageclass", "-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`))
	if len(classes) != 2 || !slices.Contains(classes, PlatformDataStorageClass) || !slices.Contains(classes, AgentPoolWorkspaceStorageClass) {
		t.Fatalf("managed StorageClass set=%v", classes)
	}
	for _, class := range classes {
		if expansion := state.kubectl(t, 30*time.Second, "get", "storageclass/"+class, "-o", "jsonpath={.allowVolumeExpansion}"); expansion != "true" {
			t.Fatalf("managed StorageClass %s allowVolumeExpansion=%q want=true", class, expansion)
		}
	}
	claims := strings.Fields(state.kubectl(t, 30*time.Second, "get", "pvc", "-A", "-o", `jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}|{.spec.storageClassName}|{.status.phase}|{.spec.volumeName}{"\n"}{end}`))
	if len(claims) == 0 {
		t.Fatal("fresh platform rendered no data claims")
	}
	for _, claim := range claims {
		parts := strings.Split(claim, "|")
		if len(parts) != 4 || parts[1] != PlatformDataStorageClass || parts[2] != "Bound" || parts[3] == "" {
			t.Fatalf("chart data claim is not explicitly Bound through the general LVM class: %s", claim)
		}
		pv := state.kubectl(t, 30*time.Second, "get", "pv/"+parts[3], "-o", `jsonpath={.spec.csi.driver}|{.spec.csi.fsType}|{.spec.csi.volumeAttributes.openebs\.io/volgroup}|{.spec.hostPath.path}`)
		if pv != "local.csi.openebs.io|xfs|iterabase-data|" {
			t.Fatalf("chart data PV %s has wrong CSI/XFS/VG/no-hostPath identity: %s", parts[3], pv)
		}
	}

	// DES-HOR-545-01 claims authority: the AgentPool workspace class is authorized
	// ONLY for the manager-created, AgentPool-owned PVC shape. A generic unrelated
	// claim selecting it under a non-manager identity must be denied by the
	// iterabase-agentpool-claim-authority ValidatingAdmissionPolicy (the chart
	// installs it with the manager ServiceAccount as authorizedManagerIdentity).
	blocked := `apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: unrelated-agentpool-claim, namespace: ` + testNamespace + `}
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: ` + AgentPoolWorkspaceStorageClass + `
  resources: {requests: {storage: 512Mi}}
`
	blockedPath := state.writeManifest(t, "unrelated-agentpool-claim.yaml", blocked)
	if _, err := state.kubectlOutput(30*time.Second, "apply", "-f", blockedPath); err == nil {
		t.Fatalf("an unrelated PVC selecting the AgentPool class was admitted; DES-HOR-545-01 requires the manager-owned AgentPool claim shape only")
	}

	// The claim-authority binding is cluster-wide (REQ-035: no unrelated claim may
	// use the AgentPool class in ANY namespace, because the StorageClass and the
	// manager watch are cluster-scoped). Prove the out-of-namespace bypass is
	// closed: a generic PVC in a namespace other than the release namespace must
	// be denied too, not just a same-namespace one.
	crossNS := `apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: unrelated-agentpool-claim-cn, namespace: kube-system}
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: ` + AgentPoolWorkspaceStorageClass + `
  resources: {requests: {storage: 512Mi}}
`
	crossNSPath := state.writeManifest(t, "unrelated-agentpool-claim-cn.yaml", crossNS)
	if _, err := state.kubectlOutput(30*time.Second, "apply", "-f", crossNSPath); err == nil {
		t.Fatalf("a cross-namespace PVC selecting the AgentPool class was admitted; REQ-035 requires cluster-wide claim authority")
	}

	// UPDATE owner-replacement bypass: an existing AgentPool claim whose matching
	// controller ownerReference is replaced with a different AgentPool {name,uid}
	// at the same count must be denied. Keep a real (intentionally dependency-
	// unready) AgentPool owner present so garbage collection cannot remove the
	// admission test claim before its WFFC consumer binds it.
	managerIdentity := "system:serviceaccount:" + testNamespace + ":" + testRelease + "-control-plane-manager"
	agentOwner := `apiVersion: platform.iterabase.com/v1alpha1
kind: AgentPool
metadata: {name: ap-manager, namespace: ` + testNamespace + `}
spec:
  replicas: 0
  workerImage: unavailable.invalid/harness:test
  podSecurity: baseline
  identity: {caSecretRef: {name: intentionally-missing-ca}}
  sandbox: {storageClassName: iterabase-agentpool-lvm-xfs, accessMode: ReadWriteOnce, size: 512Mi}
  gateways:
    controlPlane: {url: https://control-plane.invalid:8443, serverName: control-plane, selector: {podSelector: {matchLabels: {app: control-plane}}}}
    toolGateway: {url: https://tool-gateway.invalid:8443, serverName: tool-gateway, selector: {podSelector: {matchLabels: {app: tool-gateway}}}}
    inferenceGateway: {url: https://inference-gateway.invalid:8443, serverName: inference-gateway, selector: {podSelector: {matchLabels: {app: inference-gateway}}}}
`
	agentOwnerPath := state.writeManifest(t, "manager-agentpool-owner.yaml", agentOwner)
	state.kubectl(t, 30*time.Second, "apply", "-f", agentOwnerPath)
	agentOwnerUID := state.kubectl(t, 30*time.Second, "get", "agentpool/ap-manager", "-n", testNamespace, "-o", "jsonpath={.metadata.uid}")
	agentClaim := `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: manager-agentpool-claim
  namespace: ` + testNamespace + `
  ownerReferences:
    - apiVersion: platform.iterabase.com/v1alpha1
      kind: AgentPool
      name: ap-manager
      uid: ` + agentOwnerUID + `
      controller: true
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: ` + AgentPoolWorkspaceStorageClass + `
  resources: {requests: {storage: 512Mi}}
`
	agentClaimPath := state.writeManifest(t, "manager-agentpool-claim.yaml", agentClaim)
	if _, err := state.kubectlOutput(30*time.Second, "create", "-f", agentClaimPath, "--as", managerIdentity); err != nil {
		t.Fatalf("manager-identity + AgentPool-owned PVC selecting the class was denied on CREATE: %v", err)
	}
	if _, err := state.kubectlOutput(30*time.Second, "patch", "pvc/manager-agentpool-claim", "-n", testNamespace, "--type=merge", "-p", `{"metadata":{"ownerReferences":[{"apiVersion":"platform.iterabase.com/v1alpha1","kind":"AgentPool","name":"ap-other","uid":"22222222-2222-2222-2222-222222222222","controller":true}]}}`); err == nil {
		t.Fatalf("an AgentPool PVC UPDATE swapping its controller ownerReference to a different AgentPool was admitted; ownership must be immutable")
	}
	consumer := `apiVersion: v1
kind: Pod
metadata: {name: manager-agentpool-claim-consumer, namespace: ` + testNamespace + `}
spec:
  containers:
    - name: hold
      image: busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0
      command: [sh, -ceu]
      args: ['printf manager-growth > /data/marker; sync; sleep 600']
      volumeMounts: [{name: data, mountPath: /data}]
  volumes: [{name: data, persistentVolumeClaim: {claimName: manager-agentpool-claim}}]
`
	consumerPath := state.writeManifest(t, "manager-agentpool-claim-consumer.yaml", consumer)
	state.kubectl(t, 30*time.Second, "apply", "-f", consumerPath)
	state.kubectl(t, 5*time.Minute, "wait", "pod/manager-agentpool-claim-consumer", "-n", testNamespace, "--for=condition=Ready", "--timeout=4m")
	if _, err := state.kubectlOutput(30*time.Second, "patch", "pvc/manager-agentpool-claim", "-n", testNamespace, "--type=merge", "-p", `{"spec":{"resources":{"requests":{"storage":"768Mi"}}}}`); err == nil {
		t.Fatalf("a non-manager identity raised an AgentPool PVC request")
	}
	if _, err := state.kubectlOutput(30*time.Second, "patch", "pvc/manager-agentpool-claim", "-n", testNamespace, "--type=merge", "-p", `{"spec":{"resources":{"requests":{"storage":"768Mi"}}}}`, "--as", managerIdentity); err != nil {
		t.Fatalf("the exact manager identity could not raise an AgentPool PVC request: %v", err)
	}
	if _, err := state.kubectlOutput(30*time.Second, "patch", "pvc/manager-agentpool-claim", "-n", testNamespace, "--type=merge", "-p", `{"spec":{"resources":{"requests":{"storage":"512Mi"}}}}`, "--as", managerIdentity); err == nil {
		t.Fatalf("the manager identity was allowed to shrink an AgentPool PVC request")
	}
	if got := state.kubectl(t, 30*time.Second, "get", "pvc/manager-agentpool-claim", "-n", testNamespace, "-o", "jsonpath={.spec.resources.requests.storage}"); got != "768Mi" {
		t.Fatalf("grow-only admission changed the same request to %q, want 768Mi", got)
	}
	state.kubectl(t, 3*time.Minute, "delete", "pod/manager-agentpool-claim-consumer", "pvc/manager-agentpool-claim", "agentpool/ap-manager", "-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=2m")

	// Ubuntu 24.04 XFS refuses filesystems at or below 300 MB; keep this real
	// lifecycle claim above that supported minimum rather than bypassing format.
	manifest := `apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: lvm-lifecycle, namespace: iterabase-system}
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: iterabase-lvm-xfs
  resources: {requests: {storage: 512Mi}}
---
apiVersion: v1
kind: Pod
metadata: {name: lvm-lifecycle, namespace: iterabase-system}
spec:
  restartPolicy: Never
  containers:
    - name: write
      image: busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0
      command: [sh, -ceu]
      args: ['printf lvm-lifecycle=pass > /data/marker; sync; test "$(cat /data/marker)" = lvm-lifecycle=pass']
      volumeMounts: [{name: data, mountPath: /data}]
  volumes: [{name: data, persistentVolumeClaim: {claimName: lvm-lifecycle}}]
`
	path := state.writeManifest(t, "lvm-lifecycle.yaml", manifest)
	state.kubectl(t, 30*time.Second, "apply", "-f", path)
	state.kubectl(t, 5*time.Minute, "wait", "pod/lvm-lifecycle", "-n", testNamespace, "--for=jsonpath={.status.phase}=Succeeded", "--timeout=4m")
	pv := state.kubectl(t, 30*time.Second, "get", "pvc/lvm-lifecycle", "-n", testNamespace, "-o", `jsonpath={.spec.volumeName}`)
	handle := state.kubectl(t, 30*time.Second, "get", "pv/"+pv, "-o", `jsonpath={.spec.csi.volumeHandle}`)
	state.kubectl(t, 30*time.Second, "delete", "pod/lvm-lifecycle", "-n", testNamespace, "--wait=true", "--timeout=2m")
	state.kubectl(t, 30*time.Second, "delete", "pvc/lvm-lifecycle", "-n", testNamespace, "--wait=true", "--timeout=2m")
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		pvMissing := state.kubectl(t, 30*time.Second, "get", "pv/"+pv, "--ignore-not-found=true", "-o", "name") == ""
		volumeMissing := state.kubectl(t, 30*time.Second, "get", "lvmvolume.local.openebs.io/"+handle, "-n", testNamespace, "--ignore-not-found=true", "-o", "name") == ""
		if pvMissing && volumeMissing {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("Delete reclaim left PV %s or LVMVolume %s", pv, handle)
}

func assertManagerContractStage(t *testing.T, state *chartState) {
	t.Helper()
	deployment := testRelease + "-control-plane-manager"
	subject := "system:serviceaccount:" + testNamespace + ":" + state.kubectl(t, 30*time.Second,
		"get", "deployment", deployment, "-n", testNamespace, "-o", "jsonpath={.spec.template.spec.serviceAccountName}")
	state.kubectl(t, 30*time.Second, "get", "crd", "agentpools.platform.iterabase.com", "workflows.platform.iterabase.com")
	for _, resource := range []string{
		"pods", "configmaps", "persistentvolumeclaims", "networkpolicies.networking.k8s.io",
		"agentpools.platform.iterabase.com", "workflows.platform.iterabase.com", "lvmnodes.local.openebs.io", "lvmvolumes.local.openebs.io",
	} {
		if got := state.kubectl(t, 30*time.Second, "auth", "can-i", "list", resource, "--all-namespaces", "--as", subject); got != "yes" {
			t.Fatalf("%s cannot list %s", subject, resource)
		}
	}
	if got := state.kubectl(t, 30*time.Second, "auth", "can-i", "get", "secrets", "-n", testNamespace, "--as", subject); got != "yes" {
		t.Fatalf("%s cannot get namespace Secrets", subject)
	}
	state.kubectl(t, 3*time.Minute, "rollout", "status", "deployment/"+deployment, "-n", testNamespace, "--timeout=2m")
	// A controller whose informer lacks RBAC never syncs and the manager exits at
	// controller-runtime's two-minute cache-sync boundary. Poll the real
	// condition instead of sleeping past that boundary: every controller that
	// started an event source has synced its cache and started workers.
	var logs string
	err := poll.Until(state.ctx, 3*time.Minute, 3*time.Second, func(context.Context) (bool, string, error) {
		out, observeErr := state.kubectlOutput(30*time.Second, "logs", "deployment/"+deployment, "-n", testNamespace, "--all-containers", "--tail=2000")
		if observeErr != nil {
			return false, "read manager logs", observeErr
		}
		logs = out
		synced, detail, syncErr := managerCachesSynced(logs, "agentpool", "workflow")
		return synced, detail, syncErr
	})
	if err != nil {
		t.Fatalf("control-plane manager caches did not sync: %v\n%s", err, stateSafeBody([]byte(logs)))
	}
	if got := state.kubectl(t, 30*time.Second, "get", "deployment", deployment, "-n", testNamespace, "-o", "jsonpath={.status.readyReplicas}"); got != "1" {
		t.Fatalf("manager ready replicas=%q want=1", got)
	}
	if got := state.kubectl(t, 30*time.Second, "get", "pods", "-n", testNamespace, "-l", "app.kubernetes.io/component=manager", "-o", "jsonpath={.items[0].status.containerStatuses[0].restartCount}"); got != "0" {
		t.Fatalf("manager restart count=%q want=0", got)
	}
}

// managerCachesSynced reports whether the manager's controller-runtime log
// shows every controller that started an event source (before its cache sync)
// also starting workers (after its cache sync), including each required
// controller. A cache-sync or manager failure is terminal.
func managerCachesSynced(logs string, required ...string) (bool, string, error) {
	started, synced := map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(logs, "\n") {
		message, controller := managerLogEntry(line)
		switch {
		case message == "Could not wait for Cache to sync" || message == "Failed to run manager":
			return false, "", fmt.Errorf("manager logged %q for controller %q", message, controller)
		case controller == "":
		case message == "Starting EventSource":
			started[controller] = true
		case message == "Starting workers":
			synced[controller] = true
		}
	}
	for _, controller := range required {
		if !synced[controller] {
			return false, fmt.Sprintf("controller %s has not synced", controller), nil
		}
	}
	for controller := range started {
		if !synced[controller] {
			return false, fmt.Sprintf("controller %s has not synced", controller), nil
		}
	}
	return true, fmt.Sprintf("%d controllers synced", len(synced)), nil
}

// managerLogEntry extracts the message and controller of one structured JSON
// (default) or logfmt manager log line.
func managerLogEntry(line string) (string, string) {
	var entry struct {
		Message    string `json:"msg"`
		Controller string `json:"controller"`
	}
	if json.Unmarshal([]byte(line), &entry) == nil {
		return entry.Message, entry.Controller
	}
	message, controller := "", ""
	if match := logfmtMessage.FindStringSubmatch(line); match != nil {
		message = match[1]
	}
	if match := logfmtController.FindStringSubmatch(line); match != nil {
		controller = match[1]
	}
	return message, controller
}

var (
	logfmtMessage    = regexp.MustCompile(`\bmsg="([^"]*)"`)
	logfmtController = regexp.MustCompile(`\bcontroller="?([^\s"]+)`)
)

func TestUnitManagerCacheSyncRequiresEveryStartedController(t *testing.T) {
	const (
		agentpoolStarted = `{"msg":"Starting EventSource","controller":"agentpool"}`
		workflowStarted  = `{"msg":"Starting EventSource","controller":"workflow"}`
		agentpoolSynced  = `{"msg":"Starting workers","controller":"agentpool","worker count":1}`
		workflowSynced   = `{"msg":"Starting workers","controller":"workflow","worker count":1}`
	)
	for name, test := range map[string]struct {
		logs   string
		synced bool
		failed bool
	}{
		"all synced":         {logs: strings.Join([]string{agentpoolStarted, workflowStarted, agentpoolSynced, workflowSynced}, "\n"), synced: true},
		"one pending":        {logs: strings.Join([]string{agentpoolStarted, workflowStarted, agentpoolSynced}, "\n")},
		"required missing":   {logs: strings.Join([]string{agentpoolStarted, agentpoolSynced}, "\n")},
		"extra pending":      {logs: strings.Join([]string{agentpoolSynced, workflowSynced, `{"msg":"Starting EventSource","controller":"model"}`}, "\n")},
		"cache sync failure": {logs: strings.Join([]string{agentpoolStarted, `{"msg":"Could not wait for Cache to sync","controller":"workflow"}`}, "\n"), failed: true},
		"manager failure":    {logs: `{"msg":"Failed to run manager"}`, failed: true},
		"logfmt":             {logs: `level=INFO msg="Starting workers" controller=agentpool` + "\n" + `level=INFO msg="Starting workers" controller=workflow`, synced: true},
	} {
		t.Run(name, func(t *testing.T) {
			synced, _, err := managerCachesSynced(test.logs, "agentpool", "workflow")
			if (err != nil) != test.failed || synced != test.synced {
				t.Fatalf("synced=%t err=%v want synced=%t failed=%t", synced, err, test.synced, test.failed)
			}
		})
	}
}

func assertCertificateIssuerStage(t *testing.T, state *chartState) {
	t.Helper()
	state.kubectl(t, 3*time.Minute, "wait", "--for=condition=Ready", "clusterissuer/selfsigned", "--timeout=2m")
	state.kubectl(t, 3*time.Minute, "rollout", "status", "daemonset/cert-manager-csi-driver", "-n", testNamespace, "--timeout=2m")
	state.kubectl(t, 30*time.Second, "get", "csidriver", "csi.cert-manager.io")
}

func assertWorkloadIdentityStage(t *testing.T, state *chartState) {
	t.Helper()
	manifest := `apiVersion: v1
kind: Pod
metadata:
  name: workload-identity-csi-smoke
  namespace: iterabase-system
spec:
  restartPolicy: Never
  containers:
    - name: verify
      image: busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0
      command: ["/bin/sh", "-c"]
      args: ["test -s /tls/tls.crt && test -s /tls/tls.key && test -s /tls/ca.crt"]
      volumeMounts:
        - name: workload-identity
          mountPath: /tls
          readOnly: true
  volumes:
    - name: workload-identity
      csi:
        driver: csi.cert-manager.io
        readOnly: true
        volumeAttributes:
          csi.cert-manager.io/issuer-name: platform-spiffe-ca
          csi.cert-manager.io/issuer-kind: ClusterIssuer
          csi.cert-manager.io/uri-sans: spiffe://iterabase.local/pools/ci/workers/csi-smoke
          csi.cert-manager.io/duration: 1h
`
	path := state.writeManifest(t, "workload-identity.yaml", manifest)
	state.kubectl(t, 30*time.Second, "apply", "-f", path)
	state.kubectl(t, 4*time.Minute, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/workload-identity-csi-smoke", "-n", testNamespace, "--timeout=3m")
}

func assertVerifiedEdgeStage(t *testing.T, state *chartState) {
	t.Helper()
	var address string
	err := poll.Until(state.ctx, 3*time.Minute, 2*time.Second, func(context.Context) (bool, string, error) {
		out, observeErr := state.kubectlOutput(30*time.Second,
			"get", "service", "-n", testNamespace, "-l", "app.kubernetes.io/name=ingress-nginx",
			"-o", "jsonpath={.items[0].status.loadBalancer.ingress[0].ip}")
		if observeErr != nil {
			return false, "read LoadBalancer address", observeErr
		}
		address = strings.TrimSpace(out)
		return address != "", address, nil
	})
	if err != nil {
		t.Fatalf("wait for ingress LoadBalancer address: %v", err)
	}
	state.kubectl(t, 3*time.Minute, "wait", "--for=condition=Ready", "certificate/"+testRelease+"-gateway-tls", "-n", testNamespace, "--timeout=2m")
	certificate := decodeSecretValue(t, state, testRelease+"-gateway-tls", "tls.crt")
	// A MetalLB address is host-routable on Linux CI but not across every local
	// Docker backend. Prove allocation above, then exercise the same ingress
	// Service through a loopback port-forward for a portable verified-TLS check.
	forward := state.forward(t, "svc/"+testRelease+"-ingress-nginx-controller", 443, "https")
	client := verifiedDialClient(t, certificate, "gateway.iterabase.local", fmt.Sprintf("127.0.0.1:%d", forward.LocalPort))
	if err := waitHTTPReady(state.ctx, client, "https://gateway.iterabase.local/health", 2*time.Minute); err != nil {
		t.Fatalf("verified gateway edge did not become ready: %v", err)
	}
	requireHTTP(t, client, http.MethodGet, "https://gateway.iterabase.local/health", nil, http.StatusOK)
	state.stopForward(t, forward)
}

func assertPrivateIngressPlaneStage(t *testing.T, state *chartState) {
	t.Helper()
	if got := state.kubectl(t, 30*time.Second, "get", "ingressclass/nginx", "-o", "jsonpath={.spec.controller}"); got != "k8s.io/ingress-nginx" {
		t.Fatalf("public IngressClass controller=%q", got)
	}
	if got := state.kubectl(t, 30*time.Second, "get", "ingressclass/nginx-internal", "-o", "jsonpath={.spec.controller}"); got != "k8s.io/ingress-nginx-internal" {
		t.Fatalf("private IngressClass controller=%q", got)
	}
	if got := state.kubectl(t, 30*time.Second, "get", "ipaddresspool/"+testRelease+"-internal", "-n", testNamespace, "-o", "jsonpath={.spec.autoAssign}"); got != "false" {
		t.Fatalf("private MetalLB pool autoAssign=%q", got)
	}

	service := testRelease + "-internal-ingress-nginx-controller"
	var address string
	err := poll.Until(state.ctx, 3*time.Minute, 2*time.Second, func(context.Context) (bool, string, error) {
		out, observeErr := state.kubectlOutput(30*time.Second, "get", "service/"+service, "-n", testNamespace,
			"-o", "jsonpath={.status.loadBalancer.ingress[0].ip}")
		if observeErr != nil {
			return false, "read private LoadBalancer address", observeErr
		}
		address = strings.TrimSpace(out)
		return address == state.internalIngressIP, address, nil
	})
	if err != nil {
		t.Fatalf("private ingress did not receive fixed MetalLB address %s: %v", state.internalIngressIP, err)
	}

	const host = "private-gateway.iterabase.local"
	manifest := fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: private-ingress-smoke
  namespace: %s
  annotations:
    cert-manager.io/cluster-issuer: selfsigned
spec:
  ingressClassName: nginx-internal
  tls:
    - hosts: [%s]
      secretName: private-ingress-smoke-tls
  rules:
    - host: %s
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: %s-gateway
                port:
                  number: 8080
`, testNamespace, host, host, testRelease)
	state.kubectl(t, 30*time.Second, "apply", "-f", state.writeManifest(t, "private-ingress-smoke.yaml", manifest))
	state.kubectl(t, 3*time.Minute, "wait", "--for=condition=Ready", "certificate/private-ingress-smoke-tls", "-n", testNamespace, "--timeout=2m")
	certificate := decodeSecretValue(t, state, "private-ingress-smoke-tls", "tls.crt")

	privateForward := state.forward(t, "svc/"+service, 443, "https")
	privateClient := verifiedDialClient(t, certificate, host, fmt.Sprintf("127.0.0.1:%d", privateForward.LocalPort))
	if err := waitHTTPReady(state.ctx, privateClient, "https://"+host+"/health", 2*time.Minute); err != nil {
		t.Fatalf("private ingress route did not become ready: %v", err)
	}
	requireHTTP(t, privateClient, http.MethodGet, "https://"+host+"/health", nil, http.StatusOK)
	state.stopForward(t, privateForward)

	// The public controller must not load an Ingress owned by the private class.
	// Trust only the private leaf: a response would prove the public controller
	// served that route and certificate instead of its unrelated default leaf.
	publicForward := state.forward(t, "svc/"+testRelease+"-ingress-nginx-controller", 443, "https")
	publicClient := verifiedDialClient(t, certificate, host, fmt.Sprintf("127.0.0.1:%d", publicForward.LocalPort))
	if response, requestErr := publicClient.Get("https://" + host + "/health"); requestErr == nil {
		_ = response.Body.Close()
		t.Fatalf("public ingress served private-class host with status %d", response.StatusCode)
	}
	state.stopForward(t, publicForward)
}
