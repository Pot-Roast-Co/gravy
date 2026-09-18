package copilot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
)

// An ordinary prompt is still an argument. This path is by far the common one and must not grow
// a temporary file it does not need.
func TestSmallPromptStaysAnArgument(t *testing.T) {
	got, cleanup, err := promptArg(provider.AgentTask{Prompt: "do the thing", WorktreePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != nil {
		t.Error("a small prompt created something to clean up")
	}
	if got != "do the thing" {
		t.Errorf("prompt = %q, want it passed through untouched", got)
	}
}

// TestLargePromptBecomesAFile is the regression.
//
// Copilot has no stdin mode — -p takes a value — so the pipe that fixes this for claude-code and
// codex is unavailable. A path is a few hundred bytes whatever the prompt weighs.
func TestLargePromptBecomesAFile(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("the project documents go here. ", 20000) // ~600KB

	got, cleanup, err := promptArg(provider.AgentTask{
		Prompt:       huge,
		WorktreePath: dir,
		LogPath:      filepath.Join(dir, "runs", "run-1", "copilot.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil {
		t.Fatal("a file was written with nothing to remove it")
	}
	defer cleanup()

	if len(got) >= host.MaxArgLen {
		t.Fatalf("the replacement argument is %d bytes, which is the bug again", len(got))
	}

	// Nothing is lost: the file holds the whole prompt, byte for byte. Truncating instead would
	// hand an agent two thirds of a document and let it plan confidently against the rest.
	var path string
	for _, f := range strings.Fields(got) {
		if strings.Contains(f, "prompt-") {
			path = strings.TrimSuffix(f, ".")
		}
	}
	if path == "" {
		t.Fatalf("the instruction names no file: %q", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the file the agent is told to read: %v", err)
	}
	if string(body) != huge {
		t.Errorf("the file holds %d bytes, the prompt was %d", len(body), len(huge))
	}

	// It lives beside the run's artefacts, never in the worktree, so it cannot be swept into a
	// commit — the rule the claude-code settings file already follows.
	if !strings.HasPrefix(path, filepath.Join(dir, "runs")) {
		t.Errorf("the prompt file is at %q, which is not beside the run's artefacts", path)
	}

	// And the instruction tells the agent to actually read it.
	for _, want := range []string{"Read that file", "in full"} {
		if !strings.Contains(got, want) {
			t.Errorf("the instruction omits %q: %q", want, got)
		}
	}
}

// Cleanup removes the file, so a long-running fleet does not accumulate prompts.
func TestPromptFileIsRemoved(t *testing.T) {
	dir := t.TempDir()
	got, cleanup, err := promptArg(provider.AgentTask{
		Prompt:       strings.Repeat("x", promptThreshold+1),
		WorktreePath: dir,
		LogPath:      filepath.Join(dir, "run.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil {
		t.Fatal("a file was written with nothing to remove it")
	}
	var path string
	for _, f := range strings.Fields(got) {
		if strings.Contains(f, "prompt-") {
			path = strings.TrimSuffix(f, ".")
		}
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the prompt file survived cleanup: %v", err)
	}
}
