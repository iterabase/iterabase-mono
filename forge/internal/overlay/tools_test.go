package overlay

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type archiveEntry struct {
	name    string
	mode    int64
	content string
	modTime time.Time
	uid     int
}

func readArchive(t *testing.T, data []byte) []archiveEntry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	var out []archiveEntry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		require.Equal(t, byte(tar.TypeReg), hdr.Typeflag, hdr.Name)
		b, err := io.ReadAll(tr)
		require.NoError(t, err)
		out = append(out, archiveEntry{name: hdr.Name, mode: hdr.Mode, content: string(b), modTime: hdr.ModTime, uid: hdr.Uid})
	}
	return out
}

func archiveNames(t *testing.T, data []byte) []string {
	t.Helper()
	var names []string
	for _, e := range readArchive(t, data) {
		names = append(names, e.name)
	}
	return names
}

func toolsTree(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a/zeta/index.ts":         "zeta\n",
		"a/alpha/tool.json":       `{"name":"alpha"}`,
		"a/alpha/lib/deep/x.ts":   "deep\n",
		"a/.DS_Store":             "junk",
		"b/beta/index.ts":         "beta\n",
		"b/.hidden-tool/index.ts": "ignored",
	})
	return filepath.Join(root, "a"), filepath.Join(root, "b")
}

func TestPackTools_LayoutAndDeterminism(t *testing.T) {
	a, b := toolsTree(t)
	first, err := PackTools([]string{a, b})
	require.NoError(t, err)

	entries := readArchive(t, first.Bytes)
	var names []string
	for _, e := range entries {
		names = append(names, e.name)
		assert.Equal(t, int64(0o644), e.mode, e.name)
		assert.True(t, e.modTime.IsZero() || e.modTime.Unix() == 0, "no host mtimes in the archive: %s", e.name)
		assert.Zero(t, e.uid, "no host ownership in the archive")
	}
	assert.Equal(t, []string{
		"tools/client/alpha/lib/deep/x.ts",
		"tools/client/alpha/tool.json",
		"tools/client/beta/index.ts",
		"tools/client/zeta/index.ts",
	}, names, "sorted, rooted at tools/client/<tool>/, hidden entries skipped")
	assert.Equal(t, 4, first.Files)
	sum := sha256.Sum256(first.Bytes)
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), first.Digest)
	assert.Equal(t, "overlay-tools-"+hex.EncodeToString(sum[:]), first.ArchiveName())

	// Same content with different mtimes and permissions packs identically.
	past := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(filepath.Join(a, "zeta", "index.ts"), past, past))
	require.NoError(t, os.Chmod(filepath.Join(b, "beta", "index.ts"), 0o755))
	second, err := PackTools([]string{a, b})
	require.NoError(t, err)
	assert.Equal(t, first.Digest, second.Digest)
	assert.Equal(t, first.Bytes, second.Bytes)

	// Content changes change the digest.
	require.NoError(t, os.WriteFile(filepath.Join(a, "zeta", "index.ts"), []byte("zeta2\n"), 0o644))
	third, err := PackTools([]string{a, b})
	require.NoError(t, err)
	assert.NotEqual(t, first.Digest, third.Digest)
}

func TestPackTools_EmptyIsAValidArchive(t *testing.T) {
	none, err := PackTools(nil)
	require.NoError(t, err)
	assert.Zero(t, none.Files)
	assert.Empty(t, readArchive(t, none.Bytes))
	sum := sha256.Sum256(none.Bytes)
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), none.Digest)

	emptyDir, err := PackTools([]string{t.TempDir()})
	require.NoError(t, err)
	assert.Equal(t, none.Digest, emptyDir.Digest, "no tools always packs to the same archive")
}

func TestPackTools_Rejections(t *testing.T) {
	t.Run("symlinked file", func(t *testing.T) {
		a, _ := toolsTree(t)
		require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(a, "alpha", "passwd")))
		_, err := PackTools([]string{a})
		require.ErrorContains(t, err, "is not a regular file")
	})
	t.Run("symlinked tool directory", func(t *testing.T) {
		a, b := toolsTree(t)
		require.NoError(t, os.Symlink(filepath.Join(b, "beta"), filepath.Join(a, "linked")))
		_, err := PackTools([]string{a})
		require.ErrorContains(t, err, "linked is not a tool directory")
	})
	t.Run("duplicate tool across dirs", func(t *testing.T) {
		a, b := toolsTree(t)
		writeTree(t, b, map[string]string{"alpha/index.ts": "dup"})
		_, err := PackTools([]string{a, b})
		require.ErrorContains(t, err, `overlay tool "alpha" is defined in both`)
	})
	t.Run("file at tool level", func(t *testing.T) {
		a, _ := toolsTree(t)
		writeTree(t, a, map[string]string{"README.md": "not a tool"})
		_, err := PackTools([]string{a})
		require.ErrorContains(t, err, "README.md is not a tool directory")
	})
	t.Run("missing dir", func(t *testing.T) {
		_, err := PackTools([]string{filepath.Join(t.TempDir(), "missing")})
		require.ErrorContains(t, err, "overlay tools")
	})
	t.Run("size cap", func(t *testing.T) {
		dir := t.TempDir()
		big := make([]byte, MaxToolsArchiveBytes+1024)
		_, err := rand.Read(big) // incompressible
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "huge"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "huge", "blob.bin"), big, 0o644))
		_, err = PackTools([]string{dir})
		require.ErrorContains(t, err, "the limit is 716800")
	})
}

