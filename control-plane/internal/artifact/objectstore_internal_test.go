package artifact

import (
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"
)

// An unknown-length upload must buffer bounded parts, not minio-go's 5 TiB
// default (~528 MiB), so artifact uploads fit sized containers (HOR-602).
func TestPutOptionsBoundUnknownLengthPartBuffer(t *testing.T) {
	opts := putOptions("application/pdf")
	require.Equal(t, "application/pdf", opts.ContentType)

	_, partSize, _, err := minio.OptimalPartInfo(-1, opts.PartSize)
	require.NoError(t, err)
	require.EqualValues(t, 16<<20, partSize)

	_, defaultPartSize, _, err := minio.OptimalPartInfo(-1, 0)
	require.NoError(t, err)
	require.Greater(t, defaultPartSize, int64(512<<20), "regression guard: minio-go default still oversized")
	require.GreaterOrEqual(t, int64(opts.PartSize)*10000, int64(defaultMaxSizeForTest))
}

const defaultMaxSizeForTest = 1 << 30
