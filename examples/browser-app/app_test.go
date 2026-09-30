package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/fsstore"
	"github.com/looprig/inference/inferencetest"
	"github.com/looprig/inference/model"
	"github.com/looprig/stack"
	"github.com/looprig/stack/localdisk"
)

const testToken = "test-dev-token-0123456789"

// TestSessionRoundTrip boots the whole app, creates a session through the
// Factory REST API, sends input, and reads both turns back from the journal:
// Factory admits, places the session on the in-process Host, the harness
// agent calls its tool, and the journal Factory serves shows the result.
func TestSessionRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	server := boot(t, ctx, t.TempDir())

	if status, body := call(t, ctx, server.URL, http.MethodGet, "/v1/sessions", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list = %d %s, want 401", status, body)
	}

	sid := sessionwire.SessionID(newID(t))
	create := sessionwire.CreateRequest{
		CommandEnvelope: envelope(t),
		SessionID:       sid,
		AgentID:         agentID,
		Blocks:          blocks(t, "hello from the test"),
	}
	if status, body := call(t, ctx, server.URL, http.MethodPost, "/v1/sessions", "alice", create); status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	waitForJournal(t, ctx, server.URL, sid, "You said: hello from the test")

	input := sessionwire.InputRequest{CommandEnvelope: envelope(t), SessionID: sid, Blocks: blocks(t, "second message")}
	if status, body := call(t, ctx, server.URL, http.MethodPost, "/v1/sessions/"+string(sid)+"/input", "bob", input); status/100 != 2 {
		t.Fatalf("input = %d %s", status, body)
	}
	journal := waitForJournal(t, ctx, server.URL, sid, "You said: second message")
	if !strings.Contains(journal, "current_time") {
		t.Errorf("journal shows no current_time tool call:\n%s", journal)
	}

	if status, body := call(t, ctx, server.URL, http.MethodGet, "/v1/sessions", "bob", nil); status != http.StatusOK || !strings.Contains(body, string(sid)) {
		t.Fatalf("list = %d %s, want it to include %s", status, body, sid)
	}
}

// TestRestartRestoresSession stops the app and starts it again over the same
// data directory: the session is still listed, and new input is applied by a
// runtime restored from the journal, with the first turn still in history.
func TestRestartRestoresSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dir := t.TempDir()

	first := boot(t, ctx, dir)
	sid := sessionwire.SessionID(newID(t))
	create := sessionwire.CreateRequest{CommandEnvelope: envelope(t), SessionID: sid, AgentID: agentID, Blocks: blocks(t, "before restart")}
	if status, body := call(t, ctx, first.URL, http.MethodPost, "/v1/sessions", "alice", create); status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	waitForJournal(t, ctx, first.URL, sid, "You said: before restart")
	first.stop(t)

	second := boot(t, ctx, dir)
	input := sessionwire.InputRequest{CommandEnvelope: envelope(t), SessionID: sid, Blocks: blocks(t, "after restart")}
	if status, body := call(t, ctx, second.URL, http.MethodPost, "/v1/sessions/"+string(sid)+"/input", "alice", input); status/100 != 2 {
		t.Fatalf("input = %d %s", status, body)
	}
	journal := waitForJournal(t, ctx, second.URL, sid, "You said: after restart")
	if !strings.Contains(journal, "You said: before restart") {
		t.Errorf("restored journal lost the first turn:\n%s", journal)
	}
}

type running struct {
	*httptest.Server
	app  *stack.Stack
	once bool
}

// stop shuts the app down the way main does; it is also registered as cleanup.
func (r *running) stop(t *testing.T) {
	t.Helper()
	if r.once {
		return
	}
	r.once = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.app.Stop(ctx); err != nil {
		t.Errorf("Stop: %v", err)
	}
	r.Close()
}

func boot(t *testing.T, ctx context.Context, dir string) *running {
	t.Helper()
	app, _, err := Start(ctx, Config{
		DataDir:  dir,
		Tenant:   "dev",
		DevToken: testToken,
		Origins:  []string{"http://127.0.0.1"},
		Client:   scriptedModel(),
		Model:    inferencetest.Model(model.WithTools(), model.WithThinking()),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	r := &running{Server: httptest.NewServer(app.Handler()), app: app}
	t.Cleanup(func() { r.stop(t) })
	return r
}

func waitForJournal(t *testing.T, ctx context.Context, base string, sid sessionwire.SessionID, want string) string {
	t.Helper()
	var body string
	for ctx.Err() == nil {
		var status int
		status, body = call(t, ctx, base, http.MethodGet, "/v1/sessions/"+string(sid)+"/journal?limit=200", "alice", nil)
		if status == http.StatusOK && strings.Contains(body, want) {
			return body
		}
		time.Sleep(100 * time.Millisecond)
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
		req.Header.Set("Authorization", "Bearer "+user+":"+testToken)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
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

// TestLegacyDataDirIsRefused: a data directory written before fsstore v0.6.0
// is refused with a "move or delete" error, never opened or retried.
func TestLegacyDataDirIsRefused(t *testing.T) {
	dir := t.TempDir()
	// A pre-v0.6.0 fsstore KV leaf has no "@kv" suffix.
	if err := os.MkdirAll(filepath.Join(dir, "control", "kv"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "control", "kv", "old"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := Start(context.Background(), Config{
		DataDir: dir, Tenant: "dev", DevToken: testToken, Origins: []string{"http://127.0.0.1"},
		Client: scriptedModel(), Model: inferencetest.Model(model.WithTools()),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	var legacy *localdisk.LegacyDataDirError
	if !errors.Is(err, fsstore.ErrLegacyLayout) || !errors.As(err, &legacy) || legacy.Dir != dir || !strings.Contains(err.Error(), "move or delete") {
		t.Fatalf("Start = %v, want a move-or-delete ErrLegacyLayout refusal", err)
	}
}
