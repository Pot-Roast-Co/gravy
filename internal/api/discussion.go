package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/store"
)

// Discusser holds one turn of a change discussion.
//
// An interface for the same reason as Lander and Planner: api says what it needs rather than
// depending on how agents are run, and a client without one can still read a discussion, edit
// its instruction and send it — which keeps the human gate working on a machine with no model
// available.
type Discusser interface {
	Discuss(ctx context.Context, turn core.DiscussionTurn) (core.DiscussionResult, error)
}

// WithDiscusser gives the service an agent to discuss corrections with.
func (l *Local) WithDiscusser(d Discusser) *Local {
	l.discusser = d
	return l
}

// DiscussionView is a change discussion and what the ticket has already agreed.
//
// The agreed instructions travel with it because they are what makes a second correction safe:
// the human editing a new instruction needs to see the constraints earlier rounds were accepted
// on, and so does the agent proposing it.
type DiscussionView struct {
	Discussion core.ChangeDiscussion
	// Agreed are the instructions already sent for this ticket, oldest first.
	Agreed []core.ChangeInstruction
}

// DiscussReq is one turn of a change discussion.
type DiscussReq struct {
	TicketID string `json:"ticket_id"`
	// Message is what the human said. It is a message, not an instruction: sending one never
	// queues work and never authorises an edit.
	Message string `json:"message"`
	// Proposal, when set, replaces the draft before the turn, so the agent reacts to what the
	// human actually has in front of them rather than to its own last suggestion.
	Proposal *core.ChangeInstruction `json:"proposal,omitempty"`
}

// ProposalReq saves the human's edit of the proposed instruction.
type ProposalReq struct {
	TicketID string                 `json:"ticket_id"`
	Proposal core.ChangeInstruction `json:"proposal"`
}

// SendChangesReq is the gate: the instruction a human confirmed, and the confirmation itself.
type SendChangesReq struct {
	TicketID string                 `json:"ticket_id"`
	Proposal core.ChangeInstruction `json:"proposal"`
	// Confirm must be true. It is on the wire rather than left to each client's own dialog so
	// that "the human explicitly confirmed this" is a fact the daemon checked, not a convention
	// a second client could forget to follow.
	Confirm bool `json:"confirm"`
}

