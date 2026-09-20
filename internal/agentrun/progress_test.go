package agentrun_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/agentrun"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
	"github.com/pot-roast-co/gravy/internal/provider/fake"
	"github.com/pot-roast-co/gravy/internal/review"
	"github.com/pot-roast-co/gravy/internal/validate"
)

// passingReviewModel answers with a clean advisory verdict, so the review phase is narrated
// without the verdict being the thing under test.
type passingReviewModel struct{}

func (passingReviewModel) Complete(context.Context, core.Project, string) (string, error) {
	return `{"overall":"pass","summary":"nothing to flag","findings":[]}`, nil
}

// journal reads a ticket's progress entries, oldest first.
func (h *harness) journal(ticketID string) []core.Progress {
	h.t.Helper()
	entries, err := h.db.ListProgress(context.Background(), ticketID)
	if err != nil {
		h.t.Fatalf("ListProgress: %v", err)
	}
	return entries
}

// phasesOf renders a journal as its phase tokens, which is what an ordering assertion is about.
func phasesOf(entries []core.Progress) []core.ProgressPhase {
	out := make([]core.ProgressPhase, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Phase)
	}
	return out
}

// detailsFor returns the sentences recorded for one phase.
func detailsFor(entries []core.Progress, phase core.ProgressPhase) []string {
	var out []string
	for _, e := range entries {
		if e.Phase == phase {
			out = append(out, e.Detail)
		}
	}
	return out
}

// dumpJournal renders a journal for a failure message, because a test that says only "the
// phases are wrong" sends the reader back to the database.
func dumpJournal(entries []core.Progress) string {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString("  " + string(e.Phase) + ": " + e.Detail + "\n")
	}
	return b.String()
}

// TestARunLeavesAnOrderedJournal is the ticket's headline acceptance criterion: a run from Ready
// to Review narrates every phase, in the order it passed through them.
//
// The state name answers "where is this ticket" and nothing else, so the phases either all appear
// here or the Running screen is back to reporting "running" for ten silent minutes.
func TestARunLeavesAnOrderedJournal(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{
		SelfCorrectionBudget: 2, RunTimeout: time.Minute, MaxTurns: 10,
	})
	h.orch.WithReviewer(review.New(passingReviewModel{}, 0))
	h.seed([]core.Step{
		{Name: "build", Cmd: "true", Required: true},
		{Name: "test", Cmd: "true", Required: true},
	})

	res, err := runWithAgentWork(t, h, "hello.txt", "hello\n")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalState != core.StateReview {
		t.Fatalf("final state = %q, want review", res.FinalState)
	}

	entries := h.journal("GR-100")
	want := []core.ProgressPhase{
		core.PhaseFetch,
		core.PhaseWorktree,
		core.PhasePrompt,
		core.PhaseAgentStart,
		core.PhaseAgentExit,
		core.PhaseValidationStep, // build, running
		core.PhaseValidationStep, // build, settled
		core.PhaseValidationStep, // test, running
		core.PhaseValidationStep, // test, settled
		core.PhaseSummary,
		core.PhaseReview, // reading the diff
		core.PhaseReview, // the verdict
		core.PhaseHandoff,
	}
	got := phasesOf(entries)
	if len(got) != len(want) {
		t.Fatalf("journal has %d entries, want %d:\n%s", len(got), len(want), dumpJournal(entries))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("phase %d = %q, want %q:\n%s", i, got[i], want[i], dumpJournal(entries))
		}
	}

	// Each entry has to carry the facts a human would otherwise dig for, or the journal is a
	// list of state names with extra steps.
	facts := []struct {
		phase core.ProgressPhase
		want  []string
		why   string
	}{
		{core.PhaseFetch, []string{"main"}, "the branch the work is based on"},
		{core.PhaseWorktree, []string{res.Worktree.Path, res.Worktree.Branch}, "where the work is happening"},
		{core.PhasePrompt, []string{"attempt 1 of 3", "tokens"}, "the attempt and the prompt size"},
		{core.PhaseAgentStart, []string{"fake/m", "attempt 1 of 3"}, "provider, model and attempt"},
		{core.PhaseAgentExit, []string{"fake/m", "success"}, "the exit class"},
		{core.PhaseSummary, []string{"commit", "validation green"}, "what the result was built from"},
		{core.PhaseHandoff, []string{"Review"}, "where the ticket went"},
	}
	for _, f := range facts {
		lines := strings.Join(detailsFor(entries, f.phase), "\n")
		for _, want := range f.want {
			if !strings.Contains(lines, want) {
				t.Errorf("the %s entry does not carry %s (%q missing): %q",
					f.phase, f.why, want, lines)
			}
		}
	}

	// Each executed step is narrated twice: once when it starts, naming what a human is
	// waiting on and how far through the sequence it is, and once when it settles.
	steps := detailsFor(entries, core.PhaseValidationStep)
	wantSteps := []string{
		"running build (true), step 1 of 2",
		"build passed in ",
		"running test (true), step 2 of 2",
		"test passed in ",
	}
	if len(steps) != len(wantSteps) {
		t.Fatalf("validation entries = %v, want a start and a result per step:\n%s",
			steps, dumpJournal(entries))
	}
	for i, want := range wantSteps {
		if !strings.HasPrefix(steps[i], want) {
			t.Errorf("validation entry %d = %q, want it to begin %q", i, steps[i], want)
		}
	}
	for _, line := range []string{steps[1], steps[3]} {
		if !strings.Contains(line, "passed") || !strings.Contains(line, "exit 0") {
			t.Errorf("validation entry %q does not carry its outcome and exit code", line)
		}
	}

	// The entries before a run row exists are the reason the journal hangs off the ticket.
	if entries[0].RunID != "" {
		t.Errorf("the fetch entry has run %q, but it happens before any run exists", entries[0].RunID)
	}
	runs, err := h.db.ListRunsForTicket(context.Background(), "GR-100")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	for _, e := range entries {
		switch e.Phase {
		case core.PhaseAgentStart, core.PhaseAgentExit, core.PhaseValidationStep,
			core.PhaseSummary, core.PhaseReview:
			if e.RunID != runs[0].ID {
				t.Errorf("%s entry has run %q, want %q", e.Phase, e.RunID, runs[0].ID)
			}
		}
	}
}

