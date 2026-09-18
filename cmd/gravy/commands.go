package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pot-roast-co/gravy/internal/agentrun"
	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/daemon"
	"github.com/pot-roast-co/gravy/internal/scheduler"
)

// ---- project -------------------------------------------------------------

// projectSubcommand splits `gravy project ...` arguments into a subcommand and the arguments
// belonging to it.
//
// Listing is the default, so a bare `gravy project` and a leading flag such as `gravy project
// --all` both mean "list" — and neither has a subcommand word to strip off the front. Slicing
// one off regardless is how the default branch panics on an empty argument list instead of
// listing the projects.
func projectSubcommand(args []string) (sub string, rest []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func runProject(ctx context.Context, args []string) error {
	sub, rest := projectSubcommand(args)
	switch sub {
	case "add":
		return projectAdd(ctx, rest)
	case "set-repo":
		return projectSetRepo(ctx, rest)
	case "set-target":
		return projectSetTarget(ctx, rest)
	case "archive":
		return projectArchive(ctx, rest, true)
	case "unarchive":
		return projectArchive(ctx, rest, false)
	case "list", "ls", "":
		return projectList(ctx, rest)
	default:
		return fmt.Errorf(
			"unknown project command %q (try: add, list, set-repo, set-target, archive, unarchive)", sub)
	}
}

// parseFlags parses args, accepting flags before or after the positional arguments, and returns
// the positionals in order.
//
// Go's flag package stops at the first non-flag argument, so `gravy project add <path> -validate
// test:...` silently drops the flag and then fails on an argument count it never explains. That
// is the order people type, and a registration that quietly loses its validation steps is worse
// than one that is fussy about ordering, so accept both.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// stepList collects repeatable --validate flags.
type stepList []core.Step

func (s *stepList) String() string { return fmt.Sprintf("%d steps", len(*s)) }

// Set parses "name:command" or a bare command.
func (s *stepList) Set(v string) error {
	name, cmd, ok := strings.Cut(v, ":")
	if !ok {
		name, cmd = "check", v
	}
	if strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("validation step %q has no command", v)
	}
	*s = append(*s, core.Step{
		Name: strings.TrimSpace(name), Cmd: strings.TrimSpace(cmd),
		Required: true, Timeout: 10 * time.Minute,
	})
	return nil
}

func projectAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gravy project add", flag.ContinueOnError)
	target := fs.String("target-branch", "", "branch to merge into (default: the remote's HEAD)")
	mergeMode := fs.String("merge-mode", "merge", "how work lands: merge | pr")
	hostID := fs.String("host", "", "machine this project lives on, from config hosts (default: local)")
	name := fs.String("name", "", "project name (default: the directory name)")
	parallel := fs.Bool("parallel", false, "allow several tickets in flight at once (you handle conflicts)")
	maxConc := fs.Int("max-concurrency", 1, "tickets in flight when --parallel is set")
	var steps stepList
	fs.Var(&steps, "validate", "validation step as name:command, repeatable (e.g. -validate test:'go test ./...')")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gravy project add [flags] <path>")
		fs.PrintDefaults()
	}
	paths, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(paths) != 1 {
		fs.Usage()
		return fmt.Errorf("expected exactly one repository path")
	}

	a, err := newClient(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	p, err := a.svc.AddProject(ctx, api.AddProjectReq{
		Path: paths[0], Name: *name, TargetBranch: *target, Host: *hostID,
		MergeMode: core.LandMode(*mergeMode), Validation: steps,
		ParallelMode: *parallel, MaxConcurrency: *maxConc,
	})
	if err != nil {
		return err
	}

	fmt.Printf("registered %s\n", p.Slug)
	fmt.Printf("  path          %s\n", p.RepoPath)
	if p.HostID != "" {
		fmt.Printf("  host          %s\n", p.HostID)
	}
	fmt.Printf("  target branch %s\n", p.TargetBranch)
	fmt.Printf("  merge mode    %s\n", p.MergeMode)
	fmt.Printf("  mode          %s\n", modeLabel(p))
	if len(p.Validation) == 0 {
		fmt.Println("  validation    none — add steps with -validate so work is checked before review")
	} else {
		for _, s := range p.Validation {
			fmt.Printf("  validation    %s: %s\n", s.Name, s.Cmd)
		}
	}
	return nil
}

