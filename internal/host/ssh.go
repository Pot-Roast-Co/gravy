package host

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/pot-roast-co/gravy/internal/core"
)

// SSHHost runs work on another machine over ssh.
//
// It is the whole of what ARCHITECTURE.md §10 promised: the scheduler already filters on Caps
// and already handles several hosts, so a second machine is one implementation of Host and no
// change anywhere else.
//
// Addressing is left to ssh. A target is whatever `ssh` accepts — usually a Host alias from
// ~/.ssh/config — so keys, ports, jump hosts and agent forwarding are configured where the user
// already configures them, and Gravy neither parses nor duplicates any of it.
//
// Each machine has its own clone and its own worktrees. There is no shared filesystem, and paths
// on this host mean nothing on that one.
type SSHHost struct {
	id     string
	target string
	total  int
	fs     FS
	// local runs the ssh client. Delegating rather than duplicating means remote commands get
	// the same pipes, timeout and process-group kill that local ones do — and killing the ssh
	// client closes the connection, which is what takes the far end down with it.
	local *LocalHost

	mu   sync.Mutex
	used int
}

// NewSSH returns a host reachable at an ssh target.
func NewSSH(id, target string, slots int) *SSHHost {
	if slots < 1 {
		slots = 1
	}
	h := &SSHHost{id: id, target: target, total: slots, local: NewLocal(id+"-ssh", slots)}
	h.fs = sshFS{h: h}
	return h
}

// ID returns the host's identifier.
func (h *SSHHost) ID() string { return h.id }

// Target is the ssh destination, for messages that need to name it.
func (h *SSHHost) Target() string { return h.target }

// FS returns file access on the remote machine.
func (h *SSHHost) FS() FS { return h.fs }

// Slots reports worker slot usage.
func (h *SSHHost) Slots() (used, total int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.used, h.total
}

// TryClaim takes a worker slot, reporting whether one was free.
func (h *SSHHost) TryClaim() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.used >= h.total {
		return false
	}
	h.used++
	return true
}

// Release returns a worker slot.
func (h *SSHHost) Release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.used > 0 {
		h.used--
	}
}

// sshArgs builds the ssh invocation for a remote command line.
//
// BatchMode refuses to prompt: a daemon has no terminal to answer a passphrase on, and a run
// that hangs invisibly on a password prompt is worse than one that fails saying so.
func (h *SSHHost) sshArgs(remote string) []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
		// The remote end must die when the connection does, or a killed run leaves an agent
		// running on the other machine with nothing watching it.
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		h.target,
		remote,
	}
}

// remoteCommand renders an ExecSpec as a single line for a login, interactive shell.
//
// Both flags are load-bearing, and getting them wrong is silent. An agent CLI is put on PATH by
// shell startup files, and which file depends on the shell's mode: zsh reads .zprofile for a
// login shell but .zshrc only for an interactive one. A Mac with claude in ~/.local/bin and
// codex in /opt/homebrew/bin, both added by .zshrc, reports neither under `zsh -lc` and both
// under `zsh -lic` — so a login-only shell finds nothing on a machine where the same command
// works perfectly in a terminal.
func remoteCommand(spec ExecSpec) string {
	var b strings.Builder
	for k, v := range spec.Env {
		fmt.Fprintf(&b, "export %s=%s; ", k, shellQuote(v))
	}
	if spec.Dir != "" {
		fmt.Fprintf(&b, "cd %s && ", shellQuote(spec.Dir))
	}
	b.WriteString(shellQuote(spec.Cmd))
	for _, a := range spec.Args {
		b.WriteString(" ")
		b.WriteString(shellQuote(a))
	}
	// exec so the shell is replaced: one fewer process between ssh and the agent, and signals
	// reach what they are aimed at.
	return "exec $SHELL -lic " + shellQuote(b.String())
}

