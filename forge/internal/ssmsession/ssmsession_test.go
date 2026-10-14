package ssmsession

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func zipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := w.Create(name)
		require.NoError(t, err)
		_, err = f.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

type arMember struct {
	name string
	body []byte
}

func arArchive(members ...arMember) []byte {
	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	for _, m := range members {
		fmt.Fprintf(&buf, "%-16s%-12s%-6s%-6s%-8s%-10d`\n", m.name, "0", "0", "0", "100644", len(m.body))
		buf.Write(m.body)
		if len(m.body)%2 == 1 {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}

const debMemberPath = "usr/local/sessionmanagerplugin/bin/session-manager-plugin"

func debPackage(t *testing.T, dataName string, files map[string]string) []byte {
	t.Helper()
	return arArchive(
		arMember{name: "debian-binary", body: []byte("2.0\n")},
		arMember{name: "control.tar.gz", body: tarGz(t, map[string]string{"./control": "Package: x\n"})},
		arMember{name: dataName, body: tarGz(t, files)},
	)
}

func TestZipMember(t *testing.T) {
	archive := zipArchive(t, map[string]string{
		"sessionmanager-bundle/bin/session-manager-plugin": "PLUGIN",
		"sessionmanager-bundle/README":                     "readme",
	})
	got, err := zipMember(archive, "sessionmanager-bundle/bin/session-manager-plugin")
	require.NoError(t, err)
	assert.Equal(t, "PLUGIN", string(got))

	_, err = zipMember(archive, "sessionmanager-bundle/bin/other")
	require.ErrorContains(t, err, "sessionmanager-bundle/bin/other not found")

	_, err = zipMember([]byte("not a zip"), "x")
	require.Error(t, err)
}

func TestDebMember(t *testing.T) {
	deb := debPackage(t, "data.tar.gz", map[string]string{
		"./" + debMemberPath:                      "PLUGIN",
		"./usr/local/sessionmanagerplugin/README": "readme",
	})
	got, err := debMember(deb, debMemberPath)
	require.NoError(t, err)
	assert.Equal(t, "PLUGIN", string(got))

	// GNU ar terminates names with '/'.
	gnu := arArchive(arMember{name: "data.tar.gz/", body: tarGz(t, map[string]string{debMemberPath: "GNU"})})
	got, err = debMember(gnu, debMemberPath)
	require.NoError(t, err)
	assert.Equal(t, "GNU", string(got))

	_, err = debMember(deb, "usr/bin/missing")
	require.ErrorContains(t, err, "usr/bin/missing not found in data.tar.gz")

	_, err = debMember(debPackage(t, "data.tar.xz", map[string]string{debMemberPath: "x"}), debMemberPath)
	require.ErrorContains(t, err, "data.tar.gz not found in Debian package")

	_, err = debMember([]byte("PK\x03\x04"), debMemberPath)
	require.ErrorContains(t, err, "not a Debian package")

	// A header whose size field is not a number, or overruns the archive.
	bad := []byte("!<arch>\n" + fmt.Sprintf("%-16s%-12s%-6s%-6s%-8s%-10s`\n", "data.tar.gz", "0", "0", "0", "100644", "huge"))
	_, err = debMember(bad, debMemberPath)
	require.ErrorContains(t, err, "malformed Debian package")
	overrun := []byte("!<arch>\n" + fmt.Sprintf("%-16s%-12s%-6s%-6s%-8s%-10d`\n", "data.tar.gz", "0", "0", "0", "100644", 4096) + "short")
	_, err = debMember(overrun, debMemberPath)
	require.ErrorContains(t, err, "malformed Debian package")

	_, err = debMember(arArchive(arMember{name: "data.tar.gz", body: []byte("not gzip")}), debMemberPath)
	require.Error(t, err)
}

// servePluginArtifact points this platform's reviewed artifact at a local
// server serving body, with the recorded checksum sum, and counts requests.
func servePluginArtifact(t *testing.T, body []byte, sum string) *atomic.Int64 {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	key := runtime.GOOS + "/" + runtime.GOARCH
	orig, ok := pluginArtifacts[key]
	if !ok {
		t.Skipf("no reviewed plugin artifact for %s", key)
	}
	art := orig
	art.url = srv.URL + "/plugin"
	art.sha256 = sum
	pluginArtifacts[key] = art
	t.Cleanup(func() { pluginArtifacts[key] = orig })
	return &hits
}

func pluginArchiveForPlatform(t *testing.T, content string) []byte {
	t.Helper()
	art := pluginArtifacts[runtime.GOOS+"/"+runtime.GOARCH]
	if art.deb {
		return debPackage(t, "data.tar.gz", map[string]string{"./" + art.member: content})
	}
	return zipArchive(t, map[string]string{art.member: content})
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestEnsurePlugin_ChecksumMismatchInstallsNothing(t *testing.T) {
	archive := pluginArchiveForPlatform(t, "#!/bin/sh\n")
	servePluginArtifact(t, archive, strings.Repeat("0", 64))
	binDir := t.TempDir()
	_, err := EnsurePlugin(context.Background(), binDir)
	require.ErrorContains(t, err, "has sha256 "+sha(archive)+", want the reviewed "+strings.Repeat("0", 64))
	_, statErr := os.Stat(filepath.Join(binDir, "session-manager-plugin-"+PluginVersion))
	assert.True(t, os.IsNotExist(statErr), "nothing is written for a substituted archive")
}

func TestEnsurePlugin_InstallsVerifiedMemberOnceAndReusesIt(t *testing.T) {
	archive := pluginArchiveForPlatform(t, "PLUGIN-BYTES")
	hits := servePluginArtifact(t, archive, sha(archive))
	binDir := t.TempDir()
	path, err := EnsurePlugin(context.Background(), binDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(binDir, "session-manager-plugin-"+PluginVersion, "session-manager-plugin"), path)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "PLUGIN-BYTES", string(got))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary files left behind")

	again, err := EnsurePlugin(context.Background(), binDir)
	require.NoError(t, err)
	assert.Equal(t, path, again)
	assert.Equal(t, int64(1), hits.Load(), "a cached binary is reused without downloading")
}

func TestEnsurePlugin_MissingMemberAndHTTPFailure(t *testing.T) {
	art := pluginArtifacts[runtime.GOOS+"/"+runtime.GOARCH]
	var archive []byte
	if art.deb {
		archive = debPackage(t, "data.tar.gz", map[string]string{"./usr/bin/other": "x"})
	} else {
		archive = zipArchive(t, map[string]string{"other": "x"})
	}
	servePluginArtifact(t, archive, sha(archive))
	_, err := EnsurePlugin(context.Background(), t.TempDir())
	require.ErrorContains(t, err, "session-manager-plugin archive:")
	require.ErrorContains(t, err, "not found")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()
	_, err = download(context.Background(), srv.URL)
	require.ErrorContains(t, err, "HTTP 404")
}

type fakeSessionAPI struct {
	mu         sync.Mutex
	start      *ssm.StartSessionInput
	startErr   error
	terminated []string
}

func (f *fakeSessionAPI) StartSession(_ context.Context, in *ssm.StartSessionInput, _ ...func(*ssm.Options)) (*ssm.StartSessionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.start = in
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &ssm.StartSessionOutput{SessionId: aws.String("sess-123"), TokenValue: aws.String("tok"), StreamUrl: aws.String("wss://stream")}, nil
}

func (f *fakeSessionAPI) TerminateSession(_ context.Context, in *ssm.TerminateSessionInput, _ ...func(*ssm.Options)) (*ssm.TerminateSessionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, aws.ToString(in.SessionId))
	return &ssm.TerminateSessionOutput{}, nil
}

func (f *fakeSessionAPI) terminations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.terminated...)
}

