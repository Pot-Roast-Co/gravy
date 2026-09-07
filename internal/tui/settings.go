package tui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
)

// settingField is one editable line.
//
// Every field carries its own parser rather than the screen switching on a type, so adding a
// setting is one entry here and nothing else.
type settingField struct {
	Section string
	Label   string
	Hint    string
	Get     func(*settings) string
	Set     func(*settings, string) error
}

// settings edits the daemon's configuration and its projects.
//
// It works on a copy and saves on demand: a config file rewritten on every keystroke would be
// read by a daemon mid-edit, and a half-typed duration is not a configuration.
type settings struct {
	loaded   api.Settings
	cfg      config.Config
	projects []core.Project
	dirtyIDs map[string]bool

	fields []settingField
	cursor int

	loadedOK bool
	err      error
	editing  bool
	buf      string
	notice   string
	dirty    bool
}

func newSettings() *settings { return &settings{dirtyIDs: map[string]bool{}} }

// CapturesKeys is true while a field is being edited — a command or a branch name is exactly the
// sort of text that contains a "q".
func (s *settings) CapturesKeys() bool { return s.editing }

type (
	settingsLoadedMsg struct {
		settings api.Settings
		projects []core.Project
	}
	settingsErrMsg   struct{ err error }
	settingsSavedMsg struct {
		settings api.Settings
		err      error
	}
)

func loadSettings(svc api.Service) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		st, err := svc.GetSettings(ctx)
		if err != nil {
			return settingsErrMsg{err}
		}
		projects, err := svc.ListProjects(ctx)
		if err != nil {
			return settingsErrMsg{err}
		}
		return settingsLoadedMsg{settings: st, projects: projects}
	}
}

func (s *settings) Update(msg tea.Msg, ctx ViewContext) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case enteredMsg:
		if s.dirty {
			// Reloading would discard edits that are not saved yet.
			return s, nil
		}
		return s, loadSettings(ctx.Svc)

	case refreshedMsg:
		return s, nil

	case settingsLoadedMsg:
		s.loaded, s.cfg, s.projects = msg.settings, msg.settings.Config, msg.projects
		s.loadedOK, s.err, s.dirty = true, nil, false
		s.dirtyIDs = map[string]bool{}
		s.fields = buildFields(s)
		return s, nil

	case settingsErrMsg:
		s.err = msg.err
		return s, nil

	case settingsSavedMsg:
		if msg.err != nil {
			s.notice = "save failed: " + msg.err.Error()
			return s, nil
		}
		s.loaded, s.cfg = msg.settings, msg.settings.Config
		s.dirty, s.dirtyIDs = false, map[string]bool{}
		s.notice = "saved"
		return s, nil

	case tea.KeyMsg:
		return s.handleKey(msg, ctx)
	}
	return s, nil
}

func (s *settings) handleKey(msg tea.KeyMsg, ctx ViewContext) (Screen, tea.Cmd) {
	key := msg.String()

	if s.editing {
		switch {
		case key == "esc":
			s.editing, s.buf = false, ""
		case key == "enter":
			if err := s.fields[s.cursor].Set(s, strings.TrimSpace(s.buf)); err != nil {
				s.notice = err.Error()
				return s, nil
			}
			s.editing, s.buf, s.dirty, s.notice = false, "", true, ""
			s.fields = buildFields(s)
		case key == "backspace":
			if s.buf != "" {
				s.buf = s.buf[:len(s.buf)-1]
			}
		case len(msg.Runes) == 1:
			s.buf += string(msg.Runes)
			s.notice = ""
		}
		return s, nil
	}

	if !s.loadedOK || len(s.fields) == 0 {
		return s, nil
	}
	s.cursor = clamp(s.cursor, 0, len(s.fields)-1)

	switch key {
	case "up", "k":
		if s.cursor > 0 {
			s.cursor--
		}
	case "down", "j":
		if s.cursor < len(s.fields)-1 {
			s.cursor++
		}
	case "enter":
		s.editing, s.buf, s.notice = true, s.fields[s.cursor].Get(s), ""
	case "s":
		return s, s.save(ctx)
	case "r":
		s.dirty = false
		s.notice = "reloaded"
		return s, loadSettings(ctx.Svc)
	}
	return s, nil
}

// save writes the config and every project that was touched.
func (s *settings) save(ctx ViewContext) tea.Cmd {
	cfg := s.cfg
	changed := make([]core.Project, 0, len(s.dirtyIDs))
	for _, p := range s.projects {
		if s.dirtyIDs[p.ID] {
			changed = append(changed, p)
		}
	}

	svc := ctx.Svc
	return func() tea.Msg {
		bg := context.Background()
		for _, p := range changed {
			if err := svc.UpdateProject(bg, p); err != nil {
				return settingsSavedMsg{err: fmt.Errorf("project %s: %w", p.Slug, err)}
			}
		}
		st, err := svc.UpdateSettings(bg, cfg)
		return settingsSavedMsg{settings: st, err: err}
	}
}

