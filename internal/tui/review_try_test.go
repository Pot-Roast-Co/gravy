package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/pot-roast-co/gravy/internal/core"
)

func TestTryFeatureSavesProjectCommandBeforeRunning(t *testing.T) {
	f := reviewFixture()
	f.projects = []core.Project{f.review.Project}
	f.review.Ticket.WorktreePath = t.TempDir()
	m := openReview(t, f, 100, 30)
	if !strings.Contains(m.View(), "T try feature") {
		t.Fatal(m.View())
	}
	m = send(t, m, key("T"))
	for _, want := range []string{"Try feature", "a run app", "t run tests"} {
		if !strings.Contains(m.View(), want) {
			t.Fatal(m.View())
		}
	}
	m = send(t, m, key("a"))
	if !strings.Contains(m.View(), "App command") {
		t.Fatal(m.View())
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("npm run dev")})
	m, cmd := sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("no save")
	}
	if len(f.savedProjects) != 0 {
		t.Fatal("saved before command execution")
	}
	m = send(t, m, cmd())
	if len(f.savedProjects) != 1 || f.savedProjects[0].PreviewCommand != "npm run dev" {
		t.Fatalf("%+v", f.savedProjects)
	}
	if !strings.Contains(m.View(), "saved; press a") {
		t.Fatal(m.View())
	}
	m, cmd = sendCmd(t, m, key("a"))
	if cmd == nil {
		t.Fatal("no app handoff")
	}
	// A completed preview stays in Review and never calls approval/rejection.
	m = send(t, m, reviewTriedMsg{ticketID: f.review.Ticket.ID, action: "app"})
	if len(f.approved) != 0 || len(f.rejected) != 0 {
		t.Fatal("trying feature decided ticket")
	}
	m = send(t, m, key("esc"))
	if !strings.Contains(m.View(), "Add a Multiply") {
		t.Fatal(m.View())
	}
}

func TestTryFeatureTestsAndFailureKeepReview(t *testing.T) {
	f := reviewFixture()
	f.review.Ticket.WorktreePath = t.TempDir()
	f.review.Project.Validation = []core.Step{{Name: "test", Cmd: "go test ./...", Required: true}}
	m := openReview(t, f, 100, 30)
	m = send(t, m, key("T"))
	m, cmd := sendCmd(t, m, key("t"))
	if cmd == nil {
		t.Fatal("no tests handoff")
	}
	m = send(t, m, reviewTriedMsg{ticketID: f.review.Ticket.ID, action: "checks", err: fmt.Errorf("test failed")})
	if !strings.Contains(m.View(), "test failed") {
		t.Fatal(m.View())
	}
	if len(f.approved) != 0 || len(f.rejected) != 0 || len(f.moved) != 0 {
		t.Fatal("test result changed ticket")
	}
}

func TestTryFeatureRefusesUnavailableWorktree(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprint(remote), func(t *testing.T) {
			f := reviewFixture()
			f.review.Project.PreviewCommand = "npm run dev"
			if remote {
				f.review.Run.HostID = "air"
				f.review.Ticket.WorktreePath = t.TempDir()
			}
			m := openReview(t, f, 100, 30)
			m = send(t, m, key("T"))
			m, cmd := sendCmd(t, m, key("a"))
			if cmd != nil {
				t.Fatal("started in wrong worktree")
			}
			if !strings.Contains(m.View(), "worktree") {
				t.Fatal(m.View())
			}
		})
	}
}

func TestTryFeatureCommandCancelAndNarrowView(t *testing.T) {
	f := reviewFixture()
	m := openReview(t, f, 60, 24)
	m = send(t, m, key("T"))
	m = send(t, m, key("a"))
	m = send(t, m, key("q"))
	m = send(t, m, key("esc"))
	if len(f.savedProjects) != 0 {
		t.Fatal("cancel saved")
	}
	for _, line := range strings.Split(m.View(), "\n") {
		if lipgloss.Width(line) > 60 {
			t.Fatalf("overflow: %q", line)
		}
	}
	m, cmd := sendCmd(t, m, key("t"))
	if cmd != nil || !strings.Contains(m.View(), "no tests configured") {
		t.Fatal(m.View())
	}
}
