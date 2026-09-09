package host

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Cmd is exec.Cmd, re-exported so a client can hold one without importing os/exec — which
// ARCHITECTURE.md §1.1 permits only here, and which lint-layers enforces.
type Cmd = exec.Cmd

// LocalCommand builds a command that a client runs on its own terminal.
//
// It is the seam for the three things a review screen hands off to real tools — an editor, a
// difftool, a shell in the worktree — none of which can go through Host.Exec: that streams
// output back, and these need the terminal itself, keyboard and all.
//
// Deliberately local. An editor opens on the machine the human is sitting at, so a worktree on
// a remote host has nothing to open until there is a sync story; the caller checks that the path
// exists and says so rather than launching an editor on an empty directory.
func LocalCommand(name string, args []string, dir string) *Cmd {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd
}

// Editor is the editor to open, from the environment.
//
// VISUAL before EDITOR, which is the older convention and still the right one: EDITOR may be a
// line editor for a terminal that cannot do better, and VISUAL is what to use when it can.
func Editor() (string, []string, error) {
	for _, key := range []string{"VISUAL", "EDITOR"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			// The value may carry flags — "code --wait", "zed --wait" — so it is split
			// rather than treated as a bare program name.
			fields := strings.Fields(v)
			return fields[0], fields[1:], nil
		}
	}
	return "", nil, fmt.Errorf("no editor set — export VISUAL or EDITOR")
}

// Shell is the shell to open, from the environment.
func Shell() string {
	if v := strings.TrimSpace(os.Getenv("SHELL")); v != "" {
		return v
	}
	return "/bin/sh"
}
