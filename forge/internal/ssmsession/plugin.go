// Package ssmsession carries Forge's SSH connection to a hosted instance over
// an AWS Systems Manager AWS-StartSSHSession stream (DES-HOR-593-01). Forge
// starts the session with the AWS SDK and hands it to AWS's session-manager-
// plugin, which it downloads itself at a pinned, checksum-verified version. The
// plugin's stdio is the SSH connection; host-key verification stays in Forge's
// SSH client (DES-HOR-521-01).
package ssmsession

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// PluginVersion is the reviewed session-manager-plugin release.
const PluginVersion = "1.2.835.0"

// pluginArtifact is one reviewed download: the archive checksum and the path
// of the plugin binary inside it.
type pluginArtifact struct {
	url    string
	sha256 string
	member string
	deb    bool
}

const pluginBase = "https://s3.amazonaws.com/session-manager-downloads/plugin/" + PluginVersion + "/"

// pluginArtifacts are keyed by GOOS/GOARCH. Checksums were recorded from the
// published archives when the version was reviewed; AWS publishes none.
var pluginArtifacts = map[string]pluginArtifact{
	"darwin/arm64": {url: pluginBase + "mac_arm64/sessionmanager-bundle.zip", sha256: "7287f20d41eef91643b72dcf28b5f135de49090dd297216a408dc18729e70272", member: "sessionmanager-bundle/bin/session-manager-plugin"},
	"darwin/amd64": {url: pluginBase + "mac/sessionmanager-bundle.zip", sha256: "03be79726bbf30ad2a26177d6531636ab6a42f574e0e3326dfb146585cedf260", member: "sessionmanager-bundle/bin/session-manager-plugin"},
	"linux/amd64":  {url: pluginBase + "ubuntu_64bit/session-manager-plugin.deb", sha256: "7c6dcad12518571cc7959a713e6a8ae1bdf6ed66fd9bee37dc189e39ca58ae03", member: "usr/local/sessionmanagerplugin/bin/session-manager-plugin", deb: true},
	"linux/arm64":  {url: pluginBase + "ubuntu_arm64/session-manager-plugin.deb", sha256: "0add94c4c8b6ca63f26e44fd655d662b0f6455a268b5b9ebebee0f462214e928", member: "usr/local/sessionmanagerplugin/bin/session-manager-plugin", deb: true},
}

// maxPluginArchive bounds the download; the reviewed archives are ~4 MB.
const maxPluginArchive = 64 << 20

// EnsurePlugin returns the path of the reviewed plugin under binDir,
// downloading and verifying it on first use. A cached binary is reused only
// from the version-specific directory Forge itself wrote.
func EnsurePlugin(ctx context.Context, binDir string) (string, error) {
	art, ok := pluginArtifacts[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("session-manager-plugin %s has no reviewed build for %s/%s", PluginVersion, runtime.GOOS, runtime.GOARCH)
	}
	dir := filepath.Join(binDir, "session-manager-plugin-"+PluginVersion)
	path := filepath.Join(dir, "session-manager-plugin")
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return path, nil
	}
	archive, err := download(ctx, art.url)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != art.sha256 {
		return "", fmt.Errorf("session-manager-plugin archive %s has sha256 %s, want the reviewed %s", art.url, got, art.sha256)
	}
	var binary []byte
	if art.deb {
		binary, err = debMember(archive, art.member)
	} else {
		binary, err = zipMember(archive, art.member)
	}
	if err != nil {
		return "", fmt.Errorf("session-manager-plugin archive: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".plugin-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(binary); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o700); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPluginArchive+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if len(data) > maxPluginArchive {
		return nil, fmt.Errorf("download %s: larger than %d bytes", url, maxPluginArchive)
	}
	return data, nil
}

func zipMember(archive []byte, member string) ([]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}
	for _, f := range r.File {
		if f.Name != member {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxPluginArchive))
	}
	return nil, fmt.Errorf("%s not found", member)
}

// debMember reads member from the data.tar.gz of a Debian package (an ar
// archive of debian-binary, control.tar.* and data.tar.*).
func debMember(archive []byte, member string) ([]byte, error) {
	const magic = "!<arch>\n"
	if !bytes.HasPrefix(archive, []byte(magic)) {
		return nil, errors.New("not a Debian package")
	}
	for i := len(magic); i+60 <= len(archive); {
		header := archive[i : i+60]
		name := strings.TrimSuffix(strings.TrimSpace(string(header[:16])), "/")
		size, err := strconv.Atoi(strings.TrimSpace(string(header[48:58])))
		if err != nil || size < 0 || i+60+size > len(archive) {
			return nil, errors.New("malformed Debian package")
		}
		body := archive[i+60 : i+60+size]
		i += 60 + size + size%2
		if name != "data.tar.gz" {
			continue
		}
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if strings.TrimPrefix(hdr.Name, "./") == member && hdr.Typeflag == tar.TypeReg {
				return io.ReadAll(io.LimitReader(tr, maxPluginArchive))
			}
		}
		return nil, fmt.Errorf("%s not found in data.tar.gz", member)
	}
	return nil, errors.New("data.tar.gz not found in Debian package")
}
