package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// queueMode is what the keyboard is doing.
type queueMode int

const (
	queueBrowsing queueMode = iota
	queueForm
	queueConfirmDelete
	queueConfirmReject
	queueProjectPick
)

// allProjectsLabel is what an unset project filter is called. Named rather than blank: a filter
// row that renders as nothing reads as a screen that forgot to say what it is showing.
const allProjectsLabel = "all projects"

// projectFilterKey opens the Backlog's project chooser. Not `p`: that is the frame's own filter,
// checked before a screen's keys, and the whole point of this one is that the two are separate.
const projectFilterKey = "f"

// formField is one line of the ticket form.
type formField struct {
	Label string
	Value string
	// Hint is shown when the field is empty.
	Hint string
}

// queue is the Backlog and Ready screens: create and order work at speed.
//
// One type serves both, differing only in which state it lists, because they are the same list
// either side of one transition — and `space` is that transition.
type queue struct {
	state core.State

	items  []api.TicketDetail
	loaded bool
	err    error

	cursor   int
	selected map[string]bool

	// projectFilter is the Backlog's own filter: the project id its tickets must belong to,
	// empty for every project. It is deliberately not the frame's filter — the backlog is the
	// one list a human works one repository at a time, while the dashboard and the review
	// queues are read across the fleet, so the two selections must not move together.
	//
	// It lives on the screen, which the frame keeps for the session, so navigating away and back
	// returns to the same filtered list.
	projectFilter string
	// pickCursor is the row in the project chooser.
	pickCursor int

	mode       queueMode
	fields     []formField
	field      int
	editing    string // ticket id being edited, empty when creating
	notice     string
	pendDel    api.TicketDetail
	pendReject []core.Ticket
}

func newQueue(state core.State) *queue {
	return &queue{state: state, selected: map[string]bool{}}
}

// CapturesKeys is true while the form or a confirmation is open — a ticket title is exactly the
// sort of text that contains a "q".
func (q *queue) CapturesKeys() bool { return q.mode != queueBrowsing }

type (
	queueLoadedMsg struct {
		state core.State
		items []api.TicketDetail
	}
	queueErrMsg  struct{ err error }
	queueDoneMsg struct {
		verb string
		err  error
	}
)

func loadQueue(svc api.Service, state core.State) tea.Cmd {
	return func() tea.Msg {
		items, err := svc.ListQueue(context.Background(), api.TicketFilter{State: state})
		if err != nil {
			return queueErrMsg{err}
		}
		return queueLoadedMsg{state: state, items: items}
	}
}

func (q *queue) Update(msg tea.Msg, ctx ViewContext) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case enteredMsg:
		return q, loadQueue(ctx.Svc, q.state)

	case refreshedMsg:
		if q.mode == queueBrowsing {
			return q, loadQueue(ctx.Svc, q.state)
		}
		// Reloading under a half-typed form would discard what is being typed.
		return q, nil

	case queueLoadedMsg:
		if msg.state != q.state {
			return q, nil
		}
		q.items, q.loaded, q.err = msg.items, true, nil
		q.syncProjectFilter(ctx)
		present := make(map[string]bool, len(msg.items))
		for _, item := range msg.items {
			present[item.Ticket.ID] = true
		}
		for id := range q.selected {
			if !present[id] {
				delete(q.selected, id)
			}
		}
		return q, nil

	case queueErrMsg:
		q.err = msg.err
		return q, nil

	case queueDoneMsg:
		if msg.err != nil {
			q.notice = fmt.Sprintf("%s failed: %v", msg.verb, msg.err)
		} else {
			q.notice = msg.verb
		}
		return q, loadQueue(ctx.Svc, q.state)

	case tea.KeyMsg:
		return q.handleKey(msg, ctx)
	}
	return q, nil
}

// ownsProjectFilter reports whether this queue filters by project itself.
//
// The Backlog does: it is the list a human writes and grooms one repository at a time. Ready is
// what the scheduler is about to pull from, read as one queue across the fleet, so it keeps
// following the frame's filter.
func (q *queue) ownsProjectFilter() bool { return q.state == core.StateBacklog }

