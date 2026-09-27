package core

import (
	"fmt"
	"strings"
	"time"
)

// ActivityKind names what an entry in a ticket's history records.
//
// A short machine token rather than prose: clients group and filter on it, and the sentence a
// human reads lives in Activity.Detail. Adding a kind is adding a constant here — the store
// does not constrain the set, so an entry written by a newer binary still reads on an older one.
type ActivityKind string

// The run-narration kinds, in the order a run passes through them. They began life as the
// progress journal's phases and keep those string values, so rows written before the journal
// became the history still read as what they were.
const (
	// KindFetch is fetching the remote, before any worktree exists.
	KindFetch ActivityKind = "fetch"
	// KindWorktree is cutting — or reusing — the ticket's isolated worktree.
	KindWorktree ActivityKind = "worktree"
	// KindPrompt is assembling the agent's context.
	KindPrompt ActivityKind = "prompt"
	// KindAgentStart is the provider process starting.
	KindAgentStart ActivityKind = "agent_start"
	// KindAgentExit is that process finishing, with its classification and evidence.
	KindAgentExit ActivityKind = "agent_exit"
	// KindValidationStep is one validation command's result.
	KindValidationStep ActivityKind = "validation_step"
	// KindRetry is a self-correction attempt being spent.
	KindRetry ActivityKind = "retry"
	// KindSummary is the durable result record being written from the diff.
	KindSummary ActivityKind = "summary"
	// KindReview is the advisory review pass.
	KindReview ActivityKind = "review"
	// KindHandoff is where the ticket ended up, and who has it now.
	KindHandoff ActivityKind = "handoff"
)

// The kinds a human's action writes.
const (
	// KindCreated is the ticket being written down.
	KindCreated ActivityKind = "created"
	// KindQueued is the ticket being made eligible to run.
	KindQueued ActivityKind = "queued"
	// KindApproved is approval of work in Review, which starts landing it.
	KindApproved ActivityKind = "approved"
	// KindChangesRequested is work sent back from Review with a note.
	KindChangesRequested ActivityKind = "changes_requested"
	// KindRejected is the ticket being abandoned.
	KindRejected ActivityKind = "rejected"
	// KindRequeued is a parked ticket being put back in the queue.
	KindRequeued ActivityKind = "requeued"
	// KindContinued is a landing being retried after a human resolved what stopped it.
	KindContinued ActivityKind = "continued"
	// KindKilled is a live run being stopped.
	KindKilled ActivityKind = "killed"
	// KindMoved is any other transition a human applied by hand — a ticket withdrawn to the
	// backlog, a draft submitted. The payload names the event.
	KindMoved ActivityKind = "moved"
)

// The kinds Gravy writes at the steps that change what a ticket is or where it stands.
const (
	// KindCommit is the agent's work being committed in the worktree.
	KindCommit ActivityKind = "commit"
	// KindVerdict is an advisory review verdict being recorded. Advisory: it changes nothing.
	KindVerdict ActivityKind = "verdict"
	// KindParked is the ticket being moved into Needs You.
	KindParked ActivityKind = "parked"
	// KindCooldown is a model being taken out of the fleet for a while after a provider-side
	// failure.
	KindCooldown ActivityKind = "cooldown"
	// KindLanding is approved work starting its way onto the target branch.
	KindLanding ActivityKind = "landing"
	// KindLanded is the work reaching the target branch.
	KindLanded ActivityKind = "landed"
)

// AllActivityKinds lists the kinds this build writes: a run's narration in the order it reaches
// them, then the human and orchestrator kinds.
var AllActivityKinds = []ActivityKind{
	KindFetch, KindWorktree, KindPrompt, KindAgentStart, KindAgentExit,
	KindValidationStep, KindRetry, KindSummary, KindReview, KindHandoff,
	KindCreated, KindQueued, KindApproved, KindChangesRequested, KindRejected,
	KindRequeued, KindContinued, KindKilled, KindMoved,
	KindCommit, KindVerdict, KindParked, KindCooldown, KindLanding, KindLanded,
}

// Known reports whether k is one of the kinds this build writes.
//
// Deliberately not called Valid: an unrecognised kind read back from the database is an entry
// from another version, not a corrupt row, and nothing may refuse to display it.
func (k ActivityKind) Known() bool {
	for _, known := range AllActivityKinds {
		if known == k {
			return true
		}
	}
	return false
}

