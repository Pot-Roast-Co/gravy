package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// projectsLoadedMsg carries the project list and every ticket, for the counts.
type projectsLoadedMsg struct {
	projects []core.Project
	tickets  []core.Ticket
	agents   []api.AgentOption
	hosts    []string
	err      error
}

// projectDoneMsg reports a create or an edit.
type projectDoneMsg struct {
	verb string
	err  error
}

// projectMode is what the keyboard is doing.
type projectMode int

const (
	projectBrowsing projectMode = iota
	projectNaming
	projectEditingNotes
	projectConfirmDelete
	// projectConfirmArchive is a one-line y/n. Archiving is reversible with the same key, so
	// it does not ask for the project's name the way deleting does.
	projectConfirmArchive
	// projectConfig browses a project's own settings; projectEditingField types into one.
	projectConfig
	projectEditingField
)

// projects is the Projects screen: what is being built, and why.
//
// It is not the Settings screen with a nicer hat. Settings is where a registered repository's
// mechanics are configured — target branch, validation, merge mode. This is where the intent
// lives: what each project is for, what is queued in it, and which machine it is on. A project
// may have no repository at all here, because deciding what to build usually starts before
// there is anywhere to build it.
type projects struct {
	projects []core.Project
	// counts is tickets per project, by state, for the row a human scans.
	counts map[string]map[core.State]int
	err    error
	loaded bool
	// showArchived expands the collapsed group at the bottom. Collapsed by default: a finished
	// repository is kept, not shown — that is what archiving it was for.
	showArchived bool

	// cursor indexes rows(), which is the working set, then the archived group's header, then
	// the archived projects when it is expanded.
	cursor int
	mode   projectMode
	input  string
	notice string
	// scroll is the first body line drawn; notes can be longer than a screen.
	scroll int

	// fields is the selected project's own configuration, edited in place.
	fields []projectField
	field  int
	// agents and hosts are what this build can run, so a typo is refused here rather than by
	// a ticket waiting on a machine or model that does not exist.
	agents []api.AgentOption
	hosts  []string
	// draft is the project being edited, saved on leaving the config.
	draft core.Project
	dirty bool
	// buckets are the configured route names, for completing the buckets field.
	buckets []core.Route
	// matches is what the last tab found, shown under the field being edited.
	matches []string
}

func newProjects() *projects {
	return &projects{counts: map[string]map[core.State]int{}}
}

// projectRow is one line of the list: a project, or the header of the archived group.
type projectRow struct {
	project core.Project
	// group marks the "archived (N)" header, which is selectable so enter can expand it.
	group bool
	count int
}

// rows is what the list draws and what the cursor indexes: the working set, then the archived
// group's header, then the archived projects themselves when the group is expanded.
func (p *projects) rows() []projectRow {
	var out, archived []projectRow
	for _, pr := range p.projects {
		if pr.Archived {
			archived = append(archived, projectRow{project: pr})
			continue
		}
		out = append(out, projectRow{project: pr})
	}
	if len(archived) == 0 {
		return out
	}
	out = append(out, projectRow{group: true, count: len(archived)})
	if p.showArchived {
		out = append(out, archived...)
	}
	return out
}

// CapturesKeys takes the keyboard for every mode but browsing.
//
// Including projectConfig, which only browses fields: without it the frame claims esc for
// closing its help overlay, and the key that leaves the editor never arrives. Section numbers
// are unavailable while it is open, which is what being in an editor means.
func (p *projects) CapturesKeys() bool { return p.mode != projectBrowsing }

func loadProjects(svc api.Service) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		// Archived ones too: this screen is where they are found and brought back, so it is
		// the one place that asks for the whole list rather than the working set.
		list, err := svc.ListProjects(ctx, api.ProjectFilter{IncludeArchived: true})
		if err != nil {
			return projectsLoadedMsg{err: err}
		}
		// Every ticket, so a project's row can say what is in it. The counts are the reason
		// to look at this screen rather than the project list in Settings.
		tickets, err := svc.ListTickets(ctx, api.TicketFilter{})
		if err != nil {
			return projectsLoadedMsg{err: err}
		}
		// What this build can run, so a project's host and agents are checked as they are
		// typed. A client that cannot read settings simply validates nothing.
		var (
			agents []api.AgentOption
			hosts  = []string{"local"}
		)
		if st, serr := svc.GetSettings(ctx); serr == nil {
			agents = st.Agents
			for _, h := range st.Config.Hosts {
				hosts = append(hosts, h.ID)
			}
		}
		return projectsLoadedMsg{projects: list, tickets: tickets, agents: agents, hosts: hosts}
	}
}

