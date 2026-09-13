package host

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

func TestTerminalChecksUseWorktreeAndStopOnFailure(t *testing.T) {
	dir := t.TempDir()
	r := &TerminalRun{Dir: dir, Steps: []core.Step{{Name: "first", Cmd: "pwd; echo checked > checked.txt", Required: true}, {Name: "bad", Cmd: "exit 7", Required: true}, {Name: "later", Cmd: "touch should-not-exist", Required: true}}}
	var output bytes.Buffer
	r.SetStdin(strings.NewReader("\n"))
	r.SetStdout(&output)
	r.SetStderr(&output)
	if err := r.Run(); err == nil {
		t.Fatal("failed check reported success")
	}
	if _, err := os.Stat(filepath.Join(dir, "checked.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatal("continued past failed gate")
	}
	for _, want := range []string{dir, "exit status 7", "Press Enter"} {
		if !strings.Contains(output.String(), want) {
			t.Fatal(output.String())
		}
	}
}
func TestTerminalCheckTimeout(t *testing.T) {
	r := &TerminalRun{Dir: t.TempDir(), Steps: []core.Step{{Name: "slow", Cmd: "sleep 30", Timeout: 25 * time.Millisecond, Required: true}}}
	var output bytes.Buffer
	r.SetStdin(strings.NewReader("\n"))
	r.SetStdout(&output)
	r.SetStderr(&output)
	started := time.Now()
	err := r.Run()
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("%v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("timeout left child running")
	}
}
func TestTerminalPreviewRunsConfiguredApp(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	r := &TerminalRun{Dir: t.TempDir(), Interactive: true, Steps: []core.Step{{Name: "app", Cmd: "printf preview-started", Required: true}}}
	var output bytes.Buffer
	r.SetStdin(strings.NewReader("\n"))
	r.SetStdout(&output)
	r.SetStderr(&output)
	if err := r.Run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "preview-started") {
		t.Fatal(output.String())
	}
}

func TestValidationIsNoninteractive(t *testing.T) {
	r := &TerminalRun{Dir: t.TempDir()}
	r.SetStdout(os.Stdout)
	r.SetStderr(os.Stderr)
	if err := r.runStep(core.Step{Cmd: `test ! -t 0 && test ! -t 1 && test ! -t 2 && test "$CI" = true && test "$TERM" = dumb`}); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewInterrupt(t *testing.T) {
	r := &TerminalRun{Dir: t.TempDir(), Interactive: true}
	r.SetStdout(io.Discard)
	r.SetStderr(io.Discard)
	if err := r.runStep(core.Step{Cmd: "kill -INT $$"}); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("interrupt: %v", err)
	}
	if err := r.runStep(core.Step{Cmd: "exit 1"}); err == nil || errors.Is(err, ErrInterrupted) {
		t.Fatalf("failure: %v", err)
	}
}
