package e2e

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	hostFixtureEnabledEnv           = "FORGE_E2E_FIXTURE"
	hostFixtureAddressEnv           = "FORGE_E2E_FIXTURE_ADDRESS"
	hostFixtureSSHUserEnv           = "FORGE_E2E_FIXTURE_SSH_USER"
	hostFixtureSSHKeyPathEnv        = "FORGE_E2E_FIXTURE_SSH_KEY_PATH"
	hostFixtureHostKeyEnv           = "FORGE_E2E_FIXTURE_SSH_HOST_KEY"
	hostFixtureDataStorageDeviceEnv = "FORGE_E2E_FIXTURE_DATA_STORAGE_DEVICES"
	hostFixtureModelDeviceEnv       = "FORGE_E2E_MODEL_CACHE_DEVICE"
	hostFixtureModelUUIDEnv         = "FORGE_E2E_MODEL_CACHE_UUID"
	hostFixtureModelMount           = "/data/hf-cache"
)

var bootIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

//go:embed model-cache.json
var modelCacheAuthorityJSON []byte

type modelCacheAuthority struct {
	SchemaVersion int    `json:"schema_version"`
	ModelID       string `json:"model_id"`
	Revision      string `json:"revision"`
	WeightPath    string `json:"weight_path"`
	SHA256        string `json:"sha256"`
}

type hostFixture struct {
	capacity          string
	address           string
	sshUser           string
	sshKeyPath        string
	dataStorageDevice string
	modelDevice       string
	modelUUID         string

	tunnelMu      sync.Mutex
	apiTunnel     *sshAPITunnel
	apiServerName string
}

func fixtureSSHUser() string {
	if user := strings.TrimSpace(os.Getenv(hostFixtureSSHUserEnv)); user != "" {
		return user
	}
	return "forge"
}