func TestToolsManifests(t *testing.T) {
	a, _ := toolsTree(t)
	archive, err := PackTools([]string{a})
	require.NoError(t, err)
	revision := "sha256:" + hex.EncodeToString(make([]byte, 32))
	nsDoc, archiveDoc, pointerDoc, err := ToolsManifests(archive, revision)
	require.NoError(t, err)

	type object struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Immutable  *bool  `json:"immutable"`
		Metadata   struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
		Data       map[string]string `json:"data"`
		BinaryData map[string]string `json:"binaryData"`
	}
	decode := func(doc string) object {
		var o object
		require.NoError(t, json.Unmarshal([]byte(doc), &o))
		return o
	}
	wantLabels := map[string]string{OwnerLabel: OwnerOverlay, ManagedByLabel: ManagedByForge, PartLabel: PartTools}

	ns := decode(nsDoc)
	assert.Equal(t, "Namespace", ns.Kind)
	assert.Equal(t, "iterabase-overlay", ns.Metadata.Name)
	assert.Equal(t, wantLabels, ns.Metadata.Labels)

	arc := decode(archiveDoc)
	assert.Equal(t, "v1", arc.APIVersion)
	assert.Equal(t, "ConfigMap", arc.Kind)
	require.NotNil(t, arc.Immutable)
	assert.True(t, *arc.Immutable)
	assert.Equal(t, archive.ArchiveName(), arc.Metadata.Name)
	assert.Equal(t, "iterabase-overlay", arc.Metadata.Namespace)
	assert.Equal(t, wantLabels, arc.Metadata.Labels)
	assert.Empty(t, arc.Data)
	require.Len(t, arc.BinaryData, 1)
	raw, err := base64.StdEncoding.DecodeString(arc.BinaryData["tools.tar.gz"])
	require.NoError(t, err)
	assert.Equal(t, archive.Bytes, raw)

	ptr := decode(pointerDoc)
	assert.Equal(t, "ConfigMap", ptr.Kind)
	assert.Nil(t, ptr.Immutable, "the pointer is repointed on every apply")
	assert.Equal(t, "overlay-tools", ptr.Metadata.Name)
	assert.Equal(t, "iterabase-overlay", ptr.Metadata.Namespace)
	assert.Equal(t, wantLabels, ptr.Metadata.Labels)
	assert.Equal(t, map[string]string{
		"digest":   archive.Digest,
		"archive":  archive.ArchiveName(),
		"revision": revision,
	}, ptr.Data)
	assert.Empty(t, ptr.BinaryData)

	assert.Equal(t, OwnerLabel+"="+OwnerOverlay+","+PartLabel+"="+PartTools, ToolsSelector)
	assert.Equal(t, OwnerLabel+"="+OwnerOverlay+","+PartLabel+"="+PartObjects, ObjectsSelector)
}

func TestPackTools_ExcludesHiddenPathsAtEveryDepth(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"crm/index.ts":                "x",
		"crm/.env":                    "SECRET=1",
		"crm/.git/config":             "[core]",
		"crm/lib/.cache/blob":         "cached",
		"crm/lib/.npmrc":              "//registry:_authToken=x",
		"crm/lib/util.ts":             "y",
		"crm/node_modules/.bin/thing": "z",
	})
	archive, err := PackTools([]string{root})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"tools/client/crm/index.ts",
		"tools/client/crm/lib/util.ts",
	}, archiveNames(t, archive.Bytes))
	assert.Equal(t, 2, archive.Files)

	// A hidden symlink is skipped rather than rejected: it never reaches the archive.
	require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(root, "crm", ".passwd")))
	again, err := PackTools([]string{root})
	require.NoError(t, err)
	assert.Equal(t, archive.Digest, again.Digest)
}
