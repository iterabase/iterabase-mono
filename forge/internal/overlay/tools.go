package overlay

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Customer tool delivery contract with the tool-runner materializer
// (DES-HOR-593-05).
const (
	ToolsNamespace     = "iterabase-overlay"
	ToolsPointerName   = "overlay-tools"
	ToolsArchivePrefix = "overlay-tools-"
	ToolsArchiveKey    = "tools.tar.gz"
	// MaxToolsArchiveBytes keeps the archive ConfigMap, whose binaryData is
	// base64-encoded, inside Kubernetes' 1 MiB object limit.
	MaxToolsArchiveBytes = 700 * 1024
	toolsArchiveRoot     = "tools/client"
)

// ToolsArchive is the gzip tar of the overlay's tools/client layer.
type ToolsArchive struct {
	Bytes  []byte
	Digest string // sha256:<hex> of Bytes
	Files  int
}

// ArchiveName is the immutable ConfigMap that holds this archive.
func (a ToolsArchive) ArchiveName() string {
	return ToolsArchivePrefix + strings.TrimPrefix(a.Digest, "sha256:")
}

// PackTools builds a deterministic archive of every tool under dirs. Each dir
// holds one directory per tool; a tool name may appear in only one dir. Only
// regular files are packed; hidden files and directories (.git, .env, ...) at
// any depth are left out, and symlinks and other file types are refused. An
// overlay without tools still yields a valid empty archive, so a configured
// overlay is never mistaken for "not delivered yet".
func PackTools(dirs []string) (ToolsArchive, error) {
	files, err := collectToolFiles(dirs)
	if err != nil {
		return ToolsArchive{}, err
	}
	data, err := writeToolsArchive(files)
	if err != nil {
		return ToolsArchive{}, err
	}
	if len(data) > MaxToolsArchiveBytes {
		return ToolsArchive{}, fmt.Errorf("overlay tools archive is %d bytes; the limit is %d (one ConfigMap, DES-HOR-593-05)", len(data), MaxToolsArchiveBytes)
	}
	sum := sha256.Sum256(data)
	return ToolsArchive{Bytes: data, Digest: "sha256:" + hex.EncodeToString(sum[:]), Files: len(files)}, nil
}

// collectToolFiles maps archive paths (tools/client/<tool>/...) to sources.
func collectToolFiles(dirs []string) (map[string]string, error) {
	files := map[string]string{}
	owners := map[string]string{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("overlay tools %s: %w", dir, err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if !entry.IsDir() {
				return nil, fmt.Errorf("overlay tools %s: %s is not a tool directory", dir, entry.Name())
			}
			if other, ok := owners[entry.Name()]; ok {
				return nil, fmt.Errorf("overlay tool %q is defined in both %s and %s", entry.Name(), other, dir)
			}
			owners[entry.Name()] = dir
			if err := addToolFiles(files, dir, filepath.Join(dir, entry.Name())); err != nil {
				return nil, err
			}
		}
	}
	return files, nil
}

func addToolFiles(files map[string]string, dir, toolDir string) error {
	return filepath.WalkDir(toolDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != toolDir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("overlay tool file %s is not a regular file", p)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files[path.Join(toolsArchiveRoot, filepath.ToSlash(rel))] = p
		return nil
	})
}

// writeToolsArchive writes files in sorted order with fixed metadata, so the
// same content always yields the same bytes and digest.
func writeToolsArchive(files map[string]string) ([]byte, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(gz)
	for _, name := range names {
		content, err := os.ReadFile(files[name])
		if err != nil {
			return nil, err
		}
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(content); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ToolsManifests returns the namespace, the immutable archive ConfigMap and the
// pointer ConfigMap, as JSON documents for kubectl apply. The archive must
// exist before the pointer names it.
func ToolsManifests(a ToolsArchive, revision string) (namespace, archive, pointer string, err error) {
	labels := map[string]string{OwnerLabel: OwnerOverlay, ManagedByLabel: ManagedByForge, PartLabel: PartTools}
	ns := map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": ToolsNamespace, "labels": labels},
	}
	arc := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "immutable": true,
		"metadata":   map[string]any{"name": a.ArchiveName(), "namespace": ToolsNamespace, "labels": labels},
		"binaryData": map[string]string{ToolsArchiveKey: base64.StdEncoding.EncodeToString(a.Bytes)},
	}
	ptr := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": ToolsPointerName, "namespace": ToolsNamespace, "labels": labels},
		"data":     map[string]string{"digest": a.Digest, "archive": a.ArchiveName(), "revision": revision},
	}
	docs := make([]string, 0, 3)
	for _, doc := range []any{ns, arc, ptr} {
		b, err := json.Marshal(doc)
		if err != nil {
			return "", "", "", err
		}
		docs = append(docs, string(b))
	}
	return docs[0], docs[1], docs[2], nil
}
