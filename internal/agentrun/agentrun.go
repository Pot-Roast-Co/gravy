package agentrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pot-roast-co/gravy/internal/contextbuild"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/git"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/notify"
	"github.com/pot-roast-co/gravy/internal/provider"
	"github.com/pot-roast-co/gravy/internal/review"
	"github.com/pot-roast-co/gravy/internal/runlog"
	"github.com/pot-roast-co/gravy/internal/validate"
)

// Store is what the orchestrator needs from persistence.
type Store interface {
	GetTicket(ctx context.Context, id string) (core.Ticket, error)
	UpdateTicket(ctx context.Context, t core.Ticket) error
	SetTicketState(ctx context.Context, id string, ev core.Event) (core.State, error)
	GetProject(ctx context.Context, id string) (core.Project, error)
	GetRun(ctx context.Context, id string) (core.Run, error)
	// ListRunsForTicket returns a ticket's runs, newest first. Re-running the advisory review
	// needs the run its verdict is recorded on.
	ListRunsForTicket(ctx context.Context, ticketID string) ([]core.Run, error)
	CreateRun(ctx context.Context, r core.Run) error
	UpdateRun(ctx context.Context, r core.Run) error
	AddValidation(ctx context.Context, id, runID, step string, exitCode int, durationMS int64, logPath string) error
	// AddProgress appends one entry to a ticket's journal. It is how every phase boundary
	// becomes something a human can read without opening a log.
	AddProgress(ctx context.Context, p core.Progress) error
	OpenAttention(ctx context.Context, a core.Attention) error
	ResolveAttentionForTicket(ctx context.Context, ticketID string) (int, error)
	SetProviderUnavailable(ctx context.Context, a core.ProviderAvailability) error
	// ListChangeInstructions returns what a human has agreed for this ticket, oldest first.
	// The prompt carries all of them, because the preservation constraints of the first
	// correction are still binding during the third.
	ListChangeInstructions(ctx context.Context, ticketID string) ([]core.ChangeInstruction, error)
}

// Repos supplies a git repository manager per project.
type Repos interface {
	For(project core.Project) (Repo, error)
}

// Repo is the git surface the orchestrator uses.
type Repo interface {
	// Host is the machine the repository and its worktrees are on, and so the only machine
	// on which a command can be run inside one of them.
	Host() host.Host
	Fetch(ctx context.Context) error
	// TargetRef resolves the target branch to the ref holding freshly-fetched state.
	TargetRef(ctx context.Context, branch string) (string, error)
	CreateWorktree(ctx context.Context, branch, base string) (git.Worktree, error)
	OpenWorktree(ctx context.Context, branch string) (git.Worktree, bool, error)
	RemoveWorktree(ctx context.Context, w git.Worktree) error
	CommitAll(ctx context.Context, w git.Worktree, msg string) (string, error)
	Diff(ctx context.Context, w git.Worktree, base string) (git.Diff, error)
}

// Slots is the worker pool a run claims from.
type Slots interface {
	TryClaim() bool
	Release()
}

// PromptBuilder assembles the prompt for an attempt.
//
// GR-015 supplies the real context builder. Until then a minimal builder ships (see
// SimplePrompt), behind this interface so the orchestrator is written once.
type PromptBuilder interface {
	Build(ctx context.Context, t core.Ticket, p core.Project, attempt Attempt) (string, error)
}

// Attempt describes which try this is, and why the last one failed.
type Attempt struct {
	// Number is 1 for the first attempt.
	Number int
	// PriorFailure is the validation output from the previous attempt, empty on the first.
	PriorFailure string
	// Agreed are the change instructions a human confirmed for this ticket, oldest first.
	//
	// They are passed rather than looked up so that the prompt builder stays free of I/O, and
	// they are a list rather than the latest one because every round's preservation constraints
	// remain binding — losing an earlier one is precisely how a narrow correction undoes work
	// that was already accepted.
	Agreed []core.ChangeInstruction
}

// IDGen generates run identifiers.
type IDGen func() string

// Config holds the orchestrator's tunables.
type Config struct {
	// SelfCorrectionBudget is how many extra attempts a red validation gets before the ticket
	// parks in Needs You. Zero means one attempt and no retries.
	SelfCorrectionBudget int
	// RunTimeout caps a single agent run.
	RunTimeout time.Duration
	// MaxTurns caps agent turns.
	MaxTurns int
	// RunsDir is where per-run artefacts live, normally ~/.gravy/runs.
	RunsDir string
	// Home is the gravy home, normally ~/.gravy. It is where a project with no repository
	// gets a directory to be planned in.
	Home string
	// CooldownQuota, CooldownRateLimit and CooldownUnavailable are how long a model is
	// considered unavailable after each kind of provider-side failure.
	CooldownQuota       time.Duration
	CooldownRateLimit   time.Duration
	CooldownUnavailable time.Duration
	// MaxRouteAttempts bounds how many times a run re-resolves its route after provider-side
	// failures, so an entirely unavailable provider cannot spin forever.
	MaxRouteAttempts int
}

