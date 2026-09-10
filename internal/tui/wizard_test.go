package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
)

func wizardFixture(t *testing.T) (Model, *fakeService) {
	t.Helper()
	svc := newFake()
	svc.settings = api.Settings{Config: config.Default(), Agents: []api.AgentOption{{ProviderID: "codex", Models: []string{"default"}, Open: true}}}
	svc.agentStatus = []api.AgentStatus{{ProviderID: "codex", Installed: true, Authenticated: false, Command: "codex", Detail: "sign in"}}
	svc.setupPreview = api.SetupPreview{Project: core.Project{RepoPath: "/repo", Name: "repo", HostID: "local", TargetBranch: "main", MergeMode: core.LandMerge, MaxConcurrency: 1, Validation: []core.Step{{Name: "test", Cmd: "go test ./...", Required: true}}, Allowlist: core.Allowlist{Commands: []core.Pattern{{Match: "go"}}}}}
	m := boot(t, svc, 100, 30)
	m, cmd := sendCmd(t, m, startWizardMsg{})
	m = send(t, m, cmd())
	return m, svc
}

func wizardNext(t *testing.T, m Model) Model {
	t.Helper()
	return send(t, m, tea.KeyMsg{Type: tea.KeyCtrlN})
}

func TestWizardDoesNotWriteUntilFinalApproval(t *testing.T) {
	m, svc := wizardFixture(t)
	if !strings.Contains(m.View(), "not logged in") || !strings.Contains(m.View(), "codex") {
		t.Fatal(m.View())
	}
	original := cloneSetup(api.SetupInfo{Settings: svc.settings}).Settings.Config
	// Edit the provider in a private draft; it must not mutate even the fake API's maps.
	m = send(t, m, key("enter"))
	m.wizard.edit.buf = "false"
	m = send(t, m, key("enter"))
	if !reflect.DeepEqual(svc.settings.Config, original) {
		t.Fatal("draft mutated original settings")
	}
	m = wizardNext(t, m)
	m = wizardNext(t, m)
	m = send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/repo")})
	m, cmd := sendCmd(t, m, key("enter"))
	m = send(t, m, cmd())
	if m.wizard.step != 3 {
		t.Fatal("unauthenticated agent blocked setup")
	}
	m = wizardNext(t, m)
	if len(svc.setups) != 0 || len(svc.saved) != 0 || len(svc.added) != 0 {
		t.Fatal("saved before approval")
	}
	m, cmd = sendCmd(t, m, key("a"))
	m = send(t, m, cmd())
	if m.wizard != nil || len(svc.setups) != 1 || svc.setups[0].Project == nil {
		t.Fatal("approval did not save draft")
	}
	if svc.setups[0].Project.Allowlist == nil {
		t.Fatal("permissions not explicitly approved")
	}
}

func TestCancelWizardAtEveryStepWritesNothing(t *testing.T) {
	for step := 0; step <= 4; step++ {
		m, svc := wizardFixture(t)
		m.wizard.project = &svc.setupPreview.Project
		m.wizard.nextStep(step)
		m = send(t, m, key("esc"))
		if m.wizard != nil || len(svc.setups) != 0 || len(svc.saved) != 0 || len(svc.added) != 0 {
			t.Fatalf("step %d wrote on cancel", step)
		}
	}
}

func TestWizardSettingsRerunKeepsProjectsAndBuckets(t *testing.T) {
	m, svc := wizardFixture(t)
	m = send(t, m, key("esc"))
	svc.projects = []core.Project{{ID: "existing", Name: "keep"}}
	svc.settings.Config.Routes[core.Route("custom")] = []string{"codex/default"}
	svc.settings.Config.Hosts = []config.Host{{ID: "air", Target: "air", Workers: 2}}
	m.active = SectionSettings
	m, cmd := sendCmd(t, m, key("W"))
	m = send(t, m, cmd())
	m = wizardNext(t, m)
	m = send(t, m, key("enter"))
	m.wizard.edit.buf = "7"
	m = send(t, m, key("enter"))
	m = wizardNext(t, m)
	m = send(t, m, key("enter")) // skip adding a repository
	if m.wizard.step != 4 {
		t.Fatal("could not skip existing projects")
	}
	m, cmd = sendCmd(t, m, key("a"))
	_ = send(t, m, cmd())
	req := svc.setups[0]
	if req.Project != nil || req.Config.Concurrency.Workers != 7 || !reflect.DeepEqual(req.Config.Routes, svc.settings.Config.Routes) || !reflect.DeepEqual(req.Config.Hosts, svc.settings.Config.Hosts) {
		t.Fatal("unrelated settings changed")
	}
}

func TestCancelledWizardIgnoresLateDetection(t *testing.T) {
	svc := newFake()
	m := boot(t, svc, 80, 24)
	m, cmd := sendCmd(t, m, startWizardMsg{})
	m = send(t, m, key("esc"))
	m = send(t, m, cmd())
	if m.wizard != nil || len(svc.setups) != 0 {
		t.Fatal("late detection reopened or saved cancelled setup")
	}
}