// TestARetriedRunNarratesItsRetries covers the phase a green first attempt never reaches, and
// the hand-off a parked ticket ends on.
func TestARetriedRunNarratesItsRetries(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{
		SelfCorrectionBudget: 1, RunTimeout: time.Minute,
	})
	h.seed([]core.Step{{Name: "test", Cmd: "echo broken; exit 1", Required: true}})

	res, err := h.orch.Run(context.Background(), h.assignment())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalState != core.StateNeedsYou {
		t.Fatalf("final state = %q, want needs_you", res.FinalState)
	}

	entries := h.journal("GR-100")
	retries := detailsFor(entries, core.PhaseRetry)
	if len(retries) != 1 {
		t.Fatalf("retry entries = %v, want one for the single retry:\n%s", retries, dumpJournal(entries))
	}
	if !strings.Contains(retries[0], "attempt 1 of 2") {
		t.Errorf("retry entry %q does not say which attempt of the budget it was", retries[0])
	}

	// A retry is a second pass through the phases, not an extra line on the first.
	if n := len(detailsFor(entries, core.PhaseAgentStart)); n != 2 {
		t.Errorf("got %d agent_start entries, want one per attempt:\n%s", n, dumpJournal(entries))
	}
	if n := len(detailsFor(entries, core.PhaseValidationStep)); n != 4 {
		t.Errorf("got %d validation_step entries, want a start and a result per attempt:\n%s",
			n, dumpJournal(entries))
	}

	handoff := detailsFor(entries, core.PhaseHandoff)
	if len(handoff) != 1 || !strings.Contains(handoff[0], string(core.ReasonValidationFailed)) {
		t.Errorf("handoff = %v, want it to name why the ticket was parked", handoff)
	}
	// The journal is what the dashboard reads as Activity, so the last word on a parked ticket
	// must be where it went rather than the step that failed.
	if last := entries[len(entries)-1]; last.Phase != core.PhaseHandoff {
		t.Errorf("the journal ends on %q, want the hand-off:\n%s", last.Phase, dumpJournal(entries))
	}
}

