package daemon

import (
	"context"
	"testing"

	"github.com/pot-roast-co/gravy/internal/agentrun"
	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// silentStore is an agentrun.Store that records nothing and publishes nothing — the behaviour
// the daemon had before PublishingStore existed.
type silentStore struct{ agentrun.Store }

func (silentStore) SetTicketState(context.Context, string, core.Event) (core.State, error) {
	return core.StateRunning, nil
}
func (silentStore) UpdateTicket(context.Context, core.Ticket) error     { return nil }
func (silentStore) CreateRun(context.Context, core.Run) error           { return nil }
func (silentStore) UpdateRun(context.Context, core.Run) error           { return nil }
func (silentStore) OpenAttention(context.Context, core.Attention) error { return nil }
func (silentStore) ResolveAttentionForTicket(context.Context, string) (int, error) {
	return 1, nil
}
func (silentStore) AddValidation(context.Context, string, string, string, int, int64, string) error {
	return nil
}
func (silentStore) SetProviderUnavailable(context.Context, core.ProviderAvailability) error {
	return nil
}
func (silentStore) AddProgress(context.Context, core.Progress) error { return nil }

type recorder struct{ events []api.Event }

func (r *recorder) Publish(e api.Event) { r.events = append(r.events, e) }

func (r *recorder) kinds() map[api.EventKind]int {
	out := map[api.EventKind]int{}
	for _, e := range r.events {
		out[e.Kind]++
	}
	return out
}

// TestDaemonWritesReachClients is the bug: a run claimed, executed, validated and parked for
// review moved nothing on a connected client, because only calls arriving through the API
// published events. The TUI is push-driven with no ticker, so it rendered the snapshot it took
// when it connected — an empty Running list for the whole length of a run.
func TestDaemonWritesReachClients(t *testing.T) {
	rec := &recorder{}
	s := WithEvents(silentStore{}, rec)
	ctx := context.Background()

	// One run's worth of writes, in the order the orchestrator makes them.
	if _, err := s.SetTicketState(ctx, "t1", core.EventAgentFinished); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, core.Run{ID: "r1", TicketID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddValidation(ctx, "v1", "r1", "test", 0, 10, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRun(ctx, core.Run{ID: "r1", TicketID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.OpenAttention(ctx, core.Attention{ID: "a1", TicketID: "t1", ProjectID: "p1"}); err != nil {
		t.Fatal(err)
	}

	kinds := rec.kinds()
	for _, want := range []api.EventKind{
		api.EventTicketChanged, api.EventRunChanged, api.EventAttentionChanged,
	} {
		if kinds[want] == 0 {
			t.Errorf("no %s event reached clients: %+v", want, rec.events)
		}
	}

	// The run appearing is what turns an empty Running screen into a populated one, so it must
	// carry the ids a client needs to re-read.
	var found bool
	for _, e := range rec.events {
		if e.Kind == api.EventRunChanged && e.RunID == "r1" && e.TicketID == "t1" {
			found = true
		}
	}
	if !found {
		t.Errorf("the run-started event does not identify the run: %+v", rec.events)
	}
}

// TestProgressWakesClients: a journal entry nobody is told about is a journal nobody reads. The
// Running screen is push-driven with no ticker, so an Activity that only changes in the database
// changes nothing on the screen it was written for.
//
// A ticket-changed event rather than a run-changed one, because the phases before an agent starts
// have no run at all — which is the entire reason the journal hangs off the ticket.
func TestProgressWakesClients(t *testing.T) {
	rec := &recorder{}
	s := WithEvents(silentStore{}, rec)

	if err := s.AddProgress(context.Background(), core.Progress{
		ID: "pg1", TicketID: "t1", Phase: core.PhaseFetch, Detail: "fetching origin",
	}); err != nil {
		t.Fatal(err)
	}

	if len(rec.events) != 1 {
		t.Fatalf("got %d events for one journal entry: %+v", len(rec.events), rec.events)
	}
	if got := rec.events[0]; got.Kind != api.EventTicketChanged || got.TicketID != "t1" {
		t.Errorf("event = %+v, want a ticket-changed event naming t1", got)
	}
}

// TestPublishingStoreToleratesNoPublisher: the store is wired before the service that publishes
// for it, so it must be usable in between.
func TestPublishingStoreToleratesNoPublisher(t *testing.T) {
	s := WithEvents(silentStore{}, nil)
	if _, err := s.SetTicketState(context.Background(), "t1", core.EventAgentFinished); err != nil {
		t.Fatalf("a store with no publisher failed: %v", err)
	}

	rec := &recorder{}
	s.SetPublisher(rec)
	if err := s.CreateRun(context.Background(), core.Run{ID: "r1", TicketID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.events) != 1 {
		t.Errorf("got %d events after attaching a publisher, want 1", len(rec.events))
	}
}

// TestResolveAttentionOnlyPublishesWhenSomethingChanged keeps a no-op from waking every client.
func TestResolveAttentionOnlyPublishesWhenSomethingChanged(t *testing.T) {
	rec := &recorder{}
	s := WithEvents(noopResolveStore{}, rec)
	if _, err := s.ResolveAttentionForTicket(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	if len(rec.events) != 0 {
		t.Errorf("resolving nothing published %+v", rec.events)
	}
}

type noopResolveStore struct{ silentStore }

func (noopResolveStore) ResolveAttentionForTicket(context.Context, string) (int, error) {
	return 0, nil
}