func requireHostFixture(t *testing.T, capacity string) *hostFixture {
	t.Helper()
	if os.Getenv(hostFixtureEnabledEnv) != "true" {
		t.Fatalf("mandatory %s fixture is disabled — %s must be true", capacity, hostFixtureEnabledEnv)
	}
	values := map[string]string{
		hostFixtureAddressEnv:           strings.TrimSpace(os.Getenv(hostFixtureAddressEnv)),
		hostFixtureSSHUserEnv:           strings.TrimSpace(os.Getenv(hostFixtureSSHUserEnv)),
		hostFixtureSSHKeyPathEnv:        strings.TrimSpace(os.Getenv(hostFixtureSSHKeyPathEnv)),
		hostFixtureHostKeyEnv:           strings.TrimSpace(os.Getenv(hostFixtureHostKeyEnv)),
		hostFixtureDataStorageDeviceEnv: strings.TrimSpace(os.Getenv(hostFixtureDataStorageDeviceEnv)),
	}
	for name, value := range values {
		if value == "" {
			t.Fatalf("mandatory %s fixture is incomplete — %s is empty", capacity, name)
		}
	}
	if !strings.HasPrefix(values[hostFixtureDataStorageDeviceEnv], "/dev/disk/by-id/") {
		t.Fatalf("%s must be a fixed /dev/disk/by-id data-storage identity", hostFixtureDataStorageDeviceEnv)
	}
	if _, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(values[hostFixtureHostKeyEnv] + "\n")); err != nil || len(strings.TrimSpace(string(rest))) != 0 {
		t.Fatalf("%s is not exactly one pinned OpenSSH host public key", hostFixtureHostKeyEnv)
	}
	if info, err := os.Stat(values[hostFixtureSSHKeyPathEnv]); err != nil {
		t.Fatalf("fixture-scoped SSH private key is unavailable: %v", err)
	} else if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("fixture-scoped SSH private key mode is %o, want 0600", info.Mode().Perm())
	}
	fixture := &hostFixture{
		capacity: capacity, address: values[hostFixtureAddressEnv], sshUser: values[hostFixtureSSHUserEnv],
		sshKeyPath:        values[hostFixtureSSHKeyPathEnv],
		dataStorageDevice: values[hostFixtureDataStorageDeviceEnv],
	}
	if capacity == "gpu" {
		fixture.modelDevice = strings.TrimSpace(os.Getenv(hostFixtureModelDeviceEnv))
		fixture.modelUUID = strings.TrimSpace(os.Getenv(hostFixtureModelUUIDEnv))
		if err := validatePermanentGPUStorage(fixture.dataStorageDevice, fixture.modelDevice, fixture.modelUUID); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func validatePermanentGPUStorage(dataStorageDevice, modelDevice, modelUUID string) error {
	if !strings.HasPrefix(modelDevice, "/dev/disk/by-id/") || modelUUID == "" {
		return fmt.Errorf("GPU model cache requires fixed %s and %s", hostFixtureModelDeviceEnv, hostFixtureModelUUIDEnv)
	}
	if modelDevice == dataStorageDevice {
		return fmt.Errorf("GPU model-cache device must be distinct from the Forge data-storage device")
	}
	return nil
}

func (fixture *hostFixture) installName() string {
	return "forge-e2e-" + fixture.capacity
}

// prepare hands a freshly launched host to the scenario. Every run gets its own
// EC2 instance (C1), so there is nothing to destroy or reboot: it waits for the
// host baseline, proves the data device is blank and nothing is installed, and
// on GPU hosts verifies the pinned model-cache device, mount, UUID, and content.
func (fixture *hostFixture) prepare(t *testing.T) error {
	t.Helper()
	client, err := waitForHostReady(context.Background(), fixture.address, fixture.sshKeyPath)
	if err != nil {
		return fmt.Errorf("wait for fresh host readiness: %w", err)
	}
	defer client.Close()
	bootID, err := bootIDFromClient(client)
	if err != nil {
		return fmt.Errorf("read boot ID: %w", err)
	}
	if err := fixture.waitForDataStorageDevice(client); err != nil {
		return err
	}
	if err := fixture.assertFreshBaseline(client); err != nil {
		return err
	}
	if fixture.capacity == "gpu" {
		if err := fixture.validateModelCache(client); err != nil {
			return err
		}
	}
	t.Logf("fresh %s fixture ready: boot %s data-storage=%s", fixture.capacity, bootID, fixture.dataStorageDevice)
	return nil
}

func (fixture *hostFixture) releaseDataStorageConsumers() error {
	client, err := sshDial(fixture.address, fixture.sshKeyPath)
	if err != nil {
		return fmt.Errorf("connect for pre-purge claim release: %w", err)
	}
	defer client.Close()
	script := hostFixtureConsumerReleaseScript(fixture.dataStorageDevice)
	if output, err := sshOutput(client, script); err != nil {
		return fmt.Errorf("release platform consumers/claims before explicit data-storage purge: %w\n%s", err, output)
	}
	return nil
}

// hostFixtureConsumerReleaseFailureReporting makes a silent `set -e`
// abort in the consumer-release purge observable. It replays a bounded tail of
// the purge output and names the exact failing command with its step, exit
// status, source line, and function, so a candidate is never red from
// `Process exited with status 1` alone. Failures inside a command substitution
// are reported by the enclosing assignment instead of leaking duplicate
// reports; a process substitution stays as silent as its reader, which is
// unchanged for the `helm list` and pod-list loops.
const hostFixtureConsumerReleaseFailureReporting = `teardown_evidence_lines=20
teardown_step="initialize consumer release"
teardown_reason=""
teardown_log=$(mktemp "${TMPDIR:-/tmp}/forge-e2e-consumer-release.XXXXXX")
exec 3>&1
exec >"$teardown_log" 2>&1
report_teardown_failure() {
  local status=$?
  if test "${BASH_SUBSHELL:-0}" -ne 0; then
    return 0
  fi
  set +e
  printf 'fixture consumer release failed: step=%s exit_status=%d line=%d function=%s command=%s\n' \
    "$teardown_step" "$status" "${BASH_LINENO[0]:-0}" "${FUNCNAME[1]:-main}" "$BASH_COMMAND" >&3
  exit "$status"
}
finish_teardown_report() {
  local status=$?
  trap - ERR
  set +e
  if test "$status" -ne 0; then
    if test -n "$teardown_reason"; then
      printf 'fixture consumer release failed: step=%s exit_status=%d reason=%s\n' "$teardown_step" "$status" "$teardown_reason" >&3
    fi
    printf 'fixture consumer release evidence (last %s output lines):\n' "$teardown_evidence_lines" >&3
    tail -n "$teardown_evidence_lines" "$teardown_log" >&3
  else
    cat "$teardown_log" >&3
  fi
  rm -f -- "$teardown_log"
}
trap report_teardown_failure ERR
trap finish_teardown_report EXIT
`

const hostFixtureConsumerHelmUninstallFunctions = `report_helm_uninstall_evidence() {
  local release="$1" reason="$2" output="$3" evidence_lines=10
  printf 'fixture consumer release diagnostic: step=helm-uninstall release=%s reason=%s\n' "$release" "$reason" >&2
  printf 'fixture consumer release evidence: helm uninstall output (last %s lines)\n' "$evidence_lines" >&2
  printf '%s\n' "$output" | tail -n "$evidence_lines" >&2
}

uninstall_consumer_release() {
  local release="$1"
  local uninstall_output scheduled_crd expected_uninstall_error
  local crd_observation crd_release crd_remainder crd_namespace crd_deletion instances
  uninstall_output=
  if uninstall_output=$(KUBECONFIG=/etc/rancher/k3s/k3s.yaml helm uninstall "$release" -n iterabase-system --wait --timeout 5m 2>&1); then
    return 0
  fi
  scheduled_crd=$(printf "%s\n" "$uninstall_output" | sed -n 's#^Error: uninstallation completed with 1 error(s): resource CustomResourceDefinition//\([a-z0-9][a-z0-9.-]*\) still exists\. status: Terminating, message: Resource scheduled for deletion$#\1#p')
  if test -z "$scheduled_crd"; then
    report_helm_uninstall_evidence "$release" "helm uninstall returned an unexpected error" "$uninstall_output"
  fi
  test -n "$scheduled_crd"
  expected_uninstall_error="Error: uninstallation completed with 1 error(s): resource CustomResourceDefinition//$scheduled_crd still exists. status: Terminating, message: Resource scheduled for deletion
context deadline exceeded"
  if test "$uninstall_output" != "$expected_uninstall_error"; then
    report_helm_uninstall_evidence "$release" "helm uninstall error did not match the expected terminating-CRD error" "$uninstall_output"
  fi
  test "$uninstall_output" = "$expected_uninstall_error"

  crd_observation=
  if ! crd_observation=$(k3s kubectl get crd "$scheduled_crd" --ignore-not-found=true -o 'jsonpath={.metadata.annotations.meta\.helm\.sh/release-name}|{.metadata.annotations.meta\.helm\.sh/release-namespace}|{.metadata.deletionTimestamp}'); then
    echo "failed to observe scheduled CRD $scheduled_crd authoritatively" >&2
    return 1
  fi
  if test -n "$crd_observation"; then
    crd_release=${crd_observation%%|*}
    crd_remainder=${crd_observation#*|}
    test "$crd_remainder" != "$crd_observation"
    crd_namespace=${crd_remainder%%|*}
    crd_deletion=${crd_remainder#*|}
    test "$crd_deletion" != "$crd_remainder"
    case "$crd_deletion" in *"|"*) return 1 ;; esac
    test "$crd_release" = "$release"
    test "$crd_namespace" = iterabase-system
    test -n "$crd_deletion"
    instances=
    if ! instances=$(k3s kubectl get "$scheduled_crd" -A -o name); then
      echo "failed to observe instances for scheduled CRD $scheduled_crd" >&2
      return 1
    fi
    test -z "$instances"
  fi
  echo "helm uninstall reported CRD $scheduled_crd scheduled for deletion; authoritative absence/ownership/deletion/zero-instance checks passed"
}`

func hostFixtureConsumerReleaseBody(dataStorageDevice string) string {
	return fmt.Sprintf(`data_storage_device=%s
%s
%s
if ! command -v k3s >/dev/null 2>&1 || ! k3s kubectl get --raw=/readyz >/dev/null 2>&1; then exit 0; fi
teardown_step="delete Flux kustomizations"
k3s kubectl delete kustomizations.kustomize.toolkit.fluxcd.io --all -A --ignore-not-found=true --wait=true --timeout=2m || true
teardown_step="delete AgentPool resources"
if k3s kubectl get crd agentpools.platform.iterabase.com >/dev/null 2>&1; then
  k3s kubectl delete agentpools.platform.iterabase.com --all -A --ignore-not-found=true --wait=true --timeout=5m
fi
teardown_step="delete namespaced platform resources"
namespaced_resources=$(k3s kubectl api-resources --api-group=platform.iterabase.com --namespaced=true --verbs=list,delete -o name)
while IFS= read -r resource; do
  test -n "$resource" || continue
  test "$resource" = agentpools.platform.iterabase.com && continue
  k3s kubectl delete "$resource" --all -A --ignore-not-found=true --wait=true --timeout=5m
done <<<"$namespaced_resources"
teardown_step="uninstall consumer releases"
if command -v helm >/dev/null 2>&1; then
  while IFS= read -r release; do
    test -n "$release" || continue
    case "$release" in *-cert-manager|*-lvm-storage) continue ;; esac
    teardown_step="helm uninstall $release"
    uninstall_consumer_release "$release"
  done < <(KUBECONFIG=/etc/rancher/k3s/k3s.yaml helm list -n iterabase-system -q)
fi
teardown_step="delete consumer jobs"
k3s kubectl delete jobs --all -n iterabase-system --ignore-not-found=true --wait=true --timeout=5m
teardown_step="delete pod consumers"
while read -r namespace pod; do
  test -n "$namespace" && test -n "$pod" || continue
  k3s kubectl delete pod "$pod" -n "$namespace" --ignore-not-found=true --wait=true --timeout=5m
done < <(k3s kubectl get pods -A -o go-template="{{range .items}}{{\$namespace := .metadata.namespace}}{{\$pod := .metadata.name}}{{range .spec.volumes}}{{if .persistentVolumeClaim}}{{\$namespace}} {{\$pod}}{{\"\\n\"}}{{end}}{{end}}{{end}}" | sort -u)
teardown_step="delete consumer PVCs"
k3s kubectl delete pvc --all -A --ignore-not-found=true --wait=true --timeout=5m
teardown_step="wait for data-storage convergence"
for i in $(seq 1 150); do
  volumes=0
  if k3s kubectl get crd lvmvolumes.local.openebs.io >/dev/null 2>&1; then volumes=$((volumes + $(k3s kubectl get lvmvolumes.local.openebs.io -A --no-headers | awk "NF {n++} END {print n+0}"))); fi
  lvs_count=0
  if vgs iterabase-data >/dev/null 2>&1; then lvs_count=$(lvs --noheadings --select "vg_name=iterabase-data" -o lv_name | awk "NF {n++} END {print n+0}"); fi
  data_device=$(readlink -f -- "$data_storage_device")
  kernel=$(lsblk -dnro KNAME -- "$data_device")
  holders=0
  if test -d "/sys/class/block/$kernel/holders"; then holders=$(find "/sys/class/block/$kernel/holders" -mindepth 1 -maxdepth 1 | awk "NF {n++} END {print n+0}"); fi
  test "$volumes" = 0 && test "$lvs_count" = 0 && test "$holders" = 0 && exit 0
  sleep 2
done
teardown_reason="data-storage consumers did not converge after 150 attempts"
exit 42
`, candidateShellQuote(dataStorageDevice), hostFixtureConsumerReleaseFailureReporting, hostFixtureConsumerHelmUninstallFunctions)
}

// hostFixtureConsumerReleaseScript runs the purge through `bash -cEeu`.
// -E is required so the ERR reporter also names failures inside the Helm
// uninstall function instead of a bare `Process exited with status 1`.
func hostFixtureConsumerReleaseScript(dataStorageDevice string) string {
	return "sudo bash -cEeu " + candidateShellQuote(hostFixtureConsumerReleaseBody(dataStorageDevice))
}

func (fixture *hostFixture) bootID() (string, error) {
	client, err := sshDial(fixture.address, fixture.sshKeyPath)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return bootIDFromClient(client)
}

func (fixture *hostFixture) waitForReboot(before string) (string, *ssh.Client, error) {
	deadline := time.Now().Add(8 * time.Minute)
	disconnected := false
	for time.Now().Before(deadline) {
		client, err := sshDial(fixture.address, fixture.sshKeyPath)
		if err != nil {
			disconnected = true
			time.Sleep(3 * time.Second)
			continue
		}
		bootID, bootErr := bootIDFromClient(client)
		if bootErr == nil && disconnected && bootID != before {
			return bootID, client, nil
		}
		client.Close()
		time.Sleep(2 * time.Second)
	}
	return "", nil, fmt.Errorf("%s fixture did not prove SSH disconnect, reconnect, and a changed boot ID", fixture.capacity)
}

func bootIDFromClient(client *ssh.Client) (string, error) {
	output, err := sshOutput(client, "cat /proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	bootID := strings.TrimSpace(output)
	if !bootIDPattern.MatchString(bootID) {
		return "", fmt.Errorf("host returned invalid boot ID %q", bootID)
	}
	return bootID, nil
}

func (fixture *hostFixture) waitForDataStorageDevice(client *ssh.Client) error {
	deadline := time.Now().Add(2 * time.Minute)
	command := "test -L " + candidateShellQuote(fixture.dataStorageDevice) + " && test -b \"$(readlink -f " + candidateShellQuote(fixture.dataStorageDevice) + ")\""
	var lastErr error
	for time.Now().Before(deadline) {
		if _, lastErr = sshOutput(client, command); lastErr == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("dedicated data-storage device %s did not appear on %s: %w", fixture.dataStorageDevice, fixture.address, lastErr)
}

func (fixture *hostFixture) assertFreshBaseline(client *ssh.Client) error {
	script := fmt.Sprintf(`
! command -v k3s >/dev/null 2>&1
test ! -e /var/lib/iterabase/data-storage.receipt
! vgs iterabase-data >/dev/null 2>&1
! pvs "$(readlink -f -- %s)" >/dev/null 2>&1
data_device=$(readlink -f -- %s)
test -b "$data_device"
test -z "$(wipefs -n --noheadings --output TYPE -- "$data_device" | awk 'NF')"
test ! -e /var/lib/rancher/k3s
`, candidateShellQuote(fixture.dataStorageDevice), candidateShellQuote(fixture.dataStorageDevice))
	if output, err := sshOutput(client, "sudo bash -ceu "+candidateShellQuote(script)); err != nil {
		return fmt.Errorf("fresh fixture baseline assertion failed: %w\n%s", err, output)
	}
	return nil
}

func loadModelCacheAuthority() (modelCacheAuthority, error) {
	return decodeModelCacheAuthority(modelCacheAuthorityJSON)
}

func decodeModelCacheAuthority(data []byte) (modelCacheAuthority, error) {
	var authority modelCacheAuthority
	if err := json.Unmarshal(data, &authority); err != nil {
		return authority, fmt.Errorf("decode model-cache authority: %w", err)
	}
	if authority.SchemaVersion != 1 || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(authority.Revision) ||
		!regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(authority.SHA256) || authority.ModelID == "" ||
		authority.WeightPath == "" || filepath.IsAbs(authority.WeightPath) || strings.Contains(authority.WeightPath, "..") {
		return authority, fmt.Errorf("model-cache authority is incomplete")
	}
	return authority, nil
}

func (fixture *hostFixture) validateModelCache(client *ssh.Client) error {
	authority, err := loadModelCacheAuthority()
	if err != nil {
		return err
	}
	weightPath := filepath.Join(hostFixtureModelMount, authority.WeightPath)
	script := fmt.Sprintf(`
data_device=$(readlink -f -- %s)
cache=$(readlink -f -- %s)
test -b "$data_device" && test -b "$cache" && test "$data_device" != "$cache"
source=$(findmnt -n -o SOURCE --mountpoint %s)
source=${source%%%%[*}
test "$(readlink -f -- "$source")" = "$cache"
test "$(blkid -p -s UUID -o value -- "$cache")" = %s
weight=$(readlink -f -- %s)
case "$weight" in %s/*) ;; *) exit 42 ;; esac
weight_source=$(findmnt -n -o SOURCE --target "$weight")
weight_source=${weight_source%%%%[*}
test "$(readlink -f -- "$weight_source")" = "$cache"
test "$(sha256sum -- "$weight" | awk '{print $1}')" = %s
`, candidateShellQuote(fixture.dataStorageDevice), candidateShellQuote(fixture.modelDevice), candidateShellQuote(hostFixtureModelMount), candidateShellQuote(fixture.modelUUID), candidateShellQuote(weightPath), candidateShellQuote(hostFixtureModelMount), candidateShellQuote(authority.SHA256))
	if output, err := sshOutput(client, "sudo bash -ceu "+candidateShellQuote(script)); err != nil {
		return fmt.Errorf("GPU model-cache identity/revision/hash validation failed: %w\n%s", err, output)
	}
	return nil
}

func TestHostFixtureConsumerReleaseScriptIsValid(t *testing.T) {
	script := hostFixtureConsumerReleaseScript("/dev/disk/by-id/test-data-storage")
	command := exec.Command("bash", "-n")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("consumer-release shell is invalid: %v\n%s", err, output)
	}
	if !strings.HasPrefix(script, "sudo bash -cEeu ") {
		t.Fatal("consumer-release shell must run with errtrace (-E) so the ERR reporter also names failures inside shell functions")
	}
	if strings.Contains(script, `helm uninstall "$release" -n iterabase-system --wait --timeout 5m || true`) {
		t.Fatal("consumer-release shell must not broadly ignore Helm uninstall failures")
	}
}

func TestHostFixtureConsumerReleaseReportsFailingCommand(t *testing.T) {
	for _, test := range []struct {
		name             string
		k3s              string
		helm             string
		wantReports      []string
		omittedEvidence  string
		evidenceMarker   string
		maxEvidenceLines int
	}{
		{
			name: "unexpected helm uninstall error",
			k3s: `
if test "$1" != kubectl; then exit 90; fi
shift
case "$1" in
  get)
    shift
    test "$1" = --raw=/readyz && exit 0
    test "$1" = crd && exit 1
    exit 91
    ;;
  delete) exit 0 ;;
  api-resources) exit 0 ;;
esac
exit 92
`,
			helm: `
if test "$1" = list; then
  printf 'test-release\n'
  exit 0
fi
if test "$1" = uninstall; then
  index=0
  while test "$index" -lt 40; do
    index=$((index + 1))
    printf 'unexpected helm error line %02d\n' "$index"
  done
  exit 1
fi
exit 90
`,
			wantReports: []string{
				`fixture consumer release diagnostic: step=helm-uninstall release=test-release`,
				`fixture consumer release failed: step=helm uninstall test-release`,
				`exit_status=1`,
				`command=test -n "$scheduled_crd"`,
				`unexpected helm error line 40`,
			},
			omittedEvidence:  "unexpected helm error line 01",
			evidenceMarker:   "unexpected helm error line",
			maxEvidenceLines: 10,
		},
		{
			name: "failing platform delete",
			k3s: `
if test "$1" != kubectl; then exit 90; fi
shift
case "$1" in
  get)
    shift
    test "$1" = --raw=/readyz && exit 0
    test "$1" = crd && exit 0
    exit 91
    ;;
  delete)
    shift
    case "${1:-}" in
      agentpools.platform.iterabase.com)
        index=0
        while test "$index" -lt 40; do
          index=$((index + 1))
          printf 'agentpool delete failure %02d\n' "$index"
        done
        exit 1
        ;;
    esac
    exit 0
    ;;
  api-resources) exit 0 ;;
esac
exit 92
`,
			helm: "exit 90\n",
			wantReports: []string{
				`fixture consumer release failed: step=delete AgentPool resources`,
				`exit_status=1`,
				`command=k3s kubectl delete agentpools.platform.iterabase.com`,
				`agentpool delete failure 40`,
			},
			omittedEvidence:  "agentpool delete failure 01",
			evidenceMarker:   "agentpool delete failure",
			maxEvidenceLines: 20,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fakeBin := t.TempDir()
			writeHostFixtureFakeCommand(t, fakeBin, "k3s", test.k3s)
			writeHostFixtureFakeCommand(t, fakeBin, "helm", test.helm)
			command := exec.Command("bash", "-cEeu", hostFixtureConsumerReleaseBody("/dev/disk/by-id/test-data-storage"))
			command.Env = []string{"PATH=" + fakeBin + ":" + os.Getenv("PATH")}
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("forced teardown failure unexpectedly succeeded:\n%s", output)
			}
			for _, want := range test.wantReports {
				if !strings.Contains(string(output), want) {
					t.Fatalf("teardown failure report lacks %q:\n%s", want, output)
				}
			}
			if strings.Contains(string(output), test.omittedEvidence) {
				t.Fatalf("teardown failure report leaked unbounded evidence %q:\n%s", test.omittedEvidence, output)
			}
			if got := strings.Count(string(output), test.evidenceMarker); got > test.maxEvidenceLines {
				t.Fatalf("teardown failure report is not bounded: %d evidence lines, want at most %d\n%s", got, test.maxEvidenceLines, output)
			}
		})
	}
}