func (p *projects) Update(msg tea.Msg, ctx ViewContext) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case enteredMsg:
		return p, loadProjects(ctx.Svc)

	case refreshedMsg:
		return p, loadProjects(ctx.Svc)

	case projectsLoadedMsg:
		p.loaded, p.err = true, msg.err
		if msg.err != nil {
			return p, nil
		}
		p.projects, p.agents, p.hosts = msg.projects, msg.agents, msg.hosts
		p.buckets = ctx.Status.Buckets
		sort.Slice(p.projects, func(i, j int) bool { return p.projects[i].Name < p.projects[j].Name })
		p.counts = map[string]map[core.State]int{}
		for _, t := range msg.tickets {
			if p.counts[t.ProjectID] == nil {
				p.counts[t.ProjectID] = map[core.State]int{}
			}
			p.counts[t.ProjectID][t.State]++
		}
		p.cursor = clamp(p.cursor, 0, max(0, len(p.rows())-1))
		return p, nil

	case projectDoneMsg:
		if msg.err != nil {
			p.notice = msg.err.Error()
			return p, nil
		}
		p.notice = msg.verb
		return p, loadProjects(ctx.Svc)

	case tea.KeyMsg:
		return p.handleKey(msg, ctx)
	}
	return p, nil
}

func (p *projects) handleKey(msg tea.KeyMsg, ctx ViewContext) (Screen, tea.Cmd) {
	key := msg.String()

	if p.mode == projectConfig {
		switch key {
		case "esc", "c":
			return p, p.leaveConfig(ctx)
		case "up", "k":
			if p.field > 0 {
				p.field--
			}
		case "down", "j":
			if p.field < len(p.fields)-1 {
				p.field++
			}
		case "enter":
			p.mode = projectEditingField
			p.input = p.fields[p.field].Get(p.draft)
		}
		return p, nil
	}

	if p.mode == projectEditingField {
		switch {
		case key == "esc":
			p.mode, p.input = projectConfig, ""
		case key == "enter":
			if err := p.fields[p.field].Set(&p.draft, p.input); err != nil {
				p.notice = err.Error()
				return p, nil
			}
			p.mode, p.input, p.notice, p.dirty = projectConfig, "", "", true
		case key == "tab":
			// The daemon knows every bucket, host and model. Reciting them from memory is
			// how the syntax gets learned by getting it wrong.
			completed, matches := completeField(
				p.fields[p.field].Label, p.input, p.agents, p.hosts, p.buckets)
			p.input, p.matches = completed, matches
			if len(matches) == 0 {
				p.notice = "nothing matches"
			}
		case key == "ctrl+u":
			p.input, p.matches = "", nil
		case key == "backspace":
			if r := []rune(p.input); len(r) > 0 {
				p.input = string(r[:len(r)-1])
			}
		case len(msg.Runes) > 0:
			p.input += string(msg.Runes)
			p.matches = nil
		}
		return p, nil
	}

	if p.mode == projectConfirmArchive {
		// One key, not a typed name: archiving keeps everything and the same key undoes it, so
		// asking for the project's slug would be ceremony for a reversible act.
		p.mode = projectBrowsing
		if key == "y" || key == "Y" {
			return p, p.archiveSelected(ctx)
		}
		p.notice = ""
		return p, nil
	}

	if p.mode != projectBrowsing {
		switch {
		case key == "esc":
			p.mode, p.input = projectBrowsing, ""
		case key == "enter":
			return p, p.submit(ctx)
		case key == "backspace":
			if r := []rune(p.input); len(r) > 0 {
				p.input = string(r[:len(r)-1])
			}
		case len(msg.Runes) > 0:
			p.input += string(msg.Runes)
		}
		return p, nil
	}

	p.notice = ""

	switch key {
	case "n":
		// A project with no repository: the case this screen exists for.
		p.mode, p.input = projectNaming, ""
		return p, nil

	case "e":
		if cur, ok := p.current(); ok {
			p.mode, p.input = projectEditingNotes, cur.Notes
		}
		return p, nil

	case "c":
		// A project's own configuration, with the project. Settings holds what is true of the
		// whole fleet.
		if cur, ok := p.current(); ok {
			p.mode, p.draft, p.dirty, p.field = projectConfig, cur, false, 0
			p.fields = projectFields(p.agents, p.hosts)
		}
		return p, nil

	case "D":
		// Typed in full, because this takes the project's tickets, runs and history with it.
		if cur, ok := p.current(); ok {
			p.mode, p.input = projectConfirmDelete, ""
			p.notice = "type " + cur.Slug + " to delete it and everything in it"
		}
		return p, nil

	case "z":
		// The same key both ways: archiving is a reversible move between the working set and
		// the group at the bottom, not a state a second key has to undo.
		if cur, ok := p.current(); ok {
			verb := "archive"
			if cur.Archived {
				verb = "unarchive"
			}
			p.mode = projectConfirmArchive
			p.notice = verb + " " + cur.Slug + "? y to confirm · any other key cancels"
		}
		return p, nil

	case "enter":
		if row, ok := p.currentRow(); ok && row.group {
			p.showArchived = !p.showArchived
			return p, nil
		}
		// Filtering the frame to this project and going to its backlog is what "see its
		// tickets" means, rather than building a second ticket list here.
		if cur, ok := p.current(); ok {
			return p, Goto(SectionBacklog, cur.ID)
		}
		return p, nil

	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
			p.scroll = 0
		}
	case "down", "j":
		if p.cursor < len(p.rows())-1 {
			p.cursor++
			p.scroll = 0
		}
	case "pgup":
		p.scroll = max(0, p.scroll-10)
	case "pgdown":
		p.scroll += 10
	case "g":
		p.scroll = 0
	case "G":
		p.scroll = 1 << 30
	}
	return p, nil
}

