package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/provider"
)

// maxLogLines is how much scrollback the screen keeps in memory.
//
// The files hold everything; this is only what a human might scroll back through. Keeping an
// unbounded slice would let a chatty run grow the TUI's memory without limit.
const maxLogLines = 2000

// phaseLogSwitch marks a timeline line this screen wrote itself: the log moving to a newer
// attempt. It is never stored, so it lives here rather than among core's phases.
const phaseLogSwitch core.ProgressPhase = "log"

// running is the per-run detail: what an agent is doing, why it was given this work, and the
// one key that stops it.
type running struct {
	ticketID string
	runID    string
	loaded   bool
	err      error

	// last is the ticket's most recent row in the fleet snapshot. It is kept so the screen can
	// still name the ticket, and say where it went, after it has left the running set.
	last api.RunningTicket

	// runs is every attempt on this ticket, newest first, so a retry's reason is visible
	// rather than being a number with no story.
	runs    []core.Run
	explain api.Explanation
	// journal is the ticket's progress journal, oldest first. It is what the screen shows
	// before any agent exists and after the agent has gone, which is most of a ticket's life.
	journal []core.Progress
	// switches are the log-stream changes this screen made, narrated into the timeline.
	switches []core.Progress

	// A reload in flight absorbs further pushes into pending rather than stacking reads; the
	// reload that answers re-issues itself once, so the last push is never the one dropped.
	reloading bool
	pending   bool

	// The timeline's open/closed choice holds only for the state it was made in: a ticket
	// moving from Running into Validating is exactly when the timeline becomes the thing to read.
	timelineSet   bool
	timelineOpen  bool
	timelineState core.State

	lines  []api.LogLine
	follow bool
	// offset is how far scrolled back from the tail, in lines. Zero is the live tail.
	offset int
	// streamDone is set when the current run's output closed.
	streamDone bool

	confirmKill bool
	notice      string

	stopLogs func()
}

func newRunning() *running { return &running{follow: true} }

// CapturesKeys is true while the kill confirmation is open.
func (r *running) CapturesKeys() bool { return r.confirmKill }

type (
	runDetailMsg struct {
		ticketID string
		runs     []core.Run
		explain  api.Explanation
		journal  []core.Progress
		// journalOK is false when the journal could not be read, so a transient failure keeps
		// the timeline already on screen rather than blanking it.
		journalOK bool
	}
	runDetailErrMsg struct{ err error }
	logOpenedMsg    struct {
		runID string
		ch    <-chan api.LogLine
		stop  func()
	}
	// logLineMsg and logClosedMsg carry the run they came from, so a stream the screen has
	// moved off cannot write into, or end, the one it moved to.
	logLineMsg struct {
		runID string
		line  api.LogLine
	}
	logClosedMsg struct{ runID string }
	killedMsg    struct{ err error }
)