func TestHostFixtureConsumerReleaseSucceedsWithFailureReporting(t *testing.T) {
	fakeBin := t.TempDir()
	writeHostFixtureFakeCommand(t, fakeBin, "k3s", `
if test "$1" != kubectl; then exit 90; fi
shift
case "$1" in
  get)
    shift
    test "$1" = --raw=/readyz && exit 0
    exit 1
    ;;
  delete)
    printf 'k3s-teardown-step\n'
    exit 0
    ;;
  api-resources) exit 0 ;;
esac
exit 92
`)
	writeHostFixtureFakeCommand(t, fakeBin, "helm", `
if test "$1" = list; then exit 0; fi
exit 90
`)
	writeHostFixtureFakeHostTools(t, fakeBin)
	command := exec.Command("bash", "-cEeu", hostFixtureConsumerReleaseBody("/dev/disk/by-id/test-data-storage"))
	command.Env = []string{"PATH=" + fakeBin + ":" + os.Getenv("PATH")}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("successful teardown failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "k3s-teardown-step") {
		t.Fatalf("successful teardown dropped its output:\n%s", output)
	}
	if strings.Contains(string(output), "fixture consumer release failed:") {
		t.Fatalf("successful teardown reported a failure:\n%s", output)
	}
}

func TestHostFixtureConsumerReleaseReportsNonConvergence(t *testing.T) {
	fakeBin := t.TempDir()
	writeHostFixtureFakeCommand(t, fakeBin, "k3s", `
if test "$1" != kubectl; then exit 90; fi
shift
case "$1" in
  get)
    shift
    case "${1:-}" in
      --raw=/readyz) exit 0 ;;
      crd) exit 0 ;;
      lvmvolumes.local.openebs.io) printf 'lvmvolume.local.openebs.io/stuck\n'; exit 0 ;;
    esac
    exit 91
    ;;
  delete)
    printf 'k3s-teardown-step\n'
    exit 0
    ;;
  api-resources) exit 0 ;;
esac
exit 92
`)
	writeHostFixtureFakeCommand(t, fakeBin, "helm", `
if test "$1" = list; then exit 0; fi
exit 90
`)
	writeHostFixtureFakeHostTools(t, fakeBin)
	command := exec.Command("bash", "-cEeu", hostFixtureConsumerReleaseBody("/dev/disk/by-id/test-data-storage"))
	command.Env = []string{"PATH=" + fakeBin + ":" + os.Getenv("PATH")}
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 42 {
		t.Fatalf("non-converging teardown err = %v, want exit status 42:\n%s", err, output)
	}
	for _, want := range []string{
		"fixture consumer release failed: step=wait for data-storage convergence exit_status=42",
		"data-storage consumers did not converge after 150 attempts",
		"fixture consumer release evidence (last 20 output lines):",
	} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("non-convergence report lacks %q:\n%s", want, output)
		}
	}
}

