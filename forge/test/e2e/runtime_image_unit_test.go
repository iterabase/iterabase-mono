package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func runtimeImageTestPayloads() (reference, digest, listing, inspection string) {
	// Synthetic identities only; these tests never resolve or import a registry image.
	reference = "iterabase-e2e/model:exact-head"
	digest = "sha256:" + strings.Repeat("a", 64)
	listing = fmt.Sprintf(`{"images":[{"id":%q,"repoTags":["docker.io/iterabase-e2e/model:exact-head"]}]}`, digest)
	inspection = fmt.Sprintf(`{"status":{"id":%q,"repoTags":["docker.io/iterabase-e2e/model:exact-head"]},"info":{"imageSpec":{"config":{"Labels":{"org.opencontainers.image.revision":"exact-source"}}}}}`, digest)
	return
}

func TestRuntimeImageReferenceUsesCRIRegistryNames(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	library, local := "docker.io/library/", "localhost:5000/"
	for input, wanted := range map[string]string{
		"fixture:1":                                library + "fixture:1",
		"docker.io/" + "fixture:1":                 library + "fixture:1",
		"index.docker.io/" + "fixture:1":           library + "fixture:1",
		"iterabase-e2e/model:exact-head":           "docker.io/iterabase-e2e/model:exact-head",
		"docker.io/iterabase-e2e/model:exact-head": "docker.io/iterabase-e2e/model:exact-head",
		"ghcr.io/iterabase/fixture:1":              "ghcr.io/iterabase/fixture:1",
		local + "fixture:1":                        local + "fixture:1",
		"fixture:1@" + digest:                      library + "fixture@" + digest,
	} {
		if actual := runtimeImageReference(input); actual != wanted {
			t.Errorf("reference(%q)=%q want=%q", input, actual, wanted)
		}
	}
}

func TestRuntimeImageImportWaitsForVisibilityWithoutReimporting(t *testing.T) {
	reference, digest, listing, inspection := runtimeImageTestPayloads()
	imports, lists, inspections := 0, 0, 0
	run := func(_ context.Context, command string) (string, error) {
		switch {
		case command == "import exactly once":
			imports++
			return "import succeeded", nil
		case command == "sudo k3s crictl images --output json":
			lists++
			if lists < 3 {
				return `{"images":[]}`, nil
			}
			return listing, nil
		case strings.HasPrefix(command, "sudo k3s crictl inspecti "):
			inspections++
			return inspection, nil
		default:
			t.Fatalf("unexpected command %q", command)
			return "", nil
		}
	}
	image, output, err := importRuntimeImage(t.Context(), run, "import exactly once", reference, digest, time.Second, 0)
	if err != nil || imports != 1 || lists != 3 || inspections != 1 || output != "import succeeded" {
		t.Fatalf("imports=%d lists=%d inspections=%d output=%q err=%v", imports, lists, inspections, output, err)
	}
	if image.repoTag != runtimeImageReference(reference) || image.labels["org.opencontainers.image.revision"] != "exact-source" {
		t.Fatalf("lost exact reference/source identity: %+v", image)
	}
}

