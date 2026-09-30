// Package memory is the stack's in-memory storage profile, for tests and
// experiments. Nothing survives the process.
//
// A Backend may hand out several Storage handles onto the same stores, which
// is how a test restarts a stack over the same state, or runs a Factory with
// RemoteHosts and a ServeHost Host in one binary.
package memory

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/stack"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// Backend is one set of in-memory stores: a control composite and one
// journal composite per tenant, created on first use.
type Backend struct {
	control    *storage.Composite
	workspaces string

	mu       sync.Mutex
	journals map[sessionwire.TenantID]*storage.Composite
}

// New returns an empty Backend. Its workspace directory is a fresh path under
// os.TempDir, created on first use and removed by Close.
func New() *Backend {
	var suffix [8]byte
	_, _ = rand.Read(suffix[:]) // crypto/rand.Read never fails (Go 1.24+)
	return &Backend{
		control:    memstore.New(),
		workspaces: filepath.Join(os.TempDir(), "looprig-stack-memory-"+hex.EncodeToString(suffix[:])),
		journals:   map[sessionwire.TenantID]*storage.Composite{},
	}
}

// Storage returns a handle onto b. Its Close does nothing, so the state
// outlives a stopped stack; close the Backend when done.
func (b *Backend) Storage() stack.Storage {
	return stack.Storage{Control: b.control, Journal: b.journal, Workspaces: b.workspaces}
}

// Close removes the workspace directory. The stores themselves are garbage.
func (b *Backend) Close() error { return os.RemoveAll(b.workspaces) }

// Open is New().Storage() for a single use: its Close is the Backend's.
func Open() stack.Storage {
	backend := New()
	storage := backend.Storage()
	storage.Close = backend.Close
	return storage
}

func (b *Backend) journal(tenant sessionwire.TenantID) (*storage.Composite, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	backend, ok := b.journals[tenant]
	if !ok {
		backend = memstore.New()
		b.journals[tenant] = backend
	}
	return backend, nil
}