// visible applies the project and text filters. The text filter matches body as well as title: a
// queue you can only search by title is one you re-read instead of searching.
func (q *queue) visible(ctx ViewContext) []api.TicketDetail {
	needle := strings.ToLower(strings.TrimSpace(ctx.Filter))
	var out []api.TicketDetail
	for _, d := range q.items {
		if !q.inProject(d, ctx) {
			continue
		}
		if needle != "" {
			hay := strings.ToLower(d.Ticket.Title + " " + d.Ticket.Body)
			if !strings.Contains(hay, needle) {
				continue
			}
		}
		out = append(out, d)
	}
	return out
}

// inProject applies whichever project filter governs this queue.
func (q *queue) inProject(d api.TicketDetail, ctx ViewContext) bool {
	if q.ownsProjectFilter() {
		return q.projectFilter == "" || d.Project.ID == q.projectFilter
	}
	return ctx.Project == "" || projectName(d.Project) == ctx.Project
}

// setProjectFilter applies a project id — empty for every project — to this screen only.
//
// Selections the new filter hides are dropped. Actions already run over the visible rows, so a
// hidden selection could not be acted on; what it could do is sit in the footer's count,
// describing a selection that is not on screen, and come back the next time the filter changed.
func (q *queue) setProjectFilter(id string, ctx ViewContext) {
	q.projectFilter, q.cursor = id, 0
	shown := make(map[string]bool, len(q.items))
	for _, d := range q.visible(ctx) {
		shown[d.Ticket.ID] = true
	}
	for sel := range q.selected {
		if !shown[sel] {
			delete(q.selected, sel)
		}
	}
}

// syncProjectFilter drops a filter whose project has left the snapshot — archived or deleted,
// possibly from another client. A backlog filtered to a repository that is no longer there is
// empty for a reason nothing on the screen explains, and its name is not in the chooser either.
func (q *queue) syncProjectFilter(ctx ViewContext) {
	if q.projectFilter == "" || len(ctx.Status.Projects) == 0 {
		return
	}
	for _, ps := range ctx.Status.Projects {
		if ps.Project.ID == q.projectFilter {
			return
		}
	}
	q.setProjectFilter("", ctx)
}

// projectOption is one row of the chooser.
type projectOption struct {
	id    string
	label string
}

// projectOptions lists every registered project, behind the option to see all of them.
func (q *queue) projectOptions(ctx ViewContext) []projectOption {
	out := []projectOption{{label: allProjectsLabel}}
	for _, ps := range ctx.Status.Projects {
		out = append(out, projectOption{id: ps.Project.ID, label: projectName(ps.Project)})
	}
	return out
}

// projectLabel names the active filter.
func (q *queue) projectLabel(ctx ViewContext) string {
	if q.projectFilter == "" {
		return allProjectsLabel
	}
	for _, ps := range ctx.Status.Projects {
		if ps.Project.ID == q.projectFilter {
			return projectName(ps.Project)
		}
	}
	return q.projectFilter
}

