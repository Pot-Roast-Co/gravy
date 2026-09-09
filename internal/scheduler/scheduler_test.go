package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
)

// fakeStore is an in-memory Store. The scheduler only reads, so a map is enough and keeps the
// tests about scheduling rather than about SQL.
type fakeStore struct {
	projects map[string]core.Project
	tickets  map[string]core.Ticket
	deps     map[string][]string
}

func newStore() *fakeStore {
	return &fakeStore{
		projects: map[string]core.Project{},
		tickets:  map[string]core.Ticket{},
		deps:     map[string][]string{},
	}
}

func (f *fakeStore) addProject(p core.Project) *fakeStore { f.projects[p.ID] = p; return f }
func (f *fakeStore) addTicket(t core.Ticket) *fakeStore   { f.tickets[t.ID] = t; return f }

func (f *fakeStore) setState(id string, s core.State) {
	t := f.tickets[id]
	t.State = s
	f.tickets[id] = t
}

func (f *fakeStore) ListTicketsByState(_ context.Context, state core.State) ([]core.Ticket, error) {
	var out []core.Ticket
	for _, t := range f.tickets {
		if t.State == state {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeStore) GetTicket(_ context.Context, id string) (core.Ticket, error) {
	t, ok := f.tickets[id]
	if !ok {
		return core.Ticket{}, errNotFound(id)
	}
	return t, nil
}

func (f *fakeStore) GetProject(_ context.Context, id string) (core.Project, error) {
	p, ok := f.projects[id]
	if !ok {
		return core.Project{}, errNotFound(id)
	}
	return p, nil
}

func (f *fakeStore) ListProjects(context.Context) ([]core.Project, error) {
	out := make([]core.Project, 0, len(f.projects))
	for _, p := range f.projects {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeStore) CountActiveTickets(_ context.Context, projectID string) (int, error) {
	n := 0
	for _, t := range f.tickets {
		if t.ProjectID == projectID && core.IsActive(t.State) {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) CountActiveTicketsByRoute(_ context.Context, route core.Route) (int, error) {
	n := 0
	for _, t := range f.tickets {
		if t.Route == route && core.IsActive(t.State) {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) ListTickets(_ context.Context, projectID string) ([]core.Ticket, error) {
	var out []core.Ticket
	for _, t := range f.tickets {
		if t.ProjectID == projectID {
			out = append(out, t)
		}
	}
	sortTickets(out)
	return out, nil
}

func (f *fakeStore) DepsOf(_ context.Context, ticketID string) ([]string, error) {
	return f.deps[ticketID], nil
}

type notFoundErr string

func (e notFoundErr) Error() string { return string(e) + " not found" }
func errNotFound(id string) error   { return notFoundErr(id) }

// fakePool is a HostPool with declared capabilities and slot counts.
type fakePool struct {
	hosts []host.Host
	caps  map[string]core.Caps
}

func (p *fakePool) Hosts() []host.Host { return p.hosts }
func (p *fakePool) Caps(_ context.Context, h host.Host) (core.Caps, error) {
	return p.caps[h.ID()], nil
}

// newPool builds hosts with the given slot counts and capabilities.
func newPool(specs ...hostSpec) *fakePool {
	p := &fakePool{caps: map[string]core.Caps{}}
	for _, s := range specs {
		h := host.NewLocal(s.id, s.slots)
		for i := 0; i < s.busy; i++ {
			h.TryClaim()
		}
		p.hosts = append(p.hosts, h)
		p.caps[s.id] = core.Caps{OS: s.os, Arch: "arm64", Tools: s.tools}
	}
	return p
}

type hostSpec struct {
	id    string
	slots int
	busy  int
	os    string
	tools map[string]string
}

func mac(id string, slots int) hostSpec {
	return hostSpec{id: id, slots: slots, os: "darwin",
		tools: map[string]string{"git": "2.39", "go": "1.23", "xcodebuild": "15.2"}}
}

func linux(id string, slots int) hostSpec {
	return hostSpec{id: id, slots: slots, os: "linux",
		tools: map[string]string{"git": "2.39", "go": "1.23"}}
}

func project(id, slug string) core.Project {
	// RepoPath is set because a project without one cannot be scheduled at all: it is a place
	// for goals and notes, not somewhere work runs.
	return core.Project{
		ID: id, Slug: slug, RepoPath: "/repos/" + slug,
		TargetBranch: "main", MergeMode: core.LandMerge, MaxConcurrency: 1,
	}
}

func ticket(id, projectID string, state core.State, position float64) core.Ticket {
	return core.Ticket{
		ID: id, ProjectID: projectID, State: state, Position: position,
		Route: core.RouteImplementation, CreatedAt: time.Unix(1700000000, 0),
	}
}

func newScheduler(s Store, p HostPool) *Scheduler {
	return New(s, p, FixedRoute{ProviderID: "claude-code", Model: "sonnet"})
}

func assignedIDs(as []Assignment) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.TicketID
	}
	return out
}

// TestOrdering is AC1.
func TestOrdering(t *testing.T) {
	st := newStore().addProject(parallelProject("p1", "repo", 10))
	// Deliberately inserted out of order.
	st.addTicket(ticket("c", "p1", core.StateReady, 3))
	st.addTicket(ticket("a", "p1", core.StateReady, 1))
	st.addTicket(ticket("b", "p1", core.StateReady, 2))
	high := ticket("urgent", "p1", core.StateReady, 99)
	high.Priority = 5
	st.addTicket(high)

	got, err := newScheduler(st, newPool(mac("m1", 10))).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"urgent", "a", "b", "c"}
	gotIDs := assignedIDs(got)
	if len(gotIDs) != len(want) {
		t.Fatalf("assigned %v, want %v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Errorf("order = %v, want %v (priority DESC, then position ASC)", gotIDs, want)
			break
		}
	}
}

func parallelProject(id, slug string, cap int) core.Project {
	p := project(id, slug)
	p.ParallelMode = true
	p.MaxConcurrency = cap
	return p
}

// TestSerialProjectInReviewYieldsNothing is AC2 — the central test of this ticket.
//
// A ticket awaiting review holds its project even though no process is executing. This is the
// deliberate cost of serial mode: review latency, not agent speed, gates the repository's queue.
// Getting this wrong would let a second agent branch from a target that is about to change.
func TestSerialProjectInReviewYieldsNothing(t *testing.T) {
	st := newStore().addProject(project("p1", "repo"))
	st.addTicket(ticket("t1", "p1", core.StateReview, 1)) // waiting on the human
	st.addTicket(ticket("t2", "p1", core.StateReady, 2))  // wants to start

	sched := newScheduler(st, newPool(mac("m1", 8)))
	got, err := sched.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("assigned %v while a ticket awaits review; serial mode was violated", assignedIDs(got))
	}

	// AC6: the idle queue must name what it is waiting for.
	ex, err := sched.Explain(context.Background(), "t2")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Eligible {
		t.Error("Explain reports the held ticket as eligible")
	}
	if !strings.Contains(ex.Reason, "t1") {
		t.Errorf("reason %q does not name the blocking ticket", ex.Reason)
	}
	if !strings.Contains(ex.Reason, "review") {
		t.Errorf("reason %q does not say the blocker is awaiting review", ex.Reason)
	}
}

// TestSerialHoldsThroughEveryInFlightState: the hold is not special to Review.
func TestSerialHoldsThroughEveryInFlightState(t *testing.T) {
	for _, state := range []core.State{
		core.StateAssigned, core.StateRunning, core.StateValidating,
		core.StateReviewing, core.StateReview, core.StateLanding,
		core.StateBlocked, core.StateNeedsYou,
	} {
		t.Run(string(state), func(t *testing.T) {
			st := newStore().addProject(project("p1", "repo"))
			st.addTicket(ticket("blocker", "p1", state, 1))
			st.addTicket(ticket("waiting", "p1", core.StateReady, 2))

			got, err := newScheduler(st, newPool(mac("m1", 8))).Tick(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Errorf("a ticket in %s did not hold its serial project", state)
			}
		})
	}
}

// TestSerialReleasesOnDone is AC3.
func TestSerialReleasesOnDone(t *testing.T) {
	st := newStore().addProject(project("p1", "repo"))
	st.addTicket(ticket("t1", "p1", core.StateReview, 1))
	st.addTicket(ticket("t2", "p1", core.StateReady, 2))

	sched := newScheduler(st, newPool(mac("m1", 8)))
	if got, _ := sched.Tick(context.Background()); len(got) != 0 {
		t.Fatalf("assigned %v before the blocker was done", assignedIDs(got))
	}

	// The human approves and it merges.
	st.setState("t1", core.StateDone)

	got, err := sched.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TicketID != "t2" {
		t.Fatalf("assigned %v after the blocker merged, want [t2]", assignedIDs(got))
	}
}

// TestThreeSerialProjectsRunConcurrently is AC4, and the intended default shape: N repositories
// run N agents, one each, because separate repositories cannot collide.
func TestThreeSerialProjectsRunConcurrently(t *testing.T) {
	st := newStore()
	for _, p := range []string{"p1", "p2", "p3"} {
		st.addProject(project(p, "repo-"+p))
		st.addTicket(ticket(p+"-a", p, core.StateReady, 1))
		st.addTicket(ticket(p+"-b", p, core.StateReady, 2))
	}

	got, err := newScheduler(st, newPool(mac("m1", 8))).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("assigned %v, want exactly one ticket per project", assignedIDs(got))
	}

	seen := map[string]int{}
	for _, a := range got {
		seen[a.ProjectID]++
	}
	for p, n := range seen {
		if n != 1 {
			t.Errorf("project %s got %d assignments in one tick; serial mode allows one", p, n)
		}
	}
	if len(seen) != 3 {
		t.Errorf("assignments covered %d projects, want 3", len(seen))
	}
}

// TestParallelMode is AC5.
func TestParallelMode(t *testing.T) {
	st := newStore().addProject(parallelProject("p1", "repo", 3))
	for _, id := range []string{"a", "b", "c", "d"} {
		st.addTicket(ticket(id, "p1", core.StateReady, float64(len(id))))
	}

	got, err := newScheduler(st, newPool(mac("m1", 8))).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("assigned %d tickets, want 3 (max_concurrency)", len(got))
	}
}

// TestParallelReviewDoesNotHoldTheProject is the other half of AC5, and the reason a project
// opts into parallel mode at all.
func TestParallelReviewDoesNotHoldTheProject(t *testing.T) {
	st := newStore().addProject(parallelProject("p1", "repo", 2))
	st.addTicket(ticket("in-review", "p1", core.StateReview, 1))
	st.addTicket(ticket("running", "p1", core.StateRunning, 2))
	st.addTicket(ticket("next", "p1", core.StateReady, 3))

	got, err := newScheduler(st, newPool(mac("m1", 8))).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// One run is executing, the cap is two, and the review does not count.
	if len(got) != 1 || got[0].TicketID != "next" {
		t.Fatalf("assigned %v, want [next]: work awaiting review must not count against the cap",
			assignedIDs(got))
	}
}

// TestDependencyMustBeDone is AC7.
//
// Done, not approved: a dependent ticket branches from the target branch, so until its
// dependency has actually merged, the work it depends on is not there to build on.
func TestDependencyMustBeDone(t *testing.T) {
	st := newStore().addProject(parallelProject("p1", "repo", 5))
	st.addTicket(ticket("dep", "p1", core.StateReview, 1))
	st.addTicket(ticket("dependent", "p1", core.StateReady, 2))
	st.deps["dependent"] = []string{"dep"}

	sched := newScheduler(st, newPool(mac("m1", 8)))
	got, err := sched.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a.TicketID == "dependent" {
			t.Fatal("a ticket was assigned while its dependency was only in review, not merged")
		}
	}

	ex, _ := sched.Explain(context.Background(), "dependent")
	if !strings.Contains(ex.Reason, "dep") {
		t.Errorf("reason %q does not name the dependency", ex.Reason)
	}

	st.setState("dep", core.StateDone)
	got, err = sched.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range got {
		if a.TicketID == "dependent" {
			found = true
		}
	}
	if !found {
		t.Error("the ticket was not assigned after its dependency reached Done")
	}
}

