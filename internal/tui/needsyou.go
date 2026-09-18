package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

// nyMode is what the keyboard is doing on the Needs You screen.
type nyMode int

const (
	nyBrowsing nyMode = iota
	nyFeedback
	nyConfirmReject
)

// needsYou is the single queue of everything awaiting human judgement.
//
// If something is not here, Gravy does not need you. That claim is only worth making if the
// screen is complete and every row is actionable, which is why reasons are a registry: the ones
// M1 adds are entries, not edits to a function that already works.
type needsYou struct {
	cursor   int
	mode     nyMode
	feedback string
	notice   string
	// pending is the item an in-progress prompt is about, held so the row moving underneath
	// cannot redirect the action.
	pending api.AttentionItem
}

func newNeedsYou() *needsYou { return &needsYou{} }

// CapturesKeys is true while a prompt or confirmation is open.
func (s *needsYou) CapturesKeys() bool { return s.mode != nyBrowsing }

type attentionActedMsg struct {
	verb string
	err  error
}

// reasonAction is one key a reason offers.
type reasonAction struct {
	Key  string
	Help string
	Run  func(s *needsYou, item api.AttentionItem, ctx ViewContext) tea.Cmd
}

// reasonSpec is how one reason renders and what can be done about it.
type reasonSpec struct {
	Detail  func(api.AttentionItem, ViewContext) []string
	Actions []reasonAction
}

// ---- shared actions ------------------------------------------------------

func actJumpToReview() reasonAction {
	return reasonAction{Key: "enter", Help: "open the review", Run: func(_ *needsYou, item api.AttentionItem, _ ViewContext) tea.Cmd {
		return Goto(SectionReview, item.Attention.TicketID)
	}}
}

func actSendBack() reasonAction {
	return reasonAction{Key: "r", Help: "send back with guidance", Run: func(s *needsYou, item api.AttentionItem, _ ViewContext) tea.Cmd {
		s.mode, s.feedback, s.pending, s.notice = nyFeedback, "", item, ""
		return nil
	}}
}

func actReject() reasonAction {
	return reasonAction{Key: "x", Help: "reject", Run: func(s *needsYou, item api.AttentionItem, _ ViewContext) tea.Cmd {
		s.mode, s.pending, s.notice = nyConfirmReject, item, ""
		return nil
	}}
}

func actAcknowledge() reasonAction {
	return reasonAction{Key: "a", Help: "acknowledge", Run: func(_ *needsYou, item api.AttentionItem, ctx ViewContext) tea.Cmd {
		id := item.Attention.ID
		return func() tea.Msg {
			return attentionActedMsg{verb: "acknowledged", err: ctx.Svc.ResolveAttention(context.Background(), id)}
		}
	}}
}

func actContinueLanding() reasonAction {
	return reasonAction{Key: "c", Help: "retry the land", Run: func(_ *needsYou, item api.AttentionItem, ctx ViewContext) tea.Cmd {
		id := item.Attention.TicketID
		return func() tea.Msg {
			// A retry that parks again has not landed either, and the row it reopens is
			// the only thing that would otherwise say so.
			state, err := ctx.Svc.Continue(context.Background(), id, core.ApprovePush)
			verb := "landed"
			if state == core.StateNeedsYou {
				verb = "still did not land — see the reopened row"
			}
			return attentionActedMsg{verb: verb, err: err}
		}
	}}
}

// ---- the registry --------------------------------------------------------

