package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bobbybrady/gravy/internal/core"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "gravy.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testProject(id, slug string) core.Project {
	return core.Project{
		ID: id, Slug: slug, Name: slug, RepoPath: "/repos/" + slug,
		TargetBranch: "main", MergeMode: core.LandMerge, MaxConcurrency: 1,
		Requirements: core.Requirements{OS: []string{"darwin"}, Tools: map[string]string{"go": "1.23"}},
		Validation:   []core.Step{{Name: "test", Cmd: "go test ./...", Required: true}},
		Allowlist:    core.Allowlist{ReadPaths: []string{"**"}, Commands: []core.Pattern{{Match: "go *", Note: "validation"}}},
		Routes:       map[core.Route][]core.Choice{core.RouteCheap: {{ProviderID: "claude-code", Model: "haiku"}}},
		CreatedAt:    time.Unix(1700000000, 0).UTC(),
	}
}

func testTicket(id, projectID string, state core.State) core.Ticket {
	return core.Ticket{
		ID: id, ProjectID: projectID, Title: "t " + id, Body: "body",
		State: state, Route: core.RouteImplementation,
		CreatedAt: time.Unix(1700000000, 0).UTC(), UpdatedAt: time.Unix(1700000000, 0).UTC(),
	}
}

// TestMigrationsIdempotent is AC1: migrations run from empty to current and re-opening applies
// nothing further.
func TestMigrationsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gravy.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	var first int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&first); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if first == 0 {
		t.Fatal("no migrations were applied")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer db2.Close()
	var second int
	if err := db2.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&second); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if second != first {
		t.Errorf("re-open applied migrations again: %d then %d", first, second)
	}
}

// TestSchemaHasEveryTable is AC2.
func TestSchemaHasEveryTable(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	for _, table := range []string{
		"projects", "tickets", "ticket_deps", "runs", "validations", "summaries",
		"attention", "provider_availability",
	} {
		var n int
		if err := db.sql.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
			t.Fatalf("query sqlite_master: %v", err)
		}
		if n != 1 {
			t.Errorf("table %q is missing", table)
		}
	}
	for _, index := range []string{"idx_tickets_state", "idx_attention_open"} {
		var n int
		if err := db.sql.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&n); err != nil {
			t.Fatalf("query sqlite_master: %v", err)
		}
		if n != 1 {
			t.Errorf("index %q is missing", index)
		}
	}
}

func TestPragmas(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	var journal string
	if err := db.sql.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}
	var fk int
	if err := db.sql.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Error("foreign_keys is off; the schema's cascades would not fire")
	}
}

func TestProjectRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	want := testProject("p1", "gravy")

	if err := db.CreateProject(ctx, want); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	got, err := db.GetProject(ctx, "p1")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Slug != want.Slug || got.RepoPath != want.RepoPath || got.MergeMode != want.MergeMode {
		t.Errorf("scalar fields differ: %+v", got)
	}
	// The JSON columns are where a round trip actually goes wrong.
	if len(got.Validation) != 1 || got.Validation[0].Cmd != "go test ./..." {
		t.Errorf("validation did not round trip: %+v", got.Validation)
	}
	if len(got.Allowlist.Commands) != 1 || got.Allowlist.Commands[0].Note != "validation" {
		t.Errorf("allowlist did not round trip: %+v", got.Allowlist)
	}
	if got.Requirements.Tools["go"] != "1.23" {
		t.Errorf("requirements did not round trip: %+v", got.Requirements)
	}
	if len(got.Routes[core.RouteCheap]) != 1 {
		t.Errorf("routes did not round trip: %+v", got.Routes)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("created_at = %v, want %v", got.CreatedAt, want.CreatedAt)
	}

	bySlug, err := db.GetProjectBySlug(ctx, "gravy")
	if err != nil || bySlug.ID != "p1" {
		t.Errorf("GetProjectBySlug = %+v, %v", bySlug, err)
	}
}

func TestNotFound(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	if _, err := db.GetProject(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetProject on a missing row = %v, want ErrNotFound", err)
	}
	if _, err := db.GetTicket(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTicket = %v, want ErrNotFound", err)
	}
	if _, err := db.GetRun(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetRun = %v, want ErrNotFound", err)
	}
	if _, err := db.GetSummary(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSummary = %v, want ErrNotFound", err)
	}
	// A no-op update must report the missing row rather than silently succeeding.
	if err := db.DeleteProject(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteProject = %v, want ErrNotFound", err)
	}
	if err := db.ResolveAttention(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ResolveAttention = %v, want ErrNotFound", err)
	}
}