func (q *queue) handleKey(msg tea.KeyMsg, ctx ViewContext) (Screen, tea.Cmd) {
	key := msg.String()

	switch q.mode {
	case queueForm:
		return q.formKey(msg, ctx)
	case queueProjectPick:
		return q.pickKey(msg, ctx)
	case queueConfirmReject:
		q.mode = queueBrowsing
		targets := q.pendReject
		q.pendReject = nil
		if key != "y" && key != "Y" {
			q.notice = "reject cancelled"
			return q, nil
		}
		return q, func() tea.Msg {
			var failed []string
			for _, t := range targets {
				if err := ctx.Svc.Reject(context.Background(), t.ID); err != nil {
					failed = append(failed, fmt.Sprintf("%s: %v", t.ID, err))
				}
			}
			if len(failed) > 0 {
				return queueDoneMsg{verb: "reject", err: fmt.Errorf("%s", strings.Join(failed, "; "))}
			}
			return queueDoneMsg{verb: fmt.Sprintf("rejected %d ticket(s)", len(targets))}
		}
	case queueConfirmDelete:
		q.mode = queueBrowsing
		if key == "y" || key == "Y" {
			id := q.pendDel.Ticket.ID
			return q, func() tea.Msg {
				return queueDoneMsg{verb: "deleted", err: ctx.Svc.DeleteTicket(context.Background(), id)}
			}
		}
		q.notice = "delete cancelled"
		return q, nil
	}

	// A confirmation is feedback about the last key, not a permanent state. Left in place it
	// occupies the footer, which is where the keys are documented — so after saving a ticket
	// the screen stops telling you how to queue it, which is the next thing you want.
	q.notice = ""

	// The project chooser works on an empty queue, which is exactly when a filter is the thing
	// to change: a backlog emptied by its own filter must still offer the way out of it.
	if key == projectFilterKey && q.ownsProjectFilter() {
		q.mode = queueProjectPick
		q.pickCursor = 0
		for i, opt := range q.projectOptions(ctx) {
			if opt.id == q.projectFilter {
				q.pickCursor = i
				break
			}
		}
		return q, nil
	}

	// `n` works on an empty queue; everything else needs a row.
	if key == "n" {
		q.startForm("", core.Ticket{})
		var names []string
		selected := ""
		for _, item := range ctx.Status.Projects {
			name := projectName(item.Project)
			names = append(names, name)
			if item.Project.ID == q.projectID(ctx) {
				selected = name
			}
		}
		q.fields = append(q.fields, formField{Label: "project", Value: selected, Hint: strings.Join(names, " | ")})
		return q, nil
	}

	items := q.visible(ctx)
	if len(items) == 0 {
		return q, nil
	}
	q.cursor = clamp(q.cursor, 0, len(items)-1)
	cur := items[q.cursor]

	switch key {
	case "up", "k":
		if q.cursor > 0 {
			q.cursor--
		}
	case "down", "j":
		if q.cursor < len(items)-1 {
			q.cursor++
		}
	case "x":
		// Multi-select, for the bulk move.
		if q.selected[cur.Ticket.ID] {
			delete(q.selected, cur.Ticket.ID)
		} else {
			q.selected[cur.Ticket.ID] = true
		}
	case " ", "space":
		return q, q.moveSelection(items, ctx)
	case "e":
		q.startForm(cur.Ticket.ID, cur.Ticket)
	case "r":
		if q.state == core.StateReady {
			q.mode, q.pendReject = queueConfirmReject, q.targets(items)
		}
	case "D":
		if q.state == core.StateReady {
			q.mode, q.pendReject = queueConfirmReject, q.targets(items)
			return q, nil
		}
		q.mode, q.pendDel, q.notice = queueConfirmDelete, cur, ""
	case "J":
		return q, q.reorder(items, +1, ctx)
	case "K":
		return q, q.reorder(items, -1, ctx)
	case "+", "=":
		return q, q.setPriority(cur.Ticket, cur.Ticket.Priority+1, ctx)
	case "-", "_":
		return q, q.setPriority(cur.Ticket, cur.Ticket.Priority-1, ctx)
	}
	return q, nil
}

// moveSelection flips the selected tickets between Backlog and Ready.
func (q *queue) moveSelection(items []api.TicketDetail, ctx ViewContext) tea.Cmd {
	targets := q.targets(items)
	ev := core.EventMarkReady
	verb := "queued"
	if q.state == core.StateReady {
		ev = core.EventReturnToBacklog
		verb = "sent to backlog"
	}

	svc := ctx.Svc
	return func() tea.Msg {
		var failed []string
		for _, t := range targets {
			if _, err := svc.MoveTicket(context.Background(), t.ID, ev); err != nil {
				failed = append(failed, err.Error())
			}
		}
		if len(failed) > 0 {
			// The reason is the point: a dependency that is not landed must say so.
			return queueDoneMsg{verb: verb, err: fmt.Errorf("%s", strings.Join(failed, "; "))}
		}
		return queueDoneMsg{verb: fmt.Sprintf("%s %d ticket(s)", verb, len(targets))}
	}
}

// targets is the multi-selection, or the row under the cursor when nothing is selected.
func (q *queue) targets(items []api.TicketDetail) []core.Ticket {
	var out []core.Ticket
	for _, d := range items {
		if q.selected[d.Ticket.ID] {
			out = append(out, d.Ticket)
		}
	}
	if len(out) == 0 && len(items) > 0 {
		out = append(out, items[clamp(q.cursor, 0, len(items)-1)].Ticket)
	}
	return out
}

