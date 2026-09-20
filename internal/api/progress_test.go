package api

import (
	"context"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/store"
)

// assigned drives a ticket to Assigned, which is the state the journal was built for: the fetch,
// the worktree and the first prompt build all happen there, with no run row to hang them from.
func assigned(t *testing.T, db *store.DB, ticketID string) {
	t.Helper()
	ctx := context.Background()
	if err := db.CreateTicket(ctx, core.Ticket{
		ID: ticketID, ProjectID: "p1", Title: "the one in flight",
		State: core.StateBacklog, Route: core.RouteImplementation, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []core.Event{core.EventMarkReady, core.EventAssign} {
		if _, err := db.SetTicketState(ctx, ticketID, ev); err != nil {
			t.Fatalf("advance with %s: %v", ev, err)
		}
	}
}

// runningActivity finds the Running row for a ticket.
func runningActivity(t *testing.T, st SystemStatus, ticketID string) string {
	t.Helper()
	for _, rt := range st.Running {
		if rt.Ticket.ID == ticketID {
			return rt.Activity
		}
	}
	t.Fatalf("%s is not in Running: %+v", ticketID, st.Running)
	return ""
}

// TestActivityIsTheNewestJournalEntry is the ticket's dashboard criterion: a ticket in Assigned
// says "fetching origin" within one refresh rather than the state name it shares with every
// other phase before the agent starts.
func TestActivityIsTheNewestJournalEntry(t *testing.T) {
	svc, db := atReview(t)
	ctx := context.Background()
	assigned(t, db, "GR-2")

	// Before anything is narrated, the state-name mapping is all there is — and it must still
	// be there, because a ticket assigned a moment ago has an empty journal, not a broken one.
	st, err := svc.Status(ctx, ProjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := runningActivity(t, st, "GR-2"); got != activityFor(core.StateAssigned) {
		t.Errorf("activity with an empty journal = %q, want the state-name fallback %q",
			got, activityFor(core.StateAssigned))
	}

	const fetching = "fetching origin so the work starts on top of the latest main"
	if err := db.AddProgress(ctx, core.Progress{
		ID: "pg1", TicketID: "GR-2", At: time.Unix(1700000600, 0).UTC(),
		Phase: core.PhaseFetch, Detail: fetching,
	}); err != nil {
		t.Fatal(err)
	}

	st, err = svc.Status(ctx, ProjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := runningActivity(t, st, "GR-2"); got != fetching {
		t.Errorf("activity = %q, want the fetch sentence %q", got, fetching)
	}

	// And it is the newest entry, not the first: the Running screen says what Gravy is doing
	// now, which is the entire reason the journal beats the state name.
	const building = "prompt built for attempt 1 of 3: about 1200 tokens"
	if err := db.AddProgress(ctx, core.Progress{
		ID: "pg2", TicketID: "GR-2", At: time.Unix(1700000601, 0).UTC(),
		Phase: core.PhasePrompt, Detail: building,
	}); err != nil {
		t.Fatal(err)
	}
	st, err = svc.Status(ctx, ProjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := runningActivity(t, st, "GR-2"); got != building {
		t.Errorf("activity = %q, want the newest entry %q", got, building)
	}
}

// TestListProgressReturnsTheJournalOldestFirst: a journal read newest-first reads as a run
// played backwards, and the phases only mean anything in the order they happened.
func TestListProgressReturnsTheJournalOldestFirst(t *testing.T) {
	svc, db := atReview(t)
	ctx := context.Background()

	details := []string{"fetching origin", "worktree cut", "fake/m started (pid 42)"}
	for i, d := range details {
		if err := db.AddProgress(ctx, core.Progress{
			ID:       string(rune('a' + i)),
			TicketID: "GR-1",
			At:       time.Unix(1700000700, 0).Add(time.Duration(i) * time.Second).UTC(),
			Phase:    core.PhaseFetch,
			Detail:   d,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := svc.ListProgress(ctx, "GR-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(details) {
		t.Fatalf("got %d entries, want %d", len(got), len(details))
	}
	for i, want := range details {
		if got[i].Detail != want {
			t.Errorf("entry %d = %q, want %q", i, got[i].Detail, want)
		}
	}
}
