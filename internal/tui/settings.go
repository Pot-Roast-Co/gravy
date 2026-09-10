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
	loaded api.Settings
	cfg    config.Config
	// agents is what this build can run, used to refuse a route naming something it cannot.
	agents   []api.AgentOption
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
		s.agents = msg.settings.Agents
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
				runes := []rune(s.buf)
				s.buf = string(runes[:len(runes)-1])
			}
		case len(msg.Runes) > 0:
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
					choices, err := parseChoices(v, s.agents)
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
	// Hosts. Editing them needs a restart, which the screen already reports through
	// PendingRestart: the object graph binds hosts at startup, and pretending a new machine
	// joined a running daemon would be a lie the user only discovers when work does not run.
	for i := range s.cfg.Hosts {
		hIdx := i
		out = append(out,
			settingField{
				Section: "Host " + s.cfg.Hosts[hIdx].ID, Label: "ssh",
				Hint: "ssh destination, normally a Host alias from ~/.ssh/config",
				Get:  func(s *settings) string { return s.cfg.Hosts[hIdx].Target },
				Set: func(s *settings, v string) error {
					if strings.TrimSpace(v) == "" {
						return fmt.Errorf("a host needs an ssh destination")
					}
					s.cfg.Hosts[hIdx].Target = strings.TrimSpace(v)
					s.dirty = true
					return nil
				},
			},
			settingField{
				Section: "Host " + s.cfg.Hosts[hIdx].ID, Label: "workers",
				Hint: "agents that may run on this machine at once",
				Get:  func(s *settings) string { return strconv.Itoa(s.cfg.Hosts[hIdx].Workers) },
				Set: func(s *settings, v string) error {
					n, err := strconv.Atoi(v)
					if err != nil || n < 1 {
						return fmt.Errorf("workers must be at least 1")
					}
					s.cfg.Hosts[hIdx].Workers = n
					s.dirty = true
					return nil
				},
			},
			settingField{
				Section: "Host " + s.cfg.Hosts[hIdx].ID, Label: "remove",
				Hint: "type the host id to stop using this machine",
				Get:  func(s *settings) string { return "" },
				Set: func(s *settings, v string) error {
					if strings.TrimSpace(v) != s.cfg.Hosts[hIdx].ID {
						return fmt.Errorf("type %q to remove it", s.cfg.Hosts[hIdx].ID)
					}
					// A project left pointing at a removed host would have nowhere to run,
					// and would say so only when a ticket was already waiting.
					for _, p := range s.projects {
						if p.HostID == s.cfg.Hosts[hIdx].ID {
							return fmt.Errorf("project %s is on this host; move it first", p.Slug)
						}
					}
					s.cfg.Hosts = append(s.cfg.Hosts[:hIdx], s.cfg.Hosts[hIdx+1:]...)
					s.dirty = true
					return nil
				},
			},
		)
	}

	out = append(out, settingField{
		Section: "Hosts", Label: "new host",
		Hint: `an id and an ssh destination: "air" or "air=air.local"`,
		Get:  func(s *settings) string { return "" },
		Set: func(s *settings, v string) error {
			v = strings.TrimSpace(v)
			if v == "" {
				return nil
			}
			id, target, ok := strings.Cut(v, "=")
			id = strings.TrimSpace(id)
			if !ok || strings.TrimSpace(target) == "" {
				target = id
			}
			if id == "" || strings.ContainsAny(id, " \t/,:") {
				return fmt.Errorf("use a word without spaces, slashes, commas or colons")
			}
			if knownHost(s, id) {
				return fmt.Errorf("there is already a host called %q", id)
			}
			s.cfg.Hosts = append(s.cfg.Hosts, config.Host{
				ID: id, Target: strings.TrimSpace(target), Workers: 2,
			})
			s.dirty = true
			return nil
		},
	})

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
		th.Header.Render("Settings · W setup wizard"),
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

