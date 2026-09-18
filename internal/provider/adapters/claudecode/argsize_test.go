package claudecode

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/provider"
)

// maxArgStrLen is the Linux ceiling on a single argv entry: 32 pages, 128KiB. It is separate
// from ARG_MAX, which governs the whole list and is far larger, and it is not raised by ulimit.
const maxArgStrLen = 128 * 1024

// TestNoArgumentCanBeLongerThanTheKernelAllows is the regression.
//
// The prompt used to be an argument. A planning prompt carries the project's documents, and one
// repository's README and ROADMAP came to 132KB between them — over the ceiling before the
// instructions, the backlog or the human's question were added. fork/exec then failed with
// "argument list too long" and the planner never started:
//
//	plan: claude-code: start: exec claude: fork/exec …/claude: argument list too long
func TestNoArgumentCanBeLongerThanTheKernelAllows(t *testing.T) {
	huge := strings.Repeat("the project documents go here. ", 20000) // ~600KB
	if len(huge) < maxArgStrLen {
		t.Fatal("fixture is not big enough to exercise the limit")
	}

	task := provider.AgentTask{
		Prompt:       huge,
		WorktreePath: "/tmp/x",
		Allowlist:    core.Allowlist{Commands: []core.Pattern{{Match: "ls"}}},
	}
	args, cleanup, err := New().runArgs(task, "session-1")
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range args {
		if len(a) > maxArgStrLen {
			t.Fatalf("argument %d is %d bytes, over the %d ceiling: fork/exec will refuse it",
				i, len(a), maxArgStrLen)
		}
	}

	// And the same for a resumed turn, whose message is equally unbounded.
	for i, a := range New().resumeArgs(provider.SessionRef{ProviderID: ID, ID: "s"}, task.Allowlist) {
		if len(a) > maxArgStrLen {
			t.Fatalf("resume argument %d is %d bytes, over the ceiling", i, len(a))
		}
	}
}

// The prompt must not be in the argument list at all — that is what keeps it unbounded.
func TestPromptIsNotAnArgument(t *testing.T) {
	const marker = "PROMPT-MARKER-DO-NOT-PUT-ME-IN-ARGV"
	args, cleanup, err := New().runArgs(provider.AgentTask{Prompt: marker, WorktreePath: "/tmp/x"}, "s")
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), marker) {
		t.Fatalf("the prompt is back in argv:\n%v", args)
	}
	// -p is still there, as the flag it is.
	if !strings.Contains(strings.Join(args, " "), "-p") {
		t.Fatalf("lost -p:\n%v", args)
	}
}
