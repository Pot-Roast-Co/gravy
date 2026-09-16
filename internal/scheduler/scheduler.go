package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
)

// Assignment is a decision to run a ticket on a host with a provider and model.
//
// Tick returns assignments; it never starts anything. Separating the decision from the execution
// is what makes scheduling testable and, more importantly, explainable: every assignment carries
// the reasoning that produced it.
type Assignment struct {
	TicketID   string
	ProjectID  string
	HostID     string
	ProviderID string
	Model      string
	// Why is surfaced verbatim in the TUI. There is no AI in scheduling; every decision is a
	// recorded chain of dull, checkable steps.
	Why []string
}

// Explanation answers "why is this ticket not running?" from recorded state.
type Explanation struct {
	TicketID string
	State    core.State
	// Eligible reports whether the ticket could be assigned right now.
	Eligible bool
	// Reason is a single readable sentence. An idle queue is always explained.
	Reason string
	// Why is the full decision trace.
	Why []string
}

// Store is the subset of the store the scheduler reads.
//
// Narrow by design: the scheduler decides, it does not mutate. Taking a read-only view makes
// "Tick starts nothing" structural rather than a promise.
type Store interface {
	ListTicketsByState(ctx context.Context, state core.State) ([]core.Ticket, error)
	GetTicket(ctx context.Context, id string) (core.Ticket, error)
	GetProject(ctx context.Context, id string) (core.Project, error)
	CountActiveTickets(ctx context.Context, projectID string) (int, error)
	CountActiveTicketsByRoute(ctx context.Context, route core.Route) (int, error)
	ListTickets(ctx context.Context, projectID string) ([]core.Ticket, error)
	DepsOf(ctx context.Context, ticketID string) ([]string, error)
}

// Router resolves a route to a concrete provider and model.
//
// GR-016 supplies the real implementation with fallbacks and cooldowns. M0 runs a single
// hardcoded route (see FixedRoute), which is why this is an interface rather than a dependency.
type Router interface {
	Resolve(ctx context.Context, route core.Route, c core.Constraints) (core.Choice, error)
}

// FixedRoute is the M0 router: one provider and model for everything.
//
// It exists so the scheduler can be finished and tested against the real interface before GR-016
// lands, rather than being written twice.
type FixedRoute struct {
	ProviderID string
	Model      string
}

// Resolve returns the fixed choice, recording that no real routing happened.
func (f FixedRoute) Resolve(_ context.Context, route core.Route, _ core.Constraints) (core.Choice, error) {
	if f.ProviderID == "" {
		return core.Choice{}, fmt.Errorf("no provider configured for route %q", route)
	}
	return core.Choice{
		ProviderID: f.ProviderID,
		Model:      f.Model,
		Why:        []string{fmt.Sprintf("route %q resolved to the single configured choice %s/%s", route, f.ProviderID, f.Model)},
	}, nil
}

// HostPool is the set of hosts work can be placed on.
type HostPool interface {
	Hosts() []host.Host
	// Caps returns a host's capabilities, cached by the caller — capability probing shells
	// out, and the scheduler ticks far too often to do that each time.
	Caps(ctx context.Context, h host.Host) (core.Caps, error)
}

// Scheduler decides what runs next.
type Scheduler struct {
	store  Store
	hosts  HostPool
	router Router
	// routeCaps limits how many tickets on a route may run at once. Absent means unlimited,
	// bounded only by the worker pool.
	routeCaps map[core.Route]int
}

// New returns a scheduler.
func New(s Store, h HostPool, r Router) *Scheduler {
	return &Scheduler{store: s, hosts: h, router: r}
}

// WithRouteCaps limits per-route concurrency: how many agents of each kind may run at once.
func (s *Scheduler) WithRouteCaps(caps map[core.Route]int) *Scheduler {
	s.routeCaps = caps
	return s
}

// Tick returns the assignments that should be started now.
//
// It is pure with respect to execution: nothing is launched, nothing is written. The caller
// starts what it is given, which keeps the decision reproducible and the reasoning auditable.
func (s *Scheduler) Tick(ctx context.Context) ([]Assignment, error) {
	tickets, err := s.store.ListTicketsByState(ctx, core.StateReady)
	if err != nil {
		return nil, fmt.Errorf("scheduler: list ready tickets: %w", err)
	}
	sortTickets(tickets)

	pool, err := s.snapshotHosts(ctx)
	if err != nil {
		return nil, err
	}

	// Projects assigned during this tick, so a serial project cannot be handed two tickets
	// before either has started.
	claimed := map[string]int{}
	// Routes claimed during this tick, so a bucket cannot be handed more work than it has
	// room for before any of it has started.
	claimedRoutes := map[core.Route]int{}

	var out []Assignment
	for _, t := range tickets {
		decision, err := s.consider(ctx, t, pool, claimed, claimedRoutes)
		if err != nil {
			return nil, err
		}
		if decision.assignment == nil {
			continue
		}
		out = append(out, *decision.assignment)
		claimed[t.ProjectID]++
		claimedRoutes[t.Route]++
		pool.claim(decision.assignment.HostID)
	}
	return out, nil
}

