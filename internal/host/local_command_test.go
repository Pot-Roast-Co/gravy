package host

import (
	"os/exec"
	"testing"
)

func TestEditorPrefersVisual(t *testing.T) {
	for _, tc := range []struct {
		name     string
		visual   string
		editor   string
		wantName string
		wantArgs []string
		wantErr  bool
	}{
		// VISUAL before EDITOR: EDITOR may be a line editor for a terminal that cannot do
		// better, and VISUAL is what to use when it can.
		{name: "visual wins", visual: "zed", editor: "vi", wantName: "zed"},
		{name: "editor is the fallback", editor: "vi", wantName: "vi"},
		// A value carrying flags is a normal thing to export.
		{name: "flags are split off", visual: "code --wait", wantName: "code", wantArgs: []string{"--wait"}},
		{name: "whitespace is not an editor", visual: "   ", editor: "  ", wantErr: true},
		{name: "nothing set", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VISUAL", tc.visual)
			t.Setenv("EDITOR", tc.editor)

			name, args, err := Editor()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Editor() = %q, want an error naming what to set", name)
				}
				return
			}
			if err != nil {
				t.Fatalf("Editor(): %v", err)
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if len(args) != len(tc.wantArgs) {
				t.Fatalf("args = %v, want %v", args, tc.wantArgs)
			}
			for i := range args {
				if args[i] != tc.wantArgs[i] {
					t.Errorf("args = %v, want %v", args, tc.wantArgs)
				}
			}
		})
	}
}

func TestShellFallsBack(t *testing.T) {
	t.Setenv("SHELL", "/usr/bin/fish")
	if got := Shell(); got != "/usr/bin/fish" {
		t.Errorf("Shell() = %q, want the environment's", got)
	}
	t.Setenv("SHELL", "")
	if got := Shell(); got != "/bin/sh" {
		t.Errorf("Shell() = %q, want /bin/sh", got)
	}
}

func TestLocalCommandRunsInTheWorktree(t *testing.T) {
	dir := t.TempDir()
	cmd := LocalCommand("git", []string{"status"}, dir)
	if cmd.Dir != dir {
		t.Errorf("Dir = %q, want %q", cmd.Dir, dir)
	}
	if len(cmd.Args) < 2 || cmd.Args[1] != "status" {
		t.Errorf("Args = %v", cmd.Args)
	}
}

func TestDifftoolWantsOneConfigured(t *testing.T) {
	dir := t.TempDir()
	// A repository with no user or system config behind it, so only what the test sets counts.
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_DIFF_TOOL", "")
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	gitConfig := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"config"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config %v: %v: %s", args, err, out)
		}
	}

	if err := Difftool(dir); err == nil {
		t.Fatal("Difftool() = nil with nothing configured, want an error naming what to set")
	}

	t.Setenv("GIT_DIFF_TOOL", "vimdiff")
	if err := Difftool(dir); err != nil {
		t.Errorf("Difftool() with GIT_DIFF_TOOL: %v", err)
	}
	t.Setenv("GIT_DIFF_TOOL", "")

	// merge.tool is git's own fallback for difftool.
	gitConfig("merge.tool", "meld")
	if err := Difftool(dir); err != nil {
		t.Errorf("Difftool() with merge.tool: %v", err)
	}
	gitConfig("--unset", "merge.tool")

	gitConfig("diff.tool", "nvimdiff")
	if err := Difftool(dir); err != nil {
		t.Errorf("Difftool() with diff.tool: %v", err)
	}
}
