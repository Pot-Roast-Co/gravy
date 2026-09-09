package contextbuild

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/pot-roast-co/gravy/internal/host"
)

// Doc is one project document as included in assembled context.
type Doc struct {
	// Path is the repository-relative path it was read from.
	Path string
	// Body is the document's text, truncated to fit the budget.
	Body string
	// Truncated reports whether Body is an excerpt rather than the whole document.
	Truncated bool
}

// Tokens estimates how many tokens a string occupies.
//
// Deliberately approximate — four characters to a token, which GR-015 permits explicitly. A real
// tokenizer would tie this package to one provider's vocabulary, and the budget exists to keep a
// prompt sane rather than to fill it to the last token.
func Tokens(s string) int { return (len(s) + 3) / 4 }

// ProjectDocFiles are the documents looked for, in the priority order of ARCHITECTURE.md §7:
// conventions first, then design docs.
//
// A fixed list rather than a directory walk. host.FS has no ReadDir, "reading whole
// repositories" is an explicit non-goal of GR-015, and a predictable set is what makes assembled
// context reproducible between runs — a glob would silently change what an agent knows whenever
// somebody adds a file to docs/.
var ProjectDocFiles = []string{
	// Conventions: how this repository wants code written. Highest value per token, because it
	// is the document an agent is judged against.
	"CLAUDE.md",
	"AGENTS.md",
	"CONTRIBUTING.md",
	".github/CONTRIBUTING.md",
	// Design docs: what the thing is and how it is shaped.
	"README.md",
	"docs/ARCHITECTURE.md",
	"docs/PRODUCT.md",
	"docs/ROADMAP.md",
	"ARCHITECTURE.md",
	"PRODUCT.md",
	"ROADMAP.md",
}

// ProjectDocs reads a repository's convention and design documents, capped to budget tokens.
//
// Documents are taken in priority order and truncated from the end, so what survives is each
// document's opening — the overview a design document leads with, rather than its appendices.
// Reading goes through host.FS rather than os so that a repository on a remote host costs
// nothing extra later.
//
// Unreadable and missing documents are skipped silently: most repositories have most of this
// list missing, and that is not a failure worth reporting to a caller.
func ProjectDocs(fs host.FS, repoPath string, budget int) []Doc {
	if fs == nil || budget <= 0 {
		return nil
	}

	// No single document may take more than half the budget. Without this a long BACKLOG.md
	// crowds out the CLAUDE.md that actually governs how the code gets written — and the
	// conventions file is the one whose absence an agent cannot compensate for.
	perDoc := budget / 2
	if perDoc < 1 {
		perDoc = budget
	}

	var (
		out  []Doc
		used int
	)
	for _, name := range ProjectDocFiles {
		remaining := budget - used
		if remaining <= 0 {
			break
		}

		b, err := fs.ReadFile(filepath.Join(repoPath, filepath.FromSlash(name)))
		if err != nil {
			continue
		}
		body := strings.TrimSpace(string(b))
		if body == "" {
			continue
		}

		limit := perDoc
		if remaining < limit {
			limit = remaining
		}
		body, truncated := truncateTokens(body, limit)
		if body == "" {
			continue
		}

		out = append(out, Doc{Path: name, Body: body, Truncated: truncated})
		used += Tokens(body)
	}
	return out
}

// truncateTokens cuts s to at most n tokens.
func truncateTokens(s string, n int) (string, bool) {
	if Tokens(s) <= n {
		return s, false
	}

	limit := n * 4
	if limit > len(s) {
		limit = len(s)
	}
	cut := s[:limit]

	// Prefer the last line break, so an excerpt ends where a reader would have stopped rather
	// than mid-sentence. Only when one is reasonably near the end: cutting back to the first
	// line of a long unbroken block would throw away most of what fits.
	if i := strings.LastIndexByte(cut, '\n'); i > limit/2 {
		cut = cut[:i]
	}

	// A byte-wise cut can split a multibyte rune; back off until the excerpt is valid UTF-8.
	// At most three bytes, since that is the longest a partial rune can be.
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}

	return strings.TrimSpace(cut), true
}

// RenderDocs formats documents for inclusion in a prompt.
//
// Each document is labelled with the path it came from, and an excerpt says so: an agent told
// that it is reading all of ARCHITECTURE.md when it is reading the first third will answer
// confidently about sections it was never shown.
func RenderDocs(docs []Doc) string {
	if len(docs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Project documents\n")
	for _, d := range docs {
		label := d.Path
		if d.Truncated {
			label += " (excerpt)"
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", label, d.Body)
	}
	return b.String()
}
