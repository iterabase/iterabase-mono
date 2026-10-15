// Package artifacttest provides artifact-domain test doubles.
package artifacttest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	artifactstore "github.com/nunocgoncalves/iterabase-mono/control-plane/internal/artifact"
)

// MemoryObjects is an in-memory LifecycleStore with create-only writes.
type MemoryObjects struct {
	mu sync.Mutex
	m  map[string][]byte
	// FailDelete makes Delete fail for keys it reports true for.
	FailDelete func(key string) bool
}

var _ artifactstore.LifecycleStore = (*MemoryObjects)(nil)

func NewMemoryObjects() *MemoryObjects { return &MemoryObjects{m: map[string][]byte{}} }

func (m *MemoryObjects) Ready(context.Context) error { return nil }

func (m *MemoryObjects) PutNew(_ context.Context, key string, r io.Reader, size int64, _ string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if size >= 0 && int64(len(b)) != size {
		return fmt.Errorf("size mismatch: declared %d, got %d", size, len(b))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.m[key]; ok {
		return artifactstore.ErrObjectExists
	}
	m.m[key] = b
	return nil
}

func (m *MemoryObjects) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.m[key]
	if !ok {
		return nil, errors.New("missing object")
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), nil
}

func (m *MemoryObjects) Copy(ctx context.Context, src, dst string) error {
	r, err := m.Get(ctx, src)
	if err != nil {
		return err
	}
	return m.PutNew(ctx, dst, r, -1, "")
}

func (m *MemoryObjects) Delete(_ context.Context, key string) error {
	if m.FailDelete != nil && m.FailDelete(key) {
		return errors.New("injected delete failure")
	}
	m.mu.Lock()
	delete(m.m, key)
	m.mu.Unlock()
	return nil
}

// Keys returns the stored keys.
func (m *MemoryObjects) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.m))
	for k := range m.m {
		out = append(out, k)
	}
	return out
}
