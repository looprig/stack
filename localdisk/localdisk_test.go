package localdisk_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/fsstore"
	"github.com/looprig/stack/localdisk"
	"github.com/looprig/storage"
)

func TestOpenLaysOutOneDataDirectory(t *testing.T) {
	dir := t.TempDir()
	store, err := localdisk.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !store.SingleProcess {
		t.Error("localdisk storage is not marked SingleProcess")
	}
	if store.Workspaces != filepath.Join(dir, "workspaces") {
		t.Errorf("Workspaces = %q", store.Workspaces)
	}
	if _, ok := store.Control.Blobs.(storage.BlobReaderLifecycle); !ok {
		t.Error("control Blobs is not bounded; SessionStore would refuse it")
	}
	journal, err := store.Journal("acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := journal.Blobs.(storage.BlobReaderLifecycle); !ok {
		t.Error("journal Blobs is not bounded")
	}
	again, err := store.Journal("acme")
	if err != nil || again != journal {
		t.Fatalf("a second Journal call opened a second root (%v)", err)
	}
	other, err := store.Journal("globex")
	if err != nil || other == journal {
		t.Fatalf("two tenants share a journal backend (%v)", err)
	}
	for _, sub := range []string{"control", "journal/acme", "journal/globex"} {
		if info, err := os.Stat(filepath.Join(dir, sub)); err != nil || !info.IsDir() {
			t.Errorf("%s was not created: %v", sub, err)
		}
	}
	// A write lands, and survives a reopen.
	if _, err := store.Control.KV.Put(context.Background(), "probe", 0, []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if _, err := store.Journal("acme"); err == nil {
		t.Fatal("Journal after Close succeeded")
	}
	reopened, err := localdisk.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if value, _, err := reopened.Control.KV.Get(context.Background(), "probe"); err != nil || string(value) != "v" {
		t.Fatalf("reopened control lost the write: %q, %v", value, err)
	}
}

func TestOpenRefusesALegacyDataDirectory(t *testing.T) {
	for _, root := range []string{"control", "journal/acme"} {
		t.Run(root, func(t *testing.T) {
			dir := t.TempDir()
			// A pre-v0.6.0 fsstore KV leaf has no "@kv" suffix.
			legacy := filepath.Join(dir, root, "kv")
			if err := os.MkdirAll(legacy, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(legacy, "old"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := localdisk.Open(dir)
			if err == nil {
				defer func() { _ = store.Close() }()
				_, err = store.Journal("acme")
			}
			var refusal *localdisk.LegacyDataDirError
			if !errors.As(err, &refusal) || !errors.Is(err, fsstore.ErrLegacyLayout) {
				t.Fatalf("err = %v, want *LegacyDataDirError wrapping fsstore.ErrLegacyLayout", err)
			}
			if refusal.Dir != dir || !strings.Contains(err.Error(), "move or delete "+dir) {
				t.Fatalf("refusal %q does not tell the operator to move or delete %s", err, dir)
			}
		})
	}
}

func TestJournalRefusesATenantThatIsNotADirectoryName(t *testing.T) {
	store, err := localdisk.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	for _, tenant := range []string{"..", ".", "a/b", "Acme", "", "-x", strings.Repeat("a", 65)} {
		var refusal *localdisk.UnsupportedTenantError
		if _, err := store.Journal(sessionwireTenant(tenant)); !errors.As(err, &refusal) {
			t.Errorf("Journal(%q) = %v, want *UnsupportedTenantError", tenant, err)
		}
	}
}

func sessionwireTenant(s string) sessionwire.TenantID { return sessionwire.TenantID(s) }