// leaveConfig saves the edited project, if anything changed.
//
// On leaving rather than on every keystroke: a project with a half-typed host is not a project
// the scheduler should see, and writing each field as it is typed would publish exactly that.
func (p *projects) leaveConfig(ctx ViewContext) tea.Cmd {
	p.mode = projectBrowsing
	if !p.dirty {
		return nil
	}
	draft, svc := p.draft, ctx.Svc
	p.dirty = false
	return func() tea.Msg {
		return projectDoneMsg{verb: "saved " + draft.Slug, err: svc.UpdateProject(context.Background(), draft)}
	}
}

// currentRow is the selected line, which may be the archived group's header.
func (p *projects) currentRow() (projectRow, bool) {
	rows := p.rows()
	if p.cursor < 0 || p.cursor >= len(rows) {
		return projectRow{}, false
	}
	return rows[p.cursor], true
}

// current is the selected project. The archived group's header is not one, so the keys that act
// on a project do nothing there rather than acting on whichever project is next to it.
func (p *projects) current() (core.Project, bool) {
	row, ok := p.currentRow()
	if !ok || row.group {
		return core.Project{}, false
	}
	return row.project, true
}

// archiveSelected moves the selected project between the working set and the archived group.
//
// The group is expanded on archiving so the project does not appear to vanish: it went into a
// collapsed group, which is a different thing from being deleted and has to look like it.
func (p *projects) archiveSelected(ctx ViewContext) tea.Cmd {
	cur, ok := p.current()
	if !ok {
		return nil
	}
	archived := !cur.Archived
	verb := "archived "
	if !archived {
		verb = "unarchived "
	}
	p.showArchived = true
	svc := ctx.Svc
	return func() tea.Msg {
		return projectDoneMsg{
			verb: verb + cur.Slug,
			err:  svc.ArchiveProject(context.Background(), cur.ID, archived),
		}
	}
}

// submit applies whatever was being typed.
func (p *projects) submit(ctx ViewContext) tea.Cmd {
	value := strings.TrimSpace(p.input)
	mode := p.mode
	p.mode, p.input = projectBrowsing, ""

	svc := ctx.Svc
	switch mode {
	case projectNaming:
		if value == "" {
			p.notice = "a project needs a name"
			return nil
		}
		return func() tea.Msg {
			_, err := svc.AddProject(context.Background(), api.AddProjectReq{Name: value})
			return projectDoneMsg{verb: "created " + value, err: err}
		}

	case projectEditingNotes:
		cur, ok := p.current()
		if !ok {
			return nil
		}
		cur.Notes = value
		return func() tea.Msg {
			return projectDoneMsg{verb: "saved", err: svc.UpdateProject(context.Background(), cur)}
		}

	case projectConfirmDelete:
		cur, ok := p.current()
		if !ok {
			return nil
		}
		if value != cur.Slug {
			p.notice = "not deleted — the name did not match"
			return nil
		}
		p.cursor = max(0, p.cursor-1)
		return func() tea.Msg {
			return projectDoneMsg{
				verb: "deleted " + cur.Slug,
				err:  svc.DeleteProject(context.Background(), cur.ID),
			}
		}
	}
	return nil
}

