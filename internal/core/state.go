package core

import (
	"errors"
	"fmt"
)

// State is a ticket's position in the lifecycle defined in ARCHITECTURE.md §6.
type State string

// The ticket states. Draft through Done is the happy path; Blocked, NeedsYou and Rejected are
// the departures from it.
const (
	StateDraft      State = "draft"
	StateBacklog    State = "backlog"
	StateReady      State = "ready"
	StateAssigned   State = "assigned"
	StateRunning    State = "running"
	StateValidating State = "validating"
	StateReviewing  State = "reviewing" // automated review; advisory, never decides
	StateReview     State = "review"    // awaiting human judgement
	StateLanding    State = "landing"
	StateDone       State = "done"
	StateBlocked    State = "blocked"
	StateNeedsYou   State = "needs_you"
	StateRejected   State = "rejected"
)

// AllStates lists every state, in lifecycle order. Used by the store's validation and by tests
// that must stay exhaustive as states are added.
var AllStates = []State{
	StateDraft, StateBacklog, StateReady, StateAssigned, StateRunning, StateValidating,
	StateReviewing, StateReview, StateLanding, StateDone, StateBlocked, StateNeedsYou,
	StateRejected,
}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	for _, k := range AllStates {
		if k == s {
			return true
		}
	}
	return false
}

// Event is something that happens to a ticket, driving a transition.
type Event string

// The events that drive the state machine. Each corresponds to an edge in ARCHITECTURE.md §6.
const (
	// EventSubmit promotes a draft into the backlog.
	EventSubmit Event = "submit"
	// EventMarkReady makes a backlog ticket eligible for scheduling.
	EventMarkReady Event = "mark_ready"
	// EventAssign records that the scheduler picked a host, provider and model.
	EventAssign Event = "assign"
	// EventStart records that the agent process launched.
	EventStart Event = "start"
	// EventAgentFinished is a run that exited without escalating: on to validation.
	EventAgentFinished Event = "agent_finished"
	// EventAsk is the single escalation road: the agent wrote its AskPath because it hit a
	// genuine ambiguity or a permission it does not hold (ARCHITECTURE.md §6.2).
	EventAsk Event = "ask"
	// EventRunFailed is a task failure whose self-correction budget is exhausted.
	EventRunFailed Event = "run_failed"
	// EventValidationPassed sends green work to automated review.
	EventValidationPassed Event = "validation_passed"
	// EventValidationRetry is a red validation with budget remaining: back to the agent with
	// the failure output appended to its context.
	EventValidationRetry Event = "validation_retry"
	// EventValidationExhausted is a red validation with no budget left.
	EventValidationExhausted Event = "validation_exhausted"
	// EventReviewed records that automated review finished. It is advisory and never decides
	// the ticket's fate, so it has exactly one outcome.
	EventReviewed Event = "reviewed"
	// EventApprove is the human approval gate. Nothing reaches a target branch without it.
	EventApprove Event = "approve"
	// EventRequestChanges sends work back to the agent, reusing the worktree.
	EventRequestChanges Event = "request_changes"
	// EventReject abandons the ticket; the worktree is removed.
	EventReject Event = "reject"
	// EventLanded is a clean rebase, merge and push.
	EventLanded Event = "landed"
	// EventLandFailed is a merge conflict or a red re-validation after the target moved. The
	// worktree is preserved for the human.
	EventLandFailed Event = "land_failed"
	// EventAnswer is the human answering a question or granting a permission. The ticket
	// returns to Ready and resumes its prior session rather than starting fresh.
	EventAnswer Event = "answer"
	// EventKill is the human stopping a live run from the TUI.
	//
	// It sends the ticket to Backlog rather than Ready or NeedsYou. Ready would let an idle
	// worker re-claim it within the second, which is the opposite of what someone who just
	// pressed kill wants. NeedsYou would raise an attention item notifying the person who
	// caused it, adding noise to the queue that is meant to be the product.
	//
	// Backlog stops the ticket, releases its worker and its project, and preserves the
	// worktree and session so the work is not lost. The human re-readies it when they have
	// adjusted whatever made them intervene.
	EventKill Event = "kill"
	// EventRequeue returns an attention item to the queue.
	EventRequeue Event = "requeue"
	// EventReturnToReview sends an attention item back to the review queue, for example after
	// a human resolves a merge conflict by hand.
	EventReturnToReview Event = "return_to_review"
)

// ErrIllegalTransition is returned by Transition for any edge not in the table. Illegal
// transitions are always errors; they are never silently ignored.
var ErrIllegalTransition = errors.New("illegal state transition")