// reasonRegistry maps each reason to its renderer and actions.
//
// M0 populates the five reasons M0 can raise. agent_question, permission_request and
// ticket_critique are M1 tickets and are deliberately absent rather than present and inert —
// genericSpec renders anything not listed here, so an unknown reason degrades to something
// readable instead of crashing.
var reasonRegistry = map[core.AttentionReason]reasonSpec{
	core.ReasonReviewPending: {
		Detail: func(item api.AttentionItem, ctx ViewContext) []string {
			var out []string
			if c, ok := item.Attention.Payload["commit"].(string); ok && c != "" {
				out = append(out, ctx.Theme.Muted.Render("  commit "+shortID(c)))
			}
			if s, ok := item.Attention.Payload["summary"].(string); ok && s != "" {
				out = append(out, ctx.Theme.Success.Render("  "+firstLine(s)))
			}
			return out
		},
		Actions: []reasonAction{actJumpToReview(), actSendBack(), actReject()},
	},

	core.ReasonValidationFailed: {
		Detail: func(item api.AttentionItem, ctx ViewContext) []string {
			var out []string
			if r, ok := item.Attention.Payload["reason"].(string); ok && r != "" {
				out = append(out, ctx.Theme.Warning.Render("  "+r))
			}
			if s, ok := item.Attention.Payload["summary"].(string); ok && s != "" {
				for _, ln := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
					out = append(out, ctx.Theme.Danger.Render("  "+trunc(ln, max(0, ctx.Width-2))))
				}
			}
			if n, ok := item.Attention.Payload["attempts"].(float64); ok {
				out = append(out, ctx.Theme.Muted.Render(fmt.Sprintf("  after %d attempt(s)", int(n))))
			}
			return out
		},
		Actions: []reasonAction{actSendBack(), actReject()},
	},

	core.ReasonMergeConflict: {
		Detail: func(item api.AttentionItem, ctx ViewContext) []string {
			var out []string
			if e, ok := item.Attention.Payload["error"].(string); ok && e != "" {
				out = append(out, ctx.Theme.Danger.Render("  "+e))
			}
			// The conflicting paths are recorded so the human does not go looking.
			if files, ok := item.Attention.Payload["files"].([]any); ok && len(files) > 0 {
				out = append(out, ctx.Theme.Warning.Render("  conflicting files:"))
				for _, f := range files {
					out = append(out, ctx.Theme.Danger.Render("    "+fmt.Sprint(f)))
				}
			}
			if w, ok := item.Attention.Payload["worktree"].(string); ok && w != "" {
				out = append(out,
					ctx.Theme.Muted.Render("  the worktree is preserved for you at:"),
					ctx.Theme.Text.Render("  "+w))
			}
			return out
		},
		Actions: []reasonAction{actContinueLanding(), actReject()},
	},

	core.ReasonCheckoutDirty: {
		Detail: func(item api.AttentionItem, ctx ViewContext) []string {
			var out []string
			if r, ok := item.Attention.Payload["reason"].(string); ok && r != "" {
				out = append(out, ctx.Theme.Warning.Render("  "+r))
			}
			// The path first, then the files under it: the whole point of this reason is that
			// the work to do is in a directory the row does not otherwise name.
			if c, ok := item.Attention.Payload["checkout"].(string); ok && c != "" {
				out = append(out,
					ctx.Theme.Muted.Render("  commit or stash these, then continue the land:"),
					ctx.Theme.Text.Render("  "+c))
			}
			if files, ok := item.Attention.Payload["files"].([]any); ok && len(files) > 0 {
				for _, f := range files {
					out = append(out, ctx.Theme.Danger.Render("    "+fmt.Sprint(f)))
				}
			}
			return out
		},
		Actions: []reasonAction{actContinueLanding(), actReject()},
	},

	core.ReasonHostUnavailable: {
		Detail: func(item api.AttentionItem, ctx ViewContext) []string {
			var out []string
			for _, k := range []string{"reason", "detail"} {
				if v, ok := item.Attention.Payload[k].(string); ok && v != "" {
					out = append(out, ctx.Theme.Warning.Render("  "+v))
				}
			}
			if w, ok := item.Attention.Payload["worktree"].(string); ok && w != "" {
				out = append(out, ctx.Theme.Muted.Render("  worktree: "+w))
			}
			return out
		},
		Actions: []reasonAction{actSendBack(), actAcknowledge(), actReject()},
	},
}

// genericSpec renders a reason this build does not know about.
//
// A queue that crashes on an unfamiliar row is worse than one that shows it plainly: the row
// still tells the human something needs them, which is the whole job.
var genericSpec = reasonSpec{
	Detail: func(item api.AttentionItem, ctx ViewContext) []string {
		out := []string{ctx.Theme.Muted.Render("  this version of Gravy has no detail view for this reason")}
		keys := make([]string, 0, len(item.Attention.Payload))
		for k := range item.Attention.Payload {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, ctx.Theme.Text.Render(
				"  "+k+": "+trunc(fmt.Sprint(item.Attention.Payload[k]), max(0, ctx.Width-len(k)-4))))
		}
		return out
	},
	Actions: []reasonAction{actAcknowledge()},
}

func specFor(r core.AttentionReason) reasonSpec {
	if s, ok := reasonRegistry[r]; ok {
		return s
	}
	return genericSpec
}

// ---- the screen ----------------------------------------------------------

