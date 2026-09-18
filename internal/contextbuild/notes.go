package contextbuild

import "strings"

// NotesBudget caps how much of a project's notes reach a prompt, in tokens.
//
// Generous, because notes are typed by a human who was trying to explain the project and every
// sentence of that is the kind of context an agent cannot get anywhere else. Capped at all,
// because nothing else stops a notes field from being pasted design doc.
const NotesBudget = 1500

// RenderNotes formats a project's notes for a prompt.
//
// Notes are the human's own answer to "what is this project for", and for a project with no
// repository they are the only answer that exists: there are no documents to read, the backlog is
// empty, and the project name is a slug. Planning such a project without them means planning from
// the name alone.
//
// They go in as their own section rather than being folded into the documents, because they did
// not come from a file and labelling them with a path would be a lie about where to go and change
// them.
func RenderNotes(notes string) string {
	body := strings.TrimSpace(notes)
	if body == "" {
		return ""
	}
	body, truncated := truncateTokens(body, NotesBudget)
	heading := "# What this project is for"
	if truncated {
		heading += " (excerpt)"
	}
	return heading + "\n\nThe human who owns this project wrote this:\n\n" + body + "\n"
}