// TransitionError describes a rejected transition. It wraps ErrIllegalTransition so callers can
// test with errors.Is while still reporting precisely what was attempted.
type TransitionError struct {
	From  State
	Event Event
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("illegal state transition: cannot %q from %q", e.Event, e.From)
}

// Unwrap allows errors.Is(err, ErrIllegalTransition).
func (e *TransitionError) Unwrap() error { return ErrIllegalTransition }

// transitions is the whole state machine. It is an explicit table rather than a switch so that
// the legal edges can be enumerated, tested exhaustively, and rendered.
//
// One edge from ARCHITECTURE.md §6.1 is deliberately absent: a run that fails on a quota, rate
// limit or auth condition is retried by the orchestrator against the next route choice without
// the ticket leaving Running. Re-resolving a route is not a lifecycle change, and the diagram
// shows no edge for it.
//
// EventKill is an addition rather than an omission. The specification requires a kill switch
// that frees the worker (MILESTONES.md, M0 exit criterion 9) but names no target state, and a
// ticket whose run was killed cannot be left in Running with no process behind it. Landing
// deliberately does not accept it: a kill partway through a rebase-and-push would leave the
// target branch in a state nobody chose.
var transitions = map[State]map[Event]State{
	StateDraft: {
		EventSubmit: StateBacklog,
	},
	StateBacklog: {
		EventMarkReady: StateReady,
		EventReject:    StateRejected,
	},
	StateReady: {
		EventAssign: StateAssigned,
		EventReject: StateRejected,
	},
	StateAssigned: {
		EventStart: StateRunning,
		EventKill:  StateBacklog,
	},
	StateRunning: {
		EventAgentFinished: StateValidating,
		EventAsk:           StateBlocked,
		EventRunFailed:     StateNeedsYou,
		EventKill:          StateBacklog,
	},
	StateValidating: {
		EventValidationPassed:    StateReviewing,
		EventValidationRetry:     StateRunning,
		EventValidationExhausted: StateNeedsYou,
		EventKill:                StateBacklog,
	},
	StateReviewing: {
		EventReviewed: StateReview,
		EventKill:     StateBacklog,
	},
	StateReview: {
		EventApprove:        StateLanding,
		EventRequestChanges: StateReady,
		EventReject:         StateRejected,
	},
	StateLanding: {
		EventLanded:     StateDone,
		EventLandFailed: StateNeedsYou,
	},
	StateBlocked: {
		EventAnswer: StateReady,
		EventReject: StateRejected,
	},
	StateNeedsYou: {
		EventRequeue:        StateReady,
		EventReturnToReview: StateReview,
		EventReject:         StateRejected,
	},
	// Done and Rejected are terminal: no outgoing edges.
	StateDone:     {},
	StateRejected: {},
}

// Transition applies ev to from and returns the resulting state. An edge that is not in the
// table returns a *TransitionError wrapping ErrIllegalTransition.
func Transition(from State, ev Event) (State, error) {
	edges, ok := transitions[from]
	if !ok {
		return "", &TransitionError{From: from, Event: ev}
	}
	to, ok := edges[ev]
	if !ok {
		return "", &TransitionError{From: from, Event: ev}
	}
	return to, nil
}

// Events returns the events legal from a state, for building a UI that offers only valid
// actions. The order is not stable and callers that display it should sort.
func Events(from State) []Event {
	edges := transitions[from]
	out := make([]Event, 0, len(edges))
	for ev := range edges {
		out = append(out, ev)
	}
	return out
}

// IsTerminal reports whether a ticket in this state will never move again.
func IsTerminal(s State) bool {
	return s == StateDone || s == StateRejected
}

// IsActive reports whether a ticket counts as in flight for scheduling.
//
// This is the definition serial mode depends on (ARCHITECTURE.md §4.5): any state after Ready
// and before Done or Rejected. A project in serial mode is unavailable while any of its tickets
// is active, which is why the next ticket starts only once the previous one has merged — and why
// review latency, not agent speed, gates a repository's queue.
func IsActive(s State) bool {
	switch s {
	case StateAssigned, StateRunning, StateValidating, StateReviewing,
		StateReview, StateLanding, StateBlocked, StateNeedsYou:
		return true
	default:
		return false
	}
}

// NeedsHuman reports whether a ticket is waiting on human judgement rather than on Gravy.
//
// Note that a ticket can need a human without holding a worker: Blocked and NeedsYou both
// release their slot (ARCHITECTURE.md §6.2). A blocked ticket never holds a worker.
func NeedsHuman(s State) bool {
	switch s {
	case StateReview, StateBlocked, StateNeedsYou:
		return true
	default:
		return false
	}
}
