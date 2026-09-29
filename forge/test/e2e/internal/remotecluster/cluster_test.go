package remotecluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKubectlSeparatedReturnsExactStdout(t *testing.T) {
	installFakeKubectl(t, `#!/bin/sh
printf '2\n'
`)
	cluster := &Cluster{Kubeconfig: "/tmp/test-kubeconfig"}
	stdout, stderr, err := cluster.KubectlSeparated("exec", "postgresql", "--", "psql")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "2\n" {
		t.Fatalf("stdout=%q want exact PostgreSQL scalar", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr=%q want empty", stderr)
	}
}

func TestKubectlSeparatedDoesNotContaminateStdoutWithWarning(t *testing.T) {
	installFakeKubectl(t, `#!/bin/sh
printf '2\n'
printf 'E0915 websocket.go:296] Unknown stream id 1, discarding message\n' >&2
`)
	cluster := &Cluster{Kubeconfig: "/tmp/test-kubeconfig"}
	stdout, stderr, err := cluster.KubectlSeparated("exec", "postgresql", "--", "psql")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "2\n" {
		t.Fatalf("warning contaminated stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "Unknown stream id 1") {
		t.Fatalf("stderr did not retain transport warning: %q", stderr)
	}
}

func TestKubectlSeparatedBoundsBothFailureStreams(t *testing.T) {
	installFakeKubectl(t, `#!/bin/sh
printf 'stdout-marker:'
head -c 20000 /dev/zero | tr '\000' x
printf 'stderr-marker:' >&2
head -c 20000 /dev/zero | tr '\000' y >&2
exit 23
`)
	cluster := &Cluster{Kubeconfig: "/tmp/test-kubeconfig"}
	stdout, stderr, err := cluster.KubectlSeparated("exec", "postgresql", "--", "psql")
	if err == nil {
		t.Fatal("non-zero kubectl execution returned no error")
	}
	if !strings.Contains(stdout, "stdout-marker:") || !strings.Contains(stderr, "stderr-marker:") {
		t.Fatalf("failure streams lost their markers: stdout=%q stderr=%q", stdout, stderr)
	}
	if len(stdout) > maxSeparatedCommandStreamBytes || len(stderr) > maxSeparatedCommandStreamBytes {
		t.Fatalf("failure streams are unbounded: stdout=%d stderr=%d", len(stdout), len(stderr))
	}
	message := err.Error()
	for _, required := range []string{"exit status 23", "stdout-marker:", "stderr-marker:", "truncated; 20014 bytes total"} {
		if !strings.Contains(message, required) {
			t.Fatalf("failure diagnostic does not contain %q:\n%s", required, message)
		}
	}
}

func installFakeKubectl(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
