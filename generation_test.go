package stack

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

func TestNextGenerationStrictlyIncreasesPerHost(t *testing.T) {
	ctx := context.Background()
	kv := memstore.New().KV
	for want := uint64(1); want <= 3; want++ {
		got, err := nextGeneration(ctx, kv, "local")
		if err != nil || got != want {
			t.Fatalf("nextGeneration = %d, %v; want %d", got, err, want)
		}
	}
	if got, err := nextGeneration(ctx, kv, "Other Host/7"); err != nil || got != 1 {
		t.Fatalf("a second HostID's first generation = %d, %v; want 1", got, err)
	}
}

func TestNextGenerationUnderConcurrencyIsUnique(t *testing.T) {
	ctx := context.Background()
	kv := memstore.New().KV
	const starts = 6
	var (
		mu   sync.Mutex
		seen = map[uint64]bool{}
		wg   sync.WaitGroup
	)
	for range starts {
		wg.Go(func() {
			got, err := nextGeneration(ctx, kv, "local")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[got] {
				t.Errorf("generation %d handed out twice", got)
			}
			seen[got] = true
		})
	}
	wg.Wait()
}

// conflictOnce refuses the first Put with a revision conflict, as a racing
// writer would.
type conflictOnce struct {
	storage.KV
	once sync.Once
}

func (c *conflictOnce) Put(ctx context.Context, key string, rev uint64, value []byte) (uint64, error) {
	var conflict error
	c.once.Do(func() { conflict = &storage.ConflictError{Name: key, Expected: rev} })
	if conflict != nil {
		return 0, conflict
	}
	return c.KV.Put(ctx, key, rev, value)
}

func TestNextGenerationRetriesAConflict(t *testing.T) {
	kv := &conflictOnce{KV: memstore.New().KV}
	if got, err := nextGeneration(context.Background(), kv, "local"); err != nil || got != 1 {
		t.Fatalf("nextGeneration after one conflict = %d, %v; want 1", got, err)
	}
}

func TestNextGenerationRefusesACorruptCounter(t *testing.T) {
	ctx := context.Background()
	kv := memstore.New().KV
	if _, err := kv.Put(ctx, generationKey("local"), 0, []byte("not a number")); err != nil {
		t.Fatal(err)
	}
	if _, err := nextGeneration(ctx, kv, "local"); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("nextGeneration over a corrupt counter = %v, want a refusal", err)
	}
}

func TestPathSegmentNeverEscapesItsRoot(t *testing.T) {
	for _, id := range []string{"..", ".", "a/b", "../../etc", "x\x00y", "~abc", strings.Repeat("a", 200), "Ünïcode"} {
		got := pathSegment(id)
		if got == id || !strings.HasPrefix(got, "~") || strings.ContainsAny(got, "/\\\x00") || len(got) > 65 {
			t.Errorf("pathSegment(%q) = %q, want a hashed single segment", id, got)
		}
	}
	for _, id := range []string{"acme", "0b6a1c1e-0000-4000-8000-000000000000", "a.b_c-d"} {
		if got := pathSegment(id); got != id {
			t.Errorf("pathSegment(%q) = %q, want it unchanged", id, got)
		}
	}
}
