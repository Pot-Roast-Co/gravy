package api

import (
	"context"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

// parkingLander stands for a landing that re-validated, failed, and parked the ticket. It
// returns no error, because parking is not one.
type parkingLander struct{ state core.State }

func (l parkingLander) Approve(context.Context, string, core.Approval) (core.State, error) {
	return l.state, nil
}

func (l parkingLander) Continue(context.Context, string, core.Approval) (core.State, error) {
	return l.state, nil
}

// TestApproveReportsParkedRatherThanNil is the regression at the API seam.
//
// Approve used to return only an error. A landing that parks returns none, so every caller —
// the TUI, the CLI — was told the approval succeeded and concluded the work had merged. The
// state is the only thing that distinguishes "merged" from "sitting in Needs You with a failed
// validation", and it was being thrown away here.
func TestApproveReportsParkedRatherThanNil(t *testing.T) {
	svc, _ := atReview(t)
	svc = svc.WithLander(parkingLander{state: core.StateNeedsYou})

	state, err := svc.Approve(context.Background(), "GR-1", core.ApprovePush)
	if err != nil {
		t.Fatalf("parking is not an error: %v", err)
	}
	if state != core.StateNeedsYou {
		t.Fatalf("Approve returned %q; a caller cannot tell this from a merge", state)
	}
}

func TestApproveReportsDoneWhenItLands(t *testing.T) {
	svc, _ := atReview(t)
	svc = svc.WithLander(parkingLander{state: core.StateDone})

	state, err := svc.Approve(context.Background(), "GR-1", core.ApprovePush)
	if err != nil {
		t.Fatal(err)
	}
	if state != core.StateDone {
		t.Fatalf("Approve returned %q, want done", state)
	}
}

// Continue carries the same answer: a retry can park again.
func TestContinueReportsParked(t *testing.T) {
	svc, _ := atReview(t)
	svc = svc.WithLander(parkingLander{state: core.StateNeedsYou})

	state, err := svc.Continue(context.Background(), "GR-1", core.ApprovePush)
	if err != nil {
		t.Fatal(err)
	}
	if state != core.StateNeedsYou {
		t.Fatalf("Continue returned %q, want needs_you", state)
	}
}
