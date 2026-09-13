package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// ErrInterrupted indicates that the user stopped a review command.
var ErrInterrupted = errors.New("stopped")

// TerminalRun runs user-selected review commands in the current terminal, then
// keeps their output visible until Enter. It implements the TUI's terminal handoff
// interface without importing presentation code.
type TerminalRun struct {
	Dir            string
	Steps          []core.Step
	Services       []core.PreviewService
	Interactive    bool
	stdin          io.Reader
	stdout, stderr io.Writer
}

// SetStdin supplies the terminal input.
func (r *TerminalRun) SetStdin(in io.Reader) { r.stdin = in }

// SetStdout supplies terminal output.
func (r *TerminalRun) SetStdout(out io.Writer) { r.stdout = out }

// SetStderr supplies terminal error output.
func (r *TerminalRun) SetStderr(out io.Writer) { r.stderr = out }

// Run executes steps in order, stopping at the first required failure.
func (r *TerminalRun) Run() error {
	fmt.Fprintf(r.stdout, "Review worktree: %s\n", r.Dir)
	if r.Interactive {
		fmt.Fprintln(r.stdout, "Try the app using the URL or window it opens. Ctrl+C stops it.")
	}
	if len(r.Services) > 0 {
		err := r.runServices(context.Background())
		fmt.Fprintln(r.stdout, "\nPress Enter to return to Review.")
		_, _ = bufio.NewReader(r.stdin).ReadString('\n')
		return err
	}
	var failed error
	for _, step := range r.Steps {
		fmt.Fprintf(r.stdout, "\n%s: %s\n", step.Name, step.Cmd)
		err := r.runStep(step)
		if err != nil {
			fmt.Fprintf(r.stderr, "%s: %v\n", step.Name, err)
			failed = fmt.Errorf("%s: %w", step.Name, err)
			if step.Required {
				break
			}
		} else {
			fmt.Fprintf(r.stdout, "\n%s finished successfully\n", step.Name)
		}
	}
	fmt.Fprintln(r.stdout, "\nPress Enter to return to Review.")
	_, _ = bufio.NewReader(r.stdin).ReadString('\n')
	return failed
}
func (r *TerminalRun) runStep(step core.Step) error {
	if r.Interactive {
		cmd := LocalCommand(Shell(), []string{"-c", step.Cmd}, r.Dir)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = r.stdin, r.stdout, r.stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) && signalNumber(exit) == 2 {
			return ErrInterrupted
		}
		return err
	}
	timeout := step.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	interruptCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(interruptCtx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", step.Cmd)
	// Writer-only wrappers force pipes, so test tools cannot detect a terminal
	// and start pagers or interactive prompts in their background process group.
	cmd.Dir, cmd.Stdout, cmd.Stderr = r.Dir, struct{ io.Writer }{r.stdout}, struct{ io.Writer }{r.stderr}
	cmd.Env = append(os.Environ(), "CI=true", "TERM=dumb")
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid) }
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timed out after %s", timeout)
	}
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	return err
}
