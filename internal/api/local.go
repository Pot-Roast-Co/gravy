package api

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/runlog"
	"github.com/pot-roast-co/gravy/internal/scheduler"
	"github.com/pot-roast-co/gravy/internal/store"
)

// Local implements Service directly against the store.
//
// The daemon serves this over a unix socket and clients talk to it through a JSON-RPC client;
// the CLI in-process uses it directly. Both go through the same interface, so a behaviour that
// works in one works in the other by construction.
type Local struct {
	db     *store.DB
	hosts  []host.Host
	sched  *scheduler.Scheduler
	newID  func() string
	now    func() time.Time
	events *broker
	// lander is nil on a client that may not merge, which is why Approve checks it.
	lander Lander
	// logs is nil when this service cannot read run output.
	logs *runlog.Store
	// killer is nil on a client that may not stop work.
	killer Killer
	// liveness is nil on a client that is not running agents itself.
	liveness Liveness
	// planner is nil on a client that may not plan.
	planner Planner
	// update is the last release check's answer, written by whoever runs the check and read
	// by every Status. Guarded because the check runs on its own schedule, not on a request.
	updateMu sync.RWMutex
	update   UpdateStatus
	// discusser is nil on a client that cannot run a change discussion. Such a client can still
	// read one, edit its instruction and send it: the human gate does not depend on a model.
	discusser Discusser
	// checkouts is nil on a client that may not create review checkouts.
	checkouts Checkouts
	// agents is what this build can run, for validating a route before it is saved.
	agents []AgentOption
	// rereviewer is nil on a client that may not run reviews.
	rereviewer Rereviewer
	// detector probes the agent CLIs, for onboarding. Nil on a client that cannot.
	detector Detector

	// Configuration, empty on a client that cannot be configured.
	cfgMu          sync.RWMutex
	home           string
	cfg            config.Config
	applyCfg       ApplyFunc
	pendingRestart []string
}

// Killer stops a running agent and everything it spawned.
//
// An interface for the same reason as Lander: api says what it needs rather than depending on
// how running works, and a read-only client has no business being able to kill a run.
type Killer interface {
	Kill(ticketID string) error
}

// Liveness reports when a ticket's running agent last produced output. ok is false when no
// agent is running for the ticket — between attempts, say, or during validation.
type Liveness interface {
	LastEvent(ticketID string) (at time.Time, ok bool)
}

// WithLiveness lets Status say how long each running agent has been silent.
func (l *Local) WithLiveness(v Liveness) *Local {
	l.liveness = v
	return l
}

// WithKiller lets the service stop runs.
func (l *Local) WithKiller(k Killer) *Local {
	l.killer = k
	return l
}

// KillRun terminates a run's agent and everything it spawned.
//
// It takes a run id because that is what the detail screen is looking at, and resolves the
// ticket itself: the orchestrator tracks live work by ticket, since that is what holds a worker
// slot.
func (l *Local) KillRun(ctx context.Context, runID string) error {
	if l.killer == nil {
		return fmt.Errorf("this client cannot stop runs")
	}
	run, err := l.db.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if err := l.killer.Kill(run.TicketID); err != nil {
		return err
	}
	l.events.publish(Event{Kind: EventRunChanged, TicketID: run.TicketID, RunID: runID})
	return nil
}

// WithPlanner gives the service a planning engine. A service without one can do everything
// except plan, which is why Plan checks it rather than assuming.
func (l *Local) WithPlanner(p Planner) *Local {
	l.planner = p
	return l
}

// WithLander gives the service the merge gate. A service without one can read and queue work
// but cannot land it.
func (l *Local) WithLander(ld Lander) *Local {
	l.lander = ld
	return l
}

// SetUpdate records what a release check found, for Status to carry to clients.
func (l *Local) SetUpdate(u UpdateStatus) {
	l.updateMu.Lock()
	defer l.updateMu.Unlock()
	l.update = u
}

