package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/pot-roast-co/gravy/internal/core"
	"strings"
)

// projectSearch is shared by Projects and Plan; selection never reuses a different
// repository's planning session.
type projectSearch struct {
	open   bool
	query  string
	cursor int
	notice string
}

func (m Model) matchingProjects() []core.Project {
	var all []core.Project
	if m.active == SectionProjects {
		if p, ok := m.screens[SectionProjects].(*projects); ok {
			all = p.projects
		}
	} else {
		for _, p := range m.status.Projects {
			all = append(all, p.Project)
		}
	}
	var found []core.Project
	needle := strings.ToLower(strings.TrimSpace(m.projectSearch.query))
	for _, p := range all {
		if strings.Contains(strings.ToLower(p.Name+" "+p.Slug+" "+p.RepoPath), needle) {
			found = append(found, p)
		}
	}
	return found
}

func (m Model) searchProjectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := &m.projectSearch
	matches := m.matchingProjects()
	s.cursor = clamp(s.cursor, 0, max(0, len(matches)-1))
	s.notice = ""
	switch msg.String() {
	case "esc":
		s.open = false
	case "up":
		s.cursor = max(0, s.cursor-1)
	case "down":
		s.cursor = min(max(0, len(matches)-1), s.cursor+1)
	case "ctrl+u":
		s.query = ""
		s.cursor = 0
	case "backspace":
		r := []rune(s.query)
		if len(r) > 0 {
			s.query = string(r[:len(r)-1])
		}
		s.cursor = 0
	case "enter":
		if len(matches) == 0 {
			return m, nil
		}
		chosen := matches[s.cursor]
		if m.active == SectionPlan {
			p, ok := m.screens[SectionPlan].(*plan)
			if !ok {
				return m, nil
			}
			if p.busy {
				s.notice = "Wait for the current planning reply before switching projects."
				return m, nil
			}
			if p.pinnedID != "" && p.pinnedID != chosen.ID && len(p.entries) > 0 {
				s.notice = "Esc, then x to start over before choosing another project. Your conversation is preserved."
				return m, nil
			}
			p.pinnedID, p.pinnedName = chosen.ID, projectName(chosen)
			for i, p := range m.status.Projects {
				if p.Project.ID == chosen.ID {
					m.projectIdx = i
					break
				}
			}
		} else if p, ok := m.screens[SectionProjects].(*projects); ok {
			for i, pr := range p.projects {
				if pr.ID == chosen.ID {
					p.cursor = i
					p.scroll = 0
					break
				}
			}
		}
		s.open = false
	default:
		if len(msg.Runes) > 0 {
			s.query += string(msg.Runes)
			s.cursor = 0
		}
	}
	return m, nil
}

func (m Model) projectSearchView(height int) string {
	s := m.projectSearch
	lines := []string{m.theme.Header.Render("Search projects"), ""}
	lines = append(lines, inputLines("Search", s.query, m.width, max(2, height/3), m.theme)...)
	matches := m.matchingProjects()
	selected := -1
	for i, p := range matches {
		marker := "  "
		if i == clamp(s.cursor, 0, max(0, len(matches)-1)) {
			marker = "› "
			selected = len(lines)
		}
		line := marker + projectName(p) + " · " + p.RepoPath
		if marker == "› " {
			lines = append(lines, m.theme.Accent.Render(trunc(line, m.width)))
		} else {
			lines = append(lines, m.theme.Text.Render(trunc(line, m.width)))
		}
	}
	if len(matches) == 0 {
		lines = append(lines, "No matching projects")
	}
	footer := actionFooter(s.notice, "type to search · ↑/↓ select · enter choose · ctrl+u clear · esc cancel", m.width, m.theme)
	return pinFooter(lines, selected, height, m.theme, footer)
}
