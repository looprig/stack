package stack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	centrifugego "github.com/centrifugal/centrifuge-go"
	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/host"
	"github.com/looprig/host/department"
	"github.com/looprig/inference"
	"github.com/looprig/inference/inferencetest"
	"github.com/looprig/inference/model"
	"github.com/looprig/stack"
	"github.com/looprig/stack/memory"
	"github.com/looprig/storage"
)

// toolAgent is an agent whose one loop runs tools under a headless access
// gate (a tool that requests no capability is approved) and journals into the
// binding's store.
func toolAgent(client inference.Client, tools ...tool.Definition) stack.Agent {
	return stack.Agent{
		ID:            agentID,
		Compatibility: "stack-test/tools/v1",
		Capabilities:  department.Capabilities{CaptureSafety: department.CaptureSafetyStreaming},
		Define: func(_ context.Context, b stack.Binding) (*rig.Rig, error) {
			access, err := gate.NewHeadlessEvaluator(nil, nil, nil)
			if err != nil {
				return nil, err
			}
			agent, err := loop.Define(
				loop.WithName("worker"),
				loop.WithSystem("Use your tools."),
				loop.WithInference(client, inferencetest.Model(model.WithTools(), model.WithThinking())),
				loop.WithTools(tools...),
				loop.WithAccessGate(access),
				loop.WithPolicyRevision("stack-test/tools/v1"),
			)
			if err != nil {
				return nil, err
			}
			return rig.Define(rig.WithLoops(agent), rig.WithPrimers("worker"), rig.WithSessionStore(b.Journal))
		},
	}
}

// scriptedTool is a tool with no capability requirement. run is its body.
type scriptedTool struct {
	name string
	run  func(context.Context) (string, error)
}

func (s scriptedTool) definition() tool.Definition {
	return tool.NewDefinition(s.name, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{s}, nil
	})
}

func (s scriptedTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: s.name, Desc: "A scripted test tool.",
		Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}, nil
}

// PrepareCall is mandatory under an access gate; an empty requirement list
// is approved.
func (s scriptedTool) PrepareCall(_ context.Context, id uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{ToolName: s.name, Summary: "run " + s.name, ExecutionID: id.String(),
		ExpiresAtUnixMilli: time.Now().Add(time.Hour).UnixMilli()}, nil, nil
}

func (s scriptedTool) AuditSummary(string) string { return "run " + s.name }

func (s scriptedTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	text, err := s.run(ctx)
	if err != nil {
		return nil, err
	}
	return tool.TextResult(text), nil
}

func lastIsToolResult(req inference.Request) bool {
	n := len(req.Messages)
	if n == 0 {
		return false
	}
	_, ok := req.Messages[n-1].(*content.ToolResultMessage)
	return ok
}

const gateAnswer = "ULTRAMARINE"