// Explain answers why a ticket is or is not running.
func (s *Scheduler) Explain(ctx context.Context, ticketID string) (Explanation, error) {
	t, err := s.store.GetTicket(ctx, ticketID)
	if err != nil {
		return Explanation{}, fmt.Errorf("scheduler: explain %q: %w", ticketID, err)
	}

	ex := Explanation{TicketID: ticketID, State: t.State}

	if t.State != core.StateReady {
		ex.Reason = fmt.Sprintf("not scheduled: the ticket is %s, not ready", t.State)
		if core.IsActive(t.State) {
			ex.Reason = fmt.Sprintf("already in flight: the ticket is %s", t.State)
		}
		ex.Why = []string{ex.Reason}
		return ex, nil
	}

	pool, err := s.snapshotHosts(ctx)
	if err != nil {
		return Explanation{}, err
	}
	decision, err := s.consider(ctx, t, pool, map[string]int{}, map[core.Route]int{})
	if err != nil {
		return Explanation{}, err
	}

	ex.Why = decision.why
	ex.Eligible = decision.assignment != nil
	if ex.Eligible {
		ex.Reason = fmt.Sprintf("ready to start on %s", decision.assignment.HostID)
	} else {
		ex.Reason = decision.blockedBy
	}
	return ex, nil
}

// ExplainAll explains every Ready ticket, so no queue is ever unexplained.
func (s *Scheduler) ExplainAll(ctx context.Context) ([]Explanation, error) {
	tickets, err := s.store.ListTicketsByState(ctx, core.StateReady)
	if err != nil {
		return nil, fmt.Errorf("scheduler: list ready tickets: %w", err)
	}
	sortTickets(tickets)

	out := make([]Explanation, 0, len(tickets))
	for _, t := range tickets {
		ex, err := s.Explain(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ex)
	}
	return out, nil
}

// decision is the outcome of considering one ticket.
type decision struct {
	assignment *Assignment
	why        []string
	blockedBy  string
}

