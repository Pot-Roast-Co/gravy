package tui

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
)

func TestPreviewServiceSettingsAddEditAndRemove(t *testing.T) {
	p := core.Project{}
	if err := previewFields(p)[0].Set(&p, "Backend, Frontend"); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"Backend directory": "backend", "Backend command": "mix phx.server", "Backend environment file": ".env", "Frontend directory": "frontend", "Frontend command": "flutter run -d chrome"}
	for _, field := range previewFields(p) {
		if value, ok := values[field.Label]; ok {
			if err := field.Set(&p, value); err != nil {
				t.Fatal(err)
			}
			if got := field.Get(p); got != value {
				t.Fatalf("%s: %s", field.Label, got)
			}
		}
	}
	if err := core.ValidatePreviewServices(p.PreviewServices); err != nil {
		t.Fatal(err)
	}
	if err := previewFields(p)[0].Set(&p, "Frontend"); err != nil {
		t.Fatal(err)
	}
	if len(p.PreviewServices) != 1 || p.PreviewServices[0].Command != "flutter run -d chrome" {
		t.Fatal(p)
	}
	if err := previewFields(p)[0].Set(&p, "Frontend, Frontend"); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := previewFields(p)[0].Set(&p, ""); err != nil || len(p.PreviewServices) != 0 {
		t.Fatal(p)
	}
}

func TestPreviewServicesAppearInReviewAndKeepTicketAwaitingReview(t *testing.T) {
	f := reviewFixture()
	f.review.Ticket.WorktreePath = t.TempDir()
	f.review.Project.PreviewServices = []core.PreviewService{{Name: "Backend", Dir: "backend", Command: "mix phx.server", EnvFile: ".env"}, {Name: "Frontend", Dir: "frontend", Command: "flutter run -d chrome"}}
	m := openReview(t, f, 120, 35)
	m = send(t, m, key("T"))
	for _, want := range []string{"Backend", "Frontend", "mix phx.server", "Environment: .env"} {
		if !strings.Contains(m.View(), want) {
			t.Fatal(m.View())
		}
	}
	m, cmd := sendCmd(t, m, key("a"))
	if cmd == nil {
		t.Fatal("no service handoff")
	}
	m = send(t, m, reviewTriedMsg{ticketID: f.review.Ticket.ID, action: "app"})
	if len(f.approved) != 0 || len(f.moved) != 0 {
		t.Fatal("preview decided ticket")
	}
	_ = m
}

func TestIncompletePreviewServiceKeepsSettingsOpen(t *testing.T) {
	p := newProjects()
	p.mode = projectConfig
	p.dirty = true
	p.draft.PreviewServices = []core.PreviewService{{Name: "Backend"}}
	if cmd := p.leaveConfig(ViewContext{}); cmd != nil || p.mode != projectConfig || !strings.Contains(p.notice, "command") {
		t.Fatalf("%+v", p)
	}
}