// Orchestrator executes one ticket from Ready to Reviewing.
type Orchestrator struct {
	store        Store
	repos        Repos
	hosts        map[string]host.Host
	providers    map[string]provider.Provider
	slots        Slots
	newValidator func(runID string) validate.Runner
	prompts      PromptBuilder
	cfg          Config
	newID        IDGen
	log          *slog.Logger
	// logs records each run's output. Nil is legitimate: a run without a log is still a run.
	logs *runlog.Store
	// reviewer produces the advisory verdict. Nil disables the pass entirely.
	reviewer *review.Reviewer
	// notifier tells the human when a ticket lands in the Needs You queue. Nil is legitimate:
	// the item is in the queue whether or not a ping goes out.
	notifier Notifier

	mu   sync.Mutex
	live map[string]*liveRun
}

// liveRun tracks an executing run so it can be killed, and so a client can see when it last
// said anything.
type liveRun struct {
	cancel context.CancelFunc
	handle provider.Handle
	// lastEvent is the unix-nanosecond time of the run's latest event, or of its start
	// before it has produced one. In memory only: after a restart there is no live run to ask.
	lastEvent atomic.Int64
}

// New returns an orchestrator.
func New(s Store, repos Repos, slots Slots, newValidator func(runID string) validate.Runner, p PromptBuilder, cfg Config, newID IDGen) *Orchestrator {
	if cfg.MaxRouteAttempts < 1 {
		cfg.MaxRouteAttempts = 3
	}
	return &Orchestrator{
		store:        s,
		repos:        repos,
		hosts:        map[string]host.Host{},
		providers:    map[string]provider.Provider{},
		slots:        slots,
		newValidator: newValidator,
		prompts:      p,
		cfg:          cfg,
		newID:        newID,
		log:          slog.Default(),
		live:         map[string]*liveRun{},
	}
}

// SetLogger replaces the orchestrator's logger.
func (o *Orchestrator) SetLogger(l *slog.Logger) { o.log = l }

// anyHost returns a registered host, for work not tied to a specific run.
func (o *Orchestrator) anyHost() host.Host {
	for _, h := range o.hosts {
		return h
	}
	return nil
}

// RegisterHost makes a host available to runs.
func (o *Orchestrator) RegisterHost(h host.Host) { o.hosts[h.ID()] = h }

// RegisterProvider makes a provider available to runs.
func (o *Orchestrator) RegisterProvider(p provider.Provider) { o.providers[p.ID()] = p }

// Assignment is what the scheduler decided.
type Assignment struct {
	TicketID   string
	HostID     string
	ProviderID string
	Model      string
}

// Result is how a ticket's run ended.
type Result struct {
	TicketID string
	RunID    string
	// FinalState is the ticket's state when the orchestrator finished with it.
	FinalState core.State
	Attempts   int
	Validation validate.Results
	Worktree   git.Worktree
	// Commit is the hash of the agent's work, empty when it changed nothing.
	Commit string
}

// Run executes one ticket end to end.
//
// The worker slot is released on every exit path, including a panic in a provider adapter: a
// leaked slot permanently shrinks the pool, and the failure is invisible until the queue
// mysteriously stops moving.
func (o *Orchestrator) Run(ctx context.Context, a Assignment) (res Result, err error) {
	if !o.slots.TryClaim() {
		return Result{}, errors.New("agentrun: no worker slot available")
	}
	released := false
	release := func() {
		if !released {
			released = true
			o.slots.Release()
		}
	}
	defer func() {
		if p := recover(); p != nil {
			release()
			err = fmt.Errorf("agentrun: panic during run of %q: %v", a.TicketID, p)
		}
	}()
	defer release()

	res, err = o.run(ctx, a)
	if err != nil {
		// A run that fails before the agent ever starts — a worktree that will not open, a
		// fetch that fails, a provider that is not registered — would otherwise leave the
		// ticket in Assigned: holding its project's serial slot, absent from Needs You, and
		// invisible. That is the exact outcome the queue exists to prevent, so the failure
		// is parked here rather than merely logged by the caller.
		res.FinalState = o.parkFailedStart(ctx, a.TicketID, err)
	}
	return res, err
}

// parkFailedStart moves a ticket that never got going into Needs You, and reports where it
// ended up. It is best-effort: the caller is already returning an error, and failing to park is
// not a reason to lose that error.
func (o *Orchestrator) parkFailedStart(ctx context.Context, ticketID string, cause error) core.State {
	ticket, gerr := o.store.GetTicket(ctx, ticketID)
	if gerr != nil {
		return ""
	}
	// Already settled, or already waiting on a human: leave it alone.
	if !core.IsActive(ticket.State) || core.NeedsHuman(ticket.State) {
		return ticket.State
	}

	state, perr := o.park(ctx, ticket, "", core.ReasonHostUnavailable, map[string]any{
		"reason": "the run could not be started",
		"detail": cause.Error(),
	})
	if perr != nil {
		o.log.Error("could not park a ticket whose run failed to start",
			"ticket", ticketID, "error", perr)
		return ticket.State
	}
	return state
}