// consider runs the algorithm from ARCHITECTURE.md §4.5 for a single ticket.
//
// Every step appends to why, whether or not it is the step that blocks. A trace that only
// records the failure cannot answer "why this host and model" for the tickets that did run.
func (s *Scheduler) consider(ctx context.Context, t core.Ticket, pool *hostSnapshot, claimed map[string]int, claimedRoutes map[core.Route]int) (decision, error) {
	var why []string
	blocked := func(reason string) decision {
		why = append(why, reason)
		return decision{why: why, blockedBy: reason}
	}

	project, err := s.store.GetProject(ctx, t.ProjectID)
	if err != nil {
		// One unreadable ticket must not stop the tick. Returning an error here meant a single
		// row whose project had gone — orphaned by a delete that bypassed the cascade — failed
		// scheduling for every project, every two seconds, with the queue screen reporting
		// only that it could not load.
		return decision{
			why:       []string{fmt.Sprintf("ticket %s: %v", t.ID, err)},
			blockedBy: fmt.Sprintf("its project is missing: %v", err),
		}, nil
	}
	why = append(why, fmt.Sprintf("ticket %s in project %s, priority %d, position %.0f",
		t.ID, project.Slug, t.Priority, t.Position))

	// 0. An archived project queues nothing new. Its Ready tickets stay Ready — unarchiving is
	//    all it takes for the next tick to pick them up — and nothing already in flight is
	//    affected, because this runs only for tickets that have yet to start.
	if project.Archived {
		return blocked("project archived"), nil
	}

	// 0b. A project with no repository has nowhere to work.
	if strings.TrimSpace(project.RepoPath) == "" {
		return blocked(fmt.Sprintf("project %s has no repository yet", project.Slug)), nil
	}

	// 1. Dependencies must be Done, not merely approved. A dependent ticket branching from
	//    target before its dependency merged would not contain the work it depends on.
	blocker, err := s.blockingDependency(ctx, t)
	if err != nil {
		return decision{}, err
	}
	if blocker != "" {
		return blocked(blocker), nil
	}
	why = append(why, "dependencies satisfied")

	// 2. Project availability. This is the heart of the scheduler.
	reason, available, err := s.projectAvailable(ctx, project, claimed[t.ProjectID])
	if err != nil {
		return decision{}, err
	}
	if !available {
		return blocked(reason), nil
	}
	why = append(why, reason)

	// 2b. Route capacity. A route is a bucket of agent capacity that tickets ask for by name,
	//     so its limit is fleet-wide: it caps how many agents of that kind run at once,
	//     wherever the work came from.
	reason, available, err = s.routeAvailable(ctx, t.Route, claimedRoutes[t.Route])
	if err != nil {
		return decision{}, err
	}
	if !available {
		return blocked(reason), nil
	}
	why = append(why, reason)

	// 3-5. Host filtering: project requirements, then ticket requirements, then free slots.
	eligible, rejections := pool.eligibleOn(project.HostID, project.Requirements, t.Requirements)
	why = append(why, rejections...)
	if len(eligible) == 0 {
		return blocked("no host satisfies the project's requirements"), nil
	}

	// 7. An explicit human override is honoured unconditionally over preference — but not over
	//    the hard requirement filters above, since a host without Xcode cannot build an iOS
	//    app however firmly a human asserts otherwise.
	var chosen *hostState
	if t.HostOverride != "" {
		for _, h := range eligible {
			if h.id == t.HostOverride {
				chosen = h
				why = append(why, fmt.Sprintf("host %s chosen by explicit override", h.id))
				break
			}
		}
		if chosen == nil {
			return blocked(fmt.Sprintf("host override %q is not eligible for this ticket", t.HostOverride)), nil
		}
	} else {
		// 6. Prefer an idle host, otherwise the least busy.
		chosen = leastBusy(eligible)
		if chosen == nil {
			return blocked("every eligible host is at capacity"), nil
		}
		if chosen.used == 0 {
			why = append(why, fmt.Sprintf("host %s chosen: idle", chosen.id))
		} else {
			why = append(why, fmt.Sprintf("host %s chosen: least busy (%d of %d slots used)",
				chosen.id, chosen.used, chosen.total))
		}
	}
	if chosen.used >= chosen.total {
		return blocked(fmt.Sprintf("host %s has no free worker slots (%d of %d used)",
			chosen.id, chosen.used, chosen.total)), nil
	}

	// 8. Resolve the route to a concrete provider and model.
	route := t.Route
	if route == "" {
		route = core.RouteImplementation
	}
	// The project's own bucket table wins over the global one, which is what makes "this repo
	// is reviewed by a local model" a project setting rather than a fleet-wide one.
	choice, err := s.router.Resolve(ctx, route, core.Constraints{HostID: chosen.id, Routes: project.Routes})
	if err != nil {
		// A route that resolves to nothing leaves the ticket Ready with the reason recorded,
		// rather than failing it: the models may be available again in a minute.
		return blocked(fmt.Sprintf("no provider available for route %q: %v", route, err)), nil
	}
	why = append(why, choice.Why...)

	// 9. The chosen host must actually have that agent installed.
	//
	// Capabilities record which CLIs each machine has, and nothing consulted them: a ticket
	// routed to claude-code on a machine with only codex was assigned anyway and died at
	// "cli not found" — a burnt run, a consumed slot, and a failure that reads like a provider
	// outage rather than a machine missing a program.
	if len(chosen.caps.Providers) > 0 && !chosen.caps.Providers[choice.ProviderID] {
		return blocked(fmt.Sprintf("host %s does not have %s installed (route %q resolved to %s/%s)",
			chosen.id, choice.ProviderID, route, choice.ProviderID, choice.Model)), nil
	}
	why = append(why, fmt.Sprintf("host %s has %s installed", chosen.id, choice.ProviderID))

	return decision{
		assignment: &Assignment{
			TicketID:   t.ID,
			ProjectID:  t.ProjectID,
			HostID:     chosen.id,
			ProviderID: choice.ProviderID,
			Model:      choice.Model,
			Why:        why,
		},
		why: why,
	}, nil
}

// blockingDependency returns a reason if any dependency is not yet Done.
//
// Done, specifically — not Review, not approved. A ticket whose dependency has been approved but
// not merged would branch from a target that does not contain the work it depends on.
func (s *Scheduler) blockingDependency(ctx context.Context, t core.Ticket) (string, error) {
	deps, err := s.store.DepsOf(ctx, t.ID)
	if err != nil {
		return "", fmt.Errorf("scheduler: dependencies of %q: %w", t.ID, err)
	}
	for _, id := range deps {
		dep, err := s.store.GetTicket(ctx, id)
		if err != nil {
			return "", fmt.Errorf("scheduler: dependency %q of %q: %w", id, t.ID, err)
		}
		if dep.State != core.StateDone {
			return fmt.Sprintf("waiting on dependency %s (%s, not yet done)", dep.ID, dep.State), nil
		}
	}
	return "", nil
}

