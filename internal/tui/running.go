package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bobbybrady/gravy/internal/api"
	"github.com/bobbybrady/gravy/internal/core"
)

// maxLogLines is how much scrollback the screen keeps in memory.
//
// The files hold everything; this is only what a human might scroll back through. Keeping an
// unbounded slice would let a chatty run grow the TUI's memory without limit.
const maxLogLines = 2000

// running is the per-run detail: what an agent is doing, why it was given this work, and the
// one key that stops it.
type running struct {
	ticketID string
	runID    string
	loaded   bool
	err      error

	// runs is every attempt on this ticket, newest first, so a retry's reason is visible
	// rather than being a number with no story.
	runs    []core.Run
	explain api.Explanation

	lines  []api.LogLine
	follow bool
	// offset is how far scrolled back from the tail, in lines. Zero is the live tail.
	offset int

	confirmKill bool
	notice      string

	stopLogs func()
}

func newRunning() *running { return &running{follow: true} }

type (
	runDetailMsg struct {
		runs    []core.Run
		explain api.Explanation
	}
	runDetailErrMsg struct{ err error }
	logOpenedMsg    struct {
		runID string
		ch    <-chan api.LogLine
		stop  func()
	}
	logLineMsg   struct{ line api.LogLine }
	logClosedMsg struct{}
	killedMsg    struct{ err error }
)

func loadRunDetail(svc api.Service, ticketID string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		runs, err := svc.ListRuns(ctx, ticketID)
		if err != nil {
			return runDetailErrMsg{err}
		}
		// The explanation is advisory: a ticket that cannot be explained is still worth
		// showing, so its failure is not the screen's failure.
		ex, _ := svc.ExplainTicket(ctx, ticketID)
		return runDetailMsg{runs: runs, explain: ex}
	}
}

func openLogs(svc api.Service, runID string) tea.Cmd {
	return func() tea.Msg {
		ch, stop, err := svc.StreamLogs(context.Background(), runID)
		if err != nil {
			return runDetailErrMsg{err}
		}
		return logOpenedMsg{runID: runID, ch: ch, stop: stop}
	}
}

// waitLogLine reads one line and re-arms, which is what makes the tail push-driven.
func waitLogLine(ch <-chan api.LogLine) tea.Cmd {
	return func() tea.Msg {
		l, ok := <-ch
		if !ok {
			return logClosedMsg{}
		}
		return logLineMsg{line: l}
	}
}

func (r *running) Update(msg tea.Msg, ctx ViewContext) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case enteredMsg:
		id := msg.focus
		if id == "" || !isRunningTicket(ctx, id) {
			id = firstRunningTicket(ctx)
		}
		if id == "" {
			r.reset()
			return r, nil
		}
		if id == r.ticketID && r.loaded {
			return r, nil // already showing it; do not restart the stream
		}
		r.reset()
		r.ticketID = id
		return r, loadRunDetail(ctx.Svc, id)

	case runDetailMsg:
		r.runs, r.explain, r.loaded, r.err = msg.runs, msg.explain, true, nil
		if len(msg.runs) > 0 && msg.runs[0].ID != r.runID {
			r.runID = msg.runs[0].ID
			return r, openLogs(ctx.Svc, r.runID)
		}
		return r, nil

	case runDetailErrMsg:
		r.err = msg.err
		return r, nil

	case logOpenedMsg:
		if msg.runID != r.runID {
			msg.stop() // a stream for a run we have navigated away from
			return r, nil
		}
		r.stopLogs = msg.stop
		return r, waitLogLine(msg.ch)

	case logLineMsg:
		r.appendLine(msg.line)
		return r, nil

	case logClosedMsg:
		r.notice = "the run ended"
		return r, nil

	case killedMsg:
		if msg.err != nil {
			r.notice = "kill failed: " + msg.err.Error()
		} else {
			r.notice = "killed — the worker slot is free"
		}
		return r, nil

	case tea.KeyMsg:
		return r.handleKey(msg, ctx)
	}
	return r, nil
}

func (r *running) handleKey(msg tea.KeyMsg, ctx ViewContext) (Screen, tea.Cmd) {
	key := msg.String()

	if r.confirmKill {
		r.confirmKill = false
		if key == "y" || key == "Y" {
			id := r.runID
			return r, func() tea.Msg {
				return killedMsg{err: ctx.Svc.KillRun(context.Background(), id)}
			}
		}
		r.notice = "kill cancelled"
		return r, nil
	}

	switch key {
	case "f":
		// Following pins the view to the tail; scrolling back turns it off, so the two
		// cannot disagree about where the viewport is.
		r.follow = !r.follow
		if r.follow {
			r.offset = 0
		}
	case "up", "k":
		r.follow = false
		if r.offset < len(r.lines)-1 {
			r.offset++
		}
	case "down", "j":
		if r.offset > 0 {
			r.offset--
		}
		if r.offset == 0 {
			r.follow = true
		}
	case "G", "end":
		r.offset, r.follow = 0, true
	case "K":
		if r.runID == "" {
			r.notice = "nothing to kill"
			return r, nil
		}
		r.confirmKill, r.notice = true, ""
	case "e", "!":
		r.notice = "external tools are not wired up yet — see the worktree on the review screen"
	}
	return r, nil
}

