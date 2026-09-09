package tui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// planTurnMsg is one answer from the planner.
type planTurnMsg struct {
	reply api.PlanReply
	err   error
}

// planApprovedMsg reports what approving the plan created.
type planApprovedMsg struct {
	tickets []core.Ticket
	err     error
}

// planLog* carry the live tail of the planner's activity.
type (
	planLogOpenedMsg struct {
		runID string
		ch    <-chan api.LogLine
		stop  func()
	}
	planLogLineMsg   struct{ line api.LogLine }
	planLogClosedMsg struct{}
)

// planEntry is one turn of the conversation as the screen shows it.
type planEntry struct {
	// mine reports whether the human said it.
	mine bool
	// failed marks a turn that produced no answer, so a question is never left sitting in the
	// transcript looking as though it were ignored.
	failed bool
	text   string
}

// plan is the Plan screen: one conversation that reads the project's own documents, proposes
// work, is grilled until it is right, and produces tickets the human approves.
//
// It is PRODUCT.md §6.2 and §6.3 as a single view rather than two features. Assisted and planned
// creation differ only in how much conversation happens before a ticket exists, and that is a
// depth the conversation reaches on its own — not a mode anyone should have to pick up front.
type plan struct {
	entries []planEntry
	// pinnedID and pinnedName are the project this conversation is about, fixed when it
	// starts.
	//
	// The frame's p cycles a global filter, and a conversation holds a provider session
	// belonging to one repository. Re-reading the filter each turn would continue one
	// project's discussion under another's name, and approve its tickets into the wrong
	// backlog.
	pinnedID   string
	pinnedName string
	// session resumes the conversation across turns. Empty until the first answer.
	session string
	// agent is the provider and model doing the planning, reported when the conversation
	// starts. Kept so the screen can keep naming it on later turns, which do not re-resolve.
	agent string
	// tickets is the current proposal, replaced wholesale each turn.
	tickets []core.PlannedTicket
	// route is what the approved tickets will ask for. A route, never a model.
	route core.Route

	input   string
	editing bool
	busy    bool
	// activity is the planner's most recent step, shown while busy. A minute of silence is
	// indistinguishable from a hang, so the pause has to narrate itself.
	activity string
	stopLog  func()
	logCh    <-chan api.LogLine
	notice   string
	// cursor selects a proposed ticket, for reading its body.
	cursor int
	expand bool
	// scroll is the first body line drawn, and follow sticks it to the bottom.
	//
	// A conversation grows downwards and the newest answer is the one worth reading, so this
	// screen follows the end by default — unlike the list screens, which scroll to keep a
	// selected row in view.
	scroll int
	follow bool
}

func newPlan() *plan { return &plan{route: core.RouteImplementation, follow: true} }

// CapturesKeys takes the keyboard while a question is being typed, so a "q" in it does not quit.
func (p *plan) CapturesKeys() bool { return p.editing }

func (p *plan) Update(msg tea.Msg, ctx ViewContext) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case enteredMsg:
		return p, nil

	case planLogOpenedMsg:
		if !p.busy {
			msg.stop() // the turn finished before the tail opened
			return p, nil
		}
		p.stopLog, p.logCh = msg.stop, msg.ch
		return p, waitPlanLogLine(msg.ch)

	case planLogLineMsg:
		if text := strings.TrimSpace(msg.line.Text); text != "" {
			p.activity = text
		}
		if p.logCh == nil {
			return p, nil
		}
		return p, waitPlanLogLine(p.logCh)

	case planLogClosedMsg:
		p.stopLog, p.logCh, p.activity = nil, nil, ""
		return p, nil

	case planTurnMsg:
		p.busy = false
		p.endTail()
		if msg.err != nil {
			// The question stays in the transcript with the failure attached to it, rather
			// than sitting there unanswered as though it had been ignored.
			p.entries = append(p.entries, planEntry{failed: true, text: msg.err.Error()})
			p.follow = true
			return p, nil
		}
		if msg.reply.Session != "" {
			p.session = msg.reply.Session
		}
		if msg.reply.Agent != "" {
			p.agent = msg.reply.Agent
		}
		if reply := strings.TrimSpace(msg.reply.Reply); reply != "" {
			p.entries = append(p.entries, planEntry{text: reply})
		}
		p.follow = true
		if len(msg.reply.Tickets) > 0 {
			p.tickets = msg.reply.Tickets
			p.cursor = 0
			// The planner proposes a route per ticket; the human's choice on this screen is
			// what actually gets asked for, so it seeds from the first proposal and is then
			// theirs to change.
			p.route = msg.reply.Tickets[0].Route
		}
		p.notice = ""
		return p, nil

	case planApprovedMsg:
		p.busy = false
		p.endTail()
		if msg.err != nil {
			p.notice = msg.err.Error()
			return p, nil
		}
		p.notice = fmt.Sprintf("approved — %s", ticketWord(len(msg.tickets)))
		p.tickets = nil
		return p, nil

	case tea.KeyMsg:
		return p.handleKey(msg, ctx)
	}
	return p, nil
}