// shellQuote makes a string safe as one argument to a POSIX shell.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Exec runs a command on the remote machine.
func (h *SSHHost) Exec(ctx context.Context, spec ExecSpec) (Process, error) {
	if spec.Cmd == "" {
		return nil, fmt.Errorf("exec: no command given")
	}
	// The timeout travels with the ssh client: killing it drops the connection, and the far
	// end goes with it.
	return h.local.Exec(ctx, ExecSpec{
		Cmd:     "ssh",
		Args:    h.sshArgs(remoteCommand(spec)),
		Stdin:   spec.Stdin,
		Timeout: spec.Timeout,
	})
}

// StartDetached is not supported over ssh.
//
// It exists so a client can bring up a daemon without importing os/exec, and a daemon belongs on
// the machine the human is at. Saying so beats starting something on the far end that nothing
// will ever reconnect to.
func (h *SSHHost) StartDetached(ExecSpec, string) (int, error) {
	return 0, fmt.Errorf("host %q is remote: a daemon cannot be started on it", h.id)
}

// Capabilities probes the remote machine.
//
// One round trip, not one per tool: every probe is a line in a single shell script, because a
// connection setup per tool turns capability detection into a visible pause on a slow link.
func (h *SSHHost) Capabilities(ctx context.Context) (core.Caps, error) {
	var script strings.Builder
	script.WriteString(`printf 'os=%s\n' "$(uname -s)"; printf 'arch=%s\n' "$(uname -m)"; `)
	// Physical memory: darwin and linux disagree about where it lives, so both are tried and
	// whichever answers wins.
	script.WriteString(`printf 'ram=%s\n' "$(sysctl -n hw.memsize 2>/dev/null || ` +
		`awk '/MemTotal/ {print $2 * 1024}' /proc/meminfo 2>/dev/null)"; `)
	for _, t := range probedTools {
		fmt.Fprintf(&script, `command -v %s >/dev/null 2>&1 && printf 'tool=%s\n'; `, t.name, t.name)
	}
	for _, p := range []string{"claude", "codex"} {
		fmt.Fprintf(&script, `command -v %s >/dev/null 2>&1 && printf 'provider=%s\n'; `, p, p)
	}
	// The script's status is its last command's, and the last command is a probe that fails
	// whenever the thing it looks for is absent. Without this, a machine with no agent CLI —
	// exactly the machine worth reporting as having none — fails detection outright.
	script.WriteString("exit 0")

	out, err := h.run(ctx, ExecSpec{Cmd: "sh", Args: []string{"-c", script.String()}})
	if err != nil {
		return core.Caps{}, fmt.Errorf("host %q (%s): %w", h.id, h.target, err)
	}

	caps := core.Caps{Tools: map[string]string{}, Providers: map[string]bool{}}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "os":
			caps.OS = normalizeOS(value)
		case "arch":
			caps.Arch = normalizeArch(value)
		case "ram":
			if n, err := strconv.ParseUint(value, 10, 64); err == nil {
				caps.RAMBytes = n
			}
		case "tool":
			// Versions are deliberately not collected: each would be another command, and
			// nothing in v0.1 filters on a minimum version.
			caps.Tools[value] = ""
		case "provider":
			caps.Providers[providerIDFor(value)] = true
		}
	}
	if caps.OS == "" {
		return core.Caps{}, fmt.Errorf("host %q (%s) did not report an OS", h.id, h.target)
	}
	return caps, nil
}

// Home is the remote user's home directory.
//
// Probed rather than assumed: worktrees for a project on this host live under it, and the path
// cannot be written as "$HOME" because every argument is shell-quoted on the way over — which is
// what stops a path with a space in it from becoming two arguments.
func (h *SSHHost) Home(ctx context.Context) (string, error) {
	out, err := h.run(ctx, ExecSpec{Cmd: "sh", Args: []string{"-c", `printf %s "$HOME"`}})
	if err != nil {
		return "", fmt.Errorf("host %q: read home directory: %w", h.id, err)
	}
	home := strings.TrimSpace(out)
	if home == "" {
		return "", fmt.Errorf("host %q reported no home directory", h.id)
	}
	return home, nil
}