// TestReorderTicket is AC3: exactly one row changes, using fractional positioning.
func TestReorderTicket(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}

	ids := []string{"a", "b", "c", "d"}
	for _, id := range ids {
		if err := db.CreateTicket(ctx, testTicket(id, "p1", core.StateBacklog)); err != nil {
			t.Fatalf("CreateTicket %s: %v", id, err)
		}
	}

	before, err := db.ListTickets(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if order(before) != "a,b,c,d" {
		t.Fatalf("initial order = %s", order(before))
	}
	positions := map[string]float64{}
	for _, tk := range before {
		positions[tk.ID] = tk.Position
	}

	// Move d between a and b.
	if err := db.ReorderTicket(ctx, "d", "a", "b"); err != nil {
		t.Fatalf("ReorderTicket: %v", err)
	}
	after, err := db.ListTickets(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if got := order(after); got != "a,d,b,c" {
		t.Errorf("order after reorder = %s, want a,d,b,c", got)
	}

	// Exactly one row's position changed.
	changed := 0
	for _, tk := range after {
		if tk.Position != positions[tk.ID] {
			changed++
			if tk.ID != "d" {
				t.Errorf("ticket %q moved but should not have", tk.ID)
			}
		}
	}
	if changed != 1 {
		t.Errorf("%d rows changed position, want exactly 1", changed)
	}
}

func TestReorderToHeadAndTail(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := db.CreateTicket(ctx, testTicket(id, "p1", core.StateBacklog)); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.ReorderTicket(ctx, "c", "", "a"); err != nil {
		t.Fatalf("move to head: %v", err)
	}
	got, _ := db.ListTickets(ctx, "p1")
	if order(got) != "c,a,b" {
		t.Errorf("after move to head: %s, want c,a,b", order(got))
	}

	if err := db.ReorderTicket(ctx, "c", "b", ""); err != nil {
		t.Fatalf("move to tail: %v", err)
	}
	got, _ = db.ListTickets(ctx, "p1")
	if order(got) != "a,b,c" {
		t.Errorf("after move to tail: %s, want a,b,c", order(got))
	}
}

func TestReorderRejectsBadInput(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateProject(ctx, testProject("p2", "other")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := db.CreateTicket(ctx, testTicket(id, "p1", core.StateBacklog)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateTicket(ctx, testTicket("x", "p2", core.StateBacklog)); err != nil {
		t.Fatal(err)
	}

	if err := db.ReorderTicket(ctx, "a", "", ""); err == nil {
		t.Error("reorder with no neighbours succeeded")
	}
	if err := db.ReorderTicket(ctx, "nope", "a", "b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("reorder of a missing ticket = %v, want ErrNotFound", err)
	}
	// Reversed neighbours would otherwise compute a position outside the intended slot.
	if err := db.ReorderTicket(ctx, "a", "b", "a"); err == nil {
		t.Error("reorder with reversed neighbours succeeded")
	}
	// A neighbour in another project would silently corrupt the target project's ordering.
	if err := db.ReorderTicket(ctx, "a", "x", "b"); err == nil {
		t.Error("reorder against a neighbour from another project succeeded")
	}
}

// TestReorderRepeatedlyBetweenSameNeighbours subdivides the same gap many times, which is what
// eventually exhausts float precision.
func TestReorderRepeatedlyBetweenSameNeighbours(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "m"} {
		if err := db.CreateTicket(ctx, testTicket(id, "p1", core.StateBacklog)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 40; i++ {
		if err := db.ReorderTicket(ctx, "m", "a", "b"); err != nil {
			t.Fatalf("reorder %d: %v", i, err)
		}
		got, err := db.ListTickets(ctx, "p1")
		if err != nil {
			t.Fatal(err)
		}
		if order(got) != "a,m,b" {
			t.Fatalf("iteration %d: order = %s, want a,m,b", i, order(got))
		}
	}
}

func order(ts []core.Ticket) string {
	s := ""
	for i, t := range ts {
		if i > 0 {
			s += ","
		}
		s += t.ID
	}
	return s
}

// TestCascadeDeletes is AC5.
func TestCascadeDeletes(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTicket(ctx, testTicket("t1", "p1", core.StateRunning)); err != nil {
		t.Fatal(err)
	}
	run := core.Run{ID: "r1", TicketID: "t1", HostID: "local", ProviderID: "claude-code",
		Model: "sonnet", State: core.StateRunning, StartedAt: time.Unix(1700000000, 0)}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := db.AddValidation(ctx, "v1", "r1", "test", 0, 1200, "/logs/test.log"); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSummary(ctx, core.Summary{TicketID: "t1", RunID: "r1", Branch: "gravy/t1",
		Commits: []string{"abc"}, Narrative: "did a thing"}); err != nil {
		t.Fatal(err)
	}
	if err := db.OpenAttention(ctx, core.Attention{ID: "a1", ProjectID: "p1", TicketID: "t1",
		RunID: "r1", Reason: core.ReasonReviewPending, Payload: map[string]any{"files": 3}}); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteProject(ctx, "p1"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	for _, tc := range []struct{ table, want string }{
		{"tickets", "t1"}, {"runs", "r1"}, {"validations", "v1"},
		{"summaries", "t1"}, {"attention", "a1"},
	} {
		var n int
		if err := db.sql.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+tc.table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tc.table, err)
		}
		if n != 0 {
			t.Errorf("%s still has %d rows after the project was deleted", tc.table, n)
		}
	}
}

func TestTicketDepsCascade(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"t1", "t2"} {
		if err := db.CreateTicket(ctx, testTicket(id, "p1", core.StateBacklog)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AddDep(ctx, "t2", "t1"); err != nil {
		t.Fatal(err)
	}

	deps, err := db.DepsOf(ctx, "t2")
	if err != nil || len(deps) != 1 || deps[0] != "t1" {
		t.Errorf("DepsOf = %v, %v", deps, err)
	}
	dependents, err := db.DependentsOf(ctx, "t1")
	if err != nil || len(dependents) != 1 || dependents[0] != "t2" {
		t.Errorf("DependentsOf = %v, %v", dependents, err)
	}
	if err := db.AddDep(ctx, "t1", "t1"); err == nil {
		t.Error("a ticket was allowed to depend on itself")
	}

	if err := db.DeleteTicket(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	deps, err = db.DepsOf(ctx, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("dependency row survived the deletion of its target: %v", deps)
	}
}

// TestConcurrentReadersWithOneWriter is AC4.
func TestConcurrentReadersWithOneWriter(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := db.CreateTicket(ctx, testTicket(fmt.Sprintf("t%02d", i), "p1", core.StateReady)); err != nil {
			t.Fatal(err)
		}
	}

	const readers = 100
	var wg sync.WaitGroup
	errs := make(chan error, readers+1)

	stop := make(chan struct{})
	wg.Add(1)
	go func() { // the single writer, churning throughout
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			tk := testTicket(fmt.Sprintf("w%04d", i), "p1", core.StateBacklog)
			if err := db.CreateTicket(ctx, tk); err != nil {
				errs <- fmt.Errorf("writer: %w", err)
				return
			}
		}
	}()

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := db.ListTicketsByState(ctx, core.StateReady); err != nil {
					errs <- fmt.Errorf("reader: %w", err)
					return
				}
				if _, err := db.CountActiveTickets(ctx, "p1"); err != nil {
					errs <- fmt.Errorf("reader: %w", err)
					return
				}
			}
		}()
	}

	// Readers finish, then the writer stops.
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(stop)
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestSetTicketStateUsesCoreMachine: state changes go through the transition table, so an
// illegal one is rejected before anything is written.
func TestSetTicketStateUsesCoreMachine(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTicket(ctx, testTicket("t1", "p1", core.StateReady)); err != nil {
		t.Fatal(err)
	}

	got, err := db.SetTicketState(ctx, "t1", core.EventAssign)
	if err != nil {
		t.Fatalf("SetTicketState: %v", err)
	}
	if got != core.StateAssigned {
		t.Errorf("state = %q, want assigned", got)
	}

	// Approving an assigned ticket is not a legal edge, and must not be written.
	if _, err := db.SetTicketState(ctx, "t1", core.EventApprove); !errors.Is(err, core.ErrIllegalTransition) {
		t.Fatalf("illegal transition = %v, want ErrIllegalTransition", err)
	}
	tk, err := db.GetTicket(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if tk.State != core.StateAssigned {
		t.Errorf("state changed despite the illegal transition: %q", tk.State)
	}

	if _, err := db.SetTicketState(ctx, "nope", core.EventAssign); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetTicketState on a missing ticket = %v, want ErrNotFound", err)
	}
}

// TestActiveStatesMatchCore keeps the SQL's notion of "in flight" tied to core's, since serial
// mode depends on the two agreeing.
func TestActiveStatesMatchCore(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}

	wantActive := 0
	for i, s := range core.AllStates {
		if err := db.CreateTicket(ctx, testTicket(fmt.Sprintf("t%02d", i), "p1", s)); err != nil {
			t.Fatal(err)
		}
		if core.IsActive(s) {
			wantActive++
		}
	}
	got, err := db.CountActiveTickets(ctx, "p1")
	if err != nil {
		t.Fatalf("CountActiveTickets: %v", err)
	}
	if got != wantActive {
		t.Errorf("CountActiveTickets = %d, want %d", got, wantActive)
	}
}