func (p *plan) handleKey(msg tea.KeyMsg, ctx ViewContext) (Screen, tea.Cmd) {
	key := msg.String()

	if p.editing {
		switch {
		case key == "esc":
			p.editing, p.input = false, ""
		case key == "enter":
			return p, p.ask(ctx, p.input, "")
		case key == "backspace":
			if r := []rune(p.input); len(r) > 0 {
				p.input = string(r[:len(r)-1])
			}
		case len(msg.Runes) > 0:
			p.input += string(msg.Runes)
		}
		return p, nil
	}

	// Same as the queue: a notice occupies the footer, and the footer is where the keys live.
	p.notice = ""

	switch key {
	case "i", "enter":
		// enter opens the input rather than expanding a ticket, because typing is what this
		// screen is for and a conversation you cannot continue is a transcript.
		p.editing, p.notice = true, ""
		return p, nil

	case "n":
		// The question you arrive with when you have nothing in mind.
		return p, p.ask(ctx, "", "")

	case "c":
		// The other direction of grilling. Replying pushes back in your own words; this asks
		// to be interrogated, which is how a proposal turns into a ticket worth running.
		if len(p.entries) == 0 {
			p.notice = "nothing to grill yet"
			return p, nil
		}
		return p, p.ask(ctx, grillAsk, "grill me")

	case "r":
		p.route = nextRoute(ctx, p.route)
		return p, nil

	case "a":
		return p, p.approve(ctx, false)

	case "A":
		return p, p.approve(ctx, true)

	case "x":
		p.endTail()
		*p = plan{route: p.route}
		return p, nil

	case "up", "k":
		p.follow = false
		if p.scroll > 0 {
			p.scroll--
		}
	case "down", "j":
		p.follow = false
		p.scroll++
	case "pgup":
		p.follow = false
		p.scroll -= 10
		if p.scroll < 0 {
			p.scroll = 0
		}
	case "pgdown":
		p.follow = false
		p.scroll += 10
	case "g":
		p.follow, p.scroll = false, 0
	case "G":
		p.follow = true
	case "tab":
		// Cycles the proposal rather than toggling one, so the arrows stay free for the
		// transcript — which is the part long enough to need them.
		switch {
		case len(p.tickets) == 0:
		case !p.expand:
			p.expand, p.cursor = true, 0
		case p.cursor < len(p.tickets)-1:
			p.cursor++
		default:
			p.expand, p.cursor = false, 0
		}
	}
	return p, nil
}