// projectAvailable applies the serial or parallel availability rule.
func (s *Scheduler) projectAvailable(ctx context.Context, p core.Project, claimedThisTick int) (string, bool, error) {
	if !p.ParallelMode {
		// Serial: available only at zero tickets in flight — anything after Ready and before
		// Done or Rejected. The next ticket starts only once the previous one has MERGED,
		// which deliberately makes review latency the throughput gate for this repository.
		active, err := s.store.CountActiveTickets(ctx, p.ID)
		if err != nil {
			return "", false, fmt.Errorf("scheduler: project %q: %w", p.Slug, err)
		}
		if active > 0 || claimedThisTick > 0 {
			blocking, err := s.blockingTicket(ctx, p.ID)
			if err != nil {
				return "", false, err
			}
			return blocking, false, nil
		}
		return fmt.Sprintf("project %s is serial and has nothing in flight", p.Slug), true, nil
	}

	// Parallel: only tickets whose runs are executing count. Work awaiting review does not
	// hold the project, which is the whole reason a project opts in.
	running, err := s.runningCount(ctx, p.ID)
	if err != nil {
		return "", false, err
	}
	cap := p.MaxConcurrency
	if cap < 1 {
		cap = 1
	}
	if running+claimedThisTick >= cap {
		return fmt.Sprintf("project %s is at its parallel limit (%d of %d running)",
			p.Slug, running+claimedThisTick, cap), false, nil
	}
	return fmt.Sprintf("project %s is parallel (%d of %d running)", p.Slug, running+claimedThisTick, cap), true, nil
}

// blockingTicket names the in-flight ticket holding a serial project.
//
// Naming it is required, not cosmetic: serial mode costs throughput by design, and the cost is
// only acceptable if the queue can always say what it is waiting for. An unexplained idle queue
// looks like a bug and destroys trust in the scheduler.
func (s *Scheduler) blockingTicket(ctx context.Context, projectID string) (string, error) {
	tickets, err := s.store.ListTickets(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("scheduler: project %q tickets: %w", projectID, err)
	}
	for _, t := range tickets {
		if core.IsActive(t.State) {
			verb := "in flight"
			if t.State == core.StateReview {
				verb = "awaiting your review"
			}
			return fmt.Sprintf("project serialized; %s %s (%s)", t.ID, verb, t.State), nil
		}
	}
	return "project serialized; another ticket was assigned this tick", nil
}

// runningCount counts a project's tickets whose agents are actually executing.
func (s *Scheduler) runningCount(ctx context.Context, projectID string) (int, error) {
	tickets, err := s.store.ListTickets(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("scheduler: project %q tickets: %w", projectID, err)
	}
	n := 0
	for _, t := range tickets {
		switch t.State {
		case core.StateAssigned, core.StateRunning, core.StateValidating, core.StateReviewing:
			n++
		}
	}
	return n, nil
}

// sortTickets applies the scheduling order: priority DESC, position ASC, created_at ASC.
func sortTickets(ts []core.Ticket) {
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].Priority != ts[j].Priority {
			return ts[i].Priority > ts[j].Priority
		}
		if ts[i].Position != ts[j].Position {
			return ts[i].Position < ts[j].Position
		}
		return ts[i].CreatedAt.Before(ts[j].CreatedAt)
	})
}

// Trace renders a decision trace for display.
func Trace(why []string) string { return strings.Join(why, "\n  ") }

// routeAvailable applies the route's concurrency cap.
//
// Routes are buckets of agent capacity — a ticket names one, and the cap says how many of that
// kind may run at once. The limit is fleet-wide rather than per project, because what it is
// rationing is agents, not repositories.
//
// An uncapped route is limited only by the worker pool, which is the previous behaviour and
// stays the default: a cap nobody asked for would silently stall a queue.
func (s *Scheduler) routeAvailable(ctx context.Context, route core.Route, claimedThisTick int) (string, bool, error) {
	cap, capped := s.routeCaps[route]
	if !capped || cap <= 0 {
		return fmt.Sprintf("route %q is uncapped", route), true, nil
	}

	running, err := s.store.CountActiveTicketsByRoute(ctx, route)
	if err != nil {
		return "", false, fmt.Errorf("scheduler: route %q: %w", route, err)
	}
	inFlight := running + claimedThisTick
	if inFlight >= cap {
		return fmt.Sprintf("route %q is at its limit (%d of %d running)", route, inFlight, cap), false, nil
	}
	return fmt.Sprintf("route %q has room (%d of %d running)", route, inFlight, cap), true, nil
}
