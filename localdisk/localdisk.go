// Package localdisk is the stack's single-machine storage profile: fsstore
// roots under one data directory.
//
//	<dir>/control            Factory and Host's SessionStore
//	<dir>/journal/<tenant>   one harness journal backend per tenant
//	<dir>/workspaces         session workspaces
//
// SINGLE PROCESS ONLY. fsstore is a local filesystem store, so the returned
// Storage is marked SingleProcess and stack refuses it with RemoteHosts.
//
// Every root's Blobs is adapted with storage.WithBoundedBlobReaders, the
// explicit single-host opt-in SessionStore needs from a filesystem store: a
// stuck read is abandoned at Close, not cancelled.
//
// A data directory written before fsstore v0.6.0 is refused with
// *LegacyDataDirError, which wraps fsstore.ErrLegacyLayout. There is no
// migration and retrying cannot help: move or delete the directory. Never
// point an older binary at a directory this one wrote; it would silently
// misread it.
package localdisk

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/fsstore"
	"github.com/looprig/stack"
	"github.com/looprig/storage"
)

// LegacyDataDirError is a data directory holding pre-v0.6.0 fsstore data.
// Remedy: move or delete Dir. It is permanent; never retry.
type LegacyDataDirError struct {
	Dir  string // the data directory to move or delete
	Root string // the fsstore root that was refused
	Err  error  // fsstore's *LegacyLayoutError
}

func (e *LegacyDataDirError) Error() string {
	return "localdisk: " + e.Dir + " holds data from a Looprig release before fsstore v0.6.0 (" + e.Root + "); there is no migration, so move or delete " + e.Dir + ": " + e.Err.Error()
}

// Unwrap returns fsstore's refusal, so errors.Is(err, fsstore.ErrLegacyLayout)
// holds.
func (e *LegacyDataDirError) Unwrap() error { return e.Err }

// UnsupportedTenantError is a tenant id localdisk cannot use as a directory
// name. Tenants must match [a-z0-9][a-z0-9_.-]{0,63}.
type UnsupportedTenantError struct{ Tenant sessionwire.TenantID }

func (e *UnsupportedTenantError) Error() string {
	return "localdisk: tenant " + strconv.Quote(string(e.Tenant)) + " is not usable as a directory name; use [a-z0-9][a-z0-9_.-]{0,63}"
}

var tenantPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// Open opens (creating if absent) the control root under dir and returns the
// profile. Tenant journal roots open on first use. Close closes every root
// it opened.
func Open(dir string) (stack.Storage, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return stack.Storage{}, err
	}
	d := &disk{dir: dir, journals: map[sessionwire.TenantID]*storage.Composite{}}
	control, err := d.open(filepath.Join(dir, "control"))
	if err != nil {
		return stack.Storage{}, err
	}
	return stack.Storage{
		Control:       control,
		Journal:       d.journal,
		Workspaces:    filepath.Join(dir, "workspaces"),
		SingleProcess: true,
		Close:         d.close,
	}, nil
}

type disk struct {
	dir      string
	mu       sync.Mutex
	stores   []*fsstore.Store
	journals map[sessionwire.TenantID]*storage.Composite
	closed   bool
}

func (d *disk) open(root string) (*storage.Composite, error) {
	store, err := fsstore.Open(fsstore.Options{Root: root})
	if errors.Is(err, fsstore.ErrLegacyLayout) {
		return nil, &LegacyDataDirError{Dir: d.dir, Root: root, Err: err}
	}
	if err != nil {
		return nil, fmt.Errorf("localdisk: open %s: %w", root, err)
	}
	bounded, err := store.Backend().WithBoundedBlobReaders()
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	d.stores = append(d.stores, store)
	return bounded, nil
}

func (d *disk) journal(tenant sessionwire.TenantID) (*storage.Composite, error) {
	if !tenantPattern.MatchString(string(tenant)) {
		return nil, &UnsupportedTenantError{Tenant: tenant}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, errors.New("localdisk: storage is closed")
	}
	if backend, ok := d.journals[tenant]; ok {
		return backend, nil
	}
	backend, err := d.open(filepath.Join(d.dir, "journal", string(tenant)))
	if err != nil {
		return nil, err
	}
	d.journals[tenant] = backend
	return backend, nil
}

// close closes every root, journals first and control last.
func (d *disk) close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	var errs []error
	for _, store := range slices.Backward(d.stores) {
		errs = append(errs, store.Close())
	}
	return errors.Join(errs...)
}
