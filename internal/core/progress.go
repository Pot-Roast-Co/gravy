package core

import "time"

// ProgressPhase names which part of a ticket's journey an entry belongs to.
//
// A short machine token rather than prose: clients group and filter on it, and the sentence a
// human reads lives in Progress.Detail. Adding a phase is adding a constant here — the store
// does not constrain the set, so an entry written by a newer binary still reads on an older one.
type ProgressPhase string

// The phases, in the order a run passes through them.
const (
	// PhaseFetch is fetching the remote, before any worktree exists.
	PhaseFetch ProgressPhase = "fetch"
	// PhaseWorktree is cutting — or reusing — the ticket's isolated worktree.
	PhaseWorktree ProgressPhase = "worktree"
	// PhasePrompt is assembling the agent's context.
	PhasePrompt ProgressPhase = "prompt"
	// PhaseAgentStart is the provider process starting.
	PhaseAgentStart ProgressPhase = "agent_start"
	// PhaseAgentExit is that process finishing, with its classification and evidence.
	PhaseAgentExit ProgressPhase = "agent_exit"
	// PhaseValidationStep is one validation command's result.
	PhaseValidationStep ProgressPhase = "validation_step"
	// PhaseRetry is a self-correction attempt being spent.
	PhaseRetry ProgressPhase = "retry"
	// PhaseSummary is the durable result record being written from the diff.
	PhaseSummary ProgressPhase = "summary"
	// PhaseReview is the advisory review pass.
	PhaseReview ProgressPhase = "review"
	// PhaseHandoff is where the ticket ended up, and who has it now.
	PhaseHandoff ProgressPhase = "handoff"
)

// AllProgressPhases lists the phases a run narrates, in the order it reaches them.
var AllProgressPhases = []ProgressPhase{
	PhaseFetch, PhaseWorktree, PhasePrompt, PhaseAgentStart, PhaseAgentExit,
	PhaseValidationStep, PhaseRetry, PhaseSummary, PhaseReview, PhaseHandoff,
}

// Known reports whether p is one of the phases this build writes.
//
// Deliberately not called Valid: an unrecognised phase read back from the database is an entry
// from another version, not a corrupt row, and nothing may refuse to display it.
func (p ProgressPhase) Known() bool {
	for _, k := range AllProgressPhases {
		if k == p {
			return true
		}
	}
	return false
}

// Progress is one entry in a ticket's journal — what Gravy was doing, and when.
//
// The journal exists because a state name answers "where is this ticket" and nothing else. Fetch,
// worktree creation, prompt assembly, each validation step, self-correction retries and the
// advisory review all happen inside a single state, and the phases before the agent starts have
// no run row to hang anything from — which is why RunID is optional here and the ticket is what
// an entry always belongs to.
type Progress struct {
	ID       string
	TicketID string
	// RunID is empty for the phases that happen before a run row exists — fetch, worktree and
	// the first prompt build — and for landing, which is not a run at all.
	RunID string
	At    time.Time
	Phase ProgressPhase
	// Detail is one sentence a human reads, carrying the facts they would otherwise have to dig
	// for: a worktree path, a pid, an exit class and its evidence, a step's exit code.
	Detail string
}
