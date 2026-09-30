package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	harnessstore "github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
)

const systemPrompt = "You are a helpful assistant. Use the current_time tool when the user asks about the date or time."

// defineRig builds the one agent every session runs, journaling into store.
func defineRig(client inference.Client, selected model.Model, store *harnessstore.Store) (*rig.Rig, error) {
	// Without an access gate every tool call fails closed ("permission
	// denied"). current_time needs no capability, so a headless evaluator
	// with no bindings approves it and would deny anything that asked for
	// file, command or network access.
	access, err := gate.NewHeadlessEvaluator(nil, nil, nil)
	if err != nil {
		return nil, err
	}
	agent, err := loop.Define(
		loop.WithName("assistant"),
		loop.WithSystem(systemPrompt),
		loop.WithInference(client, selected),
		loop.WithTools(currentTimeDefinition()),
		loop.WithAccessGate(access),
		loop.WithPolicyRevision("browser-app/v1"),
	)
	if err != nil {
		return nil, err
	}
	return rig.Define(rig.WithLoops(agent), rig.WithPrimers("assistant"), rig.WithSessionStore(store))
}

// currentTime is a tool with no side effects, so it requests no capability.
type currentTime struct{}

func currentTimeDefinition() tool.Definition {
	return tool.NewDefinition("current_time", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{currentTime{}}, nil
	})
}

func (currentTime) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   "current_time",
		Desc:   "Returns the server's current date and time in UTC.",
		Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}, nil
}

// PrepareCall is mandatory under an access gate: a tool that cannot prepare
// a call is denied. An empty requirement list is approved.
func (currentTime) PrepareCall(_ context.Context, id uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{ToolName: "current_time", Summary: "read the clock", ExecutionID: id.String()}, nil, nil
}

func (currentTime) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult(time.Now().UTC().Format(time.RFC1123)), nil
}