// ask sends one turn to the planner.
func (p *plan) ask(ctx ViewContext, message, display string) tea.Cmd {
	if p.busy {
		return nil
	}
	project := p.pinnedID
	if project == "" {
		id, name := planProject(ctx)
		if id == "" {
			p.notice = planNoProject(ctx)
			p.editing = false
			return nil
		}
		project, p.pinnedID, p.pinnedName = id, id, name
	}

	message = strings.TrimSpace(message)
	// What is shown is not always what is sent: a canned instruction reads as the action it
	// was, not as a paragraph the human did not write.
	if label := strings.TrimSpace(display); label != "" {
		p.entries = append(p.entries, planEntry{mine: true, text: label})
	} else if message != "" {
		p.entries = append(p.entries, planEntry{mine: true, text: message})
	} else if len(p.entries) > 0 {
		p.entries = append(p.entries, planEntry{mine: true, text: "What should we work on next?"})
	}

	p.editing, p.input, p.busy, p.notice = false, "", true, ""
	p.activity, p.follow = "starting", true

	// A correlation id chosen here, so the tail can be opened at the same moment the turn is
	// sent rather than after it returns — which would be after the pause it exists to explain.
	runID := planRunID()
	svc, session := ctx.Svc, p.session

	turn := func() tea.Msg {
		reply, err := svc.Plan(context.Background(), api.PlanReq{
			ProjectID: project, Message: message, Session: session, RunID: runID,
		})
		return planTurnMsg{reply: reply, err: err}
	}
	return tea.Batch(turn, openPlanLog(svc, runID))
}

// grillAsk turns the conversation around: the planner questions the human.
//
// The opening question must be answered rather than deflected, which is why the prompt forbids
// leading with questions. This is the deliberate exception — asked for, at the point where the
// answers actually change the plan.
const grillAsk = "Now interrogate me about this plan before I approve it. Ask the questions " +
	"whose answers would change it: what done looks like, what you had to assume, what you " +
	"would need to know to build it, and anything the project's documents leave ambiguous. " +
	"Ask the most important ones first, a few at a time, and wait for my answers. Keep the " +
	"current plan unchanged until I have answered."

// planRunID mints a correlation id for one turn.
func planRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// The id only has to be unique enough to name a log; the clock will do.
		return fmt.Sprintf("plan-%d", time.Now().UnixNano())
	}
	return "plan-" + hex.EncodeToString(b[:])
}

// openPlanLog starts tailing a turn's progress.
//
// A failure to open is not reported: the tail is a convenience, and losing it must not turn a
// working planning turn into an error.
func openPlanLog(svc api.Service, runID string) tea.Cmd {
	return func() tea.Msg {
		ch, stop, err := svc.StreamLogs(context.Background(), runID)
		if err != nil {
			return planLogClosedMsg{}
		}
		return planLogOpenedMsg{runID: runID, ch: ch, stop: stop}
	}
}

// waitPlanLogLine reads one line and re-arms, which is what makes the tail push-driven. There is
// no ticker here, as there is none anywhere in this package.
func waitPlanLogLine(ch <-chan api.LogLine) tea.Cmd {
	return func() tea.Msg {
		l, ok := <-ch
		if !ok {
			return planLogClosedMsg{}
		}
		return planLogLineMsg{line: l}
	}
}

// endTail stops the live tail, if one is running.
func (p *plan) endTail() {
	if p.stopLog != nil {
		p.stopLog()
		p.stopLog = nil
	}
	p.logCh, p.activity = nil, ""
}

// approve writes the proposal into the backlog.
func (p *plan) approve(ctx ViewContext, ready bool) tea.Cmd {
	if len(p.tickets) == 0 {
		p.notice = "nothing proposed yet"
		return nil
	}
	project := p.pinnedID
	if project == "" {
		p.notice = planNoProject(ctx)
		return nil
	}

	// The human's route choice overrides what the planner suggested, on every ticket: the
	// screen shows one route, so silently keeping a different one per ticket would make the
	// display a lie.
	tickets := make([]core.PlannedTicket, len(p.tickets))
	copy(tickets, p.tickets)
	for i := range tickets {
		tickets[i].Route = p.route
	}

	p.busy, p.notice = true, ""
	svc := ctx.Svc
	return func() tea.Msg {
		created, err := svc.ApprovePlan(context.Background(), api.ApprovePlanReq{
			ProjectID: project, Tickets: tickets, Ready: ready,
		})
		return planApprovedMsg{tickets: created, err: err}
	}
}

// planProject is the project the conversation is about: the one the frame is filtered to, or
// the only one registered.
//
// The name is returned alongside the id because the screen has to say which repository it is
// planning for. A planner that thinks for a minute without naming its subject leaves the human
// watching a spinner they cannot check.
func planProject(ctx ViewContext) (id, name string) {
	if ctx.Project != "" {
		for _, p := range ctx.Status.Projects {
			if projectName(p.Project) == ctx.Project {
				return p.Project.ID, projectName(p.Project)
			}
		}
	}
	if len(ctx.Status.Projects) == 1 {
		p := ctx.Status.Projects[0].Project
		return p.ID, projectName(p)
	}
	return "", ""
}