func loadRunDetail(svc api.Service, ticketID string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		runs, err := svc.ListRuns(ctx, ticketID)
		if err != nil {
			return runDetailErrMsg{err}
		}
		// The explanation and the journal are advisory: a ticket that cannot be explained or
		// narrated is still worth showing, so their failure is not the screen's failure.
		ex, _ := svc.ExplainTicket(ctx, ticketID)
		journal, jerr := svc.ListProgress(ctx, ticketID)
		return runDetailMsg{
			ticketID: ticketID, runs: runs, explain: ex, journal: journal, journalOK: jerr == nil,
		}
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
func waitLogLine(runID string, ch <-chan api.LogLine) tea.Cmd {
	return func() tea.Msg {
		l, ok := <-ch
		if !ok {
			return logClosedMsg{runID: runID}
		}
		return logLineMsg{runID: runID, line: l}
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
			r.remember(ctx)
			return r, nil // already showing it; do not restart the stream
		}
		r.reset()
		r.ticketID = id
		r.remember(ctx)
		return r, r.reload(ctx)

	case refreshedMsg:
		if r.ticketID == "" {
			// An empty screen picks up the first ticket to start rather than saying "Nothing
			// running" until someone leaves and comes back.
			id := firstRunningTicket(ctx)
			if id == "" {
				return r, nil
			}
			r.reset()
			r.ticketID = id
		}
		r.remember(ctx)
		return r, r.reload(ctx)

	case runDetailMsg:
		if msg.ticketID != "" && msg.ticketID != r.ticketID {
			return r, nil // a read for a ticket the screen has since moved off
		}
		r.reloading = false
		r.runs, r.explain, r.loaded, r.err = msg.runs, msg.explain, true, nil
		if msg.journalOK {
			r.journal = msg.journal
		}

		var cmds []tea.Cmd
		if r.pending {
			r.pending = false
			cmds = append(cmds, r.reload(ctx))
		}
		if len(msg.runs) > 0 && msg.runs[0].ID != r.runID {
			if r.runID != "" {
				r.switchStream(msg.runs[0].ID, len(msg.runs))
			}
			r.runID = msg.runs[0].ID
			cmds = append(cmds, openLogs(ctx.Svc, r.runID))
		}
		return r, tea.Batch(cmds...)

	case runDetailErrMsg:
		r.reloading = false
		r.err = msg.err
		return r, nil

	case logOpenedMsg:
		if msg.runID != r.runID {
			msg.stop() // a stream for a run we have navigated away from
			return r, nil
		}
		r.stopLogs = msg.stop
		r.streamDone = false
		return r, waitLogLine(msg.runID, msg.ch)

	case logLineMsg:
		if msg.runID != "" && msg.runID != r.runID {
			return r, nil
		}
		r.appendLine(msg.line)
		return r, nil

	case logClosedMsg:
		if msg.runID != "" && msg.runID != r.runID {
			return r, nil
		}
		// The agent finishing is not the ticket finishing: validation, the summary and the
		// review all follow. Re-reading is how the screen learns which of them is next.
		r.streamDone = true
		return r, r.reload(ctx)

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

// reload re-reads the runs and the journal, or notes that another read is wanted when one is
// already in flight.
func (r *running) reload(ctx ViewContext) tea.Cmd {
	if r.ticketID == "" {
		return nil
	}
	if r.reloading {
		r.pending = true
		return nil
	}
	r.reloading = true
	return loadRunDetail(ctx.Svc, r.ticketID)
}

// switchStream moves the log to a newer attempt and says so in the timeline. A self-correction
// attempt is a new run with its own output, and a tail still pinned to the failed one would show
// a finished agent while a live one works.
func (r *running) switchStream(runID string, attempt int) {
	if r.stopLogs != nil {
		r.stopLogs()
		r.stopLogs = nil
	}
	r.lines, r.offset, r.follow, r.streamDone = nil, 0, true, false
	r.switches = append(r.switches, core.Progress{
		TicketID: r.ticketID, RunID: runID, At: time.Now(), Phase: phaseLogSwitch,
		Detail: fmt.Sprintf("log now follows attempt %d (run %s)", attempt, shortID(runID)),
	})
}

// remember keeps the ticket's latest snapshot row while it has one.
func (r *running) remember(ctx ViewContext) {
	if rt, ok := runningEntry(ctx, r.ticketID); ok {
		r.last = rt
	}
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
	case "t":
		open := !r.timelineExpanded(ctx)
		r.timelineSet, r.timelineOpen, r.timelineState = true, open, r.state(ctx)
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

func runningEntry(ctx ViewContext, ticketID string) (api.RunningTicket, bool) {
	for _, rt := range ctx.Status.Running {
		if rt.Ticket.ID == ticketID {
			return rt, true
		}
	}
	return api.RunningTicket{}, false
}

func isRunningTicket(ctx ViewContext, ticketID string) bool {
	_, ok := runningEntry(ctx, ticketID)
	return ok
}

func firstRunningTicket(ctx ViewContext) string {
	if len(ctx.Status.Running) > 0 {
		return ctx.Status.Running[0].Ticket.ID
	}
	return ""
}

// state is where the ticket is now: the live snapshot while it is running, the last
// explanation once it has left.
func (r *running) state(ctx ViewContext) core.State {
	if rt, ok := runningEntry(ctx, r.ticketID); ok {
		return rt.Ticket.State
	}
	return r.explain.State
}

// timelineExpanded opens the timeline by default in the phases with no agent output to read,
// and once the ticket has left, when the timeline is the whole story.
func (r *running) timelineExpanded(ctx ViewContext) bool {
	st := r.state(ctx)
	if r.timelineSet && r.timelineState == st {
		return r.timelineOpen
	}
	if !isRunningTicket(ctx, r.ticketID) {
		return true
	}
	return st == core.StateAssigned || st == core.StateValidating
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
	foot := append([]string{""}, strings.Split(r.footer(th, ctx.Width), "\n")...)

	avail := ctx.Height - len(head) - len(foot)
	// The timeline may take half of what is left, so a long journal cannot starve the log.
	timeline := r.timelineLines(ctx, max(2, avail/2))
	head = append(head, timeline...)

	logHeight := avail - len(timeline)
	if logHeight < 1 {
		// Too short for both: say where the ticket is, which is what the header is for.
		return pinFooter(head, -1, ctx.Height, th, r.footer(th, ctx.Width))
	}

	out := append([]string{}, head...)
	out = append(out, r.logLines(ctx, logHeight)...)
	out = append(out, foot...)
	return window(out, -1, ctx.Height, th)
}

// attemptRe reads "attempt N of M" out of an agent-start entry, which is the one place the
// self-correction budget reaches a client.
var attemptRe = regexp.MustCompile(`attempt (\d+) of (\d+)`)

func (r *running) headerLines(ctx ViewContext) []string {
	th := ctx.Theme
	var current core.Run
	if len(r.runs) > 0 {
		current = r.runs[0]
	}
	rt, live := runningEntry(ctx, r.ticketID)
	if !live {
		rt = r.last
	}
	// The snapshot's run row is re-read on every push, so its counters are the freshest.
	counters := current
	if live && rt.Run.ID != "" {
		counters = rt.Run
	}

	meta := fmt.Sprintf("  %s · %s · %s/%s@%s",
		projectName(rt.Project), branchName(rt.Ticket.Branch),
		current.ProviderID, current.Model, current.HostID)

	stats := []string{}
	latest, hasLatest := r.latestEntry()
	if hasLatest {
		stats = append(stats, phaseLabel(latest.Phase))
	}
	stats = append(stats,
		age(r.elapsed(rt, live, current))+" elapsed",
		fmt.Sprintf("%d turns", counters.Turns),
		fmt.Sprintf("%d in / %d out", counters.TokensIn, counters.TokensOut),
		r.attempt(),
	)
	statLine := th.Muted.Render("  " + strings.Join(stats, " · "))
	if _, quiet := splitQuiet(rt.Activity); live && quiet != "" {
		statLine += th.Muted.Render(" · ") + th.Warning.Render("no output for "+quiet)
	}

	lines := []string{
		th.Header.Render(fmt.Sprintf("%s  %s", shortID(r.ticketID), rt.Ticket.Title)),
		th.Muted.Render(meta),
		statLine,
	}

	if handoff := r.handoff(ctx); handoff != "" {
		lines = append(lines, th.Accent.Render("  → "+trunc(handoff, max(0, ctx.Width-4))))
	} else if hasLatest {
		lines = append(lines, th.Text.Render("  now: "+trunc(latest.Detail, max(0, ctx.Width-7))))
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

// elapsed is how long the current attempt has taken. Before any run row exists it is measured
// from the fetch, which is when the ticket's work actually began.
func (r *running) elapsed(rt api.RunningTicket, live bool, current core.Run) time.Duration {
	switch {
	case live && rt.Run.ID != "":
		return rt.Elapsed
	case !live && current.EndedAt != nil:
		return current.EndedAt.Sub(current.StartedAt)
	}
	for i := len(r.journal) - 1; i >= 0; i-- {
		if r.journal[i].Phase == core.PhaseFetch {
			return time.Since(r.journal[i].At)
		}
	}
	return rt.Elapsed
}

// attempt renders "attempt N of M" when the journal has said what the budget is, and the
// attempt count alone when it has not.
func (r *running) attempt() string {
	for i := len(r.journal) - 1; i >= 0; i-- {
		if r.journal[i].Phase != core.PhaseAgentStart {
			continue
		}
		if m := attemptRe.FindStringSubmatch(r.journal[i].Detail); m != nil {
			return fmt.Sprintf("attempt %s of %s", m[1], m[2])
		}
	}
	return fmt.Sprintf("attempt %d", max(1, len(r.runs)))
}

func (r *running) latestEntry() (core.Progress, bool) {
	if len(r.journal) == 0 {
		return core.Progress{}, false
	}
	return r.journal[len(r.journal)-1], true
}

// handoff is the one line saying where a ticket went once it left the running set, and the key
// that follows it there. Empty while it is still running.
func (r *running) handoff(ctx ViewContext) string {
	if isRunningTicket(ctx, r.ticketID) {
		return ""
	}

	var reason core.AttentionReason
	for _, a := range ctx.Status.Attention {
		if a.Attention.TicketID == r.ticketID {
			reason = a.Attention.Reason
			break
		}
	}
	review := "press " + SectionReview.Key()
	needs := "press " + SectionNeedsYou.Key()

	switch st := r.explain.State; {
	case st == core.StateReview || reason == core.ReasonReviewPending:
		return "moved to Review · " + review
	case st == core.StateBlocked || reason == core.ReasonAgentQuestion || reason == core.ReasonPermissionReq:
		return "blocked on a question · " + needs
	case st == core.StateNeedsYou || reason != "":
		if reason == "" {
			reason = core.AttentionReason(r.parkedReason())
		}
		if reason == "" {
			return "parked in Needs You · " + needs
		}
		return fmt.Sprintf("parked in Needs You: %s · %s", reason, needs)
	case st == core.StateReady:
		return "back in Ready · press " + SectionReady.Key()
	case st == core.StateDone:
		return "landed"
	}
	// A state this screen has no words for: the journal's own last hand-off says it best.
	for i := len(r.journal) - 1; i >= 0; i-- {
		if r.journal[i].Phase == core.PhaseHandoff {
			return r.journal[i].Detail
		}
	}
	return "no longer running"
}

// parkedReason reads the reason out of the journal's "parked in Needs You: <reason>" entry, for
// the moment between the ticket parking and the snapshot carrying its attention row.
func (r *running) parkedReason() string {
	const prefix = "parked in Needs You: "
	for i := len(r.journal) - 1; i >= 0; i-- {
		if d := r.journal[i].Detail; r.journal[i].Phase == core.PhaseHandoff && strings.HasPrefix(d, prefix) {
			return strings.TrimPrefix(d, prefix)
		}
	}
	return ""
}

// timelineLines renders the journal, newest last, capped at limit lines.
func (r *running) timelineLines(ctx ViewContext, limit int) []string {
	th := ctx.Theme
	entries := r.timeline()
	if !r.timelineExpanded(ctx) {
		return []string{th.Muted.Render(fmt.Sprintf("▸ timeline · %d entries · t to expand", len(entries))), ""}
	}

	out := []string{th.Muted.Render("▾ timeline · t to collapse")}
	if len(entries) == 0 {
		return append(out, th.Muted.Render("  nothing recorded yet"), "")
	}

	rows := max(1, limit-2) // the title and the blank line after
	if len(entries) > rows {
		hidden := len(entries) - rows + 1
		out = append(out, th.Muted.Render(fmt.Sprintf("  … %d earlier", hidden)))
		entries = entries[hidden:]
	}
	for i, e := range entries {
		out = append(out, r.timelineLine(ctx, e, i == len(entries)-1))
	}
	return append(out, "")
}

// timeline merges the journal with the screen's own log-switch notes, oldest first.
func (r *running) timeline() []core.Progress {
	out := make([]core.Progress, 0, len(r.journal)+len(r.switches))
	out = append(out, r.journal...)
	out = append(out, r.switches...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

var exitRe = regexp.MustCompile(`\(exit (-?\d+)\)`)

func (r *running) timelineLine(ctx ViewContext, e core.Progress, newest bool) string {
	th := ctx.Theme
	detail := e.Detail
	style := th.Text

	switch e.Phase {
	case core.PhaseValidationStep:
		if m := exitRe.FindStringSubmatch(detail); m != nil {
			style = th.Success
			if code, _ := strconv.Atoi(m[1]); code != 0 {
				style = th.Danger
			}
		} else if newest {
			style = th.Accent // the step in progress
		}
	case core.PhaseRetry:
		style = th.Warning
		if why := r.retryReason(e); why != "" {
			detail += " — " + why
		}
	case core.PhaseHandoff, phaseLogSwitch:
		style = th.Accent
	}

	prefix := fmt.Sprintf("  %s  %-8s ", e.At.Local().Format("15:04:05"), phaseTag(e.Phase))
	return th.Muted.Render(prefix) +
		style.Render(trunc(detail, max(0, ctx.Width-lipgloss.Width(prefix))))
}

// retryReason is why an attempt was retried: its classification when the agent failed, or the
// validation step that went red when the agent thought it had succeeded.
func (r *running) retryReason(e core.Progress) string {
	for _, run := range r.runs {
		if run.ID == e.RunID && run.EndedAt != nil && run.FailureClass != core.Success {
			if run.FailureNote == "" {
				return run.FailureClass.String()
			}
			return run.FailureClass.String() + ": " + run.FailureNote
		}
	}
	var failed string
	for _, j := range r.journal {
		if j.At.After(e.At) {
			break
		}
		if j.Phase != core.PhaseValidationStep || j.RunID != e.RunID {
			continue
		}
		if m := exitRe.FindStringSubmatch(j.Detail); m != nil && m[1] != "0" {
			failed = j.Detail
		}
	}
	return failed
}

// phaseTag is a phase's short name in the timeline's column.
func phaseTag(p core.ProgressPhase) string {
	switch p {
	case core.PhaseAgentStart, core.PhaseAgentExit:
		return "agent"
	case core.PhaseValidationStep:
		return "validate"
	default:
		return string(p)
	}
}

// phaseLabel is what the header says the ticket is doing, from its latest journal entry.
func phaseLabel(p core.ProgressPhase) string {
	switch p {
	case core.PhaseFetch:
		return "fetching"
	case core.PhaseWorktree:
		return "preparing the worktree"
	case core.PhasePrompt:
		return "building the prompt"
	case core.PhaseAgentStart:
		return "agent working"
	case core.PhaseAgentExit:
		return "agent finished"
	case core.PhaseValidationStep:
		return "validating"
	case core.PhaseRetry:
		return "retrying"
	case core.PhaseSummary:
		return "writing the summary"
	case core.PhaseReview:
		return "reviewing"
	case core.PhaseHandoff:
		return "handed off"
	default:
		return string(p)
	}
}

func (r *running) logLines(ctx ViewContext, height int) []string {
	th := ctx.Theme
	if len(r.lines) == 0 {
		return []string{th.Muted.Render("  " + r.emptyLog(ctx))}
	}

	end := len(r.lines) - r.offset
	if end < 1 {
		end = 1
	}
	// The end-of-output marker takes a row only when the tail is on screen.
	marker := r.streamDone && r.offset == 0
	rows := height
	if marker {
		rows = max(1, height-1)
	}
	start := end - rows
	if start < 0 {
		start = 0
	}

	out := make([]string, 0, height)
	for _, l := range r.lines[start:end] {
		text, style := logStyle(l, th)
		out = append(out, style.Render("  "+trunc(text, max(0, ctx.Width-2))))
	}
	if marker && len(out) < height {
		out = append(out, th.Muted.Render("  — agent output ended —"))
	}
	return out
}

// emptyLog explains an empty log by phase: before the agent starts and after it finishes there
// is no output to wait for, and saying "waiting" then is what made a fetch look like a hang.
func (r *running) emptyLog(ctx ViewContext) string {
	switch {
	case !isRunningTicket(ctx, r.ticketID):
		return "no agent output for this attempt"
	case r.state(ctx) == core.StateAssigned:
		return "no agent yet — the timeline shows what is happening"
	case r.state(ctx) != core.StateRunning:
		return "the agent has finished — the timeline shows what is happening"
	default:
		return "waiting for output…"
	}
}

// logStyle picks a line's text and style by its event kind.
func logStyle(l api.LogLine, th Theme) (string, lipgloss.Style) {
	kind, tool, text := l.Kind, l.Tool, l.Text
	if kind == "" && l.Stream == "event" {
		// A daemon that did not render the event sent it as it was stored. Reading the kind and
		// text back out is enough to keep JSON off the screen.
		kind, tool, text = decodeEvent(text)
	}

	switch kind {
	case provider.EventToolUse:
		if tool != "" && !strings.HasPrefix(text, tool) {
			text = strings.TrimSpace(tool + " " + text)
		}
		return text, th.Accent
	case provider.EventMessage:
		return text, th.Text
	case provider.EventError:
		return text, th.Danger
	case provider.EventRateLimit:
		return text, th.Warning
	case provider.EventThinking, provider.EventUsage, provider.EventStarted,
		provider.EventFinished, provider.EventToolResult:
		return text, th.Muted
	}
	if l.Stream == "event" {
		return text, th.Muted
	}
	return text, th.Text
}

// decodeEvent recovers the kind, tool and text from a stored event line, returning the line as
// it was when it is not one.
func decodeEvent(raw string) (provider.EventKind, string, string) {
	var ev struct {
		Kind provider.EventKind
		Tool string
		Text string
	}
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") || json.Unmarshal([]byte(raw), &ev) != nil || ev.Kind == "" {
		return "", "", raw
	}
	text := strings.Join(strings.Fields(ev.Text), " ")
	if text == "" {
		text = string(ev.Kind)
	}
	if ev.Kind == provider.EventThinking {
		text = "… thinking"
	}
	return ev.Kind, ev.Tool, text
}

func (r *running) footer(th Theme, widths ...int) (result string) {
	width := 100
	if len(widths) > 0 {
		width = widths[0]
	}
	defer func() { result = actionFooter(r.notice, result, width, th) }()
	if r.confirmKill {
		return th.Danger.Render("kill this run and everything it started? ") + th.Muted.Render("y / n")
	}
	mode := th.Muted.Render("paused")
	if r.follow {
		mode = th.Success.Render("following")
	}
	hints := make([]string, 0, len(runningBindings))
	for _, b := range runningBindings {
		hints = append(hints, b.Keys[0]+" "+b.Short)
	}
	return mode + th.Muted.Render("  ·  "+strings.Join(hints, " · "))
}

// runningBinding is one of the Running screen's own keys, with the word its footer uses and the
// sentence help uses.
type runningBinding struct {
	Binding
	Short string
}

// runningBindings feeds both the footer and the help overlay, so neither can list a key the
// other has forgotten.
var runningBindings = []runningBinding{
	{Binding{Keys: []string{"f"}, Help: "follow the log tail, or pause it"}, "follow"},
	{Binding{Keys: []string{"t"}, Help: "expand or collapse the progress timeline"}, "timeline"},
	{Binding{Keys: []string{"K"}, Help: "kill the run, after confirming"}, "kill"},
	{Binding{Keys: []string{"j/k"}, Help: "scroll the log"}, "scroll"},
	{Binding{Keys: []string{"G"}, Help: "back to the live tail"}, "tail"},
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
