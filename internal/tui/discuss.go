package tui

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// discussMode is what the keyboard is doing inside the discussion.
type discussMode int

const (
	discussReading discussMode = iota
	discussComposing
	discussEditing
	discussConfirming
)

// The three editable parts of a proposed instruction, in the order they are read.
const (
	fieldCorrection = iota
	fieldPreserve
	fieldVerify
	fieldCount
)

var fieldNames = [fieldCount]string{
	"Correction — what should change",
	"Preserve — behaviour that must survive it (one per line)",
	"Verify — how to check both (one per line)",
}

// discussion is the Request-changes conversation: a transcript, an editable instruction, and one
// deliberate send.
//
// It is a mode over the Review screen rather than a screen of its own, for the same reason the
// sweep is: it is a thing you do to the ticket in front of you, and a second screen would mean a
// second way to get to it and a second place for the two to drift apart.
//
// The screen holds no rules. Whether a message queues work, whether an instruction is complete,
// and whether the human confirmed are all the daemon's answers — this asks and renders.
type discussion struct {
	ticketID string
	title    string
	view     api.DiscussionView
	loaded   bool
	busy     bool

	mode  discussMode
	input string
	// fields is the proposal being edited. Lists are newline-separated, one constraint per
	// line, which is the shape a human types them in and the shape they are read back in.
	fields [fieldCount]string
	field  int

	notice string
	scroll int
	follow bool
}

// Messages the discussion raises for itself.
type (
	discussionLoadedMsg struct {
		ticketID string
		view     api.DiscussionView
		err      error
	}
	discussionClosedMsg struct{ err error }
)

func newDiscussion() *discussion { return &discussion{follow: true} }

// open starts or resumes the discussion for a ticket.
//
// draft seeds the first message with the automated review's own words, exactly as the old
// feedback prompt did: it has just read the diff and said what is wrong with it, and making the
// human retype that is asking them to be a courier between two machines.
func (d *discussion) open(svc api.Service, ticketID, title, draft string) tea.Cmd {
	*d = discussion{
		ticketID: ticketID, title: title, follow: true,
		mode: discussComposing, input: draft,
	}
	return func() tea.Msg {
		view, err := svc.OpenDiscussion(context.Background(), ticketID)
		return discussionLoadedMsg{ticketID: ticketID, view: view, err: err}
	}
}

// Update handles the messages the discussion raises.
func (d *discussion) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case discussionLoadedMsg:
		if msg.ticketID != d.ticketID {
			return nil
		}
		d.busy = false
		if msg.err != nil {
			d.notice = msg.err.Error()
			return nil
		}
		d.view, d.loaded = msg.view, true
		d.loadFields(msg.view.Discussion.Proposal)
		d.follow = true
		return nil
	}
	return nil
}

// loadFields copies a proposal into the editable buffers.
func (d *discussion) loadFields(ci core.ChangeInstruction) {
	d.fields[fieldCorrection] = ci.Correction
	d.fields[fieldPreserve] = strings.Join(ci.Preserve, "\n")
	d.fields[fieldVerify] = strings.Join(ci.Verify, "\n")
}

// proposal reads the editable buffers back as an instruction.
func (d *discussion) proposal() core.ChangeInstruction {
	return core.ChangeInstruction{
		Correction: d.fields[fieldCorrection],
		Preserve:   lines(d.fields[fieldPreserve]),
		Verify:     lines(d.fields[fieldVerify]),
	}.Normalized()
}

func lines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

func (d *discussion) handleKey(msg tea.KeyMsg, ctx ViewContext) tea.Cmd {
	key := msg.String()

	switch d.mode {
	case discussComposing:
		switch {
		case key == "esc":
			d.mode, d.input = discussReading, ""
		case key == "ctrl+u":
			d.input = ""
		case key == "enter":
			return d.send(ctx)
		case key == "backspace":
			d.input = chop(d.input)
		case len(msg.Runes) > 0:
			d.input += string(msg.Runes)
			d.notice = ""
		}
		return nil

	case discussEditing:
		switch {
		case key == "esc":
			// The saved draft wins over an abandoned edit, so backing out cannot half-apply one.
			d.loadFields(d.view.Discussion.Proposal)
			d.mode, d.notice = discussReading, "edit discarded"
		case key == "tab":
			d.field = (d.field + 1) % fieldCount
		case key == "shift+tab":
			d.field = (d.field - 1 + fieldCount) % fieldCount
		case key == "ctrl+s":
			return d.saveProposal(ctx)
		case key == "enter":
			d.fields[d.field] += "\n"
		case key == "backspace":
			d.fields[d.field] = chop(d.fields[d.field])
		case len(msg.Runes) > 0:
			d.fields[d.field] += string(msg.Runes)
			d.notice = ""
		}
		return nil

	case discussConfirming:
		switch key {
		case "y", "Y":
			return d.confirmSend(ctx)
		default:
			d.mode, d.notice = discussReading, "not sent"
		}
		return nil
	}

	switch key {
	case "enter", "i":
		d.mode, d.notice = discussComposing, ""
	case "p":
		d.mode, d.field, d.notice = discussEditing, fieldCorrection, ""
	case "S":
		if strings.TrimSpace(d.fields[fieldCorrection]) == "" {
			d.notice = "the instruction says nothing yet — p to write it"
			return nil
		}
		d.mode, d.notice = discussConfirming, ""
	case "esc", "x":
		return d.cancel(ctx)
	case "up", "k":
		d.follow = false
		if d.scroll > 0 {
			d.scroll--
		}
	case "down", "j":
		d.follow, d.scroll = false, d.scroll+1
	case "pgup":
		d.follow = false
		d.scroll = max(0, d.scroll-10)
	case "pgdown":
		d.follow, d.scroll = false, d.scroll+10
	case "g":
		d.follow, d.scroll = false, 0
	case "G":
		d.follow = true
	}
	return nil
}