// planNoProject explains which of the two reasons there is no project to plan for.
func planNoProject(ctx ViewContext) string {
	if len(ctx.Status.Projects) == 0 {
		return "no project registered yet — add one with P"
	}
	return "several projects registered — pick one with p before planning"
}

// nextRoute cycles the configured buckets.
//
// Buckets, not models: a ticket asks for a route so that a quota failure falls back without
// anyone rewriting it. Naming a model here would be the one thing routing exists to prevent.
func nextRoute(ctx ViewContext, current core.Route) core.Route {
	routes := ctx.Status.Buckets
	for i, r := range routes {
		if r == current {
			return routes[(i+1)%len(routes)]
		}
	}
	if len(routes) > 0 {
		return routes[0]
	}
	return current
}

func ticketWord(n int) string {
	if n == 1 {
		return "1 ticket"
	}
	return fmt.Sprintf("%d tickets", n)
}

// ---- rendering -----------------------------------------------------------

func (p *plan) View(ctx ViewContext) string {
	th := ctx.Theme
	if ctx.Width <= 0 || ctx.Height <= 0 {
		return ""
	}

	_, name := planProject(ctx)
	if p.pinnedName != "" {
		name = p.pinnedName
	}
	head := th.Header.Render("Plan")
	if name != "" {
		head += th.Muted.Render(" · ") + th.Accent.Render(name)
	}
	// Which agent is doing the planning. It comes from the planning bucket in the config, and
	// is not the route the proposed tickets will ask for — showing only the latter is what
	// makes the two easy to confuse.
	if p.agent != "" {
		head += th.Muted.Render("  " + p.agent)
	}
	lines := []string{head}

	// The frame filter can move after a conversation starts. Say so, rather than showing one
	// project in the header and another in the status bar with no explanation.
	if cur, curName := planProject(ctx); p.pinnedID != "" && cur != "" && cur != p.pinnedID {
		lines = append(lines, th.Warning.Render(
			"  this conversation is about "+p.pinnedName+" — x starts over on "+curName))
	}

	if len(p.entries) == 0 && !p.busy {
		if name == "" {
			lines = append(lines, "", th.Warning.Render("  "+planNoProject(ctx)))
			return window(append(lines, "", p.footer(ctx)), -1, ctx.Height, th)
		}
		lines = append(lines,
			"",
			th.Muted.Render("  A conversation about what to build in ")+th.Accent.Render(name)+
				th.Muted.Render(", against its own docs."),
			"",
			th.Key.Render("  n")+th.Muted.Render("  ask what I should work on next"),
			th.Key.Render("  i")+th.Muted.Render("  describe something in your own words"),
		)
		// The prompt belongs on this branch too. Without it, i on a fresh screen started an
		// edit whose text was drawn nowhere: the footer said "enter to send" while you typed
		// into what looked like a dead screen.
		if p.editing {
			lines = append(lines, "", p.promptLine(th))
		}
		return p.scrolled(lines, ctx, th)
	}

	width := max(20, ctx.Width-4)
	for _, e := range p.entries {
		who, style := "you", th.Accent
		switch {
		case e.failed:
			who, style = "plan", th.Danger
		case !e.mine:
			who, style = "plan", th.Text
		}
		lines = append(lines, "", style.Render("  "+who)+th.Muted.Render(":"))
		for _, ln := range wrapText(e.text, width-4) {
			lines = append(lines, "    "+style.Render(ln))
		}
	}

	if p.busy {
		thinking := "  thinking about " + name + "…"
		if name == "" {
			thinking = "  thinking…"
		}
		lines = append(lines, "", th.Muted.Render(thinking))
		// What it is doing right now, so a long pause is legible rather than suspicious.
		if p.activity != "" {
			lines = append(lines, th.Muted.Render("    "+trunc(p.activity, max(0, ctx.Width-6))))
		}
	}

	if len(p.tickets) > 0 {
		lines = append(lines, "", th.Header.Render(fmt.Sprintf("  Proposed (%s)", ticketWord(len(p.tickets)))))
		for i, t := range p.tickets {
			marker := "  "
			style := th.Text
			if i == p.cursor {
				marker, style = "> ", th.Accent
			}
			dep := ""
			if len(t.DependsOn) > 0 {
				dep = th.Muted.Render(fmt.Sprintf("  after %s", positions(t.DependsOn)))
			}
			lines = append(lines, "  "+style.Render(marker+t.Title)+dep)
			if p.expand && i == p.cursor && t.Body != "" {
				for _, ln := range wrapText(t.Body, width-8) {
					lines = append(lines, "      "+th.Muted.Render(ln))
				}
			}
		}
	}

	switch {
	case p.editing:
		lines = append(lines, "", p.promptLine(th))
	case len(p.entries) > 0 && !p.busy:
		// A visible place to answer. Without it the screen reads as a menu of commands rather
		// than a conversation waiting on you.
		lines = append(lines, "", th.Muted.Render("  > enter to reply"))
	}

	return p.scrolled(lines, ctx, th)
}

