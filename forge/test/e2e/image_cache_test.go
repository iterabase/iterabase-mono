package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	pinnedImageCacheRootEnv       = "FORGE_E2E_IMAGE_CACHE_ROOT"
	pinnedImageCacheGenerationEnv = "FORGE_E2E_IMAGE_CACHE_GENERATION"
	// pinnedImageCacheContainerdRoot is k3s's containerd state root, the
	// filesystem behind kubelet's image filesystem capacity.
	pinnedImageCacheContainerdRoot = "/var/lib/rancher/k3s/agent/containerd"
)

var pinnedImageCacheGenerationPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var pinnedImageCacheDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type pinnedImageCacheImage struct {
	Reference    string `json:"reference"`
	Digest       string `json:"digest"`
	ConfigDigest string `json:"config_digest"`
	Archive      string `json:"archive"`
}

type pinnedImageCacheManifest struct {
	SchemaVersion int                     `json:"schema_version"`
	Capacity      string                  `json:"capacity"`
	Generation    string                  `json:"generation"`
	CacheRoot     string                  `json:"cache_root"`
	Images        []pinnedImageCacheImage `json:"images"`
}

func parsePinnedImageCacheManifest(capacity string, data []byte) (pinnedImageCacheManifest, error) {
	var manifest pinnedImageCacheManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, fmt.Errorf("decode pinned image cache manifest: %w", err)
	}
	if manifest.SchemaVersion != 1 {
		return manifest, fmt.Errorf("unsupported pinned image cache schema %d", manifest.SchemaVersion)
	}
	if manifest.Capacity != capacity {
		return manifest, fmt.Errorf("pinned image cache capacity %q does not match %q", manifest.Capacity, capacity)
	}
	if !pinnedImageCacheGenerationPattern.MatchString(manifest.Generation) {
		return manifest, fmt.Errorf("invalid pinned image cache generation %q", manifest.Generation)
	}
	if len(manifest.Images) == 0 {
		return manifest, fmt.Errorf("pinned image cache manifest has no images")
	}
	archives := make(map[string]bool, len(manifest.Images))
	for _, image := range manifest.Images {
		if image.Reference == "" || !pinnedImageCacheDigestPattern.MatchString(image.Digest) {
			return manifest, fmt.Errorf("pinned image %q has an incomplete digest identity", image.Reference)
		}
		if !pinnedImageCacheDigestPattern.MatchString(image.ConfigDigest) {
			return manifest, fmt.Errorf("pinned image %q has an incomplete config digest", image.Reference)
		}
		if image.Archive == "" || path.Base(image.Archive) != image.Archive || !strings.HasSuffix(image.Archive, ".tar") {
			return manifest, fmt.Errorf("pinned image %q has an unsafe archive name %q", image.Reference, image.Archive)
		}
		if archives[image.Archive] {
			return manifest, fmt.Errorf("pinned image archive %q is duplicated", image.Archive)
		}
		archives[image.Archive] = true
	}
	return manifest, nil
}

