package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// seedProgressTicket gives the journal a ticket to hang off, since the foreign key is the point.
func seedProgressTicket(t *testing.T, db *DB, ticketID string) {
	t.Helper()
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "proj")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := db.CreateTicket(ctx, testTicket(ticketID, "p1", core.StateReady)); err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
}

func TestProgressRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedProgressTicket(t, db, "GR-1")

	at := time.Unix(1700000100, 0).UTC()
	entries := []core.Progress{
		{ID: "e1", TicketID: "GR-1", At: at, Phase: core.PhaseFetch, Detail: "fetching origin"},
		{ID: "e2", TicketID: "GR-1", RunID: "run-1", At: at.Add(time.Second),
			Phase: core.PhaseAgentStart, Detail: "fake/m started (pid 42)"},
	}
	for _, e := range entries {
		if err := db.AddProgress(ctx, e); err != nil {
			t.Fatalf("AddProgress %s: %v", e.ID, err)
		}
	}

	got, err := db.ListProgress(ctx, "GR-1")
	if err != nil {
		t.Fatalf("ListProgress: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	for i, want := range entries {
		if got[i] != want {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want)
		}
	}
}

// TestProgressKeepsTheOrderItHappenedIn is the adversarial case for the journal: a run narrates
// several phases inside one second, and timestamps are stored in whole seconds like every other
// time in this schema. Ordering by time alone would shuffle them, and a journal in the wrong
// order says something that never happened — "parked in Needs You" before the step that failed.
func TestProgressKeepsTheOrderItHappenedIn(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedProgressTicket(t, db, "GR-1")

	same := time.Unix(1700000200, 0).UTC()
	want := []string{"fetching", "worktree cut", "prompt built", "agent started", "agent exited"}
	for i, detail := range want {
		err := db.AddProgress(ctx, core.Progress{
			ID: fmt.Sprintf("e%d", i), TicketID: "GR-1", At: same,
			Phase: core.PhaseFetch, Detail: detail,
		})
		if err != nil {
			t.Fatalf("AddProgress %d: %v", i, err)
		}
	}

	got, err := db.ListProgress(ctx, "GR-1")
	if err != nil {
		t.Fatalf("ListProgress: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Detail != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i].Detail, want[i])
		}
	}

	latest, err := db.LatestProgress(ctx, "GR-1")
	if err != nil {
		t.Fatalf("LatestProgress: %v", err)
	}
	if latest.Detail != want[len(want)-1] {
		t.Errorf("latest = %q, want %q", latest.Detail, want[len(want)-1])
	}
}

func TestLatestProgressWithoutAJournal(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedProgressTicket(t, db, "GR-1")

	if _, err := db.LatestProgress(ctx, "GR-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestProgress error = %v, want ErrNotFound", err)
	}
	got, err := db.ListProgress(ctx, "GR-1")
	if err != nil || len(got) != 0 {
		t.Fatalf("ListProgress = %v, %v; want empty and no error", got, err)
	}
}

// TestProgressSurvivesARestart is the acceptance criterion for durability: the journal is what a
// human reads to understand a run they came back to in the morning, and a daemon restart is the
// ordinary thing that happens in between.
func TestProgressSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gravy.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seedProgressTicket(t, db, "GR-1")
	if err := db.AddProgress(ctx, core.Progress{
		ID: "e1", TicketID: "GR-1", RunID: "run-1", At: time.Unix(1700000300, 0).UTC(),
		Phase: core.PhaseValidationStep, Detail: "test passed in 1.2s (exit 0)",
	}); err != nil {
		t.Fatalf("AddProgress: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })

	got, err := reopened.ListProgress(ctx, "GR-1")
	if err != nil {
		t.Fatalf("ListProgress after restart: %v", err)
	}
	if len(got) != 1 || got[0].Detail != "test passed in 1.2s (exit 0)" {
		t.Fatalf("journal after restart = %+v, want the entry written before it", got)
	}
	if got[0].RunID != "run-1" || got[0].Phase != core.PhaseValidationStep {
		t.Errorf("entry lost its run or phase: %+v", got[0])
	}
}

// TestProgressGoesWithItsTicket: the journal is the ticket's, so deleting the ticket takes it.
// Without the cascade, deleting a project would leave orphaned rows nothing can reach.
func TestProgressGoesWithItsTicket(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedProgressTicket(t, db, "GR-1")

	if err := db.AddProgress(ctx, core.Progress{
		ID: "e1", TicketID: "GR-1", At: time.Unix(1700000400, 0).UTC(),
		Phase: core.PhaseFetch, Detail: "fetching origin",
	}); err != nil {
		t.Fatalf("AddProgress: %v", err)
	}
	if err := db.DeleteTicket(ctx, "GR-1"); err != nil {
		t.Fatalf("DeleteTicket: %v", err)
	}

	got, err := db.ListProgress(ctx, "GR-1")
	if err != nil {
		t.Fatalf("ListProgress: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("journal outlived its ticket: %+v", got)
	}
}

func TestAddProgressWithoutAnID(t *testing.T) {
	db := openTest(t)
	seedProgressTicket(t, db, "GR-1")

	err := db.AddProgress(context.Background(), core.Progress{
		TicketID: "GR-1", At: time.Now(), Phase: core.PhaseFetch, Detail: "fetching origin",
	})
	if err == nil {
		t.Fatal("AddProgress accepted an entry with no id")
	}
}
