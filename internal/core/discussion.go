package core

import (
	"fmt"
	"strings"
	"time"
)

// A change discussion is the step in front of "request changes".
//
// Sending a correction straight into execution is how a small fix undoes work that was already
// accepted: the agent is handed one sentence, has no record of what the previous rounds agreed,
// and is free to reach the new goal by removing the old one. The discussion exists to turn that
// sentence into an instruction that says what to change, what must survive, and how to check
// both — agreed with a human before anything runs.
//
// Nothing here decides anything. A discussion is advisory until the human confirms it, exactly
// as automated review is advisory and planning proposes rather than creates.

// DiscussionState is where a change discussion has got to.
type DiscussionState string

// The discussion states.
const (
	// DiscussionOpen is a conversation still being had. Nothing is queued and nothing is
	// authorised while a discussion is open.
	DiscussionOpen DiscussionState = "open"
	// DiscussionSent is a conversation whose instruction the human confirmed. The ticket went
	// back to the agent when it was recorded.
	DiscussionSent DiscussionState = "sent"
	// DiscussionCanceled is one abandoned by the human, or superseded because the work was run
	// again and the review it was about no longer exists.
	DiscussionCanceled DiscussionState = "canceled"
)

// Valid reports whether s is a known discussion state.
func (s DiscussionState) Valid() bool {
	return s == DiscussionOpen || s == DiscussionSent || s == DiscussionCanceled
}

// DiscussionRole says who produced a message.
type DiscussionRole string

// The roles a message can have.
const (
	RoleHuman DiscussionRole = "human"
	RoleAgent DiscussionRole = "agent"
	// RoleFailure records a turn that produced no answer. It is kept in the transcript so a
	// question is never left sitting there looking as though it had been ignored.
	RoleFailure DiscussionRole = "failure"
)

// DiscussionMessage is one turn of a change discussion.
type DiscussionMessage struct {
	Role DiscussionRole
	Text string
	At   time.Time
}

// ChangeInstruction is one agreed correction.
//
// The three parts are the whole point. A correction alone is what "request changes" already
// sent, and what let a narrow fix quietly widen; Preserve is the behaviour the correction must
// not cost, and Verify is how either party can tell afterwards whether that held.
type ChangeInstruction struct {
	ID           string
	TicketID     string
	DiscussionID string
	// Correction is what should change, in the human's terms.
	Correction string
	// Preserve is the behaviour that must survive the correction. Constraints agreed in an
	// earlier round stay binding in every later one.
	Preserve []string
	// Verify is how to check that the correction landed and the preserved behaviour held.
	Verify []string
	// AgreedAt is when the human confirmed it. Zero on a draft nobody has sent.
	AgreedAt time.Time
}

// Empty reports whether an instruction says nothing at all.
func (c ChangeInstruction) Empty() bool {
	return strings.TrimSpace(c.Correction) == "" &&
		len(nonEmpty(c.Preserve)) == 0 && len(nonEmpty(c.Verify)) == 0
}

// Normalized returns the instruction with blank list entries and surrounding space removed.
//
// A human editing three text fields leaves blank lines behind, and a blank preservation
// constraint in a prompt reads as an empty promise rather than as nothing.
func (c ChangeInstruction) Normalized() ChangeInstruction {
	c.Correction = strings.TrimSpace(c.Correction)
	c.Preserve = nonEmpty(c.Preserve)
	c.Verify = nonEmpty(c.Verify)
	return c
}

// ValidateChangeInstruction reports whether an instruction can be sent for implementation.
//
// Only the correction is required. A correction with nothing to preserve is a legitimate thing
// to agree — the first change to a ticket usually is — and demanding a constraint would teach
// people to invent one.
func ValidateChangeInstruction(c ChangeInstruction) error {
	if strings.TrimSpace(c.Correction) == "" {
		return fmt.Errorf("say what needs to change")
	}
	return nil
}

// Render writes one instruction the way an agent and a human both read it.
func (c ChangeInstruction) Render() string {
	c = c.Normalized()
	var b strings.Builder
	b.WriteString("Correction: ")
	b.WriteString(c.Correction)
	if len(c.Preserve) > 0 {
		b.WriteString("\n\nPreserve, unchanged:")
		for _, p := range c.Preserve {
			b.WriteString("\n- " + p)
		}
	}
	if len(c.Verify) > 0 {
		b.WriteString("\n\nVerify both:")
		for _, v := range c.Verify {
			b.WriteString("\n- " + v)
		}
	}
	return b.String()
}

// PreservationConstraints collects every constraint agreed so far, oldest first, without
// repeats.
//
// Repeats are ordinary: an agent asked to restate what must survive will restate the same
// sentence, and listing it four times in a prompt makes the one new constraint harder to find.
func PreservationConstraints(agreed []ChangeInstruction) []string {
	seen := map[string]bool{}
	var out []string
	for _, ci := range agreed {
		for _, p := range nonEmpty(ci.Preserve) {
			key := strings.ToLower(p)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, p)
		}
	}
	return out
}