// NewLocal returns a Service backed by the store.
func NewLocal(db *store.DB, sched *scheduler.Scheduler, hosts []host.Host, newID func() string) *Local {
	return &Local{db: db, hosts: hosts, sched: sched, newID: newID, now: time.Now, events: newBroker()}
}

// ListProjects returns the registered projects, archived ones only when asked for.
func (l *Local) ListProjects(ctx context.Context, f ProjectFilter) ([]core.Project, error) {
	return l.db.ListProjects(ctx, store.ProjectFilter{IncludeArchived: f.IncludeArchived})
}

// ArchiveProject takes a project out of the working set, or puts it back.
//
// Nothing in flight is touched. A ticket already running, awaiting review or landing finishes
// its lifecycle: archiving says "queue no more work here", not "abandon what is happening".
func (l *Local) ArchiveProject(ctx context.Context, id string, archived bool) error {
	if err := l.db.SetProjectArchived(ctx, id, archived); err != nil {
		return err
	}
	l.events.publish(Event{Kind: EventProjectChanged, ProjectID: id})
	return nil
}

// AddProject registers a repository after checking it is one.
//
// The checks are deliberately upfront: a project that turns out not to be a git repository fails
// at the first run instead, which is a far worse place to discover it — the ticket is already
// claimed, a worker is held, and the error surfaces as a mysterious run failure.
func (l *Local) AddProject(ctx context.Context, req AddProjectReq) (core.Project, error) {
	if strings.TrimSpace(req.Path) == "" {
		return l.addProjectWithoutRepo(ctx, req)
	}
	p, err := l.prepareProject(ctx, req)
	if err != nil {
		return core.Project{}, err
	}
	if err := l.db.CreateProject(ctx, p); err != nil {
		return core.Project{}, err
	}
	l.events.publish(Event{Kind: EventProjectChanged, ProjectID: p.ID})
	return p, nil
}

func (l *Local) prepareProject(ctx context.Context, req AddProjectReq) (core.Project, error) {
	// A pinned project's repository is on that machine, so every check has to happen there.
	// Resolving the path locally would turn a Mac path into a Linux one, and stat would then
	// report a repository that exists as missing.
	h, err := l.hostFor(req.Host)
	if err != nil {
		return core.Project{}, err
	}

	// Setup prepares a real repository; AddProject handles planning-only projects separately.
	if strings.TrimSpace(req.Path) == "" {
		return core.Project{}, fmt.Errorf("setup needs a repository path")
	}

	path := req.Path
	if req.Host == "" {
		if path, err = filepath.Abs(path); err != nil {
			return core.Project{}, fmt.Errorf("resolve %q: %w", req.Path, err)
		}
	} else if !filepath.IsAbs(path) {
		// There is no working directory to resolve against on the far end.
		return core.Project{}, fmt.Errorf(
			"a path on %s must be absolute: %q", req.Host, req.Path)
	}

	if !h.FS().Exists(path) {
		return core.Project{}, fmt.Errorf("%s: no such directory on host %s", path, hostName(req.Host))
	}
	if err := checkGitRepo(ctx, h, path); err != nil {
		return core.Project{}, err
	}

	target := req.TargetBranch
	if target == "" {
		target, err = resolveTargetBranch(ctx, h, path)
		if err != nil {
			return core.Project{}, err
		}
	}

	mergeMode := req.MergeMode
	if mergeMode == "" {
		mergeMode = core.LandMerge
	}
	if !mergeMode.Valid() {
		return core.Project{}, fmt.Errorf("merge mode %q is not merge or pr", mergeMode)
	}

	name := req.Name
	if name == "" {
		name = filepath.Base(path)
	}
	slug := slugify(name)
	if slug == "" {
		return core.Project{}, fmt.Errorf("cannot derive a project name from %q", path)
	}
	if _, err := l.db.GetProjectBySlug(ctx, slug); err == nil {
		return core.Project{}, fmt.Errorf("a project named %q is already registered", slug)
	}

	maxConcurrency := req.MaxConcurrency
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}

	// Stamped, never left empty: the path was just validated on this host, so this is where
	// the clone is. An unstamped project is one the scheduler is free to run anywhere, which
	// means running an agent on a machine that does not have the code.
	hostID := req.Host
	if hostID == "" {
		hostID = h.ID()
	}

	p := core.Project{
		ID:     l.newID(),
		HostID: hostID,
		Notes:  req.Notes,
		// Detected rather than left empty. An empty allowlist refuses every command an agent
		// runs, silently, and the cost shows up as runs that take three times as many turns.
		Allowlist:      seedAllowlist(ctx, h, path),
		Slug:           slug,
		Name:           name,
		RepoPath:       path,
		TargetBranch:   target,
		MergeMode:      mergeMode,
		Validation:     req.Validation,
		ParallelMode:   req.ParallelMode,
		MaxConcurrency: maxConcurrency,
		CreatedAt:      l.now(),
	}
	if req.Allowlist != nil {
		p.Allowlist = *req.Allowlist
	}
	p.Requirements, p.Routes = req.Requirements, req.Routes

	return p, nil
}

