package agentrun_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bobbybrady/gravy/internal/agentrun"
	"github.com/bobbybrady/gravy/internal/core"
	"github.com/bobbybrady/gravy/internal/git"
	"github.com/bobbybrady/gravy/internal/host"
	"github.com/bobbybrady/gravy/internal/provider"
	"github.com/bobbybrady/gravy/internal/provider/fake"
	"github.com/bobbybrady/gravy/internal/store"
	"github.com/bobbybrady/gravy/internal/validate"
)

// ---- harness -------------------------------------------------------------

type harness struct {
	t        *testing.T
	db       *store.DB
	home     string
	upstream string
	repoPath string
	h        host.Host
	slots    *countingSlots
	orch     *agentrun.Orchestrator
	provider *fake.Provider
	ids      *idGen
}

type idGen struct {
	mu sync.Mutex
	n  int
}

func (g *idGen) next() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return fmt.Sprintf("id-%03d", g.n)
}

// countingSlots records claims and releases so a leak is detectable.
type countingSlots struct {
	mu       sync.Mutex
	total    int
	used     int
	claims   int
	releases int
}

func (s *countingSlots) TryClaim() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used >= s.total {
		return false
	}
	s.used++
	s.claims++
	return true
}

func (s *countingSlots) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.used--
	s.releases++
}

func (s *countingSlots) inUse() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

// gitCmd runs git through the Host, as everything else does.
func gitCmd(t *testing.T, h host.Host, dir string, args ...string) string {
	t.Helper()
	p, err := h.Exec(context.Background(), host.ExecSpec{
		Cmd: "git", Args: args, Dir: dir, Timeout: 30 * time.Second,
		Env: map[string]string{
			"GIT_AUTHOR_NAME": "test", "GIT_AUTHOR_EMAIL": "t@example.com",
			"GIT_COMMITTER_NAME": "test", "GIT_COMMITTER_EMAIL": "t@example.com",
			"GIT_TERMINAL_PROMPT": "0",
		},
	})
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	outCh := make(chan string, 1)
	errCh := make(chan string, 1)
	go func() { b, _ := readAll(p.Stdout()); outCh <- b }()
	go func() { b, _ := readAll(p.Stderr()); errCh <- b }()
	st, waitErr := p.Wait()
	out, errOut := <-outCh, <-errCh
	if waitErr != nil {
		t.Fatalf("git %v: %v", args, waitErr)
	}
	if st.Code != 0 {
		t.Fatalf("git %v: exit %d\n%s%s", args, st.Code, out, errOut)
	}
	return out
}

func readAll(r interface{ Read([]byte) (int, error) }) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String(), nil
		}
	}
}

