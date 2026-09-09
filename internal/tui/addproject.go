package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/api"
)

// addProjectDoneMsg carries the result of registering a repository back to the frame.
type addProjectDoneMsg struct {
	name string
	err  error
}

// addProject is the prompt behind the global P.
//
// It belongs to the frame rather than to the Settings screen because the moment it is most
// needed is the empty first-run dashboard: with no projects registered there is nothing to
// navigate to yet, and telling someone to leave the TUI and run a shell command is the one
// instruction a keyboard-first tool should never have to give.
//
// Only the path is asked for. AddProject detects the name from the directory, the target branch
// from the remote's HEAD, and defaults the rest; all of it is editable in Settings afterwards,
// so the prompt stays a single field rather than a wizard.
type addProject struct {
	open   bool
	path   string
	notice string
	// busy suppresses a second submit while the first is in flight. Registering runs git
	// against the repository, so it is not instant, and a double enter would otherwise race
	// two AddProject calls that both pass the duplicate-slug check.
	busy bool
	// matches is what the last tab found, shown under the field. An ambiguous completion has
	// to say what it was ambiguous between, or tab looks broken when it stops early.
	matches []string
}

// show opens the prompt empty, discarding whatever a previous cancelled attempt left behind.
func (a *addProject) show() { *a = addProject{open: true} }

// close returns the prompt to its resting state.
func (a *addProject) close() { *a = addProject{} }

// handleKey handles one key while the prompt owns the keyboard.
func (a *addProject) handleKey(msg tea.KeyMsg, svc api.Service) tea.Cmd {
	switch key := msg.String(); {
	case key == "esc":
		a.close()
		return nil

	case key == "enter":
		if a.busy {
			return nil
		}
		return a.submit(svc)

	case key == "tab":
		a.complete()
		return nil

	case key == "backspace":
		// Trimmed by rune, not by byte: a path can hold multibyte characters, and lopping a
		// byte off one leaves invalid UTF-8 that renders as a replacement character the next
		// backspace cannot clear either.
		if r := []rune(a.path); len(r) > 0 {
			a.path = string(r[:len(r)-1])
			a.notice, a.matches = "", nil
		}
		return nil

	case len(msg.Runes) > 0:
		a.path += string(msg.Runes)
		a.notice, a.matches = "", nil
		return nil
	}
	return nil
}

// submit registers the repository, or explains why it cannot.
func (a *addProject) submit(svc api.Service) tea.Cmd {
	path, err := expandHome(strings.TrimSpace(a.path))
	if err != nil {
		a.notice = err.Error()
		return nil
	}
	if path == "" {
		a.notice = "a project needs a path"
		return nil
	}

	a.busy, a.notice = true, ""
	return func() tea.Msg {
		p, err := svc.AddProject(context.Background(), api.AddProjectReq{Path: path})
		return addProjectDoneMsg{name: p.Name, err: err}
	}
}

// done applies the result. A failure keeps the prompt open with what was typed still in it,
// because the usual failure is a typo in the path and retyping it from scratch is a punishment.
func (a *addProject) done(msg addProjectDoneMsg) {
	a.busy = false
	if msg.err != nil {
		a.notice = msg.err.Error()
		return
	}
	a.close()
}

// complete extends the typed path as far as the filesystem allows.
func (a *addProject) complete() {
	completed, matches := completePath(a.path)
	a.path, a.matches, a.notice = completed, matches, ""
	if len(matches) == 0 {
		a.notice = "no directory matches"
	}
}

// completePath completes a partially typed path against the filesystem, returning the extended
// path and every directory it matched.
//
// Directories only. A project is a repository and a repository is a directory, so offering
// regular files would be noise in the one place the field is least able to afford it — and
// completing to one would produce a path AddProject can only reject.
//
// Completion works on the typed string and expands ~ only to read the directory, so the tilde
// the user typed survives: rewriting it to /home/... mid-edit is a jarring thing for a text
// field to do to you.
func completePath(typed string) (string, []string) {
	// A bare word is a repository name, not a path. Nobody thinks of their project as
	// "~/Projects/mission-mojo"; they think of it as mission-mojo, and typing the first half
	// of a path you already know the end of is work the machine should be doing.
	if name := strings.TrimSpace(typed); name != "" && !strings.ContainsAny(typed, "/~") {
		if completed, matches := completeByName(name); len(matches) > 0 {
			return completed, matches
		}
	}

	dir, base := filepath.Split(typed)

	root := dir
	if root == "" {
		// A bare word completes against the working directory, which is what AddProject
		// resolves a relative path against too.
		root = "."
	}
	root, err := expandHome(root)
	if err != nil {
		return typed, nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		// An unreadable or missing directory is an ordinary thing to type on the way to a
		// real one. It completes to nothing rather than erroring.
		return typed, nil
	}

	var names []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base) {
			continue
		}
		// A dotfile is offered only once a dot has been typed, or completing inside a home
		// directory drowns in .cache and .local before reaching anything wanted.
		if base == "" && strings.HasPrefix(name, ".") {
			continue
		}
		if !isDir(filepath.Join(root, name)) {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return typed, nil
	}
	sort.Strings(names)

	completed := dir + longestCommonPrefix(names)
	if len(names) == 1 {
		// A single match is unambiguous, so land inside it: the next tab lists its children
		// rather than re-completing the name just finished.
		completed += string(filepath.Separator)
	}
	return completed, names
}

