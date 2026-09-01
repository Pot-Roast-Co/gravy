package validate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bobbybrady/gravy/internal/core"
	"github.com/bobbybrady/gravy/internal/host"
)

func testRunner(t *testing.T) (*StepRunner, host.Host, string) {
	t.Helper()
	logDir := filepath.Join(t.TempDir(), "validation")
	return NewRunner(logDir), host.NewLocal("local", 4), t.TempDir()
}

// TestRequiredFailureStopsTheSequence is AC1.
//
// Once the build is broken the test results are noise, and running them wastes minutes the human
// is waiting on. Later steps are recorded as Skipped rather than omitted, so "we never got here"
// and "this passed" cannot be confused when someone reads why a ticket was parked.
func TestRequiredFailureStopsTheSequence(t *testing.T) {
	r, h, wt := testRunner(t)
	marker := filepath.Join(wt, "third-ran")

	results, err := r.Run(context.Background(), h, wt, []core.Step{
		{Name: "build", Cmd: "echo building", Required: true},
		{Name: "test", Cmd: "echo failing; exit 1", Required: true},
		{Name: "lint", Cmd: "touch " + marker, Required: true},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}

	if results[0].Outcome != Passed {
		t.Errorf("build = %v, want passed", results[0].Outcome)
	}
	if results[1].Outcome != Failed || results[1].ExitCode != 1 {
		t.Errorf("test = %v exit %d, want failed exit 1", results[1].Outcome, results[1].ExitCode)
	}
	if results[2].Outcome != Skipped {
		t.Errorf("lint = %v, want skipped", results[2].Outcome)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the step after a failed required step actually ran")
	}

	if results.Green() {
		t.Error("Green() is true despite a failed required step")
	}
	failure, ok := results.FirstFailure()
	if !ok || failure.Step != "test" {
		t.Errorf("FirstFailure = %+v, %v", failure, ok)
	}
}

// TestOptionalFailureContinues is AC2.
func TestOptionalFailureContinues(t *testing.T) {
	r, h, wt := testRunner(t)
	marker := filepath.Join(wt, "later-ran")

	results, err := r.Run(context.Background(), h, wt, []core.Step{
		{Name: "build", Cmd: "echo ok", Required: true},
		{Name: "lint", Cmd: "echo lint problems; exit 2", Required: false},
		{Name: "test", Cmd: "touch " + marker + " && echo tested", Required: true},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if results[1].Outcome != Failed {
		t.Errorf("lint = %v, want failed", results[1].Outcome)
	}
	if results[2].Outcome != Passed {
		t.Errorf("test = %v, want passed; an optional failure must not stop the sequence", results[2].Outcome)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("the step after a failed optional step did not run")
	}

	// A failed optional step is a warning, which is the entire point of marking it optional.
	if !results.Green() {
		t.Error("Green() is false, but only an optional step failed")
	}
	warnings := results.Warnings()
	if len(warnings) != 1 || warnings[0].Step != "lint" {
		t.Errorf("Warnings = %+v", warnings)
	}
}

// TestStepTimeoutKillsTheProcessTree is AC3.
func TestStepTimeoutKillsTheProcessTree(t *testing.T) {
	r, h, wt := testRunner(t)
	start := time.Now()

	results, err := r.Run(context.Background(), h, wt, []core.Step{
		{Name: "test", Cmd: "sleep 60", Required: true, Timeout: 500 * time.Millisecond},
		{Name: "lint", Cmd: "echo never", Required: true},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the timeout took %s to take effect", elapsed)
	}
	if results[0].Outcome != TimedOut {
		t.Errorf("outcome = %v, want timed_out", results[0].Outcome)
	}
	// A timeout is a failure of a required step, so the sequence stops.
	if results[1].Outcome != Skipped {
		t.Errorf("the step after a timeout = %v, want skipped", results[1].Outcome)
	}
	if results.Green() {
		t.Error("Green() is true despite a timed-out required step")
	}
}

func TestOutputIsCapturedAndLogged(t *testing.T) {
	r, h, wt := testRunner(t)

	results, err := r.Run(context.Background(), h, wt, []core.Step{
		{Name: "build", Cmd: "echo to-stdout; echo to-stderr >&2", Required: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Both streams matter: a build that logs errors to stderr is entirely ordinary.
	if !strings.Contains(results[0].Output, "to-stdout") {
		t.Errorf("stdout missing from output: %q", results[0].Output)
	}
	if !strings.Contains(results[0].Output, "to-stderr") {
		t.Errorf("stderr missing from output: %q", results[0].Output)
	}

	b, err := os.ReadFile(results[0].LogPath)
	if err != nil {
		t.Fatalf("log file was not written: %v", err)
	}
	if !strings.Contains(string(b), "to-stdout") {
		t.Errorf("log file is missing output: %q", b)
	}
}

// TestLongOutputIsTailCappedButFullyLogged: a runaway build must not put megabytes into the
// database, but the full output has to remain available for diagnosis.
func TestLongOutputIsTailCappedButFullyLogged(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "validation")
	r := &StepRunner{LogDir: logDir, TailBytes: 2048, Shell: "sh"}
	h := host.NewLocal("local", 2)
	wt := t.TempDir()

	results, err := r.Run(context.Background(), h, wt, []core.Step{
		{Name: "build", Cmd: "for i in $(seq 1 5000); do echo \"line $i of noisy build output\"; done", Required: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Outcome != Passed {
		t.Fatalf("outcome = %v: %s", results[0].Outcome, results[0].Output)
	}

	if len(results[0].Output) > 8192 {
		t.Errorf("inline output is %d bytes; it must be tail-capped", len(results[0].Output))
	}
	// The tail is what matters: the end of a build log holds the error.
	if !strings.Contains(results[0].Output, "line 5000") {
		t.Errorf("the tail does not include the final line: %q", lastN(results[0].Output, 200))
	}

	b, err := os.ReadFile(results[0].LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "line 1 of") || !strings.Contains(string(b), "line 5000") {
		t.Error("the log file does not contain the full output")
	}
}

func TestShellFeaturesWork(t *testing.T) {
	r, h, wt := testRunner(t)
	// Commands are shell strings, so quoting and pipelines must survive; splitting on
	// whitespace would break every non-trivial validation command.
	results, err := r.Run(context.Background(), h, wt, []core.Step{
		{Name: "pipeline", Cmd: `echo "one two three" | tr ' ' '\n' | wc -l`, Required: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Outcome != Passed {
		t.Fatalf("outcome = %v: %s", results[0].Outcome, results[0].Output)
	}
	if !strings.Contains(results[0].Output, "3") {
		t.Errorf("pipeline output = %q, want 3", results[0].Output)
	}
}

func TestStepsRunInsideTheWorktree(t *testing.T) {
	r, h, wt := testRunner(t)
	if err := os.WriteFile(filepath.Join(wt, "marker.txt"), []byte("here"), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := r.Run(context.Background(), h, wt, []core.Step{
		{Name: "check", Cmd: "cat marker.txt", Required: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Outcome != Passed || !strings.Contains(results[0].Output, "here") {
		t.Errorf("steps did not run in the worktree: %+v", results[0])
	}
}

func TestNoStepsIsGreen(t *testing.T) {
	r, h, wt := testRunner(t)
	results, err := r.Run(context.Background(), h, wt, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A project with no configured validation has not failed validation.
	if !results.Green() {
		t.Error("a project with no steps is not Green")
	}
	if len(results) != 0 {
		t.Errorf("got %d results for no steps", len(results))
	}
}

func TestRunRejectsEmptyWorktree(t *testing.T) {
	r, h, _ := testRunner(t)
	if _, err := r.Run(context.Background(), h, "", []core.Step{{Name: "x", Cmd: "true"}}); err == nil {
		t.Error("Run accepted an empty worktree path")
	}
}

func TestSummaryReadsClearly(t *testing.T) {
	results := Results{
		{Step: "build", Outcome: Passed, Required: true},
		{Step: "test", Outcome: Failed, ExitCode: 2, Required: true},
		{Step: "lint", Outcome: Skipped, Required: false},
	}
	got := results.Summary()
	for _, want := range []string{"build", "ok", "test", "FAILED (exit 2)", "lint", "skipped"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q is missing %q", got, want)
		}
	}
}

func TestSafeFileName(t *testing.T) {
	tests := map[string]string{
		"build": "build", "go vet": "go-vet", "type check!": "type-check",
		"": "step", "///": "step", "Test-1_2": "test-1_2",
	}
	for in, want := range tests {
		if got := safeFileName(in); got != want {
			t.Errorf("safeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