func (o *Orchestrator) run(ctx context.Context, a Assignment) (Result, error) {
	res := Result{TicketID: a.TicketID}

	ticket, err := o.store.GetTicket(ctx, a.TicketID)
	if err != nil {
		return res, fmt.Errorf("agentrun: %w", err)
	}
	project, err := o.store.GetProject(ctx, ticket.ProjectID)
	if err != nil {
		return res, fmt.Errorf("agentrun: %w", err)
	}
	h, ok := o.hosts[a.HostID]
	if !ok {
		return res, fmt.Errorf("agentrun: host %q is not registered", a.HostID)
	}

	// Ready -> Assigned -> Running, through the state machine so an illegal path is caught.
	if _, err := o.store.SetTicketState(ctx, ticket.ID, core.EventAssign); err != nil {
		return res, fmt.Errorf("agentrun: %w", err)
	}

	repo, err := o.repos.For(project)
	if err != nil {
		return res, fmt.Errorf("agentrun: %w", err)
	}

	// Fetch, THEN cut the worktree. Per ticket, at claim time — never a batch fetch.
	//
	// This ordering is the whole reason queued work builds on whatever merged before it. A
	// worktree cut from a stale target silently omits the previous ticket's work, and the
	// agent then reimplements or conflicts with it.
	o.note(ctx, ticket.ID, "", core.PhaseFetch,
		"fetching origin so the work starts on top of the latest %s", project.TargetBranch)
	if err := repo.Fetch(ctx); err != nil {
		return res, fmt.Errorf("agentrun: fetch %s: %w", project.Slug, err)
	}

	// The base is the remote-tracking ref, not the local branch. A local branch does not move
	// when you fetch, so cutting from it would hand the agent yesterday's target and undo the
	// entire point of fetching per ticket at claim time.
	base, err := repo.TargetRef(ctx, project.TargetBranch)
	if err != nil {
		return res, fmt.Errorf("agentrun: %w", err)
	}
	branch := git.BranchName(ticket.ID, ticket.Title)

	// A ticket sent back for another attempt already has a worktree, and preserving it is the
	// entire point of sending work back rather than restarting it. Creating one unconditionally
	// fails on the existing branch, which left a requeued ticket permanently unable to run.
	wt, reused, err := repo.OpenWorktree(ctx, branch)
	if err != nil {
		return res, fmt.Errorf("agentrun: %w", err)
	}
	if !reused {
		if wt, err = repo.CreateWorktree(ctx, branch, base); err != nil {
			return res, fmt.Errorf("agentrun: %w", err)
		}
		o.note(ctx, ticket.ID, "", core.PhaseWorktree,
			"worktree cut at %s on %s, based on %s", wt.Path, wt.Branch, base)
	} else {
		o.log.Info("continuing in the existing worktree", "ticket", ticket.ID, "branch", branch)
		o.note(ctx, ticket.ID, "", core.PhaseWorktree,
			"continuing in the existing worktree at %s on %s", wt.Path, wt.Branch)
	}
	res.Worktree = wt

	if err := o.updateTicketFields(ctx, ticket.ID, func(t *core.Ticket) {
		t.WorktreePath = wt.Path
		t.Branch = wt.Branch
	}); err != nil {
		return res, fmt.Errorf("agentrun: %w", err)
	}

	loop, err := o.attemptLoop(ctx, ticket, project, h, a, repo, wt)
	res.Attempts, res.Validation, res.Commit, res.RunID = loop.attempts, loop.validation, loop.commit, loop.runID
	if err != nil {
		return res, err
	}

	// A provider-side failure has already requeued the ticket or parked it for attention. Falling through here
	// would tell the state machine that validation passed on a ticket that never validated.
	if loop.parked {
		current, gerr := o.store.GetTicket(ctx, ticket.ID)
		if gerr != nil {
			return res, fmt.Errorf("agentrun: %w", gerr)
		}
		res.FinalState = current.State
		return res, nil
	}

	// A run that produced no diff and did not report success has not done the work, whatever
	// the validation says. Validation on an unchanged tree passes trivially — it is testing
	// the code that was already there — so treating that as a green ticket would send an empty
	// diff to review described as "no changes" when the agent was actually blocked.
	if loop.commit == "" && loop.lastClass != provider.Success {
		state, aerr := o.park(ctx, ticket, loop.runID, core.ReasonValidationFailed, map[string]any{
			"attempts": loop.attempts,
			"reason":   "the agent produced no changes",
			"note":     loop.lastNote,
		})
		res.FinalState = state
		return res, aerr
	}

	if !loop.validation.Green() {
		// Budget exhausted and still red: park it visibly rather than hanging.
		state, aerr := o.park(ctx, ticket, loop.runID, core.ReasonValidationFailed, map[string]any{
			"attempts": loop.attempts,
			"summary":  loop.validation.Summary(),
		})
		res.FinalState = state
		return res, aerr
	}

	if _, err := o.store.SetTicketState(ctx, ticket.ID, core.EventValidationPassed); err != nil {
		return res, fmt.Errorf("agentrun: %w", err)
	}

	// The result record, from the diff and the validation evidence. Never from what the agent
	// said about itself: a self-report from the party with a motive to declare success is not
	// evidence, and the journal is read as fact by whoever picks this ticket up.
	o.note(ctx, ticket.ID, loop.runID, core.PhaseSummary,
		"recording the result from the diff: commit %s on %s after %s, validation green",
		shortCommit(loop.commit), wt.Branch, attemptsWord(loop.attempts))

	// The advisory review runs here, between Reviewing and Review. Nothing below reads its
	// verdict: it annotates the diff for a human and never decides the ticket's fate. Leaving
	// the ticket in Reviewing would strand it — no human action moves it, and its project
	// stays held forever — so the transition happens whatever the reviewer said, or did not.
	o.runReview(ctx, ticket, project, repo, wt, loop)

	state, err := o.store.SetTicketState(ctx, ticket.ID, core.EventReviewed)
	if err != nil {
		return res, fmt.Errorf("agentrun: %w", err)
	}

	// A ticket in Review is waiting on a human, so it belongs in the Needs You queue. Review is
	// the most common reason Gravy needs you, and omitting it makes "if it is not there, Gravy
	// does not need you" false in the ordinary case rather than an edge one.
	if err := o.raiseAttention(ctx, core.Attention{
		ID:        o.newID(),
		ProjectID: ticket.ProjectID,
		TicketID:  ticket.ID,
		RunID:     loop.runID,
		Reason:    core.ReasonReviewPending,
		Payload: map[string]any{
			"commit":   loop.commit,
			"attempts": loop.attempts,
			"summary":  loop.validation.Summary(),
		},
		CreatedAt: time.Now(),
	}, ticket.Title); err != nil {
		return res, fmt.Errorf("agentrun: open review attention: %w", err)
	}

	o.note(ctx, ticket.ID, loop.runID, core.PhaseHandoff, "moved to Review — waiting on you")

	res.FinalState = state
	return res, nil
}

