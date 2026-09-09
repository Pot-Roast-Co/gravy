package daemon

import (
	"context"

	"github.com/pot-roast-co/gravy/internal/agentrun"
	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// Publisher receives events for connected clients.
type Publisher interface {
	Publish(e api.Event)
}

// PublishingStore announces every write the orchestrator makes.
//
// Without it, only calls arriving through the API published anything — and the scheduler and the
// orchestrator write straight to the database. A run that was claimed, executed, validated and
// parked for review therefore moved nothing on a connected client: the TUI is push-driven with
// no ticker, so it went on rendering the snapshot it took when it connected, showing an empty
// Running list for the whole length of a run.
//
// It wraps rather than threading a publisher through the orchestrator's call sites, because a
// transition that forgets to publish is invisible in exactly the same way — and there is no test
// that fails when someone adds the thirteenth one.
type PublishingStore struct {
	// Store is embedded, so reads pass straight through and a method added to the interface
	// keeps compiling here rather than silently losing its event.
	agentrun.Store
	pub Publisher
}

// WithEvents wraps a store so its writes reach connected clients.
func WithEvents(s agentrun.Store, p Publisher) *PublishingStore {
	return &PublishingStore{Store: s, pub: p}
}

// SetPublisher attaches the publisher after construction.
//
// The service that publishes is built from the same store the orchestrator writes through, so
// one of the two has to be wired second.
func (s *PublishingStore) SetPublisher(p Publisher) { s.pub = p }

// publish tolerates a store wired before the service that publishes for it.
func (s *PublishingStore) publish(e api.Event) {
	if s.pub != nil {
		s.pub.Publish(e)
	}
}

func (s *PublishingStore) SetTicketState(ctx context.Context, id string, ev core.Event) (core.State, error) {
	state, err := s.Store.SetTicketState(ctx, id, ev)
	if err == nil {
		s.publish(api.Event{Kind: api.EventTicketChanged, TicketID: id, State: state})
	}
	return state, err
}

func (s *PublishingStore) UpdateTicket(ctx context.Context, t core.Ticket) error {
	err := s.Store.UpdateTicket(ctx, t)
	if err == nil {
		s.publish(api.Event{
			Kind: api.EventTicketChanged, ProjectID: t.ProjectID, TicketID: t.ID, State: t.State,
		})
	}
	return err
}

func (s *PublishingStore) CreateRun(ctx context.Context, r core.Run) error {
	err := s.Store.CreateRun(ctx, r)
	if err == nil {
		// The event a client waits for: a run appearing is what turns an empty Running screen
		// into a populated one.
		s.publish(api.Event{Kind: api.EventRunChanged, TicketID: r.TicketID, RunID: r.ID})
	}
	return err
}

func (s *PublishingStore) UpdateRun(ctx context.Context, r core.Run) error {
	err := s.Store.UpdateRun(ctx, r)
	if err == nil {
		s.publish(api.Event{Kind: api.EventRunChanged, TicketID: r.TicketID, RunID: r.ID})
	}
	return err
}

func (s *PublishingStore) AddValidation(ctx context.Context, id, runID, step string, exitCode int, durationMS int64, logPath string) error {
	err := s.Store.AddValidation(ctx, id, runID, step, exitCode, durationMS, logPath)
	if err == nil {
		s.publish(api.Event{Kind: api.EventRunChanged, RunID: runID})
	}
	return err
}

func (s *PublishingStore) OpenAttention(ctx context.Context, a core.Attention) error {
	err := s.Store.OpenAttention(ctx, a)
	if err == nil {
		s.publish(api.Event{
			Kind: api.EventAttentionChanged, ProjectID: a.ProjectID, TicketID: a.TicketID,
		})
	}
	return err
}

func (s *PublishingStore) ResolveAttentionForTicket(ctx context.Context, ticketID string) (int, error) {
	n, err := s.Store.ResolveAttentionForTicket(ctx, ticketID)
	if err == nil && n > 0 {
		s.publish(api.Event{Kind: api.EventAttentionChanged, TicketID: ticketID})
	}
	return n, err
}

func (s *PublishingStore) SetProviderUnavailable(ctx context.Context, a core.ProviderAvailability) error {
	err := s.Store.SetProviderUnavailable(ctx, a)
	if err == nil {
		// A cooling-down provider changes what the next assignment will choose, which the
		// Settings and Running screens both show.
		s.publish(api.Event{Kind: api.EventProjectChanged})
	}
	return err
}