// ListTickets returns tickets matching the filter.
func (l *Local) ListTickets(ctx context.Context, f TicketFilter) ([]core.Ticket, error) {
	switch {
	case f.ProjectID != "" && f.State != "":
		all, err := l.db.ListTickets(ctx, f.ProjectID)
		if err != nil {
			return nil, err
		}
		var out []core.Ticket
		for _, t := range all {
			if t.State == f.State {
				out = append(out, t)
			}
		}
		return out, nil
	case f.ProjectID != "":
		return l.db.ListTickets(ctx, f.ProjectID)
	case f.State != "":
		return l.db.ListTicketsByState(ctx, f.State)
	default:
		// Every project, archived included: a ticket does not stop existing because the
		// repository it belongs to left the working set.
		projects, err := l.db.ListProjects(ctx, store.ProjectFilter{IncludeArchived: true})
		if err != nil {
			return nil, err
		}
		var out []core.Ticket
		for _, p := range projects {
			ts, err := l.db.ListTickets(ctx, p.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, ts...)
		}
		return out, nil
	}
}

// CreateTicket adds a ticket, optionally straight into the Ready queue.
func (l *Local) CreateTicket(ctx context.Context, req CreateTicketReq) (core.Ticket, error) {
	if strings.TrimSpace(req.Title) == "" {
		return core.Ticket{}, fmt.Errorf("a ticket needs a title")
	}
	if _, err := l.db.GetProject(ctx, req.ProjectID); err != nil {
		return core.Ticket{}, err
	}

	route := req.Route
	if route == "" {
		route = core.RouteImplementation
	}
	if err := l.knownRoute(route); err != nil {
		return core.Ticket{}, err
	}

	now := l.now()
	t := core.Ticket{
		ID:        l.newID(),
		ProjectID: req.ProjectID,
		Title:     req.Title,
		Body:      req.Body,
		State:     core.StateBacklog,
		Priority:  req.Priority,
		Route:     route,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := l.db.CreateTicket(ctx, t); err != nil {
		return core.Ticket{}, err
	}

	for _, dep := range req.DependsOn {
		if _, err := l.db.GetTicket(ctx, dep); err != nil {
			return core.Ticket{}, fmt.Errorf("dependency %q: %w", dep, err)
		}
		if err := l.db.AddDep(ctx, t.ID, dep); err != nil {
			return core.Ticket{}, err
		}
	}

	if req.Ready {
		state, err := l.db.SetTicketState(ctx, t.ID, core.EventMarkReady)
		if err != nil {
			return core.Ticket{}, err
		}
		t.State = state
	}
	l.events.publish(Event{Kind: EventTicketChanged, ProjectID: t.ProjectID, TicketID: t.ID, State: t.State})
	return t, nil
}

// MoveTicket applies an event through the state machine.
func (l *Local) MoveTicket(ctx context.Context, id string, ev core.Event) (core.State, error) {
	// Queueing a ticket behind unlanded work is allowed, and the queue holds it.
	//
	// This used to be refused (GR-026 AC3) on the grounds that such a ticket would sit in Ready
	// looking eligible while the scheduler silently passed over it. It is not silent: the
	// scheduler holds a dependent until its dependency is Done and records that as the reason,
	// and every queue screen draws the "waiting on ..." line from ListQueue. What the refusal
	// actually cost was the ordinary case — a plan arrives as a chain of four tickets, and the
	// human had to come back and queue each one as its predecessor landed.

	state, err := l.db.SetTicketState(ctx, id, ev)
	if err != nil {
		return state, err
	}
	// An abandoned ticket is nobody's outstanding judgement call, so it must not keep sitting in
	// Needs You.
	if ev == core.EventReject {
		if _, err := l.db.ResolveAttentionForTicket(ctx, id); err != nil {
			return state, err
		}
		l.events.publish(Event{Kind: EventAttentionChanged, TicketID: id})
	}
	l.events.publish(Event{Kind: EventTicketChanged, TicketID: id, State: state})
	return state, nil
}

// ListRuns returns a ticket's runs, newest first.
func (l *Local) ListRuns(ctx context.Context, ticketID string) ([]core.Run, error) {
	return l.db.ListRunsForTicket(ctx, ticketID)
}

// ListProgress returns a ticket's progress journal, oldest first.
func (l *Local) ListProgress(ctx context.Context, ticketID string) ([]core.Progress, error) {
	return l.db.ListProgress(ctx, ticketID)
}

// ListAttention returns the open Needs You queue.
func (l *Local) ListAttention(ctx context.Context) ([]core.Attention, error) {
	return l.db.ListOpenAttention(ctx)
}

// ResolveAttention marks an attention item handled.
func (l *Local) ResolveAttention(ctx context.Context, id string) error {
	if err := l.db.ResolveAttention(ctx, id); err != nil {
		return err
	}
	l.events.publish(Event{Kind: EventAttentionChanged})
	return nil
}

// Events subscribes to the push stream.
func (l *Local) Events(ctx context.Context) (<-chan Event, func(), error) {
	ch, stop := l.events.subscribe(ctx)
	return ch, stop, nil
}

// Publish emits an event from outside the service, which is how the daemon reports run progress
// it makes directly against the store rather than through Service calls.
func (l *Local) Publish(e Event) { l.events.publish(e) }

// ExplainTicket answers why a ticket is or is not running.
func (l *Local) ExplainTicket(ctx context.Context, ticketID string) (Explanation, error) {
	if l.sched == nil {
		return Explanation{}, fmt.Errorf("no scheduler available to explain %q", ticketID)
	}
	ex, err := l.sched.Explain(ctx, ticketID)
	if err != nil {
		return Explanation{}, err
	}
	return Explanation{
		TicketID: ex.TicketID, State: ex.State, Eligible: ex.Eligible,
		Reason: ex.Reason, Why: ex.Why,
	}, nil
}

// Status is the one-screen answer to "what is happening, what needs me".
func (l *Local) Status(ctx context.Context, f ProjectFilter) (SystemStatus, error) {
	var st SystemStatus

	// Archived projects are read in either way, then left out of the Projects collection below.
	// Reading only the working set would drop their in-flight tickets from Running and their
	// rows from Needs You, making a ticket vanish mid-approval — exactly what archiving must
	// not do.
	projects, err := l.db.ListProjects(ctx, store.ProjectFilter{IncludeArchived: true})
	if err != nil {
		return st, err
	}

	byProject := make(map[string]core.Project, len(projects))
	byTicket := make(map[string]core.Ticket)
	now := l.now()
	l.cfgMu.RLock()
	quietAfter := l.cfg.Timeouts.Stall.D()
	l.cfgMu.RUnlock()

	for _, p := range projects {
		byProject[p.ID] = p
		ps := ProjectStatus{Project: p, Counts: map[core.State]int{}}

		tickets, err := l.db.ListTickets(ctx, p.ID)
		if err != nil {
			return st, err
		}
		for i, t := range tickets {
			byTicket[t.ID] = t
			ps.Counts[t.State]++
			if core.IsActive(t.State) && ps.Active == nil {
				ps.Active = &tickets[i]
			}
		}

		// An archived project is out of the working set: it leaves the Projects collection
		// every dashboard renders, and its queue leaves with it — those tickets are waiting on
		// a repository nobody is working on, and the scheduler will not take them.
		//
		// What archiving must not do is strand work that was already in flight. Archiving is
		// not a kill switch, so a ticket that was in Review when somebody archived its project
		// keeps its row in Needs You (assembled from the attention queue below, which spans
		// every project) and in Running, and stays approvable and landable until it reaches
		// Done or Rejected. Hiding the project hides a line on a summary, never the work.
		hidden := p.Archived && !f.IncludeArchived

		// An idle queue is always explained, never merely idle.
		switch {
		case p.Archived && ps.Counts[core.StateReady] > 0:
			// Not a fault: the repository is finished. Unarchiving starts the queue again.
			ps.Blocked = "project archived"
		case strings.TrimSpace(p.RepoPath) == "" && ps.Counts[core.StateReady] > 0:
			// The scheduler refuses these before it looks at anything else, and used to do it
			// silently: planning a project with no repository produces implementation tickets,
			// they sit in Ready looking like work about to start, and nothing says the one
			// thing standing in the way. Planning is what a project with no repository is for,
			// so this is the expected end of that road rather than a misconfiguration.
			ps.Blocked = "no repository yet — gravy project set-repo " + p.Slug + " <path>"
		case ps.Active != nil && !p.ParallelMode && ps.Counts[core.StateReady] > 0:
			verb := "in flight"
			if ps.Active.State == core.StateReview {
				verb = "awaiting your review"
			}
			ps.Blocked = fmt.Sprintf("serialized; %s %s (%s)", ps.Active.ID, verb, ps.Active.State)
		}

		for _, t := range tickets {
			switch {
			case inFlight(t.State):
				rt := RunningTicket{Ticket: t, Project: p, Activity: l.activity(ctx, t)}
				runs, err := l.db.ListRunsForTicket(ctx, t.ID)
				if err != nil {
					return st, err
				}
				if len(runs) > 0 {
					rt.Run = runs[0]
					rt.Elapsed = now.Sub(runs[0].StartedAt)
					if runs[0].EndedAt != nil {
						rt.Elapsed = runs[0].EndedAt.Sub(runs[0].StartedAt)
					}
				}
				l.noteQuiet(&rt, now, quietAfter)
				st.Running = append(st.Running, rt)
			case t.State == core.StateReady && !hidden:
				// Held carries the project's explanation, so a ticket that will not start
				// says why on its own row rather than being an unexplained absence.
				st.Ready = append(st.Ready, QueuedTicket{Ticket: t, Project: p, Held: ps.Blocked})
			}
		}

		if !hidden {
			st.Projects = append(st.Projects, ps)
		}
	}

	// Ready is fleet-wide and must be in the order the scheduler would take it, not grouped by
	// whichever project was read first.
	sort.SliceStable(st.Ready, func(i, j int) bool {
		a, b := st.Ready[i].Ticket, st.Ready[j].Ticket
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if a.Position != b.Position {
			return a.Position < b.Position
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})

	// Status is drawn constantly, so it reads reachability from memory and never probes. A
	// host that is switched off is reported as off immediately; asking the network instead is
	// what used to make one absent machine stall the whole screen.
	for _, h := range l.hosts {
		st.Hosts = append(st.Hosts, hostStatus(ctx, h))
	}

	open, err := l.db.ListOpenAttention(ctx)
	if err != nil {
		return st, err
	}
	answered := make(map[string]bool, len(open))
	for _, a := range open {
		item := AttentionItem{Attention: a, Project: byProject[a.ProjectID], Age: now.Sub(a.CreatedAt)}
		if t, ok := byTicket[a.TicketID]; ok {
			item.Ticket = t
		}
		answered[a.TicketID] = true
		st.Attention = append(st.Attention, item)
	}

	// A parked ticket with no open row is work nobody can reach.
	//
	// Needs You renders attention rows, the scheduler runs Ready tickets, and a ticket in
	// needs_you with its row resolved is in neither: invisible on the screen that exists to
	// show it, and skipped by the loop that would run it. Acknowledging used to do exactly
	// that, and the ticket it stranded was only found by reading the database.
	//
	// The row is synthesised rather than written, because the fault is that the ticket is
	// parked and unexplained, and inventing a stored reason would be a second lie on top of
	// the first.
	for _, t := range byTicket {
		if t.State != core.StateNeedsYou || answered[t.ID] {
			continue
		}
		st.Attention = append(st.Attention, AttentionItem{
			Attention: core.Attention{
				ID:        "unexplained-" + t.ID,
				ProjectID: t.ProjectID,
				TicketID:  t.ID,
				Reason:    core.ReasonUnexplained,
				CreatedAt: t.UpdatedAt,
			},
			Project: byProject[t.ProjectID],
			Ticket:  t,
			Age:     now.Sub(t.UpdatedAt),
		})
	}
	SortAttention(st.Attention)
	st.Buckets = l.Routes()
	l.updateMu.RLock()
	st.Update = l.update
	l.updateMu.RUnlock()
	return st, nil
}

// inFlight reports whether Gravy is actively working a ticket.
//
// Review, Blocked and NeedsYou are active states but belong in the Needs You queue rather than
// in Running: the distinction the dashboard draws is "waiting on the fleet" against "waiting on
// you", which is not the same line core.IsActive draws.
func inFlight(s core.State) bool {
	switch s {
	case core.StateAssigned, core.StateRunning, core.StateValidating,
		core.StateReviewing, core.StateLanding:
		return true
	default:
		return false
	}
}

// activity is what a ticket is doing now: the newest line of its progress journal.
//
// The journal is the better answer because a state name cannot distinguish fetching from
// building a prompt from waiting on the third validation step — they are all "running". The
// state mapping stays as the fallback for the window a ticket is in before anything has been
// narrated, and for a journal a failed write left behind. A journal that cannot be read is not
// worth failing a dashboard over, so a read error falls back too.
func (l *Local) activity(ctx context.Context, t core.Ticket) string {
	latest, err := l.db.LatestProgress(ctx, t.ID)
	if err != nil || strings.TrimSpace(latest.Detail) == "" {
		return activityFor(t.State)
	}
	return latest.Detail
}

// noteQuiet fills in how long a running agent has been silent, and says so in its activity once
// that passes the configured threshold.
//
// Reporting is all it does. A run that is thinking hard and one that is wedged look the same
// from here, so the call on killing it belongs to the human reading the row.
func (l *Local) noteQuiet(rt *RunningTicket, now time.Time, threshold time.Duration) {
	if l.liveness == nil {
		return
	}
	at, ok := l.liveness.LastEvent(rt.Ticket.ID)
	if !ok {
		return
	}
	rt.LastOutput = max(now.Sub(at), 0)
	if threshold > 0 && rt.LastOutput >= threshold {
		rt.Activity += " — no output for " + roughly(rt.LastOutput)
	}
}

// roughly renders a silence the way a person would say it: "4m", not "4m12.3s".
func roughly(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return strings.TrimSuffix(d.Truncate(time.Minute).String(), "0s")
}

// activityFor phrases a state as what the agent is doing, for a row a human scans.
func activityFor(s core.State) string {
	switch s {
	case core.StateAssigned:
		return "starting"
	case core.StateRunning:
		return "implementing"
	case core.StateValidating:
		return "validating"
	case core.StateReviewing:
		return "reviewing"
	case core.StateLanding:
		return "landing"
	default:
		return string(s)
	}
}

func (l *Local) aHost() host.Host {
	if len(l.hosts) == 0 {
		return nil
	}
	return l.hosts[0]
}

// addProjectWithoutRepo registers a project that has no working tree yet.
func (l *Local) addProjectWithoutRepo(ctx context.Context, req AddProjectReq) (core.Project, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return core.Project{}, fmt.Errorf("a project with no repository needs a name")
	}
	slug := slugify(name)
	if slug == "" {
		return core.Project{}, fmt.Errorf("cannot derive a project name from %q", req.Name)
	}
	if _, err := l.db.GetProjectBySlug(ctx, slug); err == nil {
		return core.Project{}, fmt.Errorf("a project named %q is already registered", slug)
	}

	hostID := req.Host
	if hostID == "" {
		if h := l.aHost(); h != nil {
			hostID = h.ID()
		}
	}

	p := core.Project{
		ID: l.newID(), Slug: slug, Name: name, HostID: hostID,
		Notes: req.Notes, MergeMode: core.LandMerge, MaxConcurrency: 1,
		CreatedAt: l.now(),
	}
	if err := l.db.CreateProject(ctx, p); err != nil {
		return core.Project{}, err
	}
	l.events.publish(Event{Kind: EventProjectChanged, ProjectID: p.ID})
	return p, nil
}

// DeleteProject removes a project and everything belonging to it.
//
// Through the store, so the foreign-key cascade takes its tickets, runs and attention rows with
// it. Deleting the row by hand leaves orphans, and an orphaned ticket is not inert: the
// scheduler reads it every tick, fails to find its project, and stops scheduling for every
// project until somebody notices.
func (l *Local) DeleteProject(ctx context.Context, id string) error {
	p, err := l.db.GetProject(ctx, id)
	if err != nil {
		return err
	}
	if err := l.db.DeleteProject(ctx, id); err != nil {
		return fmt.Errorf("delete project %s: %w", p.Slug, err)
	}
	l.events.publish(Event{Kind: EventProjectChanged, ProjectID: id})
	l.events.publish(Event{Kind: EventTicketChanged, ProjectID: id})
	l.events.publish(Event{Kind: EventAttentionChanged})
	return nil
}

// hostFor returns the host a project lives on, or an error naming what is registered.
func (l *Local) hostFor(id string) (host.Host, error) {
	if id == "" {
		if h := l.aHost(); h != nil {
			return h, nil
		}
		return nil, fmt.Errorf("no host available")
	}
	names := make([]string, 0, len(l.hosts))
	for _, h := range l.hosts {
		if h.ID() == id {
			return h, nil
		}
		names = append(names, h.ID())
	}
	return nil, fmt.Errorf("no host called %q — configured: %s", id, strings.Join(names, ", "))
}

// hostName renders a host id for a message, naming the local machine when there is no id.
func hostName(id string) string {
	if id == "" {
		return "local"
	}
	return id
}

// checkGitRepo verifies the path is a git working tree.
func checkGitRepo(ctx context.Context, h host.Host, path string) error {
	out, code, err := runGit(ctx, h, path, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if code != 0 || strings.TrimSpace(out) != "true" {
		return fmt.Errorf("%s is not a git repository", path)
	}
	return nil
}

// resolveTargetBranch reads the default branch from the remote.
//
// Landing merges into this branch, so a wrong answer here is not a mild inconvenience: approved
// work is carried onto whatever branch happened to be checked out the day the project was
// registered, the push succeeds, the ticket goes green, and nobody notices until they go looking
// for the merge on main and find it two days stale. That is a real case, not a hypothetical one —
// it is why the fallback below is as narrow as it is.
//
// The order:
//
//  1. refs/remotes/origin/HEAD. A clone sets it, and reading it costs nothing.
//  2. The remote itself. A repository created with `git init` and pushed never gets a local
//     origin/HEAD, which is precisely the case step 1 misses and precisely where the old
//     current-branch fallback did its damage.
//  3. The current branch, but only when there is no origin at all. A local-only project is a
//     legitimate thing to run agents against, and refusing it would block the very scratch
//     repositories used to try Gravy out.
//
// A repository that has an origin whose default branch cannot be read is an error, not a guess.
// Being asked for --target-branch is a smaller cost than merging into the wrong branch quietly.
func resolveTargetBranch(ctx context.Context, h host.Host, path string) (string, error) {
	out, code, err := runGit(ctx, h, path, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", err
	}
	if code == 0 {
		if ref := strings.TrimSpace(out); ref != "" {
			return strings.TrimPrefix(ref, "origin/"), nil
		}
	}

	// GIT_TERMINAL_PROMPT=0 and runGit's timeout are what make asking the remote safe here: an
	// unreachable or credential-gated origin fails rather than blocking project registration on
	// a password prompt nobody can see.
	_, code, err = runGit(ctx, h, path, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	if code == 0 {
		out, code, err = runGit(ctx, h, path, "ls-remote", "--symref", "origin", "HEAD")
		if err != nil {
			return "", err
		}
		if code == 0 {
			if branch := symrefHead(out); branch != "" {
				return branch, nil
			}
		}
		return "", fmt.Errorf(
			"%s: origin's default branch could not be read; pass --target-branch", path)
	}

	out, code, err = runGit(ctx, h, path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if code != 0 || strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("%s: cannot determine a target branch; pass --target-branch", path)
	}
	return strings.TrimSpace(out), nil
}

// symrefHead pulls the branch out of `git ls-remote --symref origin HEAD`, whose first line is
// "ref: refs/heads/main\tHEAD". Returns "" when the remote reports no symref, which an empty
// repository does.
func symrefHead(out string) string {
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ref:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		if branch := strings.TrimPrefix(fields[0], "refs/heads/"); branch != fields[0] {
			return branch
		}
	}
	return ""
}

func runGit(ctx context.Context, h host.Host, dir string, args ...string) (string, int, error) {
	p, err := h.Exec(ctx, host.ExecSpec{
		Cmd: "git", Args: args, Dir: dir, Timeout: 30 * time.Second,
		Env: map[string]string{"GIT_TERMINAL_PROMPT": "0"},
	})
	if err != nil {
		return "", 0, err
	}
	outCh := slurp(p.Stdout())
	errCh := slurp(p.Stderr())
	st, waitErr := p.Wait()
	out := <-outCh
	<-errCh
	if waitErr != nil {
		return "", 0, waitErr
	}
	return out, st.Code, nil
}

func slurp(r io.Reader) <-chan string {
	ch := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		ch <- string(b)
	}()
	return ch
}

// slugify reduces a name to a filesystem- and URL-safe identifier.
func slugify(s string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// SortAttention orders the queue oldest first, which is the order a human works through it.
func SortAttention(items []AttentionItem) {
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Attention.CreatedAt.Before(items[j].Attention.CreatedAt)
	})
}