// attemptsWord renders an attempt count as something that reads as a sentence.
func attemptsWord(n int) string {
	if n == 1 {
		return "1 attempt"
	}
	return fmt.Sprintf("%d attempts", n)
}

// loopResult is what the attempt loop produced.
type loopResult struct {
	attempts   int
	validation validate.Results
	commit     string
	runID      string
	// parked reports that the loop already moved the ticket to Needs You, so the caller must
	// not apply any further transition.
	parked bool
	// lastClass is the final attempt's outcome, needed to tell "nothing to do" apart from
	// "was prevented from doing anything".
	lastClass core.FailureClass
	lastNote  string
}

// attemptLoop runs the agent and validation, retrying within the self-correction budget.
func (o *Orchestrator) attemptLoop(ctx context.Context, ticket core.Ticket, project core.Project, h host.Host, a Assignment, repo Repo, wt git.Worktree) (loopResult, error) {
	var (
		res          loopResult
		priorFailure string
	)

	// Read once, before the first attempt: what a human agreed does not change while the agent
	// is working, and a failure to read it must not be discovered halfway through a retry.
	agreed, err := o.store.ListChangeInstructions(ctx, ticket.ID)
	if err != nil {
		return res, fmt.Errorf("agentrun: read agreed changes for %s: %w", ticket.ID, err)
	}

	maxAttempts := o.cfg.SelfCorrectionBudget + 1
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		res.attempts = attempt
		runID, outcome, err := o.executeAgent(ctx, ticket, project, h, a, wt, Attempt{
			Number:       attempt,
			PriorFailure: priorFailure,
			Agreed:       agreed,
		})
		res.runID = runID
		res.lastClass, res.lastNote = outcome.Class, outcome.Note
		if err != nil {
			return res, err
		}

		if outcome.Class != provider.Success {
			// A provider-side failure is not the agent's fault and must not consume a
			// self-correction retry. The two budgets are independent: quota has nothing to
			// do with whether the agent can fix its own build error.
			if outcome.Class.IsQuotaCondition() {
				res.parked = true
				return res, o.handleProviderFailure(ctx, ticket, a, outcome)
			}
			// An ordinary task failure: fall through so validation records the state and
			// the retry budget applies.
		}

		// Running -> Validating. Every state change goes through the transition table, so a
		// missing edge fails loudly here rather than corrupting the lifecycle silently.
		if _, err := o.store.SetTicketState(ctx, ticket.ID, core.EventAgentFinished); err != nil {
			return res, fmt.Errorf("agentrun: %w", err)
		}

		// Commit whatever the agent produced, so the diff is durable even if the next step
		// fails. An agent that changed nothing returns an empty hash, which is not an error.
		//
		// Except when the agent never started. A failed run with no turns, no denials and no
		// session did nothing at all, so it has nothing to commit — and committing anyway
		// sweeps up whatever else is in the worktree.
		//
		// Observed: a run misrouted to another machine started nothing, then committed 650,000
		// lines of Erlang crash dumps left behind by a human who had run the test suite in
		// that worktree, burying the real diff beneath them. A denied run is deliberately not
		// covered here: being refused one tool is not the same as never running, and such a
		// run often produced the work anyway.
		if outcome.Class != provider.Success && outcome.Turns == 0 &&
			len(outcome.Denials) == 0 && outcome.Session.ID == "" {
			res.lastNote = outcome.Note
			return res, fmt.Errorf("agentrun: the agent never started: %s", outcome.Note)
		}

		c, err := repo.CommitAll(ctx, wt, "gravy: "+ticket.ID+" attempt "+fmt.Sprint(attempt))
		if err != nil {
			return res, fmt.Errorf("agentrun: %w", err)
		}
		if c != "" {
			res.commit = c
		}

		res.validation, err = o.runValidation(ctx, h, ticket, project, wt, runID)
		if err != nil {
			return res, err
		}

		// The diff and the validation results are evidence; the provider's classification is
		// a report about itself. When they disagree, trust the evidence.
		//
		// Observed: an agent was denied one exploratory `find | grep`, worked around it,
		// produced correct code, and validation passed — and the run was still marked failed,
		// costing a retry and double the tokens for nothing. A denial matters when the work
		// did not happen; here it demonstrably did.
		if res.validation.Green() && (outcome.Class == provider.Success || res.commit != "") {
			return res, nil
		}

		// Feed the failure back to the next attempt, which is the entire point of the budget.
		priorFailure = failureContext(outcome, res.validation)

		if attempt < maxAttempts {
			o.note(ctx, ticket.ID, runID, core.PhaseRetry,
				"attempt %d of %d did not pass; retrying with the failure in the prompt",
				attempt, maxAttempts)
			retry := attempt
			if err := o.updateTicketFields(ctx, ticket.ID, func(t *core.Ticket) {
				t.RetryCount = retry
			}); err != nil {
				return res, fmt.Errorf("agentrun: %w", err)
			}
			if _, err := o.store.SetTicketState(ctx, ticket.ID, core.EventValidationRetry); err != nil {
				return res, fmt.Errorf("agentrun: %w", err)
			}
		}
	}
	return res, nil
}

