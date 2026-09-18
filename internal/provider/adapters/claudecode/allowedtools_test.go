package claudecode

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

// TestAllowedToolsGrantsTheReadTools is the regression.
//
// The rendered allowlist was Bash patterns and nothing else, so the only way an agent could look
// at a file was to shell out. A shell rule is a prefix rule: it authorises one simple command and
// never a chain, so `ls -la a b 2>/dev/null; find . -type f | head -5` was denied with ls, find
// and head each granted. Three planning turns died that way.
func TestAllowedToolsGrantsTheReadTools(t *testing.T) {
	got := allowedTools(core.Allowlist{Commands: []core.Pattern{{Match: "ls"}}})
	joined := strings.Join(got, " ")

	for _, want := range []string{"Read", "Glob", "Grep"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %s: an agent can only read files by shelling out\n%v", want, got)
		}
	}
	// And still grants the shell commands it was given.
	for _, want := range []string{"Bash(ls)", "Bash(ls *)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("lost %q:\n%v", want, got)
		}
	}
}

// The read tools are read-only. Nothing here may write, and a change that added Write or Edit to
// this list would hand every planning turn the ability to edit the repository it is describing.
func TestAllowedToolsGrantsNothingThatWrites(t *testing.T) {
	got := strings.Join(allowedTools(core.Allowlist{Commands: []core.Pattern{{Match: "ls"}}}), " ")
	for _, forbidden := range []string{"Write", "Edit", "NotebookEdit", "WebFetch"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the default tool grants include %q", forbidden)
		}
	}
}

// An empty allowlist still renders nothing, so runArgs passes no --allowedTools at all rather
// than a flag granting read tools to a task that was given no permissions.
func TestAllowedToolsEmptyStaysEmpty(t *testing.T) {
	if got := allowedTools(core.Allowlist{}); len(got) != 0 {
		t.Errorf("allowedTools(empty) = %v, want nothing", got)
	}
}
