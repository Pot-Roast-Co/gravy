package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/store"
)

// idleRunner stands in for the scheduler loop: the lifecycle under test is the daemon's, not
// the queue's.
type idleRunner struct{ started chan struct{} }

func (r idleRunner) Run(ctx context.Context) error {
	if r.started != nil {
		close(r.started)
	}
	<-ctx.Done()
	return ctx.Err()
}

// fixture returns a home directory, a store seeded with one project, and a service over it.
func fixture(t *testing.T) (string, *store.DB, *api.Local) {
	t.Helper()
	ctx := context.Background()

	// Unix sockets are limited to about 104 bytes of path, which t.TempDir() under a long
	// TMPDIR can exceed on its own.
	home, err := os.MkdirTemp("", "gvd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })

	db, err := store.Open(ctx, filepath.Join(home, "gravy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.CreateProject(ctx, core.Project{ID: "p1", Slug: "proj", Name: "proj",
		RepoPath: "/repo", TargetBranch: "main", MergeMode: core.LandMerge,
		MaxConcurrency: 1, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	var n int
	return home, db, api.NewLocal(db, nil, nil, func() string { n++; return fmt.Sprintf("id-%d", n) })
}

// start runs a daemon and returns a stop function.
func start(t *testing.T, home string, db *store.DB, svc *api.Local) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	errc := make(chan error, 1)

	var n int
	d := New(home, svc, idleRunner{started: started}, db, func() string { n++; return fmt.Sprintf("rid-%d", n) }, nil)
	go func() { errc <- d.Run(ctx) }()

	select {
	case <-started:
	case err := <-errc:
		cancel()
		t.Fatalf("daemon exited during startup: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("daemon did not start")
	}
	// The loop starts before the socket is guaranteed bound; wait for the real signal.
	waitForSocket(t, SocketPath(home))

	return func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("daemon exited with %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon did not shut down")
		}
	}
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := api.Dial(path); err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s never came up", path)
}

// TestDaemonAnswersOverTheSocket is AC1.
func TestDaemonAnswersOverTheSocket(t *testing.T) {
	home, db, svc := fixture(t)
	stop := start(t, home, db, svc)
	defer stop()

	c, err := api.Dial(SocketPath(home))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	st, err := c.Status(context.Background(), api.ProjectFilter{})
	if err != nil {
		t.Fatalf("Status over the socket: %v", err)
	}
	if len(st.Projects) != 1 || st.Projects[0].Project.Slug != "proj" {
		t.Errorf("status = %+v", st)
	}
}

// TestSecondDaemonIsRefusedByPidfile is AC2. Two daemons over one database would both claim the
// same ready ticket.
func TestSecondDaemonIsRefusedByPidfile(t *testing.T) {
	home, db, svc := fixture(t)
	stop := start(t, home, db, svc)
	defer stop()

	var n int
	second := New(home, svc, idleRunner{}, db, func() string { n++; return "x" }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := second.Run(ctx)
	if err == nil {
		t.Fatal("a second daemon started alongside the first")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error = %v, want it to name the running daemon", err)
	}
	// The refusal must not have deleted the live daemon's files.
	if _, serr := os.Stat(PidPath(home)); serr != nil {
		t.Errorf("the refused daemon removed the live pidfile: %v", serr)
	}
	waitForSocket(t, SocketPath(home))
}

// TestStalePidFileIsCleared is AC3. A pidfile outliving its process is the ordinary case after a
// kill -9; refusing to start would mean deleting a file by hand after every hard stop.
func TestStalePidFileIsCleared(t *testing.T) {
	home, db, svc := fixture(t)

	// A pid that cannot be running: nothing owns the maximum.
	stale := strconv.Itoa(int(^uint32(0) >> 1))
	if err := os.WriteFile(PidPath(home), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	// And a socket file with nobody behind it.
	if err := os.WriteFile(SocketPath(home), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	stop := start(t, home, db, svc)
	defer stop()

	pid, err := readPid(PidPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if pid != os.Getpid() {
		t.Errorf("pidfile holds %d, want this process %d", pid, os.Getpid())
	}
}

// TestShutdownLeavesNothingBehind is AC4.
func TestShutdownLeavesNothingBehind(t *testing.T) {
	home, db, svc := fixture(t)
	stop := start(t, home, db, svc)
	stop()

	for _, p := range []string{SocketPath(home), PidPath(home)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s outlived a clean shutdown", filepath.Base(p))
		}
	}
	if Running(home) != 0 {
		t.Error("Running still reports a live daemon after shutdown")
	}
}

// TestReconcileRescuesAnInterruptedRun is AC5, and the reason startup reconciliation exists: a
// ticket left mid-flight with no attention row is invisible, which is exactly what the queue is
// meant to prevent.
func TestReconcileRescuesAnInterruptedRun(t *testing.T) {
	home, db, svc := fixture(t)
	ctx := context.Background()

	tk := core.Ticket{ID: "GR-1", ProjectID: "p1", Title: "interrupted",
		State: core.StateBacklog, Route: core.RouteImplementation, CreatedAt: time.Now()}
	if err := db.CreateTicket(ctx, tk); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []core.Event{core.EventMarkReady, core.EventAssign, core.EventStart} {
		if _, err := db.SetTicketState(ctx, "GR-1", ev); err != nil {
			t.Fatal(err)
		}
	}
	// A run that never ended, whose process is long gone.
	if err := db.CreateRun(ctx, core.Run{ID: "r1", TicketID: "GR-1", HostID: "local",
		ProviderID: "claude-code", Model: "sonnet", State: core.StateRunning,
		PID: int(^uint32(0) >> 1), StartedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	var n int
	d := New(home, svc, idleRunner{}, db, func() string { n++; return fmt.Sprintf("a-%d", n) }, nil)
	touched, err := d.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if touched != 1 {
		t.Fatalf("reconciled %d runs, want 1", touched)
	}

	run, err := db.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if run.EndedAt == nil {
		t.Error("the interrupted run is still marked in flight")
	}
	if run.FailureNote == "" {
		t.Error("the run does not say why it ended")
	}

	got, _ := db.GetTicket(ctx, "GR-1")
	if !core.NeedsHuman(got.State) {
		t.Errorf("ticket state = %q, want it parked for a human", got.State)
	}
	open, _ := db.ListOpenAttention(ctx)
	if len(open) != 1 {
		t.Fatalf("attention = %+v, want one entry", open)
	}
	if open[0].TicketID != "GR-1" {
		t.Errorf("attention is not linked to the ticket: %+v", open[0])
	}
	if open[0].Payload["detail"] == nil {
		t.Error("attention does not say what happened to the process")
	}

	// Reconciling again must be a no-op: a settled run is not rescued twice.
	if touched, err = d.Reconcile(ctx); err != nil || touched != 0 {
		t.Errorf("second Reconcile = (%d, %v), want (0, nil)", touched, err)
	}
}

func TestReconcileRecoversReviewAfterRunEnded(t *testing.T) {
	for _, verdict := range []string{"", `{"overall":"pass"}`} {
		t.Run(verdict, func(t *testing.T) {
			home, db, svc := fixture(t)
			ctx := context.Background()
			tk := core.Ticket{ID: "review-ticket", ProjectID: "p1", Title: "finished implementation", State: core.StateReviewing, Route: core.RouteImplementation, WorktreePath: "preserved-worktree", Feedback: "preserved feedback", CreatedAt: time.Now()}
			if err := db.CreateTicket(ctx, tk); err != nil {
				t.Fatal(err)
			}
			ended := time.Now().Add(-time.Hour)
			run := core.Run{ID: "ended-run", TicketID: tk.ID, HostID: "local", ProviderID: "codex", Model: "gpt-6-astra", State: core.StateValidating, FailureClass: core.Success, StartedAt: ended, EndedAt: &ended, Verdict: verdict}
			if err := db.CreateRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			d := New(home, svc, idleRunner{}, db, func() string { return "recovered-review" }, nil)
			if n, err := d.Reconcile(ctx); err != nil || n != 1 {
				t.Fatalf("reconcile %d: %v", n, err)
			}
			got, err := db.GetTicket(ctx, tk.ID)
			if err != nil || got.State != core.StateReview || got.WorktreePath != tk.WorktreePath || got.Feedback != tk.Feedback {
				t.Fatalf("ticket %+v, %v", got, err)
			}
			saved, err := db.GetRun(ctx, run.ID)
			if err != nil || saved.FailureClass != core.Success || saved.Verdict == "" {
				t.Fatalf("run %+v, %v", saved, err)
			}
			if verdict != "" && saved.Verdict != verdict {
				t.Fatal("replaced a completed verdict")
			}
			if verdict == "" && !strings.Contains(saved.Verdict, "interrupted") && !strings.Contains(saved.Verdict, "daemon stopped") {
				t.Fatal("missing interruption explanation")
			}
			attention, err := db.ListOpenAttention(ctx)
			if err != nil || len(attention) != 1 || attention[0].Reason != core.ReasonReviewPending {
				t.Fatalf("attention %+v, %v", attention, err)
			}
			if n, err := d.Reconcile(ctx); err != nil || n != 0 {
				t.Fatalf("second reconcile %d: %v", n, err)
			}
		})
	}
}
