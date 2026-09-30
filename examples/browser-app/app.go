package main

// app.go is the whole composition: Factory (public API and browser realtime
// link), an in-process pooled Host running the assistant, local durable
// storage and dev-only sign-in, through github.com/looprig/stack. Compare the
// browser-app starter's app.go + runtime.go (about 600 lines) it replaces.

import (
	"context"
	"log/slog"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/host"
	"github.com/looprig/host/department"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/stack"
	"github.com/looprig/stack/devauth"
	"github.com/looprig/stack/localdisk"
)

// agentID is what a create request names; compatibilityID names this agent
// build (change it when a new build cannot replay an old journal).
const (
	agentID         sessionwire.AgentID        = "assistant"
	compatibilityID department.CompatibilityID = "browser-app/assistant/v1"
)

// Config is everything main reads from the environment.
type Config struct {
	DataDir  string
	Tenant   sessionwire.TenantID
	DevToken string // "" mints one; see devauth
	Origins  []string
	Client   inference.Client
	Model    model.Model
	Logger   *slog.Logger // required: devauth logs a banner
}

// Start runs the app. The returned stack owns the data directory until Stop.
func Start(ctx context.Context, cfg Config) (*stack.Stack, *devauth.Dev, error) {
	dev, err := devauth.New(cfg.Tenant, cfg.DevToken)
	if err != nil {
		return nil, nil, err
	}
	store, err := localdisk.Open(cfg.DataDir) // refuses a pre-v0.6.0 dir: move or delete it
	if err != nil {
		return nil, nil, err
	}
	app, err := stack.Start(ctx, stack.Options{
		Storage:  store,
		Tenants:  []sessionwire.TenantID{cfg.Tenant},
		Identity: dev.Identity(),
		Agents: []stack.Agent{{
			ID:            agentID,
			Compatibility: compatibilityID,
			// current_time returns a few bytes, so capture is trivially safe.
			Capabilities: department.Capabilities{CaptureSafety: department.CaptureSafetyStreaming},
			Define: func(_ context.Context, b stack.Binding) (*rig.Rig, error) {
				return defineRig(cfg.Client, cfg.Model, b.Journal)
			},
		}},
		// Stream text, reasoning and tool steps to every viewer.
		Live:    &host.LiveTextOptions{IncludeReasoning: true, IncludeToolSteps: true},
		UI:      dev.Routes(homePage()),
		Origins: cfg.Origins,
		Logger:  cfg.Logger,
		// devauth is development-only; this opt-in is what allows it.
		AllowDevelopmentIdentity: true,
	})
	return app, dev, err
}