// newHarness builds an upstream repo, a clone, a store, and a wired orchestrator.
func newHarness(t *testing.T, scripts []fake.Script, cfg agentrun.Config) *harness {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	h := host.NewLocal("local", 8)

	// An upstream so fetch has something real to do.
	upstream := filepath.Join(root, "upstream")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, h, upstream, "init", "-q", "-b", "main")
	writeFile(t, upstream, "README.md", "# project\n")
	gitCmd(t, h, upstream, "add", "-A")
	gitCmd(t, h, upstream, "commit", "-q", "-m", "initial")

	repoPath := filepath.Join(root, "repo")
	gitCmd(t, h, root, "clone", "-q", upstream, repoPath)

	db, err := store.Open(ctx, filepath.Join(root, "gravy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	home := filepath.Join(root, "gravy-home")
	if cfg.RunsDir == "" {
		cfg.RunsDir = filepath.Join(home, "runs")
	}

	ids := &idGen{}
	slots := &countingSlots{total: 4}
	p := fake.New("fake", fake.WithScripts(scripts...))

	orch := agentrun.New(
		dbStore{db},
		agentrun.LocalRepos{Host: h, Home: home},
		slots,
		validate.NewRunner(filepath.Join(home, "validation")),
		agentrun.SimplePrompt{},
		cfg,
		ids.next,
	)
	orch.RegisterHost(h)
	orch.RegisterProvider(p)

	return &harness{
		t: t, db: db, home: home, upstream: upstream, repoPath: repoPath,
		h: h, slots: slots, orch: orch, provider: p, ids: ids,
	}
}

// dbStore adapts the store to the orchestrator's narrower interface.
type dbStore struct{ *store.DB }

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seed creates a project and a Ready ticket.
func (h *harness) seed(validation []core.Step) (core.Project, core.Ticket) {
	h.t.Helper()
	ctx := context.Background()

	p := core.Project{
		ID: "p1", Slug: "proj", Name: "proj", RepoPath: h.repoPath,
		TargetBranch: "main", MergeMode: core.LandMerge, MaxConcurrency: 1,
		Validation: validation, CreatedAt: time.Now(),
	}
	if err := h.db.CreateProject(ctx, p); err != nil {
		h.t.Fatal(err)
	}

	tk := core.Ticket{
		ID: "GR-100", ProjectID: "p1", Title: "do the thing", Body: "make it work",
		State: core.StateReady, Route: core.RouteImplementation,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := h.db.CreateTicket(ctx, tk); err != nil {
		h.t.Fatal(err)
	}
	return p, tk
}

func (h *harness) assignment() agentrun.Assignment {
	return agentrun.Assignment{TicketID: "GR-100", HostID: "local", ProviderID: "fake", Model: "m"}
}

// writeInWorktree returns a script whose "agent" creates a file, by using the fake's event
// stream as a hook is not possible — instead the test writes the file before running.
func successScript() fake.Script {
	return fake.Script{Outcome: provider.Outcome{
		Class: provider.Success, Turns: 3, TokensIn: 100, TokensOut: 50,
		Session: provider.SessionRef{ProviderID: "fake", ID: "sess-1"},
	}}
}

// ---- tests ---------------------------------------------------------------

// TestReadyToReviewing is AC1.
func TestReadyToReviewing(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{
		SelfCorrectionBudget: 2, RunTimeout: time.Minute, MaxTurns: 10,
	})
	h.seed([]core.Step{{Name: "test", Cmd: "true", Required: true}})

	// The agent's "work": the fake cannot write files, so the test writes one into the
	// worktree the moment it exists, standing in for what a real agent would do.
	res, err := runWithAgentWork(t, h, "hello.txt", "hello\n")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.FinalState != core.StateReviewing {
		t.Errorf("final state = %q, want reviewing", res.FinalState)
	}
	if res.Commit == "" {
		t.Error("no commit recorded for work the agent produced")
	}
	if len(res.Validation) != 1 || res.Validation[0].Outcome != validate.Passed {
		t.Errorf("validation = %+v", res.Validation)
	}
	if !res.Validation.Green() {
		t.Error("validation is not green")
	}

	ctx := context.Background()
	tk, err := h.db.GetTicket(ctx, "GR-100")
	if err != nil {
		t.Fatal(err)
	}
	if tk.State != core.StateReviewing {
		t.Errorf("persisted state = %q, want reviewing", tk.State)
	}
	if tk.WorktreePath == "" || tk.Branch == "" {
		t.Errorf("worktree not recorded on the ticket: %+v", tk)
	}

	// AC8: the run row carries provider, model, retries, duration and PID.
	runs, err := h.db.ListRunsForTicket(ctx, "GR-100")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	r := runs[0]
	if r.ProviderID != "fake" || r.Model != "m" {
		t.Errorf("run row = %+v", r)
	}
	if r.SessionRef != "sess-1" {
		t.Errorf("session ref = %q, want the provider's", r.SessionRef)
	}
	if r.Turns != 3 || r.TokensIn != 100 {
		t.Errorf("usage not recorded: %+v", r)
	}
	if r.EndedAt == nil {
		t.Error("run has no end time")
	}

	// Validation results are persisted for the review screen to show.
	vals, err := h.db.ListValidations(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 1 || vals[0].Step != "test" {
		t.Errorf("validations = %+v", vals)
	}
}

// runWithAgentWork starts the orchestrator and writes a file into the worktree as soon as it
// appears, standing in for the agent's edits.
func runWithAgentWork(t *testing.T, h *harness, name, body string) (agentrun.Result, error) {
	t.Helper()
	worktree := filepath.Join(h.home, "projects", "proj", "worktrees", "gravy-GR-100-do-the-thing")

	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(worktree); err == nil {
				writeFile(t, worktree, name, body)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	res, err := h.orch.Run(context.Background(), h.assignment())
	<-done
	return res, err
}

// TestWorktreeIsCutFromFreshlyFetchedTarget is AC2.
//
// A commit that lands upstream after the ticket was created must be present in the worktree.
// This is the entire reason the fetch happens per ticket at claim time rather than in a batch:
// a worktree cut from a stale target silently omits the previous ticket's merged work, and the
// agent then reimplements or conflicts with it.
func TestWorktreeIsCutFromFreshlyFetchedTarget(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed(nil)

	// A previous ticket merges upstream after ours was created.
	writeFile(t, h.upstream, "landed-earlier.txt", "merged before our ticket started\n")
	gitCmd(t, h.h, h.upstream, "add", "-A")
	gitCmd(t, h.h, h.upstream, "commit", "-q", "-m", "earlier ticket merged")

	res, err := h.orch.Run(context.Background(), h.assignment())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := filepath.Join(res.Worktree.Path, "landed-earlier.txt")
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("the worktree does not contain the commit that landed before this ticket started: %v\n"+
			"the fetch must happen per ticket at claim time, immediately before the worktree is cut", err)
	}
}

// TestValidationFailureRetriesWithPriorOutput is AC3.
func TestValidationFailureRetriesWithPriorOutput(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{
		SelfCorrectionBudget: 2, RunTimeout: time.Minute,
	})
	h.seed([]core.Step{{Name: "test", Cmd: "echo 'FAIL: the widget is broken'; exit 1", Required: true}})

	res, err := h.orch.Run(context.Background(), h.assignment())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// One attempt plus two retries.
	if res.Attempts != 3 {
		t.Errorf("attempts = %d, want 3 (1 + budget of 2)", res.Attempts)
	}
	runs := h.provider.Runs()
	if len(runs) != 3 {
		t.Fatalf("the provider was run %d times, want 3", len(runs))
	}

	// The whole value of a retry is that the agent learns what went wrong. An agent asked to
	// try again with no new information usually produces the same output.
	//
	// The assertion is on the retry framing rather than on the failure text itself: the prompt
	// also lists the project's validation commands, and this fixture's command contains its own
	// output, so searching for that string would match the first attempt legitimately.
	const framing = "the previous attempt failed"
	if strings.Contains(runs[0].Prompt, framing) {
		t.Error("the first attempt's prompt was framed as a retry")
	}
	for i, r := range runs[1:] {
		if !strings.Contains(r.Prompt, framing) {
			t.Errorf("retry %d was not framed as a retry:\n%s", i+1, r.Prompt)
		}
		if !strings.Contains(r.Prompt, "the widget is broken") {
			t.Errorf("retry %d did not receive the prior failure output:\n%s", i+1, r.Prompt)
		}
		if !strings.Contains(r.Prompt, "Validation step test failed") {
			t.Errorf("retry %d does not name the failing step", i+1)
		}
	}
}

// TestBudgetExhaustionParksVisibly is AC4.
func TestBudgetExhaustionParksVisibly(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{
		SelfCorrectionBudget: 1, RunTimeout: time.Minute,
	})
	h.seed([]core.Step{{Name: "test", Cmd: "exit 1", Required: true}})

	res, err := h.orch.Run(context.Background(), h.assignment())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalState != core.StateNeedsYou {
		t.Errorf("final state = %q, want needs_you", res.FinalState)
	}

	ctx := context.Background()
	open, err := h.db.ListOpenAttention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("got %d attention items, want 1", len(open))
	}
	if open[0].Reason != core.ReasonValidationFailed {
		t.Errorf("reason = %q, want validation_failed", open[0].Reason)
	}
	if open[0].TicketID != "GR-100" {
		t.Errorf("attention item is not linked to the ticket: %+v", open[0])
	}
	// The payload must carry enough to act on without hunting.
	if open[0].Payload["summary"] == nil {
		t.Error("attention payload has no validation summary")
	}
}