func modeLabel(p core.Project) string {
	if p.ParallelMode {
		return fmt.Sprintf("parallel, up to %d at once", p.MaxConcurrency)
	}
	return "serial — one ticket in flight, through merge"
}

func projectList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gravy project list", flag.ContinueOnError)
	all := fs.Bool("all", false, "include archived projects")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := newClient(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	projects, err := a.svc.ListProjects(ctx, api.ProjectFilter{IncludeArchived: *all})
	if err != nil {
		return err
	}
	if len(projects) == 0 {
		fmt.Println("no projects registered yet — add one with: gravy project add <path>")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SLUG\tTARGET\tMODE\tVALIDATION\tPATH")
	for _, p := range projects {
		names := make([]string, 0, len(p.Validation))
		for _, s := range p.Validation {
			names = append(names, s.Name)
		}
		v := strings.Join(names, ",")
		if v == "" {
			v = "-"
		}
		mode := "serial"
		if p.ParallelMode {
			mode = fmt.Sprintf("parallel(%d)", p.MaxConcurrency)
		}
		slug := p.Slug
		if p.Archived {
			slug += " (archived)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", slug, p.TargetBranch, mode, v, p.RepoPath)
	}
	return w.Flush()
}

// projectArchive takes a finished repository out of the working set, or puts it back.
//
// Nothing is deleted and nothing in flight is stopped: the project's tickets, runs and summaries
// stay exactly where they are, and a ticket already running or awaiting review finishes.
// projectSetRepo gives a project without a repository one to work in.
//
// The step between planning a goal and building it. A project registered with no path can be
// planned — that is what it is for — but the implementation tickets planning produces cannot
// start until there is somewhere to work, and the only route to that used to be deleting the
// project and registering it again, which took the plan with it.
func projectSetRepo(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gravy project set-repo", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gravy project set-repo <slug|id> <path>")
		fmt.Fprintln(os.Stderr, "\nGives a project with no repository one to work in.")
		fs.PrintDefaults()
	}
	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		fs.Usage()
		return fmt.Errorf("expected a project and a path")
	}

	a, err := newClient(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	p, err := findProject(ctx, a.svc, rest[0])
	if err != nil {
		return err
	}
	saved, err := a.svc.AttachRepository(ctx, p.ID, rest[1])
	if err != nil {
		return err
	}
	fmt.Printf("%s now works in %s\n", saved.Slug, saved.RepoPath)
	fmt.Printf("  target branch %s\n", saved.TargetBranch)
	if n := len(saved.Allowlist.Commands); n > 0 {
		fmt.Printf("  detected %d allowed command(s); review them with `gravy` -> Projects\n", n)
	}
	return nil
}

// projectSetTarget changes the branch a project's approved work merges into.
//
// It exists because the target branch was otherwise editable only on the Settings screen, and a
// target resolved wrong at registration is invisible until work lands somewhere nobody meant.
// Finding out from a merge on the wrong branch and then being told the fix is a TUI screen is a
// bad trade for one field.
func projectSetTarget(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gravy project set-target", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gravy project set-target <slug|id> <branch>")
		fmt.Fprintln(os.Stderr, "\nChanges the branch approved work merges into.")
		fs.PrintDefaults()
	}
	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 2 {
		fs.Usage()
		return fmt.Errorf("expected a project and a branch")
	}
	branch := strings.TrimSpace(rest[1])
	if branch == "" {
		return fmt.Errorf("a project needs a target branch")
	}

	a, err := newClient(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	slug, was, changed, err := setTargetBranch(ctx, a.svc, rest[0], branch)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Printf("%s already merges into %s\n", slug, branch)
		return nil
	}
	// Both branches named. The whole point is that the old value was wrong without anyone
	// seeing it, so it gets said out loud on the way out.
	fmt.Printf("%s now merges into %s (was %s)\n", slug, branch, was)
	return nil
}

