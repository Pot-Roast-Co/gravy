package api

import (
	"context"
	"time"

	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
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
	// Setup previews are read-only. ApplySetup is called only after explicit approval.
	SetupInfo(ctx context.Context) (SetupInfo, error)
	PreviewSetup(ctx context.Context, req AddProjectReq) (SetupPreview, error)
	ApplySetup(ctx context.Context, req SetupRequest) (Settings, error)

	// projects
	ListProjects(ctx context.Context, f ProjectFilter) ([]core.Project, error)
	AddProject(ctx context.Context, req AddProjectReq) (core.Project, error)
	UpdateProject(ctx context.Context, p core.Project) error
	// ArchiveProject takes a finished repository out of the working set, or puts it back.
	//
	// Its own method rather than a field UpdateProject writes: archiving is a state change
	// worth recording on purpose, and one that must not happen as a side effect of saving an
	// edit to a project's notes from a screen holding a stale copy of the flag.
	ArchiveProject(ctx context.Context, id string, archived bool) error
	// DeleteProject removes a project and, by cascade, its tickets, runs and attention rows.
	DeleteProject(ctx context.Context, id string) error

	// Rereview runs the advisory review again, for work whose verdict failed for a reason
	// since fixed.
	Rereview(ctx context.Context, ticketID string) error

	// review checkouts: a ticket's work as uncommitted changes, for reading in an editor
	ReviewCheckout(ctx context.Context, ticketID string) (string, error)
	DiscardReviewCheckout(ctx context.Context, ticketID string) error

	// DetectAgents reports which agent CLIs are installed and logged in, for onboarding.
	DetectAgents(ctx context.Context) []AgentStatus

	// planning
	Plan(ctx context.Context, req PlanReq) (PlanReply, error)
	ApprovePlan(ctx context.Context, req ApprovePlanReq) ([]core.Ticket, error)

	// settings
	GetSettings(ctx context.Context) (Settings, error)
	UpdateSettings(ctx context.Context, c config.Config) (Settings, error)

	// tickets
	ListTickets(ctx context.Context, f TicketFilter) ([]core.Ticket, error)
	CreateTicket(ctx context.Context, req CreateTicketReq) (core.Ticket, error)
	MoveTicket(ctx context.Context, id string, ev core.Event) (core.State, error)
	// ListQueue is ListTickets with dependencies resolved, for the screens that order work.
	ListQueue(ctx context.Context, f TicketFilter) ([]TicketDetail, error)
	UpdateTicket(ctx context.Context, t core.Ticket) error
	ReorderTicket(ctx context.Context, id, before, after string) error
	DeleteTicket(ctx context.Context, id string) error

	// runs
	ListRuns(ctx context.Context, ticketID string) ([]core.Run, error)
	// StreamLogs follows a run's output. Like Events, the stop function ends the
	// subscription and the channel closes when ctx does.
	StreamLogs(ctx context.Context, runID string) (<-chan LogLine, func(), error)
	KillRun(ctx context.Context, runID string) error

	// review
	GetReview(ctx context.Context, ticketID string) (ReviewBundle, error)
	// Approve lands reviewed work and returns the state it reached: Done when it merged,
	// NeedsYou when it parked. Parking is not an error, so the state is the only honest
	// answer to "did it land".
	Approve(ctx context.Context, ticketID string) (core.State, error)
	RequestChanges(ctx context.Context, ticketID, feedback string) error

	// change discussion — the agreement step in front of RequestChanges.
	//
	// Opening one, taking a turn in it and saving its draft all change nothing about the
	// ticket. SendChanges is the only call here that moves work, and it refuses to without an
	// explicit confirmation. RequestChanges stays for callers that already have their wording.
	OpenDiscussion(ctx context.Context, ticketID string) (DiscussionView, error)
	Discuss(ctx context.Context, req DiscussReq) (DiscussionView, error)
	SaveProposal(ctx context.Context, req ProposalReq) (DiscussionView, error)
	SendChanges(ctx context.Context, req SendChangesReq) error
	CancelDiscussion(ctx context.Context, ticketID string) error
	// Continue retries a landing after a human resolved a conflict in the preserved worktree.
	Continue(ctx context.Context, ticketID string) (core.State, error)
	Reject(ctx context.Context, ticketID string) error

	// attention
	ListAttention(ctx context.Context) ([]core.Attention, error)
	ResolveAttention(ctx context.Context, id string) error

	// system
	Status(ctx context.Context, f ProjectFilter) (SystemStatus, error)
	// ReconnectHost probes one configured host now and reports what it found. It is how a
	// machine that was off rejoins the fleet without restarting the daemon.
	ReconnectHost(ctx context.Context, id string) (HostStatus, error)
	ExplainTicket(ctx context.Context, ticketID string) (Explanation, error)
	// Events is the server-push stream. Clients render from it rather than polling; the
	// returned stop function ends the subscription, and the channel closes when ctx does.
	Events(ctx context.Context) (<-chan Event, func(), error)
}

