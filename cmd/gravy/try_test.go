package main

import (
	"testing"

	"github.com/pot-roast-co/gravy/internal/provider"
)

func TestRelativize(t *testing.T) {
	const wt = "/var/folders/x/T/gravy-home-1/projects/demo/worktrees/gravy-GR-001-thing"

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain path", wt + "/main.go", "main.go"},
		{"nested path", wt + "/internal/core/state.go", "internal/core/state.go"},
		{"the worktree itself", wt, "."},
		{"unrelated path is untouched", "/etc/hosts", "/etc/hosts"},
		{"empty", "", ""},
		{"path embedded in a sentence", "edited " + wt + "/main.go now", "edited main.go now"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := relativize(tt.in, wt); got != tt.want {
				t.Errorf("relativize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	if got := relativize("/anything", ""); got != "/anything" {
		t.Errorf("relativize with no worktree = %q", got)
	}
}

// TestRelativizePrefersTheLongestPrefix guards a bug that produced "/privatemain.go".
//
// macOS resolves temp directories through a /private symlink, so the agent reports
// /private/var/... while the worktree is known as /var/.... Stripping the shorter form first
// matches inside the longer path and removes its middle rather than its prefix.
func TestRelativizePrefersTheLongestPrefix(t *testing.T) {
	const short = "/var/folders/x/T/wt"
	const long = "/private/var/folders/x/T/wt"

	got := relativizeCandidates(long+"/main.go", []string{short, long})
	if got != "main.go" {
		t.Errorf("relativize produced %q, want main.go", got)
	}
}

func TestToolLine(t *testing.T) {
	const wt = "/var/folders/x/T/wt"

	tests := []struct {
		name  string
		event provider.Event
		want  string
	}{
		{
			name:  "file path is relativized",
			event: provider.Event{Tool: "Edit", Fields: map[string]any{"file_path": wt + "/main.go"}},
			want:  "Edit main.go",
		},
		{
			name:  "command is shown verbatim",
			event: provider.Event{Tool: "Bash", Fields: map[string]any{"command": "go test ./..."}},
			want:  "Bash go test ./...",
		},
		{
			// The adapter truncates its own summary for storage, so a long path arrives cut
			// short. Reading the structured field avoids depending on that.
			name:  "falls back to the tool name",
			event: provider.Event{Tool: "Read", Text: "Read " + wt + "/very/long/path"},
			want:  "Read",
		},
		{
			name:  "no tool at all",
			event: provider.Event{Text: "something"},
			want:  "something",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolLine(tt.event, wt); got != tt.want {
				t.Errorf("toolLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOneLineAndFirstWords(t *testing.T) {
	if got := oneLine("a\n b\tc", 100); got != "a b c" {
		t.Errorf("oneLine collapsed whitespace to %q", got)
	}
	if got := oneLine("abcdefghij", 5); got != "abcde…" {
		t.Errorf("oneLine(…, 5) = %q", got)
	}
	if got := firstWords("one two three four", 2); got != "one two" {
		t.Errorf("firstWords = %q", got)
	}
	if got := firstWords("one", 5); got != "one" {
		t.Errorf("firstWords with fewer words = %q", got)
	}
}
