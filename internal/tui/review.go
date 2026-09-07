package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
	rev "github.com/pot-roast-co/gravy/internal/review"
)

// reviewMode is what the keyboard is currently doing.
type reviewMode int

const (
	reviewBrowsing reviewMode = iota
	reviewConfirmReject
	reviewFeedback
)

// maxPatchLines caps an inline diff.
//
// Review is meant to be fast triage with an escape to real tools, not a diff viewer. A
// thousand-line file rendered inline buries the ten lines that mattered.
const maxPatchLines = 24

// review is the approval gate's screen: progressive disclosure over one ticket's evidence.
type review struct {
	ticketID string
	bundle   api.ReviewBundle
	loaded   bool
	err      error

	cursor   int
	expanded map[string]bool

	mode     reviewMode
	feedback string
	// notice reports the outcome of the last action, or why a key did nothing.
	notice string

	// sweep is the ordered walk of pending reviews, empty when not sweeping. It is a mode over
	// this screen rather than a second screen, so approving in a sweep and approving from the
	// card are the same code path by construction.
	sweep    []string
	sweepIdx int
	// swept counts the tickets actually decided, so the exit line can say what was done.
	swept int
}

func newReview() *review { return &review{expanded: map[string]bool{}} }

// CapturesKeys is true while a prompt is open or a sweep is running, both of which bind keys the
// global keymap also claims.
func (r *review) CapturesKeys() bool { return r.mode != reviewBrowsing || len(r.sweep) > 0 }

// Messages the screen raises for itself.
type (
	reviewLoadedMsg struct{ bundle api.ReviewBundle }
	reviewErrMsg    struct{ err error }
	reviewActedMsg  struct {
		verb string
		err  error
	}
)

func loadReview(svc api.Service, ticketID string) tea.Cmd {
	return func() tea.Msg {
		b, err := svc.GetReview(context.Background(), ticketID)
		if err != nil {
			return reviewErrMsg{err}
		}
		return reviewLoadedMsg{b}
	}
}

func (r *review) Update(msg tea.Msg, ctx ViewContext) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case sweepMsg:
		if len(msg.ids) == 0 {
			r.notice = "nothing to review"
			return r, nil
		}
		r.sweep, r.sweepIdx, r.swept = msg.ids, 0, 0
		return r, r.openCurrent(ctx)

	case enteredMsg:
		id := msg.focus
		if id == "" {
			id = firstPendingReview(ctx)
		}
		if id == "" {
			r.ticketID, r.loaded, r.err = "", false, nil
			return r, nil
		}
		r.ticketID, r.loaded, r.err, r.cursor = id, false, nil, 0
		r.expanded = map[string]bool{}
		return r, loadReview(ctx.Svc, id)

	case reviewLoadedMsg:
		r.bundle, r.loaded, r.err = msg.bundle, true, nil
		return r, nil

	case reviewErrMsg:
		r.err, r.loaded = msg.err, false
		return r, nil

	case reviewActedMsg:
		if msg.err != nil {
			r.notice = fmt.Sprintf("%s failed: %v", msg.verb, msg.err)
			return r, nil
		}
		// The ticket has left the review queue, so the screen must stop showing work that is
		// already decided.
		r.notice = fmt.Sprintf("%s %s", shortID(r.ticketID), msg.verb)
		r.ticketID, r.loaded = "", false
		if len(r.sweep) > 0 {
			r.swept++
			return r, r.advance(ctx)
		}
		return r, nil

	case tea.KeyMsg:
		return r.handleKey(msg, ctx)
	}
	return r, nil
}

