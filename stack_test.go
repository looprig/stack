package stack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/host"
	"github.com/looprig/host/department"
	"github.com/looprig/inference"
	"github.com/looprig/inference/inferencetest"
	"github.com/looprig/inference/model"
	"github.com/looprig/stack"
	"github.com/looprig/stack/devauth"
	"github.com/looprig/stack/memory"
	"github.com/looprig/storage"
)

const (
	tenant   sessionwire.TenantID = "acme"
	agentID  sessionwire.AgentID  = "echo"
	devToken                      = "stack-test-devToken-0123456789"
)

// echoModel replies with the user's last message, streamed.
func echoModel() *inferencetest.Client {
	return inferencetest.New(inferencetest.Func(func(req inference.Request) inferencetest.Step {
		return inferencetest.Text("echo: " + inferencetest.LastUserText(req)).ChunkSize(3)
	}).Repeat())
}

// echoAgent is one agent whose rig journals into the binding's store. It
// requires a workspace, and refuses a launch whose workspace is not under
// workspaces.
func echoAgent(client inference.Client, workspaces string) stack.Agent {
	return stack.Agent{
		ID:            agentID,
		Compatibility: "stack-test/echo/v1",
		Capabilities:  department.Capabilities{CaptureSafety: department.CaptureSafetyStreaming, RequiresWorkspace: true},
		Define: func(_ context.Context, b stack.Binding) (*rig.Rig, error) {
			if b.Journal == nil || b.Tenant != tenant || b.Session == "" {
				return nil, errors.New("binding is incomplete")
			}
			if !strings.HasPrefix(b.WorkspaceRoot, workspaces+string(os.PathSeparator)) {
				return nil, fmt.Errorf("workspace %q is not under %q", b.WorkspaceRoot, workspaces)
			}
			if info, err := os.Stat(b.WorkspaceRoot); err != nil || !info.IsDir() {
				return nil, fmt.Errorf("workspace %q was not materialized: %v", b.WorkspaceRoot, err)
			}
			access, err := gate.NewHeadlessEvaluator(nil, nil, nil)
			if err != nil {
				return nil, err
			}
			agent, err := loop.Define(
				loop.WithName("echo"),
				loop.WithSystem("Echo the user."),
				loop.WithInference(client, inferencetest.Model(model.WithTools())),
				loop.WithAccessGate(access),
				loop.WithPolicyRevision("stack-test/v1"),
			)
			if err != nil {
				return nil, err
			}
			return rig.Define(rig.WithLoops(agent), rig.WithPrimers("echo"), rig.WithSessionStore(b.Journal))
		},
	}
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func options(t *testing.T, store stack.Storage, client inference.Client) stack.Options {
	t.Helper()
	dev, err := devauth.New(tenant, devToken)
	if err != nil {
		t.Fatal(err)
	}
	return stack.Options{
		Storage:  store,
		Tenants:  []sessionwire.TenantID{tenant},
		Identity: dev.Identity(),
		Agents:   []stack.Agent{echoAgent(client, store.Workspaces)},
		Live:     &host.LiveTextOptions{IncludeReasoning: true, IncludeToolSteps: true},
		Origins:  []string{"http://127.0.0.1"},
		Logger:   testLogger(),
	}
}

type running struct {
	*httptest.Server
	stack *stack.Stack
}

func start(t *testing.T, ctx context.Context, o stack.Options) *running {
	t.Helper()
	s, err := stack.Start(ctx, o)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	r := &running{Server: httptest.NewServer(s.Handler()), stack: s}
	t.Cleanup(func() { r.stop(t) })
	return r
}

func (r *running) stop(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.stack.Stop(ctx); err != nil {
		t.Errorf("Stop: %v", err)
	}
	r.Close()
}

func TestStartCreatesAppliesInputAndRestoresAfterRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	backend := memory.New()
	t.Cleanup(func() { _ = backend.Close() })

	first := start(t, ctx, options(t, backend.Storage(), echoModel()))
	if status, body := call(t, ctx, first.URL, http.MethodGet, "/v1/sessions", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list = %d %s, want 401", status, body)
	}
	sid := sessionwire.SessionID(newID(t))
	create := sessionwire.CreateRequest{CommandEnvelope: envelope(t), SessionID: sid, AgentID: agentID, Blocks: blocks(t, "first message")}
	if status, body := call(t, ctx, first.URL, http.MethodPost, "/v1/sessions", "alice", create); status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	waitForJournal(t, ctx, first.URL, sid, "echo: first message")
	input := sessionwire.InputRequest{CommandEnvelope: envelope(t), SessionID: sid, Blocks: blocks(t, "second message")}
	if status, body := call(t, ctx, first.URL, http.MethodPost, "/v1/sessions/"+string(sid)+"/input", "bob", input); status/100 != 2 {
		t.Fatalf("input = %d %s", status, body)
	}
	journal := waitForJournal(t, ctx, first.URL, sid, "echo: second message")
	// Factory stamps the verified sender and the runtime carries it into the
	// journal: the input was bob's, not the service's.
	if !strings.Contains(journal, `"bob"`) {
		t.Errorf("journal does not attribute the input to bob:\n%s", journal)
	}
	first.stop(t)

	second := start(t, ctx, options(t, backend.Storage(), echoModel()))
	input = sessionwire.InputRequest{CommandEnvelope: envelope(t), SessionID: sid, Blocks: blocks(t, "after restart")}
	if status, body := call(t, ctx, second.URL, http.MethodPost, "/v1/sessions/"+string(sid)+"/input", "alice", input); status/100 != 2 {
		t.Fatalf("input after restart = %d %s", status, body)
	}
	journal = waitForJournal(t, ctx, second.URL, sid, "echo: after restart")
	if !strings.Contains(journal, "echo: first message") {
		t.Errorf("restored journal lost the first turn:\n%s", journal)
	}
}