// setTargetBranch points a project at a branch and reports what it was before.
//
// Split from the command so the rule can be tested without a daemon: the interesting part is
// that it edits one field of the project it read, rather than writing a fresh one and dropping
// everything the caller did not mention.
func setTargetBranch(ctx context.Context, svc api.Service, ref, branch string) (slug, was string, changed bool, err error) {
	p, err := findProject(ctx, svc, ref)
	if err != nil {
		return "", "", false, err
	}
	if p.TargetBranch == branch {
		return p.Slug, branch, false, nil
	}
	was = p.TargetBranch
	p.TargetBranch = branch
	if err := svc.UpdateProject(ctx, p); err != nil {
		return "", "", false, err
	}
	return p.Slug, was, true, nil
}

func projectArchive(ctx context.Context, args []string, archived bool) error {
	verb := "archive"
	if !archived {
		verb = "unarchive"
	}
	fs := flag.NewFlagSet("gravy project "+verb, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: gravy project %s <slug|id>\n", verb)
		fs.PrintDefaults()
	}
	names, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(names) != 1 {
		fs.Usage()
		return fmt.Errorf("expected exactly one project")
	}

	a, err := newClient(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	p, err := findProject(ctx, a.svc, names[0])
	if err != nil {
		return err
	}
	if p.Archived == archived {
		fmt.Printf("%s is already %sd\n", p.Slug, verb)
		return nil
	}
	if err := a.svc.ArchiveProject(ctx, p.ID, archived); err != nil {
		return err
	}

	if archived {
		fmt.Printf("archived %s — its tickets, runs and history are kept\n", p.Slug)
		fmt.Printf("  ready tickets stay ready; nothing new starts until: gravy project unarchive %s\n", p.Slug)
		return nil
	}
	fmt.Printf("unarchived %s — the scheduler will pick its ready tickets up again\n", p.Slug)
	return nil
}

// findProject looks a project up by slug or id, archived ones included.
//
// Unlike resolveProject there is no "the only one registered" default: these commands name the
// project they act on, and an archived one has to be nameable or it could never come back.
func findProject(ctx context.Context, svc api.Service, ref string) (core.Project, error) {
	projects, err := svc.ListProjects(ctx, api.ProjectFilter{IncludeArchived: true})
	if err != nil {
		return core.Project{}, err
	}
	for _, p := range projects {
		if p.Slug == ref || p.ID == ref {
			return p, nil
		}
	}
	return core.Project{}, fmt.Errorf("no project named %q", ref)
}

// ---- ticket --------------------------------------------------------------

func runTicket(ctx context.Context, args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "add":
		return ticketAdd(ctx, args[1:])
	case "list", "ls", "":
		return ticketList(ctx, args[1:])
	default:
		return fmt.Errorf("unknown ticket command %q (try: add, list)", sub)
	}
}

func ticketAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gravy ticket add", flag.ContinueOnError)
	projectSlug := fs.String("project", "", "project slug (required when more than one is registered)")
	body := fs.String("body", "", "the ticket's detail; what done looks like")
	route := fs.String("route", "implementation", "semantic route: cheap | standard | strong | implementation | review")
	priority := fs.Int("priority", 0, "higher runs first")
	ready := fs.Bool("ready", true, "put it straight into the queue rather than the backlog")
	dependsOn := fs.String("depends-on", "", "comma-separated ticket ids that must reach Done first")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gravy ticket add [flags] \"<title>\"")
		fs.PrintDefaults()
	}
	words, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	title := strings.TrimSpace(strings.Join(words, " "))
	if title == "" {
		fs.Usage()
		return fmt.Errorf("a ticket needs a title")
	}

	a, err := newClient(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	project, err := resolveProject(ctx, a.svc, *projectSlug)
	if err != nil {
		return err
	}

	var deps []string
	if *dependsOn != "" {
		deps = strings.Split(*dependsOn, ",")
		for i := range deps {
			deps[i] = strings.TrimSpace(deps[i])
		}
	}

	t, err := a.svc.CreateTicket(ctx, api.CreateTicketReq{
		ProjectID: project.ID, Title: title, Body: *body,
		Route: core.Route(*route), Priority: *priority, Ready: *ready, DependsOn: deps,
	})
	if err != nil {
		return err
	}

	fmt.Printf("created %s in %s\n", t.ID, project.Slug)
	fmt.Printf("  %s\n", t.Title)
	fmt.Printf("  state %s, route %s\n", t.State, t.Route)
	if t.State == core.StateReady {
		fmt.Println("\nit is queued. run `gravy run` to work the queue.")
	} else {
		fmt.Printf("\nit is in the backlog. queue it with: gravy ticket ready %s\n", t.ID)
	}
	return nil
}

