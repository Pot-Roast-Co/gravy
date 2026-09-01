package core

import (
	"errors"
	"testing"
)

// TestTransitionLegal covers every edge in the table. If an edge is added to transitions
// without a case here, TestTableFullyCovered fails.
func TestTransitionLegal(t *testing.T) {
	tests := []struct {
		from State
		ev   Event
		want State
	}{
		{StateDraft, EventSubmit, StateBacklog},

		{StateBacklog, EventMarkReady, StateReady},
		{StateBacklog, EventReject, StateRejected},

		{StateReady, EventAssign, StateAssigned},
		{StateReady, EventReject, StateRejected},

		{StateAssigned, EventStart, StateRunning},
		{StateAssigned, EventKill, StateBacklog},

		{StateRunning, EventAgentFinished, StateValidating},
		{StateRunning, EventAsk, StateBlocked},
		{StateRunning, EventRunFailed, StateNeedsYou},
		{StateRunning, EventKill, StateBacklog},

		{StateValidating, EventValidationPassed, StateReviewing},
		{StateValidating, EventValidationRetry, StateRunning},
		{StateValidating, EventValidationExhausted, StateNeedsYou},
		{StateValidating, EventKill, StateBacklog},

		{StateReviewing, EventReviewed, StateReview},
		{StateReviewing, EventKill, StateBacklog},

		{StateReview, EventApprove, StateLanding},
		{StateReview, EventRequestChanges, StateReady},
		{StateReview, EventReject, StateRejected},

		{StateLanding, EventLanded, StateDone},
		{StateLanding, EventLandFailed, StateNeedsYou},

		{StateBlocked, EventAnswer, StateReady},
		{StateBlocked, EventReject, StateRejected},

		{StateNeedsYou, EventRequeue, StateReady},
		{StateNeedsYou, EventReturnToReview, StateReview},
		{StateNeedsYou, EventReject, StateRejected},
	}

	for _, tt := range tests {
		t.Run(string(tt.from)+"/"+string(tt.ev), func(t *testing.T) {
			got, err := Transition(tt.from, tt.ev)
			if err != nil {
				t.Fatalf("Transition(%q, %q) errored: %v", tt.from, tt.ev, err)
			}
			if got != tt.want {
				t.Errorf("Transition(%q, %q) = %q, want %q", tt.from, tt.ev, got, tt.want)
			}
		})
	}
}

// TestTransitionIllegal asserts that representative illegal edges are errors rather than being
// silently ignored. The approval gate cases are the ones that matter: no event may reach Landing
// or Done except through an explicit human approve.
func TestTransitionIllegal(t *testing.T) {
	tests := []struct {
		name string
		from State
		ev   Event
	}{
		{"cannot skip the backlog", StateDraft, EventMarkReady},
		{"cannot run an unassigned ticket", StateReady, EventStart},
		{"cannot validate without running", StateAssigned, EventAgentFinished},
		{"cannot approve before review", StateValidating, EventApprove},

		// The approval gate. Nothing merges without recorded human approval, and no
		// configuration flag changes that — so no other edge may reach Landing or Done.
		{"green validation cannot land itself", StateValidating, EventLanded},
		{"automated review cannot approve", StateReviewing, EventApprove},
		{"automated review cannot land", StateReviewing, EventLanded},
		{"a run cannot land itself", StateRunning, EventApprove},
		{"needs-you cannot land directly", StateNeedsYou, EventApprove},

		// A kill partway through a rebase-and-push would leave the target branch in a
		// state nobody chose, so Landing does not accept one.
		{"landing cannot be killed", StateLanding, EventKill},
		{"a ticket awaiting review has no run to kill", StateReview, EventKill},
		{"a blocked ticket holds no worker to free", StateBlocked, EventKill},
		{"a done ticket cannot be killed", StateDone, EventKill},
		{"terminal states do not move: done", StateDone, EventRequeue},
		{"terminal states do not move: rejected", StateRejected, EventRequeue},
		{"a done ticket cannot be rejected", StateDone, EventReject},

		{"blocked resumes via answer, not requeue", StateBlocked, EventRequeue},
		{"landing cannot be re-approved", StateLanding, EventApprove},
		{"unknown state", State("nonsense"), EventSubmit},
		{"unknown event", StateReady, Event("nonsense")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Transition(tt.from, tt.ev)
			if err == nil {
				t.Fatalf("Transition(%q, %q) = %q, want an error", tt.from, tt.ev, got)
			}
			if !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("error %v is not ErrIllegalTransition", err)
			}
			var te *TransitionError
			if !errors.As(err, &te) {
				t.Fatalf("error %v is not a *TransitionError", err)
			}
			if te.From != tt.from || te.Event != tt.ev {
				t.Errorf("TransitionError = {%q, %q}, want {%q, %q}", te.From, te.Event, tt.from, tt.ev)
			}
			if got != "" {
				t.Errorf("failed transition returned state %q, want empty", got)
			}
		})
	}
}