func TestRuntimeImageImportObservationErrorsAreImmediate(t *testing.T) {
	reference, digest, listing, inspection := runtimeImageTestPayloads()
	failure := errors.New("observation failed")
	wrong := "sha256:" + strings.Repeat("b", 64)
	for name, tc := range map[string]struct {
		listing, inspection            string
		importErr, listErr, inspectErr error
	}{
		"import failure":       {importErr: failure},
		"list RPC error":       {listErr: failure},
		"malformed listing":    {listing: "sudo warning\n" + listing},
		"missing images field": {listing: `{}`},
		"invalid listing type": {listing: `{"images":{}}`},
		"invalid config ID":    {listing: `{"images":[{"id":"garbage"}]}`},
		"wrong config ID":      {listing: strings.ReplaceAll(listing, digest, wrong)},
		"ambiguous reference":  {listing: fmt.Sprintf(`{"images":[{"id":%q,"repoTags":[%q]},{"id":%q,"repoTags":[%q]}]}`, digest, runtimeImageReference(reference), digest, runtimeImageReference(reference))},
		"inspect RPC error":    {listing: listing, inspectErr: failure},
		"malformed inspection": {listing: listing, inspection: "stderr warning\n" + inspection},
		"wrong inspected ID":   {listing: listing, inspection: strings.ReplaceAll(inspection, digest, wrong)},
		"ambiguous tags":       {listing: listing, inspection: fmt.Sprintf(`{"status":{"id":%q,"repoTags":["one","two"]}}`, digest)},
		"wrong inspected tag":  {listing: listing, inspection: strings.ReplaceAll(inspection, runtimeImageReference(reference), "docker.io/iterabase-e2e/unrelated:exact-head")},
	} {
		t.Run(name, func(t *testing.T) {
			imports, lists, inspections := 0, 0, 0
			run := func(_ context.Context, command string) (string, error) {
				if command == "import" {
					imports++
					return "import output", tc.importErr
				}
				if command == "sudo k3s crictl images --output json" {
					lists++
					return tc.listing, tc.listErr
				}
				inspections++
				return tc.inspection, tc.inspectErr
			}
			_, _, err := importRuntimeImage(t.Context(), run, "import", reference, digest, time.Second, 0)
			if err == nil || imports != 1 || lists > 1 || inspections > 1 {
				t.Fatalf("unexpected retry/success: imports=%d lists=%d inspections=%d err=%v", imports, lists, inspections, err)
			}
			if (tc.importErr != nil || tc.listErr != nil || tc.inspectErr != nil) && !errors.Is(err, failure) {
				t.Fatalf("lost original error: %v", err)
			}
		})
	}
}

func TestRuntimeImageImportNeverVisibleFailsDeadline(t *testing.T) {
	reference, digest, _, _ := runtimeImageTestPayloads()
	imports, lists := 0, 0
	run := func(_ context.Context, command string) (string, error) {
		if command == "import" {
			imports++
			return "", nil
		}
		if command != "sudo k3s crictl images --output json" {
			t.Fatalf("unexpected inspection or re-import: %s", command)
		}
		lists++
		return `{"images":[]}`, nil
	}
	_, _, err := importRuntimeImage(t.Context(), run, "import", reference, digest, 20*time.Millisecond, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || imports != 1 || lists == 0 {
		t.Fatalf("imports=%d lists=%d err=%v", imports, lists, err)
	}
}

func TestRuntimeImageImportCancellationDoesNotRetry(t *testing.T) {
	reference, digest, _, _ := runtimeImageTestPayloads()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	imports, lists := 0, 0
	run := func(_ context.Context, command string) (string, error) {
		if command == "import" {
			imports++
			return "", nil
		}
		lists++
		cancel()
		return `{"images":[]}`, nil
	}
	_, _, err := importRuntimeImage(ctx, run, "import", reference, digest, time.Second, time.Second)
	if !errors.Is(err, context.Canceled) || imports != 1 || lists != 1 {
		t.Fatalf("imports=%d lists=%d err=%v", imports, lists, err)
	}
}

func TestRuntimeImageImportBoundsHungObservation(t *testing.T) {
	reference, digest, _, _ := runtimeImageTestPayloads()
	run := func(ctx context.Context, command string) (string, error) {
		if command == "import" {
			return "", nil
		}
		if _, bounded := ctx.Deadline(); !bounded {
			t.Fatal("image observation has no deadline")
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	_, _, err := importRuntimeImage(t.Context(), run, "import", reference, digest, 20*time.Millisecond, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung observation returned %v", err)
	}
}

func TestRuntimeImageUnrelatedOrDigestOnlyEntries(t *testing.T) {
	reference, digest, listing, _ := runtimeImageTestPayloads()
	visible, err := runtimeImageVisible([]byte(strings.ReplaceAll(listing, runtimeImageReference(reference), "docker.io/iterabase-e2e/unrelated:exact-head")), reference, digest)
	if err != nil || visible {
		t.Fatalf("unrelated image satisfied request: visible=%t err=%v", visible, err)
	}
	manifest := "sha256:" + strings.Repeat("c", 64)
	visible, err = runtimeImageVisible([]byte(fmt.Sprintf(`{"images":[{"id":%q,"repoDigests":["docker.io/iterabase-e2e/model@%s"]}]}`, digest, manifest)), reference+"@"+manifest, digest)
	if err != nil || !visible {
		t.Fatalf("digest-qualified image not resolved: visible=%t err=%v", visible, err)
	}
}
