package contextbuild

import (
	"strings"
	"testing"
)

func TestRenderNotesCarriesTheText(t *testing.T) {
	got := RenderNotes("  A fantasy hockey assistant. Start with the draft board.  ")
	if !strings.Contains(got, "A fantasy hockey assistant") {
		t.Fatalf("notes lost:\n%s", got)
	}
	if !strings.Contains(got, "What this project is for") {
		t.Errorf("no heading to hang them on:\n%s", got)
	}
}

// Empty notes add nothing, rather than an empty heading that reads as "nobody said".
func TestRenderNotesEmptyIsEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t\n"} {
		if got := RenderNotes(in); got != "" {
			t.Errorf("RenderNotes(%q) = %q, want empty", in, got)
		}
	}
}

// A pasted design doc is trimmed and says so: an agent told it has all of something it has a
// third of will answer confidently about the rest.
func TestRenderNotesTruncatesAndSaysSo(t *testing.T) {
	long := strings.Repeat("the project is for hockey. ", 2000)
	got := RenderNotes(long)
	if !strings.Contains(got, "excerpt") {
		t.Errorf("truncated silently:\n%s", got[:200])
	}
	if Tokens(got) > NotesBudget*2 {
		t.Errorf("rendered %d tokens against a %d budget", Tokens(got), NotesBudget)
	}
}