func TestHostFixtureConsumerHelmUninstallIsAuthoritative(t *testing.T) {
	const (
		release = "test-release"
		crd     = "widgets.platform.iterabase.com"
	)
	terminating := "Error: uninstallation completed with 1 error(s): resource CustomResourceDefinition//" + crd + " still exists. status: Terminating, message: Resource scheduled for deletion\ncontext deadline exceeded"
	for _, test := range []struct {
		name                string
		helmStatus          int
		helmOutput          string
		crdStatus           int
		crdObservation      string
		instanceStatus      int
		instanceObservation string
		wantError           bool
		wantAcceptedMarker  bool
	}{
		{name: "ordinary successful uninstall", helmStatus: 0},
		{name: "terminating owned empty CRD", helmStatus: 1, helmOutput: terminating, crdObservation: release + "|iterabase-system|2026-09-10T20:00:00Z", wantAcceptedMarker: true},
		{name: "authoritatively absent CRD", helmStatus: 1, helmOutput: terminating, wantAcceptedMarker: true},
		{name: "terminating error without context deadline", helmStatus: 1, helmOutput: strings.Split(terminating, "\n")[0], crdObservation: release + "|iterabase-system|2026-09-10T20:00:00Z", wantError: true},
		{name: "unrelated Helm error", helmStatus: 1, helmOutput: "Error: Kubernetes cluster unreachable", wantError: true},
		{name: "CRD observation error", helmStatus: 1, helmOutput: terminating, crdStatus: 1, wantError: true},
		{name: "foreign release owner", helmStatus: 1, helmOutput: terminating, crdObservation: "other-release|iterabase-system|2026-09-10T20:00:00Z", wantError: true},
		{name: "foreign release namespace", helmStatus: 1, helmOutput: terminating, crdObservation: release + "|other-system|2026-09-10T20:00:00Z", wantError: true},
		{name: "missing deletion timestamp", helmStatus: 1, helmOutput: terminating, crdObservation: release + "|iterabase-system|", wantError: true},
		{name: "instance observation error", helmStatus: 1, helmOutput: terminating, crdObservation: release + "|iterabase-system|2026-09-10T20:00:00Z", instanceStatus: 1, wantError: true},
		{name: "remaining instance", helmStatus: 1, helmOutput: terminating, crdObservation: release + "|iterabase-system|2026-09-10T20:00:00Z", instanceObservation: "widgets.platform.iterabase.com/remaining", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fakeBin := t.TempDir()
			writeHostFixtureFakeCommand(t, fakeBin, "helm", `
if test "$1" != uninstall; then exit 90; fi
printf "%s" "${HOR545_FAKE_HELM_OUTPUT:-}"
exit "${HOR545_FAKE_HELM_STATUS:-0}"
`)
			writeHostFixtureFakeCommand(t, fakeBin, "k3s", `
if test "$1" != kubectl; then exit 90; fi
shift
if test "$1" != get; then exit 91; fi
shift
if test "$1" = crd; then
  shift
  test "$1" = "$HOR545_FAKE_CRD_NAME"
  shift
  test "$1" = --ignore-not-found=true
  shift
  test "$1" = -o
  shift
  case "$1" in jsonpath=*) ;; *) exit 92 ;; esac
  printf "%s" "${HOR545_FAKE_CRD_OBSERVATION:-}"
  exit "${HOR545_FAKE_CRD_STATUS:-0}"
fi
test "$1" = "$HOR545_FAKE_CRD_NAME"
shift
test "$1" = -A
shift
test "$1" = -o
shift
test "$1" = name
printf "%s" "${HOR545_FAKE_INSTANCE_OBSERVATION:-}"
exit "${HOR545_FAKE_INSTANCE_STATUS:-0}"
`)
			command := exec.Command("bash", "-ceu", hostFixtureConsumerHelmUninstallFunctions+"\nuninstall_consumer_release "+release)
			command.Env = []string{
				"PATH=" + fakeBin + ":" + os.Getenv("PATH"),
				"HOR545_FAKE_HELM_STATUS=" + fmt.Sprint(test.helmStatus),
				"HOR545_FAKE_HELM_OUTPUT=" + test.helmOutput,
				"HOR545_FAKE_CRD_NAME=" + crd,
				"HOR545_FAKE_CRD_STATUS=" + fmt.Sprint(test.crdStatus),
				"HOR545_FAKE_CRD_OBSERVATION=" + test.crdObservation,
				"HOR545_FAKE_INSTANCE_STATUS=" + fmt.Sprint(test.instanceStatus),
				"HOR545_FAKE_INSTANCE_OBSERVATION=" + test.instanceObservation,
			}
			output, err := command.CombinedOutput()
			if (err != nil) != test.wantError {
				t.Fatalf("uninstall error = %v, wantError=%t\n%s", err, test.wantError, output)
			}
			accepted := strings.Contains(string(output), "authoritative absence/ownership/deletion/zero-instance checks passed")
			if accepted != test.wantAcceptedMarker {
				t.Fatalf("accepted marker = %t, want %t\n%s", accepted, test.wantAcceptedMarker, output)
			}
		})
	}
}

