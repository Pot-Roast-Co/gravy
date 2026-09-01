package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeGo creates a .go file at rel under dir, creating parent directories.
func writeGo(t *testing.T, dir, rel, src string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func TestCheck(t *testing.T) {
	const execSrc = "package p\n\nimport _ \"os/exec\"\n"
	const teaSrc = "package p\n\nimport _ \"github.com/charmbracelet/bubbletea\"\n"
	const lipSrc = "package p\n\nimport _ \"github.com/charmbracelet/lipgloss/table\"\n"
	const cleanSrc = "package p\n\nimport _ \"fmt\"\n"

	tests := []struct {
		name  string
		files map[string]string
		want  int
	}{
		{
			name:  "clean tree has no violations",
			files: map[string]string{"internal/core/core.go": cleanSrc},
			want:  0,
		},
		{
			name:  "host may import os/exec",
			files: map[string]string{"internal/host/local.go": execSrc},
			want:  0,
		},
		{
			name:  "scheduler may not import os/exec",
			files: map[string]string{"internal/scheduler/sched.go": execSrc},
			want:  1,
		},
		{
			name:  "tui may import bubbletea",
			files: map[string]string{"internal/tui/app.go": teaSrc},
			want:  0,
		},
		{
			name:  "cmd may import bubbletea",
			files: map[string]string{"cmd/gravy/main.go": teaSrc},
			want:  0,
		},
		{
			name:  "core may not import bubbletea",
			files: map[string]string{"internal/core/ticket.go": teaSrc},
			want:  1,
		},
		{
			name:  "lipgloss subpackage is caught",
			files: map[string]string{"internal/store/store.go": lipSrc},
			want:  1,
		},
		{
			// A directory merely prefixed with an allowed name must not inherit its exemption:
			// internal/hostile is not internal/host.
			name:  "prefix collision does not grant an exemption",
			files: map[string]string{"internal/hostile/x.go": execSrc},
			want:  1,
		},
		{
			name: "test files are checked too",
			// A violation hidden in a _test.go file is still a violation.
			files: map[string]string{"internal/git/git_test.go": execSrc},
			want:  1,
		},
		{
			name: "violations are counted per import, across files",
			files: map[string]string{
				"internal/core/a.go":  execSrc,
				"internal/store/b.go": teaSrc,
				"internal/host/c.go":  execSrc,
				"internal/tui/d.go":   teaSrc,
			},
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for rel, src := range tt.files {
				writeGo(t, dir, rel, src)
			}
			got, err := check(dir)
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if len(got) != tt.want {
				t.Errorf("got %d violations, want %d: %+v", len(got), tt.want, got)
			}
		})
	}
}

// TestCheckReportsLocation guards the part a human actually reads: the file and line.
func TestCheckReportsLocation(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "internal/agentrun/run.go", "package p\n\nimport (\n\t_ \"fmt\"\n\t_ \"os/exec\"\n)\n")

	got, err := check(dir)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d violations, want 1", len(got))
	}
	if got[0].file != "internal/agentrun/run.go" {
		t.Errorf("file = %q, want internal/agentrun/run.go", got[0].file)
	}
	if got[0].line != 5 {
		t.Errorf("line = %d, want 5", got[0].line)
	}
	if got[0].imp != "os/exec" {
		t.Errorf("imp = %q, want os/exec", got[0].imp)
	}
}