// executeAgent performs one agent run and records it.
func (o *Orchestrator) executeAgent(ctx context.Context, ticket core.Ticket, project core.Project, h host.Host, a Assignment, wt git.Worktree, attempt Attempt) (string, provider.Outcome, error) {
	p, ok := o.providers[a.ProviderID]
	if !ok {
		return "", provider.Outcome{}, fmt.Errorf("agentrun: provider %q is not registered", a.ProviderID)
	}

	maxAttempts := o.cfg.SelfCorrectionBudget + 1

	prompt, err := o.prompts.Build(ctx, ticket, project, attempt)
	if err != nil {
		return "", provider.Outcome{}, fmt.Errorf("agentrun: build prompt: %w", err)
	}
	// Before the run row exists, so the entry carries no run id — which is the whole reason the
	// journal hangs off the ticket.
	o.note(ctx, ticket.ID, "", core.PhasePrompt,
		"prompt built for attempt %d of %d: about %d tokens",
		attempt.Number, maxAttempts, contextbuild.Tokens(prompt))

	runID := o.newID()
	runDir := filepath.Join(o.cfg.RunsDir, runID)

	run := core.Run{
		ID: runID, TicketID: ticket.ID, HostID: a.HostID, ProviderID: a.ProviderID,
		Model: a.Model, State: core.StateRunning, StartedAt: time.Now(),
	}
	if err := o.store.CreateRun(ctx, run); err != nil {
		return runID, provider.Outcome{}, fmt.Errorf("agentrun: %w", err)
	}

	// The first attempt moves Assigned -> Running; later attempts are already Running.
	if attempt.Number == 1 {
		if _, err := o.store.SetTicketState(ctx, ticket.ID, core.EventStart); err != nil {
			return runID, provider.Outcome{}, fmt.Errorf("agentrun: %w", err)
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	handle, err := p.Run(runCtx, h, provider.AgentTask{
		RunID:        runID,
		WorktreePath: wt.Path,
		Prompt:       prompt,
		Model:        a.Model,
		Timeout:      o.cfg.RunTimeout,
		MaxTurns:     o.cfg.MaxTurns,
		LogPath:      filepath.Join(runDir, "agent.log"),
		AskPath:      filepath.Join(runDir, "ask.json"),
		Allowlist:    effectiveAllowlist(project),
	})
	if err != nil {
		return runID, provider.Outcome{}, fmt.Errorf("agentrun: start agent: %w", err)
	}
	started := time.Now()
	o.note(ctx, ticket.ID, runID, core.PhaseAgentStart, "%s/%s started%s, attempt %d of %d",
		a.ProviderID, a.Model, pidNote(pidOf(handle)), attempt.Number, maxAttempts)

	lr := &liveRun{cancel: cancel, handle: handle}
	lr.lastEvent.Store(started.UnixNano())
	o.trackLive(ticket.ID, lr)
	defer o.untrackLive(ticket.ID)

	// Events must be drained or the provider stalls once its buffer fills, so this goroutine
	// exists whether or not anything is recording them.
	var logw *runlog.Writer
	if o.logs != nil {
		if logw, err = o.logs.Open(runID); err != nil {
			// Losing the log is not worth losing the run over.
			logw = nil
		}
	}
	counts := newLiveCounts(o.store, run)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		ticker := time.NewTicker(liveEvery)
		defer ticker.Stop()
		events := handle.Events()
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					return
				}
				lr.lastEvent.Store(time.Now().UnixNano())
				if logw != nil {
					_ = logw.WriteEvent(ev)
				}
				counts.observe(ctx, ev)
			case <-ticker.C:
				counts.tick(ctx)
			}
		}
	}()

	outcome, waitErr := handle.Wait()

	// Wait returning means the run is over, so the event channel is closing. The timeout is
	// for the provider that does not honour that: a stuck drain must not wedge the worker.
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
	}
	// Whatever the drain is still doing, it may not write the row after the final write does.
	counts.stop()
	if logw != nil {
		_ = logw.Close()
	}
	ended := time.Now()

	o.note(ctx, ticket.ID, runID, core.PhaseAgentExit, "%s/%s exited %s after %d turns in %s%s",
		a.ProviderID, a.Model, outcome.Class.String(), outcome.Turns,
		took(ended.Sub(started)), evidence(outcome.Note))

	run.State = core.StateValidating
	run.FailureClass = outcome.Class
	run.FailureNote = outcome.Note
	run.SessionRef = outcome.Session.ID
	run.Turns = outcome.Turns
	run.TokensIn = outcome.TokensIn
	run.TokensOut = outcome.TokensOut
	run.CostUSD = outcome.CostUSD
	run.EndedAt = &ended
	if err := o.store.UpdateRun(ctx, run); err != nil {
		return runID, outcome, fmt.Errorf("agentrun: %w", err)
	}
	if waitErr != nil {
		return runID, outcome, fmt.Errorf("agentrun: agent: %w", waitErr)
	}
	return runID, outcome, nil
}