// ---- the field table -----------------------------------------------------

// buildFields lists every editable setting, in the order the screen shows them.
func buildFields(s *settings) []settingField {
	var out []settingField

	out = append(out, settingField{
		Section: "Concurrency", Label: "workers",
		Hint: "worker slots shared across all projects",
		Get:  func(s *settings) string { return strconv.Itoa(s.cfg.Concurrency.Workers) },
		Set: func(s *settings, v string) error {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return fmt.Errorf("workers must be a positive whole number")
			}
			s.cfg.Concurrency.Workers = n
			return nil
		},
	})

	// One pair of lines per bucket, over the buckets this configuration actually defines —
	// their names are the user's, not a list compiled into Gravy.
	for _, r := range configuredBuckets(s.cfg) {
		route := r
		out = append(out,
			settingField{
				Section: "Bucket " + string(route), Label: "agents",
				Hint: "comma-separated provider/model, first usable one wins",
				Get:  func(s *settings) string { return strings.Join(s.cfg.Routes[route], ", ") },
				Set: func(s *settings, v string) error {
					choices, err := parseChoices(v)
					if err != nil {
						return err
					}
					if s.cfg.Routes == nil {
						s.cfg.Routes = map[core.Route][]string{}
					}
					s.cfg.Routes[route] = choices
					return nil
				},
			},
			settingField{
				Section: "Bucket " + string(route), Label: "capacity",
				Hint: "how many may run at once; blank is uncapped",
				Get: func(s *settings) string {
					if n, ok := s.cfg.Concurrency.Routes[route]; ok && n > 0 {
						return strconv.Itoa(n)
					}
					return ""
				},
				Set: func(s *settings, v string) error {
					if s.cfg.Concurrency.Routes == nil {
						s.cfg.Concurrency.Routes = map[core.Route]int{}
					}
					if v == "" || v == "0" {
						delete(s.cfg.Concurrency.Routes, route)
						return nil
					}
					n, err := strconv.Atoi(v)
					if err != nil || n < 0 {
						return fmt.Errorf("a cap must be a whole number, or blank for uncapped")
					}
					s.cfg.Concurrency.Routes[route] = n
					return nil
				},
			},
			settingField{
				Section: "Bucket " + string(route), Label: "delete",
				Hint: "type the bucket's name to remove it",
				Get:  func(s *settings) string { return "" },
				Set: func(s *settings, v string) error {
					if v != string(route) {
						return fmt.Errorf("type %q exactly to remove this bucket", route)
					}
					delete(s.cfg.Routes, route)
					delete(s.cfg.Concurrency.Routes, route)
					return nil
				},
			},
		)
	}

	out = append(out, settingField{
		Section: "Buckets", Label: "new bucket",
		Hint: "a name of your own — planning, astra, whatever you call it",
		Get:  func(s *settings) string { return "" },
		Set: func(s *settings, v string) error {
			name := core.Route(strings.TrimSpace(v))
			if v == "" {
				return nil
			}
			if !name.Named() {
				return fmt.Errorf("use a word without spaces, slashes, commas or colons")
			}
			if _, exists := s.cfg.Routes[name]; exists {
				return fmt.Errorf("there is already a bucket called %q", name)
			}
			if s.cfg.Routes == nil {
				s.cfg.Routes = map[core.Route][]string{}
			}
			// Created empty: an agent list invented on the user's behalf would be a guess
			// about which model they meant.
			s.cfg.Routes[name] = nil
			return nil
		},
	})

	for _, id := range sortedProviderIDs(s.cfg) {
		providerID := id
		out = append(out,
			settingField{
				Section: "Agents", Label: providerID + " enabled",
				Hint: "true or false",
				Get:  func(s *settings) string { return strconv.FormatBool(s.cfg.Providers[providerID].Enabled) },
				Set: func(s *settings, v string) error {
					b, err := strconv.ParseBool(v)
					if err != nil {
						return fmt.Errorf("enabled must be true or false")
					}
					p := s.cfg.Providers[providerID]
					p.Enabled = b
					s.cfg.Providers[providerID] = p
					return nil
				},
			},
			settingField{
				Section: "Agents", Label: providerID + " command",
				Hint: "the executable to run, looked up on PATH",
				Get:  func(s *settings) string { return s.cfg.Providers[providerID].Command },
				Set: func(s *settings, v string) error {
					if strings.TrimSpace(v) == "" {
						return fmt.Errorf("a provider needs a command")
					}
					p := s.cfg.Providers[providerID]
					p.Command = v
					s.cfg.Providers[providerID] = p
					return nil
				},
			})
	}

	out = append(out,
		durationField("Timeouts", "run", "how long one agent run may take",
			func(s *settings) *config.Duration { return &s.cfg.Timeouts.Run }),
		durationField("Timeouts", "validation step", "how long one validation command may take",
			func(s *settings) *config.Duration { return &s.cfg.Timeouts.ValidationStep }),
		durationField("Timeouts", "stall", "silence after which a run is considered stalled",
			func(s *settings) *config.Duration { return &s.cfg.Timeouts.Stall }),
		settingField{
			Section: "Retry", Label: "self-correction budget",
			Hint: "how many times an agent may fix its own validation failure",
			Get:  func(s *settings) string { return strconv.Itoa(s.cfg.Retry.SelfCorrectionBudget) },
			Set: func(s *settings, v string) error {
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 {
					return fmt.Errorf("the budget must be zero or a positive whole number")
				}
				s.cfg.Retry.SelfCorrectionBudget = n
				return nil
			},
		},
		durationField("Retry", "quota cooldown", "how long a model rests after a quota failure",
			func(s *settings) *config.Duration { return &s.cfg.Retry.CooldownQuota }),
		durationField("Retry", "rate-limit cooldown", "how long a model rests after a rate limit",
			func(s *settings) *config.Duration { return &s.cfg.Retry.CooldownRateLimit }),
		durationField("Retention", "run logs", "how long finished runs' logs are kept; summaries are unaffected",
			func(s *settings) *config.Duration { return &s.cfg.Retention.RunLogs }),
	)

	// Projects, which live in the database rather than the config file.
	for i := range s.projects {
		idx := i
		p := &s.projects[idx]
		mark := func(s *settings) { s.dirtyIDs[s.projects[idx].ID] = true }

		out = append(out,
			settingField{
				Section: "Project " + p.Slug, Label: "target branch",
				Hint: "the branch approved work merges into",
				Get:  func(s *settings) string { return s.projects[idx].TargetBranch },
				Set: func(s *settings, v string) error {
					if strings.TrimSpace(v) == "" {
						return fmt.Errorf("a project needs a target branch")
					}
					s.projects[idx].TargetBranch = v
					mark(s)
					return nil
				},
			},
			settingField{
				Section: "Project " + p.Slug, Label: "parallel",
				Hint: "true runs several tickets at once and you handle the conflicts",
				Get:  func(s *settings) string { return strconv.FormatBool(s.projects[idx].ParallelMode) },
				Set: func(s *settings, v string) error {
					b, err := strconv.ParseBool(v)
					if err != nil {
						return fmt.Errorf("parallel must be true or false")
					}
					s.projects[idx].ParallelMode = b
					if b && s.projects[idx].MaxConcurrency < 1 {
						s.projects[idx].MaxConcurrency = 2
					}
					mark(s)
					return nil
				},
			},
			settingField{
				Section: "Project " + p.Slug, Label: "max concurrency",
				Hint: "tickets in flight at once when parallel",
				Get:  func(s *settings) string { return strconv.Itoa(s.projects[idx].MaxConcurrency) },
				Set: func(s *settings, v string) error {
					n, err := strconv.Atoi(v)
					if err != nil || n < 1 {
						return fmt.Errorf("concurrency must be at least 1")
					}
					s.projects[idx].MaxConcurrency = n
					mark(s)
					return nil
				},
			},
			settingField{
				Section: "Project " + p.Slug, Label: "validation",
				Hint: "name:command, separated by ; — what must pass before review",
				Get: func(s *settings) string {
					parts := make([]string, 0, len(s.projects[idx].Validation))
					for _, st := range s.projects[idx].Validation {
						parts = append(parts, st.Name+":"+st.Cmd)
					}
					return strings.Join(parts, "; ")
				},
				Set: func(s *settings, v string) error {
					steps, err := parseSteps(v)
					if err != nil {
						return err
					}
					s.projects[idx].Validation = steps
					mark(s)
					return nil
				},
			},
		)
	}
	return out
}