// AddProjectReq registers a repository.
type AddProjectReq struct {
	// Non-nil means these are the human-approved permissions, including an empty list.
	Allowlist    *core.Allowlist
	Requirements core.Requirements
	Routes       map[core.Route][]core.Choice

	// Path is the working tree. Empty registers a project with no repository: a place for
	// goals and notes while the shape of the thing is still being decided.
	Path string
	Name string
	// Host pins the project to a machine. Empty means the local one.
	Host string
	// Notes are what the project is for. Optional.
	Notes string
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

// ProjectFilter narrows what a project listing — or the fleet snapshot — shows. The zero value
// is the working set.
type ProjectFilter struct {
	// IncludeArchived adds archived projects back in, for a caller looking at history rather
	// than at what is being worked on.
	IncludeArchived bool `json:"include_archived,omitempty"`
}

// TicketFilter narrows a ticket listing. Zero values mean "no filter".
type TicketFilter struct {
	ProjectID string
	State     core.State
}

// SystemStatus is the one-screen answer to "what is happening".
//
// It spans every project on purpose. Remembering which agent is on which repository is the exact
// pain Gravy removes, so the snapshot a dashboard renders is fleet-wide by construction rather
// than by the caller looping over projects and hoping the reads agree.
type SystemStatus struct {
	// Projects is the working set: archived projects are left out unless the caller asks for
	// them. Their in-flight work is not — archiving is not a kill switch, so a ticket that was
	// already running or awaiting review keeps its row in Running and Attention until it
	// reaches Done or Rejected, even though the project itself is no longer listed here.
	Projects []ProjectStatus
	Hosts    []HostStatus
	// Attention is the open Needs You queue, oldest first.
	Attention []AttentionItem
	// Running is every in-flight ticket, across every project.
	Running []RunningTicket
	// Ready is the queue across every project, in the order the scheduler would take it.
	Ready []QueuedTicket
	// Buckets are the configured route names, so a client can offer them rather than guess at
	// a list compiled into Gravy.
	Buckets []core.Route
}

// AttentionItem is one Needs You row, resolved against the ticket and project it concerns so a
// client never has to join them itself.
type AttentionItem struct {
	Attention core.Attention
	Project   core.Project
	Ticket    core.Ticket
	// Age is how long it has been waiting. A queue without ages hides the item that has been
	// ignored for two days behind the one raised a minute ago.
	Age time.Duration
}

// RunningTicket is one ticket the fleet is actively working.
type RunningTicket struct {
	Ticket  core.Ticket
	Project core.Project
	// Run is the current run. It is the zero value in the window between a ticket being
	// assigned and its run row existing.
	Run core.Run
	// Elapsed is how long the run has been going.
	Elapsed time.Duration
	// Activity is what it is doing now, phrased for a human rather than as a state name.
	Activity string
}

// QueuedTicket is one Ready ticket.
type QueuedTicket struct {
	Ticket  core.Ticket
	Project core.Project
	// Held explains why a Ready ticket will not start yet — most often its project's serial
	// cap. Empty means it is simply waiting its turn. An idle queue is always explained.
	Held string
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

// HostStatus is one host's load and whether it is answering.
type HostStatus struct {
	ID          string
	OS          string
	Arch        string
	UsedSlots   int
	TotalSlots  int
	Tools       map[string]string
	ProviderIDs []string
	// Online is whether the last probe reached the machine. A configured host that is switched
	// off reports false here forever rather than being quietly missing, because "yeet is off"
	// is the answer to "why is nothing happening on yeet".
	Online bool
	// Unreachable is ssh's own reason, when there is one.
	Unreachable string
	// CheckedAt is when that was last established; zero means it has not been probed yet.
	CheckedAt time.Time
	// Checking is true while a reconnect is in flight.
	Checking bool
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
