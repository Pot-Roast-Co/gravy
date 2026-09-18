package core

import (
	"strings"
	"testing"
	"time"
)

func TestValidateChangeInstruction(t *testing.T) {
	tests := []struct {
		name string
		in   ChangeInstruction
		ok   bool
	}{
		{"a correction is enough", ChangeInstruction{Correction: "handle nil"}, true},
		{"blank correction", ChangeInstruction{Correction: "   "}, false},
		{"nothing at all", ChangeInstruction{}, false},
		// Demanding a constraint would teach people to invent one, and an invented promise is
		// worse than an absent one.
		{"constraints without a correction", ChangeInstruction{Preserve: []string{"x"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateChangeInstruction(tt.in)
			if tt.ok && err != nil {
				t.Fatalf("rejected a valid instruction: %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("accepted an instruction that says nothing to do")
			}
		})
	}
}

// TestNormalizedDropsBlanks: a human editing three text fields leaves blank lines behind, and a
// blank preservation constraint in a prompt reads as an empty promise rather than as nothing.
func TestNormalizedDropsBlanks(t *testing.T) {
	got := ChangeInstruction{
		Correction: "  handle nil  ",
		Preserve:   []string{"", "keeps returning 0", "   "},
		Verify:     []string{"  ", "go test ./..."},
	}.Normalized()

	if got.Correction != "handle nil" {
		t.Errorf("correction = %q", got.Correction)
	}
	if len(got.Preserve) != 1 || got.Preserve[0] != "keeps returning 0" {
		t.Errorf("preserve = %q", got.Preserve)
	}
	if len(got.Verify) != 1 || got.Verify[0] != "go test ./..." {
		t.Errorf("verify = %q", got.Verify)
	}
	if got.Empty() {
		t.Error("a real instruction reported itself empty")
	}
	if !(ChangeInstruction{Preserve: []string{" "}}).Empty() {
		t.Error("an instruction of blanks did not report itself empty")
	}
}

// TestPreservationConstraintsDeduplicates: an agent asked to restate what must survive will
// restate the same sentence, and listing it four times makes the one new constraint harder to
// find.
func TestPreservationConstraintsDeduplicates(t *testing.T) {
	got := PreservationConstraints([]ChangeInstruction{
		{Preserve: []string{"Multiply keeps returning 0", "the CLI flag stays"}},
		{Preserve: []string{"multiply KEEPS returning 0", ""}},
		{Preserve: []string{"the parser still accepts empty input"}},
	})
	want := []string{
		"Multiply keeps returning 0", "the CLI flag stays",
		"the parser still accepts empty input",
	}
	if len(got) != len(want) {
		t.Fatalf("constraints = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("constraint %d = %q, want %q (oldest first)", i, got[i], want[i])
		}
	}
}

// TestRenderChangeInstructionsKeepsEarlierConstraints is the core of the regression.
//
// The second correction is narrow and says nothing about what the first one protected. If the
// rendered instructions drop that constraint, the agent implementing round two is free to reach
// its goal by removing behaviour round one was accepted on.
func TestRenderChangeInstructionsKeepsEarlierConstraints(t *testing.T) {
	agreed := []ChangeInstruction{
		{
			Correction: "Return an error on a nil operand instead of panicking",
			Preserve:   []string{"Multiply keeps returning 0 when either operand is 0"},
			Verify:     []string{"go test ./calc"},
			AgreedAt:   time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
		},
		{
			Correction: "Name the error variable ErrNilOperand",
			Verify:     []string{"go test ./calc"},
			AgreedAt:   time.Date(2026, 9, 2, 11, 30, 0, 0, time.UTC),
		},
	}

	got := RenderChangeInstructions(agreed)

	// The task is the newest correction.
	correction := strings.Index(got, "Name the error variable ErrNilOperand")
	if correction < 0 {
		t.Fatalf("the latest correction is missing:\n%s", got)
	}
	if strings.Contains(got, "### The correction to make") &&
		strings.Index(got, "### The correction to make") > correction {
		t.Errorf("the correction is not under its own heading:\n%s", got)
	}
	// The older constraint is present, named as binding, and after the correction — it is the
	// boundary, not the task.
	binding := strings.Index(got, "Multiply keeps returning 0 when either operand is 0")
	if binding < 0 {
		t.Fatalf("the first round's preservation constraint was dropped:\n%s", got)
	}
	if binding < correction {
		t.Errorf("the earlier constraint outranks the correction to make:\n%s", got)
	}
	if !strings.Contains(got, "Still binding from earlier corrections") {
		t.Errorf("the earlier constraint is not marked as still binding:\n%s", got)
	}
	// And the conflict instruction, which is what an agent does when both cannot hold.
	if !strings.Contains(got, "stop and say so") {
		t.Errorf("nothing tells the agent what to do when the two conflict:\n%s", got)
	}
	if !strings.Contains(got, "2026-09-02") {
		t.Errorf("the agreement is undated:\n%s", got)
	}
}

// TestRenderChangeInstructionsDoesNotRepeatItself: a constraint the latest round restated is
// already in front of the agent, and printing it twice teaches that repetition means emphasis.
func TestRenderChangeInstructionsDoesNotRepeatItself(t *testing.T) {
	got := RenderChangeInstructions([]ChangeInstruction{
		{Correction: "one", Preserve: []string{"the CLI flag stays"}},
		{Correction: "two", Preserve: []string{"The CLI Flag Stays"}},
	})
	if n := strings.Count(strings.ToLower(got), "the cli flag stays"); n != 1 {
		t.Errorf("the constraint appears %d times:\n%s", n, got)
	}
	// One round, or a repeat of it, has nothing "still binding" to add.
	if strings.Contains(got, "Still binding from earlier corrections") {
		t.Errorf("an empty binding section was rendered:\n%s", got)
	}
}

// TestRenderChangeInstructionsEmpty lets a caller append it unconditionally.
func TestRenderChangeInstructionsEmpty(t *testing.T) {
	for _, in := range [][]ChangeInstruction{nil, {}, {{}}, {{Preserve: []string{"  "}}}} {
		if got := RenderChangeInstructions(in); got != "" {
			t.Errorf("RenderChangeInstructions(%+v) = %q, want empty", in, got)
		}
	}
}

// TestRenderReadsTheSameToBothParties: the human confirms what the agent is told, so the two
// must be the same words.
func TestRenderReadsTheSameToBothParties(t *testing.T) {
	got := ChangeInstruction{
		Correction: "handle nil",
		Preserve:   []string{"zero still returns 0"},
		Verify:     []string{"go test ./..."},
	}.Render()
	for _, want := range []string{
		"Correction: handle nil", "Preserve, unchanged:", "- zero still returns 0",
		"Verify both:", "- go test ./...",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Render omits %q:\n%s", want, got)
		}
	}
}

// TestSayIgnoresEmptyTurns keeps a blank line out of a transcript a human reads.
func TestSayIgnoresEmptyTurns(t *testing.T) {
	var d ChangeDiscussion
	at := time.Unix(1700000000, 0)
	d.Say(RoleHuman, "   ", at)
	d.Say(RoleAgent, "", at)
	d.Say(RoleHuman, "  it panics on nil  ", at)

	if len(d.Messages) != 1 {
		t.Fatalf("transcript = %+v, want only the message that said something", d.Messages)
	}
	if d.Messages[0].Text != "it panics on nil" || d.Messages[0].Role != RoleHuman {
		t.Errorf("message = %+v", d.Messages[0])
	}
}

func TestDiscussionStateValid(t *testing.T) {
	for _, s := range []DiscussionState{DiscussionOpen, DiscussionSent, DiscussionCanceled} {
		if !s.Valid() {
			t.Errorf("%q is not valid", s)
		}
	}
	for _, s := range []DiscussionState{"", "closed", "Open"} {
		if s.Valid() {
			t.Errorf("%q passed as a known state", s)
		}
	}
}
