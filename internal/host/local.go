package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
)

// LocalHost runs work as the current user on this machine.
//
// Gravy does not sandbox agents (PRODUCT.md): the real boundaries are the worktree, the
// permission allowlist and the merge gate. What this type does provide is a timeout, a turn cap
// enforced by its caller, and a kill switch that takes the whole process group with it.
type LocalHost struct {
	id    string
	total int
	fs    FS

	mu   sync.Mutex
	used int

	capsCache capsCache
}

// NewLocal returns a LocalHost with the given number of worker slots.
func NewLocal(id string, slots int) *LocalHost {
	if slots < 1 {
		slots = 1
	}
	return &LocalHost{id: id, total: slots, fs: localFS{}}
}

// ID returns the host's identifier.
func (h *LocalHost) ID() string { return h.id }

// FS returns file access on this machine.
func (h *LocalHost) FS() FS { return h.fs }

// Slots reports worker slot usage.
func (h *LocalHost) Slots() (used, total int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.used, h.total
}

// TryClaim takes a worker slot, reporting whether one was free.
func (h *LocalHost) TryClaim() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.used >= h.total {
		return false
	}
	h.used++
	return true
}

// Release returns a worker slot.
//
// Releasing more than was claimed is a bug in the caller, not something to paper over: a slot
// count that drifts upward would let a project exceed its concurrency cap silently.
func (h *LocalHost) Release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.used == 0 {
		panic("host: Release called with no slots claimed")
	}
	h.used--
}

// Exec starts a command and returns immediately; output streams as it is produced.
func (h *LocalHost) Exec(ctx context.Context, spec ExecSpec) (Process, error) {
	if spec.Cmd == "" {
		return nil, errors.New("exec: no command given")
	}

	cmd := exec.Command(spec.Cmd, spec.Args...) //nolint:gosec // running configured commands is this package's purpose
	cmd.Dir = spec.Dir
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), envSlice(spec.Env)...)
	}
	cmd.Stdin = spec.Stdin
	setProcessGroup(cmd)

	// os.Pipe rather than cmd.StdoutPipe: Wait closes the pipes StdoutPipe returns, which
	// races a consumer that has not finished reading. With our own pipes the read ends stay
	// open until the caller closes them, and EOF arrives when the child exits and the parent's
	// copy of the write end is closed.
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("exec %s: stdout pipe: %w", spec.Cmd, err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("exec %s: stderr pipe: %w", spec.Cmd, err)
	}
	cmd.Stdout = outW
	cmd.Stderr = errW

	started := time.Now()
	if err := cmd.Start(); err != nil {
		outR.Close()
		outW.Close()
		errR.Close()
		errW.Close()
		return nil, fmt.Errorf("exec %s: %w", spec.Cmd, err)
	}
	// The child holds its own descriptors now; the parent's copies must go, or the reader
	// never sees EOF.
	outW.Close()
	errW.Close()

	p := &localProcess{
		cmd:     cmd,
		stdout:  outR,
		stderr:  errR,
		started: started,
		done:    make(chan struct{}),
	}
	p.watch(ctx, spec.Timeout)
	return p, nil
}

// localProcess is a running command.
type localProcess struct {
	cmd            *exec.Cmd
	stdout, stderr *os.File
	started        time.Time
	done           chan struct{}

	mu       sync.Mutex
	timedOut bool
	killed   bool

	waitOnce sync.Once
	status   ExitStatus
	waitErr  error
}

func (p *localProcess) Stdout() io.Reader { return p.stdout }
func (p *localProcess) Stderr() io.Reader { return p.stderr }

// PID returns the process id, which is recorded on the run row so a restarted daemon can tell an
// orphaned run from a live one.
func (p *localProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// watch kills the process group when the context is cancelled or the timeout elapses.
func (p *localProcess) watch(ctx context.Context, timeout time.Duration) {
	if ctx.Done() == nil && timeout <= 0 {
		return
	}
	var timer *time.Timer
	var timeoutC <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		timeoutC = timer.C
	}
	go func() {
		if timer != nil {
			defer timer.Stop()
		}
		select {
		case <-p.done:
			return
		case <-timeoutC:
			p.mu.Lock()
			p.timedOut = true
			p.mu.Unlock()
			_ = p.Kill()
		case <-ctx.Done():
			_ = p.Kill()
		}
	}()
}

// Kill terminates the process and everything it started.
//
// The child runs in its own process group, so signalling the negated pid reaches grandchildren
// too. An agent CLI that spawns compilers and test runners would otherwise leave them behind,
// holding the worktree open and burning CPU after the run is gone.
func (p *localProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()

	if err := killGroup(p.cmd.Process.Pid); err != nil {
		// Falling back to the single process is better than giving up: the run still ends.
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("kill process %d: %w", p.cmd.Process.Pid, err)
		}
	}
	return nil
}

// Wait blocks until the process exits and reports how it ended. It is safe to call more than
// once and returns the same result each time.
func (p *localProcess) Wait() (ExitStatus, error) {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		close(p.done)

		p.mu.Lock()
		timedOut, killed := p.timedOut, p.killed
		p.mu.Unlock()

		st := ExitStatus{Duration: time.Since(p.started), TimedOut: timedOut}

		var exitErr *exec.ExitError
		switch {
		case err == nil:
			st.Code = 0
		case errors.As(err, &exitErr):
			st.Code = exitErr.ExitCode()
			// A signalled process reports -1; record that it was signalled rather than
			// inventing an exit code.
			if st.Code < 0 {
				st.Signaled = true
				st.Code = 128 + signalNumber(exitErr)
			}
		default:
			p.status, p.waitErr = st, fmt.Errorf("wait: %w", err)
			return
		}
		if killed {
			st.Signaled = true
		}
		p.status = st
	})
	return p.status, p.waitErr
}