// TestRequirementsExcludeHost is AC8.
func TestRequirementsExcludeHost(t *testing.T) {
	p := project("p1", "ios-app")
	p.Requirements = core.Requirements{OS: []string{"darwin"}, Tools: map[string]string{"xcodebuild": ""}}
	st := newStore().addProject(p)
	st.addTicket(ticket("t1", "p1", core.StateReady, 1))

	sched := newScheduler(st, newPool(linux("lin", 4), mac("mac", 4)))
	got, err := sched.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("assigned %d, want 1", len(got))
	}
	if got[0].HostID != "mac" {
		t.Errorf("host = %q, want mac", got[0].HostID)
	}

	// The exclusion must be explained, not silent.
	trace := Trace(got[0].Why)
	if !strings.Contains(trace, "lin") {
		t.Errorf("trace does not mention the excluded host:\n%s", trace)
	}
	if !strings.Contains(trace, "linux") && !strings.Contains(trace, "darwin") {
		t.Errorf("trace does not say why the host was excluded:\n%s", trace)
	}
}

func TestTicketRequirementsAddToProjectRequirements(t *testing.T) {
	st := newStore().addProject(project("p1", "repo"))
	tk := ticket("t1", "p1", core.StateReady, 1)
	tk.Requirements = core.Requirements{Tools: map[string]string{"xcodebuild": ""}}
	st.addTicket(tk)

	got, err := newScheduler(st, newPool(linux("lin", 4), mac("mac", 4))).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].HostID != "mac" {
		t.Fatalf("assigned %+v, want the mac (only host with xcodebuild)", got)
	}
}

