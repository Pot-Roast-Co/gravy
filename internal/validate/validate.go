package validate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
)

// Step is one validation command. It is core's type, aliased so callers can depend on this
// package alone.
type Step = core.Step

// Outcome is how a single step ended.
type Outcome string

// The step outcomes.
const (
	// Passed means the command exited zero.
	Passed Outcome = "passed"
	// Failed means it exited non-zero.
	Failed Outcome = "failed"
	// TimedOut means it exceeded the step's timeout and was killed.
	TimedOut Outcome = "timed_out"
	// Skipped means an earlier required step failed, so this one never ran.
	//
	// Recorded rather than omitted: "we never got here" and "this passed" must not look alike
	// when a human is reading why a ticket was parked.
	Skipped Outcome = "skipped"
)

// Result is the outcome of one validation step.
type Result struct {
	Step     string
	Outcome  Outcome
	ExitCode int
	// Output is tail-capped for storage and display; the full log is at LogPath.
	Output   string
	LogPath  string
	Duration time.Duration
	// Required mirrors the step's setting, so a reader of the results alone can tell whether a
	// failure was fatal.
	Required bool
}

// Failed reports whether this step failed in a way that matters.
func (r Result) Failed() bool { return r.Outcome == Failed || r.Outcome == TimedOut }

// Results is an ordered set of step results.
type Results []Result

// Green reports whether validation passed: every required step succeeded.
//
// A failed optional step is a warning, not a failure — that is the entire point of marking a
// step optional.
func (rs Results) Green() bool {
	for _, r := range rs {
		if r.Required && r.Failed() {
			return false
		}
	}
	return true
}

// FirstFailure returns the required step that stopped the sequence, if any.
func (rs Results) FirstFailure() (Result, bool) {
	for _, r := range rs {
		if r.Required && r.Failed() {
			return r, true
		}
	}
	return Result{}, false
}

// Warnings returns failed optional steps.
func (rs Results) Warnings() []Result {
	var out []Result
	for _, r := range rs {
		if !r.Required && r.Failed() {
			out = append(out, r)
		}
	}
	return out
}

// Summary renders the results for a human in one line per step.
func (rs Results) Summary() string {
	var b strings.Builder
	for i, r := range rs {
		if i > 0 {
			b.WriteByte('\n')
		}
		mark := "ok"
		switch r.Outcome {
		case Failed:
			mark = fmt.Sprintf("FAILED (exit %d)", r.ExitCode)
		case TimedOut:
			mark = "TIMED OUT"
		case Skipped:
			mark = "skipped"
		}
		if !r.Required && r.Failed() {
			mark += " (optional)"
		}
		fmt.Fprintf(&b, "%-12s %s", r.Step, mark)
	}
	return b.String()
}

// Runner runs a project's validation steps.
type Runner interface {
	Run(ctx context.Context, h host.Host, worktree string, steps []Step) (Results, error)
}

// DefaultTailBytes is how much step output is kept inline. The full output always goes to the
// step's log file.
const DefaultTailBytes = 8 * 1024

// DefaultStepTimeout applies to a step that does not set its own.
const DefaultStepTimeout = 10 * time.Minute

// StepRunner runs validation steps through a Host.
type StepRunner struct {
	// LogDir is where per-step logs are written, normally ~/.gravy/runs/<run-id>/validation.
	// When empty, no log files are written and only the tail is kept.
	LogDir string
	// TailBytes overrides DefaultTailBytes.
	TailBytes int
	// Shell is the interpreter commands run under.
	Shell string
}

// NewRunner returns a runner writing logs to logDir.
func NewRunner(logDir string) *StepRunner {
	return &StepRunner{LogDir: logDir, TailBytes: DefaultTailBytes, Shell: "sh"}
}

// Run executes the steps in order inside the worktree.
//
// It stops at the first failed required step: later steps are recorded as Skipped rather than
// run, because once the build is broken the test results are noise, and running them wastes
// minutes the human is waiting on. A failed optional step records a warning and the sequence
// continues.
func (r *StepRunner) Run(ctx context.Context, h host.Host, worktree string, steps []Step) (Results, error) {
	if worktree == "" {
		return nil, fmt.Errorf("validate: no worktree given")
	}

	results := make(Results, 0, len(steps))
	stopped := false

	for _, step := range steps {
		if stopped {
			results = append(results, Result{
				Step: step.Name, Outcome: Skipped, Required: step.Required,
			})
			continue
		}

		res, err := r.runStep(ctx, h, worktree, step)
		if err != nil {
			return results, err
		}
		results = append(results, res)

		if res.Required && res.Failed() {
			stopped = true
		}
	}
	return results, nil
}