// TestGateResponseSettlesThroughTheStack: an agent's tool raises an ask_user
// gate; the gate reaches Factory's gates read; the answer, posted to
// Factory's REST gate route, is admitted only because the in-process Host
// advertises gate_response, is applied by the Host, settles applied, and
// reaches the tool, after which the agent finishes its turn.
func TestGateResponseSettlesThroughTheStack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var (
		mu      sync.Mutex
		answers []string
	)
	ask := scriptedTool{name: "ask", run: func(ctx context.Context) (string, error) {
		answer, err := loop.RequestUserInput(ctx, "which colour?", nil)
		if err != nil {
			return "", err
		}
		mu.Lock()
		answers = append(answers, answer)
		mu.Unlock()
		return "the user answered: " + answer, nil
	}}
	client := inferencetest.New(inferencetest.Func(func(req inference.Request) inferencetest.Step {
		if lastIsToolResult(req) {
			return inferencetest.Text("thank you")
		}
		return inferencetest.ToolCall("ask", `{}`)
	}).Repeat())

	o := options(t, memory.Open(), client)
	o.Agents = []stack.Agent{toolAgent(client, ask.definition())}
	app := start(t, ctx, o)

	sid := sessionwire.SessionID(newID(t))
	create := sessionwire.CreateRequest{CommandEnvelope: envelope(t), SessionID: sid, AgentID: agentID, Blocks: blocks(t, "ask me")}
	if status, body := call(t, ctx, app.URL, http.MethodPost, "/v1/sessions", "alice", create); status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}

	var projected sessionwire.GateProjection
	waitFor(t, ctx, "the gate at Factory's gates read", func() bool {
		status, body := call(t, ctx, app.URL, http.MethodGet, "/v1/sessions/"+string(sid)+"/gates", "alice", nil)
		var page sessionwire.GatePage
		if status != http.StatusOK || page.UnmarshalJSON([]byte(body)) != nil || len(page.Gates) == 0 {
			return false
		}
		projected = page.Gates[0]
		return true
	})
	if projected.Answerability != sessionwire.GateAnswerabilityResident || !strings.Contains(projected.Prompt.Title+projected.Prompt.Body, "which colour?") {
		t.Fatalf("projected gate = %+v, want a resident gate carrying the agent's question", projected)
	}

	answer := envelope(t)
	status, body := call(t, ctx, app.URL, http.MethodPost, "/v1/sessions/"+string(sid)+"/gates/"+string(projected.GateID), "alice",
		sessionwire.GateResponseRequest{
			CommandEnvelope:        answer,
			SessionID:              sid,
			GateID:                 projected.GateID,
			Action:                 "answer",
			Values:                 map[string]json.RawMessage{"answer": json.RawMessage(`"` + gateAnswer + `"`)},
			ExpectedOpenJournalSeq: projected.OpenedJournalSeq,
		})
	if status != http.StatusAccepted {
		t.Fatalf("gate response = %d %s (409 gate_not_resumable means the Host did not advertise gate_response)", status, body)
	}
	waitFor(t, ctx, "the gate response settled applied", func() bool {
		status, body := call(t, ctx, app.URL, http.MethodGet, "/v1/sessions/"+string(sid)+"/commands/"+string(answer.CommandID), "alice", nil)
		return status == http.StatusOK && strings.Contains(body, `"status":"applied"`)
	})
	journal := waitForJournal(t, ctx, app.URL, sid, "thank you")
	for _, want := range []string{"GateResolved", "the user answered: " + gateAnswer} {
		if !strings.Contains(journal, want) {
			t.Errorf("journal lacks %q:\n%s", want, journal)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(answers) != 1 || answers[0] != gateAnswer {
		t.Fatalf("the tool received %v, want exactly [%s]", answers, gateAnswer)
	}
}

// slowLedger delays every journal append so each live preview has a clear
// lead over the committed frame that follows it (a preview still being
// projected when its StepDone arrives is superseded by design).
type slowLedger struct {
	storage.Ledger
	delay time.Duration
}

func (l slowLedger) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	select {
	case <-time.After(l.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return l.Ledger.Append(ctx, name, expected, payload)
}

// liveFrame is one ClientLink publication, classified.
type liveFrame struct {
	kind string // "text", "reasoning", "ToolCallStarted", "ToolCallCompleted", "StepDone", or ""
	body json.RawMessage
}

func classify(t *testing.T, frame []byte) liveFrame {
	t.Helper()
	kind, err := sessionwire.SessionRecordTypeOf(frame)
	if err != nil {
		return liveFrame{}
	}
	var body json.RawMessage
	switch kind {
	case sessionwire.SessionRecordTypeEphemeralPublication:
		var p sessionwire.EphemeralPublication
		if err := p.UnmarshalJSON(frame); err != nil {
			t.Fatalf("ephemeral frame: %v: %s", err, frame)
		}
		body = p.Body
	case sessionwire.SessionRecordTypeEnduringPublication:
		var p sessionwire.EnduringPublication
		if err := p.UnmarshalJSON(frame); err != nil {
			t.Fatalf("enduring frame: %v: %s", err, frame)
		}
		body = p.Body
	default:
		return liveFrame{}
	}
	var head struct {
		Type  string `json:"type"`
		Chunk struct {
			ChunkType string `json:"chunk_type"`
		} `json:"chunk"`
	}
	if json.Unmarshal(body, &head) != nil {
		return liveFrame{}
	}
	switch {
	case kind == sessionwire.SessionRecordTypeEphemeralPublication && head.Type == "TokenDelta" && head.Chunk.ChunkType == "text":
		return liveFrame{kind: "text", body: body}
	case kind == sessionwire.SessionRecordTypeEphemeralPublication && head.Type == "TokenDelta" && head.Chunk.ChunkType == "thinking":
		return liveFrame{kind: "reasoning", body: body}
	case kind == sessionwire.SessionRecordTypeEphemeralPublication && (head.Type == "ToolCallStarted" || head.Type == "ToolCallCompleted"):
		return liveFrame{kind: head.Type, body: body}
	case kind == sessionwire.SessionRecordTypeEnduringPublication && head.Type == "StepDone":
		return liveFrame{kind: "StepDone", body: body}
	}
	return liveFrame{}
}

// viewer is a browser-shaped ClientLink connection that records every
// publication on one session channel.
type viewer struct {
	mu     sync.Mutex
	frames [][]byte
}

func (v *viewer) classified(t *testing.T) []liveFrame {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]liveFrame, 0, len(v.frames))
	for _, frame := range v.frames {
		if f := classify(t, frame); f.kind != "" {
			out = append(out, f)
		}
	}
	return out
}