// visibleItems applies the frame's project and text filters. Ordering is left alone: the service
// returns the queue oldest-first, which is the order a human works through it and the reason
// nothing starves.
// clearHint names whatever is doing the hiding, so the way out is on the screen rather than
// something the human has to remember about a filter they set three screens ago.
func clearHint(ctx ViewContext) string {
	switch {
	case ctx.Project != "" && strings.TrimSpace(ctx.Filter) != "":
		return fmt.Sprintf("Scoped to %s and filtered by %q — esc clears the filter, p switches project.", ctx.Project, ctx.Filter)
	case ctx.Project != "":
		return fmt.Sprintf("Scoped to %s — p switches project, and the count in the status bar is fleet-wide.", ctx.Project)
	case strings.TrimSpace(ctx.Filter) != "":
		return fmt.Sprintf("Filtered by %q — esc clears it.", ctx.Filter)
	}
	return "Nothing here matches, which should not happen; refresh with r."
}

// plural picks a form and prefixes the count, because "1 items are hidden" reads like a bug in
// the thing that is telling you about a bug.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (s *needsYou) visibleItems(ctx ViewContext) []api.AttentionItem {
	var out []api.AttentionItem
	needle := strings.ToLower(strings.TrimSpace(ctx.Filter))

	for _, item := range ctx.Status.Attention {
		if ctx.Project != "" && projectName(item.Project) != ctx.Project {
			continue
		}
		if needle != "" {
			hay := strings.ToLower(strings.Join([]string{
				string(item.Attention.Reason), item.Attention.TicketID,
				item.Ticket.Title, projectName(item.Project),
			}, " "))
			if !strings.Contains(hay, needle) {
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func (s *needsYou) Update(msg tea.Msg, ctx ViewContext) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case enteredMsg:
		if msg.focus != "" {
			items := s.visibleItems(ctx)
			for i, it := range items {
				if it.Attention.ID == msg.focus || it.Attention.TicketID == msg.focus {
					s.cursor = i
					break
				}
			}
		}
		return s, nil

	case attentionActedMsg:
		if msg.err != nil {
			s.notice = fmt.Sprintf("%s failed: %v", msg.verb, msg.err)
		} else {
			s.notice = msg.verb
		}
		return s, nil

	case tea.KeyMsg:
		return s.handleKey(msg, ctx)
	}
	return s, nil
}

func (s *needsYou) handleKey(msg tea.KeyMsg, ctx ViewContext) (Screen, tea.Cmd) {
	key := msg.String()

	switch s.mode {
	case nyFeedback:
		switch {
		case key == "esc":
			s.mode, s.feedback = nyBrowsing, ""
		case key == "enter":
			if strings.TrimSpace(s.feedback) == "" {
				s.notice = "say what needs to change, or esc to cancel"
				return s, nil
			}
			id, fb := s.pending.Attention.TicketID, s.feedback
			s.mode, s.feedback = nyBrowsing, ""
			return s, func() tea.Msg {
				return attentionActedMsg{verb: "sent back to the agent",
					err: ctx.Svc.RequestChanges(context.Background(), id, fb)}
			}
		case key == "backspace":
			if s.feedback != "" {
				_, size := utf8.DecodeLastRuneInString(s.feedback)
				s.feedback = s.feedback[:len(s.feedback)-size]
			}
		case len(msg.Runes) > 0:
			s.feedback += string(msg.Runes)
			s.notice = ""
		}
		return s, nil

	case nyConfirmReject:
		s.mode = nyBrowsing
		if key == "y" || key == "Y" {
			id := s.pending.Attention.TicketID
			return s, func() tea.Msg {
				return attentionActedMsg{verb: "rejected",
					err: ctx.Svc.Reject(context.Background(), id)}
			}
		}
		s.notice = "rejection cancelled"
		return s, nil
	}

	items := s.visibleItems(ctx)
	if len(items) == 0 {
		return s, nil
	}
	s.cursor = clamp(s.cursor, 0, len(items)-1)

	switch key {
	case "up", "k":
		if s.cursor > 0 {
			s.cursor--
		}
		return s, nil
	case "down", "j":
		if s.cursor < len(items)-1 {
			s.cursor++
		}
		return s, nil
	case "S":
		return s, Sweep(sweepOrder(ctx))
	case "home", "g":
		s.cursor = 0
		return s, nil
	case "end", "G":
		s.cursor = len(items) - 1
		return s, nil
	}

	// Whatever the selected row's reason offers.
	item := items[s.cursor]
	for _, a := range specFor(item.Attention.Reason).Actions {
		if a.Key == key {
			s.notice = ""
			return s, a.Run(s, item, ctx)
		}
	}
	return s, nil
}

func (s *needsYou) View(ctx ViewContext) string {
	th := ctx.Theme
	if ctx.Width <= 0 || ctx.Height <= 0 {
		return ""
	}

	items := s.visibleItems(ctx)
	if len(items) == 0 {
		// "Nothing needs you" is a promise, and this screen filters by project and by search
		// while the status bar counts the whole fleet. When those disagree the old empty
		// state said the reassuring thing and the status bar said "needs you 1", and the
		// human went looking for work the screen had hidden from them.
		head := []string{th.Header.Render("Needs You (0)"), ""}
		if hidden := len(ctx.Status.Attention); hidden > 0 {
			head = append(head,
				th.Warning.Render(fmt.Sprintf("  Nothing here, but %s hidden by this view.", plural(hidden, "item is", "items are"))),
				th.Muted.Render("  "+clearHint(ctx)),
			)
		} else {
			head = append(head,
				th.Success.Render("  Nothing needs you."),
				th.Muted.Render("  If it is not here, Gravy does not need you."),
			)
		}
		return pinFooter(append(head, s.heldQueues(ctx)...), -1, ctx.Height, th, actionFooter(s.notice, th.Muted.Render("1 dashboard · 4 backlog · 7 review"), ctx.Width, th))
	}
	s.cursor = clamp(s.cursor, 0, len(items)-1)

	lines := []string{th.Header.Render(fmt.Sprintf("Needs You (%d)", len(items)))}
	selected := -1
	for i, item := range items {
		marker, style := "  ", th.Text
		if i == s.cursor {
			marker, style = "▸ ", th.Accent
			selected = len(lines)
		}
		lines = append(lines, style.Render(marker+columns(ctx.Width-2,
			col{text: string(item.Attention.Reason), width: 17},
			col{text: projectName(item.Project), width: 11},
			col{text: shortID(item.Attention.TicketID), width: 8},
			col{text: item.Ticket.Title, flex: true},
			col{text: age(item.Age), width: 5, right: true},
		)))
	}

	// The detail pane for the selected row: enough to act on without leaving the screen.
	item := items[s.cursor]
	lines = append(lines, "")
	lines = append(lines, specFor(item.Attention.Reason).Detail(item, ctx)...)
	lines = append(lines, s.heldQueues(ctx)...)

	footer := s.footer(item, th, ctx.Width)
	if s.mode == nyFeedback {
		hint := "enter send · esc cancel"
		if s.notice != "" {
			hint = s.notice + " · " + hint
		}
		footer = feedbackInput(s.feedback, hint, ctx.Width, ctx.Height, th)
	}
	return pinFooter(lines, selected, ctx.Height, th, footer)
}

// heldQueues names the ticket blocking each serialised project, so an idle queue is never
// unexplained.
func (s *needsYou) heldQueues(ctx ViewContext) []string {
	var out []string
	for _, p := range ctx.Status.Projects {
		if p.Blocked == "" {
			continue
		}
		if ctx.Project != "" && projectName(p.Project) != ctx.Project {
			continue
		}
		out = append(out, ctx.Theme.Muted.Render(
			"  "+projectName(p.Project)+" is holding its queue: "+p.Blocked))
	}
	if len(out) > 0 {
		out = append([]string{"", ctx.Theme.Header.Render("Held queues")}, out...)
	}
	return out
}

func (s *needsYou) footer(item api.AttentionItem, th Theme, widths ...int) (result string) {
	width := 100
	if len(widths) > 0 {
		width = widths[0]
	}
	defer func() { result = actionFooter(s.notice, result, width, th) }()
	switch s.mode {
	case nyFeedback:
		hint := "▏  enter to send back · esc to cancel"
		if s.notice != "" {
			hint = "▏  " + s.notice + " · enter send · esc cancel"
		}
		return th.Accent.Render("what needs to change: ") + th.Text.Render(s.feedback) + th.Muted.Render(hint)
	case nyConfirmReject:
		return th.Danger.Render("reject "+shortID(s.pending.Attention.TicketID)+
			" and delete its worktree? ") + th.Muted.Render("y / n")
	}

	parts := make([]string, 0, 4)
	for _, a := range specFor(item.Attention.Reason).Actions {
		parts = append(parts, a.Key+" "+a.Help)
	}
	parts = append(parts, "j/k move")
	return th.Muted.Render(strings.Join(parts, " · "))
}