func (r *review) handleKey(msg tea.KeyMsg, ctx ViewContext) (Screen, tea.Cmd) {
	key := msg.String()

	switch r.mode {
	case reviewFeedback:
		switch {
		case key == "esc":
			r.mode, r.feedback = reviewBrowsing, ""
		case key == "enter":
			if strings.TrimSpace(r.feedback) == "" {
				r.notice = "say what needs to change, or esc to cancel"
				return r, nil
			}
			fb, id := r.feedback, r.ticketID
			r.mode, r.feedback = reviewBrowsing, ""
			return r, func() tea.Msg {
				err := ctx.Svc.RequestChanges(context.Background(), id, fb)
				return reviewActedMsg{verb: "sent back for changes", err: err}
			}
		case key == "backspace":
			if r.feedback != "" {
				r.feedback = r.feedback[:len(r.feedback)-1]
			}
		case len(msg.Runes) == 1:
			r.feedback += string(msg.Runes)
			r.notice = ""
		}
		return r, nil

	case reviewConfirmReject:
		switch key {
		case "y", "Y":
			id := r.ticketID
			r.mode = reviewBrowsing
			return r, func() tea.Msg {
				err := ctx.Svc.Reject(context.Background(), id)
				return reviewActedMsg{verb: "rejected", err: err}
			}
		default:
			r.mode = reviewBrowsing
			r.notice = "rejection cancelled"
		}
		return r, nil
	}

	if !r.loaded {
		return r, nil
	}

	switch key {
	case "up", "k":
		if r.cursor > 0 {
			r.cursor--
		}
	case "down", "j":
		if r.cursor < len(r.bundle.Diff.Files)-1 {
			r.cursor++
		}
	case "enter":
		if r.cursor < len(r.bundle.Diff.Files) {
			path := r.bundle.Diff.Files[r.cursor].Path
			r.expanded[path] = !r.expanded[path]
		}
	case "a":
		id := r.ticketID
		return r, func() tea.Msg {
			err := ctx.Svc.Approve(context.Background(), id)
			return reviewActedMsg{verb: "approved and landed", err: err}
		}
	case "r":
		r.mode, r.feedback, r.notice = reviewFeedback, "", ""
	case "x":
		r.mode, r.notice = reviewConfirmReject, ""
	case "s":
		if len(r.sweep) > 0 {
			// Skipped, not decided: the ticket is left exactly as it was.
			return r, r.advance(ctx)
		}
	case "q":
		if len(r.sweep) > 0 {
			left := len(r.sweep) - r.sweepIdx
			done := r.swept
			r.endSweep()
			r.notice = fmt.Sprintf("sweep exited — %d decided, %d left untouched", done, left)
			return r, nil
		}
	case "e", "d", "!":
		// Launching an editor, difftool or shell needs an execution seam that does not exist
		// yet: ARCHITECTURE.md 1.1 permits os/exec only under internal/host, and the TUI
		// talks to api.Service rather than to a Host. Saying so beats a key that silently
		// does nothing.
		r.notice = "external tools are not wired up yet — the worktree is at " + r.bundle.Ticket.WorktreePath
	case "l":
		r.notice = "run logs arrive with GR-011"
	}
	return r, nil
}

// firstPendingReview picks the oldest ticket awaiting judgement, so pressing 5 with nothing
// selected opens the one that has been waiting longest.
func firstPendingReview(ctx ViewContext) string {
	for _, a := range ctx.Status.Attention {
		if a.Attention.Reason == core.ReasonReviewPending {
			return a.Attention.TicketID
		}
	}
	return ""
}

