package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/notify"
)

// LogFileName is where a detached daemon's output goes.
const LogFileName = "gravyd.log"

// SocketPath is the daemon's socket for a given home.
func SocketPath(home string) string { return filepath.Join(home, api.SocketName) }

// LogPath is the daemon's log file for a given home.
func LogPath(home string) string { return filepath.Join(home, LogFileName) }

// Runner is the scheduler loop the daemon drives. An interface so the lifecycle can be tested
// without standing up an orchestrator and a provider.
type Runner interface {
	Run(ctx context.Context) error
}

// ReconcileStore is what startup reconciliation needs from persistence.
type ReconcileStore interface {
	ListUnfinishedRuns(ctx context.Context) ([]core.Run, error)
	UpdateRun(ctx context.Context, r core.Run) error
	GetTicket(ctx context.Context, id string) (core.Ticket, error)
	SetTicketState(ctx context.Context, id string, ev core.Event) (core.State, error)
	OpenAttention(ctx context.Context, a core.Attention) error
}

// Daemon owns the socket, the scheduler loop and the process's lifetime.
//
// It is the single writer to the database. Everything else — the TUI, the CLI, a later GUI — is
// a client over the socket, which is what stops any of them from quietly growing a shortcut into
// the store.
type Daemon struct {
	home  string
	svc   api.Service
	loop  Runner
	store ReconcileStore
	newID func() string
	log   *slog.Logger
	// notifier is told when a run is found orphaned by a restart. Nil is legitimate.
	notifier Notifier

	srv *api.Server
}

// Notifier tells the human that Gravy needs them.
type Notifier interface {
	Notify(ctx context.Context, title, body string, urgency notify.Urgency)
}

// WithNotifier sets who is told when reconciliation parks an orphaned run.
func (d *Daemon) WithNotifier(n Notifier) *Daemon {
	d.notifier = n
	return d
}

// New returns a daemon for a home directory.
func New(home string, svc api.Service, loop Runner, store ReconcileStore, newID func() string, log *slog.Logger) *Daemon {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Daemon{home: home, svc: svc, loop: loop, store: store, newID: newID, log: log}
}

// Run starts the daemon and blocks until ctx is cancelled.
//
// The order matters: claim the pidfile before binding the socket, so a second daemon is refused
// by the cheaper check; reconcile before serving, so no client can read a state that startup is
// about to change underneath it.
func (d *Daemon) Run(ctx context.Context) error {
	pidPath := PidPath(d.home)
	if err := acquirePidFile(pidPath); err != nil {
		return err
	}
	defer func() {
		if err := releasePidFile(pidPath); err != nil {
			d.log.Warn("could not remove the pidfile", "error", err)
		}
	}()

	d.srv = api.NewServer(d.svc, SocketPath(d.home), d.log)
	if err := d.srv.Listen(); err != nil {
		return err
	}
	defer d.srv.Close()

	n, err := d.Reconcile(ctx)
	switch {
	// Being told to stop while starting is a clean shutdown, not a failed reconciliation.
	case errors.Is(err, context.Canceled):
		return nil
	case err != nil:
		return fmt.Errorf("startup reconciliation: %w", err)
	}
	if n > 0 {
		d.log.Warn("recovered runs left behind by a previous daemon", "count", n)
	}

	d.log.Info("gravy daemon listening", "socket", SocketPath(d.home), "pid", pidPath)

	var (
		wg      sync.WaitGroup
		serveEr error
		loopEr  error
	)
	wg.Add(2)
	go func() { defer wg.Done(); serveEr = d.srv.Serve(ctx) }()
	go func() { defer wg.Done(); loopEr = d.loop.Run(ctx) }()
	wg.Wait()

	// A cancelled context is how a clean shutdown arrives, not a failure.
	if serveEr != nil && !errors.Is(serveEr, context.Canceled) {
		return serveEr
	}
	if loopEr != nil && !errors.Is(loopEr, context.Canceled) {
		return loopEr
	}
	return nil
}

// Reconcile settles runs a previous daemon left in flight, and reports how many it touched.
//
// A run recorded as active whose daemon is gone cannot be resumed: the pipe its output came
// through died with the process that held it, so even a process still alive is one nothing can
// hear from. Both cases therefore end the same way — the run is failed and the ticket is parked
// visibly — with the payload saying which happened. A still-running orphan is killed first,
// because it is writing into a worktree the next run would otherwise reuse underneath it.
func (d *Daemon) Reconcile(ctx context.Context) (int, error) {
	runs, err := d.store.ListUnfinishedRuns(ctx)
	if err != nil {
		return 0, err
	}

	touched := 0
	for _, r := range runs {
		orphan := "the run's process was already gone"
		if host.Alive(r.PID) {
			orphan = fmt.Sprintf("the run's process (pid %d) was still running and has been stopped", r.PID)
			if err := host.Reap(r.PID); err != nil {
				d.log.Warn("could not stop an orphaned run", "pid", r.PID, "error", err)
				orphan = fmt.Sprintf("the run's process (pid %d) is still running and could not be stopped", r.PID)
			}
		}

		now := time.Now()
		r.State = core.StateNeedsYou
		r.FailureClass = core.Unknown
		r.FailureNote = "the daemon stopped while this run was in flight: " + orphan
		r.EndedAt = &now
		if err := d.store.UpdateRun(ctx, r); err != nil {
			return touched, err
		}

		if err := d.parkTicket(ctx, r, orphan); err != nil {
			return touched, err
		}
		touched++
		d.log.Warn("recovered an interrupted run", "run", r.ID, "ticket", r.TicketID, "detail", orphan)
	}
	return touched, nil
}

// parkTicket moves a ticket out of flight and into the Needs You queue.
func (d *Daemon) parkTicket(ctx context.Context, r core.Run, detail string) error {
	t, err := d.store.GetTicket(ctx, r.TicketID)
	if err != nil {
		return err
	}
	if !core.IsActive(t.State) || core.NeedsHuman(t.State) {
		return nil // already settled, or already waiting on a human
	}

	// The legal edge out of flight depends on where the ticket got to. Try the one its state
	// permits, then the other, rather than leaving it stuck mid-flight — a ticket in limbo with
	// no attention row is invisible, which is the outcome the queue exists to prevent.
	if _, err := d.store.SetTicketState(ctx, t.ID, core.EventRunFailed); err != nil {
		if _, err = d.store.SetTicketState(ctx, t.ID, core.EventValidationExhausted); err != nil {
			return fmt.Errorf("park ticket %s: %w", t.ID, err)
		}
	}

	// The project name is not worth widening ReconcileStore for: reconciliation runs once at
	// startup and the ticket title already says which work stopped.
	if d.notifier != nil {
		title, body, urgency := notify.ForAttention(core.ReasonHostUnavailable, "", t.Title)
		notify.Deliver(ctx, d.notifier, title, body, urgency, t.ID)
	}

	return d.store.OpenAttention(ctx, core.Attention{
		ID:        d.newID(),
		ProjectID: t.ProjectID,
		TicketID:  t.ID,
		RunID:     r.ID,
		Reason:    core.ReasonHostUnavailable,
		Payload: map[string]any{
			"reason":   "the daemon stopped while this run was in flight",
			"detail":   detail,
			"worktree": t.WorktreePath,
		},
		CreatedAt: time.Now(),
	})
}