// fakePlugin writes its arguments NUL-separated to argsFile, then runs body.
func fakePlugin(t *testing.T, body string) (plugin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	plugin = filepath.Join(dir, "session-manager-plugin")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done > '" + argsFile + "'\n" + body + "\n"
	require.NoError(t, os.WriteFile(plugin, []byte(script), 0o700))
	return plugin, argsFile
}

func TestDial_RoundTripsBytesAndPassesPluginContract(t *testing.T) {
	plugin, argsFile := fakePlugin(t, "exec cat")
	api := &fakeSessionAPI{}
	d := Dialer{API: api, Region: "eu-west-1", Plugin: plugin}
	c, err := d.Dial(context.Background(), "i-0abc")
	require.NoError(t, err)

	assert.Equal(t, "i-0abc", aws.ToString(api.start.Target))
	assert.Equal(t, "AWS-StartSSHSession", aws.ToString(api.start.DocumentName))
	assert.Equal(t, map[string][]string{"portNumber": {"22"}}, api.start.Parameters)

	msg := []byte("SSH-2.0-forge\r\n")
	_, err = c.Write(msg)
	require.NoError(t, err)
	got := make([]byte, len(msg))
	_, err = io.ReadFull(c, got)
	require.NoError(t, err)
	assert.Equal(t, msg, got)
	assert.Equal(t, "i-0abc", c.RemoteAddr().String())
	assert.Equal(t, "ssm", c.RemoteAddr().Network())

	raw, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	args := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	require.Len(t, args, 6)
	var session map[string]string
	require.NoError(t, json.Unmarshal([]byte(args[0]), &session))
	assert.Equal(t, map[string]string{"SessionId": "sess-123", "TokenValue": "tok", "StreamUrl": "wss://stream"}, session)
	assert.Equal(t, "eu-west-1", args[1])
	assert.Equal(t, "StartSession", args[2])
	assert.Equal(t, "", args[3], "no profile: the session token authorizes the stream")
	var request map[string]any
	require.NoError(t, json.Unmarshal([]byte(args[4]), &request))
	assert.Equal(t, map[string]any{
		"Target":       "i-0abc",
		"DocumentName": "AWS-StartSSHSession",
		"Parameters":   map[string]any{"portNumber": []any{"22"}},
	}, request)
	assert.Equal(t, "https://ssm.eu-west-1.amazonaws.com", args[5])

	assert.Empty(t, api.terminations())
	require.NoError(t, c.Close())
	require.NoError(t, c.Close(), "Close is idempotent")
	assert.Equal(t, []string{"sess-123"}, api.terminations(), "the SSM session is terminated exactly once")
}

