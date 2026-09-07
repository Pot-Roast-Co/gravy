package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bobbybrady/gravy/internal/api"
	"github.com/bobbybrady/gravy/internal/core"
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
}

func newReview() *review { return &review{expanded: map[string]bool{}} }

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
		// The ticket has left the review queue. Clearing it is what makes the sweep in
		// GR-038 possible: the screen is never showing work that is already decided.
		r.notice = fmt.Sprintf("%s %s", r.ticketID, msg.verb)
		r.ticketID, r.loaded = "", false
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
		return th.Muted.Render("loading " + shortID(r.ticketID) + "…")
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

	// Advisory only. It never decides the ticket's fate.
	if b.Verdict != "" {
		lines = append(lines, th.Muted.Render("  automated review  ")+th.Text.Render(b.Verdict))
	} else {
		lines = append(lines, th.Muted.Render("  automated review  not yet available (GR-020)"))
	}

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

	lines = append(lines, "", r.footer(th))
	return window(lines, -1, ctx.Height, th)
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
	if r.notice != "" {
		return th.Warning.Render(r.notice)
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
