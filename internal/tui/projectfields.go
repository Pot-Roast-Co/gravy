package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
)

// projectField is one editable setting on a project.
//
// These used to live in the Settings screen, six rows per project in one flat list — which is
// tolerable at one project and unreadable at ten, and split the work of registering a project
// across two screens. Settings now holds what is true of the whole fleet; a project's own
// configuration lives with the project.
type projectField struct {
	Label string
	Hint  string
	Get   func(core.Project) string
	Set   func(*core.Project, string) error
}

// projectFields lists a project's settings, in the order the screen shows them.
//
// agents and hosts are what this build can actually run, so a typo is refused here rather than
// discovered by a ticket that waits on a machine or a model that does not exist.
func projectFields(agents []api.AgentOption, hosts []string, projects ...core.Project) []projectField {
	fields := []projectField{
		{
			Label: "host",
			Hint:  "the machine this project's clone is on",
			Get:   func(p core.Project) string { return p.HostID },
			Set: func(p *core.Project, v string) error {
				v = strings.TrimSpace(v)
				if v == "" {
					return fmt.Errorf("a project runs on a machine; name one of: %s",
						strings.Join(hosts, ", "))
				}
				if !contains(hosts, v) {
					return fmt.Errorf("no host called %q — configured: %s", v, strings.Join(hosts, ", "))
				}
				// Changing this does not move the repository: the path has to exist there too.
				p.HostID = v
				return nil
			},
		},
		{
			// "buckets", matching Settings. It was "agents", which collides with the Agents
			// section there — that one is which CLIs exist and what command runs them, and
			// the same word meaning two things one screen apart is a question waiting to be
			// asked.
			Label: "buckets",
			Hint:  `overrides the global buckets for this project, as "bucket=provider/model ..."`,
			Get:   func(p core.Project) string { return formatProjectRoutes(p.Routes) },
			Set: func(p *core.Project, v string) error {
				routes, err := parseProjectRoutes(v, agents)
				if err != nil {
					return err
				}
				p.Routes = routes
				return nil
			},
		},
		{
			Label: "allowed commands",
			Hint:  "what an agent may run without asking, comma separated — empty refuses everything",
			Get: func(p core.Project) string {
				out := make([]string, 0, len(p.Allowlist.Commands))
				for _, c := range p.Allowlist.Commands {
					out = append(out, c.Match)
				}
				return strings.Join(out, ", ")
			},
			Set: func(p *core.Project, v string) error {
				cmds, err := parseAllowedCommands(v)
				if err != nil {
					return err
				}
				p.Allowlist.Commands = cmds
				return nil
			},
		},
		{
			Label: "validation",
			Hint:  "name:command, separated by ; — what must pass before review",
			Get: func(p core.Project) string {
				parts := make([]string, 0, len(p.Validation))
				for _, st := range p.Validation {
					parts = append(parts, st.Name+":"+st.Cmd)
				}
				return strings.Join(parts, "; ")
			},
			Set: func(p *core.Project, v string) error {
				steps, err := parseSteps(v)
				if err != nil {
					return err
				}
				p.Validation = steps
				return nil
			},
		},
		{
			Label: "preview command",
			Hint:  "starts the app from a ticket worktree in Review (e.g. npm run dev)",
			Get:   func(p core.Project) string { return p.PreviewCommand },
			Set:   func(p *core.Project, v string) error { p.PreviewCommand = strings.TrimSpace(v); return nil },
		},
		{
			Label: "target branch",
			Hint:  "the branch approved work merges into",
			Get:   func(p core.Project) string { return p.TargetBranch },
			Set: func(p *core.Project, v string) error {
				if strings.TrimSpace(v) == "" {
					return fmt.Errorf("a project needs a target branch")
				}
				p.TargetBranch = strings.TrimSpace(v)
				return nil
			},
		},
		{
			Label: "merge mode",
			Hint:  "merge | pr",
			Get:   func(p core.Project) string { return string(p.MergeMode) },
			Set: func(p *core.Project, v string) error {
				m := core.LandMode(strings.TrimSpace(v))
				if !m.Valid() {
					return fmt.Errorf("merge mode is merge or pr")
				}
				p.MergeMode = m
				return nil
			},
		},
		{
			Label: "parallel",
			Hint:  "true runs several tickets at once and you handle the conflicts",
			Get:   func(p core.Project) string { return strconv.FormatBool(p.ParallelMode) },
			Set: func(p *core.Project, v string) error {
				b, err := strconv.ParseBool(strings.TrimSpace(v))
				if err != nil {
					return fmt.Errorf("parallel must be true or false")
				}
				p.ParallelMode = b
				if b && p.MaxConcurrency < 1 {
					p.MaxConcurrency = 2
				}
				return nil
			},
		},
		{
			Label: "max concurrency",
			Hint:  "tickets in flight at once when parallel",
			Get:   func(p core.Project) string { return strconv.Itoa(p.MaxConcurrency) },
			Set: func(p *core.Project, v string) error {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil || n < 1 {
					return fmt.Errorf("concurrency must be at least 1")
				}
				p.MaxConcurrency = n
				return nil
			},
		},
	}
	var project core.Project
	if len(projects) > 0 {
		project = projects[0]
	}
	return append(fields, previewFields(project)...)
}