func TestRunRoundTripAndReconciliation(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTicket(ctx, testTicket("t1", "p1", core.StateRunning)); err != nil {
		t.Fatal(err)
	}

	cost := 0.42
	run := core.Run{ID: "r1", TicketID: "t1", HostID: "local", ProviderID: "claude-code",
		Model: "sonnet", SessionRef: "sess-1", State: core.StateRunning, PID: 4242,
		StartedAt: time.Unix(1700000000, 0).UTC()}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	// An unfinished run is what startup reconciliation looks for.
	open, err := db.ListUnfinishedRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].PID != 4242 || open[0].SessionRef != "sess-1" {
		t.Fatalf("ListUnfinishedRuns = %+v", open)
	}

	end := time.Unix(1700000600, 0).UTC()
	run.State = core.StateValidating
	run.FailureClass = core.Success
	run.Turns, run.TokensIn, run.TokensOut = 7, 1000, 2000
	run.CostUSD, run.EndedAt = &cost, &end
	if err := db.UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.CostUSD == nil || *got.CostUSD != cost {
		t.Errorf("cost = %v, want %v", got.CostUSD, cost)
	}
	if got.EndedAt == nil || !got.EndedAt.Equal(end) {
		t.Errorf("ended_at = %v, want %v", got.EndedAt, end)
	}
	if got.Turns != 7 || got.TokensIn != 1000 {
		t.Errorf("usage did not round trip: %+v", got)
	}

	open, err = db.ListUnfinishedRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("a finished run is still listed as unfinished: %+v", open)
	}
}

