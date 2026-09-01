package provider

import (
	"context"
	"time"

	"github.com/bobbybrady/gravy/internal/core"
	"github.com/bobbybrady/gravy/internal/host"
)

// FailureClass is how a finished run is judged. It is core's enum, aliased so adapters can
// depend on this package alone.
type FailureClass = core.FailureClass

// The failure classes, re-exported for adapters.
const (
	Success             = core.Success
	TaskFailure         = core.TaskFailure
	QuotaExhausted      = core.QuotaExhausted
	RateLimited         = core.RateLimited
	ProviderUnavailable = core.ProviderUnavailable
	AuthExpired         = core.AuthExpired
	Timeout             = core.Timeout
	Unknown             = core.Unknown
)

// Provider is a coding agent CLI Gravy can drive.
//
// Adding one should mean adding a package and a registry entry, and changing nothing else. If it
// ever requires more than that, the abstraction has a bug.
type Provider interface {
	ID() string
	Detect(ctx context.Context, h host.Host) (Availability, error)
	Models(ctx context.Context) ([]Model, error)
	Run(ctx context.Context, h host.Host, t AgentTask) (Handle, error)
	Resume(ctx context.Context, h host.Host, s SessionRef, msg string) (Handle, error)
	Classify(exit int, stdout, stderr string) Classification
}

// Availability is what detection found on a host.
type Availability struct {
	Installed     bool
	Version       string
	Authenticated bool
	// Detail is human-readable and is shown in onboarding and in Needs You, so it should say
	// what to do rather than merely what is wrong.
	Detail string
}

// Model is one model a provider can run.
type Model struct {
	ID   string
	Name string
}

// SessionRef identifies a resumable conversation. It is opaque and provider-specific.
type SessionRef struct {
	ProviderID string
	ID         string
}

// Valid reports whether the reference can be resumed.
func (s SessionRef) Valid() bool { return s.ProviderID != "" && s.ID != "" }

// AgentTask is one unit of work handed to a provider.
type AgentTask struct {
	RunID        string
	WorktreePath string
	// Prompt is built by contextbuild. Agents receive a prompt, never a transcript.
	Prompt   string
	Model    string
	Timeout  time.Duration
	MaxTurns int
	LogPath  string
	// AskPath is where the agent writes a question or permission request, and is deliberately
	// outside the worktree (~/.gravy/runs/<run-id>/ask.json).
	//
	// An in-worktree file would be committed into the branch by the WIP commit that precedes
	// escalation, land in the merge, and dirty git status in exactly the worktree a human may
	// be resolving a conflict in. Gravy writes nothing into a repository, and the escalation
	// channel is not an exception.
	AskPath   string
	Allowlist core.Allowlist
}

// EventKind classifies a streamed progress event.
type EventKind string

// The event kinds. These are provider-agnostic: adapters translate their own output into them so
// the TUI renders one vocabulary regardless of which CLI produced it.
const (
	EventStarted    EventKind = "started"
	EventMessage    EventKind = "message"
	EventToolUse    EventKind = "tool_use"
	EventToolResult EventKind = "tool_result"
	EventThinking   EventKind = "thinking"
	EventUsage      EventKind = "usage"
	// EventRateLimit carries provider-reported quota telemetry, which can arrive on a
	// perfectly healthy run. It is strictly better than inferring quota state from failures
	// after the fact, because it comes with a real reset time rather than a guessed backoff.
	EventRateLimit EventKind = "rate_limit"
	EventError     EventKind = "error"
	EventFinished  EventKind = "finished"
)

// Event is a parsed progress event from a running agent.
type Event struct {
	Kind EventKind
	At   time.Time
	// Text is the human-readable payload: a message, a tool name, an error.
	Text string
	// Tool is the tool being used, for EventToolUse and EventToolResult.
	Tool string
	// Fields carries structured extras without forcing every provider's shape into this type.
	Fields map[string]any
	// Raw is the provider's original line, kept so events.jsonl is faithful and a parsing gap
	// is diagnosable after the fact.
	Raw string
}

// Handle is a running agent.
type Handle interface {
	// Events streams parsed progress. It is closed when the run ends.
	Events() <-chan Event
	Wait() (Outcome, error)
	Kill() error
}

// Outcome is how a run ended.
type Outcome struct {
	Class FailureClass
	// Note records the evidence for Class, so a misclassification is diagnosable rather than
	// mysterious.
	Note      string
	Session   SessionRef
	ExitCode  int
	Turns     int
	TokensIn  int
	TokensOut int
	// CostUSD is nil when the provider does not report cost.
	CostUSD *float64
	// TimedOut reports that the run was killed by its wall-clock timeout.
	TimedOut bool
}