func TestDial_PluginStderrSurfacesOnEOF(t *testing.T) {
	plugin, _ := fakePlugin(t, "echo 'TargetNotConnected: i-0abc is not connected' >&2\nsleep 0.3\nexit 1")
	api := &fakeSessionAPI{}
	c, err := Dialer{API: api, Region: "eu-west-1", Plugin: plugin}.Dial(context.Background(), "i-0abc")
	require.NoError(t, err)
	defer c.Close()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(c)
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "ssm session to i-0abc ended: TargetNotConnected")
	case <-time.After(5 * time.Second):
		t.Fatal("read did not finish after the plugin exited")
	}
}

func TestDial_Failures(t *testing.T) {
	api := &fakeSessionAPI{startErr: assert.AnError}
	_, err := Dialer{API: api, Region: "eu-west-1", Plugin: "/nonexistent"}.Dial(context.Background(), "i-0abc")
	require.ErrorContains(t, err, "ssm start session to i-0abc")
	assert.Empty(t, api.terminations())

	api = &fakeSessionAPI{}
	_, err = Dialer{API: api, Region: "eu-west-1", Plugin: filepath.Join(t.TempDir(), "missing-plugin")}.Dial(context.Background(), "i-0abc")
	require.ErrorContains(t, err, "start session-manager-plugin")
	assert.Equal(t, []string{"sess-123"}, api.terminations(), "a started session is terminated when the plugin cannot run")
}