// TestFailureClassRoundTrip: an unreadable class must come back as Unknown, which core treats
// as a task failure rather than a quota condition.
func TestFailureClassRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTicket(ctx, testTicket("t1", "p1", core.StateRunning)); err != nil {
		t.Fatal(err)
	}

	for i, class := range []core.FailureClass{
		core.Success, core.TaskFailure, core.QuotaExhausted, core.RateLimited,
		core.ProviderUnavailable, core.AuthExpired, core.Timeout, core.Unknown,
	} {
		id := fmt.Sprintf("r%d", i)
		r := core.Run{ID: id, TicketID: "t1", HostID: "local", ProviderID: "p", Model: "m",
			State: core.StateRunning, FailureClass: class, StartedAt: time.Unix(1, 0)}
		if err := db.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.FailureClass != class {
			t.Errorf("class %v round tripped as %v", class, got.FailureClass)
		}
	}

	// A class name this build does not recognise must not become a quota condition.
	if _, err := db.exec(ctx, `UPDATE runs SET failure_class = 'from_a_future_version' WHERE id = 'r0'`); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetRun(ctx, "r0")
	if err != nil {
		t.Fatal(err)
	}
	if got.FailureClass != core.Unknown {
		t.Errorf("unrecognised class read back as %v, want Unknown", got.FailureClass)
	}
	if got.FailureClass.Effective().IsQuotaCondition() {
		t.Error("an unrecognised class resolved to a quota condition")
	}
}

