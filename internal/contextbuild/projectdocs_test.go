package contextbuild

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mapFS is a host.FS backed by a map, so a test can describe a repository in a literal.
type mapFS map[string]string

func (m mapFS) ReadFile(path string) ([]byte, error) {
	b, ok := m[filepath.ToSlash(path)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(b), nil
}

func (m mapFS) WriteFile(string, []byte, os.FileMode) error { return os.ErrPermission }
func (m mapFS) Stat(string) (os.FileInfo, error)            { return nil, os.ErrNotExist }
func (m mapFS) MkdirAll(string, os.FileMode) error          { return os.ErrPermission }
func (m mapFS) RemoveAll(string) error                      { return os.ErrPermission }
func (m mapFS) Exists(path string) bool {
	_, ok := m[filepath.ToSlash(path)]
	return ok
}

// repo builds a mapFS rooted at /repo from repo-relative names.
func repo(files map[string]string) mapFS {
	m := mapFS{}
	for name, body := range files {
		m["/repo/"+name] = body
	}
	return m
}

func paths(docs []Doc) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Path)
	}
	return out
}

func TestProjectDocsPriorityOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{name: "Copilot instructions without CLAUDE.md", files: map[string]string{".github/copilot-instructions.md": "use tabs", "README.md": "readme"}, want: []string{".github/copilot-instructions.md", "README.md"}},
		{
			name:  "conventions come before design docs",
			files: map[string]string{"README.md": "readme", "CLAUDE.md": "conventions"},
			want:  []string{"CLAUDE.md", "README.md"},
		},
		{
			name: "the full spread, in order",
			files: map[string]string{
				"docs/PRODUCT.md":      "product",
				"README.md":            "readme",
				"CLAUDE.md":            "conventions",
				"docs/ARCHITECTURE.md": "arch",
				"CONTRIBUTING.md":      "contributing",
			},
			want: []string{
				"CLAUDE.md", "CONTRIBUTING.md", "README.md",
				"docs/ARCHITECTURE.md", "docs/PRODUCT.md",
			},
		},
		{
			name:  "AGENTS.md counts as conventions",
			files: map[string]string{"AGENTS.md": "agents", "README.md": "readme"},
			want:  []string{"AGENTS.md", "README.md"},
		},
		{
			name:  "a repository with none of them",
			files: map[string]string{"main.go": "package main"},
			want:  nil,
		},
		{
			name:  "an empty document is skipped, not included blank",
			files: map[string]string{"CLAUDE.md": "   \n\n  ", "README.md": "readme"},
			want:  []string{"README.md"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := paths(ProjectDocs(repo(tc.files), "/repo", 10000))
			if len(got) != len(tc.want) {
				t.Fatalf("docs = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("docs = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestProjectDocsNeverExceedsBudget is AC1 of GR-015, at the sizes that actually stress it.
func TestProjectDocsNeverExceedsBudget(t *testing.T) {
	big := strings.Repeat("a long line of project documentation\n", 5000)
	files := map[string]string{
		"CLAUDE.md":            big,
		"README.md":            big,
		"docs/ARCHITECTURE.md": big,
		"docs/PRODUCT.md":      big,
	}
	for _, budget := range []int{1, 10, 100, 1000, 10000} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			docs := ProjectDocs(repo(files), "/repo", budget)
			total := 0
			for _, d := range docs {
				total += Tokens(d.Body)
			}
			if total > budget {
				t.Errorf("assembled %d tokens, over the %d budget", total, budget)
			}
		})
	}
}

// TestProjectDocsKeepsConventionsUnderPressure is the case the per-document cap exists for: a
// huge design doc must not crowd out the file that says how to write the code.
func TestProjectDocsKeepsConventionsUnderPressure(t *testing.T) {
	docs := ProjectDocs(repo(map[string]string{
		"CLAUDE.md": "always use tabs",
		"README.md": strings.Repeat("filler filler filler\n", 10000),
	}), "/repo", 200)

	if len(docs) == 0 || docs[0].Path != "CLAUDE.md" {
		t.Fatalf("conventions were crowded out: %v", paths(docs))
	}
	if !strings.Contains(docs[0].Body, "always use tabs") {
		t.Errorf("CLAUDE.md was truncated away: %q", docs[0].Body)
	}
}

func TestProjectDocsTruncationIsMarked(t *testing.T) {
	docs := ProjectDocs(repo(map[string]string{
		"CLAUDE.md": strings.Repeat("line of conventions\n", 1000),
	}), "/repo", 100)

	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs))
	}
	if !docs[0].Truncated {
		t.Error("a truncated document is not marked as one")
	}
	// An agent told it read the whole file will answer about sections it never saw.
	if !strings.Contains(RenderDocs(docs), "(excerpt)") {
		t.Error("the rendered prompt does not say the document is an excerpt")
	}
}

// TestTruncationLeavesValidUTF8 is the adversarial case: a byte-wise cut through a multibyte
// character leaves a prompt containing invalid UTF-8.
func TestTruncationLeavesValidUTF8(t *testing.T) {
	// No newlines, so the line-boundary path cannot rescue the cut.
	body := strings.Repeat("café ", 500)
	for budget := 1; budget < 60; budget++ {
		docs := ProjectDocs(repo(map[string]string{"CLAUDE.md": body}), "/repo", budget)
		for _, d := range docs {
			if !utf8ValidString(d.Body) {
				t.Fatalf("budget %d produced invalid UTF-8: %q", budget, d.Body)
			}
		}
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestProjectDocsZeroBudget(t *testing.T) {
	for _, budget := range []int{0, -1} {
		if docs := ProjectDocs(repo(map[string]string{"CLAUDE.md": "x"}), "/repo", budget); docs != nil {
			t.Errorf("budget %d returned %v, want nothing", budget, paths(docs))
		}
	}
}

func TestProjectDocsNilFS(t *testing.T) {
	if docs := ProjectDocs(nil, "/repo", 1000); docs != nil {
		t.Errorf("a nil filesystem returned %v", paths(docs))
	}
}

func TestTokensIsApproximate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
	} {
		if got := Tokens(tc.in); got != tc.want {
			t.Errorf("Tokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestRenderDocsEmpty(t *testing.T) {
	if got := RenderDocs(nil); got != "" {
		t.Errorf("RenderDocs(nil) = %q, want empty", got)
	}
}
