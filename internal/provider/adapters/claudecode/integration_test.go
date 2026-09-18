package claudecode_test

import (
	"context"
	"github.com/pot-roast-co/gravy/internal/core"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
	"github.com/pot-roast-co/gravy/internal/provider/adapters/claudecode"
)

// These tests drive the real claude CLI. They cost tokens and need an authenticated install, so
// they are opt-in via GRAVY_INTEGRATION=1 and skip with a clear message otherwise.
//
// The model is pinned to the cheapest available, since what is being tested is the adapter's
// plumbing rather than the model's ability.
const integrationModel = "haiku"

func requireCLI(t *testing.T) (*claudecode.Provider, host.Host) {
	t.Helper()
	if os.Getenv("GRAVY_INTEGRATION") != "1" {
		t.Skip("set GRAVY_INTEGRATION=1 to run tests against the real claude CLI (costs tokens)")
	}
	p := claudecode.New()
	h := host.NewLocal("local", 2)

	av, err := p.Detect(context.Background(), h)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !av.Installed {
		t.Skipf("claude CLI is not installed: %s", av.Detail)
	}
	if !av.Authenticated {
		t.Skipf("claude CLI is not authenticated: %s", av.Detail)
	}
	return p, h
}

// TestIntegrationDetect covers detection against the real binary.
func TestIntegrationDetect(t *testing.T) {
	p, h := requireCLI(t)
	av, err := p.Detect(context.Background(), h)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if av.Version == "" {
		t.Error("no version was captured")
	}
	t.Logf("claude %s, authenticated=%v (%s)", av.Version, av.Authenticated, av.Detail)
}

// TestIntegrationRunCreatesFile is AC1 and AC2: a trivial ticket runs to completion in a temp
// worktree, produces the file, and streams tool use during the run rather than only at exit.
func TestIntegrationRunCreatesFile(t *testing.T) {
	p, h := requireCLI(t)

	worktree := t.TempDir()
	runDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	hd, err := p.Run(ctx, h, provider.AgentTask{
		RunID:        "it-1",
		WorktreePath: worktree,
		Prompt:       "Create a file named hello.txt containing exactly the word hello. Then stop.",
		Model:        integrationModel,
		Timeout:      3 * time.Minute,
		MaxTurns:     8,
		LogPath:      filepath.Join(runDir, "agent.log"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Collect events as they arrive, recording when the first one did.
	start := time.Now()
	var (
		firstEventAt time.Duration
		kinds        []provider.EventKind
		tools        []string
	)
	for e := range hd.Events() {
		if firstEventAt == 0 {
			firstEventAt = time.Since(start)
		}
		kinds = append(kinds, e.Kind)
		if e.Kind == provider.EventToolUse {
			tools = append(tools, e.Tool)
		}
	}

	out, err := hd.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	t.Logf("outcome: class=%v turns=%d in=%d out=%d note=%s",
		out.Class, out.Turns, out.TokensIn, out.TokensOut, out.Note)
	t.Logf("events: %d kinds, tools=%v, first after %s", len(kinds), tools, firstEventAt.Round(time.Millisecond))

	if out.Class != provider.Success {
		t.Fatalf("run failed: %v (%s)", out.Class, out.Note)
	}

	// AC1: the file exists with the right contents.
	b, err := os.ReadFile(filepath.Join(worktree, "hello.txt"))
	if err != nil {
		t.Fatalf("hello.txt was not created: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != "hello" {
		t.Errorf("hello.txt = %q, want %q", got, "hello")
	}

	// AC2: tool use surfaced as events.
	if len(tools) == 0 {
		t.Error("no tool use was streamed; the run wrote a file, so it must have used one")
	}

	// AC3's first half: a session was captured, so the run can be resumed.
	if !out.Session.Valid() {
		t.Errorf("no session captured: %+v", out.Session)
	}
	if out.Turns == 0 {
		t.Error("no turn count captured")
	}
	if out.TokensOut == 0 {
		t.Error("no output token count captured")
	}

	// The raw stream is kept for diagnosis.
	if _, err := os.Stat(filepath.Join(runDir, "agent.log")); err != nil {
		t.Errorf("agent.log was not written: %v", err)
	}
}

// TestIntegrationResumeContinuesContext is AC3: Resume continues prior context rather than
// restarting, which is what makes a blocked ticket resumable.
func TestIntegrationResumeContinuesContext(t *testing.T) {
	p, h := requireCLI(t)

	worktree := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	hd, err := p.Run(ctx, h, provider.AgentTask{
		RunID:        "it-2",
		WorktreePath: worktree,
		Prompt:       "Remember this word, it is important: pumpernickel. Reply with just: noted. Do not use tools.",
		Model:        integrationModel,
		Timeout:      2 * time.Minute,
		MaxTurns:     4,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	drain(hd)
	first, err := hd.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if first.Class != provider.Success {
		t.Fatalf("first run failed: %v (%s)", first.Class, first.Note)
	}
	if !first.Session.Valid() {
		t.Fatalf("no session to resume: %+v", first.Session)
	}

	// A separate invocation resumes the session. If context were lost, the word would be gone.
	hd2, err := p.Resume(ctx, h, first.Session, "What was the important word? Reply with just that word. Do not use tools.", core.Allowlist{})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	var said strings.Builder
	for e := range hd2.Events() {
		if e.Kind == provider.EventMessage || e.Kind == provider.EventFinished {
			said.WriteString(e.Text)
			said.WriteString(" ")
		}
	}
	second, err := hd2.Wait()
	if err != nil {
		t.Fatalf("Wait after resume: %v", err)
	}
	if second.Class != provider.Success {
		t.Fatalf("resumed run failed: %v (%s)", second.Class, second.Note)
	}
	if !strings.Contains(strings.ToLower(said.String()), "pumpernickel") {
		t.Errorf("resumed session lost prior context; reply was %q", said.String())
	}
	// The session id is stable across resume, so no reconciliation is needed.
	if second.Session.ID != first.Session.ID {
		t.Errorf("session id changed on resume: %q then %q", first.Session.ID, second.Session.ID)
	}
}

// TestIntegrationTimeoutKillsRun is AC4.
func TestIntegrationTimeoutKillsRun(t *testing.T) {
	p, h := requireCLI(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The timeout is set below the CLI's own startup-to-completion time rather than relying on
	// a prompt that "should" take a while. Asking the model to do something lengthy is not a
	// controllable input: an earlier version of this test asked it to count to 10000 and it
	// finished in a single turn well inside the timeout, so the test passed or failed on the
	// model's mood. Observed timings are ~330ms to the first event and ~9s to completion, so a
	// 2s cap fires reliably while still exercising a real run rather than a startup failure.
	start := time.Now()
	hd, err := p.Run(ctx, h, provider.AgentTask{
		RunID:        "it-3",
		WorktreePath: t.TempDir(),
		Prompt:       "Create ten files named one.txt through ten.txt, each containing its own name.",
		Model:        integrationModel,
		Timeout:      2 * time.Second,
		MaxTurns:     50,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	drain(hd)

	out, err := hd.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	elapsed := time.Since(start)

	if !out.TimedOut {
		t.Errorf("outcome does not report a timeout: %+v", out)
	}
	if out.Class != provider.Timeout {
		t.Errorf("class = %v, want Timeout", out.Class)
	}
	if elapsed > 30*time.Second {
		t.Errorf("timeout took %s to take effect", elapsed.Round(time.Second))
	}
	t.Logf("timed out after %s: %s", elapsed.Round(time.Millisecond), out.Note)
}

func drain(h provider.Handle) {
	go func() {
		for range h.Events() {
		}
	}()
}