// TestIdlePreferredThenLeastBusy is AC9.
func TestIdlePreferredThenLeastBusy(t *testing.T) {
	t.Run("idle wins", func(t *testing.T) {
		st := newStore().addProject(project("p1", "repo"))
		st.addTicket(ticket("t1", "p1", core.StateReady, 1))

		pool := newPool(
			hostSpec{id: "busy", slots: 4, busy: 3, os: "darwin", tools: map[string]string{"git": "2"}},
			hostSpec{id: "idle", slots: 4, busy: 0, os: "darwin", tools: map[string]string{"git": "2"}},
		)
		got, err := newScheduler(st, pool).Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got[0].HostID != "idle" {
			t.Errorf("host = %q, want idle", got[0].HostID)
		}
	})

	t.Run("least busy among busy", func(t *testing.T) {
		st := newStore().addProject(project("p1", "repo"))
		st.addTicket(ticket("t1", "p1", core.StateReady, 1))

		pool := newPool(
			hostSpec{id: "heavy", slots: 8, busy: 6, os: "darwin", tools: map[string]string{"git": "2"}},
			hostSpec{id: "lighter", slots: 8, busy: 2, os: "darwin", tools: map[string]string{"git": "2"}},
		)
		got, err := newScheduler(st, pool).Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got[0].HostID != "lighter" {
			t.Errorf("host = %q, want lighter", got[0].HostID)
		}
	})
}