// searchRoots are where repositories usually live, for completing a bare name.
//
// A fixed list rather than a scan of the home directory: the point is to find a project in one
// keystroke, and walking every directory under $HOME to do it would be slower than typing the
// path.
var searchRoots = []string{".", "~/Projects", "~/projects", "~/src", "~/code", "~/dev", "~/work", "~/repos"}

// completeByName finds directories whose name contains what was typed.
//
// Contains rather than begins with: "blooms" should find pocket-blooms-ios, and a repository
// named for the thing it does rarely starts with the word you remember it by.
func completeByName(name string) (string, []string) {
	lower := strings.ToLower(name)

	var matches []string
	seen := map[string]bool{}
	for _, root := range searchRoots {
		dir, err := expandHome(root)
		if err != nil {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, ".") || !strings.Contains(strings.ToLower(n), lower) {
				continue
			}
			full := filepath.Join(dir, n)
			if seen[full] || !isDir(full) {
				continue
			}
			seen[full] = true
			matches = append(matches, full)
		}
	}
	sort.Strings(matches)

	switch len(matches) {
	case 0:
		return name, nil
	case 1:
		// Unambiguous, so land inside it and let the next tab list its contents.
		return matches[0] + string(filepath.Separator), matches
	default:
		// Ambiguous: the candidates are shown, and what was typed is left alone rather than
		// replaced by an arbitrary one of them.
		return name, matches
	}
}

// isDir reports whether path is a directory, following symlinks.
//
// os.DirEntry.IsDir reports false for a symlink pointing at one, and a projects directory full
// of symlinked repositories is a normal way to work.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// longestCommonPrefix is how far tab can extend without guessing.
//
// Compared by rune: truncating at a byte could split a multibyte character and leave the field
// holding invalid UTF-8 that matches nothing on the next tab.
func longestCommonPrefix(names []string) string {
	if len(names) == 0 {
		return ""
	}
	prefix := []rune(names[0])
	for _, n := range names[1:] {
		r := []rune(n)
		if len(r) < len(prefix) {
			prefix = prefix[:len(r)]
		}
		for i := range prefix {
			if prefix[i] != r[i] {
				prefix = prefix[:i]
				break
			}
		}
	}
	return string(prefix)
}

// expandHome resolves a leading ~.
//
// The TUI has to do this itself: a path typed into a form never passes through a shell, so
// "~/Projects/gravy" would reach os.Stat verbatim and fail with a "no such file or directory"
// naming a directory literally called "~" — which reads as a bug in Gravy, not a missing shell.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// matchList renders the candidates tab stopped between, on one line within maxw cells.
//
// Truncated by count rather than by cutting the line off, so the last name shown is a whole
// name: half a directory name is worse than an honest "+3 more".
func matchList(names []string, maxw int) string {
	if maxw < 12 {
		return fmt.Sprintf("%d matches", len(names))
	}
	var (
		shown []string
		width int
	)
	for i, n := range names {
		// Reserve room for the "+N more" that will be needed if anything is left over.
		more := ""
		if i < len(names)-1 {
			more = fmt.Sprintf("  +%d more", len(names)-i-1)
		}
		if width+len(n)+2+len(more) > maxw && len(shown) > 0 {
			return strings.Join(shown, "  ") + fmt.Sprintf("  +%d more", len(names)-len(shown))
		}
		shown = append(shown, n)
		width += len(n) + 2
	}
	return strings.Join(shown, "  ")
}

// view renders the prompt as an overlay panel.
func (a addProject) view(th Theme, width int) string {
	value := th.Accent.Render(a.path) + th.Muted.Render("▏")
	if a.path == "" {
		value = th.Muted.Render("~/Projects/my-repo") + th.Muted.Render("▏")
	}

	hint := "tab to complete · enter to register · esc to cancel"
	style := th.Muted
	switch {
	case a.busy:
		hint = "registering…"
	case a.notice != "":
		hint, style = a.notice, th.Danger
	}

	lines := []string{
		th.Header.Render("Add a project"),
		"",
		th.Muted.Render("Path to a git repository. The name, target branch and"),
		th.Muted.Render("validation steps are detected, and editable in Settings."),
		"",
		th.Muted.Render("  path  ") + value,
	}
	// One match is already in the field; listing it would just repeat what tab did.
	if len(a.matches) > 1 {
		lines = append(lines, "", th.Muted.Render("  "+matchList(a.matches, width-8)))
	}
	lines = append(lines, "", style.Render("  "+hint))

	body := strings.Join(lines, "\n")

	if width < 30 {
		return body
	}
	return th.Overlay.Render(body)
}