// reorder moves the cursor's ticket one place up or down, updating exactly one row: the new
// position is the average of its new neighbours', so the rest of the queue is untouched.
func (q *queue) reorder(items []api.TicketDetail, delta int, ctx ViewContext) tea.Cmd {
	i := q.cursor
	j := i + delta
	if j < 0 || j >= len(items) {
		return nil
	}

	var before, after string
	if delta < 0 {
		if j-1 >= 0 {
			before = items[j-1].Ticket.ID
		}
		after = items[j].Ticket.ID
	} else {
		before = items[j].Ticket.ID
		if j+1 < len(items) {
			after = items[j+1].Ticket.ID
		}
	}

	id := items[i].Ticket.ID
	q.cursor = j
	svc := ctx.Svc
	return func() tea.Msg {
		return queueDoneMsg{verb: "reordered", err: svc.ReorderTicket(context.Background(), id, before, after)}
	}
}

func (q *queue) setPriority(t core.Ticket, p int, ctx ViewContext) tea.Cmd {
	t.Priority = p
	svc := ctx.Svc
	return func() tea.Msg {
		return queueDoneMsg{verb: fmt.Sprintf("priority %d", p), err: svc.UpdateTicket(context.Background(), t)}
	}
}

// ---- the project chooser -------------------------------------------------

func (q *queue) pickKey(msg tea.KeyMsg, ctx ViewContext) (Screen, tea.Cmd) {
	options := q.projectOptions(ctx)
	q.pickCursor = clamp(q.pickCursor, 0, max(0, len(options)-1))

	switch msg.String() {
	case "esc":
		q.mode, q.notice = queueBrowsing, ""
	case "up", "k":
		q.pickCursor = max(0, q.pickCursor-1)
	case "down", "j":
		q.pickCursor = min(max(0, len(options)-1), q.pickCursor+1)
	case "enter":
		if len(options) == 0 {
			q.mode = queueBrowsing
			return q, nil
		}
		chosen := options[q.pickCursor]
		q.setProjectFilter(chosen.id, ctx)
		q.mode, q.notice = queueBrowsing, "showing "+chosen.label
	}
	return q, nil
}

// ---- the form ------------------------------------------------------------

// startForm opens the create or edit form. Creating writes to Backlog immediately on save; there
// is no round trip before the ticket exists, because a thought you have to wait to record is one
// you stop recording.
func (q *queue) startForm(ticketID string, t core.Ticket) {
	route := string(t.Route)
	if route == "" {
		route = string(core.RouteImplementation)
	}
	q.mode, q.field, q.editing, q.notice = queueForm, 0, ticketID, ""
	q.fields = []formField{
		{Label: "title", Value: t.Title, Hint: "what needs doing"},
		{Label: "body", Value: t.Body, Hint: "what done looks like"},
		{Label: "route", Value: route, Hint: "implementation | cheap | standard | strong | review"},
	}
}

func (q *queue) formKey(msg tea.KeyMsg, ctx ViewContext) (Screen, tea.Cmd) {
	switch key := msg.String(); {
	case key == "esc":
		q.mode, q.fields, q.notice = queueBrowsing, nil, "cancelled"
		return q, nil

	case key == "tab" || key == "down":
		q.field = (q.field + 1) % len(q.fields)
		return q, nil

	case key == "shift+tab" || key == "up":
		q.field = (q.field - 1 + len(q.fields)) % len(q.fields)
		return q, nil

	case key == "enter":
		return q, q.submitForm(ctx)

	case key == "backspace":
		if v := q.fields[q.field].Value; v != "" {
			_, size := utf8.DecodeLastRuneInString(v)
			q.fields[q.field].Value = v[:len(v)-size]
		}
		return q, nil

	case len(msg.Runes) > 0:
		q.fields[q.field].Value += string(msg.Runes)
		q.notice = ""
		return q, nil
	}
	return q, nil
}

