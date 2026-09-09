package core

import "time"

// Project is a registered repository and the policy Gravy applies to it.
type Project struct {
	ID   string
	Slug string
	Name string
	// RepoPath is the working tree on HostID. Empty is a project with no repository yet: a
	// place to keep goals and notes while the shape of the thing is still being decided.
	// Nothing can run in one, and the scheduler says so rather than failing a ticket in it.
	RepoPath string
	// Notes are what this project is for — goals, constraints, decisions. They are read by
	// planning, which is the difference between proposing work for a repository and proposing
	// work for a project someone actually has intentions about.
	Notes        string
	TargetBranch string
	MergeMode    LandMode
	// HostID pins the project to one machine. Empty means any host that meets Requirements.
	//
	// A project belongs to a machine because its clone does: each host has its own checkout
	// and its own worktrees, with no shared filesystem, so RepoPath means nothing anywhere
	// else. An Xcode project on a Mac is the case this exists for.
	HostID string

	// Requirements a host must satisfy to run this project's tickets (OS, tools).
	Requirements Requirements
	// Validation steps run inside the worktree, in order.
	Validation []Step
	// Allowlist is what an agent may do here without asking.
	Allowlist Allowlist
	// Routes overrides the global route table for this project.
	Routes map[Route][]Choice

	// ParallelMode opts this project out of the conservative default of one ticket in flight
	// through merge. Conflicts become possible and are the human's to handle.
	ParallelMode bool
	// MaxConcurrency is consulted only when ParallelMode is set.
	MaxConcurrency int

	CreatedAt time.Time
}

// Requirements constrain which hosts can run a project's or a ticket's work.
type Requirements struct {
	OS    []string          // empty means any
	Tools map[string]string // name -> minimum version, "" for any version
}

// Step is one validation command.
type Step struct {
	Name     string // build, test, lint, typecheck
	Cmd      string
	Timeout  time.Duration
	Required bool // a failed optional step warns; it does not fail the run
}

// Allowlist is what an agent may do in a project without escalating. Everything not permitted
// here stops the agent and asks the human; nothing is ever auto-granted.
type Allowlist struct {
	ReadPaths  []string // globs, relative to the worktree
	WritePaths []string
	Commands   []Pattern
	Network    bool
}

// Pattern is an allowed shell command, as a glob or a ^-prefixed regex.
type Pattern struct {
	Match string
	// Note is shown in Needs You when this rule is what allowed an action, so a human can see
	// why something was permitted without reading configuration.
	Note string
}

// Choice is a concrete provider and model a route can resolve to.
type Choice struct {
	ProviderID string
	Model      string
	// Why records how this choice was reached, surfaced verbatim in the TUI. Every automated
	// decision in Gravy must be answerable.
	Why []string
}

// Constraints narrow a route resolution to the context it is happening in.
//
// ARCHITECTURE.md names this type with a HostID and an Exclude list; Exclude belongs to the
// retry path and is not built yet. Routes is here because a project's own bucket table has to
// reach the router somehow, and the alternative — handing the router a store and a project id —
// would make it read the database on a path the scheduler has already read it on.
type Constraints struct {
	// HostID is the machine the work was placed on.
	HostID string
	// Routes is the project's bucket table. A bucket it names replaces the global entry for
	// that bucket outright — the project's list is the whole preference order, not a prefix of
	// the global one. A bucket it does not name falls through to the global table.
	Routes map[Route][]Choice
}

// Ticket is a unit of work.
type Ticket struct {
	ID        string
	ProjectID string
	Title     string
	Body      string
	State     State
	Priority  int
	// Position orders the queue. It is fractional so reordering is a single row update
	// (average the neighbours) rather than renumbering the queue.
	Position float64
	Route    Route

	Requirements Requirements
	// HostOverride is an explicit human choice and is honoured unconditionally.
	HostOverride string

	WorktreePath string
	Branch       string
	// RetryCount is the self-correction budget consumed so far.
	RetryCount int
	// Feedback is what a reviewer wrote when sending the work back. It is carried into the
	// next attempt's prompt and cleared once the ticket is approved or rejected — an agent
	// asked to try again with no new information will usually produce the same output.
	Feedback string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Run is one execution of an agent against a ticket.
type Run struct {
	ID         string
	TicketID   string
	HostID     string
	ProviderID string
	Model      string
	// SessionRef is the provider's opaque session identifier, and is what makes resuming a
	// blocked ticket possible rather than restarting it.
	SessionRef string
	State      State

	FailureClass FailureClass
	// FailureNote records the evidence that produced FailureClass, so a misclassification is
	// diagnosable rather than mysterious.
	FailureNote string

	PID       int
	Turns     int
	TokensIn  int
	TokensOut int
	// CostUSD is nil when the provider does not report cost.
	CostUSD *float64

	StartedAt time.Time
	EndedAt   *time.Time

	// Verdict is the automated reviewer's advisory opinion as JSON, empty when none was
	// produced. It annotates the diff for a human and never decides anything.
	Verdict string
}

// Summary is the durable account of what a run changed.
type Summary struct {
	TicketID string
	RunID    string
	Branch   string
	Commits  []string
	Files    []FileChange
	// Narrative is markdown generated from the diff — never from the agent's transcript. A
	// self-report from the party with a motive to declare success is not evidence.
	Narrative string
	// Assumptions the agent recorded while working.
	Assumptions []string
	CreatedAt   time.Time
}

// FileChange is one file's contribution to a diff.
type FileChange struct {
	Path      string
	Status    string // added, modified, deleted, renamed
	Additions int
	Deletions int
}

// Attention is one entry in the Needs You queue.
type Attention struct {
	ID        string
	ProjectID string
	TicketID  string
	RunID     string
	Reason    AttentionReason
	// Payload carries whatever the reason needs to be acted on without hunting: the question
	// and its options, the requested command, the conflicting paths, diff stats.
	Payload   map[string]any
	Resolved  bool
	CreatedAt time.Time
}

// Caps describes what a host can do. Hosts are filtered against project and ticket
// Requirements before a ticket is assigned.
type Caps struct {
	OS        string // darwin, linux, windows
	Arch      string
	RAMBytes  uint64
	GPU       string
	Tools     map[string]string // name -> version
	Providers map[string]bool   // provider id -> installed
}

// ProviderAvailability records a model cooled down after a genuine provider-side failure.
type ProviderAvailability struct {
	ProviderID string
	Model      string
	Class      FailureClass
	// Until is when the cooldown expires. A provider-reported reset time is preferred over a
	// guessed backoff whenever one is available.
	Until time.Time
	Note  string
}