// TestQuotaDoesNotConsumeSelfCorrectionBudget is AC5.
//
// The two budgets are independent. A quota limit says nothing about whether the agent could fix
// its own build error, so spending a self-correction retry on it silently shortens the budget
// for the attempt that actually matters.
func TestQuotaDoesNotConsumeSelfCorrectionBudget(t *testing.T) {
	h := newHarness(t, []fake.Script{
		{Outcome: provider.Outcome{Class: provider.QuotaExhausted, Note: "usage limit reached"}},
	}, agentrun.Config{
		SelfCorrectionBudget: 2, RunTimeout: time.Minute, CooldownQuota: 42 * time.Minute,
	})
	h.seed([]core.Step{{Name: "test", Cmd: "true", Required: true}})

	res, err := h.orch.Run(context.Background(), h.assignment())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// It must stop immediately rather than burning retries against an exhausted provider.
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: a quota failure must not consume the self-correction budget", res.Attempts)
	}
	if n := len(h.provider.Runs()); n != 1 {
		t.Errorf("the provider was run %d times, want 1", n)
	}

	ctx := context.Background()
	tk, _ := h.db.GetTicket(ctx, "GR-100")
	if tk.RetryCount != 0 {
		t.Errorf("retry_count = %d, want 0: the self-correction budget was consumed by a quota failure", tk.RetryCount)
	}

	// The model must be cooled down so the router avoids it.
	unavailable, err := h.db.IsUnavailable(ctx, "fake", "m", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !unavailable {
		t.Error("the model was not cooled down after a quota failure")
	}
	rows, _ := h.db.ListUnavailable(ctx, time.Now())
	if len(rows) != 1 || rows[0].Class != core.QuotaExhausted {
		t.Errorf("availability rows = %+v", rows)
	}
}

