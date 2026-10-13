package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/forge/test/e2e/internal/remotecluster"
)

// Run the real testing.T-based helpers in a subprocess so their fail-closed
// Fatalf paths can be asserted without failing the parent test.
func TestWorkspaceWorkerReplacement(t *testing.T) {
	if scenario := os.Getenv("FORGE_WORKSPACE_REPLACEMENT_TEST"); scenario != "" {
		cluster := remotecluster.Use(t, "/tmp/workspace-replacement-kubeconfig")
		switch scenario {
		case "same-name-ready", "delete-error", "pvc-changed":
			replaceIdleWorkspaceWorkerAtGate(t, cluster, &cpuFixtureState{agentPoolPVCUID: "original-pvc"})
		case "transition-to-ready":
			waitWorkspaceReplacementPod(t, cluster, "forge-storage-pool-worker-0", "original-uid", 8*time.Second)
		default:
			waitWorkspaceReplacementPod(t, cluster, "forge-storage-pool-worker-0", "original-uid", 100*time.Millisecond)
		}
		return
	}

	tests := []struct {
		name string
		want string
	}{
		{name: "same-name-ready"},
		{name: "transition-to-ready"},
		{name: "replacement-ready"},
		{name: "original-ready", want: `old=original-uid last="original-uid|True"`},
		{name: "absent", want: `old=original-uid last=""`},
		{name: "replacement-not-ready", want: `old=original-uid last="replacement-uid|False"`},
		{name: "empty-uid", want: `old=original-uid last="|True"`},
		{name: "observation-error", want: "observation forbidden"},
		{name: "delete-error", want: "delete forbidden"},
		{name: "pvc-changed", want: "in-band worker replacement changed AgentPool PVC"},
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			kubectl := filepath.Join(dir, "kubectl")
			if err := os.WriteFile(kubectl, []byte(workspaceReplacementKubectl), 0o700); err != nil {
				t.Fatal(err)
			}
			trace := filepath.Join(dir, "commands")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, "-test.run=^TestWorkspaceWorkerReplacement$", "-test.v")
			command.Env = append(os.Environ(),
				"FORGE_WORKSPACE_REPLACEMENT_TEST="+test.name,
				"FORGE_WORKSPACE_REPLACEMENT_TRACE="+trace,
				"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
			)
			started := time.Now()
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("replacement helper did not terminate: %v\n%s", ctx.Err(), output)
			}
			if test.want == "" {
				if err != nil {
					t.Fatalf("replacement helper failed: %v\n%s", err, output)
				}
			} else if err == nil || !strings.Contains(string(output), test.want) {
				t.Fatalf("error=%v output=%s, want failure containing %q", err, output, test.want)
			}
			// Allow process startup/race-detector overhead, but reject the old
			// unconditional two-second sleep after a 100ms observation bound.
			if test.want != "" && time.Since(started) >= 2*time.Second {
				t.Fatalf("bounded failure took %s\n%s", time.Since(started), output)
			}
			commands, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			calls := string(commands)
			if test.name == "same-name-ready" {
				if !strings.Contains(calls, "delete pod/forge-storage-pool-worker-0 -n iterabase-system --wait=false --timeout=3m") {
					t.Fatalf("worker delete must not wait for name absence:\n%s", calls)
				}
				if !strings.Contains(calls, "--ignore-not-found -o jsonpath={.metadata.uid}|{.status.conditions") {
					t.Fatalf("replacement did not check UID and readiness:\n%s", calls)
				}
				if strings.Count(calls, "get pvc/forge-storage-pool-sandbox") != 2 {
					t.Fatalf("replacement lost the before/after PVC identity check:\n%s", calls)
				}
			}
			if test.name == "transition-to-ready" && strings.Count(calls, "get pod/") != 4 {
				t.Fatalf("replacement must observe original, absent, not-ready, then Ready:\n%s", calls)
			}
			if test.name == "delete-error" && strings.Contains(calls, "--ignore-not-found") {
				t.Fatalf("failed deletion was followed by replacement observation:\n%s", calls)
			}
			if test.name == "observation-error" && strings.Count(calls, "get pod/") != 1 {
				t.Fatalf("observation failure was retried:\n%s", calls)
			}
		})
	}
}

// A name-based deletion wait fails just as it did in release CI, despite the
// controller's same-name, different-UID replacement already being Ready.
const workspaceReplacementKubectl = `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FORGE_WORKSPACE_REPLACEMENT_TRACE"
test "$1" = --kubeconfig
test "$2" = /tmp/workspace-replacement-kubeconfig
shift 2
case "$1/$2" in
get/pods)
  printf 'pod/forge-storage-pool-worker-0\n'
  ;;
get/pvc/forge-storage-pool-sandbox)
  if test "$FORGE_WORKSPACE_REPLACEMENT_TEST" = pvc-changed && test -f "$FORGE_WORKSPACE_REPLACEMENT_TRACE.pvc"; then
    printf 'changed-pvc\n'
  else
    printf 'original-pvc\n'
  fi
  touch "$FORGE_WORKSPACE_REPLACEMENT_TRACE.pvc"
  ;;
delete/pod/forge-storage-pool-worker-0)
  if test "$FORGE_WORKSPACE_REPLACEMENT_TEST" = delete-error; then
    printf 'delete forbidden\n' >&2
    exit 1
  fi
  case " $* " in
    *' --wait=false '*) printf 'pod "forge-storage-pool-worker-0" deleted\n' ;;
    *) printf 'error: timed out waiting for the condition on pods/forge-storage-pool-worker-0\n' >&2; exit 1 ;;
  esac
  ;;
get/pod/forge-storage-pool-worker-0)
  case "$*" in
    *'jsonpath={.metadata.uid}' ) printf 'original-uid\n'; exit 0 ;;
  esac
  case "$FORGE_WORKSPACE_REPLACEMENT_TEST" in
    original-ready) printf 'original-uid|True\n' ;;
    absent) ;;
    replacement-not-ready) printf 'replacement-uid|False\n' ;;
    empty-uid) printf '|True\n' ;;
    observation-error) printf 'observation forbidden\n' >&2; exit 1 ;;
    transition-to-ready)
      count=$(grep -c 'get pod/' "$FORGE_WORKSPACE_REPLACEMENT_TRACE")
      case "$count" in
        1) printf 'original-uid|True\n' ;;
        2) ;;
        3) printf 'replacement-uid|False\n' ;;
        *) printf 'replacement-uid|True\n' ;;
      esac
      ;;
    *) printf 'replacement-uid|True\n' ;;
  esac
  ;;
*) printf 'unexpected kubectl arguments: %s\n' "$*" >&2; exit 1 ;;
esac
`
