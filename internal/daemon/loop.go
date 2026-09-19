package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pot-roast-co/gravy/internal/agentrun"
	"github.com/pot-roast-co/gravy/internal/scheduler"
)

// Loop drives the queue: tick the scheduler, start what it assigns, repeat.
//
// The scheduler decides and the loop executes, and the separation is deliberate: scheduling
// stays a pure function of recorded state, which is what makes an assignment reproducible and
// explainable after the fact.
type Loop struct {
	sched *scheduler.Scheduler
	orch  *agentrun.Orchestrator
	log   *slog.Logger

	// Interval is how often the queue is re-examined when nothing else wakes it.
	Interval time.Duration

	// Prune, when set, sweeps run logs past their retention window. It lives on the loop
	// because the loop is the thing still running an hour — or a month — after startup, and a
	// daemon nobody restarts is precisely the one accumulating logs.
	Prune func()

	// PruneInterval is how often Prune runs. Retention is measured in days, so it is
	// deliberately nothing like Interval: the sweep walks the whole runs directory and has no
	// business doing that every couple of seconds.
	PruneInterval time.Duration

	// OnAssign, when set, is called as each assignment starts. Used by the CLI to narrate.
	OnAssign func(scheduler.Assignment)
	// OnFinish, when set, is called as each run ends.
	OnFinish func(agentrun.Result, error)

	mu      sync.Mutex
	running map[string]bool
	wg      sync.WaitGroup
}

// NewLoop returns a scheduler loop.
func NewLoop(s *scheduler.Scheduler, o *agentrun.Orchestrator, log *slog.Logger) *Loop {
	return &Loop{
		sched:         s,
		orch:          o,
		log:           log,
		Interval:      2 * time.Second,
		PruneInterval: time.Hour,
		running:       map[string]bool{},
	}
}

// Run drives the loop until the context is cancelled, then waits for in-flight runs to finish.
func (l *Loop) Run(ctx context.Context) error {
	ticker := time.NewTicker(l.Interval)
	defer ticker.Stop()

	// A second, far slower ticker rather than a counter on the first one: the two answer
	// different questions, and a nil channel is how "no retention configured" blocks forever.
	var pruneC <-chan time.Time
	if l.Prune != nil && l.PruneInterval > 0 {
		pruner := time.NewTicker(l.PruneInterval)
		defer pruner.Stop()
		pruneC = pruner.C
	}

	for {
		if err := l.tick(ctx); err != nil {
			// A failing tick must not kill the loop: a transient store or git error should
			// cost one cycle, not the daemon.
			l.log.Error("scheduler tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			l.wg.Wait()
			return nil
		case <-pruneC:
			l.Prune()
		case <-ticker.C:
		}
	}
}

// RunUntilIdle drives the loop until nothing is running and nothing is assignable.
//
// This is what makes the loop testable and what `gravy run --once` uses: it answers "work the
// queue until there is nothing left to do", rather than running forever.
func (l *Loop) RunUntilIdle(ctx context.Context) error {
	for {
		if err := l.tick(ctx); err != nil {
			return err
		}
		if l.inFlight() == 0 {
			// Nothing running and the last tick assigned nothing: the queue is drained of
			// everything it can act on without a human.
			assignments, err := l.sched.Tick(ctx)
			if err != nil {
				return err
			}
			if len(assignments) == 0 {
				l.wg.Wait()
				return nil
			}
			continue
		}
		select {
		case <-ctx.Done():
			l.wg.Wait()
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// tick assigns and starts whatever the scheduler decides is eligible.
func (l *Loop) tick(ctx context.Context) error {
	assignments, err := l.sched.Tick(ctx)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	for _, a := range assignments {
		if l.claim(a.TicketID) {
			l.start(ctx, a)
		}
	}
	return nil
}

// claim records that a ticket is being started, so two ticks cannot double-start it.
//
// The scheduler is stateless between ticks and will keep returning a ticket until its state
// changes in the store. Without this guard a slow first transition would let the next tick start
// the same ticket again, and two agents would share one worktree.
func (l *Loop) claim(ticketID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[ticketID] {
		return false
	}
	l.running[ticketID] = true
	return true
}

func (l *Loop) release(ticketID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.running, ticketID)
}

func (l *Loop) inFlight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.running)
}

// start runs one assignment in the background.
func (l *Loop) start(ctx context.Context, a scheduler.Assignment) {
	if l.OnAssign != nil {
		l.OnAssign(a)
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer l.release(a.TicketID)

		res, err := l.orch.Run(ctx, agentrun.Assignment{
			TicketID:   a.TicketID,
			HostID:     a.HostID,
			ProviderID: a.ProviderID,
			Model:      a.Model,
		})
		if err != nil {
			l.log.Error("run failed", "ticket", a.TicketID, "error", err)
		}
		if l.OnFinish != nil {
			l.OnFinish(res, err)
		}
	}()
}

// Kill terminates a ticket's live run.
func (l *Loop) Kill(ticketID string) error { return l.orch.Kill(ticketID) }
