package api

import (
	"context"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

// park drives GR-1 from review into needs_you, the way a failed landing or a dead login does.
func park(t *testing.T, l *Local) {
	t.Helper()
	if _, err := l.db.SetTicketState(context.Background(), "GR-1", core.EventApprove); err != nil {
		t.Fatal(err)
	}
	if _, err := l.db.SetTicketState(context.Background(), "GR-1", core.EventLandFailed); err != nil {
		t.Fatal(err)
	}
}

// TestRequeuePutsParkedWorkBack is the way out of Needs You for a problem that was never the
// ticket's fault.
func TestRequeuePutsParkedWorkBack(t *testing.T) {
	svc, _ := atReview(t)
	park(t, svc)

	state, err := svc.Requeue(context.Background(), "GR-1")
	if err != nil {
		t.Fatal(err)
	}
	if state != core.StateReady {
		t.Fatalf("state = %q, want ready so the scheduler picks it up", state)
	}

	// And the row that sent it here is answered.
	open, err := svc.db.ListOpenAttention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range open {
		if a.TicketID == "GR-1" {
			t.Error("the attention row survived a requeue")
		}
	}
}

// A ticket that is not parked cannot be requeued: it would be a state change nobody asked for.
func TestRequeueRefusesAnUnparkedTicket(t *testing.T) {
	svc, _ := atReview(t)
	if _, err := svc.Requeue(context.Background(), "GR-1"); err == nil {
		t.Fatal("requeued a ticket that was in review, not parked")
	}
}

// TestParkedWorkWithNoRowIsStillVisible is the regression.
//
// Acknowledging a row resolved it without moving the ticket, leaving needs_you work with no open
// attention: invisible on the screen that exists to show it, and skipped by the scheduler that
// would run it. The stranded ticket was only found by reading the database.
func TestParkedWorkWithNoRowIsStillVisible(t *testing.T) {
	svc, _ := atReview(t)
	ctx := context.Background()
	park(t, svc)

	// Acknowledge: the row goes, the ticket stays parked.
	if _, err := svc.db.ResolveAttentionForTicket(ctx, "GR-1"); err != nil {
		t.Fatal(err)
	}

	st, err := svc.Status(ctx, ProjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range st.Attention {
		if item.Attention.TicketID == "GR-1" {
			found = true
			if item.Attention.Reason != core.ReasonUnexplained {
				t.Errorf("reason = %q, want it named as unexplained", item.Attention.Reason)
			}
			if item.Ticket.Title == "" {
				t.Error("the synthesised row carries no ticket, so it says nothing useful")
			}
		}
	}
	if !found {
		t.Fatal("a parked ticket with no row is invisible: not in the queue, not on the screen")
	}
}

// Once it is requeued it stops being an anomaly, rather than lingering as one.
func TestRequeuedWorkLeavesTheQueue(t *testing.T) {
	svc, _ := atReview(t)
	ctx := context.Background()
	park(t, svc)
	if _, err := svc.Requeue(ctx, "GR-1"); err != nil {
		t.Fatal(err)
	}

	st, err := svc.Status(ctx, ProjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range st.Attention {
		if item.Attention.TicketID == "GR-1" {
			t.Errorf("a requeued ticket is still listed as needing you: %+v", item.Attention.Reason)
		}
	}
}
