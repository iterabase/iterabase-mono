package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	runtimeImageReadyTimeout = 90 * time.Second
	runtimeImagePollInterval = time.Second
)

type runtimeImageConfig struct {
	repoTag string
	labels  map[string]string
}

// runtimeImageReference mirrors the Docker default registry/library naming used
// by K3s CRI. A digest-qualified reference identifies the digest, not its tag.
func runtimeImageReference(reference string) string {
	name, digest, hasDigest := strings.Cut(reference, "@")
	if hasDigest {
		if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
			name = name[:colon]
		}
	}
	first, rest, slash := strings.Cut(name, "/")
	if first == "index.docker.io" {
		first, name = "docker.io", "docker.io/"+rest
	}
	switch {
	case !slash:
		name = "docker.io/library/" + name
	case first == "docker.io" && !strings.Contains(rest, "/"):
		name = "docker.io/library/" + rest
	case !strings.ContainsAny(first, ".:") && first != "localhost":
		name = "docker.io/" + name
	}
	if hasDigest {
		return name + "@" + digest
	}
	return name
}

// runtimeImageVisible distinguishes an explicit successful CRI empty/missing
// listing from a failed observation. No CLI error text or exit code is treated
// as "not found". Visible mismatched/ambiguous identities fail immediately.
func runtimeImageVisible(data []byte, reference, expectedConfigDigest string) (bool, error) {
	var response struct {
		Images json.RawMessage `json:"images"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return false, fmt.Errorf("decode CRI image listing: %w", err)
	}
	if len(response.Images) == 0 {
		return false, fmt.Errorf("CRI image listing has no images field")
	}
	var images []struct {
		ID          string   `json:"id"`
		RepoTags    []string `json:"repoTags"`
		RepoDigests []string `json:"repoDigests"`
	}
	if err := json.Unmarshal(response.Images, &images); err != nil {
		return false, fmt.Errorf("decode CRI listed images: %w", err)
	}
	wanted := runtimeImageReference(reference)
	found := false
	for _, image := range images {
		if !isCanonicalSHA256Digest(image.ID) {
			return false, fmt.Errorf("CRI listed image has invalid config digest %q", image.ID)
		}
		if !slices.Contains(image.RepoTags, wanted) && !slices.Contains(image.RepoDigests, wanted) {
			continue
		}
		if found {
			return false, fmt.Errorf("CRI image reference %q is ambiguous", reference)
		}
		if image.ID != expectedConfigDigest {
			return false, fmt.Errorf("CRI config digest %q != %q for %s", image.ID, expectedConfigDigest, reference)
		}
		found = true
	}
	return found, nil
}

func observeRuntimeImage(ctx context.Context, run runtimeImageCommand, reference, configDigest string) (runtimeImageConfig, bool, error) {
	var image runtimeImageConfig
	if err := ctx.Err(); err != nil {
		return image, false, err
	}
	if reference == "" || !isCanonicalSHA256Digest(configDigest) {
		return image, false, fmt.Errorf("incomplete runtime image reference/config identity")
	}
	listing, err := run(ctx, "sudo k3s crictl images --output json")
	if err != nil {
		return image, false, fmt.Errorf("list CRI images: %w", err)
	}
	visible, err := runtimeImageVisible([]byte(listing), reference, configDigest)
	if err != nil || !visible {
		return image, false, err
	}
	inspection, err := run(ctx, "sudo k3s crictl inspecti "+candidateShellQuote(reference))
	if err != nil {
		return image, false, fmt.Errorf("inspect visible CRI image %s: %w", reference, err)
	}
	image.repoTag, image.labels, err = importedRuntimeImageConfig([]byte(inspection), configDigest)
	if err != nil {
		return image, false, err
	}
	if !strings.Contains(reference, "@") && image.repoTag != runtimeImageReference(reference) {
		return image, false, fmt.Errorf("CRI runtime tag %q != requested %q", image.repoTag, reference)
	}
	if err := ctx.Err(); err != nil {
		return image, false, err
	}
	return image, true, nil
}

// importRuntimeImage imports exactly once, then observes CRI's asynchronous
// cache with a separate finite readiness deadline. Only a successful listing
// without the reference may wait; every observation/identity error is terminal.
func importRuntimeImage(ctx context.Context, run runtimeImageCommand, command, reference, configDigest string, timeout, interval time.Duration) (runtimeImageConfig, string, error) {
	var image runtimeImageConfig
	if err := ctx.Err(); err != nil {
		return image, "", err
	}
	importCtx, cancelImport := context.WithTimeout(ctx, structuredSSHTimeout)
	output, err := run(importCtx, command)
	if err == nil {
		err = importCtx.Err()
	}
	cancelImport()
	if err != nil {
		return image, output, fmt.Errorf("import %s: %w", reference, err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		observed, ready, err := observeRuntimeImage(readyCtx, run, reference, configDigest)
		if err != nil {
			return image, output, fmt.Errorf("observe imported image %s: %w", reference, err)
		}
		if ready {
			return observed, output, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-readyCtx.Done():
			timer.Stop()
			return image, output, fmt.Errorf("wait for imported image %s CRI visibility: %w", reference, readyCtx.Err())
		case <-timer.C:
		}
	}
}