// resolveProject picks the named project, or the only one when there is exactly one.
// resolveProject takes the service rather than an app so both the client commands and the
// in-process ones can use it.
func resolveProject(ctx context.Context, svc api.Service, slug string) (core.Project, error) {
	// Named explicitly, an archived project still resolves — filing a ticket against finished
	// work is a deliberate act, and it simply will not start until the project comes back. What
	// an archived project never becomes is the implicit default below.
	projects, err := svc.ListProjects(ctx, api.ProjectFilter{IncludeArchived: true})
	if err != nil {
		return core.Project{}, err
	}
	if slug != "" {
		for _, p := range projects {
			if p.Slug == slug {
				return p, nil
			}
		}
		return core.Project{}, fmt.Errorf("no project named %q", slug)
	}

	working := make([]core.Project, 0, len(projects))
	for _, p := range projects {
		if !p.Archived {
			working = append(working, p)
		}
	}
	switch len(working) {
	case 0:
		if len(projects) > 0 {
			return core.Project{}, fmt.Errorf(
				"every project is archived; name one with -project, or: gravy project unarchive <slug>")
		}
		return core.Project{}, fmt.Errorf("no projects registered — add one with: gravy project add <path>")
	case 1:
		return working[0], nil
	}
	names := make([]string, len(working))
	for i, p := range working {
		names[i] = p.Slug
	}
	return core.Project{}, fmt.Errorf("several projects registered (%s); pass -project",
		strings.Join(names, ", "))
}

func ticketList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gravy ticket list", flag.ContinueOnError)
	state := fs.String("state", "", "only tickets in this state")
	projectSlug := fs.String("project", "", "only this project")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := newClient(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	filter := api.TicketFilter{State: core.State(*state)}
	if *projectSlug != "" {
		p, err := resolveProject(ctx, a.svc, *projectSlug)
		if err != nil {
			return err
		}
		filter.ProjectID = p.ID
	}

	tickets, err := a.svc.ListTickets(ctx, filter)
	if err != nil {
		return err
	}
	if len(tickets) == 0 {
		fmt.Println("no tickets")
		return nil
	}

	// Archived ones too: a listed ticket names its project, and one in a finished repository
	// would otherwise print a blank column.
	projects, _ := a.svc.ListProjects(ctx, api.ProjectFilter{IncludeArchived: true})
	slugByID := map[string]string{}
	for _, p := range projects {
		slugByID[p.ID] = p.Slug
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPROJECT\tSTATE\tTITLE")
	for _, t := range tickets {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.ID, slugByID[t.ProjectID], t.State, truncateTo(t.Title, 60))
	}
	return w.Flush()
}

// ---- run -----------------------------------------------------------------

