package main

import (
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/inferencetest"
)

// scriptedModel answers without a provider: it reasons briefly, calls
// current_time, then streams a reply that echoes the user's message. It
// repeats, so every turn of every session gets the same script.
func scriptedModel() *inferencetest.Client {
	return inferencetest.New(inferencetest.Func(func(req inference.Request) inferencetest.Step {
		if n := len(req.Messages); n > 0 {
			if _, answered := req.Messages[n-1].(*content.ToolResultMessage); answered {
				return inferencetest.Text("(scripted model) I checked the clock. You said: " + inferencetest.LastUserText(req)).
					ChunkSize(4).ChunkDelay(10 * time.Millisecond)
			}
		}
		return inferencetest.Thinking("The user wrote something; let me check the clock first.").ToolCall("current_time", `{}`)
	}).Repeat())
}
