package artifact

import (
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"
)

// Every V2 artifact fits in one part, so minio-go sends a single conditional
// PUT (no multipart upload) and one in-flight upload buffers a bounded amount
// (HOR-602).
func TestPutOptionsSendOneBoundedCreateOnlyPart(t *testing.T) {
	opts := putOptions("application/pdf")
	require.Equal(t, "application/pdf", opts.ContentType)
	require.Greater(t, int64(opts.PartSize), MaxSize+1)

	_, defaultPartSize, _, err := minio.OptimalPartInfo(-1, 0)
	require.NoError(t, err)
	require.Greater(t, defaultPartSize, int64(512<<20), "regression guard: minio-go default still oversized")
	require.LessOrEqual(t, opts.PartSize, uint64(64<<20))
	require.Equal(t, "*", opts.Header().Get("If-None-Match"), "writes are create-only")
}
