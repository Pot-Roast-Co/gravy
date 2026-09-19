package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/scheduler"
)

// idleLoop returns a loop over an empty queue: no ready tickets, no hosts, so every tick decides
// nothing and the only behaviour left to observe is the timers.
func idleLoop(t *testing.T) *Loop {
	t.Helper()
	_, db, _ := fixture(t)

	pool, err := scheduler.NewStaticPool(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	l := NewLoop(scheduler.New(db, pool, scheduler.FixedRoute{}), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	l.Interval = time.Millisecond
	return l
}

// TestLoopPrunesOnItsOwnTicker is the bug this ticket exists for: retention used to run exactly
// once, on the way up, so a daemon left running for months swept nothing the whole time.
//
// It waits for two sweeps rather than one. A single fire proves only that something happened at
// some point, which is what startup already did; the claim is that it keeps happening.
func TestLoopPrunesOnItsOwnTicker(t *testing.T) {
	l := idleLoop(t)
	l.PruneInterval = 5 * time.Millisecond

	swept := make(chan struct{}, 8)
	l.Prune = func() {
		select {
		case swept <- struct{}{}:
		default:
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	for i := 1; i <= 2; i++ {
		select {
		case <-swept:
		case <-time.After(5 * time.Second):
			t.Fatalf("the loop stopped pruning after %d sweeps", i-1)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not stop")
	}
}

// TestLoopWithoutPruneStillRuns covers the CLI, which drives the same loop with no retention
// hook attached. A nil hook must be a loop that never sweeps, not one that panics on its first
// hour.
func TestLoopWithoutPruneStillRuns(t *testing.T) {
	l := idleLoop(t)
	l.PruneInterval = time.Millisecond // would fire immediately, if it fired at all

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