// run executes a command and returns its stdout, for the short probes this type makes itself.
func (h *SSHHost) run(ctx context.Context, spec ExecSpec) (string, error) {
	p, err := h.Exec(ctx, spec)
	if err != nil {
		return "", err
	}
	outCh := drain(p.Stdout())
	errCh := drain(p.Stderr())
	st, err := p.Wait()
	out, stderr := <-outCh, <-errCh
	if err != nil {
		return "", err
	}
	if st.Code != 0 {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = strings.TrimSpace(out)
		}
		return "", fmt.Errorf("exit %d: %s", st.Code, detail)
	}
	return out, nil
}

// drain reads a stream without blocking the caller.
func drain(r io.Reader) <-chan string {
	ch := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		ch <- string(b)
	}()
	return ch
}

// normalizeOS maps uname's answer onto the names core.Caps uses, which are Go's.
func normalizeOS(uname string) string {
	switch strings.ToLower(strings.TrimSpace(uname)) {
	case "darwin":
		return "darwin"
	case "linux":
		return "linux"
	case "":
		return ""
	default:
		return strings.ToLower(strings.TrimSpace(uname))
	}
}

// normalizeArch maps uname -m onto Go's names, so a requirement written against one host reads
// the same against another.
func normalizeArch(uname string) string {
	switch strings.ToLower(strings.TrimSpace(uname)) {
	case "x86_64", "amd64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return strings.ToLower(strings.TrimSpace(uname))
	}
}

// providerIDFor maps a CLI's command name to the provider id that drives it.
func providerIDFor(command string) string {
	if command == "claude" {
		return "claude-code"
	}
	return command
}

// sshFS is file access on the remote machine, expressed as shell commands.
//
// Deliberately small: only what contextbuild and the run pipeline actually ask for. Anything
// needing more is better served by a gravy-worker service than by growing this.
type sshFS struct{ h *SSHHost }

func (f sshFS) ReadFile(path string) ([]byte, error) {
	out, err := f.h.run(context.Background(), ExecSpec{Cmd: "cat", Args: []string{path}})
	if err != nil {
		return nil, fmt.Errorf("read %s on %s: %w", path, f.h.id, err)
	}
	return []byte(out), nil
}

func (f sshFS) WriteFile(path string, b []byte, _ os.FileMode) error {
	spec := ExecSpec{
		Cmd:   "sh",
		Args:  []string{"-c", "cat > " + shellQuote(path)},
		Stdin: strings.NewReader(string(b)),
	}
	if _, err := f.h.run(context.Background(), spec); err != nil {
		return fmt.Errorf("write %s on %s: %w", path, f.h.id, err)
	}
	return nil
}

func (f sshFS) MkdirAll(path string, _ os.FileMode) error {
	if _, err := f.h.run(context.Background(), ExecSpec{Cmd: "mkdir", Args: []string{"-p", path}}); err != nil {
		return fmt.Errorf("mkdir %s on %s: %w", path, f.h.id, err)
	}
	return nil
}

func (f sshFS) RemoveAll(path string) error {
	if _, err := f.h.run(context.Background(), ExecSpec{Cmd: "rm", Args: []string{"-rf", path}}); err != nil {
		return fmt.Errorf("remove %s on %s: %w", path, f.h.id, err)
	}
	return nil
}

// Stat is not supported: nothing in the run pipeline stats a remote path, and returning a fake
// os.FileInfo would be a lie the caller could not detect. Exists answers the question that is
// actually asked.
func (f sshFS) Stat(path string) (os.FileInfo, error) {
	return nil, fmt.Errorf("stat %s on %s: not supported over ssh", path, f.h.id)
}

func (f sshFS) Exists(path string) bool {
	_, err := f.h.run(context.Background(), ExecSpec{Cmd: "test", Args: []string{"-e", path}})
	return err == nil
}
