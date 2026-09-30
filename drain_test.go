package stack

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/looprig/host"
)

func TestDrainErrorsExcuseOnlyAnAbandonedSessionsRefusedRelease(t *testing.T) {
	s := &Stack{logger: slog.New(slog.DiscardHandler)}
	gated := host.DrainSession{TenantID: "acme", SessionID: "gated"}
	other := host.DrainSession{TenantID: "acme", SessionID: "other"}
	parked := host.DrainSession{TenantID: "acme", SessionID: "parked"}
	failure := func(on host.DrainSession, step string) host.DrainFailure {
		return host.DrainFailure{TenantID: on.TenantID, SessionID: on.SessionID, Step: step, Err: errors.New(step)}
	}

	clean := s.drainErrors(host.DrainReport{
		Abandoned: []host.DrainSession{gated},
		Failures:  []host.DrainFailure{failure(gated, "wait_idle"), failure(gated, "release_residency")},
	})
	if len(clean) != 0 {
		t.Fatalf("an abandoned session's refused release is reported as %v", clean)
	}

	errs := s.drainErrors(host.DrainReport{
		Abandoned: []host.DrainSession{gated},
		Parked:    []host.DrainSession{parked},
		Failures: []host.DrainFailure{
			failure(gated, stepAbandonResidency), // an abandon step failing is never excused
			failure(other, "wait_idle"),          // a session that was not abandoned
		},
	})
	if len(errs) != 3 {
		t.Fatalf("drainErrors = %v, want the abandon failure, the other session's failure and the parked leak", errs)
	}
	var leak *ParkedSessionsError
	if !errors.As(errors.Join(errs...), &leak) || len(leak.Sessions) != 1 || leak.Sessions[0] != parked {
		t.Fatalf("no ParkedSessionsError naming %v in %v", parked, errs)
	}
}