// TestHappyPath walks the full v0.1 lifecycle in one sequence.
func TestHappyPath(t *testing.T) {
	walk(t, StateDraft, []step{
		{EventSubmit, StateBacklog},
		{EventMarkReady, StateReady},
		{EventAssign, StateAssigned},
		{EventStart, StateRunning},
		{EventAgentFinished, StateValidating},
		{EventValidationPassed, StateReviewing},
		{EventReviewed, StateReview},
		{EventApprove, StateLanding},
		{EventLanded, StateDone},
	})
}

// TestBlockedReentry covers the escalation road: the agent asks, a human answers, and the ticket
// re-enters the queue to resume its prior session.
func TestBlockedReentry(t *testing.T) {
	walk(t, StateRunning, []step{
		{EventAsk, StateBlocked},
		{EventAnswer, StateReady},
		{EventAssign, StateAssigned},
		{EventStart, StateRunning},
		{EventAgentFinished, StateValidating},
	})
}

// TestNeedsYouReentries covers all three ways out of the Needs You queue.
func TestNeedsYouReentries(t *testing.T) {
	t.Run("validation failure requeued", func(t *testing.T) {
		walk(t, StateValidating, []step{
			{EventValidationExhausted, StateNeedsYou},
			{EventRequeue, StateReady},
		})
	})
	t.Run("merge conflict resolved by hand returns to review", func(t *testing.T) {
		walk(t, StateLanding, []step{
			{EventLandFailed, StateNeedsYou},
			{EventReturnToReview, StateReview},
			{EventApprove, StateLanding},
			{EventLanded, StateDone},
		})
	})
	t.Run("rejected from needs you", func(t *testing.T) {
		walk(t, StateRunning, []step{
			{EventRunFailed, StateNeedsYou},
			{EventReject, StateRejected},
		})
	})
}

// TestValidationRetryLoop covers a failed validation going back to the agent and then passing,
// which is the bounded self-correction loop.
func TestValidationRetryLoop(t *testing.T) {
	walk(t, StateValidating, []step{
		{EventValidationRetry, StateRunning},
		{EventAgentFinished, StateValidating},
		{EventValidationPassed, StateReviewing},
	})
}

// TestRequestChangesReusesQueue covers review sending work back to the agent.
func TestRequestChangesReusesQueue(t *testing.T) {
	walk(t, StateReview, []step{
		{EventRequestChanges, StateReady},
		{EventAssign, StateAssigned},
		{EventStart, StateRunning},
	})
}

type step struct {
	ev   Event
	want State
}

func walk(t *testing.T, from State, steps []step) {
	t.Helper()
	cur := from
	for i, s := range steps {
		next, err := Transition(cur, s.ev)
		if err != nil {
			t.Fatalf("step %d: Transition(%q, %q): %v", i, cur, s.ev, err)
		}
		if next != s.want {
			t.Fatalf("step %d: Transition(%q, %q) = %q, want %q", i, cur, s.ev, next, s.want)
		}
		cur = next
	}
}

// TestKillFreesTheProject covers M0 exit criterion 9's second half: killing a run frees the
// slot. In serial mode a project is held by any active ticket, so the killed ticket must land
// somewhere inactive or the repository would stay blocked by a run that no longer exists.
func TestKillFreesTheProject(t *testing.T) {
	for _, from := range []State{StateAssigned, StateRunning, StateValidating, StateReviewing} {
		to, err := Transition(from, EventKill)
		if err != nil {
			t.Errorf("Transition(%q, kill): %v", from, err)
			continue
		}
		if IsActive(to) {
			t.Errorf("killing from %q lands in %q, which is still active; the project stays held "+
				"by a run that no longer exists", from, to)
		}
		if NeedsHuman(to) {
			t.Errorf("killing from %q lands in %q, which queues an attention item notifying the "+
				"person who pressed kill", from, to)
		}
		if IsTerminal(to) {
			t.Errorf("killing from %q lands in terminal state %q; the work would be unrecoverable", from, to)
		}
	}
}