// OpenDiscussion starts or resumes the discussion about a ticket's current review.
//
// Opening one changes nothing: the ticket stays where it is, no run is queued, and nothing is
// authorised. That is the point — request changes used to be a single keystroke that put work
// back in front of an agent, and this is the step in between.
func (l *Local) OpenDiscussion(ctx context.Context, ticketID string) (DiscussionView, error) {
	t, err := l.db.GetTicket(ctx, ticketID)
	if err != nil {
		return DiscussionView{}, err
	}
	if !core.NeedsHuman(t.State) {
		return DiscussionView{}, fmt.Errorf(
			"%s is %s — a change discussion belongs to work that is waiting on you", ticketID, t.State)
	}

	runID := ""
	if runs, err := l.db.ListRunsForTicket(ctx, ticketID); err == nil && len(runs) > 0 {
		runID = runs[0].ID
	}

	cd, err := l.db.OpenDiscussionFor(ctx, ticketID)
	switch {
	case err == nil && cd.RunID == runID:
		return l.view(ctx, cd)
	case err == nil:
		// The work ran again since this conversation started, so it is agreement about a diff
		// that no longer exists. Superseded rather than silently continued: carrying it on would
		// attach the human's reasoning to code neither party has read.
		cd.State, cd.UpdatedAt = core.DiscussionCanceled, l.now()
		if err := l.db.SaveDiscussion(ctx, cd); err != nil {
			return DiscussionView{}, err
		}
	case !errors.Is(err, store.ErrNotFound):
		return DiscussionView{}, err
	}

	now := l.now()
	fresh := core.ChangeDiscussion{
		ID: l.newID(), TicketID: ticketID, RunID: runID, State: core.DiscussionOpen,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := l.db.SaveDiscussion(ctx, fresh); err != nil {
		return DiscussionView{}, err
	}
	return l.view(ctx, fresh)
}

// Discuss takes one turn.
//
// It never moves the ticket. Everything it writes is a transcript and a draft, both of which a
// human can throw away by cancelling.
func (l *Local) Discuss(ctx context.Context, req DiscussReq) (DiscussionView, error) {
	cd, err := l.db.OpenDiscussionFor(ctx, req.TicketID)
	if err != nil {
		return DiscussionView{}, fmt.Errorf("discuss %s: %w", req.TicketID, err)
	}
	if strings.TrimSpace(req.Message) == "" {
		return DiscussionView{}, fmt.Errorf("discuss %s: say something", req.TicketID)
	}
	if l.discusser == nil {
		return DiscussionView{}, fmt.Errorf("this client cannot run a discussion")
	}
	if req.Proposal != nil {
		cd.Proposal = req.Proposal.Normalized()
	}

	t, err := l.db.GetTicket(ctx, req.TicketID)
	if err != nil {
		return DiscussionView{}, err
	}
	project, err := l.db.GetProject(ctx, t.ProjectID)
	if err != nil {
		return DiscussionView{}, err
	}
	prior, err := l.db.ListChangeInstructions(ctx, req.TicketID)
	if err != nil {
		return DiscussionView{}, err
	}

	// The evidence is assembled here rather than by the agent: the conversation has to be about
	// the review the human is looking at, not about whatever the agent decides to go and read.
	evidence := ""
	if rb, err := l.GetReview(ctx, req.TicketID); err == nil {
		evidence = renderEvidence(rb)
	} else {
		evidence = "The diff could not be read: " + err.Error()
	}

	history := append([]core.DiscussionMessage(nil), cd.Messages...)
	cd.Say(core.RoleHuman, req.Message, l.now())

	res, derr := l.discusser.Discuss(ctx, core.DiscussionTurn{
		Ticket: t, Project: project, Evidence: evidence, Prior: prior,
		History: history, Proposal: cd.Proposal,
		Session: cd.Session, Agent: cd.Agent, Message: req.Message,
		RunID: discussionRunID(cd.ID, len(cd.Messages)),
	})
	if derr != nil {
		// The failure is kept in the transcript so the question does not sit there looking as
		// though it had been ignored, and so it survives a restart like everything else here.
		cd.Say(core.RoleFailure, derr.Error(), l.now())
		cd.UpdatedAt = l.now()
		if serr := l.db.SaveDiscussion(ctx, cd); serr != nil {
			return DiscussionView{}, serr
		}
		return DiscussionView{}, derr
	}

	if res.Session != "" {
		cd.Session = res.Session
	}
	if res.Agent != "" {
		cd.Agent = res.Agent
	}
	cd.Say(core.RoleAgent, res.Reply, l.now())
	if !res.Proposal.Normalized().Empty() {
		cd.Proposal = res.Proposal.Normalized()
	}
	cd.UpdatedAt = l.now()
	if err := l.db.SaveDiscussion(ctx, cd); err != nil {
		return DiscussionView{}, err
	}
	return l.view(ctx, cd)
}

// SaveProposal records the human's edit of the proposed instruction.
//
// Saved rather than held in the client so that closing the UI, or restarting the daemon, does
// not throw away wording somebody spent five minutes getting right.
func (l *Local) SaveProposal(ctx context.Context, req ProposalReq) (DiscussionView, error) {
	cd, err := l.db.OpenDiscussionFor(ctx, req.TicketID)
	if err != nil {
		return DiscussionView{}, fmt.Errorf("save proposal for %s: %w", req.TicketID, err)
	}
	cd.Proposal = req.Proposal.Normalized()
	cd.UpdatedAt = l.now()
	if err := l.db.SaveDiscussion(ctx, cd); err != nil {
		return DiscussionView{}, err
	}
	return l.view(ctx, cd)
}

// SendChanges records the agreed instruction and sends the work back.
//
// This is the gate. Everything before it is conversation; this is the one call that moves a
// ticket, and it refuses to move one that the human has not explicitly confirmed.
func (l *Local) SendChanges(ctx context.Context, req SendChangesReq) error {
	if !req.Confirm {
		return fmt.Errorf("send changes: nothing is sent for implementation without your confirmation")
	}
	instruction := req.Proposal.Normalized()
	if err := core.ValidateChangeInstruction(instruction); err != nil {
		return fmt.Errorf("send changes: %w", err)
	}

	cd, err := l.db.OpenDiscussionFor(ctx, req.TicketID)
	if err != nil {
		return fmt.Errorf("send changes for %s: %w", req.TicketID, err)
	}

	now := l.now()
	instruction.ID = l.newID()
	instruction.TicketID = req.TicketID
	instruction.DiscussionID = cd.ID
	instruction.AgreedAt = now

	// The instruction is recorded before the ticket moves. A run that started against a ticket
	// whose agreement had not been written yet would be the one case where the whole step buys
	// nothing.
	if err := l.db.AddChangeInstruction(ctx, instruction); err != nil {
		return err
	}

	cd.Proposal = instruction
	cd.State, cd.UpdatedAt = core.DiscussionSent, now
	if err := l.db.SaveDiscussion(ctx, cd); err != nil {
		return err
	}

	// The same lifecycle request changes has always used: the worktree is preserved and the
	// ticket goes back to the queue. What changed is what it carries.
	return l.requestChanges(ctx, req.TicketID, instruction.Render())
}

// CancelDiscussion abandons the conversation.
//
// The ticket is not touched: it is still in Review, still carrying whatever instructions it
// already had, and still awaiting the same judgement. Backing out of a conversation must never
// be a decision.
func (l *Local) CancelDiscussion(ctx context.Context, ticketID string) error {
	cd, err := l.db.OpenDiscussionFor(ctx, ticketID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // cancelling a conversation nobody is having is not a failure
	}
	if err != nil {
		return err
	}
	cd.State, cd.UpdatedAt = core.DiscussionCanceled, l.now()
	return l.db.SaveDiscussion(ctx, cd)
}

// view loads the instructions that belong alongside a discussion.
func (l *Local) view(ctx context.Context, cd core.ChangeDiscussion) (DiscussionView, error) {
	agreed, err := l.db.ListChangeInstructions(ctx, cd.TicketID)
	if err != nil {
		return DiscussionView{}, err
	}
	return DiscussionView{Discussion: cd, Agreed: agreed}, nil
}

// discussionRunID names where one turn's progress is logged, so a client can tail it rather
// than watch a silent pause.
func discussionRunID(discussionID string, turn int) string {
	return fmt.Sprintf("discuss-%s-%d", discussionID, turn)
}

// maxEvidencePatch caps how much patch text one discussion prompt carries.
//
// The discussion is about a correction to a diff a human has already read, so the whole patch
// is rarely what makes the agent useful — and a generated file would otherwise push the ticket,
// the validation results and the earlier instructions out of any budget.
const maxEvidencePatch = 24000

// renderEvidence turns the review bundle into the evidence a discussion argues from.
func renderEvidence(rb ReviewBundle) string {
	var b strings.Builder

	b.WriteString("# The work under review\n\n")
	fmt.Fprintf(&b, "Branch %s, merging into %s.\n\n", rb.Ticket.Branch, rb.Project.TargetBranch)

	if len(rb.Validations) > 0 {
		b.WriteString("## Recorded validation\n\n")
		for _, v := range rb.Validations {
			status := "passed"
			if v.ExitCode != 0 {
				status = fmt.Sprintf("FAILED (exit %d)", v.ExitCode)
			}
			fmt.Fprintf(&b, "- %s: %s\n", v.Step, status)
		}
		b.WriteString("\n")
	}

	// The advisory verdict travels as what it is. A discussion that treats it as a finding of
	// fact would be arguing from a second machine's opinion rather than from the diff.
	if v := rb.Verdict; v.Available() {
		b.WriteString("## Automated review (advisory)\n\n")
		if s := strings.TrimSpace(v.Summary); s != "" {
			b.WriteString(s + "\n")
		}
		for _, f := range v.Findings {
			where := f.File
			if f.Line > 0 {
				where = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			fmt.Fprintf(&b, "- [%s] %s: %s\n", f.Severity, where, strings.TrimSpace(f.Rationale))
		}
		b.WriteString("\n")
	} else if v.Unavailable != "" {
		fmt.Fprintf(&b, "## Automated review (advisory)\n\nNo verdict: %s\n\n", v.Unavailable)
	}

	if n := strings.TrimSpace(rb.Summary.Narrative); n != "" {
		b.WriteString("## What changed, from the diff\n\n" + n + "\n\n")
	}
	for _, a := range rb.Summary.Assumptions {
		fmt.Fprintf(&b, "- assumption recorded while working: %s\n", a)
	}

	adds, dels := rb.Diff.Totals()
	fmt.Fprintf(&b, "## The diff — %d file(s), +%d -%d\n\n", len(rb.Diff.Files), adds, dels)
	budget := maxEvidencePatch
	for _, f := range rb.Diff.Files {
		fmt.Fprintf(&b, "### %s (%s, +%d -%d)\n\n", f.Path, f.Status, f.Additions, f.Deletions)
		patch := f.Patch
		if len(patch) > budget {
			patch = patch[:max(0, budget)]
			budget = 0
		} else {
			budget -= len(patch)
		}

		if strings.TrimSpace(patch) == "" {
			b.WriteString("(patch omitted — the diff is too large to carry in full)\n\n")
			continue
		}
		b.WriteString("```diff\n" + patch + "\n```\n\n")
	}
	return b.String()
}
