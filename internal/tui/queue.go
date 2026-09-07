package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bobbybrady/gravy/internal/api"
	"github.com/bobbybrady/gravy/internal/core"
)

// queueMode is what the keyboard is doing.
type queueMode int

const (
	queueBrowsing queueMode = iota
	queueForm
	queueConfirmDelete
)

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

	mode    queueMode
	fields  []formField
	field   int
	editing string // ticket id being edited, empty when creating
	notice  string
	pendDel api.TicketDetail
}

func newQueue(state core.State) *queue {
	return &queue{state: state, selected: map[string]bool{}}
}

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

// visible applies the frame's project and text filters. The filter matches body as well as
// title: a queue you can only search by title is one you re-read instead of searching.
func (q *queue) visible(ctx ViewContext) []api.TicketDetail {
	needle := strings.ToLower(strings.TrimSpace(ctx.Filter))
	var out []api.TicketDetail
	for _, d := range q.items {
		if ctx.Project != "" && projectName(d.Project) != ctx.Project {
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

func (q *queue) handleKey(msg tea.KeyMsg, ctx ViewContext) (Screen, tea.Cmd) {
	key := msg.String()

	switch q.mode {
	case queueForm:
		return q.formKey(msg, ctx)
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

	// `n` works on an empty queue; everything else needs a row.
	if key == "n" {
		q.startForm("", core.Ticket{})
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
	case "D":
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
		// There is no edge back from Ready to Backlog in the state machine, so this direction
		// is refused rather than faked.
		return func() tea.Msg {
			return queueDoneMsg{verb: "move", err: fmt.Errorf("a Ready ticket cannot go back to the backlog; reject it instead")}
		}
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
			return queueDoneMsg{verb: "queue", err: fmt.Errorf("%s", strings.Join(failed, "; "))}
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
			q.fields[q.field].Value = v[:len(v)-1]
		}
		return q, nil

	case len(msg.Runes) == 1:
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
	if !route.Valid() {
		q.notice = fmt.Sprintf("route %q is not one of %v", route, core.AllRoutes)
		return nil
	}

	editing, svc := q.editing, ctx.Svc
	project := q.projectID(ctx)
	q.mode, q.fields = queueBrowsing, nil

	if editing != "" {
		return func() tea.Msg {
			return queueDoneMsg{verb: "saved", err: svc.UpdateTicket(context.Background(),
				core.Ticket{ID: editing, Title: title, Body: body, Route: route})}
		}
	}
	if project == "" {
		return func() tea.Msg {
			return queueDoneMsg{verb: "create", err: fmt.Errorf("no project selected — register one with `gravy project add`, or pick one with p")}
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

// projectID is the project a new ticket belongs to: the one the frame is filtered to, or the
// only one registered.
func (q *queue) projectID(ctx ViewContext) string {
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

	items := q.visible(ctx)
	title := "Backlog"
	if q.state == core.StateReady {
		title = "Ready"
	}

	lines := []string{th.Header.Render(fmt.Sprintf("%s (%d)", title, len(items)))}
	if len(items) == 0 {
		lines = append(lines,
			"",
			th.Muted.Render("  nothing here"),
			th.Key.Render("  n")+th.Muted.Render(" writes a ticket"))
		return window(append(lines, "", q.footer(ctx)), -1, ctx.Height, th)
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

	return window(append(lines, "", q.footer(ctx)), selected, ctx.Height, th)
}

func (q *queue) formView(ctx ViewContext) string {
	th := ctx.Theme
	head := "New ticket"
	if q.editing != "" {
		head = "Edit " + shortID(q.editing)
	}

	lines := []string{th.Header.Render(head), ""}
	for i, f := range q.fields {
		label := th.Muted.Render(fmt.Sprintf("  %-6s ", f.Label))
		value := f.Value
		style := th.Text
		if value == "" {
			value, style = f.Hint, th.Muted
		}
		if i == q.field {
			lines = append(lines, label+th.Accent.Render(f.Value)+th.Muted.Render("▏"))
			continue
		}
		lines = append(lines, label+style.Render(trunc(value, max(0, ctx.Width-10))))
	}

	hint := "tab to move · enter to save · esc to cancel"
	if q.notice != "" {
		hint = q.notice
	}
	return window(append(lines, "", th.Muted.Render("  "+hint)), -1, ctx.Height, th)
}

func (q *queue) footer(ctx ViewContext) string {
	th := ctx.Theme
	if q.mode == queueConfirmDelete {
		return th.Danger.Render("delete "+shortID(q.pendDel.Ticket.ID)+" permanently? ") +
			th.Muted.Render("y / n")
	}
	if q.notice != "" {
		return th.Warning.Render(q.notice)
	}
	move := "space queue it"
	if q.state == core.StateReady {
		move = "space (ready already)"
	}
	if n := len(q.selected); n > 0 {
		move += " (" + strconv.Itoa(n) + " selected)"
	}
	return th.Muted.Render(strings.Join([]string{
		"n new", "e edit", move, "x select", "J/K reorder", "+/- priority", "D delete",
	}, " · "))
}