func TestAuthFailureRaisesProviderAuth(t *testing.T) {
	h := newHarness(t, []fake.Script{
		{Outcome: provider.Outcome{Class: provider.AuthExpired, Note: "401 api key is invalid"}},
	}, agentrun.Config{SelfCorrectionBudget: 2, RunTimeout: time.Minute})
	h.seed(nil)

	if _, err := h.orch.Run(context.Background(), h.assignment()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	open, err := h.db.ListOpenAttention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Reason != core.ReasonProviderAuth {
		t.Fatalf("attention = %+v, want provider_auth", open)
	}
}

// TestSlotIsReleasedOnEveryPath is AC6.
//
// A leaked slot permanently shrinks the worker pool, and the failure is invisible until the
// queue mysteriously stops moving.
func TestSlotIsReleasedOnEveryPath(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
		h.seed(nil)
		if _, err := h.orch.Run(context.Background(), h.assignment()); err != nil {
			t.Fatal(err)
		}
		assertSlotsBalanced(t, h)
	})

	t.Run("validation failure", func(t *testing.T) {
		h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
		h.seed([]core.Step{{Name: "test", Cmd: "exit 1", Required: true}})
		if _, err := h.orch.Run(context.Background(), h.assignment()); err != nil {
			t.Fatal(err)
		}
		assertSlotsBalanced(t, h)
	})

	t.Run("error mid-run", func(t *testing.T) {
		h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
		h.seed(nil)
		// An unregistered provider fails after the slot is claimed.
		_, err := h.orch.Run(context.Background(), agentrun.Assignment{
			TicketID: "GR-100", HostID: "local", ProviderID: "nonexistent", Model: "m",
		})
		if err == nil {
			t.Fatal("expected an error")
		}
		assertSlotsBalanced(t, h)
	})

	t.Run("panic inside the provider", func(t *testing.T) {
		h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
		h.seed(nil)
		h.orch.RegisterProvider(panicProvider{})

		_, err := h.orch.Run(context.Background(), agentrun.Assignment{
			TicketID: "GR-100", HostID: "local", ProviderID: "panicky", Model: "m",
		})
		if err == nil {
			t.Fatal("a panicking provider did not produce an error")
		}
		if !strings.Contains(err.Error(), "panic") {
			t.Errorf("error = %v, want it to name the panic", err)
		}
		assertSlotsBalanced(t, h)
	})
}

func assertSlotsBalanced(t *testing.T, h *harness) {
	t.Helper()
	if got := h.slots.inUse(); got != 0 {
		t.Errorf("%d worker slots still held after the run; the pool has permanently shrunk", got)
	}
	if h.slots.claims != h.slots.releases {
		t.Errorf("%d claims but %d releases", h.slots.claims, h.slots.releases)
	}
}

