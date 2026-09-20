package validate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
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

// TestObserverSeesEachStepAsItSettles is what the optional interface exists for: a caller that
// narrates validation needs the first step's result while the second is still running, not a
// batch once the sequence is over.
//
// The observation is taken before the next step starts, so a runner that collected results and
// reported them at the end would fail here — which is exactly the behaviour being replaced.
func TestObserverSeesEachStepAsItSettles(t *testing.T) {
	r, h, wt := testRunner(t)
	marker := filepath.Join(wt, "second-started")

	var seen []Result
	results, err := r.RunObserved(context.Background(), h, wt, []core.Step{
		// The second step records what the observer had been told by the time it ran.
		{Name: "build", Cmd: "echo building", Required: true},
		{Name: "test", Cmd: "touch " + marker + "; echo failing; exit 1", Required: true},
		{Name: "lint", Cmd: "echo linting", Required: true},
	}, func(at Observation) {
		if at.Kind != StepSettled {
			return
		}
		res := at.Result
		if res.Step == "test" {
			if _, err := os.Stat(marker); err != nil {
				t.Errorf("the test step was observed before it ran: %v", err)
			}
		}
		if len(seen) == 0 {
			if _, err := os.Stat(marker); err == nil {
				t.Error("the build step was not observed until the test step had started")
			}
		}
		seen = append(seen, res)
	})
	if err != nil {
		t.Fatalf("RunObserved: %v", err)
	}

	if len(seen) != len(results) {
		t.Fatalf("observed %d steps, want %d", len(seen), len(results))
	}
	for i := range results {
		if seen[i] != results[i] {
			t.Errorf("observation %d = %+v, want %+v", i, seen[i], results[i])
		}
	}
	// A step skipped after a required failure is observed too: "we never got here" is a thing
	// the human needs told, and omitting it makes a skipped step indistinguishable from one
	// still running.
	if seen[2].Step != "lint" || seen[2].Outcome != Skipped {
		t.Errorf("third observation = %+v, want the skipped lint step", seen[2])
	}
}

// TestRunIsRunObservedWithoutAnObserver keeps the plain Runner contract from ARCHITECTURE §4.7
// intact: nothing about the sequence changes when nobody is watching.
func TestRunIsRunObservedWithoutAnObserver(t *testing.T) {
	r, h, wt := testRunner(t)
	steps := []core.Step{
		{Name: "build", Cmd: "echo building", Required: true},
		{Name: "test", Cmd: "exit 1", Required: true},
		{Name: "lint", Cmd: "echo linting", Required: true},
	}

	plain, err := r.Run(context.Background(), h, wt, steps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(plain) != 3 || plain[1].Outcome != Failed || plain[2].Outcome != Skipped {
		t.Fatalf("Run with no observer = %+v", plain)
	}

	watched, err := r.RunObserved(context.Background(), h, wt, steps, func(Observation) {})
	if err != nil {
		t.Fatalf("RunObserved: %v", err)
	}
	if len(watched) != len(plain) {
		t.Fatalf("watched %d steps, want %d", len(watched), len(plain))
	}
	// Duration and captured output differ run to run; what must not differ is which steps ran
	// and how they ended.
	for i := range plain {
		a, b := plain[i], watched[i]
		if a.Step != b.Step || a.Outcome != b.Outcome || a.ExitCode != b.ExitCode || a.Required != b.Required {
			t.Errorf("step %d observed = %+v, want the unobserved %+v", i, b, a)
		}
	}
}

// TestObserverSeesAStepStartBeforeItsCommandRuns is the correction this ticket was sent back for:
// the entry a human most wants is the one naming the step that is running now, and a journal that
// can only report finished steps is silent for exactly as long as the slow step takes.
//
// The start has to be reported before the command executes or it is just a differently worded
// result, so the step's command touches a marker and the assertion is made against the filesystem
// rather than against the order the observer happened to be called in.
func TestObserverSeesAStepStartBeforeItsCommandRuns(t *testing.T) {
	r, h, wt := testRunner(t)
	marker := filepath.Join(wt, "test-ran")

	var seen []Observation
	results, err := r.RunObserved(context.Background(), h, wt, []core.Step{
		{Name: "build", Cmd: "echo building", Required: true},
		{Name: "test", Cmd: "touch " + marker + "; exit 1", Required: true},
		{Name: "lint", Cmd: "echo linting", Required: true},
	}, func(at Observation) {
		if at.Step.Name == "test" {
			_, err := os.Stat(marker)
			switch at.Kind {
			case StepStarted:
				if err == nil {
					t.Error("the test step was announced only after its command had run")
				}
			case StepSettled:
				if err != nil {
					t.Errorf("the test step settled before its command ran: %v", err)
				}
			}
		}
		seen = append(seen, at)
	})
	if err != nil {
		t.Fatalf("RunObserved: %v", err)
	}

	// A skipped step never started, so it is reported once. Announcing one would put a step
	// that never ran on the Running screen as the thing currently happening.
	want := []Observation{
		{Kind: StepStarted, Index: 0, Total: 3},
		{Kind: StepSettled, Index: 0, Total: 3},
		{Kind: StepStarted, Index: 1, Total: 3},
		{Kind: StepSettled, Index: 1, Total: 3},
		{Kind: StepSettled, Index: 2, Total: 3},
	}
	if len(seen) != len(want) {
		t.Fatalf("got %d observations, want %d: %+v", len(seen), len(want), seen)
	}
	for i, w := range want {
		got := seen[i]
		if got.Kind != w.Kind || got.Index != w.Index || got.Total != w.Total {
			t.Errorf("observation %d = %s of step %d/%d, want %s of step %d/%d",
				i, got.Kind, got.Index, got.Total, w.Kind, w.Index, w.Total)
		}
	}

	// The start carries the configured step, because "running test (go test ./...)" is the
	// sentence, and a caller holding only a name would have to be handed the sequence too.
	if start := seen[2]; start.Step.Name != "test" || start.Step.Cmd == "" {
		t.Errorf("the start observation = %+v, want the configured step", start.Step)
	}
	if got := seen[4].Result.Outcome; got != Skipped {
		t.Errorf("the lint observation = %v, want the skipped result", got)
	}
	if n := len(results); n != 3 {
		t.Errorf("got %d results, want 3", n)
	}
}
