package tui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/pot-roast-co/gravy/internal/core"
	"strings"
	"testing"
)

func TestAllScreenNoticesKeepOptions(t *testing.T) {
	th := DefaultTheme()
	ctx := ViewContext{Theme: th, Width: 50, Height: 30}
	notice := "sent back to the agent"
	tests := []struct{ name, footer, option string }{
		{"projects", (&projects{notice: notice}).footer(th, 50), "n new"},
		{"settings", (&settings{notice: notice}).footer(th, 50), "s save"},
		{"running", (&running{notice: notice}).footer(th, 50), "K kill"},
		{"needs you", (&needsYou{notice: notice}).footer(queueFixture().status.Attention[3], th, 50), "enter open the review"},
		{"backlog", (&queue{notice: notice, state: core.StateBacklog}).footer(ctx), "space queue it"},
		{"ready", (&queue{notice: notice, state: core.StateReady}).footer(ctx), "r reject"},
		{"plan", (&plan{notice: notice}).footer(ctx), "n what next"},
		{"review", (&review{notice: notice}).footer(th, 50), "tab file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plain := strings.Join(strings.Fields(tc.footer), " ")
			if !strings.Contains(plain, notice) || !strings.Contains(plain, tc.option) {
				t.Fatal(tc.footer)
			}
			for _, line := range strings.Split(tc.footer, "\n") {
				if lipgloss.Width(line) > 50 {
					t.Fatalf("overflow: %q", line)
				}
			}
		})
	}
}