func durationField(section, label, hint string, ref func(*settings) *config.Duration) settingField {
	return settingField{
		Section: section, Label: label, Hint: hint,
		Get: func(s *settings) string { return ref(s).String() },
		Set: func(s *settings, v string) error {
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				return fmt.Errorf("use a duration like 30m, 1h30m or 336h")
			}
			*ref(s) = config.Duration(d)
			return nil
		},
	}
}

// parseSteps reads "name:command; name:command" into validation steps.
func parseSteps(v string) ([]core.Step, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	var out []core.Step
	for _, part := range strings.Split(v, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, cmd, ok := strings.Cut(part, ":")
		if !ok {
			name, cmd = "check", part
		}
		if strings.TrimSpace(cmd) == "" {
			return nil, fmt.Errorf("validation step %q has no command", part)
		}
		out = append(out, core.Step{
			Name: strings.TrimSpace(name), Cmd: strings.TrimSpace(cmd),
			Required: true, Timeout: 10 * time.Minute,
		})
	}
	return out, nil
}

func sortedProviderIDs(c config.Config) []string {
	out := make([]string, 0, len(c.Providers))
	for id := range c.Providers {
		out = append(out, id)
	}
	// Stable order: a settings screen whose rows move between renders is unusable.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ---- rendering -----------------------------------------------------------

func (s *settings) View(ctx ViewContext) string {
	th := ctx.Theme
	if ctx.Width <= 0 || ctx.Height <= 0 {
		return ""
	}
	switch {
	case s.err != nil:
		return strings.Join([]string{
			th.Danger.Render("Could not load settings."), "", th.Muted.Render(s.err.Error()),
		}, "\n")
	case !s.loadedOK:
		return th.Muted.Render("loading settings…")
	}

	lines := []string{
		th.Header.Render("Settings"),
		th.Muted.Render("  " + s.loaded.Path),
	}

	// Settings saved but not in effect: a screen that accepted a change silently and did not
	// apply it would leave you believing something that is not true.
	if len(s.loaded.PendingRestart) > 0 {
		lines = append(lines,
			th.Warning.Render("  ⚠ saved, but the running daemon still uses the old values for:"),
			th.Warning.Render("    "+trunc(strings.Join(s.loaded.PendingRestart, ", "), max(0, ctx.Width-4))),
			th.Muted.Render("    restart it with: gravy serve"))
	}

	selected := -1
	section := ""
	for i, f := range s.fields {
		if f.Section != section {
			section = f.Section
			lines = append(lines, "", th.Header.Render(section))
		}

		value := f.Get(s)
		style := th.Text
		if value == "" {
			value, style = "(unset)", th.Muted
		}

		marker := "  "
		if i == s.cursor {
			marker = "▸ "
			selected = len(lines)
			if s.editing {
				lines = append(lines, th.Accent.Render(marker+columns(ctx.Width-2,
					col{text: f.Label, width: 22},
					col{text: s.buf + "▏", flex: true},
				)))
				continue
			}
			style = th.Accent
		}
		lines = append(lines, style.Render(marker+columns(ctx.Width-2,
			col{text: f.Label, width: 22, style: th.Muted},
			col{text: value, flex: true, style: style},
		)))
	}

	// The footer is pinned rather than appended to the scrolling list. This list is longer
	// than any terminal, so a footer inside the window is a footer you never see — which
	// silently hid every validation message and the unsaved-changes warning.
	return pinFooter(lines, selected, ctx.Height, th, s.footer(th))
}

func (s *settings) footer(th Theme) string {
	if s.editing {
		hint := "enter to accept · esc to cancel"
		if s.notice != "" {
			hint = s.notice
		}
		return th.Muted.Render("  " + s.fields[s.cursor].Hint + "  ·  " + hint)
	}
	if s.notice != "" {
		return th.Warning.Render(s.notice)
	}

	state := th.Muted.Render("saved")
	if s.dirty {
		state = th.Warning.Render("unsaved changes")
	}
	hint := ""
	if s.cursor < len(s.fields) {
		hint = "  ·  " + s.fields[s.cursor].Hint
	}
	return state + th.Muted.Render("  ·  enter edit · s save · r reload"+trunc(hint, 60))
}

// configuredBuckets lists the buckets a configuration defines, in a stable order.
//
// It unions the two places a bucket can appear, so one that has a capacity but no agents yet is
// still visible and editable rather than invisible until it is complete.
func configuredBuckets(c config.Config) []core.Route {
	seen := map[core.Route]bool{}
	var out []core.Route
	for r := range c.Routes {
		if !seen[r] {
			seen[r], out = true, append(out, r)
		}
	}
	for r := range c.Concurrency.Routes {
		if !seen[r] {
			seen[r], out = true, append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// parseChoices reads "provider/model, provider/model" and refuses a typo here rather than at run
// time with a ticket already waiting on it.
func parseChoices(v string) ([]string, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, err := config.ParseChoice(part); err != nil {
			return nil, err
		}
		out = append(out, part)
	}
	return out, nil
}
