package tui

import (
	"strings"
	"testing"

	"github.com/pot-roast-co/gravy/internal/api"
)

func TestStatusBarMentionsANewerVersion(t *testing.T) {
	f := newFake()
	f.status.Update = api.UpdateStatus{Latest: "v0.1.4", Available: true}
	view := boot(t, f, 140, 24).View()

	if !strings.Contains(view, "v0.1.4 available") {
		t.Errorf("the frame never mentions the newer version:\n%s", view)
	}
}

// Nothing newer means nothing said. A status bar that always carries a version line teaches
// people to stop reading the status bar.
func TestStatusBarSaysNothingWhenCurrent(t *testing.T) {
	f := newFake()
	f.status.Update = api.UpdateStatus{Latest: "v0.1.4", Available: false}
	view := boot(t, f, 140, 24).View()

	if strings.Contains(view, "available") {
		t.Errorf("mentioned an update that is not one:\n%s", view)
	}
}

// And a build with no answer yet — the first seconds of a daemon, a machine with no network,
// checking switched off — says nothing rather than guessing.
func TestStatusBarSaysNothingWithoutAnAnswer(t *testing.T) {
	view := boot(t, newFake(), 140, 24).View()
	if strings.Contains(view, "available") {
		t.Errorf("invented an update before any check returned:\n%s", view)
	}
}
