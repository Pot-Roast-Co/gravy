package core

import "fmt"

// PlannedTicket is a ticket a planning conversation proposes.
//
// It is a proposal and not a ticket. Nothing reaches the backlog until a human approves the
// plan, for the same reason detection proposes rather than writes and review is advisory: the
// judgement about what is worth building is the part being protected.
type PlannedTicket struct {
	Title string
	Body  string
	// Route is the bucket the work will ask for when it runs. A route, never a model — the
	// point of routing is that a quota failure falls back without anyone rewriting tickets.
	Route Route
	// DependsOn indexes other tickets in the same plan.
	//
	// Indexes rather than ids because none of these tickets exists yet: they are given ids
	// only when the human approves the plan, and a proposal that referred to ids would have
	// to be rewritten at the moment of approval.
	DependsOn []int
}

// PlanTurn is one request to the planner.
type PlanTurn struct {
	Project Project
	// Backlog is what already exists, so "what should be next" is answered against real work
	// rather than in a vacuum.
	Backlog []Ticket
	// Session continues an existing conversation. Empty starts one.
	Session string
	// Message is what the human said. Empty means the standing question: what should be next?
	Message string
	// RunID is where this turn's progress is logged, so a client can watch the planner work
	// instead of staring at a silent pause.
	RunID string
}

// PlanResult is one turn of a planning conversation.
type PlanResult struct {
	// Session resumes the conversation. Opaque, and provider-specific.
	Session string
	// Agent names the provider and model running the conversation, for the screen to show.
	//
	// Set when a conversation starts, and empty on a resume: resuming is bound to the session's
	// agent and never re-resolves a route, so there is no fresh choice to report.
	Agent string
	// Reply is what the planner said, for the human to read and push back on.
	Reply string
	// Tickets is the plan as it currently stands, and is empty while the conversation is still
	// exploring. Each turn replaces it wholesale: a plan is a current best proposal, not a
	// list that accumulates every idea mentioned along the way.
	Tickets []PlannedTicket
}

// ValidatePlan reports whether a proposed plan can be approved into the backlog.
//
// Checked before anything is written, because a plan arrives from a language model and a
// dependency index it invented would otherwise become a ticket that can never become Ready.
func ValidatePlan(tickets []PlannedTicket) error {
	if len(tickets) == 0 {
		return fmt.Errorf("the plan has no tickets")
	}
	for i, t := range tickets {
		if t.Title == "" {
			return fmt.Errorf("ticket %d has no title", i+1)
		}
		seen := map[int]bool{}
		for _, d := range t.DependsOn {
			switch {
			case d < 0 || d >= len(tickets):
				return fmt.Errorf("ticket %d depends on %d, which is not in the plan", i+1, d+1)
			case d == i:
				return fmt.Errorf("ticket %d depends on itself", i+1)
			case seen[d]:
				return fmt.Errorf("ticket %d depends on %d twice", i+1, d+1)
			}
			seen[d] = true
		}
	}
	return planIsAcyclic(tickets)
}

// planIsAcyclic reports whether the plan's dependencies can actually be ordered.
//
// A cycle is not a theoretical concern: asked for several linked tickets, a model will
// cheerfully propose two that each wait on the other, and nothing downstream would ever notice
// — both tickets would simply sit in the backlog forever, blocked, with no error anywhere.
func planIsAcyclic(tickets []PlannedTicket) error {
	const (
		unvisited = 0
		active    = 1
		done      = 2
	)
	state := make([]int, len(tickets))

	var walk func(i int) error
	walk = func(i int) error {
		switch state[i] {
		case active:
			return fmt.Errorf("ticket %d is part of a dependency cycle", i+1)
		case done:
			return nil
		}
		state[i] = active
		for _, d := range tickets[i].DependsOn {
			if err := walk(d); err != nil {
				return err
			}
		}
		state[i] = done
		return nil
	}

	for i := range tickets {
		if err := walk(i); err != nil {
			return err
		}
	}
	return nil
}