func TestAttentionQueue(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateProject(ctx, testProject("p2", "other")); err != nil {
		t.Fatal(err)
	}

	base := time.Unix(1700000000, 0).UTC()
	items := []core.Attention{
		{ID: "a1", ProjectID: "p1", Reason: core.ReasonReviewPending, CreatedAt: base.Add(2 * time.Second),
			Payload: map[string]any{"files": float64(3)}},
		{ID: "a2", ProjectID: "p1", Reason: core.ReasonValidationFailed, CreatedAt: base.Add(1 * time.Second)},
		{ID: "a3", ProjectID: "p2", Reason: core.ReasonMergeConflict, CreatedAt: base.Add(3 * time.Second)},
	}
	for _, a := range items {
		if err := db.OpenAttention(ctx, a); err != nil {
			t.Fatalf("OpenAttention %s: %v", a.ID, err)
		}
	}

	// An unknown reason must be rejected: the queue is typed, and a bad row would render as a
	// mystery the human cannot act on.
	if err := db.OpenAttention(ctx, core.Attention{ID: "bad", ProjectID: "p1", Reason: "invented"}); err == nil {
		t.Error("OpenAttention accepted an unknown reason")
	}

	open, err := db.ListOpenAttention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 3 {
		t.Fatalf("got %d open items, want 3", len(open))
	}
	if open[0].ID != "a2" || open[2].ID != "a3" {
		t.Errorf("queue is not oldest-first: %s, %s, %s", open[0].ID, open[1].ID, open[2].ID)
	}
	if open[1].Payload["files"] != float64(3) {
		t.Errorf("payload did not round trip: %+v", open[1].Payload)
	}

	if err := db.ResolveAttention(ctx, "a2"); err != nil {
		t.Fatal(err)
	}
	open, _ = db.ListOpenAttention(ctx)
	if len(open) != 2 {
		t.Errorf("got %d open items after resolving one, want 2", len(open))
	}

	forP1, err := db.ListOpenAttentionForProject(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(forP1) != 1 || forP1[0].ID != "a1" {
		t.Errorf("per-project queue = %+v", forP1)
	}
}

func TestProviderAvailability(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	if err := db.SetProviderUnavailable(ctx, core.ProviderAvailability{
		ProviderID: "claude-code", Model: "sonnet", Class: core.QuotaExhausted,
		Until: now.Add(time.Hour), Note: "five_hour window exhausted",
	}); err != nil {
		t.Fatal(err)
	}

	unavailable, err := db.IsUnavailable(ctx, "claude-code", "sonnet", now)
	if err != nil || !unavailable {
		t.Errorf("IsUnavailable = %v, %v; want true", unavailable, err)
	}
	// After the cooldown expires it becomes available again without any sweep.
	unavailable, err = db.IsUnavailable(ctx, "claude-code", "sonnet", now.Add(2*time.Hour))
	if err != nil || unavailable {
		t.Errorf("IsUnavailable after expiry = %v, %v; want false", unavailable, err)
	}
	// An unrecorded model is available.
	unavailable, err = db.IsUnavailable(ctx, "claude-code", "opus", now)
	if err != nil || unavailable {
		t.Errorf("IsUnavailable for an unrecorded model = %v, %v; want false", unavailable, err)
	}

	rows, err := db.ListUnavailable(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Class != core.QuotaExhausted || rows[0].Note != "five_hour window exhausted" {
		t.Errorf("ListUnavailable = %+v", rows)
	}

	// Re-recording the same model replaces rather than duplicating.
	if err := db.SetProviderUnavailable(ctx, core.ProviderAvailability{
		ProviderID: "claude-code", Model: "sonnet", Class: core.RateLimited,
		Until: now.Add(5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	rows, _ = db.ListUnavailable(ctx, now)
	if len(rows) != 1 || rows[0].Class != core.RateLimited {
		t.Errorf("re-recording produced %+v", rows)
	}

	if err := db.ClearProviderAvailability(ctx, "claude-code", "sonnet"); err != nil {
		t.Fatal(err)
	}
	rows, _ = db.ListUnavailable(ctx, now)
	if len(rows) != 0 {
		t.Errorf("cooldown survived being cleared: %+v", rows)
	}
}

func TestSummaryUpsert(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTicket(ctx, testTicket("t1", "p1", core.StateReview)); err != nil {
		t.Fatal(err)
	}
	run := core.Run{ID: "r1", TicketID: "t1", HostID: "local", ProviderID: "p", Model: "m",
		State: core.StateReview, StartedAt: time.Unix(1, 0)}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	s := core.Summary{TicketID: "t1", RunID: "r1", Branch: "gravy/t1",
		Commits:   []string{"abc", "def"},
		Files:     []core.FileChange{{Path: "main.go", Status: "modified", Additions: 10, Deletions: 2}},
		Narrative: "generated from the diff", Assumptions: []string{"assumed X"},
		CreatedAt: time.Unix(1700000000, 0).UTC()}
	if err := db.PutSummary(ctx, s); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetSummary(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Commits) != 2 || len(got.Files) != 1 || got.Files[0].Additions != 10 {
		t.Errorf("summary did not round trip: %+v", got)
	}
	if len(got.Assumptions) != 1 {
		t.Errorf("assumptions did not round trip: %+v", got.Assumptions)
	}

	// A re-run replaces the summary rather than colliding on the primary key.
	s.Narrative = "second pass"
	if err := db.PutSummary(ctx, s); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	got, _ = db.GetSummary(ctx, "t1")
	if got.Narrative != "second pass" {
		t.Errorf("narrative = %q, want the replacement", got.Narrative)
	}
}

func TestForeignKeysRejectOrphans(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	// A ticket in a project that does not exist must be refused, not silently stored.
	if err := db.CreateTicket(ctx, testTicket("t1", "ghost", core.StateBacklog)); err == nil {
		t.Error("a ticket was created against a nonexistent project")
	}
}

func TestValidationsRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTicket(ctx, testTicket("t1", "p1", core.StateValidating)); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, core.Run{ID: "r1", TicketID: "t1", HostID: "local",
		ProviderID: "p", Model: "m", State: core.StateValidating, StartedAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}

	if err := db.AddValidation(ctx, "v1", "r1", "build", 0, 900, "/logs/build.log"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddValidation(ctx, "v2", "r1", "test", 1, 4200, "/logs/test.log"); err != nil {
		t.Fatal(err)
	}

	got, err := db.ListValidations(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d validations, want 2", len(got))
	}
	// Order matters: steps stop at the first failed required step, so the sequence is evidence.
	if got[0].Step != "build" || got[1].Step != "test" || got[1].ExitCode != 1 {
		t.Errorf("validations = %+v", got)
	}
}

// TestResolveAttentionForTicket covers the queue telling the truth after a ticket moves on.
//
// A stale row is as corrosive as a missing one: either way the queue stops matching reality, and
// a queue that does not match reality stops being read.
func TestResolveAttentionForTicket(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	if err := db.CreateProject(ctx, testProject("p1", "gravy")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"t1", "t2"} {
		if err := db.CreateTicket(ctx, testTicket(id, "p1", core.StateReview)); err != nil {
			t.Fatal(err)
		}
	}
	items := []core.Attention{
		{ID: "a1", ProjectID: "p1", TicketID: "t1", Reason: core.ReasonReviewPending},
		{ID: "a2", ProjectID: "p1", TicketID: "t1", Reason: core.ReasonMergeConflict},
		{ID: "a3", ProjectID: "p1", TicketID: "t2", Reason: core.ReasonReviewPending},
	}
	for _, a := range items {
		if err := db.OpenAttention(ctx, a); err != nil {
			t.Fatalf("OpenAttention %s: %v", a.ID, err)
		}
	}

	n, err := db.ResolveAttentionForTicket(ctx, "t1")
	if err != nil {
		t.Fatalf("ResolveAttentionForTicket: %v", err)
	}
	if n != 2 {
		t.Errorf("resolved %d rows, want 2", n)
	}

	open, err := db.ListOpenAttention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != "a3" {
		t.Fatalf("open queue = %+v, want only a3: another ticket's rows were resolved", open)
	}

	// Resolving again is a no-op rather than an error, so a retried transition cannot fail on
	// work it already did.
	if n, err = db.ResolveAttentionForTicket(ctx, "t1"); err != nil || n != 0 {
		t.Errorf("second resolve = (%d, %v), want (0, nil)", n, err)
	}
}
