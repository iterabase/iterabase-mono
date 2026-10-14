package admission

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/admission/admissiontest"
)

// TestCorpus pins the shared admissiontest corpus, including its literal
// MIME types and reason codes, to this package's policy.
func TestCorpus(t *testing.T) {
	for _, tc := range admissiontest.Corpus() {
		t.Run(tc.Name, func(t *testing.T) {
			depth := tc.Depth
			if depth == 0 {
				depth = 1
			}
			got := Evaluate(Input{Data: tc.Data, Filename: tc.Filename, ClaimedMIME: tc.ClaimedMIME, ContainerDepth: depth})
			assert.Equal(t, tc.Accepted, got.Accepted, "reason %q", got.Reason)
			assert.Equal(t, tc.CanonicalMIME, got.CanonicalMIME)
			assert.Equal(t, tc.Reason, got.Reason)
			assert.Len(t, got.Children, tc.Children)
			for _, child := range got.Children {
				childResult := Evaluate(Input{Data: child.Data, Filename: child.Filename, ClaimedMIME: child.ClaimedMIME, ContainerDepth: depth + 1})
				assert.True(t, childResult.Accepted, "child %s: %s", child.Filename, childResult.Reason)
			}
		})
	}
}
