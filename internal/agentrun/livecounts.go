package agentrun

import (
	"context"
	"sync"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/provider"
)

// How often a running run's counters reach its row. A usage event is written as it arrives
// unless the last write was under liveMinGap ago; otherwise pending counts go out after
// liveToolEvery tool uses or liveEvery, whichever comes first. The gap is what stops a chatty
// run from becoming a SQLite write per event.
const (
	liveMinGap    = time.Second
	liveToolEvery = 10
	liveEvery     = 15 * time.Second
)

// liveCounts keeps a run's Turns and token counts current while its agent is still going.
//
// Before this, the row was written once when the agent exited, so every client showed zero
// for the whole of a run. The final write in executeAgent stays authoritative: the counts here
// are the running total of what the events have said so far, and the provider's own Outcome
// replaces them.
type liveCounts struct {
	store Store
	now   func() time.Time

	// mu is held across the write, so stop cannot return while one is in flight and nothing
	// written here can land after the final row.
	mu      sync.Mutex
	run     core.Run
	stopped bool

	turns, in, out int
	pending        bool
	toolsSince     int
	lastWrite      time.Time
}

func newLiveCounts(s Store, run core.Run) *liveCounts {
	return &liveCounts{store: s, now: time.Now, run: run}
}

// observe folds one event in and writes the row if it is due.
func (c *liveCounts) observe(ctx context.Context, ev provider.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch ev.Kind {
	case provider.EventUsage:
		// Adapters report usage per turn, not as a running total, and name the fields in
		// their own CLI's spelling. Input is taken as reported: codex's cached count is a
		// portion of its input, not extra to it, and claude-code already folds its cache reads
		// into input_tokens.
		c.turns++
		c.in += fieldInt(ev.Fields, "input_tokens", "inputTokens")
		c.out += fieldInt(ev.Fields, "output_tokens", "outputTokens")
		c.pending = true
		c.flushLocked(ctx, c.now().Sub(c.lastWrite) >= liveMinGap)
	case provider.EventToolUse:
		c.toolsSince++
		c.flushLocked(ctx, c.toolsSince >= liveToolEvery)
	default:
		c.flushLocked(ctx, false)
	}
}

// tick writes pending counts that have waited long enough, for a run that went quiet right
// after a throttled usage event.
func (c *liveCounts) tick(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushLocked(ctx, false)
}

// stop ends live writes. After it returns, nothing here touches the row again.
func (c *liveCounts) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
}

func (c *liveCounts) flushLocked(ctx context.Context, due bool) {
	if c.stopped || !c.pending {
		return
	}
	if !due && c.now().Sub(c.lastWrite) < liveEvery {
		return
	}
	c.run.Turns, c.run.TokensIn, c.run.TokensOut = c.turns, c.in, c.out
	// A failed write costs a stale header until the next one, never the run.
	_ = c.store.UpdateRun(ctx, c.run)
	c.pending = false
	c.toolsSince = 0
	c.lastWrite = c.now()
}

// fieldInt reads a count from an event's fields. In process they are Go ints; an event that has
// been through JSON carries float64s.
func fieldInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := m[k].(type) {
		case int:
			return v
		case int64:
			return int(v)
		case float64:
			return int(v)
		}
	}
	return 0
}
