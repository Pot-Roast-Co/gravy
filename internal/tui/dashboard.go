package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// dashboard is the screen that replaces the terminal juggling.
//
// Three sections in one fixed order — Needs You, Running, Ready — spanning every project at
// once. The order is not configurable: it answers "what needs me" before "what is happening",
// because the first question is the one that costs you when it goes unanswered.
type dashboard struct {
	cursor int
}

// dashRow is one selectable line and where enter takes it.
type dashRow struct {
	id    string
	dest  Section
	lines []string
}

func (d dashboard) Update(msg tea.Msg, ctx ViewContext) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return d, nil
	}
	rows := dashRows(ctx)

	switch key.String() {
	case "up", "k":
		if d.cursor > 0 {
			d.cursor--
		}
	case "down", "j":
		if d.cursor < len(rows)-1 {
			d.cursor++
		}
	case "home", "g":
		d.cursor = 0
	case "end", "G":
		d.cursor = max(0, len(rows)-1)
	case "enter":
		if d.cursor < len(rows) {
			r := rows[d.cursor]
			return d, Goto(r.dest, r.id)
		}
	case "S":
		// Review latency gates throughput under the serial default, so clearing the queue
		// fast is the highest-leverage thing this screen can offer.
		return d, Sweep(sweepOrder(ctx))
	}
	return d, nil
}

func (d dashboard) View(ctx ViewContext) string {
	if ctx.Width <= 0 || ctx.Height <= 0 {
		return ""
	}
	th := ctx.Theme

	if len(ctx.Status.Projects) == 0 {
		return strings.Join([]string{
			th.Header.Render("No projects yet"),
			"",
			th.Muted.Render("Register a repository to get started:"),
			th.Key.Render("  gravy project add <path>"),
		}, "\n")
	}

	rows := dashRows(ctx)
	cursor := clamp(d.cursor, 0, max(0, len(rows)-1))

	var (
		lines    []string
		selected = -1
	)
	appendSection := func(title string, count int, body []dashRow, empty []string) {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, th.Header.Render(fmt.Sprintf("%s (%d)", title, count)))
		if len(body) == 0 {
			for _, e := range empty {
				lines = append(lines, th.Muted.Render("  "+e))
			}
			return
		}
		for _, r := range body {
			idx := indexOfRow(rows, r.id)
			for i, ln := range r.lines {
				if idx == cursor && i == 0 {
					selected = len(lines)
					lines = append(lines, th.Accent.Render("▸ "+ln))
					continue
				}
				lines = append(lines, th.Text.Render("  "+ln))
			}
		}
	}

	needs, running, ready := splitRows(ctx)
	appendSection("NEEDS YOU", len(ctx.Status.Attention), needs,
		[]string{"nothing — Gravy does not need you"})
	appendSection("RUNNING", len(ctx.Status.Running), running,
		[]string{"no agents working", "queue a ticket and run: gravy run"})
	appendSection("READY", len(ctx.Status.Ready), ready,
		[]string{"the queue is empty", `add work: gravy ticket add "<title>"`})

	return window(lines, selected, ctx.Height, th)
}

// window scrolls so the selected line stays visible, and reports what it hid rather than
// silently cutting the list off.
func window(lines []string, selected, height int, th Theme) string {
	if height <= 0 {
		return ""
	}
	if len(lines) <= height {
		return strings.Join(lines, "\n")
	}

	// Reserve the last line for the "more" indicator.
	view := height - 1
	offset := 0
	if selected >= 0 && selected >= view {
		offset = selected - view + 1
	}
	if offset+view > len(lines) {
		offset = len(lines) - view
	}

	out := append([]string{}, lines[offset:offset+view]...)
	hidden := len(lines) - view
	out = append(out, th.Muted.Render(fmt.Sprintf("  … %d more (↑↓ to scroll)", hidden)))
	return strings.Join(out, "\n")
}

// splitRows renders the three sections' rows.
func splitRows(ctx ViewContext) (needs, running, ready []dashRow) {
	w := ctx.Width - 2 // the selection marker
	th := ctx.Theme

	for _, a := range ctx.Status.Attention {
		title := a.Ticket.Title
		if title == "" {
			title = a.Attention.TicketID
		}
		needs = append(needs, dashRow{
			id: a.Attention.ID, dest: SectionNeedsYou,
			lines: []string{columns(w,
				col{text: string(a.Attention.Reason), width: 17, style: th.Warning},
				col{text: projectName(a.Project), width: 11, style: th.Muted},
				col{text: shortID(a.Attention.TicketID), width: 8, style: th.Muted},
				col{text: title, flex: true},
				col{text: age(a.Age), width: 5, right: true, style: th.Muted},
			)},
		})
	}

	for _, r := range ctx.Status.Running {
		agent := fmt.Sprintf("%s/%s", r.Run.ProviderID, r.Run.Model)
		if r.Run.HostID != "" {
			agent += "@" + r.Run.HostID
		}
		running = append(running, dashRow{
			id: r.Ticket.ID, dest: SectionRunning,
			lines: []string{columns(w,
				col{text: projectName(r.Project), width: 12, style: th.Accent},
				col{text: branchName(r.Ticket.Branch), width: 20, style: th.Muted},
				// The agent flexes: "which model on which host" is the question this row
				// exists to answer, so it is the last thing that should be truncated.
				col{text: agent, flex: true, style: th.Muted},
				col{text: r.Activity, width: 12, style: th.Success},
				col{text: age(r.Elapsed), width: 6, right: true, style: th.Muted},
			)},
		})
	}

	for _, q := range ctx.Status.Ready {
		// A ticket held by its project's cap says so on its own row. An unexplained absence
		// from Running is the failure mode this section exists to prevent.
		trailing := col{text: q.Ticket.Title, flex: true}
		if q.Held != "" {
			trailing = col{text: "held · " + q.Held, flex: true, style: th.Warning}
		}
		ready = append(ready, dashRow{
			id: q.Ticket.ID, dest: SectionReady,
			lines: []string{columns(w,
				col{text: projectName(q.Project), width: 12, style: th.Muted},
				col{text: shortID(q.Ticket.ID), width: 9, style: th.Muted},
				trailing,
			)},
		})
	}
	return needs, running, ready
}

// dashRows is every selectable row, in the order the screen presents them.
func dashRows(ctx ViewContext) []dashRow {
	needs, running, ready := splitRows(ctx)
	out := make([]dashRow, 0, len(needs)+len(running)+len(ready))
	out = append(out, needs...)
	out = append(out, running...)
	out = append(out, ready...)
	return out
}

func indexOfRow(rows []dashRow, id string) int {
	for i, r := range rows {
		if r.id == id {
			return i
		}
	}
	return -1
}