// TestAQuotaFailureNarratesItsRequeue: the ticket goes back to Ready with nobody to tell, so the
// journal is the only place that says why it stopped and when it can start again.
func TestAQuotaFailureNarratesItsRequeue(t *testing.T) {
	h := newHarness(t, []fake.Script{
		{Outcome: provider.Outcome{Class: provider.QuotaExhausted, Note: "usage limit reached"}},
	}, agentrun.Config{SelfCorrectionBudget: 2, RunTimeout: time.Minute, CooldownQuota: 42 * time.Minute})
	h.seed([]core.Step{{Name: "test", Cmd: "true", Required: true}})

	res, err := h.orch.Run(context.Background(), h.assignment())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalState != core.StateReady {
		t.Fatalf("final state = %q, want ready", res.FinalState)
	}

	entries := h.journal("GR-100")
	handoff := detailsFor(entries, core.PhaseHandoff)
	if len(handoff) != 1 {
		t.Fatalf("handoff entries = %v, want one:\n%s", handoff, dumpJournal(entries))
	}
	for _, want := range []string{"requeued", "quota", "fake/m", "42m"} {
		if !strings.Contains(handoff[0], want) {
			t.Errorf("requeue entry %q does not carry %q", handoff[0], want)
		}
	}
}

// watchingRunner is a validation runner that reads the journal from inside the sequence.
//
// It is the only honest way to ask the question this ticket's correction is about: whether the
// first step's entry is written when the first step finishes, or only once every step has. A
// runner that recorded the whole sequence afterwards passes every assertion made after the run
// and tells the human nothing while it is going on.
type watchingRunner struct {
	steps func(step string) validate.Result
	// inspect reads the journal as it stands right now.
	inspect func() []core.Progress

	mu       sync.Mutex
	observed map[string][]core.Progress
}

func (w *watchingRunner) Run(ctx context.Context, h host.Host, worktree string, steps []validate.Step) (validate.Results, error) {
	return w.RunObserved(ctx, h, worktree, steps, nil)
}

func (w *watchingRunner) RunObserved(_ context.Context, _ host.Host, worktree string,
	steps []validate.Step, observe validate.StepObserver) (validate.Results, error) {

	var out validate.Results
	for i, s := range steps {
		// Snapshot before anything is said about this step: what the journal held as it began.
		w.mu.Lock()
		w.observed[s.Name] = w.inspect()
		w.mu.Unlock()

		at := validate.Observation{Step: s, Index: i, Total: len(steps), Kind: validate.StepStarted}
		if observe != nil {
			observe(at)
		}

		res := w.steps(s.Name)
		res.Step, res.Required = s.Name, s.Required
		out = append(out, res)
		if observe != nil {
			at.Kind, at.Result = validate.StepSettled, res
			observe(at)
		}
	}
	return out, nil
}

func (w *watchingRunner) snapshot(step string) []core.Progress {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.observed[step]
}