// send asks one turn of the conversation.
//
// It sends a message and nothing else: no ticket moves, no run is queued. The current draft
// travels with it so the agent reacts to what the human has in front of them rather than to its
// own last suggestion.
func (d *discussion) send(ctx ViewContext) tea.Cmd {
	message := strings.TrimSpace(d.input)
	if message == "" {
		d.notice = "say something, or esc to go back"
		return nil
	}
	draft := d.proposal()
	id, svc := d.ticketID, ctx.Svc
	d.mode, d.input, d.busy, d.notice = discussReading, "", true, ""
	d.follow = true

	// Shown immediately, so the transcript does not sit empty while an agent thinks. The
	// daemon's copy is what survives; this is the same message.
	d.view.Discussion.Say(core.RoleHuman, message, time.Now())

	return func() tea.Msg {
		view, err := svc.Discuss(context.Background(), api.DiscussReq{
			TicketID: id, Message: message, Proposal: &draft,
		})
		if err != nil {
			// Reload rather than guess: the daemon keeps the failed turn in the transcript, and
			// the screen should show what was actually recorded.
			view, _ = svc.OpenDiscussion(context.Background(), id)
			return discussionLoadedMsg{ticketID: id, view: view, err: err}
		}
		return discussionLoadedMsg{ticketID: id, view: view}
	}
}

// saveProposal persists the human's edit.
func (d *discussion) saveProposal(ctx ViewContext) tea.Cmd {
	draft := d.proposal()
	id, svc := d.ticketID, ctx.Svc
	d.mode, d.busy, d.notice = discussReading, true, ""
	return func() tea.Msg {
		view, err := svc.SaveProposal(context.Background(), api.ProposalReq{
			TicketID: id, Proposal: draft,
		})
		return discussionLoadedMsg{ticketID: id, view: view, err: err}
	}
}

// confirmSend is the gate. It is the only thing on this screen that moves the ticket.
func (d *discussion) confirmSend(ctx ViewContext) tea.Cmd {
	draft := d.proposal()
	id, svc := d.ticketID, ctx.Svc
	d.mode, d.busy, d.notice = discussReading, true, ""
	return func() tea.Msg {
		err := svc.SendChanges(context.Background(), api.SendChangesReq{
			TicketID: id, Proposal: draft, Confirm: true,
		})
		return reviewActedMsg{verb: "sent back for changes", err: err}
	}
}

// cancel abandons the conversation, leaving the ticket exactly where it was.
func (d *discussion) cancel(ctx ViewContext) tea.Cmd {
	id, svc := d.ticketID, ctx.Svc
	return func() tea.Msg {
		return discussionClosedMsg{err: svc.CancelDiscussion(context.Background(), id)}
	}
}

func chop(s string) string {
	if s == "" {
		return s
	}
	_, size := utf8.DecodeLastRuneInString(s)
	return s[:len(s)-size]
}

// ---- rendering -----------------------------------------------------------