// runStep executes one command and captures its output.
func (r *StepRunner) runStep(ctx context.Context, h host.Host, worktree string, step Step) (Result, error) {
	timeout := step.Timeout
	if timeout <= 0 {
		timeout = DefaultStepTimeout
	}
	shell := r.Shell
	if shell == "" {
		shell = "sh"
	}

	res := Result{Step: step.Name, Required: step.Required}

	// Commands are shell strings ("go test ./...", "npm run build"), so they run under a shell
	// rather than being split on whitespace, which would break quoting and pipelines.
	proc, err := h.Exec(ctx, host.ExecSpec{
		Cmd:     shell,
		Args:    []string{"-c", step.Cmd},
		Dir:     worktree,
		Timeout: timeout,
		Env: map[string]string{
			// Tell tooling it is not on a terminal, so nothing waits for input or emits
			// progress spinners into the log.
			"CI":   "true",
			"TERM": "dumb",
		},
	})
	if err != nil {
		return res, fmt.Errorf("validate %s: %w", step.Name, err)
	}

	logPath := r.logPath(step)
	output, err := captureBoth(proc, logPath, r.tailBytes())
	if err != nil {
		return res, fmt.Errorf("validate %s: %w", step.Name, err)
	}
	status, waitErr := proc.Wait()
	if waitErr != nil {
		return res, fmt.Errorf("validate %s: %w", step.Name, waitErr)
	}

	res.ExitCode = status.Code
	res.Duration = status.Duration
	res.Output = output
	res.LogPath = logPath

	switch {
	case status.TimedOut:
		res.Outcome = TimedOut
	case status.Code != 0:
		res.Outcome = Failed
	default:
		res.Outcome = Passed
	}
	return res, nil
}

func (r *StepRunner) tailBytes() int {
	if r.TailBytes > 0 {
		return r.TailBytes
	}
	return DefaultTailBytes
}

func (r *StepRunner) logPath(step Step) string {
	if r.LogDir == "" {
		return ""
	}
	return filepath.Join(r.LogDir, safeFileName(step.Name)+".log")
}

// safeFileName reduces a step name to something usable as a filename.
func safeFileName(name string) string {
	var b strings.Builder
	for _, ch := range strings.ToLower(name) {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '-', ch == '_':
			b.WriteRune(ch)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "step"
	}
	return out
}

// captureBoth streams stdout and stderr to the log file and returns the tail.
//
// Both streams are drained concurrently: a command that fills one pipe while we read the other
// would deadlock, and a build that logs heavily to stderr is entirely ordinary.
func captureBoth(proc host.Process, logPath string, tailBytes int) (string, error) {
	var logFile *os.File
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			return "", fmt.Errorf("create log directory: %w", err)
		}
		f, err := os.Create(logPath)
		if err != nil {
			return "", fmt.Errorf("create log: %w", err)
		}
		logFile = f
		defer f.Close()
	}

	outCh := drainInto(proc.Stdout(), logFile, tailBytes)
	errCh := drainInto(proc.Stderr(), logFile, tailBytes)
	out, errOut := <-outCh, <-errCh

	combined := out
	if errOut != "" {
		if combined != "" {
			combined += "\n"
		}
		combined += errOut
	}
	return tail(combined, tailBytes), nil
}

// drainInto reads a stream to completion, mirroring it into the log file, and returns its tail.
func drainInto(r io.Reader, log *os.File, tailBytes int) <-chan string {
	ch := make(chan string, 1)
	go func() {
		var buf strings.Builder
		chunk := make([]byte, 32*1024)
		for {
			n, err := r.Read(chunk)
			if n > 0 {
				if log != nil {
					_, _ = log.Write(chunk[:n])
				}
				buf.Write(chunk[:n])
				// Keep memory bounded on a runaway build: only the tail is ever used
				// inline, and the full output is already in the log.
				if buf.Len() > tailBytes*4 {
					trimmed := tail(buf.String(), tailBytes*2)
					buf.Reset()
					buf.WriteString(trimmed)
				}
			}
			if err != nil {
				break
			}
		}
		ch <- buf.String()
	}()
	return ch
}

// tail returns the last n bytes, cut at a line boundary so output does not begin mid-token.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return "…\n" + s
}

var _ Runner = (*StepRunner)(nil)
