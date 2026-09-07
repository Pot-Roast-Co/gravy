package api

import (
	"context"
	"sync"
	"time"

	"github.com/bobbybrady/gravy/internal/core"
)

// EventKind names what changed. Clients re-read what they render rather than reconstructing
// state from a stream of deltas, so the kinds stay coarse on purpose: a missed or coalesced
// event costs a redundant read, never a wrong screen.
type EventKind string

// The event kinds.
const (
	EventProjectChanged   EventKind = "project_changed"
	EventTicketChanged    EventKind = "ticket_changed"
	EventRunChanged       EventKind = "run_changed"
	EventAttentionChanged EventKind = "attention_changed"
)

// Event is a server push saying something moved. It carries identifiers, not payloads.
type Event struct {
	Kind      EventKind
	ProjectID string
	TicketID  string
	RunID     string
	State     core.State
	At        time.Time
}

// eventBufferSize is how far a subscriber may fall behind before it starts losing events.
//
// Losing one is harmless — every kind means "re-read what you render" — whereas blocking the
// writer would stall the daemon behind a wedged UI, so the trade runs one way only.
const eventBufferSize = 64

// broker fans events out to every subscriber.
type broker struct {
	mu   sync.Mutex
	subs map[int]chan Event
	next int
}

func newBroker() *broker { return &broker{subs: make(map[int]chan Event)} }

// subscribe returns a channel of events and a function that stops the subscription. The channel
// is closed when the context ends or the returned function is called, whichever comes first.
func (b *broker) subscribe(ctx context.Context) (<-chan Event, func()) {
	ch := make(chan Event, eventBufferSize)

	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			b.mu.Lock()
			if c, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(c)
			}
			b.mu.Unlock()
		})
	}

	go func() {
		<-ctx.Done()
		stop()
	}()

	return ch, stop
}

// publish delivers to every subscriber, skipping any that has fallen behind.
func (b *broker) publish(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			// Full: this subscriber is not keeping up. Dropping is correct — see
			// eventBufferSize.
		}
	}
}