// runValidation runs the project's steps, recording and narrating each one as it starts and as
// it settles.
//
// Per step rather than per sequence: validation is where a run spends its minutes, and a journal
// written after the last step tells the human nothing for the ten of them they are waiting on
// the first. The step that is running now is the sentence they most want, so it is written before
// the command is, not after. The row and the settled sentence are written together so the two can
// never disagree about what happened or in what order.
func (o *Orchestrator) runValidation(ctx context.Context, h host.Host, ticket core.Ticket, project core.Project, wt git.Worktree, runID string) (validate.Results, error) {
	if len(project.Validation) == 0 {
		return nil, nil
	}

	// The first recording failure is carried out rather than returned from the observer, which
	// has nowhere to return to. The remaining steps still run and are still narrated: a database
	// that would not take one row is no reason to stop telling the human what is happening.
	var recordErr error
	// A fresh runner per run, so each run's logs land under runs/<run-id>/validation rather
	// than every run overwriting one shared file and destroying the evidence for the last.
	results, err := runSteps(ctx, o.newValidator(runID), h, wt.Path, project.Validation, func(at validate.Observation) {
		if at.Kind == validate.StepStarted {
			o.note(ctx, ticket.ID, runID, core.PhaseValidationStep, "running %s", stepNow(at))
			return
		}
		r := at.Result
		// The id is the step's configured position, so the rows read back in the order the
		// steps were written however the sequence ended. A start never makes a row: it is a
		// sentence about a step, not a result for one.
		id := fmt.Sprintf("%s-%d", runID, at.Index)
		if aerr := o.store.AddValidation(ctx, id, runID, r.Step, r.ExitCode, r.Duration.Milliseconds(), r.LogPath); aerr != nil && recordErr == nil {
			recordErr = fmt.Errorf("agentrun: record validation: %w", aerr)
		}
		o.note(ctx, ticket.ID, runID, core.PhaseValidationStep, "%s %s in %s (exit %d)",
			r.Step, r.Outcome, took(r.Duration), r.ExitCode)
	})
	if err != nil {
		return results, fmt.Errorf("agentrun: validation: %w", err)
	}
	return results, recordErr
}

// stepNow renders the step a human is waiting on: what is running, and how far through.
func stepNow(at validate.Observation) string {
	return fmt.Sprintf("%s (%s), step %d of %d",
		at.Step.Name, flatten(at.Step.Cmd, 80), at.Index+1, at.Total)
}

// runSteps runs validation, reporting each step as it starts and settles when the runner can say.
// observe is required; a caller with nothing to say should call the runner directly.
//
// The fallback narrates settled results in a batch afterwards rather than not at all: a runner
// that cannot report as it goes — a stub in a test, an adapter written later — must still produce
// the same results in the same order, just later than it might have, and with nothing to say
// about a step while it was running, because it never said so.
func runSteps(ctx context.Context, r validate.Runner, h host.Host, worktree string,
	steps []validate.Step, observe validate.StepObserver) (validate.Results, error) {

	if or, ok := r.(validate.ObservingRunner); ok {
		return or.RunObserved(ctx, h, worktree, steps, observe)
	}
	results, err := r.Run(ctx, h, worktree, steps)
	for i, res := range results {
		at := validate.Observation{Kind: validate.StepSettled, Index: i, Total: len(steps), Result: res}
		if i < len(steps) {
			at.Step = steps[i]
		}
		observe(at)
	}
	return results, err
}

// handleProviderFailure cools down the model and requeues quota/rate-limit failures.
// Authentication and host failures still need human attention.
//
// The self-correction budget is deliberately untouched: it exists for the agent failing at the
// work, and a quota limit says nothing about whether the agent could fix its own build error.
// Spending a retry here would silently shorten the budget for the attempt that actually matters.
// hostLocalFailure reports a failure that says something about the machine rather than the
// model.
//
// "cli not found" is the case: the agent CLI is missing on that host, which is nothing to do
// with the model and everything to do with the machine. Cooling the model down for it takes a
// working model out of the fleet — observed: a Mac without claude installed put claude-code/haiku
// into a fleet-wide cooldown, so work that would have run perfectly well on Linux stopped too.
func hostLocalFailure(o provider.Outcome) bool {
	return o.Class == provider.ProviderUnavailable &&
		strings.Contains(strings.ToLower(o.Note), "cli not found")
}