var _ Service = (*Local)(nil)

// hostStatus reads one host's load and last-known reachability without touching the network.
func hostStatus(ctx context.Context, h host.Host) HostStatus {
	used, total := h.Slots()
	r := h.Reachability()
	hs := HostStatus{
		ID:          h.ID(),
		UsedSlots:   used,
		TotalSlots:  total,
		Online:      r.Online,
		Unreachable: r.Err,
		CheckedAt:   r.CheckedAt,
		Checking:    r.Checking,
	}
	// Capabilities are served from the same cache the reachability came from, so this is a
	// map read rather than a connection. An off host keeps the tools it had when it was last
	// reached, which is what makes "it is off, and it is the one with Xcode" sayable.
	if caps, err := h.Capabilities(ctx); err == nil {
		hs.OS, hs.Arch, hs.Tools = caps.OS, caps.Arch, caps.Tools
	}
	return hs
}

// ReconnectHost probes one host now, so a machine that has just been switched on can rejoin
// without restarting the daemon.
func (l *Local) ReconnectHost(ctx context.Context, id string) (HostStatus, error) {
	for _, h := range l.hosts {
		if h.ID() != id {
			continue
		}
		// The probe result is deliberately discarded: a failure here is not an error to
		// report up, it is the answer — the machine is still off, and it is recorded as such
		// for every later caller to read without waiting.
		_, _ = h.Recheck(ctx)
		return hostStatus(ctx, h), nil
	}
	return HostStatus{}, fmt.Errorf("no host %q is configured", id)
}
