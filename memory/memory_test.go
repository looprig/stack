package memory_test

import (
	"context"
	"os"
	"testing"

	"github.com/looprig/stack/memory"
	"github.com/looprig/storage"
)

func TestHandlesShareOneBackend(t *testing.T) {
	backend := memory.New()
	first, second := backend.Storage(), backend.Storage()
	if first.Control != second.Control || first.Workspaces != second.Workspaces {
		t.Fatal("two handles onto one Backend do not share its stores")
	}
	a, _ := first.Journal("acme")
	b, _ := second.Journal("acme")
	c, _ := second.Journal("globex")
	if a != b || a == c {
		t.Fatal("journals are not one per tenant across handles")
	}
	if _, ok := first.Control.Blobs.(storage.BlobReaderLifecycle); !ok {
		t.Fatal("memory Blobs does not implement BlobReaderLifecycle")
	}
	if first.Close != nil {
		t.Fatal("a Backend handle's Close must not end the Backend")
	}
	if first.SingleProcess {
		t.Fatal("memory is marked SingleProcess; the in-binary RemoteHosts test needs it shared")
	}
}

func TestOpenClosesItsWorkspaces(t *testing.T) {
	store := memory.Open()
	if err := os.MkdirAll(store.Workspaces, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Control.KV.Put(context.Background(), "k", 0, []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Workspaces); !os.IsNotExist(err) {
		t.Fatalf("workspaces survived Close: %v", err)
	}
}
