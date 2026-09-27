package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// seedHistoryTicket gives the journal a ticket to hang off, since the foreign key is the point.
func seedHistoryTicket(t *testing.T, db *DB, ticketID string) {
	t.Helper()
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "proj")); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := db.CreateTicket(ctx, testTicket(ticketID, "p1", core.StateReady)); err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
}

func TestActivityRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedHistoryTicket(t, db, "GR-1")

	at := time.Unix(1700000100, 0).UTC()
	entries := []core.Activity{
		{ID: "e1", TicketID: "GR-1", At: at, Kind: core.KindFetch, Actor: core.ActorGravy,
			Detail: "fetching origin", Payload: map[string]any{}},
		{ID: "e2", TicketID: "GR-1", RunID: "run-1", At: at.Add(time.Second),
			Kind: core.KindAgentStart, Actor: core.AgentActor("fake", "m"),
			Detail: "fake/m started (pid 42)",
			// Numbers come back as float64: the payload is JSON, and a reader is a program that
			// has to cope with that whichever side of the wire it is on.
			Payload: map[string]any{"provider": "fake", "model": "m", "attempt": float64(1)}},
		{ID: "e3", TicketID: "GR-1", At: at.Add(2 * time.Second), Kind: core.KindApproved,
			Actor: core.ActorHuman, Detail: "approved: push",
			Payload: map[string]any{"approval": "push"}},
	}
	for _, e := range entries {
		if err := db.AddActivity(ctx, e); err != nil {
			t.Fatalf("AddActivity %s: %v", e.ID, err)
		}
	}

	got, err := db.ListHistory(ctx, "GR-1")
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("got %d entries, want %d", len(got), len(entries))
	}
	for i, want := range entries {
		if !reflect.DeepEqual(got[i], want) {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want)
		}
	}
}

// TestAnEntryWithNoActorIsGravys: before the history had actors, only the orchestrator wrote to
// it. An entry written without one is attributed the same way, so nothing reads back with an
// actor outside the grammar.
func TestAnEntryWithNoActorIsGravys(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedHistoryTicket(t, db, "GR-1")

	if err := db.AddActivity(ctx, core.Activity{
		ID: "e1", TicketID: "GR-1", At: time.Unix(1700000100, 0), Kind: core.KindFetch, Detail: "x",
	}); err != nil {
		t.Fatalf("AddActivity: %v", err)
	}
	got, err := db.LatestActivity(ctx, "GR-1")
	if err != nil {
		t.Fatalf("LatestActivity: %v", err)
	}
	if got.Actor != core.ActorGravy {
		t.Errorf("actor = %q, want gravy", got.Actor)
	}
	if got.Payload == nil || len(got.Payload) != 0 {
		t.Errorf("payload = %#v, want an empty map", got.Payload)
	}
}

