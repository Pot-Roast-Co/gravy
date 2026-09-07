package host

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// Host is a machine Gravy can run work on.
//
// Everything that executes a command or touches a working tree goes through this interface. That
// is the whole point: a remote implementation drops in beneath an unchanged scheduler, run
// orchestrator, provider layer and UI, so multi-machine stays a later addition rather than a
// rewrite (ARCHITECTURE.md §1.1).
type Host interface {
	ID() string
	Capabilities(ctx context.Context) (core.Caps, error)
	Exec(ctx context.Context, spec ExecSpec) (Process, error)
	// StartDetached launches a process that outlives the caller, appending its output to
	// logPath, and returns its pid. It exists so a client can bring up a daemon without
	// importing os/exec, which ARCHITECTURE.md 1.1 permits only here.
	StartDetached(spec ExecSpec, logPath string) (int, error)
	FS() FS
	Slots() (used, total int)
}

// ExecSpec describes a command to run.
type ExecSpec struct {
	Cmd     string
	Args    []string
	Dir     string
	Env     map[string]string
	Timeout time.Duration
	Stdin   io.Reader
}

// Process is a running command. Output streams as it is produced; it is never buffered to
// completion, because the TUI shows a run live and a wedged run must be visible as one.
type Process interface {
	Stdout() io.Reader
	Stderr() io.Reader
	Wait() (ExitStatus, error)
	Kill() error
	PID() int
}

// ExitStatus is how a process ended.
type ExitStatus struct {
	Code     int
	Signaled bool
	Duration time.Duration
	// TimedOut reports that the process was killed because ExecSpec.Timeout elapsed.
	//
	// This is distinct from a generic error on purpose: a timeout classifies as core.Timeout
	// and is retried once, whereas an unexplained failure is a task failure. It is also the
	// primary detector of a wedged provider, since a CLI can retry silently for minutes
	// without exiting (docs/SPIKE-claude-code.md, F2).
	TimedOut bool
}

// FS is file access on a host.
type FS interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, b []byte, perm os.FileMode) error
	Stat(path string) (os.FileInfo, error)
	MkdirAll(path string, perm os.FileMode) error
	RemoveAll(path string) error
	Exists(path string) bool
}