func (q *queue) submitForm(ctx ViewContext) tea.Cmd {
	title := strings.TrimSpace(q.fields[0].Value)
	if title == "" {
		q.notice = "a ticket needs a title"
		return nil
	}
	body := q.fields[1].Value
	route := core.Route(strings.TrimSpace(q.fields[2].Value))
	if err := knownBucket(ctx, route); err != nil {
		q.notice = err.Error()
		return nil
	}

	editing, svc := q.editing, ctx.Svc
	project := q.projectID(ctx)
	if editing == "" && len(q.fields) > 3 {
		project = ""
		for _, item := range ctx.Status.Projects {
			if projectName(item.Project) == strings.TrimSpace(q.fields[3].Value) {
				project = item.Project.ID
				break
			}
		}
	}
	if editing == "" && project == "" {
		q.notice = "choose a project in the project field (esc then P to add one)"
		return nil
	}
	q.mode, q.fields = queueBrowsing, nil

	if editing != "" {
		return func() tea.Msg {
			return queueDoneMsg{verb: "saved", err: svc.UpdateTicket(context.Background(),
				core.Ticket{ID: editing, Title: title, Body: body, Route: route})}
		}
	}

	return func() tea.Msg {
		_, err := svc.CreateTicket(context.Background(), api.CreateTicketReq{
			ProjectID: project, Title: title, Body: body, Route: route,
			// Straight to the backlog: ordering and readiness are separate decisions, made
			// on this screen, after the thought is safely written down.
			Ready: false,
		})
		return queueDoneMsg{verb: "created", err: err}
	}
}

// projectID is the project a new ticket belongs to: the one this screen is filtered to, else the
// one the frame is filtered to, or the only one registered.
func (q *queue) projectID(ctx ViewContext) string {
	if q.ownsProjectFilter() && q.projectFilter != "" {
		for _, p := range ctx.Status.Projects {
			if p.Project.ID == q.projectFilter {
				return q.projectFilter
			}
		}
	}
	if ctx.Project != "" {
		for _, p := range ctx.Status.Projects {
			if projectName(p.Project) == ctx.Project {
				return p.Project.ID
			}
		}
	}
	if len(ctx.Status.Projects) == 1 {
		return ctx.Status.Projects[0].Project.ID
	}
	return ""
}

// ---- rendering -----------------------------------------------------------

func (q *queue) View(ctx ViewContext) string {
	th := ctx.Theme
	if ctx.Width <= 0 || ctx.Height <= 0 {
		return ""
	}
	if q.err != nil {
		return strings.Join([]string{
			th.Danger.Render("Could not load the queue."), "", th.Muted.Render(q.err.Error()),
		}, "\n")
	}
	if q.mode == queueForm {
		return q.formView(ctx)
	}
	if q.mode == queueProjectPick {
		return q.pickView(ctx)
	}

	items := q.visible(ctx)
	title := "Backlog"
	if q.state == core.StateReady {
		title = "Ready"
	}

	lines := []string{th.Header.Render(fmt.Sprintf("%s (%d)", title, len(items)))}
	// The filter and its key, above the rows and whether or not there are any: a filtered list
	// that looks unfiltered is how you conclude a backlog is empty when it is only narrowed.
	if q.ownsProjectFilter() {
		lines = append(lines, th.Muted.Render("  project ")+th.Text.Render(q.projectLabel(ctx))+
			th.Muted.Render(" · ")+th.Key.Render(projectFilterKey)+th.Muted.Render(" to change"))
	}
	if len(items) == 0 {
		lines = append(lines,
			"",
			th.Muted.Render("  nothing here"),
			th.Key.Render("  n")+th.Muted.Render(" writes a ticket"))
		return pinFooter(lines, -1, ctx.Height, th, q.footer(ctx))
	}

	q.cursor = clamp(q.cursor, 0, len(items)-1)
	selected := -1
	for i, d := range items {
		marker, style := "  ", th.Text
		if i == q.cursor {
			marker, style = "▸ ", th.Accent
			selected = len(lines)
		}
		mark := " "
		if q.selected[d.Ticket.ID] {
			mark = "✓"
		}
		prio := ""
		if d.Ticket.Priority != 0 {
			prio = fmt.Sprintf("p%+d", d.Ticket.Priority)
		}
		lines = append(lines, style.Render(marker+columns(ctx.Width-2,
			col{text: mark, width: 1},
			col{text: projectName(d.Project), width: 11, style: th.Muted},
			col{text: shortID(d.Ticket.ID), width: 8, style: th.Muted},
			col{text: d.Ticket.Title, flex: true},
			col{text: prio, width: 4, right: true, style: th.Muted},
		)))
		// A ticket queued behind unlanded work says what it is waiting for, on its own row.
		if d.Blocked != "" {
			lines = append(lines, th.Warning.Render("    ⚠ "+trunc(d.Blocked, max(0, ctx.Width-6))))
		}
	}

	if body := strings.TrimSpace(items[q.cursor].Ticket.Body); body != "" {
		lines = append(lines, "", th.Muted.Render("  "+trunc(firstLine(body), max(0, ctx.Width-2))))
	}

	return pinFooter(lines, selected, ctx.Height, th, q.footer(ctx))
}