// TestStopRunsTheSafeOrder: Storage.Close runs once, last, after the Host has
// drained and stopped, and a second Stop returns the first's result.
func TestStopRunsTheSafeOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	backend := memory.New()
	t.Cleanup(func() { _ = backend.Close() })
	store := backend.Storage()
	var (
		closes      atomic.Int64
		hostLive    atomic.Bool
		application atomic.Pointer[stack.Stack]
	)
	store.Close = func() error {
		closes.Add(1)
		hostLive.Store(application.Load().Host().Live())
		return nil
	}
	s, err := stack.Start(ctx, options(t, store, echoModel()))
	if err != nil {
		t.Fatal(err)
	}
	application.Store(s)
	server := httptest.NewServer(s.Handler())
	defer server.Close()

	// Make a session resident so the drain has work.
	sid := sessionwire.SessionID(newID(t))
	create := sessionwire.CreateRequest{CommandEnvelope: envelope(t), SessionID: sid, AgentID: agentID, Blocks: blocks(t, "resident")}
	if status, body := call(t, ctx, server.URL, http.MethodPost, "/v1/sessions", "alice", create); status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	waitForJournal(t, ctx, server.URL, sid, "echo: resident")

	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v, want a clean drain", err)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("second Stop = %v", err)
	}
	if closes.Load() != 1 {
		t.Fatalf("Storage.Close ran %d times, want once", closes.Load())
	}
	if hostLive.Load() {
		t.Fatal("Storage.Close ran while the Host was still live")
	}
}

func TestStartReleasesEverythingOnFailure(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		spoil func(*stack.Options)
		check func(*testing.T, error)
	}{
		{"validation", func(o *stack.Options) { o.Agents = nil }, func(t *testing.T, err error) {
			var refusal *stack.OptionError
			if !errors.As(err, &refusal) || refusal.Field != "Agents" {
				t.Fatalf("Start = %v, want an Agents refusal", err)
			}
		}},
		{"journal backend", func(o *stack.Options) {
			o.Storage.Journal = func(sessionwire.TenantID) (*storage.Composite, error) { return nil, errJournal }
		}, func(t *testing.T, err error) {
			if !errors.Is(err, errJournal) {
				t.Fatalf("Start = %v, want the journal failure", err)
			}
		}},
		{"hostlink port taken", func(o *stack.Options) {
			busy, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = busy.Close() })
			o.Hosts = stack.InProcess{Listen: busy.Addr().String()}
		}, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "HostLink listener") {
				t.Fatalf("Start = %v, want the listener failure", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := memory.New()
			t.Cleanup(func() { _ = backend.Close() })
			o := options(t, backend.Storage(), echoModel())
			var closes atomic.Int64
			o.Storage.Close = func() error { closes.Add(1); return nil }
			tc.spoil(&o)
			s, err := stack.Start(ctx, o)
			if s != nil {
				t.Fatal("Start returned a stack with an error")
			}
			tc.check(t, err)
			if closes.Load() != 1 {
				t.Fatalf("Storage.Close ran %d times after a failed Start, want once", closes.Load())
			}
		})
	}
}

var errJournal = errors.New("journal backend unavailable")