// panicProvider panics when run, standing in for a buggy adapter.
//
// It implements the interface directly rather than embedding fake.Provider: that type holds a
// mutex, and embedding it by value copies the lock — which go vet correctly refuses.
type panicProvider struct{}

func (panicProvider) ID() string { return "panicky" }
func (panicProvider) Run(context.Context, host.Host, provider.AgentTask) (provider.Handle, error) {
	panic("adapter exploded")
}
func (panicProvider) Detect(context.Context, host.Host) (provider.Availability, error) {
	return provider.Availability{}, nil
}
func (panicProvider) Models(context.Context) ([]provider.Model, error) { return nil, nil }
func (panicProvider) Resume(context.Context, host.Host, provider.SessionRef, string) (provider.Handle, error) {
	return nil, nil
}
func (panicProvider) Classify(int, string, string) provider.Classification {
	return provider.Classification{}
}

// TestKillTerminatesTheRun is AC7.
func TestKillTerminatesTheRun(t *testing.T) {
	h := newHarness(t, []fake.Script{{BlockUntilKilled: true}}, agentrun.Config{RunTimeout: time.Minute})
	h.seed(nil)

	done := make(chan error, 1)
	go func() {
		_, err := h.orch.Run(context.Background(), h.assignment())
		done <- err
	}()

	// Wait until the run is live enough to be killable.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := h.orch.Kill("GR-100"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Kill did not end the run")
	}
	assertSlotsBalanced(t, h)
}

func TestKillUnknownTicket(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	if err := h.orch.Kill("nope"); err == nil {
		t.Error("Kill accepted a ticket with no live run")
	}
}

// TestNoWorkerSlotAvailable: the orchestrator refuses rather than queueing internally, which
// would hide the pool being full from the scheduler.
func TestNoWorkerSlotAvailable(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed(nil)
	h.slots.total = 0

	if _, err := h.orch.Run(context.Background(), h.assignment()); err == nil {
		t.Error("Run proceeded with no worker slot available")
	}
}

// TestOptionalValidationFailureStillReachesReview: a failed advisory step is a warning.
func TestOptionalValidationFailureStillReachesReview(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{
		SelfCorrectionBudget: 1, RunTimeout: time.Minute,
	})
	h.seed([]core.Step{
		{Name: "test", Cmd: "true", Required: true},
		{Name: "lint", Cmd: "exit 1", Required: false},
	})

	res, err := h.orch.Run(context.Background(), h.assignment())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalState != core.StateReviewing {
		t.Errorf("final state = %q, want reviewing despite a failed optional step", res.FinalState)
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d; an optional failure must not trigger a retry", res.Attempts)
	}
	if len(res.Validation.Warnings()) != 1 {
		t.Errorf("warnings = %+v, want the failed optional step", res.Validation.Warnings())
	}
}

// TestNoValidationConfigured: a project without validation steps has not failed validation.
func TestNoValidationConfigured(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed(nil)

	res, err := h.orch.Run(context.Background(), h.assignment())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalState != core.StateReviewing {
		t.Errorf("final state = %q, want reviewing", res.FinalState)
	}
}

// TestGravyWritesNothingIntoTheRepository is M0 exit criterion 11.
func TestGravyWritesNothingIntoTheRepository(t *testing.T) {
	h := newHarness(t, []fake.Script{successScript()}, agentrun.Config{RunTimeout: time.Minute})
	h.seed([]core.Step{{Name: "test", Cmd: "true", Required: true}})

	before := gitCmd(t, h.h, h.repoPath, "status", "--porcelain")

	res, err := runWithAgentWork(t, h, "work.txt", "agent output\n")
	if err != nil {
		t.Fatal(err)
	}

	after := gitCmd(t, h.h, h.repoPath, "status", "--porcelain")
	if strings.TrimSpace(before) != strings.TrimSpace(after) {
		t.Errorf("the run dirtied the main working copy:\n%s", after)
	}
	// And the worktree lives outside the repository entirely.
	if strings.HasPrefix(res.Worktree.Path, h.repoPath+string(filepath.Separator)) {
		t.Errorf("worktree %q is inside the repository", res.Worktree.Path)
	}
}

var _ = git.Worktree{}