func (q *queue) pickView(ctx ViewContext) string {
	th := ctx.Theme
	options := q.projectOptions(ctx)
	q.pickCursor = clamp(q.pickCursor, 0, max(0, len(options)-1))

	lines := []string{th.Header.Render("Show which project?"), ""}
	selected := -1
	for i, opt := range options {
		marker, style := "  ", th.Text
		if i == q.pickCursor {
			marker, style = "▸ ", th.Accent
			selected = len(lines)
		}
		mark := " "
		if opt.id == q.projectFilter {
			mark = "✓"
		}
		lines = append(lines, style.Render(trunc(marker+mark+" "+opt.label, max(0, ctx.Width))))
	}

	hint := th.Muted.Render("↑/↓ choose · enter apply · esc cancel")
	return pinFooter(lines, selected, ctx.Height, th, actionFooter("", hint, ctx.Width, th))
}

func (q *queue) formView(ctx ViewContext) string {
	th := ctx.Theme
	head := "New ticket"
	if q.editing != "" {
		head = "Edit " + shortID(q.editing)
	}

	lines := []string{th.Header.Render(head), ""}
	if q.editing == "" {
		lines = append(lines, th.Muted.Render("  Saves to Backlog. Queue with space when ready."), th.Muted.Render("  Plan and instruction files are optional."), "")
	}
	for i, f := range q.fields {
		label := th.Muted.Render(fmt.Sprintf("  %-6s ", f.Label))
		value := f.Value
		style := th.Text
		if value == "" {
			value, style = f.Hint, th.Muted
		}
		if i == q.field {
			lines = append(lines, inputLines(f.Label, f.Value, ctx.Width, max(2, ctx.Height/2), th)...)
			continue
		}
		lines = append(lines, label+style.Render(trunc(value, max(0, ctx.Width-10))))
	}

	hint := "tab to move · enter to save · esc to cancel"
	if q.notice != "" {
		hint = q.notice + "\n" + hint
	}
	return pinFooter(lines, cursorRow(lines), ctx.Height, th, actionFooter("", th.Muted.Render(hint), ctx.Width, th))
}

func (q *queue) footer(ctx ViewContext) (result string) {
	defer func() { result = actionFooter(q.notice, result, ctx.Width, ctx.Theme) }()
	th := ctx.Theme
	if q.mode == queueConfirmReject {
		target := fmt.Sprintf("%d selected tickets", len(q.pendReject))
		if len(q.pendReject) == 1 {
			target = shortID(q.pendReject[0].ID)
		}
		return th.Danger.Render("reject "+target+" and remove their worktrees? ") + th.Muted.Render("y / n")
	}
	if q.mode == queueConfirmDelete {
		return th.Danger.Render("delete "+shortID(q.pendDel.Ticket.ID)+" permanently? ") +
			th.Muted.Render("y / n")
	}
	action := "D delete"
	move := "space queue it"
	if q.state == core.StateReady {
		move = "space send to backlog"
		action = "r reject"
	}
	if n := len(q.selected); n > 0 {
		move += " (" + strconv.Itoa(n) + " selected)"
	}
	return th.Muted.Render(strings.Join([]string{
		"n new ticket", "e edit", move, "x select", "J/K reorder", "+/- priority", action,
	}, " · "))
}

// knownBucket checks a route against the configured buckets rather than a list compiled into
// Gravy, so a name the user invented is accepted and a typo is still caught.
func knownBucket(ctx ViewContext, r core.Route) error {
	if strings.TrimSpace(string(r)) == "" {
		return fmt.Errorf("a ticket needs a bucket")
	}
	if len(ctx.Status.Buckets) == 0 {
		return nil // nothing to check against yet
	}
	names := make([]string, 0, len(ctx.Status.Buckets))
	for _, b := range ctx.Status.Buckets {
		if b == r {
			return nil
		}
		names = append(names, string(b))
	}
	return fmt.Errorf("no bucket called %q — try: %s", r, strings.Join(names, ", "))
}
