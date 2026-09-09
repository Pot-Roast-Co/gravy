package api

import (
	"context"
	"fmt"

	"github.com/pot-roast-co/gravy/internal/core"
)

// Planner runs planning conversations. Nil on a client that may not plan.
//
// The signature carries only core types so that the package implementing it — agentrun, where
// the providers and hosts live — does not have to import this one.
type Planner interface {
	Plan(ctx context.Context, turn core.PlanTurn) (core.PlanResult, error)
}

// PlanReq is one turn of a planning conversation.
type PlanReq struct {
	ProjectID string
	// Message is what the human said. Empty on a new conversation means "what should be next?".
	Message string
	// Session continues an existing conversation. Empty starts one.
	Session string
	// RunID is the client's correlation id for this turn. Progress is written to the run log
	// under it, so the client can StreamLogs it and watch rather than wait.
	RunID string
}

// PlanReply is the planner's answer.
type PlanReply struct {
	Session string
	// Agent names the provider and model running the conversation. Set when one starts.
	Agent   string
	Reply   string
	Tickets []core.PlannedTicket
}

// ApprovePlanReq turns a proposal into real tickets.
type ApprovePlanReq struct {
	ProjectID string
	Tickets   []core.PlannedTicket
	// Ready queues the tickets that have no unmet dependencies rather than leaving everything
	// in the backlog.
	Ready bool
}

// Plan takes one turn of a planning conversation.
func (l *Local) Plan(ctx context.Context, req PlanReq) (PlanReply, error) {
	if l.planner == nil {
		return PlanReply{}, fmt.Errorf("this client cannot plan")
	}
	project, err := l.db.GetProject(ctx, req.ProjectID)
	if err != nil {
		return PlanReply{}, fmt.Errorf("plan: %w", err)
	}

	// The planner is asked "what should be next" against work that already exists, so the
	// backlog travels with the question. Without it the answer is a guess about a repository
	// whose queue it cannot see.
	backlog, err := l.db.ListTickets(ctx, req.ProjectID)
	if err != nil {
		return PlanReply{}, fmt.Errorf("plan: %w", err)
	}

	res, err := l.planner.Plan(ctx, core.PlanTurn{
		Project: project, Backlog: backlog,
		Session: req.Session, Message: req.Message, RunID: req.RunID,
	})
	if err != nil {
		return PlanReply{Session: res.Session}, err
	}
	return PlanReply{Session: res.Session, Agent: res.Agent, Reply: res.Reply, Tickets: res.Tickets}, nil
}

// ApprovePlan writes an approved plan into the backlog.
//
// This is the gate. A planning conversation proposes and a human approves — nothing here is
// reachable from the planner itself, for the same reason review is advisory and detection only
// proposes: deciding what is worth building is the judgement being protected.
func (l *Local) ApprovePlan(ctx context.Context, req ApprovePlanReq) ([]core.Ticket, error) {
	if err := core.ValidatePlan(req.Tickets); err != nil {
		return nil, fmt.Errorf("approve plan: %w", err)
	}
	if _, err := l.db.GetProject(ctx, req.ProjectID); err != nil {
		return nil, fmt.Errorf("approve plan: %w", err)
	}

	// Created in plan order, which ValidatePlan has already shown to be acyclic. A ticket's
	// dependencies therefore always exist by the time it is created, so their ids are known.
	created := make([]core.Ticket, 0, len(req.Tickets))
	ids := make([]string, len(req.Tickets))

	for i, pt := range req.Tickets {
		deps := make([]string, 0, len(pt.DependsOn))
		for _, d := range pt.DependsOn {
			deps = append(deps, ids[d])
		}

		t, err := l.CreateTicket(ctx, CreateTicketReq{
			ProjectID: req.ProjectID,
			Title:     pt.Title,
			Body:      pt.Body,
			Route:     pt.Route,
			DependsOn: deps,
			// Only work that can actually start goes to Ready. Queueing a ticket that is
			// blocked on one of its own siblings would put it in a queue it cannot leave.
			Ready: req.Ready && len(deps) == 0,
		})
		if err != nil {
			// Partial failure leaves what was already written: the alternative is unwinding
			// tickets a human approved, and a half-created plan is visible in the backlog
			// where it can be finished by hand.
			return created, fmt.Errorf("approve plan: ticket %d (%s): %w", i+1, pt.Title, err)
		}
		ids[i] = t.ID
		created = append(created, t)
	}
	return created, nil
}