func runQueue(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gravy run", flag.ContinueOnError)
	once := fs.Bool("once", false, "work the queue until it is idle, then exit")
	interval := fs.Duration("interval", 2*time.Second, "how often to re-examine the queue")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gravy run [flags]")
		fmt.Fprintln(os.Stderr, "\nWorks the queue: claims ready tickets, runs agents, validates, and")
		fmt.Fprintln(os.Stderr, "stops at review. Nothing merges.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	// Two schedulers over one database would both claim the same ready ticket and start two
	// agents in one worktree. The daemon owns the queue whenever it is running.
	if pid := daemon.Running(a.home); pid != 0 {
		return fmt.Errorf("the gravy daemon is already working the queue (pid %d) — "+
			"watch it with `gravy`, or stop it first", pid)
	}

	loop := a.loop()
	loop.Interval = *interval

	started := time.Now()
	loop.OnAssign = func(as scheduler.Assignment) {
		fmt.Printf("%s  start   %s on %s via %s/%s\n",
			stamp(started), as.TicketID, as.HostID, as.ProviderID, as.Model)
		for _, w := range as.Why {
			fmt.Printf("%s          %s\n", strings.Repeat(" ", 7), w)
		}
	}
	loop.OnFinish = func(res agentrun.Result, err error) {
		if err != nil {
			fmt.Printf("%s  ERROR   %s: %v\n", stamp(started), res.TicketID, err)
			return
		}
		summary := fmt.Sprintf("%s  done    %s -> %s after %d attempt(s)",
			stamp(started), res.TicketID, res.FinalState, res.Attempts)
		if res.Commit != "" {
			summary += fmt.Sprintf(", commit %s", shortHash(res.Commit))
		}
		fmt.Println(summary)
		for _, v := range res.Validation {
			fmt.Printf("%s          validation %s: %s\n", strings.Repeat(" ", 7), v.Step, v.Outcome)
		}
	}

	// Explain anything that is ready but cannot start, so an idle queue is never a mystery.
	if err := explainIdle(ctx, a); err != nil {
		return err
	}

	if *once {
		return loop.RunUntilIdle(ctx)
	}
	fmt.Println("working the queue; ctrl-c to stop")
	return loop.Run(ctx)
}

func explainIdle(ctx context.Context, a *app) error {
	explanations, err := a.sched.ExplainAll(ctx)
	if err != nil {
		return err
	}
	for _, ex := range explanations {
		if !ex.Eligible {
			fmt.Printf("         held    %s: %s\n", ex.TicketID, ex.Reason)
		}
	}
	return nil
}

func stamp(started time.Time) string {
	return fmt.Sprintf("%6.1fs", time.Since(started).Seconds())
}

// ---- status --------------------------------------------------------------

func runStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gravy status", flag.ContinueOnError)
	all := fs.Bool("all", false, "include archived projects")
	if err := fs.Parse(args); err != nil {
		return err
	}

	a, err := newClient(ctx)
	if err != nil {
		return err
	}
	defer a.Close()

	st, err := a.svc.Status(ctx, api.ProjectFilter{IncludeArchived: *all})
	if err != nil {
		return err
	}

	// Needs You first, then what is happening. That order is the product.
	fmt.Printf("NEEDS YOU (%d)\n", len(st.Attention))
	if len(st.Attention) == 0 {
		fmt.Println("  nothing — Gravy does not need you")
	} else {
		for _, at := range st.Attention {
			fmt.Printf("  %-16s %s  %s\n",
				at.Attention.Reason, at.Attention.TicketID, payloadHint(at.Attention))
		}
	}

	fmt.Println()
	if len(st.Projects) == 0 {
		fmt.Println("PROJECTS\n  none registered — gravy project add <path>")
	} else {
		fmt.Println("PROJECTS")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  PROJECT\tREADY\tACTIVE\tDONE\tNOTE")
		for _, ps := range st.Projects {
			active := "-"
			if ps.Active != nil {
				active = fmt.Sprintf("%s (%s)", ps.Active.ID, ps.Active.State)
			}
			note := ps.Blocked
			if note == "" {
				note = "-"
				if ps.Project.Archived {
					// Only reachable with --all: the default listing has no archived rows.
					note = "archived"
				}
			}
			fmt.Fprintf(w, "  %s\t%d\t%s\t%d\t%s\n",
				ps.Project.Slug, ps.Counts[core.StateReady], active, ps.Counts[core.StateDone], note)
		}
		w.Flush()
	}

	fmt.Println()
	fmt.Println("HOSTS")
	for _, h := range st.Hosts {
		// A machine that is off is the answer to "why is nothing happening on yeet", so it
		// says so here rather than appearing as a row with the platform column empty.
		platform := h.OS + "/" + h.Arch
		if platform == "/" {
			platform = "-"
		}
		fmt.Printf("  %-8s %-12s workers %d/%d%s\n", h.ID, platform, h.UsedSlots, h.TotalSlots, hostNote(h))
	}
	return nil
}

// hostNote describes a host that is not simply online, and nothing at all for one that is.
func hostNote(h api.HostStatus) string {
	switch {
	case h.Online:
		return ""
	case h.CheckedAt.IsZero():
		return "  not reached yet"
	default:
		return "  OFF — " + truncateTo(firstLine(h.Unreachable), 60)
	}
}

// firstLine keeps a multi-line ssh complaint on one row.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func payloadHint(at core.Attention) string {
	keys := make([]string, 0, len(at.Payload))
	for k := range at.Payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, truncateTo(fmt.Sprint(at.Payload[k]), 40)))
	}
	return strings.Join(parts, " ")
}

func truncateTo(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
