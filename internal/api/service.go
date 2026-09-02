package api

import (
	"context"
	"time"

	"github.com/bobbybrady/gravy/internal/core"
)

// Service is everything a client can ask Gravy to do.
//
// It is the seam between the daemon and every client. The TUI holds no domain logic: it renders
// Service results and sends Service calls, and anything it needs that this cannot answer is a
// missing method here rather than a reason to reach into core.
//
// v0.1 implements the subset M0 needs. The remaining methods from ARCHITECTURE.md §8 arrive with
// the features that require them — StreamLogs with GR-011, GetReview with GR-020, Approve with
// GR-022 — rather than as stubs that lie about what works.
type Service interface {
	// projects
	ListProjects(ctx context.Context) ([]core.Project, error)
	AddProject(ctx context.Context, req AddProjectReq) (core.Project, error)

	// tickets
	ListTickets(ctx context.Context, f TicketFilter) ([]core.Ticket, error)
	CreateTicket(ctx context.Context, req CreateTicketReq) (core.Ticket, error)
	MoveTicket(ctx context.Context, id string, ev core.Event) (core.State, error)

	// runs
	ListRuns(ctx context.Context, ticketID string) ([]core.Run, error)

	// attention
	ListAttention(ctx context.Context) ([]core.Attention, error)
	ResolveAttention(ctx context.Context, id string) error

	// system
	Status(ctx context.Context) (SystemStatus, error)
	ExplainTicket(ctx context.Context, ticketID string) (Explanation, error)
}

// AddProjectReq registers a repository.
type AddProjectReq struct {
	Path string
	Name string
	// TargetBranch is resolved from the remote's HEAD when empty.
	TargetBranch string
	MergeMode    core.LandMode
	// Validation steps supplied on the command line, used by runs as given.
	Validation     []core.Step
	ParallelMode   bool
	MaxConcurrency int
}

// CreateTicketReq creates a ticket.
type CreateTicketReq struct {
	ProjectID string
	Title     string
	Body      string
	Route     core.Route
	Priority  int
	// Ready places the ticket straight into the queue rather than the backlog.
	Ready bool
	// DependsOn lists tickets that must reach Done first.
	DependsOn []string
}

// TicketFilter narrows a ticket listing. Zero values mean "no filter".
type TicketFilter struct {
	ProjectID string
	State     core.State
}

// SystemStatus is the one-screen answer to "what is happening".
type SystemStatus struct {
	Projects []ProjectStatus
	Hosts    []HostStatus
	// Attention is the open Needs You queue, oldest first.
	Attention []core.Attention
}

// ProjectStatus summarises one repository.
type ProjectStatus struct {
	Project core.Project
	// Counts is the number of tickets in each state.
	Counts map[core.State]int
	// Active is the ticket currently holding a serial project, if any.
	Active *core.Ticket
	// Blocked explains why the project's queue is idle, when it is.
	Blocked string
}

// HostStatus is one host's load.
type HostStatus struct {
	ID          string
	OS          string
	Arch        string
	UsedSlots   int
	TotalSlots  int
	Tools       map[string]string
	ProviderIDs []string
}

// Explanation answers "why is this ticket not running?".
type Explanation struct {
	TicketID string
	State    core.State
	Eligible bool
	Reason   string
	Why      []string
}

// RunSummary is a compact view of a run for listings.
type RunSummary struct {
	Run      core.Run
	Duration time.Duration
}
