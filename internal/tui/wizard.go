package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/core"
)

type startWizardMsg struct{}
type wizardInfoMsg struct {
	token int
	info  api.SetupInfo
	err   error
}
type wizardPreviewMsg struct {
	token   int
	preview api.SetupPreview
	err     error
}
type wizardSavedMsg struct {
	token    int
	settings api.Settings
	err      error
}

// wizard owns only a draft. Its sole write command is created on final approval.
type wizard struct {
	token, step  int
	info         api.SetupInfo
	edit         *settings
	project      *core.Project
	path         addProject
	evidence     []string
	busy         bool
	notice       string
	reviewCursor int
}

func (m *Model) startWizard() tea.Cmd {
	m.wizardSeq++
	token := m.wizardSeq
	m.wizard = &wizard{token: token, busy: true, edit: newSettings()}
	return func() tea.Msg {
		info, err := m.svc.SetupInfo(context.Background())
		return wizardInfoMsg{token: token, info: info, err: err}
	}
}

func (w *wizard) fields() []settingField {
	all := buildFields(w.edit)
	var out []settingField
	for _, f := range all {
		if (w.step == 0 && f.Section == "Agents") ||
			(w.step == 1 && (f.Section == "Concurrency" || strings.HasPrefix(f.Section, "Bucket"))) {
			out = append(out, f)
		}
	}
	if w.step != 3 || w.project == nil {
		return out
	}
	hosts := []string{"local"}
	for _, h := range w.edit.cfg.Hosts {
		hosts = append(hosts, h.ID)
	}
	out = append(out, settingField{Label: "name", Get: func(*settings) string { return w.project.Name }, Set: func(_ *settings, v string) error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("a project needs a name")
		}
		w.project.Name = v
		return nil
	}})
	for _, pf := range projectFields(w.info.Settings.Agents, hosts) {
		field := pf
		out = append(out, settingField{Label: field.Label, Hint: field.Hint, Get: func(*settings) string { return field.Get(*w.project) }, Set: func(_ *settings, v string) error { return field.Set(w.project, v) }})
	}
	out = append(out,
		settingField{Label: "required OS", Hint: "comma separated; blank permits any OS", Get: func(*settings) string { return strings.Join(w.project.Requirements.OS, ", ") }, Set: func(_ *settings, v string) error { w.project.Requirements.OS = splitSetupList(v); return nil }},
		settingField{Label: "required tools", Hint: "comma separated tool names", Get: func(*settings) string {
			var names []string
			for k := range w.project.Requirements.Tools {
				names = append(names, k)
			}
			sort.Strings(names)
			return strings.Join(names, ", ")
		}, Set: func(_ *settings, v string) error {
			w.project.Requirements.Tools = map[string]string{}
			for _, k := range splitSetupList(v) {
				w.project.Requirements.Tools[k] = ""
			}
			return nil
		}},
		settingField{Label: "read paths", Hint: "worktree-relative globs, comma separated", Get: func(*settings) string { return strings.Join(w.project.Allowlist.ReadPaths, ", ") }, Set: func(_ *settings, v string) error { w.project.Allowlist.ReadPaths = splitSetupList(v); return nil }},
		settingField{Label: "write paths", Hint: "worktree-relative globs, comma separated", Get: func(*settings) string { return strings.Join(w.project.Allowlist.WritePaths, ", ") }, Set: func(_ *settings, v string) error { w.project.Allowlist.WritePaths = splitSetupList(v); return nil }},
		settingField{Label: "network", Hint: "true or false", Get: func(*settings) string { return strconv.FormatBool(w.project.Allowlist.Network) }, Set: func(_ *settings, v string) error {
			b, err := strconv.ParseBool(v)
			if err == nil {
				w.project.Allowlist.Network = b
			}
			return err
		}},
	)
	return out
}

func splitSetupList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (w *wizard) nextStep(step int) {
	w.step = step
	w.edit.cursor = 0
	w.edit.fields = w.fields()
	w.notice = ""
}