// Actors are plain strings with a fixed grammar, so a new kind of worker is a new prefix rather
// than a schema change:
//
//	human
//	gravy
//	agent:<provider>/<model>
//	worker:<provider>/<model>
const (
	// ActorHuman is the person driving Gravy.
	ActorHuman = "human"
	// ActorGravy is the orchestrator itself.
	ActorGravy = "gravy"

	actorAgentPrefix  = "agent:"
	actorWorkerPrefix = "worker:"
)

// AgentActor names a coding agent as an actor.
func AgentActor(providerID, model string) string {
	return actorAgentPrefix + providerID + "/" + model
}

// WorkerActor names a worker as an actor.
func WorkerActor(providerID, model string) string {
	return actorWorkerPrefix + providerID + "/" + model
}

// ActorRole is the part of an actor before any provider and model: human, gravy, agent or worker.
type ActorRole string

// The roles an actor can have.
const (
	ActorRoleHuman  ActorRole = "human"
	ActorRoleGravy  ActorRole = "gravy"
	ActorRoleAgent  ActorRole = "agent"
	ActorRoleWorker ActorRole = "worker"
)

// ParsedActor is an actor string taken apart.
type ParsedActor struct {
	Role ActorRole
	// Provider and Model are set for agents and workers, and empty otherwise.
	Provider string
	Model    string
}

// ParseActor takes an actor string apart, and refuses one that does not follow the grammar.
//
// The model half may itself contain slashes — some providers name models that way — so only the
// first slash separates it from the provider.
func ParseActor(s string) (ParsedActor, error) {
	switch s {
	case ActorHuman:
		return ParsedActor{Role: ActorRoleHuman}, nil
	case ActorGravy:
		return ParsedActor{Role: ActorRoleGravy}, nil
	}
	var (
		role ActorRole
		rest string
	)
	switch {
	case strings.HasPrefix(s, actorAgentPrefix):
		role, rest = ActorRoleAgent, strings.TrimPrefix(s, actorAgentPrefix)
	case strings.HasPrefix(s, actorWorkerPrefix):
		role, rest = ActorRoleWorker, strings.TrimPrefix(s, actorWorkerPrefix)
	default:
		return ParsedActor{}, fmt.Errorf("actor %q: not human, gravy, agent:… or worker:…", s)
	}
	providerID, model, ok := strings.Cut(rest, "/")
	if !ok || providerID == "" || model == "" {
		return ParsedActor{}, fmt.Errorf("actor %q: want %s:<provider>/<model>", s, role)
	}
	return ParsedActor{Role: role, Provider: providerID, Model: model}, nil
}

// String renders the actor back into its string form.
func (a ParsedActor) String() string {
	switch a.Role {
	case ActorRoleAgent:
		return AgentActor(a.Provider, a.Model)
	case ActorRoleWorker:
		return WorkerActor(a.Provider, a.Model)
	default:
		return string(a.Role)
	}
}

// Activity is one entry in a ticket's history — who did what to it, and when.
//
// The history began as a progress journal, because a state name answers "where is this ticket"
// and nothing else: fetch, worktree creation, prompt assembly, each validation step,
// self-correction retries and the advisory review all happen inside a single state. It now also
// carries every human action and every step that changes the ticket, so one append-only table
// answers both "what is it doing" and "how did it get here".
//
// Writing an entry never changes the ticket and never asks for attention. It is a record of what
// happened, not a way of making something happen.
type Activity struct {
	ID       string
	TicketID string
	// RunID is empty for the entries that happen before a run row exists — fetch, worktree and
	// the first prompt build — and for human actions on a ticket that has never run.
	RunID string
	At    time.Time
	Kind  ActivityKind
	// Actor is who did it, in the grammar ParseActor reads. Empty is written as gravy.
	Actor string
	// Detail is one sentence a human reads, carrying the facts they would otherwise have to dig
	// for: a worktree path, a pid, an exit class and its evidence, a step's exit code.
	Detail string
	// Payload is the same facts in a form a program can read. Never nil once read back.
	Payload map[string]any
}