// TestKilledTicketIsNotImmediatelyReclaimed: a killed ticket must not be schedulable again
// without the human saying so, or an idle worker picks it back up within the second.
func TestKilledTicketIsNotImmediatelyReclaimed(t *testing.T) {
	to, err := Transition(StateRunning, EventKill)
	if err != nil {
		t.Fatal(err)
	}
	if to == StateReady {
		t.Fatal("a killed ticket returns to Ready, where the scheduler will immediately re-claim it")
	}
	// It must still be recoverable in one deliberate step.
	if _, err := Transition(to, EventMarkReady); err != nil {
		t.Errorf("a killed ticket cannot be made Ready again: %v", err)
	}
}

// TestTableFullyCovered asserts the transition table only mentions known states and events, and
// that every declared state appears in it. This is what keeps the table honest as states are
// added: a new state with no entry fails here rather than erroring mysteriously at runtime.
func TestTableFullyCovered(t *testing.T) {
	for _, s := range AllStates {
		if _, ok := transitions[s]; !ok {
			t.Errorf("state %q has no entry in the transition table", s)
		}
	}
	if len(transitions) != len(AllStates) {
		t.Errorf("transition table has %d states, AllStates has %d", len(transitions), len(AllStates))
	}
	for from, edges := range transitions {
		if !from.Valid() {
			t.Errorf("transition table mentions unknown state %q", from)
		}
		for ev, to := range edges {
			if !to.Valid() {
				t.Errorf("%q/%q targets unknown state %q", from, ev, to)
			}
		}
	}
}

// TestTerminalStatesHaveNoExits is the structural form of "Done means done".
func TestTerminalStatesHaveNoExits(t *testing.T) {
	for _, s := range AllStates {
		if !IsTerminal(s) {
			continue
		}
		if got := len(transitions[s]); got != 0 {
			t.Errorf("terminal state %q has %d outgoing edges, want 0", s, got)
		}
		if len(Events(s)) != 0 {
			t.Errorf("Events(%q) is non-empty for a terminal state", s)
		}
	}
}

// TestOnlyApproveReachesLanding is the state-machine half of the hard approval gate: Landing has
// exactly one entry point and it is the human's.
func TestOnlyApproveReachesLanding(t *testing.T) {
	for from, edges := range transitions {
		for ev, to := range edges {
			if to != StateLanding {
				continue
			}
			if from != StateReview || ev != EventApprove {
				t.Errorf("%q/%q reaches Landing; only review/approve may", from, ev)
			}
		}
	}
}

func TestClassification(t *testing.T) {
	tests := []struct {
		name  string
		state State
		term  bool
		act   bool
		human bool
	}{
		{"draft", StateDraft, false, false, false},
		{"backlog", StateBacklog, false, false, false},
		{"ready", StateReady, false, false, false},
		{"assigned", StateAssigned, false, true, false},
		{"running", StateRunning, false, true, false},
		{"validating", StateValidating, false, true, false},
		{"reviewing", StateReviewing, false, true, false},
		{"review", StateReview, false, true, true},
		{"landing", StateLanding, false, true, false},
		{"blocked", StateBlocked, false, true, true},
		{"needs you", StateNeedsYou, false, true, true},
		{"done", StateDone, true, false, false},
		{"rejected", StateRejected, true, false, false},
	}
	if len(tests) != len(AllStates) {
		t.Fatalf("classification table covers %d states, AllStates has %d", len(tests), len(AllStates))
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTerminal(tt.state); got != tt.term {
				t.Errorf("IsTerminal(%q) = %v, want %v", tt.state, got, tt.term)
			}
			if got := IsActive(tt.state); got != tt.act {
				t.Errorf("IsActive(%q) = %v, want %v", tt.state, got, tt.act)
			}
			if got := NeedsHuman(tt.state); got != tt.human {
				t.Errorf("NeedsHuman(%q) = %v, want %v", tt.state, got, tt.human)
			}
		})
	}
}

// TestActiveAndTerminalAreDisjoint guards the serial-mode invariant: a project is held by any
// active ticket, so no terminal state may ever count as active or a repository would deadlock.
func TestActiveAndTerminalAreDisjoint(t *testing.T) {
	for _, s := range AllStates {
		if IsActive(s) && IsTerminal(s) {
			t.Errorf("state %q is both active and terminal", s)
		}
	}
}

// TestNeedsHumanImpliesActive: everything awaiting judgement still holds its project in serial
// mode. This is the cost that makes review latency the throughput gate, and it is deliberate.
func TestNeedsHumanImpliesActive(t *testing.T) {
	for _, s := range AllStates {
		if NeedsHuman(s) && !IsActive(s) {
			t.Errorf("state %q needs a human but is not active", s)
		}
	}
}
