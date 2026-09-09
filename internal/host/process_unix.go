//go:build unix

package host

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group, so that killing the group reaches
// every process it starts. An agent CLI spawns compilers, test runners and language servers;
// without this they survive the run that started them.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup sends SIGKILL to the whole process group led by pid.
//
// SIGKILL rather than SIGTERM: this is the kill switch, reached on a timeout, a cancelled run,
// or a human pressing kill in the TUI. A provider CLI that ignores SIGTERM while retrying
// internally would otherwise keep its worker slot.
//
// ESRCH is returned rather than swallowed. It means "no such process group", which is true both
// when the group has already exited and when the child was never put in its own group at all —
// and those need opposite responses. Reporting success for the second case would leave the
// process running while Kill believed it had done its job, so the caller falls back to killing
// the process directly and lets an already-exited process report os.ErrProcessDone.
func killGroup(pid int) error {
	if pid <= 0 {
		return errors.New("kill group: invalid pid")
	}
	// A negative pid signals the process group.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill process group %d: %w", pid, err)
	}
	return nil
}

// signalNumber extracts the signal that ended a process, for reporting 128+n as the exit code.
func signalNumber(err *exec.ExitError) int {
	if ws, ok := err.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return int(ws.Signal())
	}
	return int(syscall.SIGKILL)
}

// Alive reports whether a process is still running.
//
// Signal 0 performs the permission and existence checks without delivering anything, which is
// the portable way to ask. EPERM counts as alive: the process exists, it simply is not ours.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Terminate asks a process to shut down cleanly.
//
// SIGTERM, not SIGKILL: the daemon holds a SQLite database and a listening socket, and a signal
// it can handle lets it flush and unlink both. Killing it outright leaves a stale pidfile and,
// worse, a database that has to be recovered on the next open.
//
// A process that is already gone is not an error — stopping something twice should be safe.
func Terminate(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("terminate: %d is not a pid", pid)
	}
	err := syscall.Kill(pid, syscall.SIGTERM)
	switch {
	case err == nil, errors.Is(err, syscall.ESRCH):
		return nil
	case errors.Is(err, syscall.EPERM):
		return fmt.Errorf("terminate %d: not permitted — the daemon belongs to another user", pid)
	}
	return fmt.Errorf("terminate %d: %w", pid, err)
}

// Reap kills a process group left behind by a daemon that did not shut down cleanly.
//
// It is deliberately the group, not the process: an agent CLI spawns compilers and test runners,
// and reaping only the parent leaves those writing into a worktree the next run will reuse.
func Reap(pid int) error { return killGroup(pid) }

// setNewSession detaches a process from the caller's session and controlling terminal.
//
// Setpgid alone is not enough for something meant to outlive its starter: a new process group in
// the same session keeps the terminal, and when that terminal goes away the kernel delivers
// SIGHUP to the session's groups. A daemon started from a TUI would then die the moment the TUI
// exited, which is the exact failure GR-007 exists to prevent. Setsid implies a new process group
// as well, so it replaces setProcessGroup rather than joining it.
func setNewSession(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
