package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nunocgoncalves/iterabase-mono/testkit/e2e/redact"
)

func TestPinnedImageCacheManifestParsesTheSeededContract(t *testing.T) {
	valid := `{
		"schema_version": 1,
		"capacity": "cpu",
		"generation": "` + strings.Repeat("a", 64) + `",
		"cache_root": "/var/lib/iterabase-e2e/image-cache",
		"images": [
			{"reference": "ghcr.io/nunocgoncalves/iterabase-third-party/minio:RELEASE.2025-09-07T16-13-09Z@sha256:786c852164a4fab14fd194fdbe6b4ed6f34934fcf6e4556aa9368432081719e9", "digest": "sha256:` + strings.Repeat("b", 64) + `", "config_digest": "sha256:` + strings.Repeat("c", 64) + `", "archive": "ghcr.io_nunocgoncalves_iterabase-third-party_minio-abc.tar", "sha256": "` + strings.Repeat("d", 64) + `", "size": 12345678}
		]
	}`
	manifest, err := parsePinnedImageCacheManifest("cpu", []byte(valid))
	if err != nil {
		t.Fatalf("valid manifest was rejected: %v", err)
	}
	if manifest.Generation != strings.Repeat("a", 64) || len(manifest.Images) != 1 {
		t.Fatalf("manifest was not parsed exactly: %+v", manifest)
	}
}

func TestPinnedImageCacheManifestRejectsInvalidContracts(t *testing.T) {
	generation := strings.Repeat("a", 64)
	digest := "sha256:" + strings.Repeat("b", 64)
	cases := map[string]string{
		"unsupported schema": `{"schema_version":2,"capacity":"cpu","generation":"` + generation + `","cache_root":"/cache","images":[{"reference":"busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0","digest":"` + digest + `","archive":"busybox.tar"}]}`,
		"wrong capacity":     `{"schema_version":1,"capacity":"gpu","generation":"` + generation + `","cache_root":"/cache","images":[{"reference":"busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0","digest":"` + digest + `","archive":"busybox.tar"}]}`,
		"bad generation":     `{"schema_version":1,"capacity":"cpu","generation":"short","cache_root":"/cache","images":[{"reference":"busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0","digest":"` + digest + `","archive":"busybox.tar"}]}`,
		"no images":          `{"schema_version":1,"capacity":"cpu","generation":"` + generation + `","cache_root":"/cache","images":[]}`,
		"bad digest":         `{"schema_version":1,"capacity":"cpu","generation":"` + generation + `","cache_root":"/cache","images":[{"reference":"busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0","digest":"sha256:xyz","archive":"busybox.tar"}]}`,
		"bad config digest":  `{"schema_version":1,"capacity":"cpu","generation":"` + generation + `","cache_root":"/cache","images":[{"reference":"busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0","digest":"` + digest + `","config_digest":"sha256:xyz","archive":"busybox.tar"}]}`,
		"path traversal":     `{"schema_version":1,"capacity":"cpu","generation":"` + generation + `","cache_root":"/cache","images":[{"reference":"busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0","digest":"` + digest + `","archive":"../busybox.tar"}]}`,
		"duplicate archive":  `{"schema_version":1,"capacity":"cpu","generation":"` + generation + `","cache_root":"/cache","images":[{"reference":"busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0","digest":"` + digest + `","archive":"busybox.tar"},{"reference":"debian:13-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132","digest":"` + digest + `","archive":"busybox.tar"}]}`,
	}
	for name, payload := range cases {
		if _, err := parsePinnedImageCacheManifest("cpu", []byte(payload)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestPinnedImageCacheRepositoryStripsTagAndDigest(t *testing.T) {
	cases := map[string]string{
		"registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.19.1@sha256:85108987d044b18a098126732f98602df408888c0f7d456241f5abefb9744bc1": "registry.k8s.io/kube-state-metrics/kube-state-metrics",
		"docker.io/library/busybox:1.37.0@sha256:" + strings.Repeat("a", 64):                                                                    "docker.io/library/busybox",
		"ghcr.io/nunocgoncalves/iterabase-third-party/minio":                                                                                    "ghcr.io/nunocgoncalves/iterabase-third-party/minio",
		"ghcr.io/nunocgoncalves/iterabase-third-party/minio:RELEASE.2025-09-07T16-13-09Z":                                                       "ghcr.io/nunocgoncalves/iterabase-third-party/minio",
		"busybox:1.37.0@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0":                                                "busybox",
	}
	for reference, expected := range cases {
		if got := pinnedImageCacheRepository(reference); got != expected {
			t.Errorf("repository(%q) = %q, want %q", reference, got, expected)
		}
	}
}

func TestPinnedImageCacheEvidenceCommandsRetainTheClassificationInputs(t *testing.T) {
	archive := "/var/lib/iterabase-e2e/image-cache/cpu/" + strings.Repeat("a", 64) +
		"/images/registry.k8s.io_kube-state-metrics_kube-state-metrics-85108987d044.tar"
	archiveCommand := pinnedImageCacheArchiveEvidenceCommand(archive)
	for _, fragment := range []string{"ls -l", "sha256sum", archive} {
		if !strings.Contains(archiveCommand, fragment) {
			t.Errorf("archive evidence %q does not contain %q", archiveCommand, fragment)
		}
	}

	capacityCommand := pinnedImageCacheCapacityEvidenceCommand("/var/lib/iterabase-e2e/image-cache")
	for _, fragment := range []string{"df -h", "/var/lib/iterabase-e2e/image-cache", pinnedImageCacheContainerdRoot} {
		if !strings.Contains(capacityCommand, fragment) {
			t.Errorf("capacity evidence %q does not contain %q", capacityCommand, fragment)
		}
	}

	reference := "registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.19.1@sha256:85108987d044b18a098126732f98602df408888c0f7d456241f5abefb9744bc1"
	runtimeCommand := pinnedImageCacheRuntimeEvidenceCommand(reference)
	for _, fragment := range []string{"crictl images", "registry.k8s.io/kube-state-metrics/kube-state-metrics"} {
		if !strings.Contains(runtimeCommand, fragment) {
			t.Errorf("runtime evidence %q does not contain %q", runtimeCommand, fragment)
		}
	}
}

func TestPinnedImageCacheImportEvidenceIsRetainedAndRedacted(t *testing.T) {
	diagnostics := forgeDiagnostics{outputDir: t.TempDir(), redactor: redact.New("import-secret")}
	diagnostics.recordRemoteLog(t, "pinned-image-cache-import",
		"ctr: content digest mismatch\ntoken=import-secret")

	contents, err := os.ReadFile(filepath.Join(diagnostics.outputDir, "remote-pinned-image-cache-import.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "import-secret") {
		t.Fatalf("import evidence retained a registered secret: %q", contents)
	}
	if !strings.Contains(string(contents), "ctr: content digest mismatch") {
		t.Fatalf("import evidence dropped the import output: %q", contents)
	}
}
