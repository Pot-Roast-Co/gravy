//go:build !unix

package host

import (
	"os"
	"os/exec"
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
