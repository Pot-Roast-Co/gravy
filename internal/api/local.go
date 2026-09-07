package api

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bobbybrady/gravy/internal/core"
	"github.com/bobbybrady/gravy/internal/host"
	"github.com/bobbybrady/gravy/internal/scheduler"
	"github.com/bobbybrady/gravy/internal/store"
)

// Local implements Service directly against the store.
//
// The daemon serves this over a unix socket and clients talk to it through a JSON-RPC client;
// the CLI in-process uses it directly. Both go through the same interface, so a behaviour that
// works in one works in the other by construction.
type Local struct {
	db    *store.DB
	hosts []host.Host
	sched *scheduler.Scheduler
	newID func() string
	now   func() time.Time
}

// NewLocal returns a Service backed by the store.
func NewLocal(db *store.DB, sched *scheduler.Scheduler, hosts []host.Host, newID func() string) *Local {
	return &Local{db: db, hosts: hosts, sched: sched, newID: newID, now: time.Now}
}

// ListProjects returns every registered project.
func (l *Local) ListProjects(ctx context.Context) ([]core.Project, error) {
	return l.db.ListProjects(ctx)
}

// AddProject registers a repository after checking it is one.
//
// The checks are deliberately upfront: a project that turns out not to be a git repository fails
// at the first run instead, which is a far worse place to discover it — the ticket is already
// claimed, a worker is held, and the error surfaces as a mysterious run failure.
func (l *Local) AddProject(ctx context.Context, req AddProjectReq) (core.Project, error) {
	path, err := filepath.Abs(req.Path)
	if err != nil {
		return core.Project{}, fmt.Errorf("resolve %q: %w", req.Path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return core.Project{}, fmt.Errorf("%s: %w", path, err)
	}
	if !info.IsDir() {
		return core.Project{}, fmt.Errorf("%s is not a directory", path)
	}

	h := l.aHost()
	if h == nil {
		return core.Project{}, fmt.Errorf("no host available to inspect %s", path)
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

	p := core.Project{
		ID:             l.newID(),
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
	if err := l.db.CreateProject(ctx, p); err != nil {
		return core.Project{}, err
	}
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
		projects, err := l.db.ListProjects(ctx)
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
	if !route.Valid() {
		return core.Ticket{}, fmt.Errorf("route %q is not one of %v", route, core.AllRoutes)
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
	return t, nil
}

// MoveTicket applies an event through the state machine.
func (l *Local) MoveTicket(ctx context.Context, id string, ev core.Event) (core.State, error) {
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
	}
	return state, nil
}

// ListRuns returns a ticket's runs, newest first.
func (l *Local) ListRuns(ctx context.Context, ticketID string) ([]core.Run, error) {
	return l.db.ListRunsForTicket(ctx, ticketID)
}

// ListAttention returns the open Needs You queue.
func (l *Local) ListAttention(ctx context.Context) ([]core.Attention, error) {
	return l.db.ListOpenAttention(ctx)
}

// ResolveAttention marks an attention item handled.
func (l *Local) ResolveAttention(ctx context.Context, id string) error {
	return l.db.ResolveAttention(ctx, id)
}

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
func (l *Local) Status(ctx context.Context) (SystemStatus, error) {
	var st SystemStatus

	projects, err := l.db.ListProjects(ctx)
	if err != nil {
		return st, err
	}
	for _, p := range projects {
		ps := ProjectStatus{Project: p, Counts: map[core.State]int{}}

		tickets, err := l.db.ListTickets(ctx, p.ID)
		if err != nil {
			return st, err
		}
		for i, t := range tickets {
			ps.Counts[t.State]++
			if core.IsActive(t.State) && ps.Active == nil {
				ps.Active = &tickets[i]
			}
		}

		// An idle queue is always explained, never merely idle.
		if ps.Active != nil && !p.ParallelMode && ps.Counts[core.StateReady] > 0 {
			verb := "in flight"
			if ps.Active.State == core.StateReview {
				verb = "awaiting your review"
			}
			ps.Blocked = fmt.Sprintf("serialized; %s %s (%s)", ps.Active.ID, verb, ps.Active.State)
		}
		st.Projects = append(st.Projects, ps)
	}

	for _, h := range l.hosts {
		used, total := h.Slots()
		hs := HostStatus{ID: h.ID(), UsedSlots: used, TotalSlots: total}
		if caps, err := h.Capabilities(ctx); err == nil {
			hs.OS, hs.Arch, hs.Tools = caps.OS, caps.Arch, caps.Tools
		}
		st.Hosts = append(st.Hosts, hs)
	}

	st.Attention, err = l.db.ListOpenAttention(ctx)
	if err != nil {
		return st, err
	}
	return st, nil
}

func (l *Local) aHost() host.Host {
	if len(l.hosts) == 0 {
		return nil
	}
	return l.hosts[0]
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

// resolveTargetBranch reads the default branch from the remote's HEAD.
//
// A repository with no remote is accepted and falls back to the current branch: a local-only
// project is a legitimate thing to run agents against, and refusing it would block the very
// scratch repositories used to try Gravy out.
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

	out, code, err = runGit(ctx, h, path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if code != 0 || strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("%s: cannot determine a target branch; pass --target-branch", path)
	}
	return strings.TrimSpace(out), nil
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
func SortAttention(items []core.Attention) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
}

var _ Service = (*Local)(nil)