func (r *review) View(ctx ViewContext) string {
	th := ctx.Theme
	if ctx.Width <= 0 || ctx.Height <= 0 {
		return ""
	}

	switch {
	case r.err != nil:
		return strings.Join([]string{
			th.Danger.Render("Could not load the review."),
			"",
			th.Muted.Render(r.err.Error()),
		}, "\n")

	case r.ticketID == "":
		lines := []string{
			th.Header.Render("Nothing awaiting review"),
			"",
			th.Muted.Render("Work stops here for your judgement. When something is ready,"),
			th.Muted.Render("it appears on the dashboard and in Needs You."),
		}
		if r.notice != "" {
			lines = append([]string{th.Success.Render(r.notice), ""}, lines...)
		}
		return strings.Join(lines, "\n")

	case !r.loaded:
		// The sweep's progress stays on screen while the next card loads, or a sweep looks
		// like it stopped every time it advances.
		loading := th.Muted.Render("loading " + shortID(r.ticketID) + "…")
		if len(r.sweep) > 0 {
			return strings.Join([]string{loading, "", r.footer(th)}, "\n")
		}
		return loading
	}

	b := r.bundle
	lines := []string{
		th.Header.Render(fmt.Sprintf("%s  %s", shortID(b.Ticket.ID), b.Ticket.Title)),
		th.Muted.Render(fmt.Sprintf("  %s · %s · %s",
			projectName(b.Project), branchName(b.Ticket.Branch), b.Ticket.State)),
		"",
	}

	// What produced this, and what it cost.
	if b.Run.ID != "" {
		meta := fmt.Sprintf("  run %s  %s/%s@%s  %d turns",
			shortID(b.Run.ID), b.Run.ProviderID, b.Run.Model, b.Run.HostID, b.Run.Turns)
		if b.Run.CostUSD != nil {
			meta += fmt.Sprintf("  $%.4f", *b.Run.CostUSD)
		}
		if b.Ticket.RetryCount > 0 {
			meta += fmt.Sprintf("  retries %d", b.Ticket.RetryCount)
		}
		lines = append(lines, th.Text.Render(meta))
	}

	for _, v := range b.Validations {
		status, style := "passed", th.Success
		if v.ExitCode != 0 {
			status, style = fmt.Sprintf("FAILED (exit %d)", v.ExitCode), th.Danger
		}
		lines = append(lines, th.Muted.Render(fmt.Sprintf("  validation %-10s ", v.Step))+style.Render(status))
	}

	lines = append(lines, r.verdictLines(b, th, ctx.Width)...)

	if n := strings.TrimSpace(b.Summary.Narrative); n != "" {
		lines = append(lines, "", th.Text.Render("  "+firstLine(n)))
	}
	// Assumptions are the reason to read a diff you would otherwise wave through.
	for _, a := range b.Summary.Assumptions {
		lines = append(lines, th.Warning.Render("  ⚠ assumption: "+a))
	}

	adds, dels := b.Diff.Totals()
	lines = append(lines, "",
		th.Header.Render(fmt.Sprintf("%d file(s)  +%d -%d", len(b.Diff.Files), adds, dels)))

	for i, f := range b.Diff.Files {
		marker, style := "  ", th.Text
		if i == r.cursor {
			marker, style = "▸ ", th.Accent
		}
		lines = append(lines, style.Render(marker+columns(ctx.Width-2,
			col{text: f.Status, width: 9},
			col{text: f.Path, flex: true},
			col{text: fmt.Sprintf("+%d -%d", f.Additions, f.Deletions), width: 12, right: true},
		)))
		if r.expanded[f.Path] {
			lines = append(lines, patchLines(f.Patch, th)...)
		}
	}
	if len(b.Diff.Files) == 0 {
		lines = append(lines, th.Muted.Render("  no changes on this branch"))
	}

	return pinFooter(lines, -1, ctx.Height, th, r.footer(th))
}

// footer says what the keys do, and doubles as the prompt in the modes that take input.
func (r *review) footer(th Theme) string {
	switch r.mode {
	case reviewFeedback:
		hint := "▏  enter to send back · esc to cancel"
		if r.notice != "" {
			hint = "▏  " + r.notice
		}
		return th.Accent.Render("what needs to change: ") + th.Text.Render(r.feedback) +
			th.Muted.Render(hint)
	case reviewConfirmReject:
		return th.Danger.Render("reject this ticket and delete its worktree? ") +
			th.Muted.Render("y / n")
	}
	if r.notice != "" && len(r.sweep) == 0 {
		return th.Warning.Render(r.notice)
	}
	if len(r.sweep) > 0 {
		// The progress indicator is the whole reason a sweep feels different from a list: you
		// can see the end of it.
		progress := th.Accent.Render(fmt.Sprintf("sweep %d of %d  ", r.sweepIdx+1, len(r.sweep)))
		return progress + th.Muted.Render("a approve · r changes · x reject · s skip · q exit")
	}
	return th.Muted.Render("a approve · r request changes · x reject · enter expand · j/k move")
}