func (r *running) appendLine(l api.LogLine) {
	r.lines = append(r.lines, l)
	if len(r.lines) > maxLogLines {
		drop := len(r.lines) - maxLogLines
		r.lines = r.lines[drop:]
		// Scrollback is measured from the tail, so dropping from the head does not move it.
	}
}

func (r *running) reset() {
	if r.stopLogs != nil {
		r.stopLogs()
	}
	*r = running{follow: true}
}

func isRunningTicket(ctx ViewContext, ticketID string) bool {
	for _, rt := range ctx.Status.Running {
		if rt.Ticket.ID == ticketID {
			return true
		}
	}
	return false
}

func firstRunningTicket(ctx ViewContext) string {
	if len(ctx.Status.Running) > 0 {
		return ctx.Status.Running[0].Ticket.ID
	}
	return ""
}

func (r *running) View(ctx ViewContext) string {
	th := ctx.Theme
	if ctx.Width <= 0 || ctx.Height <= 0 {
		return ""
	}

	switch {
	case r.err != nil:
		return strings.Join([]string{
			th.Danger.Render("Could not load the run."), "", th.Muted.Render(r.err.Error()),
		}, "\n")

	case r.ticketID == "":
		return strings.Join([]string{
			th.Header.Render("Nothing running"),
			"",
			th.Muted.Render("Idle workers take Ready tickets automatically."),
			th.Muted.Render(`Queue work with: gravy ticket add "<title>"`),
		}, "\n")

	case !r.loaded:
		return th.Muted.Render("loading " + shortID(r.ticketID) + "…")
	}

	head := r.headerLines(ctx)
	foot := []string{"", r.footer(th)}

	logHeight := ctx.Height - len(head) - len(foot)
	if logHeight < 1 {
		// Too short for both: the log is what this screen is for.
		return window(append(head, foot...), -1, ctx.Height, th)
	}

	out := append([]string{}, head...)
	out = append(out, r.logLines(ctx, logHeight)...)
	out = append(out, foot...)
	return window(out, -1, ctx.Height, th)
}

func (r *running) headerLines(ctx ViewContext) []string {
	th := ctx.Theme
	var current core.Run
	if len(r.runs) > 0 {
		current = r.runs[0]
	}

	// Find the ticket on the dashboard snapshot for its project and branch.
	var rt api.RunningTicket
	for _, x := range ctx.Status.Running {
		if x.Ticket.ID == r.ticketID {
			rt = x
			break
		}
	}

	meta := fmt.Sprintf("  %s · %s · %s/%s@%s",
		projectName(rt.Project), branchName(rt.Ticket.Branch),
		current.ProviderID, current.Model, current.HostID)
	stats := fmt.Sprintf("  %s elapsed · %d turns · %d in / %d out · attempt %d",
		age(rt.Elapsed), current.Turns, current.TokensIn, current.TokensOut, len(r.runs))

	lines := []string{
		th.Header.Render(fmt.Sprintf("%s  %s", shortID(r.ticketID), rt.Ticket.Title)),
		th.Muted.Render(meta),
		th.Muted.Render(stats),
	}

	// Why this host and this model, answered rather than assumed.
	for _, why := range r.explain.Why {
		lines = append(lines, th.Muted.Render("  · "+why))
	}

	// Each earlier attempt, and what went wrong with it.
	if len(r.runs) > 1 {
		lines = append(lines, th.Header.Render("attempts"))
		for i := len(r.runs) - 1; i >= 0; i-- {
			run := r.runs[i]

			// core.Success is the zero value, so an unfinished run would otherwise report
			// itself as having succeeded. EndedAt is what actually distinguishes them.
			outcome, note, style := "in flight", "", th.Success
			if run.EndedAt != nil {
				outcome, note = run.FailureClass.String(), run.FailureNote
				style = th.Muted
				if run.FailureClass != core.Success {
					style = th.Warning
				}
			}
			lines = append(lines, style.Render(fmt.Sprintf("  %d. %-18s %s",
				len(r.runs)-i, outcome, trunc(note, max(0, ctx.Width-26)))))
		}
	}

	return append(lines, "")
}

func (r *running) logLines(ctx ViewContext, height int) []string {
	th := ctx.Theme
	if len(r.lines) == 0 {
		return []string{th.Muted.Render("  waiting for output…")}
	}

	end := len(r.lines) - r.offset
	if end < 1 {
		end = 1
	}
	start := end - height
	if start < 0 {
		start = 0
	}

	out := make([]string, 0, height)
	for _, l := range r.lines[start:end] {
		style := th.Text
		if l.Stream == "event" {
			style = th.Muted
		}
		out = append(out, style.Render("  "+trunc(l.Text, max(0, ctx.Width-2))))
	}
	return out
}

func (r *running) footer(th Theme) string {
	if r.confirmKill {
		return th.Danger.Render("kill this run and everything it started? ") + th.Muted.Render("y / n")
	}
	if r.notice != "" {
		return th.Warning.Render(r.notice)
	}
	mode := th.Muted.Render("paused")
	if r.follow {
		mode = th.Success.Render("following")
	}
	return mode + th.Muted.Render("  ·  f follow · K kill · j/k scroll · G tail")
}

// trunc shortens a string to width, with an ellipsis.
func trunc(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}
