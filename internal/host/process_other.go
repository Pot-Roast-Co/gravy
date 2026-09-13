//go:build !unix

package host

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup is a no-op where process groups are not available.
func setProcessGroup(*exec.Cmd) {}

// killGroup falls back to killing only the named process. Children survive, which is a real
// limitation rather than a hidden one: v0.1 targets darwin and linux.
func killGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// signalNumber has no meaningful answer off unix.
func signalNumber(*exec.ExitError) int { return 9 }

// Alive reports whether a process is still running. Off unix this is best-effort: FindProcess
// succeeds for any pid, so the answer is only as good as Signal reporting.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// Terminate asks a process to shut down cleanly. Off unix this is best-effort.
func Terminate(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("terminate: %d is not a pid", pid)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil // already gone
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("terminate %d: %w", pid, err)
	}
	return nil
}

// Reap kills a process left behind by a daemon that did not shut down cleanly.
func Reap(pid int) error { return killGroup(pid) }

// setNewSession is a no-op where sessions are not available.
func setNewSession(*exec.Cmd) {}

func terminateGroup(pid int) error { return Terminate(pid) }
