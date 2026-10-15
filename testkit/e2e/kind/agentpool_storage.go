package kind

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/process"
)

// StorageClassExpectation names one managed OpenEBS LVM StorageClass the
// consuming owner expects, and whether the class is `shared: yes` (the AgentPool
// class is shared because its PV may be claimed by multiple workspaces; the
// general class is not).
type StorageClassExpectation struct {
	Name   string
	Shared bool
}

// LVMStorageContract carries the exact storage identities the consuming owner
// suite expects the pinned lvm-storage-substrate chart to render and the Kind
// node to discover. testkit/e2e provides only generic reusable Kind/LVM
// orchestration and parameterized validation mechanics; the owner constructs
// this contract so the exact product/chart expectations remain with the owning
// suite (testkit/AGENTS.md). It is deliberately a value type with no behavior.
type LVMStorageContract struct {
	DataVolumeGroupName string
	Provisioner         string
	NodeTopologyKey     string
	StorageClasses      []StorageClassExpectation
}

const (
	kindKubeletDirectory       = "/var/lib/kubelet"
	defaultClassAnnotation     = "storageclass.kubernetes.io/is-default-class"
	betaDefaultClassAnnotation = "storageclass.beta.kubernetes.io/is-default-class"
	// The largest owner scenario enables 70 GiB of thick chart claims; keep
	// bounded headroom for representative general and AgentPool claims.
	kindDataVolumeGroupSizeBytes = 96 << 30
)