// TestHostOverrideBypassesPreferenceButNotRequirements is AC10.
func TestHostOverrideBypassesPreferenceButNotRequirements(t *testing.T) {
	t.Run("override beats preference", func(t *testing.T) {
		st := newStore().addProject(project("p1", "repo"))
		tk := ticket("t1", "p1", core.StateReady, 1)
		tk.HostOverride = "busy"
		st.addTicket(tk)

		pool := newPool(
			hostSpec{id: "busy", slots: 4, busy: 3, os: "darwin", tools: map[string]string{"git": "2"}},
			hostSpec{id: "idle", slots: 4, busy: 0, os: "darwin", tools: map[string]string{"git": "2"}},
		)
		got, err := newScheduler(st, pool).Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].HostID != "busy" {
			t.Fatalf("assigned %+v, want the explicitly overridden host", got)
		}
	})

	t.Run("override cannot defeat a hard requirement", func(t *testing.T) {
		// A host without Xcode cannot build an iOS app however firmly a human asserts it.
		p := project("p1", "ios-app")
		p.Requirements = core.Requirements{Tools: map[string]string{"xcodebuild": ""}}
		st := newStore().addProject(p)
		tk := ticket("t1", "p1", core.StateReady, 1)
		tk.HostOverride = "lin"
		st.addTicket(tk)

		sched := newScheduler(st, newPool(linux("lin", 4), mac("mac", 4)))
		got, err := sched.Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("assigned %+v; an override must not bypass a hard requirement", got)
		}
		ex, _ := sched.Explain(context.Background(), "t1")
		if !strings.Contains(ex.Reason, "override") {
			t.Errorf("reason %q does not explain the override failure", ex.Reason)
		}
	})
}