// scrolled draws the body at the current offset, with the footer pinned to the bottom.
func (p *plan) scrolled(lines []string, ctx ViewContext, th Theme) string {
	footer := p.footer(ctx)

	// The body gets everything except a blank line and the footer.
	body := ctx.Height - 2
	if body < 1 {
		return trunc(footer, ctx.Width)
	}
	if len(lines) <= body {
		return strings.Join(append(lines, "", footer), "\n")
	}

	// One line goes to the indicator, so hidden content is never silently cut off.
	view := body - 1
	maxOffset := len(lines) - view
	if p.follow {
		p.scroll = maxOffset
	}
	p.scroll = clamp(p.scroll, 0, maxOffset)

	out := append([]string{}, lines[p.scroll:p.scroll+view]...)
	above, below := p.scroll, maxOffset-p.scroll
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

// promptLine is the line you type on.
//
// It is a method rather than three inline calls because it has to appear on every branch of the
// view: the one that returns early for an empty conversation is exactly where a first message is
// typed.
func (p *plan) promptLine(th Theme) string {
	return th.Muted.Render("  > ") + th.Accent.Render(p.input) + th.Muted.Render("▏")
}

func (p *plan) footer(ctx ViewContext) string {
	th := ctx.Theme
	if p.notice != "" {
		return th.Warning.Render("  " + p.notice)
	}
	if p.editing {
		return th.Muted.Render("  enter to send · esc to cancel")
	}

	// Ordered by what comes next in the conversation, not by what the screen can do.
	//
	// Leading with "what next" after an answer invites pressing it again, which asks the same
	// question a second time and stacks a duplicate on the transcript. Once there is something
	// on the table, replying to it is the step.
	var parts []string
	switch {
	case len(p.entries) == 0:
		// The body already spells out n and i on an empty screen; repeating them here is
		// noise, so the footer carries only what the body does not.
		if len(ctx.Status.Projects) > 1 && p.pinnedID == "" {
			parts = append(parts, "p project")
		}
	case len(p.tickets) > 0:
		parts = []string{
			"enter reply", "c grill me",
			"a to backlog", "A to ready",
			"r route: " + string(p.route),
			"tab detail",
		}
	default:
		parts = []string{"enter reply", "c grill me", "n another option"}
	}
	if len(p.entries) > 0 {
		parts = append(parts, "j/k scroll", "x start over")
	}
	return th.Muted.Render("  " + strings.Join(parts, " · "))
}

// positions renders dependency indexes the way the list is numbered on screen.
func positions(deps []int) string {
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		out = append(out, fmt.Sprint(d+1))
	}
	return "#" + strings.Join(out, ", #")
}

// wrapText breaks text to width, on word boundaries.
func wrapText(s string, width int) []string {
	if width < 8 {
		width = 8
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := words[0]
		for _, w := range words[1:] {
			if len(line)+1+len(w) > width {
				out = append(out, line)
				line = w
				continue
			}
			line += " " + w
		}
		out = append(out, line)
	}
	return out
}
