package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
	rev "github.com/pot-roast-co/gravy/internal/review"
)

// reviewMode is what the keyboard is currently doing.
type reviewMode int

const (
	reviewBrowsing reviewMode = iota
	reviewConfirmReject
	// reviewDiscuss is Request changes: a conversation that agrees the correction, and the
	// preservation constraints that go with it, before any of it reaches an agent.
	reviewDiscuss
	reviewTry
	reviewPreviewCommand
	reviewTrySaving
	// reviewLanding is the window between pressing "a" and the merge finishing. It exists
	// because that window is minutes long — rebase, re-validation, push — and the screen used
	// to spend all of it looking untouched, which invites a second press the state machine
	// then refuses.
	reviewLanding
)

// maxPatchLines caps an inline diff.
//
// Review is fast triage with an escape to real tools, not a diff viewer — but the cap is a guard
// against a generated file with ten thousand lines, not a reading limit. It was 24, which cut an
// ordinary change in half and sent people to an editor to read something the screen could have
// shown them.
const maxPatchLines = 500

// review is the approval gate's screen: progressive disclosure over one ticket's evidence.
//
// The body scrolls: a diff is the long thing on this screen, so j/k move through content and
// tab moves between files.
type review struct {
	ticketID string
	bundle   api.ReviewBundle
	loaded   bool
	err      error

	cursor   int
	expanded map[string]bool
	// scroll is the first body line drawn; scrollToCursor pulls the selected file into view
	// after tab moves it, without overriding a scroll made by hand.
	scroll         int
	scrollToCursor bool
	// showTicket reveals what was asked for. Reviewing a diff without the ticket in front of
	// you is checking whether it looks reasonable, not whether it did what was asked.
	showTicket bool

	mode reviewMode
	// talk is the Request-changes conversation. It owns its own keys and body while it is open.
	talk         *discussion
	previewInput string
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

func newReview() *review { return &review{expanded: map[string]bool{}, talk: newDiscussion()} }

// CapturesKeys is true while a prompt is open or a sweep is running, both of which bind keys the
// global keymap also claims.
func (r *review) CapturesKeys() bool {
	// Landing is deliberately not key-capturing. It runs in the daemon and outlives this
	// screen, so holding the human here for the minutes it takes would be a worse bargain than
	// letting them go and look at something else.
	return (r.mode != reviewBrowsing && r.mode != reviewLanding) || len(r.sweep) > 0
}

// Messages the screen raises for itself.
type (
	reviewLoadedMsg struct{ bundle api.ReviewBundle }
	reviewErrMsg    struct{ err error }
	reviewActedMsg  struct {
		verb string
		err  error
		// state is what the ticket reached, for actions that have more than one non-error
		// outcome. Landing can park instead of merging, and parking reports no error.
		state core.State
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
	case previewSavedMsg:
		if msg.ticketID != r.ticketID {
			return r, nil
		}
		r.mode = reviewTry
		if msg.err != nil {
			r.notice = "save failed: " + msg.err.Error()
		} else {
			r.bundle.Project.PreviewCommand = msg.command
			r.notice = "saved; press a to run the app"
		}
		return r, nil
	case reviewTriedMsg:
		if msg.ticketID != r.ticketID {
			return r, nil
		}
		if errors.Is(msg.err, host.ErrInterrupted) {
			r.notice = msg.action + " stopped"
		} else if msg.err != nil {
			r.notice = msg.action + ": " + msg.err.Error()
		} else {
			r.notice = msg.action + " finished successfully"
		}
		return r, nil

	case rereviewedMsg:
		if msg.err != nil {
			r.notice = "review: " + msg.err.Error()
			return r, nil
		}
		// Reload so the new verdict is what the card shows, rather than the one it replaced.
		r.notice, r.loaded = "", false
		return r, loadReview(ctx.Svc, r.ticketID)

	case checkoutReadyMsg:
		if msg.err != nil {
			r.notice = msg.err.Error()
			return r, nil
		}
		if _, err := os.Stat(msg.path); err != nil {
			r.notice = "the changes are not on this machine: " + msg.path
			return r, nil
		}
		name, args, err := host.Editor()
		if err != nil {
			r.notice = err.Error()
			return r, nil
		}
		r.notice = ""
		// The directory, not the files: the editor's own changed-file list is the point, and
		// it only appears when the editor knows it is looking at a repository.
		cmd := host.LocalCommand(name, append(args, "."), msg.path)
		return r, tea.ExecProcess(cmd, func(err error) tea.Msg {
			return externalDoneMsg{tool: externalEditor, err: err}
		})

	case externalDoneMsg:
		// A tool that exits non-zero is worth saying, but it is not a failure of the review:
		// the diff is unchanged and the decision is still the human's to make.
		if msg.err != nil {
			r.notice = msg.tool.String() + ": " + msg.err.Error()
		}
		return r, nil

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

	case refreshedMsg:
		// Not while a prompt or a conversation is open: re-reading is free, but rebuilding the
		// card underneath someone who is mid-sentence is not.
		if r.ticketID != "" && r.mode != reviewPreviewCommand && r.mode != reviewTrySaving &&
			r.mode != reviewDiscuss {
			return r, loadReview(ctx.Svc, r.ticketID)
		}
		return r, nil

	case discussionLoadedMsg:
		return r, r.talk.Update(msg)

	case discussionClosedMsg:
		// Cancelling is not a decision. The ticket is still in Review, still carrying whatever
		// it carried before, and the card goes back to offering the same three keys.
		r.mode = reviewBrowsing
		if msg.err != nil {
			r.notice = "closing the discussion: " + msg.err.Error()
		} else {
			r.notice = "discussion cancelled — the ticket is still awaiting your decision"
		}
		return r, nil

	case reviewLoadedMsg:
		r.bundle, r.loaded, r.err = msg.bundle, true, nil
		return r, nil

	case reviewErrMsg:
		r.err, r.loaded = msg.err, false
		return r, nil

	case reviewActedMsg:
		r.mode = reviewBrowsing
		if msg.err != nil {
			// A duplicate approval is not a failed one. The work is landing or landed, and
			// saying "failed" over a merge that succeeded is how a correct refusal becomes a
			// bug report.
			if errors.Is(msg.err, core.ErrAlreadyLanded) {
				r.notice = "already landing — the first approval is still going"
				return r, nil
			}
			r.notice = fmt.Sprintf("%s failed: %v", msg.verb, msg.err)
			return r, nil
		}
		// A landing that parked did not land, and saying it did is how a human learns their
		// work merged when it is sitting in Needs You with a failed validation. Parking is
		// not an error, so only the state can tell these apart.
		if msg.state == core.StateNeedsYou {
			r.notice = shortID(r.ticketID) + " did not land — parked for you, see Needs You (8)"
			r.ticketID, r.loaded = "", false
			if len(r.sweep) > 0 {
				r.swept++
				return r, r.advance(ctx)
			}
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

	// While a landing is in flight the decisions have already been made, so the keys that make
	// one are refused rather than queued. This is the actual fix for the double press: the
	// second "a" never reaches the daemon, so the daemon never has to refuse it.
	if r.mode == reviewLanding {
		switch key {
		case "a", "r", "x", "v", "T", "s":
			r.notice = "landing — this takes a few minutes; it finishes even if you leave"
			return r, nil
		}
	}

	switch r.mode {
	case reviewTry, reviewPreviewCommand, reviewTrySaving:
		return r.tryKey(msg, ctx)
	case reviewDiscuss:
		return r, r.talk.handleKey(msg, ctx)

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
		r.scrollToCursor = false
		if r.scroll > 0 {
			r.scroll--
		}
	case "down", "j":
		r.scrollToCursor = false
		r.scroll++
	case "pgup":
		r.scrollToCursor = false
		r.scroll -= 10
		if r.scroll < 0 {
			r.scroll = 0
		}
	case "pgdown":
		r.scrollToCursor = false
		r.scroll += 10
	case "g":
		r.scrollToCursor, r.scroll = false, 0
	case "G":
		r.scrollToCursor, r.scroll = false, 1<<30
	case "T":
		r.mode, r.notice = reviewTry, ""
	case "t":
		r.showTicket = !r.showTicket
	case "v":
		// The automatic pass runs once, inside the run. When it failed for a reason since
		// fixed, the verdict on this card stays broken with no way to ask again.
		id, svc := r.ticketID, ctx.Svc
		if id == "" {
			return r, nil
		}
		r.notice = "asking for a fresh verdict…"
		return r, func() tea.Msg {
			return rereviewedMsg{err: svc.Rereview(context.Background(), id)}
		}
	case "tab":
		if n := len(r.bundle.Diff.Files); n > 0 {
			r.cursor = (r.cursor + 1) % n
			r.scrollToCursor = true
		}
	case "shift+tab":
		if n := len(r.bundle.Diff.Files); n > 0 {
			r.cursor = (r.cursor - 1 + n) % n
			r.scrollToCursor = true
		}
	case "enter":
		if r.cursor < len(r.bundle.Diff.Files) {
			path := r.bundle.Diff.Files[r.cursor].Path
			r.expanded[path] = !r.expanded[path]
		}
	case "a":
		id := r.ticketID
		r.mode, r.notice = reviewLanding, "landing — rebasing, re-validating and pushing"
		return r, func() tea.Msg {
			state, err := ctx.Svc.Approve(context.Background(), id)
			return reviewActedMsg{verb: "approved and landed", err: err, state: state}
		}
	case "r":
		// Request changes opens a conversation, not a send. Nothing is queued and nothing is
		// authorised until the instruction it produces is confirmed.
		//
		// The opening message is seeded with the automated review's own words: it has just read
		// the diff and said what is wrong with it, and making the human retype that is asking
		// them to be a courier between two machines.
		r.mode, r.notice = reviewDiscuss, ""
		return r, r.talk.open(ctx.Svc, r.ticketID, r.bundle.Ticket.Title,
			verdictAsFeedback(r.bundle.Verdict))
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
	case "e":
		return r, r.openInEditor(ctx)
	case "d":
		return r, r.handOff(externalDifftool)
	case "!":
		return r, r.handOff(externalShell)
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
	if ctx.Width <= 0 || ctx.Height <= 0 {
		return ""
	}
	if r.mode == reviewTry || r.mode == reviewPreviewCommand || r.mode == reviewTrySaving {
		return r.tryView(ctx)
	}
	if r.mode == reviewDiscuss {
		return r.talk.View(ctx)
	}
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
			return strings.Join([]string{loading, "", r.footer(th, ctx.Width)}, "\n")
		}
		return loading
	}

	b := r.bundle
	lines := []string{
		th.Header.Render(fmt.Sprintf("%s  %s", shortID(b.Ticket.ID), b.Ticket.Title)),
		th.Muted.Render(fmt.Sprintf("  %s · %s · %s",
			projectName(b.Project), mergePath(b.Project, b.Ticket), b.Ticket.State)),
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

	// What was asked for, so the diff can be checked against it rather than merely read.
	if r.showTicket {
		if body := strings.TrimSpace(b.Ticket.Body); body != "" {
			lines = append(lines, "")
			for _, ln := range wrapText(body, max(20, ctx.Width-4)) {
				lines = append(lines, th.Text.Render("  "+ln))
			}
			lines = append(lines, "")
		} else {
			lines = append(lines, "", th.Muted.Render("  this ticket has no body"), "")
		}
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

	cursorLine := -1
	for i, f := range b.Diff.Files {
		marker, style := "  ", th.Text
		if i == r.cursor {
			marker, style = "▸ ", th.Accent
			cursorLine = len(lines)
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

	return r.scrolled(lines, cursorLine, ctx, th)
}

// scrolled draws the body at the current offset, with the footer pinned to the bottom.
//
// Moving between files scrolls to the file; scrolling by hand does not fight back afterwards,
// which is why the two are tracked separately.
func (r *review) scrolled(lines []string, cursorLine int, ctx ViewContext, th Theme) string {
	footer := r.footer(th, ctx.Width)

	body := ctx.Height - 1 - lipgloss.Height(footer)
	if body < 1 {
		return trunc(footer, ctx.Width)
	}
	if len(lines) <= body {
		r.scroll = 0
		return strings.Join(append(lines, "", footer), "\n")
	}

	view := body - 1 // one line for the indicator
	maxOffset := len(lines) - view

	if r.scrollToCursor && cursorLine >= 0 {
		switch {
		case cursorLine < r.scroll:
			r.scroll = cursorLine
		case cursorLine >= r.scroll+view:
			r.scroll = cursorLine - view + 1
		}
	}
	r.scroll = clamp(r.scroll, 0, maxOffset)

	out := append([]string{}, lines[r.scroll:r.scroll+view]...)
	above, below := r.scroll, maxOffset-r.scroll
	switch {
	case below == 0:
		out = append(out, th.Muted.Render(fmt.Sprintf("  ↑ %d more above · g top", above)))
	case above == 0:
		out = append(out, th.Muted.Render(fmt.Sprintf("  ↓ %d more below · G bottom", below)))
	default:
		out = append(out, th.Muted.Render(fmt.Sprintf("  ↑ %d · ↓ %d · g top · G bottom", above, below)))
	}
	return strings.Join(append(out, "", footer), "\n")
}

// footer says what the keys do, and doubles as the prompt in the modes that take input.
func (r *review) footer(th Theme, width int) string {
	switch r.mode {
	case reviewConfirmReject:
		return th.Danger.Render("reject this ticket and delete its worktree? ") +
			th.Muted.Render("y / n")
	}
	if r.mode == reviewLanding {
		return th.Accent.Render(trunc(r.notice, width))
	}
	withNotice := func(actions string) string {
		if r.notice == "" {
			return actions
		}
		return th.Warning.Render(trunc(r.notice, width)) + "\n" + actions
	}
	if len(r.sweep) > 0 {
		// The progress indicator is the whole reason a sweep feels different from a list: you
		// can see the end of it.
		progress := th.Accent.Render(fmt.Sprintf("sweep %d of %d  ", r.sweepIdx+1, len(r.sweep)))
		return withNotice(progress + th.Muted.Render("a approve · r changes · x reject · T try feature · s skip · q exit"))
	}
	ticket := "t ticket"
	if r.showTicket {
		ticket = "t hide ticket"
	}
	return withNotice(actionFooter("", th.Muted.Render("a approve · r changes · x reject · T try feature · "+ticket+" · v re-review · e editor · d difftool · ! shell · enter expand · tab file"), width, th))
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

// mergePath renders "branch → target", the second half being where "a" on this screen sends the
// work.
//
// It is on the card because a target branch resolved wrong at registration stays wrong in
// silence: every approval reports success, pushes, and merges somewhere nobody meant. The moment
// before approval is the last cheap chance to notice, and it used to say nothing at all.
//
// A project with no target branch renders as the branch alone rather than an arrow pointing at
// nothing.
func mergePath(p core.Project, t core.Ticket) string {
	branch := branchName(t.Branch)
	if strings.TrimSpace(p.TargetBranch) == "" {
		return branch
	}
	return branch + " → " + branchName(p.TargetBranch)
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

// verdictAsFeedback turns an advisory verdict into a first draft of what to say.
//
// A draft, not a decision: it lands in the discussion's editable message field, and a verdict
// that found nothing leaves it empty rather than opening with a cheerful summary of the agent's
// own success. The severities travel with it, because "low" and "high" change what matters first.
func verdictAsFeedback(v rev.Verdict) string {
	if !v.Available() || len(v.Findings) == 0 {
		return ""
	}
	var b strings.Builder
	if s := strings.TrimSpace(v.Summary); s != "" {
		b.WriteString(s)
	}
	for _, f := range v.Findings {
		where := f.File
		if f.Line > 0 {
			where = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		if where == "" {
			where = "general"
		}
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "[%s] %s: %s", f.Severity, where, strings.TrimSpace(f.Rationale))
	}
	return b.String()
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
	// Wrapped rather than truncated. The screen scrolls now, so cutting a reviewer's reasoning
	// at the right-hand edge buys nothing and costs the half of the sentence that says what to
	// do about it.
	if s := strings.TrimSpace(v.Summary); s != "" {
		for _, ln := range wrapText(s, max(20, width-4)) {
			out = append(out, th.Text.Render("  "+ln))
		}
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
		// The severity as a word, not only as a colour: high and medium are otherwise the
		// same line in two hues, which is no use to anyone reading in a light terminal or
		// skimming for the one that matters.
		head := fmt.Sprintf("  · [%s] %s", f.Severity, where)
		out = append(out, fstyle.Render(head))
		for _, ln := range wrapText(strings.TrimSpace(f.Rationale), max(20, width-8)) {
			out = append(out, th.Muted.Render("      "+ln))
		}
	}
	return out
}

// rereviewedMsg reports a re-run of the advisory review.
type rereviewedMsg struct{ err error }

// checkoutReadyMsg carries a prepared review checkout back to the screen.
type checkoutReadyMsg struct {
	path string
	err  error
}

// openInEditor prepares the ticket's work as uncommitted changes, then opens an editor on it.
//
// Not the ticket's own worktree: Gravy commits what the agent produced, so that worktree is
// clean, and an editor's git integration — gutter marks, the changed-file list, click-to-diff —
// reports uncommitted changes and therefore shows nothing. Reading the work there means reading
// files with no indication of what moved. The checkout puts the same content on disk with the
// index at its base, which is the state every editor is built to display.
func (r *review) openInEditor(ctx ViewContext) tea.Cmd {
	id, svc := r.ticketID, ctx.Svc
	if id == "" {
		r.notice = "nothing selected"
		return nil
	}
	r.notice = "preparing the changes…"
	return func() tea.Msg {
		path, err := svc.ReviewCheckout(context.Background(), id)
		return checkoutReadyMsg{path: path, err: err}
	}
}

// externalTool is one of the review screen's escape hatches to a real tool.
type externalTool int

const (
	externalEditor externalTool = iota
	externalDifftool
	externalShell
)

// String names the tool, for a message a human reads.
func (t externalTool) String() string {
	switch t {
	case externalShell:
		return "shell"
	case externalDifftool:
		return "difftool"
	default:
		return "editor"
	}
}

// externalDoneMsg reports how a hand-off went.
type externalDoneMsg struct {
	tool externalTool
	err  error
}

// handOff suspends the TUI and gives the terminal to a real tool.
//
// Review is the bottleneck the product creates, so it optimises for fast triage with a one-key
// escape to real tools rather than trying to replace them (ARCHITECTURE.md §9). Reading a diff
// in your own editor, with your own bindings, is not something a card in a TUI wins at.
func (r *review) handOff(tool externalTool) tea.Cmd {
	wt := r.bundle.Ticket.WorktreePath
	if wt == "" {
		r.notice = "this ticket has no worktree to open"
		return nil
	}
	// The worktree is on the machine running the daemon, which is not necessarily this one.
	// Checking beats launching an editor on a directory that is not there.
	if _, err := os.Stat(wt); err != nil {
		r.notice = "the worktree is not on this machine: " + wt
		return nil
	}

	var files []string
	for _, f := range r.bundle.Diff.Files {
		if f.Path != "" {
			files = append(files, f.Path)
		}
	}

	cmd, err := reviewCommand(tool, wt, files)
	if err != nil {
		r.notice = err.Error()
		return nil
	}

	r.notice = ""
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return externalDoneMsg{tool: tool, err: err}
	})
}

// reviewCommand builds the command for one hand-off.
func reviewCommand(tool externalTool, worktree string, files []string) (*host.Cmd, error) {
	switch tool {
	case externalShell:
		return host.LocalCommand(host.Shell(), nil, worktree), nil

	case externalDifftool:
		// Against the commit's parent, which is the diff the card is showing.
		return host.LocalCommand("git", []string{"difftool", "--no-prompt", "HEAD~1"}, worktree), nil

	default:
		name, args, err := host.Editor()
		if err != nil {
			return nil, err
		}
		// The changed files, not the whole tree: the question on this screen is what this
		// ticket did, and an editor opened on a repository answers a different one.
		args = append(args, files...)
		if len(files) == 0 {
			args = append(args, ".")
		}
		return host.LocalCommand(name, args, worktree), nil
	}
}
