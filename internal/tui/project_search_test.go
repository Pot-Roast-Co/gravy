package tui

import (
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
	"strings"
	"testing"
)

func TestProjectSearchSelection(t *testing.T) {
	for _, section := range []Section{SectionProjects, SectionPlan} {
		t.Run(fmt.Sprint(section), func(t *testing.T) {
			first := core.Project{ID: "a", Name: "Alpha", Slug: "alpha"}
			target := core.Project{ID: "b", Name: "Mission Mojo", Slug: "mission-mojo", RepoPath: "/projects/mojo"}
			ps := newProjects()
			ps.projects = []core.Project{first, target}
			ps.loaded = true
			pl := newPlan()
			m := Model{active: section, keys: DefaultKeyMap(), theme: DefaultTheme(), width: 80, height: 24, screens: map[Section]Screen{SectionProjects: ps, SectionPlan: pl}}
			m.status.Projects = []api.ProjectStatus{{Project: first}, {Project: target}}
			next, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
			m = next.(Model)
			if !m.projectSearch.open {
				t.Fatal("search did not open")
			}
			next, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("MOJO")})
			m = next.(Model)
			if got := m.matchingProjects(); len(got) != 1 || got[0].ID != "b" {
				t.Fatalf("matches %+v", got)
			}
			if view := m.projectSearchView(15); !strings.Contains(view, "enter choose") || !strings.Contains(view, "Mission Mojo") {
				t.Fatal(view)
			}
			next, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
			m = next.(Model)
			if m.projectSearch.open {
				t.Fatal("picker remained open")
			}
			if section == SectionProjects && ps.cursor != 1 {
				t.Fatal("wrong project selected")
			}
			if section == SectionPlan && pl.pinnedID != "b" {
				t.Fatal("wrong planning project")
			}
		})
	}
}

func TestProjectSearchPreservesConversation(t *testing.T) {
	p := newPlan()
	p.pinnedID = "a"
	p.session = "existing"
	p.entries = []planEntry{{mine: true, text: "keep this"}}
	m := Model{active: SectionPlan, projectSearch: projectSearch{open: true}, screens: map[Section]Screen{SectionPlan: p}}
	m.status.Projects = []api.ProjectStatus{{Project: core.Project{ID: "b", Name: "other"}}}
	next, _ := m.searchProjectKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if !m.projectSearch.open || m.projectSearch.notice == "" || p.pinnedID != "a" || p.session != "existing" {
		t.Fatal("lost conversation protection")
	}
	m.projectSearch.query = "不存在"
	next, _ = m.searchProjectKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if !m.projectSearch.open {
		t.Fatal("selected nonexistent match")
	}
	next, _ = m.searchProjectKey(tea.KeyMsg{Type: tea.KeyBackspace})
	m = next.(Model)
	if m.projectSearch.query != "不存" {
		t.Fatal("backspace split Unicode")
	}
	next, _ = m.searchProjectKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.projectSearch.open || p.session != "existing" {
		t.Fatal("cancel changed conversation")
	}
}
