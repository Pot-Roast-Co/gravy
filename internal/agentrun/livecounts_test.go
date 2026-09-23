package agentrun

import (
	"context"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/provider"
)

// runWriter records UpdateRun and nothing else; liveCounts touches nothing else.
type runWriter struct {
	Store
	writes []core.Run
}

func (w *runWriter) UpdateRun(_ context.Context, r core.Run) error {
	w.writes = append(w.writes, r)
	return nil
}

// step is one event, observed after the clock advances by gap.
type step struct {
	gap time.Duration
	ev  provider.Event
}

func usage(in, out int) provider.Event {
	return provider.Event{Kind: provider.EventUsage, Fields: map[string]any{"input_tokens": in, "output_tokens": out}}
}

func TestLiveCountsThrottle(t *testing.T) {
	tool := provider.Event{Kind: provider.EventToolUse, Tool: "Bash"}
	msg := provider.Event{Kind: provider.EventMessage, Text: "hi"}

	nTools := []step{{0, usage(1, 1)}, {0, usage(5, 5)}}
	for i := 0; i < liveToolEvery; i++ {
		nTools = append(nTools, step{0, tool})
	}

	cases := []struct {
		name    string
		steps   []step
		stopped bool
		writes  int
		final   [3]int // turns, in, out as last written
	}{
		{
			name:   "first usage is written at once",
			steps:  []step{{0, usage(100, 10)}},
			writes: 1, final: [3]int{1, 100, 10},
		},
		{
			name: "a burst of usage is one write",
			steps: []step{
				{0, usage(1, 1)}, {time.Millisecond, usage(1, 1)}, {time.Millisecond, usage(1, 1)},
				{time.Millisecond, usage(1, 1)}, {time.Millisecond, usage(1, 1)},
			},
			writes: 1, final: [3]int{1, 1, 1},
		},
		{
			name:   "usage after the gap is written",
			steps:  []step{{0, usage(1, 1)}, {liveMinGap, usage(2, 2)}},
			writes: 2, final: [3]int{2, 3, 3},
		},
		{
			name:   "pending counts go out after N tool uses",
			steps:  nTools,
			writes: 2, final: [3]int{2, 6, 6},
		},
		{
			name:   "pending counts go out after M seconds on any event",
			steps:  []step{{0, usage(1, 1)}, {0, usage(5, 5)}, {liveEvery, msg}},
			writes: 2, final: [3]int{2, 6, 6},
		},
		{
			name:   "tool uses alone change nothing and write nothing",
			steps:  []step{{0, tool}, {liveEvery, tool}, {0, msg}},
			writes: 0,
		},
		{
			name:    "nothing is written after stop",
			steps:   []step{{0, usage(1, 1)}, {liveEvery, usage(1, 1)}},
			stopped: true, writes: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &runWriter{}
			now := time.Unix(1_000_000, 0)
			c := newLiveCounts(w, core.Run{ID: "r1", State: core.StateRunning})
			c.now = func() time.Time { return now }
			if tc.stopped {
				c.stop()
			}
			for _, s := range tc.steps {
				now = now.Add(s.gap)
				c.observe(context.Background(), s.ev)
			}
			if len(w.writes) != tc.writes {
				t.Fatalf("writes = %d, want %d", len(w.writes), tc.writes)
			}
			if tc.writes == 0 {
				return
			}
			last := w.writes[len(w.writes)-1]
			if got := [3]int{last.Turns, last.TokensIn, last.TokensOut}; got != tc.final {
				t.Errorf("last write (turns, in, out) = %v, want %v", got, tc.final)
			}
			if last.ID != "r1" || last.State != core.StateRunning {
				t.Errorf("live write changed the row's identity or state: %+v", last)
			}
		})
	}
}

// TestLiveCountsTickFlushesAQuietRun: a run that goes silent straight after a throttled usage
// event still gets its counts out, rather than holding them until it next speaks.
func TestLiveCountsTickFlushesAQuietRun(t *testing.T) {
	w := &runWriter{}
	now := time.Unix(1_000_000, 0)
	c := newLiveCounts(w, core.Run{ID: "r1"})
	c.now = func() time.Time { return now }

	c.observe(context.Background(), usage(1, 1))
	c.observe(context.Background(), usage(2, 2))
	c.tick(context.Background())
	if len(w.writes) != 1 {
		t.Fatalf("writes before the interval = %d, want 1", len(w.writes))
	}
	now = now.Add(liveEvery)
	c.tick(context.Background())
	if len(w.writes) != 2 || w.writes[1].TokensIn != 3 {
		t.Fatalf("tick after the interval did not write the pending counts: %+v", w.writes)
	}
}

// TestLiveCountsCodexCachedInputIsNotAdded: codex's cached count is a share of input_tokens, so
// the row reads the same 1200 in that the log line renders for this event, not 2000.
func TestLiveCountsCodexCachedInputIsNotAdded(t *testing.T) {
	w := &runWriter{}
	c := newLiveCounts(w, core.Run{ID: "r1"})

	// The codex sample usage event from internal/api's renderEvent table, which renders as
	// "usage: 1200 tokens in, 340 out".
	c.observe(context.Background(), provider.Event{Kind: provider.EventUsage,
		Fields: map[string]any{"input_tokens": 1200, "cached_tokens": 800, "output_tokens": 340}})

	if len(w.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(w.writes))
	}
	if got := w.writes[0]; got.TokensIn != 1200 || got.TokensOut != 340 {
		t.Errorf("row = in %d, out %d; want in 1200, out 340", got.TokensIn, got.TokensOut)
	}
}