func (d *discussion) View(ctx ViewContext) string {
	th := ctx.Theme
	width := max(20, ctx.Width-4)

	head := th.Header.Render("Request changes") + th.Muted.Render("  ·  ") +
		th.Text.Render(shortID(d.ticketID))
	if d.title != "" {
		head += th.Muted.Render("  " + trunc(d.title, max(10, ctx.Width-40)))
	}
	if agent := d.view.Discussion.Agent; agent != "" {
		head += th.Muted.Render("  " + agent)
	}
	lines := []string{
		head,
		th.Muted.Render("  Nothing here starts work. The instruction goes to the agent only when you send it."),
	}

	// What earlier rounds were accepted on. This is on screen rather than only in the prompt
	// because the human writing the second correction is the one who can spot that it would
	// undo the first.
	if constraints := core.PreservationConstraints(d.view.Agreed); len(constraints) > 0 {
		lines = append(lines, "", th.Header.Render("  Already agreed on this ticket"))
		for _, c := range constraints {
			for i, ln := range wrapText(c, width-6) {
				prefix := "    · "
				if i > 0 {
					prefix = "      "
				}
				lines = append(lines, th.Warning.Render(prefix+ln))
			}
		}
	}

	if !d.loaded && d.mode != discussComposing {
		lines = append(lines, "", th.Muted.Render("  opening the discussion…"))
	}

	for _, m := range d.view.Discussion.Messages {
		who, style := "agent", th.Text
		switch m.Role {
		case core.RoleHuman:
			who, style = "you", th.Accent
		case core.RoleFailure:
			who, style = "failed", th.Danger
		}
		lines = append(lines, "", style.Render("  "+who)+th.Muted.Render(":"))
		for _, ln := range wrapText(m.Text, width-4) {
			lines = append(lines, "    "+style.Render(ln))
		}
	}

	if d.busy {
		lines = append(lines, "", th.Muted.Render("  thinking about the correction…"))
	}

	lines = append(lines, d.proposalLines(th, width)...)

	switch d.mode {
	case discussComposing:
		lines = append(lines, "")
		lines = append(lines, inputLines("Your message", d.input, ctx.Width, max(2, ctx.Height/3), th)...)
	case discussEditing:
		lines = append(lines, "")
		lines = append(lines, inputLines(fieldNames[d.field], d.fields[d.field],
			ctx.Width, max(2, ctx.Height/3), th)...)
	case discussConfirming:
		lines = append(lines, "",
			th.Warning.Render("  Send this for implementation? The ticket goes back to the agent."))
	}

	return d.scrolled(lines, ctx, th)
}

// proposalLines renders the instruction currently on the table.
func (d *discussion) proposalLines(th Theme, width int) []string {
	ci := d.proposal()
	if ci.Empty() {
		return []string{"", th.Muted.Render("  No instruction yet — reply to shape one, or p to write it yourself.")}
	}

	out := []string{"", th.Header.Render("  Proposed instruction")}
	label := func(i int, s string) string {
		if d.mode == discussEditing && d.field == i {
			return th.Accent.Render(s)
		}
		return th.Muted.Render(s)
	}
	if ci.Correction != "" {
		out = append(out, label(fieldCorrection, "    Correction"))
		for _, ln := range wrapText(ci.Correction, width-8) {
			out = append(out, th.Text.Render("      "+ln))
		}
	}
	if len(ci.Preserve) > 0 {
		out = append(out, label(fieldPreserve, "    Preserve, unchanged"))
		for _, p := range ci.Preserve {
			for i, ln := range wrapText(p, width-10) {
				prefix := "      - "
				if i > 0 {
					prefix = "        "
				}
				out = append(out, th.Text.Render(prefix+ln))
			}
		}
	} else {
		out = append(out, th.Warning.Render(
			"    Nothing listed to preserve — say what this correction must not cost."))
	}
	if len(ci.Verify) > 0 {
		out = append(out, label(fieldVerify, "    Verify both"))
		for _, v := range ci.Verify {
			for i, ln := range wrapText(v, width-10) {
				prefix := "      - "
				if i > 0 {
					prefix = "        "
				}
				out = append(out, th.Muted.Render(prefix+ln))
			}
		}
	}
	return out
}

func (d *discussion) scrolled(lines []string, ctx ViewContext, th Theme) string {
	footer := d.footer(ctx)

	body := ctx.Height - 2 - strings.Count(footer, "\n")
	if body < 1 {
		return trunc(footer, ctx.Width)
	}
	if len(lines) <= body {
		return strings.Join(append(lines, "", footer), "\n")
	}

	view := body - 1
	maxOffset := len(lines) - view
	if d.follow || d.mode == discussComposing || d.mode == discussEditing {
		d.scroll = maxOffset
	}
	d.scroll = clamp(d.scroll, 0, maxOffset)

	out := append([]string{}, lines[d.scroll:d.scroll+view]...)
	above, below := d.scroll, maxOffset-d.scroll
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

func (d *discussion) footer(ctx ViewContext) string {
	th := ctx.Theme
	var actions string
	switch d.mode {
	case discussComposing:
		actions = th.Muted.Render("  enter to send the message · ctrl+u clear · esc back")
	case discussEditing:
		actions = th.Muted.Render("  tab next field · enter newline · ctrl+s save · esc discard")
	case discussConfirming:
		actions = th.Muted.Render("  y send for implementation · any other key to go back")
	default:
		actions = th.Muted.Render(
			"  enter reply · p edit the instruction · S send for implementation · esc cancel · j/k scroll")
	}
	return actionFooter(d.notice, actions, ctx.Width, th)
}
