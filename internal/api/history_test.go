package api

import (
	"context"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

func TestHumanActionHistory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parked bool
		kind   core.ActivityKind
		act    func(*Local) error
	}{
		{"changes", false, core.KindChangesRequested, func(l *Local) error { return l.RequestChanges(context.Background(), "GR-1", "fix it") }},
		{"reject", false, core.KindRejected, func(l *Local) error { return l.Reject(context.Background(), "GR-1") }},
		{"requeue", true, core.KindRequeued, func(l *Local) error { _, err := l.Requeue(context.Background(), "GR-1"); return err }},
		{"parked changes", true, core.KindRequeued, func(l *Local) error { return l.RequestChanges(context.Background(), "GR-1", "fix it") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, db := atReview(t)
			ctx := context.Background()
			if tc.parked {
				for _, ev := range []core.Event{core.EventApprove, core.EventLandFailed} {
					if _, err := db.SetTicketState(ctx, "GR-1", ev); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := tc.act(svc); err != nil {
				t.Fatal(err)
			}
			rows, err := svc.ListHistory(ctx, "GR-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Kind != tc.kind || rows[0].Actor != core.ActorHuman {
				t.Fatalf("history = %+v", rows)
			}
		})
	}
}

func TestHistoryWriteIgnoresCancellationAndDoesNotChangeStateOrAttention(t *testing.T) {
	svc, db := atReview(t)
	ctx := context.Background()
	before, err := db.GetTicket(ctx, "GR-1")
	if err != nil {
		t.Fatal(err)
	}
	attention, err := db.ListOpenAttention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	svc.recordHuman(cancelled, "GR-1", "", core.KindRejected, nil, "a record only")
	rows, err := svc.ListHistory(ctx, "GR-1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("history = %+v, %v", rows, err)
	}
	after, err := db.GetTicket(ctx, "GR-1")
	if err != nil || after.State != before.State {
		t.Fatalf("ticket = %+v, %v", after, err)
	}
	now, err := db.ListOpenAttention(ctx)
	if err != nil || len(now) != len(attention) {
		t.Fatalf("attention = %+v, %v", now, err)
	}
	// An unserializable payload must not turn this best-effort helper into a failed action.
	svc.recordHuman(ctx, "GR-1", "", core.KindRejected, map[string]any{"bad": make(chan int)}, "cannot serialize")
	rows, err = svc.ListHistory(ctx, "GR-1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("history = %+v, %v", rows, err)
	}
}

type historyKiller struct{ kill func(string) error }

func (k historyKiller) Kill(id string) error { return k.kill(id) }

func TestKillRunRecordsTheHumanEvenAfterCancellation(t *testing.T) {
	svc, db := atReview(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := db.CreateRun(ctx, core.Run{ID: "history-run", TicketID: "GR-1", ProviderID: "fake", Model: "m", HostID: "local", State: core.StateRunning}); err != nil {
		t.Fatal(err)
	}
	svc.WithKiller(historyKiller{func(id string) error {
		if id != "GR-1" {
			t.Errorf("killed ticket = %q", id)
		}
		cancel()
		return nil
	}})
	if err := svc.KillRun(ctx, "history-run"); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.ListHistory(context.Background(), "GR-1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("history = %+v, %v", rows, err)
	}
	row := rows[0]
	if row.Kind != core.KindKilled || row.Actor != core.ActorHuman || row.RunID != "history-run" {
		t.Fatalf("kill = %+v", row)
	}
}