// TestEveryReadyTicketIsExplained is AC11: an idle queue is always explained.
func TestEveryReadyTicketIsExplained(t *testing.T) {
	st := newStore()
	st.addProject(project("p1", "held"))
	st.addTicket(ticket("blocker", "p1", core.StateRunning, 1))
	st.addTicket(ticket("held", "p1", core.StateReady, 2))

	p2 := project("p2", "needs-xcode")
	p2.Requirements = core.Requirements{Tools: map[string]string{"xcodebuild": ""}}
	st.addProject(p2)
	st.addTicket(ticket("no-host", "p2", core.StateReady, 1))

	st.addProject(project("p3", "waiting-on-dep"))
	st.addTicket(ticket("dep", "p3", core.StateBacklog, 1))
	st.addTicket(ticket("dependent", "p3", core.StateReady, 2))
	st.deps["dependent"] = []string{"dep"}

	sched := newScheduler(st, newPool(linux("lin", 4)))
	explanations, err := sched.ExplainAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(explanations) != 3 {
		t.Fatalf("explained %d ready tickets, want 3", len(explanations))
	}
	for _, ex := range explanations {
		if ex.Reason == "" {
			t.Errorf("ticket %s has no reason; an idle queue must always be explained", ex.TicketID)
		}
		if ex.Eligible {
			t.Errorf("ticket %s reported eligible but nothing was assigned", ex.TicketID)
		}
		if len(ex.Why) == 0 {
			t.Errorf("ticket %s has an empty decision trace", ex.TicketID)
		}
	}
}

// TestTickStartsNothing is AC12.
func TestTickStartsNothing(t *testing.T) {
	st := newStore().addProject(project("p1", "repo"))
	st.addTicket(ticket("t1", "p1", core.StateReady, 1))

	sched := newScheduler(st, newPool(mac("m1", 4)))
	before := st.tickets["t1"]

	got, err := sched.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("assigned %d, want 1", len(got))
	}

	// Nothing may be mutated: the caller starts what it is given, which keeps the decision
	// reproducible and lets it be shown to a human before anything happens.
	if st.tickets["t1"].State != before.State {
		t.Errorf("Tick changed ticket state from %s to %s", before.State, st.tickets["t1"].State)
	}

	// Ticking twice with unchanged state must produce the same answer.
	again, err := sched.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].TicketID != got[0].TicketID || again[0].HostID != got[0].HostID {
		t.Error("two ticks over identical state produced different assignments")
	}
}

// TestWorkerSlotsBoundAssignments: the global pool is shared across projects.
func TestWorkerSlotsBoundAssignments(t *testing.T) {
	st := newStore()
	for _, p := range []string{"p1", "p2", "p3", "p4"} {
		st.addProject(project(p, "repo-"+p))
		st.addTicket(ticket(p+"-a", p, core.StateReady, 1))
	}

	// Four eligible tickets in four projects, but only two worker slots.
	got, err := newScheduler(st, newPool(mac("m1", 2))).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("assigned %d tickets from a 2-slot pool, want 2", len(got))
	}
}

func TestRouteResolutionFailureLeavesTicketReady(t *testing.T) {
	st := newStore().addProject(project("p1", "repo"))
	st.addTicket(ticket("t1", "p1", core.StateReady, 1))

	// No provider configured: the route resolves to nothing.
	sched := New(st, newPool(mac("m1", 4)), FixedRoute{})
	got, err := sched.Tick(context.Background())
	if err != nil {
		t.Fatalf("a route failure must not fail the tick: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("assigned %+v with no provider available", got)
	}
	ex, _ := sched.Explain(context.Background(), "t1")
	if !strings.Contains(ex.Reason, "provider") {
		t.Errorf("reason %q does not explain the routing failure", ex.Reason)
	}
}

func TestExplainForNonReadyTicket(t *testing.T) {
	st := newStore().addProject(project("p1", "repo"))
	st.addTicket(ticket("running", "p1", core.StateRunning, 1))
	st.addTicket(ticket("draft", "p1", core.StateDraft, 2))

	sched := newScheduler(st, newPool(mac("m1", 4)))

	ex, err := sched.Explain(context.Background(), "running")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ex.Reason, "in flight") {
		t.Errorf("reason for a running ticket = %q", ex.Reason)
	}

	ex, err = sched.Explain(context.Background(), "draft")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ex.Reason, "not ready") {
		t.Errorf("reason for a draft ticket = %q", ex.Reason)
	}
}