// ---- rendering -----------------------------------------------------------

func (p *projects) View(ctx ViewContext) string {
	th := ctx.Theme
	if ctx.Width <= 0 || ctx.Height <= 0 {
		return ""
	}
	if p.err != nil {
		return strings.Join([]string{
			th.Danger.Render("Could not load projects."), "", th.Muted.Render(p.err.Error()),
		}, "\n")
	}
	if !p.loaded {
		return th.Muted.Render("loading projects…")
	}

	rows := p.rows()
	// The count is the working set. Archived projects are counted in their own group's header,
	// because a heading that grows for work that is over is the problem this screen had.
	active := 0
	for _, pr := range p.projects {
		if !pr.Archived {
			active++
		}
	}
	lines := []string{th.Header.Render(fmt.Sprintf("Projects (%d)", active))}

	if len(p.projects) == 0 {
		lines = append(lines,
			"",
			th.Muted.Render("  Nothing registered yet."),
			"",
			th.Key.Render("  P")+th.Muted.Render("  add a repository"),
			th.Key.Render("  n")+th.Muted.Render("  start a project with no repository yet"),
		)
		return p.scrolled(lines, ctx, th)
	}

	for i, row := range rows {
		marker, style := "  ", th.Text
		if i == p.cursor {
			marker, style = "▸ ", th.Accent
		}
		if row.group {
			caret := "▸"
			if p.showArchived {
				caret = "▾"
			}
			lines = append(lines, "", style.Render(fmt.Sprintf(
				"%s%s archived (%d)", marker, caret, row.count)))
			continue
		}
		pr := row.project
		where := pr.HostID
		if where == "" {
			where = "local"
		}
		// Muted rather than absent: an archived project is still a project, and the row has to
		// say why it is sitting apart from the others.
		summary := p.countSummary(pr.ID)
		if pr.Archived {
			summary = "archived · " + summary
			if i != p.cursor {
				style = th.Muted
			}
		}
		lines = append(lines, style.Render(marker+columns(ctx.Width-2,
			col{text: pr.Name, width: 22},
			col{text: where, width: 10},
			col{text: summary, flex: true},
		)))
	}

	if p.mode == projectConfig || p.mode == projectEditingField {
		lines = append(lines, "", th.Header.Render("  "+p.draft.Name+"  settings"))
		lines = append(lines, p.configLines(ctx, th)...)
		return p.scrolled(lines, ctx, th)
	}

	if cur, ok := p.current(); ok {
		lines = append(lines, "", th.Header.Render("  "+cur.Name))
		lines = append(lines, p.detail(cur, ctx, th)...)
	}

	// The archive confirm asks its question in the footer: there is nothing to type.
	if p.mode != projectBrowsing && p.mode != projectConfirmArchive {
		label := "  name  "
		switch p.mode {
		case projectEditingNotes:
			label = "  notes "
		case projectConfirmDelete:
			label = "  delete "
		}
		lines = append(lines, "",
			th.Muted.Render(label)+th.Accent.Render(p.input)+th.Muted.Render("▏"))
	}

	return p.scrolled(lines, ctx, th)
}

// configLines renders a project's own settings as an editable list.
func (p *projects) configLines(ctx ViewContext, th Theme) []string {
	out := make([]string, 0, len(p.fields)*2)
	for i, f := range p.fields {
		marker, style := "  ", th.Text
		if i == p.field {
			marker, style = "▸ ", th.Accent
		}

		value := f.Get(p.draft)
		if p.mode == projectEditingField && i == p.field {
			out = append(out, style.Render(fmt.Sprintf("  %s%-18s", marker, f.Label))+
				th.Accent.Render(p.input)+th.Muted.Render("▏"))
			if len(p.matches) > 1 {
				out = append(out, th.Muted.Render("      "+matchList(p.matches, max(20, ctx.Width-10))))
			}
			out = append(out, th.Muted.Render("      "+f.Hint))
			continue
		}
		if value == "" {
			// An empty allowlist refuses every command an agent tries, which is not obvious
			// from a blank line — and cost two runs today before anyone noticed.
			value = th.Danger.Render("(not set)")
			if f.Label == "allowed commands" {
				value = th.Danger.Render("(none — agents will be refused every command)")
			}
			out = append(out, style.Render(fmt.Sprintf("  %s%-18s", marker, f.Label))+value)
		} else {
			out = append(out, style.Render(fmt.Sprintf("  %s%-18s", marker, f.Label))+
				th.Muted.Render(trunc(value, max(10, ctx.Width-30))))
		}
		if i == p.field {
			out = append(out, th.Muted.Render("      "+f.Hint))
		}
	}
	return out
}