// parseAllowedCommands reads "mix, cd, go" into allowlist patterns.
//
// Program names, not shell lines: each becomes `Bash(name)` and `Bash(name *)` for the agent
// CLI, so "mix" permits every mix subcommand and "mix test" would permit only that exact one.
func parseAllowedCommands(v string) ([]core.Pattern, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	var out []core.Pattern
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.ContainsAny(part, "|&;><$`") {
			// A shell operator here would be permitted verbatim and match nothing useful,
			// while reading like it granted a pipeline.
			return nil, fmt.Errorf("%q is not a command name", part)
		}
		out = append(out, core.Pattern{Match: part})
	}
	return out, nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
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

var _ = config.Host{} // config is used by the settings screen's host section

// completeField extends a partly-typed field value.
//
// The buckets field is the one that needs it most: it asks for a bucket name, a provider and a
// model, all of which the daemon already knows and none of which a human should be reciting from
// memory — the syntax was learned by getting it wrong.
func completeField(label, input string, agents []api.AgentOption, hosts []string, buckets []core.Route) (string, []string) {
	switch label {
	case "host":
		return completeToken(input, "", hosts)
	case "merge mode":
		return completeToken(input, "", []string{"merge", "pr"})
	case "parallel":
		return completeToken(input, "", []string{"true", "false"})
	case "buckets":
		return completeBuckets(input, agents, buckets)
	}
	return input, nil
}

// completeBuckets completes whichever part of "bucket=provider/model" is being typed.
func completeBuckets(input string, agents []api.AgentOption, buckets []core.Route) (string, []string) {
	// Only the last entry is being edited; everything before the final comma is settled.
	head, tail := splitLast(input, ",")
	tail = strings.TrimLeft(tail, " ")

	name, choices, isChoice := strings.Cut(tail, "=")
	if !isChoice {
		// A bucket name, which gets its "=" for free: the separator is not the interesting
		// part and forgetting it is the commonest way to get this wrong.
		names := make([]string, 0, len(buckets))
		for _, b := range buckets {
			names = append(names, string(b)+"=")
		}
		completed, matches := completeToken(name, "", names)
		return rejoin(head, completed, matches)
	}

	// Within the choice list, only the last provider/model is being typed.
	before, last := splitLast(choices, " ")
	var options []string
	for _, a := range agents {
		if len(a.Models) == 0 {
			options = append(options, a.ProviderID+"/")
			continue
		}
		for _, m := range a.Models {
			options = append(options, a.ProviderID+"/"+m)
		}
	}
	completed, matches := completeToken(last, "", options)

	// before already carries its trailing space: splitLast keeps the separator, and adding
	// another produced "codex/gpt-5.6-sol  claude-code/..." on every fallback.
	prefix := name + "=" + before
	return rejoin(head, prefix+completed, matches)
}

// completeToken extends one token to the longest common prefix of what it matches.
func completeToken(token, _ string, options []string) (string, []string) {
	var matches []string
	for _, o := range options {
		if strings.HasPrefix(strings.ToLower(o), strings.ToLower(token)) {
			matches = append(matches, o)
		}
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return token, nil
	}
	return longestCommonPrefix(matches), matches
}

// splitLast splits on the final separator, returning everything before it and the remainder.
func splitLast(s, sep string) (before, last string) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i+len(sep)], s[i+len(sep):]
	}
	return "", s
}

// rejoin puts a completed tail back after the settled part of the line.
func rejoin(head, completed string, matches []string) (string, []string) {
	if head == "" {
		return completed, matches
	}
	return head + " " + completed, matches
}