// TestValidationIsNarratedStepByStep is the adversarial case for the correction: with two steps,
// the entry for the first must already be readable when the second begins.
//
// Batching the narration until the sequence ends is invisible to every after-the-fact assertion
// and is exactly the failure a human sees — a ticket that says "implementing" for the twelve
// minutes its test suite is running.
func TestValidationIsNarratedStepByStep(t *testing.T) {
	var h *harness
	watcher := &watchingRunner{
		observed: map[string][]core.Progress{},
		steps: func(string) validate.Result {
			return validate.Result{Outcome: validate.Passed, Duration: 1200 * time.Millisecond}
		},
	}
	h = newHarnessWithValidator(t, []fake.Script{successScript()}, agentrun.Config{
		RunTimeout: time.Minute,
	}, func(string) validate.Runner { return watcher })
	watcher.inspect = func() []core.Progress { return h.journal("GR-100") }

	h.seed([]core.Step{
		{Name: "build", Cmd: "true", Required: true},
		{Name: "test", Cmd: "true", Required: true},
	})

	if _, err := runWithAgentWork(t, h, "hello.txt", "hello\n"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Nothing about validation has been said when the first step starts.
	if got := detailsFor(watcher.snapshot("build"), core.PhaseValidationStep); len(got) != 0 {
		t.Errorf("validation was narrated before the first step ran: %v", got)
	}

	// By the time the second step begins, the first one's start and result are both readable —
	// which is the whole point, and what a batch at the end of the sequence cannot do.
	atTest := detailsFor(watcher.snapshot("test"), core.PhaseValidationStep)
	if len(atTest) != 2 {
		t.Fatalf("journal at the start of step 2 has %d validation entries, want the first "+
			"step's start and result:\n%s", len(atTest), dumpJournal(watcher.snapshot("test")))
	}
	if !strings.HasPrefix(atTest[0], "running build ") {
		t.Errorf("mid-sequence entry = %q, want the first step's start", atTest[0])
	}
	if !strings.HasPrefix(atTest[1], "build ") || !strings.Contains(atTest[1], "passed") {
		t.Errorf("mid-sequence entry = %q, want the first step's result", atTest[1])
	}

	// And the rows the review screen reads are written per step too, with their configured ids.
	runs, err := h.db.ListRunsForTicket(context.Background(), "GR-100")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	vals, err := h.db.ListValidations(context.Background(), runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 2 || vals[0].Step != "build" || vals[1].Step != "test" {
		t.Fatalf("validation rows = %+v, want one per step in configured order", vals)
	}
	for i, v := range vals {
		if want := fmt.Sprintf("%s-%d", runs[0].ID, i); v.ID != want {
			t.Errorf("validation row %d has id %q, want %q", i, v.ID, want)
		}
	}
}

// inspectingHost delegates to a real host and reads the ticket's journal at the moment one
// particular command is executing on it.
//
// A host rather than a stub runner because the question is about the real StepRunner: the
// snapshot is taken after the process has been launched and before anybody has waited on it,
// which is precisely the window in which a step is running and the human is looking at a screen
// that has to say so.
type inspectingHost struct {
	host.Host
	// match is a token in the command this is about, so the many git commands a run executes
	// through the same host are ignored.
	match   string
	inspect func() []core.Progress

	mu   sync.Mutex
	seen []core.Progress
}

func (h *inspectingHost) Exec(ctx context.Context, spec host.ExecSpec) (host.Process, error) {
	proc, err := h.Host.Exec(ctx, spec)
	if err != nil || !strings.Contains(strings.Join(spec.Args, " "), h.match) {
		return proc, err
	}
	h.mu.Lock()
	h.seen = h.inspect()
	h.mu.Unlock()
	return proc, err
}

func (h *inspectingHost) snapshot() []core.Progress {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seen
}

// TestTheJournalNamesTheStepRunningRightNow is the correction's adversarial case: while the
// second step's command is executing, the newest entry must be that step's own, not the first
// step's result.
//
// A journal of finished steps is indistinguishable from this one in every after-the-fact
// assertion, and is what the human actually complained about: a ticket whose Activity reads
// "build passed" for the twelve minutes the test suite is running.
func TestTheJournalNamesTheStepRunningRightNow(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	watcher := &inspectingHost{Host: h.h, match: "slow-second-step"}
	watcher.inspect = func() []core.Progress { return h.journal("GR-100") }
	// Same id, so the assignment's host is this one; git still goes through the plain host.
	h.orch.RegisterHost(watcher)

	h.seed([]core.Step{
		{Name: "build", Cmd: "true", Required: true},
		{Name: "test", Cmd: "sleep 0.2 # slow-second-step", Required: true},
	})

	if _, err := runWithAgentWork(t, h, "hello.txt", "hello\n"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	seen := detailsFor(watcher.snapshot(), core.PhaseValidationStep)
	if len(seen) != 3 {
		t.Fatalf("journal while step 2 was running had %d validation entries, want the first "+
			"step's start and result and the second step's start:\n%s",
			len(seen), dumpJournal(watcher.snapshot()))
	}
	if newest := seen[len(seen)-1]; !strings.HasPrefix(newest, "running test (") ||
		!strings.Contains(newest, "step 2 of 2") {
		t.Errorf("newest entry while step 2 was running = %q, want it to name step 2", newest)
	}
	// And the step before it is still there, settled, in order: the start replaces nothing.
	if !strings.HasPrefix(seen[1], "build ") || !strings.Contains(seen[1], "passed") {
		t.Errorf("the entry before it = %q, want the first step's result", seen[1])
	}
}

// TestLandingNarratesItsOwnPhases: landing happens long after the run ended, so it has no run
// row to hang anything from and is the half of a ticket's life that used to be entirely silent.
func TestLandingNarratesItsOwnPhases(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed([]core.Step{
		{Name: "build", Cmd: "true", Required: true},
		{Name: "test", Cmd: "true", Required: true},
	})
	landReady(t, h, "feature.txt", "the work\n")

	before := len(h.journal("GR-100"))
	land, err := h.orch.Land().Approve(context.Background(), "GR-100", core.ApprovePush)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if land.State != core.StateDone {
		t.Fatalf("state = %s, want done", land.State)
	}

	landing := h.journal("GR-100")[before:]
	want := []core.ProgressPhase{
		core.PhaseFetch,
		core.PhaseWorktree,       // rebasing
		core.PhaseWorktree,       // rebased cleanly
		core.PhaseValidationStep, // build, running
		core.PhaseValidationStep, // build, settled
		core.PhaseValidationStep, // test, running
		core.PhaseValidationStep, // test, settled
		core.PhaseHandoff,        // squashed and pushed
	}
	got := phasesOf(landing)
	if len(got) != len(want) {
		t.Fatalf("landing wrote %d entries, want %d:\n%s", len(got), len(want), dumpJournal(landing))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("landing phase %d = %q, want %q:\n%s", i, got[i], want[i], dumpJournal(landing))
		}
	}

	// Re-validation is narrated step by step here too, start and result: an approval that sits
	// for ten minutes on a test suite should say which step it is on while it is on it.
	steps := detailsFor(landing, core.PhaseValidationStep)
	wantSteps := []string{
		"re-validating before landing: running build (true), step 1 of 2",
		"re-validating before landing: build passed",
		"re-validating before landing: running test (true), step 2 of 2",
		"re-validating before landing: test passed",
	}
	for i, want := range wantSteps {
		if !strings.HasPrefix(steps[i], want) {
			t.Errorf("re-validation entry %d = %q, want it to begin %q", i, steps[i], want)
		}
	}

	last := landing[len(landing)-1].Detail
	for _, fact := range []string{"squashed onto main", "pushed"} {
		if !strings.Contains(last, fact) {
			t.Errorf("the landing hand-off %q does not say %q", last, fact)
		}
	}
}

// TestSkippedStepsAreNarratedToo: a step that never ran because the build broke is recorded
// rather than omitted, and "we never got here" must not look like silence.
func TestSkippedStepsAreNarratedToo(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed([]core.Step{
		{Name: "build", Cmd: "exit 1", Required: true},
		{Name: "test", Cmd: "true", Required: true},
	})

	if _, err := h.orch.Run(context.Background(), h.assignment()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	entries := h.journal("GR-100")
	steps := detailsFor(entries, core.PhaseValidationStep)
	// The step that ran is announced and then settled; the one that never ran is settled only.
	// Announcing a skipped step would put a command that was never executed on the Running
	// screen as the thing happening now.
	if len(steps) != 3 {
		t.Fatalf("validation entries = %v, want the failed step's start and result and the "+
			"skipped step's result:\n%s", steps, dumpJournal(entries))
	}
	if !strings.HasPrefix(steps[0], "running build ") {
		t.Errorf("first entry %q is not the start of the step that ran", steps[0])
	}
	if !strings.Contains(steps[1], "failed") {
		t.Errorf("second entry %q does not say the step failed", steps[1])
	}
	if !strings.Contains(steps[2], string(validate.Skipped)) {
		t.Errorf("third entry %q does not say the step was skipped", steps[2])
	}
}