func TestAssignmentCarriesItsReasoning(t *testing.T) {
	st := newStore().addProject(project("p1", "repo"))
	st.addTicket(ticket("t1", "p1", core.StateReady, 1))

	got, err := newScheduler(st, newPool(mac("m1", 4))).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	trace := Trace(got[0].Why)
	// "Why this host, why this model" must be answerable from the assignment alone.
	for _, want := range []string{"t1", "dependencies satisfied", "serial", "host m1", "claude-code"} {
		if !strings.Contains(trace, want) {
			t.Errorf("trace is missing %q:\n%s", want, trace)
		}
	}
}

// TestRouteCapsBucketAgents covers the bucket model: a route is a pool of agent capacity that
// tickets ask for by name, and its limit is fleet-wide rather than per repository — what it
// rations is agents, not repositories.
func TestRouteCapsBucketAgents(t *testing.T) {
	st := newStore().
		addProject(parallelProject("p1", "repo-one", 10)).
		addProject(parallelProject("p2", "repo-two", 10))

	// Four planning tickets spread over two repositories.
	for i, project := range []string{"p1", "p1", "p2", "p2"} {
		tk := ticket(fmt.Sprintf("plan-%d", i), project, core.StateReady, float64(i))
		tk.Route = core.RoutePlanning
		st.addTicket(tk)
	}
	// And two on another route, which the planning cap must not touch.
	for i, project := range []string{"p1", "p2"} {
		tk := ticket(fmt.Sprintf("impl-%d", i), project, core.StateReady, float64(10+i))
		tk.Route = core.RouteImplementation
		st.addTicket(tk)
	}

	s := newScheduler(st, newPool(mac("m1", 10))).
		WithRouteCaps(map[core.Route]int{core.RoutePlanning: 2})

	got, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var planning, other int
	for _, a := range got {
		if st.tickets[a.TicketID].Route == core.RoutePlanning {
			planning++
			continue
		}
		other++
	}

	if planning != 2 {
		t.Errorf("assigned %d planning tickets, want the route's cap of 2", planning)
	}
	// The cap spans projects: two of the four planning tickets waited even though their
	// repositories were free.
	if other != 2 {
		t.Errorf("assigned %d tickets on uncapped routes, want 2 — a cap on one route must not "+
			"stall another", other)
	}
}

// TestRouteCapCountsWorkAlreadyInFlight is the half a per-tick counter alone would miss.
func TestRouteCapCountsWorkAlreadyInFlight(t *testing.T) {
	st := newStore().addProject(parallelProject("p1", "repo", 10))

	running := ticket("already-going", "p1", core.StateRunning, 1)
	running.Route = core.RoutePlanning
	st.addTicket(running)

	waiting := ticket("next-up", "p1", core.StateReady, 2)
	waiting.Route = core.RoutePlanning
	st.addTicket(waiting)

	s := newScheduler(st, newPool(mac("m1", 10))).
		WithRouteCaps(map[core.Route]int{core.RoutePlanning: 1})

	got, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("assigned %v, want nothing: the route's one slot is already occupied", assignedIDs(got))
	}

	// And the reason is explainable rather than an unexplained idle queue.
	ex, err := s.Explain(context.Background(), "next-up")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Eligible {
		t.Error("a ticket blocked by its route cap reports itself eligible")
	}
	if !strings.Contains(ex.Reason, "planning") || !strings.Contains(ex.Reason, "limit") {
		t.Errorf("reason = %q, want it to name the route and its limit", ex.Reason)
	}
}

// TestUncappedRoutesAreUnchanged: a cap nobody asked for would silently stall a queue.
func TestUncappedRoutesAreUnchanged(t *testing.T) {
	st := newStore().addProject(parallelProject("p1", "repo", 10))
	for i := 0; i < 3; i++ {
		tk := ticket(fmt.Sprintf("t%d", i), "p1", core.StateReady, float64(i))
		tk.Route = core.RouteImplementation
		st.addTicket(tk)
	}

	// A cap on a different route entirely.
	s := newScheduler(st, newPool(mac("m1", 10))).
		WithRouteCaps(map[core.Route]int{core.RoutePlanning: 1})

	got, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("assigned %d, want all 3: an uncapped route is limited only by the pool", len(got))
	}
}
