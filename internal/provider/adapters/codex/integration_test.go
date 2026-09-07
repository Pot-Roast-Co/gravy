package codex_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
	"github.com/pot-roast-co/gravy/internal/provider/adapters/codex"
)

// These tests drive the real codex CLI. They cost tokens and need an authenticated install, so
// they are opt-in via GRAVY_INTEGRATION=1 and skip with a clear message otherwise.
func requireCLI(t *testing.T) (*codex.Provider, host.Host) {
	t.Helper()
	if os.Getenv("GRAVY_INTEGRATION") != "1" {
		t.Skip("set GRAVY_INTEGRATION=1 to run tests against the real codex CLI (costs tokens)")
	}
	p, h := codex.New(), host.NewLocal("local", 2)
	av, err := p.Detect(context.Background(), h)
	if err != nil || !av.Installed {
		t.Skipf("codex is not installed: %v (%s)", err, av.Detail)
	}
	return p, h
}

// worktree makes a throwaway git repository for the agent to work in.
//
// git runs through the Host rather than os/exec: ARCHITECTURE.md 1.1 permits that import only
// under internal/host, and a test is not an exemption from the rule it is meant to protect.
func worktree(t *testing.T, h host.Host) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"-c", "user.email=t@example.com", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		proc, err := h.Exec(context.Background(), host.ExecSpec{
			Cmd: "git", Args: args, Dir: dir, Timeout: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		if status, werr := proc.Wait(); werr != nil || status.Code != 0 {
			t.Fatalf("git %v: exit %d: %v", args, status.Code, werr)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "calc.go"),
		[]byte("package calc\n\nfunc Add(a, b int) int { return a + b }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDetect checks installation and authentication reporting against the real binary.
func TestDetect(t *testing.T) {
	p, h := requireCLI(t)

	av, err := p.Detect(context.Background(), h)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !av.Installed {
		t.Fatalf("codex not detected as installed: %+v", av)
	}
	if av.Version == "" {
		t.Error("no version reported")
	}
	// Detail must say what to do, not merely what is wrong.
	if av.Detail == "" {
		t.Error("no detail reported")
	}
	t.Logf("installed=%v authenticated=%v version=%q detail=%q",
		av.Installed, av.Authenticated, av.Version, av.Detail)
}

// TestRunCompletesATrivialTicket is GR-013 AC1.
func TestRunCompletesATrivialTicket(t *testing.T) {
	p, h := requireCLI(t)
	dir := worktree(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	handle, err := p.Run(ctx, h, provider.AgentTask{
		WorktreePath: dir,
		Prompt:       "Add a Sub function to calc.go returning a - b. Change nothing else.",
		Timeout:      4 * time.Minute,
		LogPath:      filepath.Join(t.TempDir(), "agent.log"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var kinds []provider.EventKind
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range handle.Events() {
			kinds = append(kinds, e.Kind)
		}
	}()

	out, err := handle.Wait()
	<-done
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if out.Class != provider.Success {
		t.Fatalf("class = %v, note = %q", out.Class, out.Note)
	}
	// The session id has to be scraped from the stream, so its absence is a real failure.
	if out.Session.ID == "" || out.Session.ProviderID != codex.ID {
		t.Errorf("session = %+v, want a captured thread id", out.Session)
	}
	if out.TokensIn == 0 {
		t.Error("no token usage reported")
	}
	if len(kinds) == 0 {
		t.Error("no events streamed")
	}

	// The work actually happened.
	body, err := os.ReadFile(filepath.Join(dir, "calc.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "func Sub") {
		t.Errorf("the agent did not add Sub:\n%s", body)
	}
}