func (o *Orchestrator) handleProviderFailure(ctx context.Context, ticket core.Ticket, a Assignment, outcome provider.Outcome) error {
	cooldown := o.cooldownFor(outcome.Class)
	if hostLocalFailure(outcome) {
		// The model is fine; this machine is not equipped. Recording a cooldown would punish
		// every host for one host's missing program.
		cooldown = 0
	}
	if cooldown > 0 {
		if err := o.store.SetProviderUnavailable(ctx, core.ProviderAvailability{
			ProviderID: a.ProviderID,
			Model:      a.Model,
			Class:      outcome.Class,
			Until:      time.Now().Add(cooldown),
			Note:       outcome.Note,
		}); err != nil {
			return fmt.Errorf("agentrun: record cooldown: %w", err)
		}
	}

	if outcome.Class == provider.QuotaExhausted || outcome.Class == provider.RateLimited {
		// Persist the cooldown before releasing the ticket, so the scheduler cannot
		// immediately select the same exhausted choice. It resolves the route afresh;
		// if all choices are cooling, Ready waits without holding a worker or asking
		// the human to retry. Keep the existing worktree and feedback intact.
		if _, err := o.store.SetTicketState(ctx, ticket.ID, core.EventProviderRetry); err != nil {
			return fmt.Errorf("agentrun: requeue after provider limit: %w", err)
		}
		o.log.Info("provider limit; ticket returned to routing", "ticket", ticket.ID,
			"provider", a.ProviderID, "model", a.Model, "class", outcome.Class.String())
		o.note(ctx, ticket.ID, "", core.PhaseHandoff,
			"requeued: %s cooldown on %s/%s for %s",
			cooldownWord(outcome.Class), a.ProviderID, a.Model, cooldown)
		return nil
	}

	reason := core.ReasonValidationFailed
	if outcome.Class == provider.AuthExpired {
		reason = core.ReasonProviderAuth
	}
	_, err := o.park(ctx, ticket, "", reason, map[string]any{
		"provider": a.ProviderID,
		"model":    a.Model,
		"class":    outcome.Class.String(),
		"note":     outcome.Note,
	})
	return err
}

// cooldownWord names a provider-side limit the way a human would say it, rather than as the
// class token the run row stores.
func cooldownWord(c core.FailureClass) string {
	if c == provider.RateLimited {
		return "rate limit"
	}
	return "quota"
}

func (o *Orchestrator) cooldownFor(c core.FailureClass) time.Duration {
	switch c {
	case provider.QuotaExhausted:
		return orDefault(o.cfg.CooldownQuota, time.Hour)
	case provider.RateLimited:
		return orDefault(o.cfg.CooldownRateLimit, 5*time.Minute)
	case provider.ProviderUnavailable, provider.AuthExpired:
		return orDefault(o.cfg.CooldownUnavailable, 15*time.Minute)
	default:
		return 0
	}
}

func orDefault(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// updateTicketFields re-reads a ticket, applies the change, and writes it back.
//
// UpdateTicket writes the whole row, so mutating a struct loaded earlier and saving it would
// silently revert any state transition made in between — an Assigned ticket would drop back to
// Ready, and the next legal transition would then be rejected. Read-modify-write is safe here
// because the orchestrator is the only writer for a ticket while its run is in flight.
func (o *Orchestrator) updateTicketFields(ctx context.Context, id string, mutate func(*core.Ticket)) error {
	t, err := o.store.GetTicket(ctx, id)
	if err != nil {
		return err
	}
	mutate(&t)
	return o.store.UpdateTicket(ctx, t)
}

// park moves a ticket to Needs You with a reason, so it is visible rather than hanging.
func (o *Orchestrator) park(ctx context.Context, ticket core.Ticket, runID string, reason core.AttentionReason, payload map[string]any) (core.State, error) {
	// The event depends on where the ticket actually is now, which may differ from the struct
	// the caller is holding.
	current, err := o.store.GetTicket(ctx, ticket.ID)
	if err != nil {
		return "", fmt.Errorf("agentrun: park ticket %q: %w", ticket.ID, err)
	}
	ev := core.EventValidationExhausted
	if current.State == core.StateRunning {
		ev = core.EventRunFailed
	}

	state, err := o.store.SetTicketState(ctx, ticket.ID, ev)
	if err != nil {
		// Fall back to the other route into Needs You rather than leaving the ticket in
		// limbo: a ticket stuck mid-flight with no attention row is invisible, which is the
		// one outcome the queue exists to prevent.
		if state, err = o.store.SetTicketState(ctx, ticket.ID, core.EventRunFailed); err != nil {
			return "", fmt.Errorf("agentrun: park ticket %q: %w", ticket.ID, err)
		}
	}

	if err := o.raiseAttention(ctx, core.Attention{
		ID:        o.newID(),
		ProjectID: ticket.ProjectID,
		TicketID:  ticket.ID,
		RunID:     runID,
		Reason:    reason,
		Payload:   payload,
		CreatedAt: time.Now(),
	}, ticket.Title); err != nil {
		return state, fmt.Errorf("agentrun: open attention: %w", err)
	}

	// Every route into Needs You passes through here, so narrating it here is what stops the
	// next reason added from being the silent one.
	o.note(ctx, ticket.ID, runID, core.PhaseHandoff, "parked in Needs You: %s", reason)
	return state, nil
}

// Notifier tells the human that Gravy needs them.
//
// An interface rather than the concrete notifier so a test can assert what would have been sent
// without a terminal to ring or a desktop to post to.
type Notifier interface {
	Notify(ctx context.Context, title, body string, urgency notify.Urgency)
}

// WithNotifier sets who is told when a ticket needs a human.
func (o *Orchestrator) WithNotifier(n Notifier) *Orchestrator {
	o.notifier = n
	return o
}

// raiseAttention records that a ticket needs a human, and says so out loud.
//
// Every route into the Needs You queue goes through here. Notifying at each call site instead
// would mean the next reason added is silent until somebody remembers — and a queue you are not
// told about is just a list you have to keep checking.
func (o *Orchestrator) raiseAttention(ctx context.Context, a core.Attention, ticketTitle string) error {
	if err := o.store.OpenAttention(ctx, a); err != nil {
		return err
	}
	o.announce(ctx, a, ticketTitle)
	return nil
}

// announce sends the notification for an attention item, if anyone is listening.
func (o *Orchestrator) announce(ctx context.Context, a core.Attention, ticketTitle string) {
	if o.notifier == nil {
		return
	}
	project := a.ProjectID
	if p, err := o.store.GetProject(ctx, a.ProjectID); err == nil {
		project = p.Name
	}
	title, body, urgency := notify.ForAttention(a.Reason, project, ticketTitle)
	notify.Deliver(ctx, o.notifier, title, body, urgency, a.TicketID)
}

// WithLogs records run output through the given store.
func (o *Orchestrator) WithLogs(s *runlog.Store) *Orchestrator {
	o.logs = s
	return o
}

// Kill terminates a ticket's live run and everything it spawned.
func (o *Orchestrator) Kill(ticketID string) error {
	o.mu.Lock()
	lr, ok := o.live[ticketID]
	o.mu.Unlock()
	if !ok {
		return fmt.Errorf("agentrun: no live run for ticket %q", ticketID)
	}
	lr.cancel()
	if err := lr.handle.Kill(); err != nil {
		return fmt.Errorf("agentrun: kill %q: %w", ticketID, err)
	}
	return nil
}

func (o *Orchestrator) trackLive(ticketID string, lr *liveRun) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.live[ticketID] = lr
}

