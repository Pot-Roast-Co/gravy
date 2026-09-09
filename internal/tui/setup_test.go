package tui

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/api"
)

// freshInstall is a daemon with nothing registered.
func freshInstall(t *testing.T, agents []api.AgentStatus) Model {
	t.Helper()
	f := newFake()
	f.status.Projects = nil
	f.agentStatus = agents
	m := boot(t, f, 100, 34)
	return send(t, m, agentsDetectedMsg{agents: agents})
}

// TestSetupSeparatesNotInstalledFromNotLoggedIn: they fail differently and are fixed
// differently, and telling somebody to install what they already have sends them the wrong way.
func TestSetupSeparatesNotInstalledFromNotLoggedIn(t *testing.T) {
	m := freshInstall(t, []api.AgentStatus{
		{ProviderID: "claude-code", Command: "claude", Installed: true, Authenticated: false},
		{ProviderID: "codex", Command: "codex", Installed: false, Detail: "not on PATH"},
	})
	view := m.View()

	if !strings.Contains(view, "not logged in") {
		t.Errorf("an installed but unauthenticated agent is not distinguished:\n%s", view)
	}
	if !strings.Contains(view, "not installed") {
		t.Errorf("a missing agent is not reported:\n%s", view)
	}
	// And it says what to do about the one that is nearly working.
	if !strings.Contains(view, "run: claude") {
		t.Errorf("no instruction for the agent that only needs a login:\n%s", view)
	}
}

// TestSetupWithNoAgentsSaysWhatGravyNeeds rather than offering a repository that could not run.
func TestSetupWithNoAgentsSaysWhatGravyNeeds(t *testing.T) {
	m := freshInstall(t, []api.AgentStatus{
		{ProviderID: "claude-code", Installed: false},
		{ProviderID: "codex", Installed: false},
	})
	view := m.View()

	if !strings.Contains(view, "needs at least one") {
		t.Errorf("does not say an agent is required:\n%s", view)
	}
	// Registering a repository with no usable agent produces a queue that cannot move.
	if strings.Contains(view, "add one — its path") {
		t.Error("offered to register a repository with no agent able to run it")
	}
}

// TestSetupWithAnAgentOffersTheNextStep.
func TestSetupWithAnAgentOffersTheNextStep(t *testing.T) {
	m := freshInstall(t, []api.AgentStatus{
		{ProviderID: "claude-code", Installed: true, Authenticated: true},
	})
	view := m.View()

	for _, want := range []string{"ready", "add one", "Review", "without your approval"} {
		if !strings.Contains(view, want) {
			t.Errorf("the first run omits %q:\n%s", want, view)
		}
	}
}

// TestSetupOnlyReplacesTheDashboard: a setup screen that swallows every section is a wall, not
// a welcome.
func TestSetupOnlyReplacesTheDashboard(t *testing.T) {
	m := freshInstall(t, []api.AgentStatus{{ProviderID: "claude-code", Installed: true, Authenticated: true}})
	if !strings.Contains(m.View(), "engineering manager") {
		t.Fatal("the dashboard does not show the first-run screen")
	}

	m = send(t, m, key(SectionPlan.Key()))
	if strings.Contains(m.View(), "engineering manager") {
		t.Errorf("the first-run screen swallowed the Plan section:\n%s", m.View())
	}
}

// TestSetupDisappearsOnceAProjectExists.
func TestSetupDisappearsOnceAProjectExists(t *testing.T) {
	f := newFake() // the fake registers two projects
	m := boot(t, f, 100, 34)
	if strings.Contains(m.View(), "engineering manager") {
		t.Errorf("the first-run screen is shown to an existing install:\n%s", m.View())
	}
}