// TestJournalRowsReadAsHistory is the migration's adversarial case: a row the progress journal
// wrote before 0012 has neither an actor nor a payload, and must still read — as Gravy's, with
// its phase as its kind — rather than failing the whole ticket's history.
func TestJournalRowsReadAsHistory(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedHistoryTicket(t, db, "GR-1")

	// The columns 0011 knew about, and nothing else, exactly as the journal used to write them.
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO ticket_progress (id, ticket_id, run_id, at, phase, detail) VALUES (?,?,?,?,?,?)`,
		"old", "GR-1", "run-1", int64(1700000000), "validation_step", "test passed in 1s (exit 0)",
	); err != nil {
		t.Fatalf("insert a journal row: %v", err)
	}

	got, err := db.ListHistory(ctx, "GR-1")
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	e := got[0]
	if e.Kind != core.KindValidationStep || e.Actor != core.ActorGravy || e.RunID != "run-1" {
		t.Errorf("journal row read as %+v", e)
	}
	if e.Payload == nil || len(e.Payload) != 0 {
		t.Errorf("payload = %#v, want an empty map", e.Payload)
	}
}

// TestHistoryKeepsTheOrderItHappenedIn is the adversarial case for the journal: a run narrates
// several phases inside one second, and timestamps are stored in whole seconds like every other
// time in this schema. Ordering by time alone would shuffle them, and a journal in the wrong
// order says something that never happened — "parked in Needs You" before the step that failed.
func TestHistoryKeepsTheOrderItHappenedIn(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedHistoryTicket(t, db, "GR-1")

	same := time.Unix(1700000200, 0).UTC()
	want := []string{"fetching", "worktree cut", "prompt built", "agent started", "agent exited"}
	for i, detail := range want {
		err := db.AddActivity(ctx, core.Activity{
			ID: fmt.Sprintf("e%d", i), TicketID: "GR-1", At: same,
			Kind: core.KindFetch, Detail: detail,
		})
		if err != nil {
			t.Fatalf("AddActivity %d: %v", i, err)
		}
	}

	got, err := db.ListHistory(ctx, "GR-1")
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Detail != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i].Detail, want[i])
		}
	}

	latest, err := db.LatestActivity(ctx, "GR-1")
	if err != nil {
		t.Fatalf("LatestActivity: %v", err)
	}
	if latest.Detail != want[len(want)-1] {
		t.Errorf("latest = %q, want %q", latest.Detail, want[len(want)-1])
	}
}

func TestLatestActivityWithoutAHistory(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedHistoryTicket(t, db, "GR-1")

	if _, err := db.LatestActivity(ctx, "GR-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestActivity error = %v, want ErrNotFound", err)
	}
	got, err := db.ListHistory(ctx, "GR-1")
	if err != nil || len(got) != 0 {
		t.Fatalf("ListHistory = %v, %v; want empty and no error", got, err)
	}
}

// TestHistorySurvivesARestart is the acceptance criterion for durability: the journal is what a
// human reads to understand a run they came back to in the morning, and a daemon restart is the
// ordinary thing that happens in between.
func TestHistorySurvivesARestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gravy.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seedHistoryTicket(t, db, "GR-1")
	if err := db.AddActivity(ctx, core.Activity{
		ID: "e1", TicketID: "GR-1", RunID: "run-1", At: time.Unix(1700000300, 0).UTC(),
		Kind: core.KindValidationStep, Detail: "test passed in 1.2s (exit 0)",
	}); err != nil {
		t.Fatalf("AddActivity: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })

	got, err := reopened.ListHistory(ctx, "GR-1")
	if err != nil {
		t.Fatalf("ListHistory after restart: %v", err)
	}
	if len(got) != 1 || got[0].Detail != "test passed in 1.2s (exit 0)" {
		t.Fatalf("journal after restart = %+v, want the entry written before it", got)
	}
	if got[0].RunID != "run-1" || got[0].Kind != core.KindValidationStep {
		t.Errorf("entry lost its run or phase: %+v", got[0])
	}
}

// TestHistoryGoesWithItsTicket: the journal is the ticket's, so deleting the ticket takes it.
// Without the cascade, deleting a project would leave orphaned rows nothing can reach.
func TestHistoryGoesWithItsTicket(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedHistoryTicket(t, db, "GR-1")

	if err := db.AddActivity(ctx, core.Activity{
		ID: "e1", TicketID: "GR-1", At: time.Unix(1700000400, 0).UTC(),
		Kind: core.KindFetch, Detail: "fetching origin",
	}); err != nil {
		t.Fatalf("AddActivity: %v", err)
	}
	if err := db.DeleteTicket(ctx, "GR-1"); err != nil {
		t.Fatalf("DeleteTicket: %v", err)
	}

	got, err := db.ListHistory(ctx, "GR-1")
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("journal outlived its ticket: %+v", got)
	}
}

func TestAddActivityWithoutAnID(t *testing.T) {
	db := openTest(t)
	seedHistoryTicket(t, db, "GR-1")

	err := db.AddActivity(context.Background(), core.Activity{
		TicketID: "GR-1", At: time.Now(), Kind: core.KindFetch, Detail: "fetching origin",
	})
	if err == nil {
		t.Fatal("AddActivity accepted an entry with no id")
	}
}

// Landing links history to the last attempt, including retries started in the same second.
func TestHistoryLandingRunOrder(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedHistoryTicket(t, db, "GR-1")
	at := time.Unix(1700000100, 0)
	for _, id := range []string{"first", "second"} {
		if err := db.CreateRun(ctx, core.Run{ID: id, TicketID: "GR-1", State: core.StateRunning, StartedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := db.ListRunsForTicket(ctx, "GR-1")
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if runs[0].ID != "second" {
		t.Fatalf("latest run = %s, want second", runs[0].ID)
	}
}