// TestRemoteHostsServeHost runs the split in one binary: a Factory-only stack
// with RemoteHosts, and a ServeHost Host on its own listener, over two
// Storage handles onto one memory backend.
func TestRemoteHostsServeHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	backend := memory.New()
	t.Cleanup(func() { _ = backend.Close() })
	credential := strings.Repeat("r", stack.MinHostLinkCredentialBytes)

	// The Host process.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client := echoModel()
	service, handler, err := stack.ServeHost(ctx, stack.HostOptions{
		Storage:    backend.Storage(),
		Tenants:    []sessionwire.TenantID{tenant},
		Agents:     []stack.Agent{echoAgent(client, backend.Storage().Workspaces)},
		HostID:     "remote-1",
		Base:       sessionwire.InternalEndpoint("ws://" + ln.Addr().String()),
		Credential: stack.TokenVerifier(credential),
		Logger:     testLogger(),
	})
	if err != nil {
		t.Fatalf("ServeHost: %v", err)
	}
	hostServer := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hostServer.Serve(ln) }()

	// The Factory process.
	o := options(t, backend.Storage(), client)
	o.Hosts = stack.RemoteHosts{}
	o.Limits.HostLinkCredential = credential
	s, err := stack.Start(ctx, o)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.Host() != nil {
		t.Fatal("a RemoteHosts stack runs an in-process Host")
	}
	server := httptest.NewServer(s.Handler())
	defer server.Close()

	sid := sessionwire.SessionID(newID(t))
	create := sessionwire.CreateRequest{CommandEnvelope: envelope(t), SessionID: sid, AgentID: agentID, Blocks: blocks(t, "over the split")}
	if status, body := call(t, ctx, server.URL, http.MethodPost, "/v1/sessions", "alice", create); status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	waitForJournal(t, ctx, server.URL, sid, "echo: over the split")

	// Remote order: fence Factory, drain the Host while HostLink is served,
	// then stop Factory and close storage.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	if err := s.Factory().Quiesce(stopCtx); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	report, err := service.Stop(stopCtx)
	if err != nil || len(report.Failures) != 0 {
		t.Fatalf("host Stop = %v, %+v", err, report.Failures)
	}
	_ = hostServer.Close()
	if err := s.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestServeHostRefusesBeforeIO(t *testing.T) {
	var journals atomic.Int64
	store := memory.Open()
	defer func() { _ = store.Close() }()
	inner := store.Journal
	store.Journal = func(t sessionwire.TenantID) (*storage.Composite, error) { journals.Add(1); return inner(t) }
	_, _, err := stack.ServeHost(context.Background(), stack.HostOptions{
		Storage: store, Tenants: []sessionwire.TenantID{tenant}, Agents: []stack.Agent{echoAgent(echoModel(), store.Workspaces)},
		HostID: "h", Base: "ws://10.0.0.1:9000/prefix", Credential: stack.TokenVerifier(devToken),
	})
	var refusal *stack.OptionError
	if !errors.As(err, &refusal) || refusal.Field != "Base" {
		t.Fatalf("ServeHost = %v, want a Base refusal", err)
	}
	if journals.Load() != 0 {
		t.Fatal("ServeHost opened storage before refusing its options")
	}
}

func TestTokenVerifier(t *testing.T) {
	v := stack.TokenVerifier(devToken)
	if err := v.VerifyTenant(context.Background(), tenant, devToken); err != nil {
		t.Fatalf("matching devToken refused: %v", err)
	}
	for _, presented := range []string{"", devToken + "x", devToken[:len(devToken)-1]} {
		if err := v.VerifyTenant(context.Background(), tenant, presented); err == nil {
			t.Fatalf("devToken %q accepted", presented)
		}
	}
	if err := stack.TokenVerifier("").VerifyTenant(context.Background(), tenant, ""); err == nil {
		t.Fatal("an empty configured devToken accepts an empty presented one")
	}
}

// --- REST helpers -----------------------------------------------------------

func waitForJournal(t *testing.T, ctx context.Context, base string, sid sessionwire.SessionID, want string) string {
	t.Helper()
	var body string
	for ctx.Err() == nil {
		var status int
		status, body = call(t, ctx, base, http.MethodGet, "/v1/sessions/"+string(sid)+"/journal?limit=200", "alice", nil)
		if status == http.StatusOK && strings.Contains(body, want) {
			return body
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("journal never contained %q; last body:\n%s", want, body)
	return ""
}

func call(t *testing.T, ctx context.Context, base, method, path, user string, payload any) (int, string) {
	t.Helper()
	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.Header.Set("Authorization", "Bearer "+user+":"+devToken)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func envelope(t *testing.T) sessionwire.CommandEnvelope {
	return sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: sessionwire.CommandID(newID(t))}
}

func blocks(t *testing.T, text string) json.RawMessage {
	raw, err := content.MarshalBlocks([]content.Block{&content.TextBlock{Text: text}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newID(t *testing.T) string {
	id, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}