// patchLines renders a diff hunk, truncated. Review optimises for fast triage with an escape to
// real tools rather than trying to replace them.
func patchLines(patch string, th Theme) []string {
	if strings.TrimSpace(patch) == "" {
		return []string{th.Muted.Render("      (no textual diff)")}
	}
	raw := strings.Split(strings.TrimRight(patch, "\n"), "\n")
	truncated := false
	if len(raw) > maxPatchLines {
		raw, truncated = raw[:maxPatchLines], true
	}

	out := make([]string, 0, len(raw)+1)
	for _, ln := range raw {
		style := th.Muted
		switch {
		case strings.HasPrefix(ln, "+"):
			style = th.Success
		case strings.HasPrefix(ln, "-"):
			style = th.Danger
		case strings.HasPrefix(ln, "@@"):
			style = th.Accent
		}
		out = append(out, style.Render("      "+ln))
	}
	if truncated {
		out = append(out, th.Muted.Render(fmt.Sprintf(
			"      … truncated at %d lines — open the worktree for the rest", maxPatchLines)))
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---- sweep ---------------------------------------------------------------

// openCurrent loads the sweep's current ticket.
func (r *review) openCurrent(ctx ViewContext) tea.Cmd {
	if r.sweepIdx >= len(r.sweep) {
		return nil
	}
	r.ticketID, r.loaded, r.err, r.cursor = r.sweep[r.sweepIdx], false, nil, 0
	r.expanded = map[string]bool{}
	return loadReview(ctx.Svc, r.ticketID)
}

// advance moves to the next ticket in the sweep, or ends it.
//
// Every step is one deliberate decision on one ticket. Nothing here decides more than one, which
// is why there is no bulk affordance to find.
func (r *review) advance(ctx ViewContext) tea.Cmd {
	r.sweepIdx++
	if r.sweepIdx >= len(r.sweep) {
		done := r.swept
		r.endSweep()
		r.notice = fmt.Sprintf("sweep finished — %d decided", done)
		return nil
	}
	return r.openCurrent(ctx)
}

func (r *review) endSweep() {
	r.sweep, r.sweepIdx, r.swept = nil, 0, 0
}

// sweepOrder is the queue a sweep walks: reviews holding a repository first, so the sweep
// unblocks queues soonest, then oldest first within each group.
func sweepOrder(ctx ViewContext) []string {
	blocking := map[string]bool{}
	for _, p := range ctx.Status.Projects {
		if p.Blocked != "" {
			blocking[p.Project.ID] = true
		}
	}

	var held, rest []string
	for _, item := range ctx.Status.Attention {
		if item.Attention.Reason != core.ReasonReviewPending {
			continue
		}
		if blocking[item.Attention.ProjectID] || blocking[item.Project.ID] {
			held = append(held, item.Attention.TicketID)
			continue
		}
		rest = append(rest, item.Attention.TicketID)
	}
	return append(held, rest...)
}

// verdictLines renders the advisory verdict.
//
// It is labelled advisory on screen as well as in the prompt. A verdict that renders like a
// gate gets treated like one, and this never decides anything.
func (r *review) verdictLines(b api.ReviewBundle, th Theme, width int) []string {
	v := b.Verdict

	if !v.Available() {
		reason := v.Unavailable
		if reason == "" {
			reason = "no automated review ran"
		}
		return []string{th.Muted.Render("  automated review  " + reason)}
	}

	style := th.Success
	switch v.Overall {
	case rev.Concerns:
		style = th.Warning
	case rev.Fail:
		style = th.Danger
	}

	out := []string{
		th.Muted.Render("  automated review  ") + style.Render(string(v.Overall)) +
			th.Muted.Render("  (advisory)"),
	}
	if s := strings.TrimSpace(v.Summary); s != "" {
		out = append(out, th.Text.Render("  "+trunc(firstLine(s), max(0, width-2))))
	}
	for _, f := range v.Findings {
		fstyle := th.Muted
		switch f.Severity {
		case rev.High:
			fstyle = th.Danger
		case rev.Medium:
			fstyle = th.Warning
		}
		where := f.File
		if f.Line > 0 {
			where = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		if where == "" {
			where = "(no location)"
		}
		out = append(out, fstyle.Render("  · "+trunc(where+" — "+f.Rationale, max(0, width-4))))
	}
	return out
}
