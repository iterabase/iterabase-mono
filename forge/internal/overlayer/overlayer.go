// Package overlayer defines the host-level overlay staging interface — the
// testability seam for placing the operator-rendered overlay's Helm value files
// on the host, where Helm runs. The real implementation lives in
// internal/sshprovisioner; tests use fakes. Lifecycle logic never talks to SSH
// directly — it orchestrates against this interface.
package overlayer

import "context"

// File is one staged file: a base name inside the staging directory and its
// content.
type File struct {
	Name    string
	Content []byte
}

// Overlayer stages rendered overlay files on the host. Forge renders the
// overlay on the operator machine (DES-HOR-593-03); only these files reach the
// host.
type Overlayer interface {
	// StageFiles replaces dir with exactly files (directory 0700, files 0600),
	// written over SSH stdin, and returns their host paths in order.
	StageFiles(ctx context.Context, dir string, files []File) ([]string, error)
	// Remove deletes the staging directory. A missing directory is not an error.
	Remove(ctx context.Context, dir string) error
}