func (w *wizard) key(msg tea.KeyMsg, svc api.Service) (bool, tea.Cmd) {
	key := msg.String()
	if w.busy {
		if key == "esc" && w.step != 4 {
			return true, nil
		}
		return false, nil
	}
	if w.edit.editing {
		_, cmd := w.edit.handleKey(msg, ViewContext{Svc: svc})
		w.edit.fields = w.fields()
		return false, cmd
	}
	if key == "esc" {
		return true, nil
	}
	if w.step == 2 {
		switch {
		case key == "ctrl+b":
			w.nextStep(1)
		case key == "ctrl+n" || key == "enter":
			if strings.TrimSpace(w.path.path) == "" && len(w.info.Projects) > 0 {
				w.project = nil
				w.nextStep(4)
				return false, nil
			}
			path, err := expandHome(strings.TrimSpace(w.path.path))
			if err != nil || path == "" {
				w.notice = "enter a repository path"
				return false, nil
			}
			w.busy = true
			return false, func() tea.Msg {
				p, err := svc.PreviewSetup(context.Background(), api.AddProjectReq{Path: path})
				return wizardPreviewMsg{token: w.token, preview: p, err: err}
			}
		case key == "tab":
			w.path.complete()
		case key == "backspace":
			r := []rune(w.path.path)
			if len(r) > 0 {
				w.path.path = string(r[:len(r)-1])
			}
		case len(msg.Runes) > 0:
			w.path.path += string(msg.Runes)
		}
		return false, nil
	}
	switch key {
	case "ctrl+b":
		if w.step > 0 {
			w.nextStep(w.step - 1)
		}
	case "ctrl+n":
		if w.step < 4 {
			w.nextStep(w.step + 1)
		}
	case "a":
		if w.step != 4 {
			break
		}
		if err := w.edit.cfg.Validate(); err != nil {
			w.notice = err.Error()
			return false, nil
		}
		req := api.SetupRequest{Original: w.info.Settings.Config, Config: w.edit.cfg}
		if w.project != nil {
			p := *w.project
			req.Project = &api.AddProjectReq{Path: p.RepoPath, Name: p.Name, Host: p.HostID, TargetBranch: p.TargetBranch, MergeMode: p.MergeMode, Validation: p.Validation, Allowlist: &p.Allowlist, Requirements: p.Requirements, Routes: p.Routes, ParallelMode: p.ParallelMode, MaxConcurrency: p.MaxConcurrency}
		}
		w.busy = true
		return false, func() tea.Msg {
			st, err := svc.ApplySetup(context.Background(), req)
			return wizardSavedMsg{token: w.token, settings: st, err: err}
		}
	case "up", "k":
		if w.step == 4 {
			w.reviewCursor = max(0, w.reviewCursor-1)
		} else {
			w.edit.cursor = max(0, w.edit.cursor-1)
		}
	case "down", "j":
		if w.step == 4 {
			w.reviewCursor++
		} else {
			w.edit.cursor = min(len(w.edit.fields)-1, w.edit.cursor+1)
		}
	case "enter":
		if w.step != 4 && len(w.edit.fields) > 0 {
			w.edit.editing = true
			w.edit.buf = w.edit.fields[w.edit.cursor].Get(w.edit)
		}
	}
	return false, nil
}

