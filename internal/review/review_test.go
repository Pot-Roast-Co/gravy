package review

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bobbybrady/gravy/internal/core"
	"github.com/bobbybrady/gravy/internal/git"
	"github.com/bobbybrady/gravy/internal/validate"
)

// stubModel answers with whatever it was given, or fails.
type stubModel struct {
	answer  string
	err     error
	prompts []string
}

func (m *stubModel) Complete(_ context.Context, prompt string) (string, error) {
	m.prompts = append(m.prompts, prompt)
	if m.err != nil {
		return "", m.err
	}
	return m.answer, nil
}

func request() Request {
	return Request{
		Ticket:  core.Ticket{ID: "GR-1", Title: "Add Divide", Body: "handle the zero case"},
		Project: core.Project{Slug: "gravy"},
		Diff: git.Diff{Files: []git.FileDiff{
			{Path: "calc.go", Status: "modified", Additions: 6, Deletions: 0,
				Patch: "@@ -1,3 +1,9 @@\n+func Divide(a, b int) int { return a / b }"},
		}},
		Validation: validate.Results{{Step: "test", Outcome: validate.Passed}},
	}
}

// TestVerdictIsStructured is AC1: the Review screen renders fields, not prose.
func TestVerdictIsStructured(t *testing.T) {
	m := &stubModel{answer: `Here is my review:
` + "```json" + `
{"overall":"concerns","summary":"Divide by zero is unhandled.",
 "findings":[{"severity":"high","file":"calc.go","line":2,"rationale":"b may be zero"}]}
` + "```"}

	v := New(m, 0).Review(context.Background(), request())

	if !v.Available() {
		t.Fatalf("verdict is unavailable: %+v", v)
	}
	if v.Overall != Concerns {
		t.Errorf("overall = %q, want concerns", v.Overall)
	}
	if len(v.Findings) != 1 {
		t.Fatalf("findings = %+v, want one", v.Findings)
	}
	// AC2: file and line where determinable.
	f := v.Findings[0]
	if f.File != "calc.go" || f.Line != 2 || f.Severity != High {
		t.Errorf("finding = %+v", f)
	}
}

// TestUnparseableDegradesToUnavailable is AC3. Looking like a clean pass is the one way an
// advisory tool does real harm.
func TestUnparseableDegradesToUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model *stubModel
	}{
		{"prose only", &stubModel{answer: "Looks fine to me, ship it."}},
		{"malformed json", &stubModel{answer: `{"overall": "concerns", "findings": [`}},
		{"unknown outcome", &stubModel{answer: `{"overall":"probably fine"}`}},
		{"model failed", &stubModel{err: fmt.Errorf("quota exhausted")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := New(tc.model, 0).Review(context.Background(), request())
			if v.Available() {
				t.Fatalf("an unusable answer produced a usable verdict: %+v", v)
			}
			if v.Unavailable == "" {
				t.Error("no explanation for the missing verdict")
			}
			if v.Overall == Pass {
				t.Error("a failed review reported a pass")
			}
		})
	}
}

// TestNoModelIsNotAnError covers the pass being disabled entirely.
func TestNoModelIsNotAnError(t *testing.T) {
	v := New(nil, 0).Review(context.Background(), request())
	if v.Available() || v.Unavailable == "" {
		t.Errorf("verdict = %+v, want an explained unavailable", v)
	}
	var nilReviewer *Reviewer
	if got := nilReviewer.Review(context.Background(), request()); got.Available() {
		t.Error("a nil reviewer produced a verdict")
	}
}

// TestPromptSaysItIsAdvisory is the instruction the whole design rests on: a model told it is a
// gate writes like a gate.
func TestPromptSaysItIsAdvisory(t *testing.T) {
	p := BuildPrompt(request(), 0)
	if !strings.Contains(p, "ADVISORY") {
		t.Error("the prompt does not say the review is advisory")
	}
	if !strings.Contains(p, "does not approve, block or merge") {
		t.Error("the prompt does not say what the review cannot do")
	}
	for _, want := range []string{"GR-1", "Add Divide", "handle the zero case", "calc.go", "test"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt omits %q", want)
		}
	}
}

// TestLargeDiffTruncatesByFile is AC5. Half a hunk is worse than no hunk, because it reads as
// complete.
func TestLargeDiffTruncatesByFile(t *testing.T) {
	req := request()
	req.Diff.Files = nil
	for i := 0; i < 40; i++ {
		req.Diff.Files = append(req.Diff.Files, git.FileDiff{
			Path:      fmt.Sprintf("file%02d.go", i),
			Status:    "modified",
			Additions: 100,
			Patch:     "@@ -1 +1 @@\n" + strings.Repeat("+a line of diff\n", 400),
		})
	}

	budget := 20000
	p := BuildPrompt(req, budget)

	if len(p) > budget*2 {
		t.Errorf("prompt is %d chars against a %d budget", len(p), budget)
	}
	if !strings.Contains(p, "Omitted for length") {
		t.Fatalf("a truncated diff does not say what was left out")
	}
	// Whatever survived must be whole: a rendered file's patch appears in full.
	for _, f := range req.Diff.Files {
		header := fmt.Sprintf("### %s (modified", f.Path)
		if !strings.Contains(p, header) {
			continue
		}
		if !strings.Contains(p, f.Patch) {
			t.Errorf("%s was included but its patch was cut mid-way", f.Path)
		}
	}
}

// TestEmptyDiffNeedsNoReview avoids spending a model call on nothing.
func TestEmptyDiffNeedsNoReview(t *testing.T) {
	m := &stubModel{answer: `{"overall":"pass"}`}
	req := request()
	req.Diff.Files = nil

	v := New(m, 0).Review(context.Background(), req)
	if v.Available() {
		t.Errorf("verdict = %+v, want unavailable", v)
	}
	if len(m.prompts) != 0 {
		t.Error("the model was called for an empty diff")
	}
}

// TestFindingsWithoutRationaleAreDropped keeps empty rows off the review card.
func TestFindingsWithoutRationaleAreDropped(t *testing.T) {
	m := &stubModel{answer: `{"overall":"pass","findings":[
		{"severity":"low","file":"a.go","rationale":"   "},
		{"severity":"nonsense","file":"b.go","rationale":"this one is real"}]}`}

	v := New(m, 0).Review(context.Background(), request())
	if len(v.Findings) != 1 {
		t.Fatalf("findings = %+v, want the one with a rationale", v.Findings)
	}
	// An unrecognised severity becomes medium rather than being dropped or rendered raw.
	if v.Findings[0].Severity != Medium {
		t.Errorf("severity = %q, want medium", v.Findings[0].Severity)
	}
}

// TestParseFindsJSONAmongProse covers what models actually return.
func TestParseFindsJSONAmongProse(t *testing.T) {
	answers := []string{
		`{"overall":"pass"}`,
		"```json\n{\"overall\":\"pass\"}\n```",
		"Sure!\n\n{\"overall\":\"pass\",\"summary\":\"a } brace in a string\"}\n\nHope that helps.",
	}
	for i, a := range answers {
		v, err := Parse(a)
		if err != nil {
			t.Errorf("answer %d: %v", i, err)
			continue
		}
		if v.Overall != Pass {
			t.Errorf("answer %d: overall = %q", i, v.Overall)
		}
	}
}
