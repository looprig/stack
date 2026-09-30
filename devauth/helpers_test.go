package devauth_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/host/department"
	"github.com/looprig/stack"
	"github.com/looprig/stack/memory"
)

// validWith is a composition Validate accepts but for its identity and logger.
func validWith(t *testing.T, id stack.Identity, logger *slog.Logger) stack.Options {
	t.Helper()
	store := memory.Open()
	t.Cleanup(func() { _ = store.Close() })
	return stack.Options{
		Storage:  store,
		Tenants:  []sessionwire.TenantID{"dev"},
		Identity: id,
		Agents: []stack.Agent{{
			ID: "a", Compatibility: "c",
			Capabilities: department.Capabilities{CaptureSafety: department.CaptureSafetyStreaming},
			Define:       func(context.Context, stack.Binding) (*rig.Rig, error) { return nil, errors.New("unused") },
		}},
		Origins: []string{"http://127.0.0.1"},
		Logger:  logger,
	}
}
