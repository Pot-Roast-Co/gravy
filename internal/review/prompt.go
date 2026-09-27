package review

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pot-roast-co/gravy/internal/core"
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

	writeDecisions(&b, req.Decisions)
	writeCriteria(&b, acceptanceCriteria(req.Ticket.Body))
	writePrevious(&b, req.Previous)

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

// writeDecisions renders what the human has agreed since the ticket was written.
//
// These amend the ticket, and the reviewer is told so in as many words. Shown only the ticket's
// original text, it flagged a deliberate narrowing as missing scope on every round — the
// human had already decided, and the verdict kept asking them to decide again.
func writeDecisions(b *strings.Builder, decisions []core.ChangeInstruction) {
	var kept []core.ChangeInstruction
	for _, d := range decisions {
		if d = d.Normalized(); !d.Empty() {
			kept = append(kept, d)
		}
	}
	if len(kept) == 0 {
		return
	}

	b.WriteString(`## Decisions the human has made since the ticket was written

These amend the ticket, oldest first. Where one narrows, drops or changes a requirement, judge
the change against the decision, not the ticket's original wording. Do not report a gap the
human has already accepted, and do not ask them to decide something they have decided.

`)
	for i, d := range kept {
		fmt.Fprintf(b, "%d. %s\n", i+1, d.Correction)
		for _, p := range d.Preserve {
			fmt.Fprintf(b, "   - Preserve: %s\n", p)
		}
	}
	b.WriteString("\n")
}

// acceptanceCriteria returns the ticket's bullet points, in order.
//
// A ticket's "done looks like" list is bullets by convention, and nothing else in a ticket body
// is reliably shaped. Any bullet counts, nested ones included: over-including costs the reviewer
// a line, and a criterion left out is exactly the one nobody checks.
func acceptanceCriteria(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		for _, marker := range []string{"- ", "* ", "+ "} {
			if item, ok := strings.CutPrefix(line, marker); ok {
				if item = strings.TrimSpace(item); item != "" {
					out = append(out, item)
				}
				break
			}
		}
	}
	return out
}

// writeCriteria asks for the ticket to be checked one criterion at a time.
//
// A reviewer asked only for defects reviews the code that exists and never notices the code that
// does not: a ticket whose list asked for a settings page with a refresh button was reviewed
// clean without one, and the league it imported could not be reached from anywhere in the app.
// A missing criterion leaves nothing in the diff to object to, so it has to be asked for.
func writeCriteria(b *strings.Builder, criteria []string) {
	if len(criteria) == 0 {
		return
	}

	b.WriteString(`## Check each of the ticket's acceptance criteria

Go through these one at a time and find where the diff delivers each. For any the diff does not
deliver at all, report a finding that quotes the criterion and says what is missing: "high"
when the change is unusable without it, "medium" otherwise. A criterion a decision above narrowed
or dropped is judged by the decision. Report only criteria that are missing or clearly broken,
not the ones that are met; if every criterion is met, say nothing about them.

`)
	for _, c := range criteria {
		fmt.Fprintf(b, "- %s\n", c)
	}
	b.WriteString("\n")
}

// writePrevious renders the last round's findings, so this round can say what changed.
//
// Without them every round is a first review: a finding that cannot be fixed in code — a
// manual check nobody can run from a diff — came back word for word each time.
func writePrevious(b *strings.Builder, prev *Verdict) {
	if prev == nil || !prev.Available() || len(prev.Findings) == 0 {
		return
	}

	b.WriteString(`## Your findings on the previous round

The work was revised after these. Do not repeat one that has been addressed or that a decision
above settles. Repeat one only if it still applies to this diff, and say that it is still open.
Something that can only be checked outside the diff (a manual run, a measurement, a build on
another machine) is not a defect in the diff; mention it once in the summary at most.

`)
	for _, f := range prev.Findings {
		where := f.File
		if where != "" && f.Line > 0 {
			where = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		if where == "" {
			where = "general"
		}
		fmt.Fprintf(b, "- [%s] %s: %s\n", f.Severity, where, strings.TrimSpace(f.Rationale))
	}
	b.WriteString("\n")
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
