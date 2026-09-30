package stack

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/storage/memstore"
)

// TestEveryStartStepReleasesEverything fails Start after each step in turn
// and proves the failure released what the step had opened: Storage.Close
// ran exactly once, the HostLink port is free again, and a new Start over
// the same control backend succeeds and stops cleanly.
func TestEveryStartStepReleasesEverything(t *testing.T) {
	steps := []string{"journals", "readers", "control", "factory", "generation", "listen", "host-compose", "host-start", "factory-start"}
	injected := errors.New("injected start failure")
	defer func() { startFault = nil }()
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			control := memstore.New()
			listen := freeLoopback(t)
			options := func() (Options, *atomic.Int64) {
				o := validOptions()
				o.Storage.Control = control
				o.Storage.Workspaces = t.TempDir()
				o.Hosts = InProcess{Listen: listen}
				var closes atomic.Int64
				o.Storage.Close = func() error { closes.Add(1); return nil }
				return o, &closes
			}

			reached := false
			startFault = func(at string) error {
				if at == step {
					reached = true
					return injected
				}
				return nil
			}
			o, closes := options()
			s, err := Start(ctx, o)
			startFault = nil
			if !reached {
				t.Fatalf("Start never reached step %q (err %v)", step, err)
			}
			if s != nil || !errors.Is(err, injected) {
				t.Fatalf("Start = %v, %v; want nil and the injected failure", s, err)
			}
			if closes.Load() != 1 {
				t.Fatalf("Storage.Close ran %d times, want once", closes.Load())
			}
			ln, err := net.Listen("tcp", listen)
			if err != nil {
				t.Fatalf("the HostLink port %s is still held: %v", listen, err)
			}
			_ = ln.Close()

			o, closes = options()
			s, err = Start(ctx, o)
			if err != nil {
				t.Fatalf("a Start after the failed one: %v", err)
			}
			if err := s.Stop(ctx); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if closes.Load() != 1 {
				t.Fatalf("Storage.Close ran %d times on Stop, want once", closes.Load())
			}
		})
	}
}

// freeLoopback returns a loopback address no one is listening on.
func freeLoopback(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