// detail renders the selected project.
func (p *projects) detail(pr core.Project, ctx ViewContext, th Theme) []string {
	var out []string

	if pr.Archived {
		// Said plainly, because a ready ticket here will sit ready forever and the queue
		// screen's explanation is one screen away.
		out = append(out, th.Muted.Render(
			"    archived — nothing new starts here until z brings it back"))
	}

	if pr.RepoPath == "" {
		// Said plainly rather than shown as a blank path: a project with no repository cannot
		// run anything, and finding that out from a ticket that never starts is worse.
		out = append(out, th.Warning.Render("    no repository yet — nothing can run here"))
	} else {
		out = append(out, th.Muted.Render("    "+pr.RepoPath))
		out = append(out, th.Muted.Render(fmt.Sprintf(
			"    target %s · %s · %s", pr.TargetBranch, pr.MergeMode, serialLabel(pr))))
	}

	if len(pr.Routes) > 0 {
		out = append(out, th.Muted.Render("    agents "+formatProjectRoutes(pr.Routes)))
	}

	notes := strings.TrimSpace(pr.Notes)
	if notes == "" {
		out = append(out, "", th.Muted.Render("    no notes — e to write what this is for"))
		return out
	}
	out = append(out, "")
	for _, ln := range wrapText(notes, max(20, ctx.Width-6)) {
		out = append(out, th.Text.Render("    "+ln))
	}
	return out
}

// countSummary renders a project's tickets by state, busiest first.
func (p *projects) countSummary(projectID string) string {
	counts := p.counts[projectID]
	if len(counts) == 0 {
		return "no tickets"
	}
	// A fixed order, so a row does not reshuffle as work moves through it.
	order := []core.State{
		core.StateNeedsYou, core.StateReview, core.StateRunning,
		core.StateReady, core.StateBacklog, core.StateDone,
	}
	var parts []string
	for _, st := range order {
		if n := counts[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st))
		}
	}
	if len(parts) == 0 {
		return "no tickets"
	}
	return strings.Join(parts, " · ")
}

// serialLabel says how a project runs its work.
func serialLabel(p core.Project) string {
	if p.ParallelMode {
		return fmt.Sprintf("parallel ×%d", p.MaxConcurrency)
	}
	return "serial"
}

// scrolled draws the body at the current offset, with the footer pinned to the bottom.
func (p *projects) scrolled(lines []string, ctx ViewContext, th Theme) string {
	footer := p.footer(th)

	body := ctx.Height - 2
	if body < 1 {
		return trunc(footer, ctx.Width)
	}
	if len(lines) <= body {
		p.scroll = 0
		return strings.Join(append(lines, "", footer), "\n")
	}

	view := body - 1
	maxOffset := len(lines) - view
	p.scroll = clamp(p.scroll, 0, maxOffset)

	out := append([]string{}, lines[p.scroll:p.scroll+view]...)
	out = append(out, th.Muted.Render(fmt.Sprintf(
		"  ↑ %d · ↓ %d · g top · G bottom", p.scroll, maxOffset-p.scroll)))
	return strings.Join(append(out, "", footer), "\n")
}

func (p *projects) footer(th Theme) string {
	if p.notice != "" {
		return th.Warning.Render("  " + p.notice)
	}
	switch p.mode {
	case projectNaming:
		return th.Muted.Render("  enter to create · esc to cancel")
	case projectEditingNotes:
		return th.Muted.Render("  enter to save · esc to cancel")
	case projectConfirmDelete:
		return th.Danger.Render("  type the project name to confirm · esc to cancel")
	case projectConfig:
		return th.Muted.Render("  enter to edit · j/k move · esc saves and closes")
	case projectEditingField:
		return th.Muted.Render("  tab completes · enter to accept · ctrl+u clear · esc to cancel")
	}
	return th.Muted.Render(
		"  c settings · e notes · n new · z archive · D delete · enter its tickets · P add · j/k move")
}
