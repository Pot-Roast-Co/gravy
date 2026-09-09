package host

import (
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
