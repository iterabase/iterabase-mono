package testutil

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// MinIOCredential is one zone credential.
type MinIOCredential struct {
	AccessKey string
	SecretKey string
}

// MinIOConfig is a ready integration-test bucket. Root is the administrative
// credential; Ingest, Serve, and Lifecycle are provisioned with the exact
// zone policies the chart ships (charts/charts/minio/files/policies), so
// tests exercise MinIO's own enforcement of the credential split.
type MinIOConfig struct {
	Endpoint  string
	Bucket    string
	Root      MinIOCredential
	Ingest    MinIOCredential
	Serve     MinIOCredential
	Lifecycle MinIOCredential
}

const minioImage = "ghcr.io/iterabase/iterabase-third-party/minio:RELEASE.2025-09-07T16-13-09Z@sha256:786c852164a4fab14fd194fdbe6b4ed6f34934fcf6e4556aa9368432081719e9"

func NewMinIO(t *testing.T) MinIOConfig {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping MinIO integration test in short mode")
	}
	ctx := context.Background()
	root := MinIOCredential{AccessKey: "artifact-test-root", SecretKey: "artifact-test-root-secret"}
	container, err := testcontainers.Run(ctx, minioImage,
		testcontainers.WithExposedPorts("9000/tcp"),
		testcontainers.WithEnv(map[string]string{"MINIO_ROOT_USER": root.AccessKey, "MINIO_ROOT_PASSWORD": root.SecretKey}),
		testcontainers.WithCmd("server", "/data"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/minio/health/ready").WithPort("9000/tcp")),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })
	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "9000/tcp")
	require.NoError(t, err)
	cfg := MinIOConfig{Endpoint: fmt.Sprintf("%s:%s", host, port.Port()), Bucket: "iterabase-artifacts", Root: root,
		Ingest:    MinIOCredential{AccessKey: "artifact-ingest", SecretKey: "artifact-ingest-secret"},
		Serve:     MinIOCredential{AccessKey: "artifact-serve", SecretKey: "artifact-serve-secret"},
		Lifecycle: MinIOCredential{AccessKey: "artifact-lifecycle", SecretKey: "artifact-lifecycle-secret"},
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(root.AccessKey, root.SecretKey, "")})
	require.NoError(t, err)
	require.NoError(t, client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{}))

	mcExec(t, container, "mc", "alias", "set", "local", "http://127.0.0.1:9000", root.AccessKey, root.SecretKey)
	for name, cred := range map[string]MinIOCredential{
		"artifact-ingest": cfg.Ingest, "artifact-serve": cfg.Serve, "artifact-lifecycle": cfg.Lifecycle,
	} {
		policy := strings.ReplaceAll(readChartPolicy(t, name), "__BUCKET__", cfg.Bucket)
		path := "/tmp/" + name + ".json"
		require.NoError(t, container.CopyToContainer(ctx, []byte(policy), path, 0o644))
		mcExec(t, container, "mc", "admin", "user", "add", "local", cred.AccessKey, cred.SecretKey)
		mcExec(t, container, "mc", "admin", "policy", "create", "local", name, path)
		mcExec(t, container, "mc", "admin", "policy", "attach", "local", name, "--user", cred.AccessKey)
	}
	// MinIO applies new users and policies asynchronously; wait until every
	// zone credential is effective so tests never race IAM propagation.
	for _, cred := range []MinIOCredential{cfg.Ingest, cfg.Serve, cfg.Lifecycle} {
		zone, err := minio.New(cfg.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(cred.AccessKey, cred.SecretKey, "")})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			_, err := zone.GetBucketLocation(ctx, cfg.Bucket)
			return err == nil
		}, 30*time.Second, 200*time.Millisecond, "zone credential %s never became effective", cred.AccessKey)
	}
	lifecycle, err := minio.New(cfg.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(cfg.Lifecycle.AccessKey, cfg.Lifecycle.SecretKey, "")})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := lifecycle.PutObject(ctx, cfg.Bucket, "quarantine/.iam-probe", strings.NewReader("x"), 1, minio.PutObjectOptions{})
		if err != nil {
			return false
		}
		_, err = lifecycle.StatObject(ctx, cfg.Bucket, "quarantine/.iam-probe", minio.StatObjectOptions{})
		return err == nil && lifecycle.RemoveObject(ctx, cfg.Bucket, "quarantine/.iam-probe", minio.RemoveObjectOptions{}) == nil
	}, 30*time.Second, 200*time.Millisecond, "lifecycle policy never became effective")
	return cfg
}

func mcExec(t *testing.T, c testcontainers.Container, cmd ...string) {
	t.Helper()
	code, out, err := c.Exec(context.Background(), cmd)
	require.NoError(t, err)
	if code != 0 {
		b, _ := io.ReadAll(out)
		t.Fatalf("%v exited %d: %s", cmd, code, b)
	}
}

// readChartPolicy loads the shipped policy so the test and the chart can
// never drift.
func readChartPolicy(t *testing.T, name string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "charts", "charts", "minio", "files", "policies", name+".json")
	b, err := os.ReadFile(path)
	require.NoError(t, err, "chart policy %s", name)
	return string(b)
}
