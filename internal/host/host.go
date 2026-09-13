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
	// Capabilities describes the machine. The first call for a host may connect and block;
	// every call after it must answer from memory, refreshing behind the caller if what it
	// holds has gone stale. Callers on a request path — the scheduler's tick, the dashboard —
	// rely on that, because a machine that is switched off does not refuse a connection, it
	// simply never answers, and probing one inline stalls whatever asked.
	Capabilities(ctx context.Context) (core.Caps, error)
	// Reachability reports what Gravy last learned about this machine, from memory. It never
	// touches the network, so a status screen can name every host that is off without waiting
	// on any of them.
	Reachability() Reachability
	// Recheck probes now and waits, whatever is cached. It is the human saying they have just
	// turned the machine back on.
	Recheck(ctx context.Context) (core.Caps, error)
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
	Cmd  string
	Args []string
	Dir  string
	Env  map[string]string
	// UnsetEnv removes inherited and explicitly supplied variables. Removal wins.
	UnsetEnv []string
	Timeout  time.Duration
	Stdin    io.Reader
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

// Reachability is what Gravy last learned about whether a machine answers.
//
// It is remembered rather than discovered on demand: a configured host that is switched off
// must be reportable as "off" instantly and indefinitely, without the act of reporting it
// costing a connection timeout.
type Reachability struct {
	// Online is true only when the last probe succeeded. A host that has never been probed is
	// neither online nor known to be off, which is why Err and CheckedAt matter.
	Online bool
	// Err is why the last probe failed, verbatim from ssh, so the human is told "connection
	// timed out" rather than "unreachable".
	Err string
	// CheckedAt is when that was learned. Zero means never probed.
	CheckedAt time.Time
	// Checking is true while a probe is in flight, so the UI can say so rather than appearing
	// to have ignored the keystroke.
	Checking bool
}

// Off reports whether this host is known not to answer.
func (r Reachability) Off() bool { return !r.Online && !r.CheckedAt.IsZero() }
