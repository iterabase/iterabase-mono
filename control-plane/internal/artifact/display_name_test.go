package artifact_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	artifactstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact"
	"github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact/admission/admissiontest"
)

// TestDisplayNameIsNormalizedAtTheMetadataBoundary proves section 6 for
// persisted metadata: only the normalized name reaches status, search, and
// download metadata, and unsafe names are refused before any byte is stored.
func TestDisplayNameIsNormalizedAtTheMetadataBoundary(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for claimed, want := range map[string]string{
		`C:\customer\secret\report.txt`: "report.txt",
		"../../etc/notes.txt":           "notes.txt",
		"quote\x01\x7f.txt":             "quote.txt",
		"  spaced name.txt .":           "spaced name.txt",
	} {
		a := h.admit(claimed, "text/plain", admissiontest.TXT())
		require.NotNil(t, a.DisplayName, claimed)
		assert.Equal(t, want, *a.DisplayName, "%q is stored normalized", claimed)
	}
	for _, unsafe := range []string{"invoice\u202etxt.exe", "invoice.pdf.exe", "report.exe.txt", `C:\dir\`} {
		_, err := h.svc.Admit(ctx, artifactstore.AdmitInput{SourceType: artifactstore.SourceUpload, DisplayName: unsafe,
			ClaimedMIME: "text/plain", ContainerDepth: 1,
			Attribution: artifactstore.Attribution{RequestActorID: h.actorID, Source: artifactstore.AuthBrowser}},
			bytes.NewReader(admissiontest.TXT()))
		assert.ErrorIs(t, err, artifactstore.ErrInvalidInput, "%q is refused", unsafe)
	}
	var stored int
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM artifact.artifacts
		WHERE display_name ~ '[\\/\x01-\x1f\u202a-\u202e\u2066-\u2069]' OR display_name ~* '\.exe'`).Scan(&stored))
	assert.Zero(t, stored, "no raw path, control, bidi, or executable name is persisted")

	// Container children are normalized at the same boundary even when a
	// scanner response carries an unsafe name.
	parent := h.admit("parent.txt", "text/plain", admissiontest.TXT())
	childID, _, err := h.store.ReserveChild(ctx, parent, 3, artifactstore.ChildAttachment, "x\u202ey.exe", "text/plain")
	require.NoError(t, err)
	var child string
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT display_name FROM artifact.artifacts WHERE id=$1`, childID).Scan(&child))
	assert.Equal(t, "attachment-3", child)
	childID, _, err = h.store.ReserveChild(ctx, parent, 4, artifactstore.ChildAttachment, `inner\dir\scan.pdf`, "application/pdf")
	require.NoError(t, err)
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT display_name FROM artifact.artifacts WHERE id=$1`, childID).Scan(&child))
	assert.Equal(t, "scan.pdf", child)
}