func writeHostFixtureFakeHostTools(t *testing.T, fakeBin string) {
	t.Helper()
	writeHostFixtureFakeCommand(t, fakeBin, "vgs", "exit 1\n")
	writeHostFixtureFakeCommand(t, fakeBin, "lvs", "exit 1\n")
	writeHostFixtureFakeCommand(t, fakeBin, "lsblk", "printf 'forge-e2e-fake-kernel\\n'\n")
	writeHostFixtureFakeCommand(t, fakeBin, "readlink", "printf '/dev/disk/by-id/test-data-storage\\n'\n")
	writeHostFixtureFakeCommand(t, fakeBin, "seq", "printf '1\\n'\n")
	writeHostFixtureFakeCommand(t, fakeBin, "sleep", "exit 0\n")
}

func writeHostFixtureFakeCommand(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/usr/bin/env bash\nset -eu\n"+body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

func TestModelCacheAuthorityPinsImmutablePublicWeight(t *testing.T) {
	authority, err := loadModelCacheAuthority()
	if err != nil {
		t.Fatal(err)
	}
	if authority.ModelID != "Qwen/Qwen3.5-0.8B" || authority.Revision != "2fc06364715b967f1860aea9cf38778875588b17" ||
		authority.SHA256 != "04b1c301231dd422b8860db31311ab2721511346a32cb1e079c4c4e5f1fe4696" {
		t.Fatalf("model-cache authority drifted: %+v", authority)
	}
}

func TestModelCacheAuthorityRejectsFloatingCorruptAndEscapingRecords(t *testing.T) {
	valid, err := loadModelCacheAuthority()
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*modelCacheAuthority){
		"floating revision": func(authority *modelCacheAuthority) { authority.Revision = "main" },
		"corrupt hash":      func(authority *modelCacheAuthority) { authority.SHA256 = strings.Repeat("g", 64) },
		"escaping path":     func(authority *modelCacheAuthority) { authority.WeightPath = "../workspace/marker" },
	} {
		t.Run(name, func(t *testing.T) {
			authority := valid
			mutate(&authority)
			data, err := json.Marshal(authority)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeModelCacheAuthority(data); err == nil {
				t.Fatalf("invalid model-cache authority unexpectedly passed: %+v", authority)
			}
		})
	}
}

func TestPermanentGPUFixtureRejectsDataStorageCacheSubstitution(t *testing.T) {
	dataStorage := "/dev/disk/by-id/data-storage"
	if err := validatePermanentGPUStorage(dataStorage, dataStorage, "cache-uuid"); err == nil {
		t.Fatal("Forge data-storage device unexpectedly passed as the model-cache device")
	}
	if err := validatePermanentGPUStorage(dataStorage, "/dev/sdc", "cache-uuid"); err == nil {
		t.Fatal("volatile model-cache device unexpectedly passed")
	}
	if err := validatePermanentGPUStorage(dataStorage, "/dev/disk/by-id/model-cache", ""); err == nil {
		t.Fatal("missing model-cache UUID unexpectedly passed")
	}
}
