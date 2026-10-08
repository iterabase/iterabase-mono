package e2e

import (
	"os/exec"
	"strings"
	"testing"
)

// hostInotifyDropInPath is the stable Forge-owned managed-host drop-in HOR-569
// reconciles on every apply and upgrade.
const hostInotifyDropInPath = "/etc/sysctl.d/90-iterabase-k3s-inotify.conf"

// hostInotifyEvidenceScript proves the released Forge artifact persisted and
// applied the exact canonical value on the fixture host, then prints the
// live value and the drop-in's device/inode/mtime identity so the reapply
// stage can prove Forge did not rewrite it.
const hostInotifyEvidenceScript = `sudo bash -ceu '
dropin=/etc/sysctl.d/90-iterabase-k3s-inotify.conf
test -f "$dropin" && test ! -L "$dropin"
test "$(stat -c "%u:%g:%a" -- "$dropin")" = 0:0:644
test "$(cat -- "$dropin")" = "fs.inotify.max_user_instances = 8192"
effective=$(cat /proc/sys/fs/inotify/max_user_instances)
case "$effective" in *[!0-9]*) printf "effective inotify instance value is not numeric: %s\n" "$effective" >&2; exit 42 ;; esac
test -n "$effective" || { printf "effective inotify instance value is empty\n" >&2; exit 42; }
test "$effective" = 8192
printf "%s|%s\n" "$effective" "$(stat -c "%d:%i:%Y" -- "$dropin")"
'`

// hostInotifyFollowScript proves Kubernetes CRI log following stays connected
// on the reconciled host: the followed stream must deliver several lines and
// must never emit the fsnotify EMFILE failure the OPO1 incident reported.
const hostInotifyFollowScript = `sudo bash -ceu '
pod=forge-inotify-follow
k3s kubectl -n default delete pod "$pod" --ignore-not-found=true --wait=true --timeout=1m >/dev/null 2>&1 || true
k3s kubectl -n default run "$pod" --restart=Never --image=debian:13-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132 --command -- sh -c "while true; do echo follow-marker; sleep 1; done" >/dev/null
k3s kubectl -n default wait --for=condition=Ready pod/"$pod" --timeout=5m >/dev/null
output=$(timeout 15 k3s kubectl -n default logs -f "$pod" || true)
k3s kubectl -n default delete pod "$pod" --wait=true --timeout=5m >/dev/null
case "$output" in
  *fsnotify*|*"too many open files"*) printf "%s\n" "$output" >&2; exit 42 ;;
esac
count=$(printf "%s\n" "$output" | grep -c "^follow-marker" || true)
case "$count" in *[!0-9]*) exit 43 ;; esac
test -n "$count"
test "$count" -ge 3
printf "followed-lines=%s\n" "$count"
'`

// assertHostInotifyCapacityStage proves the fresh/target release reconciled the
// canonical persistent and live inotify instance ceiling and that followed pod
// logs stream without the fsnotify EMFILE failure. It records the drop-in
// identity for the post-reapply proof.
func assertHostInotifyCapacityStage(t *testing.T, state *cpuFixtureState) {
	t.Helper()
	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer sc.Close()
	evidence := strings.TrimSpace(mustSSHOutput(t, sc, hostInotifyEvidenceScript))
	parts := strings.Split(evidence, "|")
	if len(parts) != 2 || parts[0] != "8192" || parts[1] == "" {
		t.Fatalf("managed host inotify evidence = %q, want effective=8192 and a drop-in identity", evidence)
	}
	state.inotifyDropInIdentity = parts[1]
	followed := strings.TrimSpace(mustSSHOutput(t, sc, hostInotifyFollowScript))
	if !strings.Contains(followed, "followed-lines=") {
		t.Fatalf("followed pod-log streaming did not report observed lines: %q", followed)
	}
	t.Logf("host inotify capacity verified: drop-in=%s %s", state.inotifyDropInIdentity, followed)
}

// assertHostInotifyReapplyStage proves a repeated Forge apply left the
// canonical drop-in byte-for-byte and inode-for-inode untouched while the live
// value remained the required 8192.
func assertHostInotifyReapplyStage(t *testing.T, state *cpuFixtureState) {
	t.Helper()
	if state.inotifyDropInIdentity == "" {
		t.Fatal("host inotify drop-in identity was not recorded before reapply")
	}
	sc, err := sshDial(state.ip, state.privKeyPath)
	if err != nil {
		t.Fatalf("ssh dial %s: %v", state.ip, err)
	}
	defer sc.Close()
	evidence := strings.TrimSpace(mustSSHOutput(t, sc, hostInotifyEvidenceScript))
	parts := strings.Split(evidence, "|")
	if len(parts) != 2 || parts[0] != "8192" || parts[1] != state.inotifyDropInIdentity {
		t.Fatalf("forge reapply changed the canonical host inotify state: before=%q after=%q", state.inotifyDropInIdentity, evidence)
	}
}

func TestHostInotifyFixtureScriptsAreValid(t *testing.T) {
	for name, script := range map[string]string{
		"evidence": hostInotifyEvidenceScript,
		"follow":   hostInotifyFollowScript,
	} {
		t.Run(name, func(t *testing.T) {
			inner := exec.Command("bash", "-n")
			inner.Stdin = strings.NewReader(script)
			if output, err := inner.CombinedOutput(); err != nil {
				t.Fatalf("%s shell is invalid: %v\n%s", name, err, output)
			}
			// The scripts are executed as `sudo bash -ceu '<script>'` over SSH, so
			// parse the exact assembled command line too: an embedded single quote
			// would terminate the remote argument and corrupt the command.
			outer := exec.Command("bash", "-n", "-c", "sudo bash -ceu "+candidateShellQuote(script))
			if output, err := outer.CombinedOutput(); err != nil {
				t.Fatalf("%s remote command line is invalid: %v\n%s", name, err, output)
			}
		})
	}
	if !strings.Contains(hostInotifyEvidenceScript, hostInotifyDropInPath) {
		t.Fatalf("evidence script does not assert the Forge-owned drop-in %s", hostInotifyDropInPath)
	}
	if !strings.Contains(hostInotifyEvidenceScript, "0:0:644") {
		t.Fatal("evidence script does not assert root ownership and mode 0644")
	}
	if !strings.Contains(hostInotifyEvidenceScript, "fs.inotify.max_user_instances = 8192") {
		t.Fatal("evidence script does not assert the canonical persisted value")
	}
}