func (o *Orchestrator) untrackLive(ticketID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.live, ticketID)
}

// LastEvent reports when a ticket's live agent last produced an event, or when it started if it
// has produced none. ok is false when the ticket has no agent running.
func (o *Orchestrator) LastEvent(ticketID string) (at time.Time, ok bool) {
	o.mu.Lock()
	lr, ok := o.live[ticketID]
	o.mu.Unlock()
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(0, lr.lastEvent.Load()), true
}

// effectiveAllowlist is the project's allowlist plus its own validation commands.
//
// An agent must be able to run the commands its work is judged by. Without them it writes
// correct code, cannot verify it, reports that it needs approval, and the run is classified a
// failure — which then burns a self-correction retry repeating the same thing. The product's
// default allowlist has always included the project's declared validation commands; this is
// where that becomes true in practice.
func effectiveAllowlist(p core.Project) core.Allowlist {
	a := p.Allowlist
	for _, step := range p.Validation {
		a.Commands = append(a.Commands, core.Pattern{
			Match: step.Cmd,
			Note:  "declared validation step " + step.Name,
		})
	}
	for _, cmd := range readOnlyShell {
		a.Commands = append(a.Commands, core.Pattern{Match: cmd, Note: "common read-only shell"})
	}
	return a
}

// readOnlyShell is the set of commands the default allowlist grants for looking around
// (PRODUCT.md §12).
//
// Without these an agent is denied on `find` or `git log` while orienting itself, works around
// it, and the run is marked a failure over a command that could not have changed anything.
//
// These are commands whose purpose is to read. A shell redirection can still write, which is
// equally true of `cat`, so this is a statement about intent and blast radius rather than a
// sandbox: nothing here edits in place by default, deletes, or reaches the network.
//
// sed and awk earn their place by being how an agent reads part of a file. Denied them, the
// planner reaches for `sed -n 98,128p` anyway, is refused, and the whole turn is recorded as a
// task failure over a command that wanted to print twenty lines.
var readOnlyShell = []string{
	"ls", "cat", "head", "tail", "wc", "find", "grep", "rg", "which", "pwd", "file", "stat",
	"sed", "awk", "cut", "tr", "sort", "uniq", "diff", "basename", "dirname", "echo", "true",
	"git status", "git diff", "git log", "git show", "git branch", "git ls-files",
}

// planAllowlist is what a planning turn may run.
//
// Read-only and nothing else, deliberately: planning reads a repository and proposes work, and
// it is the one agent role that never edits. It does not inherit the project's build and test
// commands for the same reason — a planner has no business running `make`.
//
// It used to be empty, which is not the same as restrictive. An AgentTask with no allowlist
// sends no --allowedTools at all, so the agent fell back to the CLI's own default and was denied
// every shell call it made: three planning turns in a row died on "permission denied" for ls,
// sed and grep, and the Plan screen was unusable on any project.
func planAllowlist() core.Allowlist {
	out := make([]core.Pattern, 0, len(readOnlyShell))
	for _, cmd := range readOnlyShell {
		out = append(out, core.Pattern{Match: cmd, Note: "reading the repository to plan"})
	}
	return core.Allowlist{Commands: out}
}

// failureContext renders what went wrong for the next attempt's prompt.
func failureContext(outcome provider.Outcome, results validate.Results) string {
	var b []byte
	if outcome.Class != provider.Success {
		b = append(b, ("The previous attempt ended with: " + outcome.Note + "\n\n")...)
	}
	if failure, ok := results.FirstFailure(); ok {
		b = append(b, ("Validation step " + failure.Step + " failed:\n")...)
		b = append(b, failure.Output...)
	}
	return string(b)
}