func watch(t *testing.T, ctx context.Context, base, user string, s sessionwire.SessionID) *viewer {
	t.Helper()
	client := centrifugego.NewJsonClient("ws"+strings.TrimPrefix(base, "http")+"/v1/realtime", centrifugego.Config{
		Token:            user + ":" + devToken,
		Data:             []byte(`{"protocol_version":"1"}`),
		Header:           http.Header{"Authorization": {"Bearer " + user + ":" + devToken}, "Origin": {base}},
		HandshakeTimeout: 10 * time.Second,
		LogLevel:         centrifugego.LogLevelNone,
	})
	t.Cleanup(client.Close)
	client.OnError(func(e centrifugego.ErrorEvent) { t.Logf("ClientLink error: %v", e.Error) })
	client.OnDisconnected(func(e centrifugego.DisconnectedEvent) { t.Logf("ClientLink disconnected: %d %s", e.Code, e.Reason) })
	if err := client.Connect(); err != nil {
		t.Fatalf("ClientLink connect: %v", err)
	}
	v := &viewer{}
	// Factory's ClientLink channel for a session.
	sub, err := client.NewSubscription("session:" + string(tenant) + ":" + string(s))
	if err != nil {
		t.Fatal(err)
	}
	subscribed := make(chan error, 1)
	sub.OnSubscribed(func(centrifugego.SubscribedEvent) { subscribed <- nil })
	sub.OnError(func(e centrifugego.SubscriptionErrorEvent) {
		select {
		case subscribed <- e.Error:
		default:
		}
	})
	sub.OnPublication(func(e centrifugego.PublicationEvent) {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.frames = append(v.frames, bytes.Clone(e.Data))
	})
	if err := sub.Subscribe(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-subscribed:
		if err != nil {
			t.Fatalf("ClientLink subscribe refused: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("ClientLink subscribe was never answered")
	}
	return v
}

// TestLivePreviewsReachAClientLinkViewer: with Live{IncludeReasoning,
// IncludeToolSteps}, a ClientLink viewer of the stack's Handler receives the
// turn's reasoning and tool-step previews before the tool step's committed
// StepDone, and its text previews before the reply's StepDone.
func TestLivePreviewsReachAClientLinkViewer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	probe := scriptedTool{name: "probe", run: func(context.Context) (string, error) {
		time.Sleep(20 * time.Millisecond)
		return "probe result", nil
	}}
	client := inferencetest.New(inferencetest.Func(func(req inference.Request) inferencetest.Step {
		switch {
		case lastIsToolResult(req):
			return inferencetest.Text("the probe says all is well").ChunkSize(4).ChunkDelay(15 * time.Millisecond)
		case inferencetest.LastUserText(req) == "go":
			return inferencetest.Thinking("pondering which probe to run").ChunkSize(4).ChunkDelay(15*time.Millisecond).ToolCall("probe", `{}`)
		default:
			return inferencetest.Text("ready")
		}
	}).Repeat())

	backend := memory.New()
	t.Cleanup(func() { _ = backend.Close() })
	store := backend.Storage()
	journal := store.Journal
	store.Journal = func(tenant sessionwire.TenantID) (*storage.Composite, error) {
		composite, err := journal(tenant)
		if err != nil {
			return nil, err
		}
		slowed := *composite
		slowed.Ledger = slowLedger{Ledger: composite.Ledger, delay: 25 * time.Millisecond}
		return &slowed, nil
	}

	// The viewer's Origin must be trusted, so learn the address first.
	server := httptest.NewUnstartedServer(nil)
	base := "http://" + server.Listener.Addr().String()
	o := options(t, store, client)
	o.Agents = []stack.Agent{toolAgent(client, probe.definition())}
	o.Origins = []string{base}
	o.Live = &host.LiveTextOptions{IncludeReasoning: true, IncludeToolSteps: true}
	s, err := stack.Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = s.Handler()
	server.Start()
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		if err := s.Stop(stopCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
		server.Close()
	})

	// Turn one makes the session resident; once the viewer has received its
	// committed StepDone, Factory's live tail for the session is bound.
	sid := sessionwire.SessionID(newID(t))
	v := watch(t, ctx, base, "alice", sid)
	create := sessionwire.CreateRequest{CommandEnvelope: envelope(t), SessionID: sid, AgentID: agentID, Blocks: blocks(t, "ready?")}
	if status, body := call(t, ctx, base, http.MethodPost, "/v1/sessions", "alice", create); status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	waitFor(t, ctx, "turn one's StepDone at the viewer", func() bool { return count(v.classified(t), "StepDone") >= 1 })
	before := len(v.classified(t))

	input := sessionwire.InputRequest{CommandEnvelope: envelope(t), SessionID: sid, Blocks: blocks(t, "go")}
	if status, body := call(t, ctx, base, http.MethodPost, "/v1/sessions/"+string(sid)+"/input", "alice", input); status/100 != 2 {
		t.Fatalf("input = %d %s", status, body)
	}
	waitFor(t, ctx, "turn two's two StepDones at the viewer", func() bool {
		return count(v.classified(t)[before:], "StepDone") >= 2
	})
	frames := v.classified(t)[before:]
	var order []string
	for _, f := range frames {
		order = append(order, f.kind)
	}
	t.Logf("turn two at the viewer: %v", order)

	firstStep := index(frames, "StepDone", 0)
	secondStep := index(frames, "StepDone", firstStep+1)
	for _, want := range []string{"reasoning", "ToolCallStarted", "ToolCallCompleted"} {
		if at := index(frames, want, 0); at < 0 || at > firstStep {
			t.Errorf("%s preview at %d, want one before the tool step's StepDone at %d", want, at, firstStep)
		}
	}
	if started, completed := index(frames, "ToolCallStarted", 0), index(frames, "ToolCallCompleted", 0); started > completed {
		t.Errorf("ToolCallStarted at %d after ToolCallCompleted at %d", started, completed)
	}
	if at := index(frames, "text", firstStep+1); at < 0 || at > secondStep {
		t.Errorf("text preview at %d, want one between the tool StepDone (%d) and the reply's StepDone (%d)", at, firstStep, secondStep)
	}
	var started struct {
		ToolName  string `json:"tool_name"`
		Summary   string `json:"summary"`
		ToolUseID string `json:"tool_use_id"`
	}
	if at := index(frames, "ToolCallStarted", 0); at >= 0 {
		if err := json.Unmarshal(frames[at].body, &started); err != nil || started.ToolName != "probe" || started.Summary != "run probe" || started.ToolUseID == "" {
			t.Errorf("ToolCallStarted = %s, want tool probe with its audit summary and a tool_use_id", frames[at].body)
		}
	}
}

func count(frames []liveFrame, kind string) int {
	n := 0
	for _, f := range frames {
		if f.kind == kind {
			n++
		}
	}
	return n
}

// index is the first frame of kind at or after from, or -1.
func index(frames []liveFrame, kind string, from int) int {
	for i := max(from, 0); i < len(frames); i++ {
		if frames[i].kind == kind {
			return i
		}
	}
	return -1
}

func waitFor(t *testing.T, ctx context.Context, what string, done func() bool) {
	t.Helper()
	for ctx.Err() == nil {
		if done() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