// preparePinnedImageCache imports the seeded pinned-image generation on the
// fixture and proves every image is present under its exact reference before any
// Helm apply runs, so the applies never depend on a public registry. Missing or
// mismatched cache state fails the scenario instead of silently falling back.
// An import or post-import verification failure additionally retains bounded
// classification evidence through diagnostics.
func preparePinnedImageCache(t *testing.T, diagnostics *forgeDiagnostics, ip, keyPath, capacity string) {
	t.Helper()
	root := os.Getenv(pinnedImageCacheRootEnv)
	generation := os.Getenv(pinnedImageCacheGenerationEnv)
	if root == "" || generation == "" {
		t.Fatalf("pinned image cache identity is not configured (%s, %s); bake the fixture AMI with bake.yml",
			pinnedImageCacheRootEnv, pinnedImageCacheGenerationEnv)
	}
	if !pinnedImageCacheGenerationPattern.MatchString(generation) {
		t.Fatalf("invalid pinned image cache generation %q", generation)
	}
	client, err := sshDial(ip, keyPath)
	if err != nil {
		t.Fatalf("ssh dial %s for pinned image cache: %v", ip, err)
	}
	defer client.Close()

	manifestPath := path.Join(root, capacity, generation, "generation.json")
	output, err := sshOutput(client, "sudo cat "+candidateShellQuote(manifestPath))
	if err != nil {
		t.Fatalf("read pinned image cache manifest %s: %v\n%s; bake the fixture AMI with bake.yml", manifestPath, err, output)
	}
	manifest, err := parsePinnedImageCacheManifest(capacity, []byte(output))
	if err != nil {
		t.Fatalf("pinned image cache manifest %s: %v", manifestPath, err)
	}
	if manifest.Generation != generation || manifest.CacheRoot != root {
		t.Fatalf("pinned image cache identity mismatch: host generation=%s root=%s, expected generation=%s root=%s",
			manifest.Generation, manifest.CacheRoot, generation, root)
	}

	imported := 0
	for _, image := range manifest.Images {
		verified := false
		if output, err := sshOutput(client, "sudo k3s crictl inspecti "+candidateShellQuote(image.Reference)); err == nil {
			_, _, configErr := importedRuntimeImageConfig([]byte(output), image.ConfigDigest)
			verified = configErr == nil
		}
		if !verified {
			archive := path.Join(root, capacity, generation, "images", image.Archive)
			started := time.Now()
			importOutput, err := sshOutput(client, "sudo k3s ctr images import "+candidateShellQuote(archive))
			t.Logf("pinned image %s import took %s", image.Reference, time.Since(started).Round(time.Second))
			if err != nil {
				diagnostics.collectPinnedImageCacheEvidence(t, ip, keyPath, archive, root, image.Reference, importOutput)
				t.Fatalf("import pinned image %s from %s: %v\n%s", image.Reference, archive, err, importOutput)
			}
			inspectOutput, err := sshOutput(client, "sudo k3s crictl inspecti "+candidateShellQuote(image.Reference))
			if err != nil {
				diagnostics.collectPinnedImageCacheEvidence(t, ip, keyPath, archive, root, image.Reference, importOutput)
				t.Fatalf("pinned image %s is absent after import from %s: %v\n%s", image.Reference, archive, err, inspectOutput)
			}
			if _, _, err := importedRuntimeImageConfig([]byte(inspectOutput), image.ConfigDigest); err != nil {
				diagnostics.collectPinnedImageCacheEvidence(t, ip, keyPath, archive, root, image.Reference, importOutput)
				t.Fatalf("pinned image %s does not match the cached config digest %s: %v\n%s",
					image.Reference, image.ConfigDigest, err, inspectOutput)
			}
			imported++
		}
	}
	t.Logf("pinned image cache generation %s ready: %d images verified (%d imported this run)", generation, len(manifest.Images), imported)
}

// pinnedImageCacheRepository returns the registry/repository path of a pinned
// image reference so runtime evidence can be filtered to it.
func pinnedImageCacheRepository(reference string) string {
	name, _, _ := strings.Cut(reference, "@")
	slash := strings.LastIndex(name, "/")
	if colon := strings.LastIndex(name, ":"); colon > slash {
		name = name[:colon]
	}
	return name
}

func pinnedImageCacheArchiveEvidenceCommand(archive string) string {
	return fmt.Sprintf("sudo ls -l %s 2>&1 || true; sudo sha256sum %s 2>&1 || true",
		candidateShellQuote(archive), candidateShellQuote(archive))
}

func pinnedImageCacheCapacityEvidenceCommand(root string) string {
	return fmt.Sprintf("df -h %s 2>&1 || true; df -h %s 2>&1 || true",
		candidateShellQuote(root), candidateShellQuote(pinnedImageCacheContainerdRoot))
}

func pinnedImageCacheRuntimeEvidenceCommand(reference string) string {
	return fmt.Sprintf("sudo k3s crictl images 2>&1 | grep -F -- %s || true",
		candidateShellQuote(pinnedImageCacheRepository(reference)))
}

// collectPinnedImageCacheEvidence retains bounded, classifiable evidence for a
// cache import or verification failure: the failed archive's size and sha256,
// cache-root and containerd-root capacity, the runtime's view of the reference,
// and the raw import output. Only one archive is hashed, so the cost is bounded
// regardless of cache size.
func (diagnostics *forgeDiagnostics) collectPinnedImageCacheEvidence(
	t *testing.T, ip, keyPath, archive, root, reference, importOutput string,
) {
	t.Helper()
	diagnostics.collectSSH(t, ip, keyPath, map[string]string{
		"pinned-image-cache-archive":  pinnedImageCacheArchiveEvidenceCommand(archive),
		"pinned-image-cache-capacity": pinnedImageCacheCapacityEvidenceCommand(root),
		"pinned-image-cache-runtime":  pinnedImageCacheRuntimeEvidenceCommand(reference),
	})
	diagnostics.recordRemoteLog(t, "pinned-image-cache-import", importOutput)
}