func (w *wizard) view(ctx ViewContext) string {
	th := ctx.Theme
	names := []string{"Agents", "Workers and buckets", "Repository", "Project suggestions", "Approve setup"}
	lines := []string{th.Header.Render(fmt.Sprintf("Setup · %d/5 · %s", w.step+1, names[w.step])), th.Muted.Render("Nothing is saved until you approve the final step."), ""}
	if w.busy {
		return strings.Join(append(lines, "working…"), "\n")
	}
	selected := 0
	switch w.step {
	case 0:
		lines = append(lines, agentLines(w.info.Agents, th)...)
		lines = append(lines, "", "Gravy uses the CLIs' own login; it does not store credentials.")
		for _, a := range w.info.Settings.Agents {
			lines = append(lines, a.ProviderID+" models: "+strings.Join(a.Models, ", "))
		}
		lines = append(lines, "You can finish setup before signing in.", "")
	case 1:
		lines = append(lines, fmt.Sprintf("Local host: %s/%s · RAM %.1f GiB", w.info.Caps.OS, w.info.Caps.Arch, float64(w.info.Caps.RAMBytes)/(1<<30)))
		var tools []string
		for k, v := range w.info.Caps.Tools {
			tools = append(tools, k+" "+v)
		}
		sort.Strings(tools)
		lines = append(lines, "Tools: "+strings.Join(tools, ", "), "Choices run in order; later entries are fallbacks.", "")
	case 2:
		lines = append(lines, "Repository path (Tab completes):", w.path.path+"▏")
		if len(w.info.Projects) > 0 {
			lines = append(lines, "Leave blank to keep existing projects and edit global settings only.")
		}
		lines = append(lines, w.path.notice, strings.Join(w.path.matches, "  "))
	case 3:
		lines = append(lines, w.evidence...)
		lines = append(lines, "Edit any suggestion below.")
		if len(w.project.Requirements.OS) > 0 && w.project.Requirements.OS[0] == "darwin" {
			lines = append(lines, "Xcode may need a scheme and destination.")
		}
		lines = append(lines, "")
	case 4:
		lines = append(lines, "Review every value below; Ctrl+B returns to editing.", "")
		for _, f := range buildFields(w.edit) {
			if f.Label != "delete" && f.Label != "new bucket" {
				lines = append(lines, f.Section+" / "+f.Label+": "+f.Get(w.edit))
			}
		}
		if w.project != nil {
			lines = append(lines, "", "Repository: "+w.project.RepoPath)
			old := w.step
			w.step = 3
			fields := w.fields()
			w.step = old
			for _, f := range fields {
				lines = append(lines, f.Label+": "+f.Get(w.edit))
			}
		} else {
			lines = append(lines, "", "Existing projects: unchanged")
		}
		var wrapped []string
		for _, line := range lines {
			wrapped = append(wrapped, wrapText(line, max(8, ctx.Width-2))...)
		}
		lines = wrapped
		w.reviewCursor = min(w.reviewCursor, len(lines)-1)
		selected = w.reviewCursor
	}
	if w.step != 2 && w.step != 4 {
		for i, f := range w.edit.fields {
			value := f.Get(w.edit)
			prefix := "  "
			if i == w.edit.cursor {
				prefix = "▸ "
				selected = len(lines)
				if w.edit.editing {
					value = w.edit.buf + "▏"
				}
			}
			lines = append(lines, prefix+f.Label+": "+value)
		}
	}
	footer := "↑↓ select · Enter edit · Ctrl+N next · Ctrl+B back · Esc cancel"
	if w.step == 2 {
		footer = "Tab complete · Enter next · Ctrl+B back · Esc cancel"
	}
	if w.step == 4 {
		footer = "↑↓ scroll · a approve and save · Ctrl+B back · Esc cancel"
	}
	if w.edit.editing {
		footer = w.edit.fields[w.edit.cursor].Hint + " · Enter accept edit · Esc cancel edit"
	}
	notice := w.notice
	if w.edit.notice != "" {
		notice = w.edit.notice
	}
	if notice != "" {
		footer = notice + "\n" + footer
	}
	return pinFooter(lines, selected, ctx.Height, th, th.Muted.Render(footer))
}

// cloneSetup isolates maps and slices so editing a draft cannot mutate the API snapshot.
func cloneSetup(info api.SetupInfo) api.SetupInfo {
	b, _ := json.Marshal(info)
	var copy api.SetupInfo
	_ = json.Unmarshal(b, &copy)
	return copy
}
