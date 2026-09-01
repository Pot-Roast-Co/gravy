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
