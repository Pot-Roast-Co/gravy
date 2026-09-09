package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/api"
)

// agentsDetectedMsg carries what the daemon found.
type agentsDetectedMsg struct{ agents []api.AgentStatus }

// detectAgents probes the agent CLIs.
//
// Not on connect: it spawns processes, and paying for that on every launch to answer a question
// that only matters before the first project is registered would be a tax on everybody else.
func detectAgents(svc api.Service) tea.Cmd {
	return func() tea.Msg {
		return agentsDetectedMsg{agents: svc.DetectAgents(context.Background())}
	}
}

// setupView is what a fresh install shows instead of an empty dashboard.
//
// Someone who has never seen Gravy runs it and gets a screen with nothing on it and no
// indication of what to do — the README used to claim a wizard walks you through setup, which
// was not true. This is the smallest honest version: what Gravy needs, whether you have it, and
// the one key that starts.
//
// It answers three questions in the order they block you: can it run agents at all, does it have
// a repository, and what happens once it does.
func setupView(agents []api.AgentStatus, th Theme, width int) string {
	lines := []string{
		th.Header.Render("Gravy"),
		"",
		th.Muted.Render("  An engineering manager for coding agents. You write tickets and"),
		th.Muted.Render("  approve diffs; it runs the agents, worktrees, validation and merges."),
		"",
		th.Header.Render("  1. Agents"),
	}
	lines = append(lines, agentLines(agents, th)...)

	lines = append(lines,
		"",
		th.Header.Render("  2. A repository"),
	)
	if anyReady(agents) {
		lines = append(lines,
			th.Key.Render("     P")+th.Muted.Render("  add one — its path completes as you type"),
			th.Muted.Render("     Gravy detects its toolchain and proposes what agents may run."),
		)
	} else {
		// Registering a repository with no usable agent produces a queue that cannot move, so
		// the order is not decoration.
		lines = append(lines,
			th.Muted.Render("     Once an agent above is ready, ")+th.Key.Render("P")+
				th.Muted.Render(" registers your first repository."),
		)
	}

	lines = append(lines,
		"",
		th.Header.Render("  3. Then"),
		th.Muted.Render("     Write a ticket in ")+th.Key.Render("4 Backlog")+
			th.Muted.Render(", or talk it through in ")+th.Key.Render("2 Plan")+th.Muted.Render("."),
		th.Muted.Render("     Queue it, and an agent picks it up. It stops at ")+
			th.Key.Render("7 Review")+th.Muted.Render(" for you."),
		th.Muted.Render("     Nothing merges without your approval. No setting changes that."),
		"",
		th.Muted.Render("  ")+th.Key.Render("?")+th.Muted.Render(" lists every key · ")+
			th.Key.Render(SettingsKey)+th.Muted.Render(" is settings"),
	)
	return strings.Join(lines, "\n")
}

// agentLines reports each agent CLI and what to do about it.
func agentLines(agents []api.AgentStatus, th Theme) []string {
	if len(agents) == 0 {
		return []string{th.Muted.Render("     checking…")}
	}

	out := make([]string, 0, len(agents)+1)
	for _, a := range agents {
		var mark, note string
		var style = th.Muted
		switch {
		case a.Ready():
			mark, style = "ready", th.Success
		case a.Installed:
			// Installed but not logged in is a different problem from not installed, and
			// telling someone to install what they already have sends them the wrong way.
			mark, style = "not logged in", th.Warning
			note = "run: " + a.Command + " (and log in)"
		default:
			mark, style = "not installed", th.Muted
			note = a.Detail
		}
		out = append(out, fmt.Sprintf("     %-14s %s", a.ProviderID, style.Render(mark)))
		if note != "" {
			out = append(out, th.Muted.Render("       "+note))
		}
	}
	if !anyReady(agents) {
		out = append(out, "", th.Warning.Render(
			"     Gravy drives the agent CLIs you have logged in. It needs at least one."))
	}
	return out
}

// anyReady reports whether any agent can run work.
func anyReady(agents []api.AgentStatus) bool {
	for _, a := range agents {
		if a.Ready() {
			return true
		}
	}
	return false
}
