package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestLongFeedbackWrapsKeepsCursorAndSubmitsWholeValue(t *testing.T) {
	f := reviewFixture()
	m := openReview(t, f, 60, 18)
	m = send(t, m, key("r"))
	value := strings.Repeat("session remains stuck 界界 ", 30) + "END"
	m = send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)})
	view := m.View()
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > 60 {
			t.Fatalf("overflow: %q", line)
		}
	}
	if lipgloss.Height(view) > 18 {
		t.Fatalf("too tall: %d", lipgloss.Height(view))
	}
	if !strings.Contains(view, "END▏") || !strings.Contains(view, "enter to send the message") {
		t.Fatal(view)
	}
	m, cmd := sendCmd(t, m, key("enter"))
	if cmd == nil {
		t.Fatal("no submit")
	}
	_ = send(t, m, cmd())
	if len(f.said) != 1 || f.said[0].Message != value {
		t.Fatal("wrapping changed submitted text")
	}
}

func TestFeedbackBackspacePreservesUnicode(t *testing.T) {
	f := reviewFixture()
	m := openReview(t, f, 80, 24)
	m = send(t, m, key("r"))
	m = send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("fix 界")})
	m = send(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	r := m.screens[SectionReview].(*review)
	if r.talk.input != "fix " || !utf8.ValidString(r.talk.input) {
		t.Fatalf("%q", r.talk.input)
	}
}

func TestReviewNoticeDoesNotHideActions(t *testing.T) {
	f := reviewFixture()
	m := openReview(t, f, 100, 24)
	r := m.screens[SectionReview].(*review)
	r.notice = "previous ticket sent back for changes"
	view := m.View()
	for _, want := range []string{r.notice, "a approve", "r changes", "T try feature"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %s: %s", want, view)
		}
	}
	if lipgloss.Height(view) > 24 {
		t.Fatal("notice pushed actions below screen")
	}
}
