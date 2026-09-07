package review

import (
	"fmt"
	"sort"
	"strings"
)

// BuildPrompt assembles the review prompt within a character budget.
//
// The instruction that this is advisory is not decoration: a model told it is a gate writes like
// a gate, hedging and escalating, and the result is a verdict a human learns to ignore.
func BuildPrompt(req Request, budget int) string {
	if budget <= 0 {
		budget = DefaultBudget
	}

	var b strings.Builder
	b.WriteString(`You are reviewing one change for a human reviewer.

Your output is ADVISORY: it annotates the diff so a person can decide faster.
It does not approve, block or merge anything, and a human reads every change
regardless of what you say.

Report only what you can defend from the diff in front of you. Prefer saying nothing to
speculating. Do not comment on formatting, naming taste, or anything a linter would catch.

Answer with one JSON object and nothing else:

{
  "overall": "pass" | "concerns" | "fail",
  "summary": "one or two sentences a reviewer can read in three seconds",
  "findings": [
    {"severity": "high"|"medium"|"low", "file": "path", "line": 0, "rationale": "why this matters"}
  ]
}

Use "fail" only for a defect you can point at: a bug, a broken invariant, a security problem.
Use "concerns" when something needs a human's eye. Use "pass" when the change does what the
ticket asked and you found nothing worth a reviewer's time. An empty findings list is a good
answer when the change is fine.

`)

	fmt.Fprintf(&b, "## Ticket %s: %s\n\n", req.Ticket.ID, req.Ticket.Title)
	if body := strings.TrimSpace(req.Ticket.Body); body != "" {
		b.WriteString(body)
		b.WriteString("\n\n")
	}

	if len(req.Validation) > 0 {
		b.WriteString("## Validation\n\n")
		for _, v := range req.Validation {
			fmt.Fprintf(&b, "- %s: %s\n", v.Step, v.Outcome)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Diff\n\n")
	b.WriteString(renderDiff(req, budget-b.Len()))
	return b.String()
}

// renderDiff writes as many whole files as fit, then says what it left out.
//
// Truncating by file rather than by byte keeps every hunk the model does see intact: half a hunk
// is worse than no hunk, because it reads as complete. Smallest first, so a budget is spent on
// the most files rather than on one enormous one.
func renderDiff(req Request, budget int) string {
	if budget < 200 {
		return fmt.Sprintf("(%d file(s) omitted: no room in the prompt)\n", len(req.Diff.Files))
	}

	order := make([]int, len(req.Diff.Files))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return len(req.Diff.Files[order[a]].Patch) < len(req.Diff.Files[order[b]].Patch)
	})

	included := map[int]bool{}
	used := 0
	for _, i := range order {
		f := req.Diff.Files[i]
		cost := len(f.Patch) + len(f.Path) + 32
		if used+cost > budget {
			continue
		}
		used += cost
		included[i] = true
	}

	var b strings.Builder
	var omitted []string
	// Render in the diff's own order, so the model sees the change as it is laid out.
	for i, f := range req.Diff.Files {
		if !included[i] {
			omitted = append(omitted, fmt.Sprintf("%s (+%d -%d)", f.Path, f.Additions, f.Deletions))
			continue
		}
		fmt.Fprintf(&b, "### %s (%s, +%d -%d)\n\n", f.Path, f.Status, f.Additions, f.Deletions)
		if strings.TrimSpace(f.Patch) == "" {
			b.WriteString("(no textual diff)\n\n")
			continue
		}
		b.WriteString(f.Patch)
		b.WriteString("\n\n")
	}

	// What was left out is stated, so a verdict is never silently based on part of the change.
	if len(omitted) > 0 {
		fmt.Fprintf(&b, "### Omitted for length: %d file(s)\n\n", len(omitted))
		for _, o := range omitted {
			fmt.Fprintf(&b, "- %s\n", o)
		}
		b.WriteString("\nJudge only what you were shown, and say so in the summary if that matters.\n")
	}
	return b.String()
}
