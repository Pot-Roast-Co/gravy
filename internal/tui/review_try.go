package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/host"
)

type previewSavedMsg struct {
	ticketID, command string
	err               error
}
type reviewTriedMsg struct {
	ticketID, action string
	err              error
}

func (r *review) tryKey(msg tea.KeyMsg, ctx ViewContext) (Screen, tea.Cmd) {
	key := msg.String()
	if r.mode == reviewPreviewCommand {
		switch {
		case key == "esc":
			r.mode = reviewTry
			r.notice = ""
		case key == "ctrl+u":
			r.previewInput = ""
		case key == "backspace":
			if r.previewInput != "" {
				_, n := utf8.DecodeLastRuneInString(r.previewInput)
				r.previewInput = r.previewInput[:len(r.previewInput)-n]
			}
		case key == "enter":
			command := strings.TrimSpace(r.previewInput)
			if command == "" {
				r.notice = "enter the command that starts your app"
				return r, nil
			}
			id, projectID := r.ticketID, r.bundle.Project.ID
			r.mode = reviewTrySaving
			return r, func() tea.Msg {
				projects, err := ctx.Svc.ListProjects(context.Background(), api.ProjectFilter{IncludeArchived: true})
				if err != nil {
					return previewSavedMsg{ticketID: id, err: err}
				}
				for _, project := range projects {
					if project.ID == projectID {
						project.PreviewCommand = command
						return previewSavedMsg{ticketID: id, command: command, err: ctx.Svc.UpdateProject(context.Background(), project)}
					}
				}
				return previewSavedMsg{ticketID: id, err: fmt.Errorf("project no longer exists")}
			}
		case len(msg.Runes) > 0:
			r.previewInput += string(msg.Runes)
		}
		return r, nil
	}
	if r.mode == reviewTrySaving {
		return r, nil
	}
	r.notice = ""
	switch key {
	case "esc", "T":
		r.mode = reviewBrowsing
	case "e":
		if len(r.bundle.Project.PreviewServices) > 0 {
			r.notice = "Edit services in Projects → c settings → preview services"
			return r, nil
		}
		r.mode = reviewPreviewCommand
		r.previewInput = r.bundle.Project.PreviewCommand
	case "a":
		if len(r.bundle.Project.PreviewServices) == 0 && strings.TrimSpace(r.bundle.Project.PreviewCommand) == "" {
			r.mode = reviewPreviewCommand
			r.previewInput = ""
			return r, nil
		}
		return r, r.tryCommands("app", []core.Step{{Name: "app", Cmd: r.bundle.Project.PreviewCommand, Required: true}}, true)
	case "t":
		if len(r.bundle.Project.Validation) == 0 {
			r.notice = "no tests configured: Projects → c settings → validation"
			return r, nil
		}
		return r, r.tryCommands("checks", r.bundle.Project.Validation, false)
	}
	return r, nil
}

func (r *review) tryCommands(action string, steps []core.Step, interactive bool) tea.Cmd {
	wt := r.bundle.Ticket.WorktreePath
	if wt == "" {
		r.notice = "this ticket has no worktree to test"
		return nil
	}
	// A same-named local path is not evidence that a remote checkout is local.
	hostID := r.bundle.Run.HostID
	if hostID == "" {
		hostID = r.bundle.Project.HostID
	}
	if hostID != "" && hostID != "local" {
		r.notice = "this worktree is on " + hostID + "; run its commands there (remote previews are not supported yet)"
		return nil
	}
	if info, err := os.Stat(wt); err != nil || !info.IsDir() {
		r.notice = "the worktree is not on this machine: " + wt
		return nil
	}
	id := r.ticketID
	run := &host.TerminalRun{Dir: wt, Steps: append([]core.Step(nil), steps...), Interactive: interactive}
	if interactive {
		run.Services = append([]core.PreviewService(nil), r.bundle.Project.PreviewServices...)
	}
	return tea.Exec(run, func(err error) tea.Msg { return reviewTriedMsg{ticketID: id, action: action, err: err} })
}

func (r *review) tryView(ctx ViewContext) string {
	th := ctx.Theme
	lines := []string{th.Header.Render("Try feature · " + shortID(r.ticketID)), "", th.Text.Render(r.bundle.Ticket.Title), "", th.Muted.Render("Worktree: " + r.bundle.Ticket.WorktreePath), ""}
	command := r.bundle.Project.PreviewCommand
	if len(r.bundle.Project.PreviewServices) > 0 {
		command = "all preview services (Ctrl+C stops all)"
		for _, service := range r.bundle.Project.PreviewServices {
			lines = append(lines, th.Text.Render(service.Name+" · "+service.Dir+" · "+service.Command))
			if service.EnvFile != "" {
				lines = append(lines, th.Muted.Render("  Environment: "+service.EnvFile))
			}
		}
	}
	if command == "" {
		command = "not set — press a to configure"
	}
	lines = append(lines, th.Text.Render("Run app: "+command), th.Muted.Render("The app takes this terminal. Open its URL/window, then Ctrl+C to stop."), "", th.Text.Render("Run tests: project validation commands"))
	for _, step := range r.bundle.Project.Validation {
		lines = append(lines, th.Muted.Render("  "+step.Name+": "+step.Cmd))
	}
	lines = append(lines, "", th.Muted.Render("These manual runs leave the ticket awaiting your review."))
	if r.mode == reviewPreviewCommand {
		lines = append(lines, "", th.Text.Render("App command (saved for this project):"), strings.Join(inputLines("", r.previewInput, ctx.Width, max(2, ctx.Height/2), th), "\n"), th.Muted.Render("Examples: npm run dev · mix phx.server · go run ./cmd/app"))
	}
	footer := "a run app · t run tests · e edit app command · esc back"
	if r.mode == reviewPreviewCommand {
		footer = "enter save · ctrl+u clear · esc cancel"
	}
	if r.mode == reviewTrySaving {
		footer = "saving app command…"
	}
	if r.notice != "" {
		lines = append(lines, "", th.Warning.Render(r.notice))
	}
	lines = strings.Split(strings.Join(lines, "\n"), "\n")
	for i, line := range lines {
		lines[i] = trunc(line, ctx.Width)
	}
	return pinFooter(lines, cursorRow(lines), ctx.Height, th, th.Muted.Render(footer))
}