// ConfigureLVMStorage removes Kind's local-path fallback, creates a real thick
// loop-backed LVM VG inside the one privileged node, installs the exact pinned
// lvm-storage-substrate chart, and waits until the StorageClasses, CSI
// registration, and LVMNode discovery match contract.
func (cluster *Cluster) ConfigureLVMStorage(ctx context.Context, chart, namespace, release string, contract LVMStorageContract) error {
	if cluster == nil || cluster.executor == nil || cluster.Kubeconfig == "" {
		return fmt.Errorf("kind LVM storage requires an initialized cluster")
	}
	if chart == "" || namespace == "" || release == "" {
		return fmt.Errorf("kind LVM storage requires chart, namespace, and release identities")
	}
	if contract.DataVolumeGroupName == "" || contract.Provisioner == "" || contract.NodeTopologyKey == "" || len(contract.StorageClasses) == 0 {
		return fmt.Errorf("kind LVM storage requires a complete owner-supplied LVMStorageContract")
	}
	if info, err := os.Stat(chart); err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
		return fmt.Errorf("kind LVM storage chart %q is unavailable", chart)
	}
	nodes, err := cluster.executor.Run(ctx, process.Command{
		Name: "kind", Args: []string{"get", "nodes", "--name", cluster.Name}, Timeout: 30 * time.Second,
		OutputName: "kind-lvm-nodes-" + cluster.Name + ".log",
	})
	if err != nil {
		return fmt.Errorf("list Kind nodes for LVM storage: %w", err)
	}
	nodeNames := strings.Fields(nodes.Output)
	if len(nodeNames) != 1 {
		return fmt.Errorf("kind LVM storage requires exactly one node, observed %d", len(nodeNames))
	}

	if _, err := cluster.runKubectl(ctx, 2*time.Minute, "kind-remove-local-path-"+cluster.Name+".log",
		"delete", "namespace", "local-path-storage", "--ignore-not-found=true", "--wait=true", "--timeout=90s"); err != nil {
		return fmt.Errorf("remove Kind local-path provisioner: %w", err)
	}
	if _, err := cluster.runKubectl(ctx, 30*time.Second, "kind-remove-default-class-"+cluster.Name+".log",
		"delete", "storageclass", "standard", "--ignore-not-found=true"); err != nil {
		return fmt.Errorf("remove Kind default StorageClass: %w", err)
	}

	prepare := `set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
if ! command -v pvcreate >/dev/null || ! command -v mkfs.xfs >/dev/null || ! command -v losetup >/dev/null; then
  apt-get update
  apt-get install -y lvm2 xfsprogs util-linux
fi
if vgs "$1" >/dev/null 2>&1; then
  test "$(vgs --noheadings -o pv_count "$1" | awk '{$1=$1;print}')" = 1
else
  test ! -e /var/lib/iterabase-lvm-loop || exit 42
  truncate -s ` + fmt.Sprintf("%d", kindDataVolumeGroupSizeBytes) + ` /var/lib/iterabase-data.img
  loop=$(losetup --find --show /var/lib/iterabase-data.img)
  printf '%s\n' "$loop" > /var/lib/iterabase-lvm-loop
  pvcreate --yes "$loop"
  vgcreate --yes "$1" "$loop"
fi
vgs --noheadings -o vg_name,vg_uuid,vg_size,vg_free,pv_count,lv_count "$1"
`
	if result, err := cluster.executor.Run(ctx, process.Command{
		Name: "docker", Args: []string{"exec", nodeNames[0], "bash", "-ceu", prepare, "kind-lvm-prepare", contract.DataVolumeGroupName}, Timeout: 5 * time.Minute,
		OutputName: "kind-prepare-lvm-" + cluster.Name + ".log",
	}); err != nil {
		return fmt.Errorf("prepare real Kind %s VG: %w\n%s", contract.DataVolumeGroupName, err, result.Output)
	}

	cluster.mu.Lock()
	cluster.lvmPrepared = true
	cluster.lvmNode = nodeNames[0]
	cluster.lvmNamespace = namespace
	cluster.lvmRelease = release
	cluster.lvmContract = contract
	cluster.mu.Unlock()

	if result, err := cluster.executor.Run(ctx, process.Command{
		Name: "helm", Args: lvmStorageHelmArgs(release, chart, cluster.Kubeconfig, namespace),
		Timeout: 10 * time.Minute, OutputName: "kind-install-lvm-storage-" + cluster.Name + ".log",
	}); err != nil {
		return fmt.Errorf("install pinned LVM storage substrate: %w\n%s", err, result.Output)
	}

	deadline := time.Now().Add(3 * time.Minute)
	for {
		if err := cluster.validateLVMStorage(ctx, namespace, nodeNames[0], contract); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (cluster *Cluster) cleanupLVMStorage(ctx context.Context) error {
	script := `set -euo pipefail
kubeconfig=$1
namespace=$2
lvm_release=$3
node=$4
vg=$5
if kubectl --kubeconfig "$kubeconfig" get crd agentpools.platform.iterabase.com >/dev/null 2>&1; then
  kubectl --kubeconfig "$kubeconfig" delete agentpools.platform.iterabase.com --all -A --ignore-not-found=true --wait=true --timeout=3m
fi
namespaced_resources=$(kubectl --kubeconfig "$kubeconfig" api-resources --api-group=platform.iterabase.com --namespaced=true --verbs=list,delete -o name)
while IFS= read -r resource; do
  test -n "$resource" || continue
  test "$resource" = agentpools.platform.iterabase.com && continue
  kubectl --kubeconfig "$kubeconfig" delete "$resource" --all -A --ignore-not-found=true --wait=true --timeout=3m
done <<<"$namespaced_resources"
cluster_resources=$(kubectl --kubeconfig "$kubeconfig" api-resources --api-group=platform.iterabase.com --namespaced=false --verbs=list,delete -o name)
while IFS= read -r resource; do
  test -n "$resource" || continue
  kubectl --kubeconfig "$kubeconfig" delete "$resource" --all --ignore-not-found=true --wait=true --timeout=3m
done <<<"$cluster_resources"
while IFS= read -r release; do
  test -n "$release" || continue
  test "$release" = "$lvm_release" && continue
  helm uninstall "$release" --kubeconfig "$kubeconfig" -n "$namespace" --wait --timeout 3m
done < <(helm list --kubeconfig "$kubeconfig" -n "$namespace" -q)
kubectl --kubeconfig "$kubeconfig" delete pod -n "$namespace" -l 'app notin (openebs-lvm-node,openebs-lvm-controller)' --ignore-not-found=true --wait=true --timeout=3m
kubectl --kubeconfig "$kubeconfig" delete job --all -n "$namespace" --ignore-not-found=true --wait=true --timeout=3m
kubectl --kubeconfig "$kubeconfig" delete pvc --all -A --ignore-not-found=true --wait=true --timeout=3m
for i in $(seq 1 90); do
  count=0
  if kubectl --kubeconfig "$kubeconfig" get crd lvmvolumes.local.openebs.io >/dev/null 2>&1; then
    count=$((count + $(kubectl --kubeconfig "$kubeconfig" get lvmvolumes.local.openebs.io -A --no-headers | awk 'NF {n++} END {print n+0}')))
  fi
  test "$count" = 0 && break
  test "$i" -lt 90 || exit 42
  sleep 2
done
if helm status "$lvm_release" --kubeconfig "$kubeconfig" -n "$namespace" >/dev/null 2>&1; then
  helm uninstall "$lvm_release" --kubeconfig "$kubeconfig" -n "$namespace" --wait --timeout 3m
fi
docker exec "$node" bash -ceu '
vg=$1
test "$(lvs --noheadings --select vg_name="$vg" -o lv_name | awk "NF {n++} END {print n+0}")" = 0
loop=$(cat /var/lib/iterabase-lvm-loop)
vgremove --yes "$vg"
pvremove --yes "$loop"
losetup -d "$loop"
rm -f /var/lib/iterabase-lvm-loop /var/lib/iterabase-data.img
' kind-lvm-cleanup-inner "$vg"
`
	vgName := ""
	if cluster.lvmContract.DataVolumeGroupName != "" {
		vgName = cluster.lvmContract.DataVolumeGroupName
	}
	result, err := cluster.executor.Run(ctx, process.Command{
		Name: "bash", Args: []string{"-ceu", script, "lvm-cleanup", cluster.Kubeconfig, cluster.lvmNamespace, cluster.lvmRelease, cluster.lvmNode, vgName},
		Timeout: 10 * time.Minute, OutputName: "kind-cleanup-lvm-storage-" + cluster.Name + ".log",
	})
	if err != nil {
		return fmt.Errorf("cleanup Kind LVM storage before cluster deletion: %w\n%s", err, result.Output)
	}
	return nil
}

func lvmStorageHelmArgs(release, chart, kubeconfig, namespace string) []string {
	// The substrate chart's admission policy (DES-HOR-545 gate) requires the
	// exact control-plane manager identity; the substrate release is
	// <platform>-lvm-storage and the manager SA is <platform>-control-plane-manager.
	managerIdentity := lvmStorageManagerIdentityForTest(release, namespace)
	return []string{"upgrade", "--install", release, chart, "--kubeconfig", kubeconfig,
		"--namespace", namespace, "--create-namespace", "--set-string", "lvm-localpv.global.kubeletDir=" + kindKubeletDirectory,
		"--set-string", "agentpool.authorizedManagerIdentity=" + managerIdentity,
		// DES-HOR-461-05: the signature class admits only this platform
		// release's artifact signature PVC.
		"--set-string", "signatures.authorizedClaim.namespace=" + namespace,
		"--set-string", "signatures.authorizedClaim.name=" + strings.TrimSuffix(release, "-lvm-storage") + "-control-plane-artifact-signatures",
		"--wait", "--timeout", "8m"}
}

func lvmStorageManagerIdentityForTest(release, namespace string) string {
	platformRelease := strings.TrimSuffix(release, "-lvm-storage")
	return "system:serviceaccount:" + namespace + ":" + platformRelease + "-control-plane-manager"
}

func (cluster *Cluster) validateLVMStorage(ctx context.Context, namespace, nodeName string, contract LVMStorageContract) error {
	classes, err := cluster.runKubectl(ctx, 30*time.Second, "kind-lvm-storageclasses-"+cluster.Name+".json", "get", "storageclass", "-o", "json")
	if err != nil {
		return fmt.Errorf("read managed LVM StorageClasses: %w", err)
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(classes.Output), &list); err != nil {
		return fmt.Errorf("decode managed LVM StorageClasses: %w", err)
	}
	if len(list.Items) != len(contract.StorageClasses) {
		return fmt.Errorf("managed cluster must have exactly %d StorageClasses, observed %d", len(contract.StorageClasses), len(list.Items))
	}
	seen := map[string]bool{}
	for _, item := range list.Items {
		name, err := ValidateManagedLVMStorageClass(item, contract)
		if err != nil {
			return err
		}
		seen[name] = true
	}
	for _, expected := range contract.StorageClasses {
		if !seen[expected.Name] {
			return fmt.Errorf("managed LVM StorageClass %q is missing", expected.Name)
		}
	}
	if _, err := cluster.runKubectl(ctx, 30*time.Second, "kind-lvm-csidriver-"+cluster.Name+".json", "get", "csidriver", contract.Provisioner, "-o", "json"); err != nil {
		return fmt.Errorf("read OpenEBS LVM CSIDriver: %w", err)
	}
	if err := cluster.validateLVMSnapshotDependencyBoundary(ctx, namespace); err != nil {
		return err
	}
	csiNode, err := cluster.runKubectl(ctx, 30*time.Second, "kind-lvm-csinode-"+cluster.Name+".json", "get", "csinode", nodeName, "-o", "json")
	if err != nil {
		return fmt.Errorf("read OpenEBS LVM CSINode registration: %w", err)
	}
	if err := ValidateLVMCSINodeRegistration([]byte(csiNode.Output), nodeName, contract); err != nil {
		return err
	}
	nodes, err := cluster.runKubectl(ctx, 30*time.Second, "kind-lvmnode-"+cluster.Name+".json", "get", "lvmnodes.local.openebs.io", "-n", namespace, "-o", "json")
	if err != nil {
		return fmt.Errorf("read OpenEBS LVMNode: %w", err)
	}
	var lvmNodes struct {
		Items []struct {
			VolumeGroups []struct {
				Name           string `json:"name"`
				UUID           string `json:"uuid"`
				PVCount        int    `json:"pvCount"`
				MissingPVCount int    `json:"missingPvCount"`
				ThinPools      []any  `json:"thinPools"`
			} `json:"volumeGroups"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(nodes.Output), &lvmNodes); err != nil {
		return fmt.Errorf("decode OpenEBS LVMNode: %w", err)
	}
	if len(lvmNodes.Items) != 1 {
		return fmt.Errorf("expected one OpenEBS LVMNode, observed %d", len(lvmNodes.Items))
	}
	for _, group := range lvmNodes.Items[0].VolumeGroups {
		if group.Name == contract.DataVolumeGroupName && group.UUID != "" && group.PVCount == 1 && group.MissingPVCount == 0 && len(group.ThinPools) == 0 {
			return nil
		}
	}
	return fmt.Errorf("OpenEBS LVMNode has not discovered the exact thick %s VG", contract.DataVolumeGroupName)
}

func (cluster *Cluster) validateLVMSnapshotDependencyBoundary(ctx context.Context, namespace string) error {
	inertCRD, err := cluster.runKubectl(ctx, 30*time.Second, "kind-lvm-inert-lvmsnapshot-crd.log", "get", "crd", "lvmsnapshots.local.openebs.io", "-o", "name")
	if err != nil {
		return fmt.Errorf("observe inert LVMSnapshot CRD: %w", err)
	}
	if strings.TrimSpace(inertCRD.Output) != "customresourcedefinition.apiextensions.k8s.io/lvmsnapshots.local.openebs.io" {
		return fmt.Errorf("inert LVMSnapshot CRD is missing or ambiguous")
	}
	instances, err := cluster.runKubectl(ctx, 30*time.Second, "kind-lvm-lvmsnapshot-instances.log", "get", "lvmsnapshots.local.openebs.io", "-A", "-o", "name")
	if err != nil {
		return fmt.Errorf("observe inert LVMSnapshot instances: %w", err)
	}
	if strings.TrimSpace(instances.Output) != "" {
		return fmt.Errorf("inert LVMSnapshot API contains forbidden instances")
	}
	for _, name := range []string{
		"volumesnapshotclasses.snapshot.storage.k8s.io",
		"volumesnapshotcontents.snapshot.storage.k8s.io",
		"volumesnapshots.snapshot.storage.k8s.io",
	} {
		result, err := cluster.runKubectl(ctx, 30*time.Second, "kind-lvm-forbidden-crd-"+safeFileName(name)+".log", "get", "crd", name, "--ignore-not-found=true", "-o", "name")
		if err != nil {
			return fmt.Errorf("observe forbidden CSI snapshot CRD %s: %w", name, err)
		}
		if strings.TrimSpace(result.Output) != "" {
			return fmt.Errorf("volume-only cluster exposes forbidden CSI snapshot CRD %s", name)
		}
	}
	for _, resource := range []string{"clusterrole/openebs-lvm-snapshotter-role", "clusterrolebinding/openebs-lvm-snapshotter-binding"} {
		result, err := cluster.runKubectl(ctx, 30*time.Second, "kind-lvm-forbidden-rbac-"+safeFileName(resource)+".log", "get", resource, "--ignore-not-found=true", "-o", "name")
		if err != nil {
			return fmt.Errorf("observe forbidden broad snapshot RBAC %s: %w", resource, err)
		}
		if strings.TrimSpace(result.Output) != "" {
			return fmt.Errorf("volume-only cluster exposes forbidden broad snapshot RBAC %s", resource)
		}
	}
	deployment, err := cluster.runKubectl(ctx, 30*time.Second, "kind-lvm-volume-controller-"+cluster.Name+".json", "get", "deployment", "-n", namespace, "-l", "app=openebs-lvm-controller", "-o", "json")
	if err != nil {
		return fmt.Errorf("read OpenEBS volume controller deployment: %w", err)
	}
	var workloads struct {
		Items []struct {
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name  string `json:"name"`
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				AvailableReplicas int32 `json:"availableReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(deployment.Output), &workloads); err != nil {
		return fmt.Errorf("decode OpenEBS volume controller deployment: %w", err)
	}
	if len(workloads.Items) != 1 || workloads.Items[0].Status.AvailableReplicas != 1 {
		return fmt.Errorf("expected one Ready OpenEBS volume controller deployment")
	}
	for _, container := range workloads.Items[0].Spec.Template.Spec.Containers {
		identity := strings.ToLower(container.Name + " " + container.Image)
		if strings.Contains(identity, "snapshot") {
			return fmt.Errorf("volume-only OpenEBS controller exposes forbidden CSI snapshot container %q", container.Name)
		}
	}
	return nil
}

// ValidateLVMCSINodeRegistration rejects a missing, duplicate, wrong-node, or
// incomplete OpenEBS node/topology registration before a WaitForFirstConsumer
// claim can enter an unschedulable capacity loop.
func ValidateLVMCSINodeRegistration(data []byte, nodeName string, contract LVMStorageContract) error {
	var csiNode struct {
		Spec struct {
			Drivers []struct {
				Name         string   `json:"name"`
				NodeID       string   `json:"nodeID"`
				TopologyKeys []string `json:"topologyKeys"`
			} `json:"drivers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &csiNode); err != nil {
		return fmt.Errorf("decode OpenEBS LVM CSINode registration: %w", err)
	}
	matches := 0
	for _, driver := range csiNode.Spec.Drivers {
		if driver.Name != contract.Provisioner {
			continue
		}
		matches++
		keys := append([]string(nil), driver.TopologyKeys...)
		sort.Strings(keys)
		expectedKeys := []string{"kubernetes.io/hostname", contract.NodeTopologyKey}
		if driver.NodeID != nodeName || !reflect.DeepEqual(keys, expectedKeys) {
			return fmt.Errorf("OpenEBS LVM CSINode registration for %q does not match node/topology contract", nodeName)
		}
	}
	if matches != 1 {
		return fmt.Errorf("expected exactly one OpenEBS LVM CSINode registration for %q, observed %d", nodeName, matches)
	}
	return nil
}

// ValidateManagedLVMStorageClass returns the exact recognized class name and
// rejects any default/provisioner/VG/XFS/thick/shared/policy drift.
func ValidateManagedLVMStorageClass(data []byte, contract LVMStorageContract) (string, error) {
	var storageClass struct {
		Metadata struct {
			Name        string            `json:"name"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Provisioner          string            `json:"provisioner"`
		ReclaimPolicy        string            `json:"reclaimPolicy"`
		VolumeBindingMode    string            `json:"volumeBindingMode"`
		AllowVolumeExpansion *bool             `json:"allowVolumeExpansion"`
		Parameters           map[string]string `json:"parameters"`
	}
	if err := json.Unmarshal(data, &storageClass); err != nil {
		return "", fmt.Errorf("decode managed LVM StorageClass contract: %w", err)
	}
	shared := ""
	for _, expected := range contract.StorageClasses {
		if storageClass.Metadata.Name == expected.Name {
			if expected.Shared {
				shared = "yes"
			} else {
				shared = "no"
			}
			break
		}
	}
	if shared == "" {
		return "", fmt.Errorf("unsupported managed StorageClass %q", storageClass.Metadata.Name)
	}
	expected := map[string]string{
		"storage": "lvm", "vgpattern": "^" + contract.DataVolumeGroupName + "$",
		"fsType": "xfs", "thinProvision": "no", "shared": shared,
	}
	if storageClass.Provisioner != contract.Provisioner || storageClass.ReclaimPolicy != "Delete" ||
		storageClass.VolumeBindingMode != "WaitForFirstConsumer" || storageClass.AllowVolumeExpansion == nil || !*storageClass.AllowVolumeExpansion ||
		!reflect.DeepEqual(storageClass.Parameters, expected) || storageClass.Metadata.Annotations[defaultClassAnnotation] == "true" ||
		storageClass.Metadata.Annotations[betaDefaultClassAnnotation] == "true" {
		return "", fmt.Errorf("StorageClass %q does not match the exact non-default thick XFS LVM contract", storageClass.Metadata.Name)
	}
	return storageClass.Metadata.Name, nil
}

func (cluster *Cluster) runKubectl(ctx context.Context, timeout time.Duration, outputName string, args ...string) (process.Result, error) {
	result, err := cluster.executor.Run(ctx, process.Command{
		Name: "kubectl", Args: append([]string{"--kubeconfig", cluster.Kubeconfig}, args...),
		Timeout: timeout, OutputName: outputName,
	})
	if err != nil && result.Output != "" {
		return result, fmt.Errorf("%w\n%s", err, result.Output)
	}
	return result, err
}