// knownHost reports whether a host id is one the daemon knows about.
func knownHost(s *settings, id string) bool {
	for _, n := range hostNames(s) {
		if n == id {
			return true
		}
	}
	return false
}

// hostNames lists the configured hosts, for an error that says what would work.
// The machine running the daemon is always available and is never listed in the file, so it is
// added here rather than being a name that mysteriously fails validation.
func hostNames(s *settings) []string {
	out := []string{localHostID}
	for _, h := range s.cfg.Hosts {
		out = append(out, h.ID)
	}
	return out
}

// localHostID is the id the daemon registers its own machine under.
const localHostID = "local"

// formatProjectRoutes renders a project's route overrides as one editable line.
func formatProjectRoutes(routes map[core.Route][]core.Choice) string {
	if len(routes) == 0 {
		return ""
	}
	names := make([]string, 0, len(routes))
	for r := range routes {
		names = append(names, string(r))
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, n := range names {
		choices := routes[core.Route(n)]
		rendered := make([]string, 0, len(choices))
		for _, c := range choices {
			rendered = append(rendered, config.FormatChoice(c))
		}
		parts = append(parts, n+"="+strings.Join(rendered, " "))
	}
	return strings.Join(parts, ", ")
}

// parseProjectRoutes reads "route=provider/model provider/model, route=..." back.
//
// A project's overrides are per route, so the shape has to carry both — and it is validated the
// same way the global buckets are, because a typo here fails at run time with a ticket already
// waiting on it.
func parseProjectRoutes(v string, agents []api.AgentOption) (map[core.Route][]core.Choice, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	out := map[core.Route][]core.Choice{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, list, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("%q is not \"route=provider/model\"", part)
		}
		route := core.Route(strings.TrimSpace(name))
		if !route.Named() {
			return nil, fmt.Errorf("%q is not a bucket name", name)
		}
		var choices []core.Choice
		for _, f := range strings.Fields(list) {
			c, err := config.ParseChoice(f)
			if err != nil {
				return nil, err
			}
			if err := knownAgent(c, agents); err != nil {
				return nil, err
			}
			choices = append(choices, c)
		}
		if len(choices) == 0 {
			return nil, fmt.Errorf("bucket %q has no agents", route)
		}
		out[route] = choices
	}
	return out, nil
}

// parseChoices reads "provider/model, provider/model" and refuses a typo here rather than at run
// time with a ticket already waiting on it.
//
// It checks the names against what this build can actually run, not just the shape. A route of
// "codex/sol" parses perfectly and is still wrong: no such model exists, and the only way anyone
// found out was a ticket reaching review with a 400 from the provider attached to it.
func parseChoices(v string, agents []api.AgentOption) ([]string, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		choice, err := config.ParseChoice(part)
		if err != nil {
			return nil, err
		}
		if err := knownAgent(choice, agents); err != nil {
			return nil, err
		}
		out = append(out, part)
	}
	return out, nil
}

// knownAgent reports whether this build can run a choice.
//
// Silent when the service did not say what it has: a client that cannot enumerate agents should
// not refuse a configuration it has no basis to judge.
func knownAgent(c core.Choice, agents []api.AgentOption) error {
	if len(agents) == 0 {
		return nil
	}
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.ProviderID)
		if a.ProviderID != c.ProviderID {
			continue
		}
		// A provider that does not enumerate its models accepts any of them, and one whose
		// list is only a suggestion is not evidence that an unlisted name is wrong.
		if len(a.Models) == 0 || a.Open {
			return nil
		}
		for _, m := range a.Models {
			if m == c.Model {
				return nil
			}
		}
		return fmt.Errorf("%s has no model %q — it offers: %s",
			c.ProviderID, c.Model, strings.Join(a.Models, ", "))
	}
	return fmt.Errorf("no agent called %q in this build — it has: %s",
		c.ProviderID, strings.Join(names, ", "))
}