// Capabilities describes this machine, for filtering which hosts can run a project's tickets.
func (h *LocalHost) Capabilities(ctx context.Context) (core.Caps, error) {
	// Cheaper than ssh, but not free: each probe spawns a process per tool, and the dashboard
	// asks on every refresh.
	return h.capsCache.get(ctx, h.probeCapabilities)
}

// Reachability reports the local machine as reachable once it has been probed. It is the
// machine Gravy is running on: if it were not answering, nothing would be asking.
func (h *LocalHost) Reachability() Reachability { return h.capsCache.state() }

// Recheck re-probes this machine's toolchain. Cheap, and the honest answer to a human asking
// Gravy to look again after installing something.
func (h *LocalHost) Recheck(ctx context.Context) (core.Caps, error) {
	return h.capsCache.recheck(ctx, h.probeCapabilities)
}

func (h *LocalHost) probeCapabilities(ctx context.Context) (core.Caps, error) {
	caps := core.Caps{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		RAMBytes:  memoryBytes(ctx),
		Tools:     detectTools(ctx),
		Providers: map[string]bool{},
		// GPU is left empty: there is no probe on either platform that is both cheap and
		// reliable, and nothing in v0.1 filters on it. Better blank than confidently wrong.
	}
	return caps, nil
}

// probedTools are the tools worth knowing about when matching a host to a project's
// requirements. Each is probed once, concurrently.
var probedTools = []struct {
	name string
	args []string
}{
	{"git", []string{"--version"}},
	{"go", []string{"version"}},
	{"node", []string{"--version"}},
	{"python3", []string{"--version"}},
	{"docker", []string{"--version"}},
	{"swift", []string{"--version"}},
	{"xcodebuild", []string{"-version"}},
}

// detectTools probes for each tool's presence and version. A tool that is absent is simply not
// in the map; a tool that is present but whose version cannot be parsed maps to "".
func detectTools(ctx context.Context) map[string]string {
	// A missing tool should not cost the caller a stall, and xcodebuild in particular can be
	// slow when the command line tools are half-installed.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	type result struct {
		name, version string
		found         bool
	}
	results := make(chan result, len(probedTools))

	var wg sync.WaitGroup
	for _, t := range probedTools {
		wg.Add(1)
		go func(name string, args []string) {
			defer wg.Done()
			path, err := exec.LookPath(name)
			if err != nil {
				results <- result{name: name}
				return
			}
			out, err := exec.CommandContext(ctx, path, args...).Output() //nolint:gosec // fixed probe list
			if err != nil {
				// Present but unhappy still counts as installed.
				results <- result{name: name, found: true}
				return
			}
			results <- result{name: name, version: parseVersion(string(out)), found: true}
		}(t.name, t.args)
	}
	wg.Wait()
	close(results)

	tools := map[string]string{}
	for r := range results {
		if r.found {
			tools[r.name] = r.version
		}
	}
	return tools
}

// parseVersion pulls the first dotted number out of a version banner, which covers
// "git version 2.39.5", "go version go1.23.2 darwin/arm64", "v22.11.0" and "Python 3.12.1".
func parseVersion(s string) string {
	line := s
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	for _, field := range strings.Fields(line) {
		f := strings.TrimPrefix(field, "v")
		f = strings.TrimPrefix(f, "go")
		if f == "" {
			continue
		}
		if f[0] < '0' || f[0] > '9' || !strings.Contains(f, ".") {
			continue
		}
		return strings.TrimRight(f, ",;")
	}
	return ""
}

// memoryBytes reports physical RAM, or 0 when it cannot be determined.
func memoryBytes(ctx context.Context) uint64 {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return 0
		}
		return n
	case "linux":
		b, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, "MemTotal:") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0
			}
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return kb * 1024
		}
	}
	return 0
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// StartDetached launches a process that outlives this one.
//
// Nothing is piped: the child gets the log file for stdout and stderr and /dev/null for stdin, so
// its writes cannot block on a reader that has gone away — a daemon started by a TUI must not die
// when that TUI exits. setProcessGroup puts it in its own group for the same reason, so a signal
// sent to the starter's group does not reach it.
func (h *LocalHost) StartDetached(spec ExecSpec, logPath string) (int, error) {
	if spec.Cmd == "" {
		return 0, errors.New("start: no command given")
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return 0, fmt.Errorf("start %s: log directory: %w", spec.Cmd, err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("start %s: open %s: %w", spec.Cmd, logPath, err)
	}
	defer logFile.Close()

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("start %s: %w", spec.Cmd, err)
	}
	defer devNull.Close()

	cmd := exec.Command(spec.Cmd, spec.Args...) //nolint:gosec // running configured commands is this package's purpose
	cmd.Dir = spec.Dir
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), envSlice(spec.Env)...)
	}
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setNewSession(cmd)

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", spec.Cmd, err)
	}
	// Release rather than Wait: the caller is not this process's parent in any meaningful
	// sense, and waiting would defeat the point. The child is reparented to init on exit.
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return pid, fmt.Errorf("start %s: release: %w", spec.Cmd, err)
	}
	return pid, nil
}