// RenderChangeInstructions builds the block an implementation prompt carries.
//
// It renders the latest correction in full and then every preservation constraint from earlier
// rounds that the latest one does not already repeat. That ordering is deliberate: the agent's
// task is the newest correction, and the older constraints are the boundary it must not cross
// to get there. Dropping them is exactly the regression this whole step exists to prevent, so
// they are rendered as their own section rather than left to be inferred from a transcript.
//
// Returns the empty string when nothing has been agreed, so a caller can append it
// unconditionally.
func RenderChangeInstructions(agreed []ChangeInstruction) string {
	var kept []ChangeInstruction
	for _, ci := range agreed {
		if ci.Normalized().Empty() {
			continue
		}
		kept = append(kept, ci.Normalized())
	}
	if len(kept) == 0 {
		return ""
	}

	latest := kept[len(kept)-1]

	var b strings.Builder
	b.WriteString("## Agreed change instructions\n\n")
	b.WriteString("A human agreed these with an agent before this attempt. They are instructions, " +
		"not suggestions.\n\n")

	b.WriteString("### The correction to make")
	if !latest.AgreedAt.IsZero() {
		fmt.Fprintf(&b, " (agreed %s)", latest.AgreedAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	b.WriteString("\n\n")
	b.WriteString(latest.Correction)
	b.WriteString("\n")

	if len(latest.Preserve) > 0 {
		b.WriteString("\nPreserve, unchanged, while making it:\n")
		for _, p := range latest.Preserve {
			b.WriteString("- " + p + "\n")
		}
	}
	if len(latest.Verify) > 0 {
		b.WriteString("\nHow to verify both:\n")
		for _, v := range latest.Verify {
			b.WriteString("- " + v + "\n")
		}
	}

	// Everything agreed in earlier rounds that the latest correction does not already name.
	// These are the promises previous reviews were accepted on.
	if earlier := remaining(PreservationConstraints(kept[:len(kept)-1]), latest.Preserve); len(earlier) > 0 {
		b.WriteString("\n### Still binding from earlier corrections\n\n")
		b.WriteString("Earlier rounds of this ticket were accepted on these. The correction above " +
			"must not cost any of them:\n\n")
		for _, p := range earlier {
			b.WriteString("- " + p + "\n")
		}
	}

	if len(kept) > 1 {
		b.WriteString("\nIf the correction above cannot be made without breaking one of these, " +
			"stop and say so rather than choosing for the human.\n")
	}
	return b.String()
}

// remaining returns the entries of all that already is missing, comparing case-insensitively.
func remaining(all, already []string) []string {
	have := map[string]bool{}
	for _, a := range already {
		have[strings.ToLower(strings.TrimSpace(a))] = true
	}
	var out []string
	for _, s := range all {
		if !have[strings.ToLower(strings.TrimSpace(s))] {
			out = append(out, s)
		}
	}
	return out
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ChangeDiscussion is one conversation about one correction, scoped to a ticket and the review
// it is about.
//
// Scoped to the run as well as the ticket because a discussion is about a specific diff. Once
// the ticket runs again the diff is different, and a conversation about the old one would be
// agreement about code that no longer exists.
type ChangeDiscussion struct {
	ID       string
	TicketID string
	// RunID is the run whose diff is under discussion. Empty when the ticket has no run yet.
	RunID string
	State DiscussionState
	// Session resumes the agent's side of the conversation. Opaque and provider-specific.
	Session string
	// Agent names the provider and model holding it, for the screen to show.
	Agent    string
	Messages []DiscussionMessage
	// Proposal is the instruction currently on the table. It is a draft: the human edits it,
	// and nothing happens until they send it.
	Proposal  ChangeInstruction
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Say appends a message.
func (d *ChangeDiscussion) Say(role DiscussionRole, text string, at time.Time) {
	if strings.TrimSpace(text) == "" {
		return
	}
	d.Messages = append(d.Messages, DiscussionMessage{
		Role: role, Text: strings.TrimSpace(text), At: at,
	})
}

// DiscussionTurn is one request to the discussion agent.
//
// It carries rendered evidence rather than a diff because core owns no git types, and because
// what the agent needs is the review a human is looking at — not the ability to go and form its
// own.
type DiscussionTurn struct {
	Ticket  Ticket
	Project Project
	// Evidence is the current diff, the recorded validation results and the advisory verdict,
	// already rendered for a prompt.
	Evidence string
	// Prior are the instructions already agreed for this ticket, oldest first. The agent is
	// asked to say when a new correction conflicts with one of them.
	Prior []ChangeInstruction
	// History is the conversation so far, so a fallback to another model can rebuild it.
	History []DiscussionMessage
	// Proposal is the draft on the table, including any edit the human just made.
	Proposal ChangeInstruction
	// Session continues the conversation with the same agent. Empty starts one.
	Session string
	// Agent is the provider/model the session belongs to.
	Agent string
	// Message is what the human said this turn.
	Message string
	// RunID is where this turn's progress is logged, so a client can watch rather than wait.
	RunID string
}

// DiscussionResult is one turn of a change discussion.
type DiscussionResult struct {
	Session string
	Agent   string
	// Reply is what the agent said: the correction as it understood it, what it believes must
	// be preserved, and any question it needs answered.
	Reply string
	// Proposal is the instruction the agent now proposes. It replaces the draft wholesale, and
	// is still only a proposal — the human edits and confirms it.
	Proposal ChangeInstruction
}
