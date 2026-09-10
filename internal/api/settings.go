package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
)

// Settings is the editable configuration, with what changing it actually costs.
type Settings struct {
	Config config.Config
	// Path is where the file lives, so the screen can say what it is editing.
	Path string
	// PendingRestart names settings that have been saved but that the running daemon is not
	// using yet. Some values are baked into the object graph at startup — the size of the
	// worker pool, which agent CLIs are registered — and pretending otherwise would leave a
	// user believing a change took effect when it did not.
	PendingRestart []string
	// Agents is what this build can actually run: each compiled-in provider and the models it
	// offers. A route naming anything else is knowably wrong before a ticket waits on it —
	// which is how "codex/sol" reached a run and came back as a 400 from the server.
	Agents []AgentOption
}

// AgentOption is one provider and the models it offers.
type AgentOption struct {
	ProviderID string
	// Models are the ids a route may name. Empty means the provider does not enumerate them,
	// and any model is accepted.
	Models []string
	// Open reports that Models is a suggestion rather than a whitelist. A provider whose names
	// are transcribed by hand says so, because refusing an unlisted one would break the day its
	// vendor ships a model.
	Open bool
}

// WithAgents records what this build can run, for validating routes.
func (l *Local) WithAgents(a []AgentOption) *Local {
	l.agents = a
	return l
}

// ApplyFunc applies a saved config to the running daemon and reports which settings could not
// be applied live and therefore need a restart.
type ApplyFunc func(config.Config) []string

// WithSettings lets the service read and write configuration.
//
// apply is what makes a change take effect without a restart; it returns the settings it could
// not apply. A service without this can still run the queue, it simply cannot be configured
// through the API.
func (l *Local) WithSettings(home string, loaded config.Config, apply ApplyFunc) *Local {
	l.home = home
	l.cfg = loaded
	l.applyCfg = apply
	return l
}

// GetSettings returns the current configuration.
func (l *Local) GetSettings(_ context.Context) (Settings, error) {
	l.cfgMu.RLock()
	defer l.cfgMu.RUnlock()
	if l.home == "" {
		return Settings{}, fmt.Errorf("this client cannot read configuration")
	}
	return Settings{
		Config: copyConfig(l.cfg), Path: config.Path(l.home),
		PendingRestart: l.pendingRestart, Agents: l.agents,
	}, nil
}

// UpdateSettings validates, saves and applies a configuration.
//
// It is written to disk only after it validates, so a rejected edit cannot leave the file in a
// state the daemon would refuse to start from next time.
func (l *Local) UpdateSettings(_ context.Context, c config.Config) (Settings, error) {
	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	return l.updateSettings(c)
}

func (l *Local) updateSettings(c config.Config) (Settings, error) {
	if l.home == "" {
		return Settings{}, fmt.Errorf("this client cannot change configuration")
	}
	if err := c.Validate(); err != nil {
		return Settings{}, fmt.Errorf("that configuration is not usable: %w", err)
	}
	if err := config.Save(l.home, c); err != nil {
		return Settings{}, err
	}

	l.cfg = copyConfig(c)
	if l.applyCfg != nil {
		l.pendingRestart = l.applyCfg(c)
	}
	l.events.publish(Event{Kind: EventProjectChanged})
	return Settings{
		Config: copyConfig(l.cfg), Path: config.Path(l.home),
		PendingRestart: l.pendingRestart, Agents: l.agents,
	}, nil
}

// UpdateProject saves a project's editable fields.
//
// RepoPath and Slug are not among them: a project's identity and its checkout are what every
// worktree, branch and run already recorded points at, so changing either would orphan work
// rather than edit it. Remove and re-add instead.
func (l *Local) UpdateProject(ctx context.Context, p core.Project) error {
	current, err := l.db.GetProject(ctx, p.ID)
	if err != nil {
		return err
	}

	if strings.TrimSpace(p.TargetBranch) == "" {
		return fmt.Errorf("a project needs a target branch")
	}
	if p.MergeMode != "" && !p.MergeMode.Valid() {
		return fmt.Errorf("merge mode %q is not merge or pr", p.MergeMode)
	}
	if p.ParallelMode && p.MaxConcurrency < 1 {
		return fmt.Errorf("parallel mode needs a concurrency of at least 1")
	}

	// Changing the machine is allowed, but the clone has to be on the new one: nothing here
	// moves a repository, and a project pointing at a path that does not exist on its host
	// fails every ticket at the first git command instead of at the edit that caused it.
	if p.HostID != current.HostID {
		h, err := l.hostFor(p.HostID)
		if err != nil {
			return err
		}
		if path := strings.TrimSpace(current.RepoPath); path != "" {
			if !h.FS().Exists(path) {
				return fmt.Errorf("%s: no such directory on host %s", path, hostName(p.HostID))
			}
			if err := checkGitRepo(ctx, h, path); err != nil {
				return err
			}
		}
		current.HostID = p.HostID
	}

	current.Name = p.Name
	current.TargetBranch = p.TargetBranch
	if p.MergeMode != "" {
		current.MergeMode = p.MergeMode
	}
	current.Validation = p.Validation
	// Buckets, allowlist and notes are edited on the same screen as everything above. Copying
	// only some of the fields a client sends is a save that reports success and changes
	// nothing — which is how a project's bucket override could be typed in, redrawn from the
	// edited copy in memory, and be gone on the next load.
	current.Routes = p.Routes
	current.Allowlist = p.Allowlist
	current.Notes = p.Notes
	current.ParallelMode = p.ParallelMode
	current.MaxConcurrency = p.MaxConcurrency
	if !p.ParallelMode {
		// Serial mode is one at a time by definition; leaving a stale cap behind would make
		// the stored project describe a mode it is not in.
		current.MaxConcurrency = 1
	}

	if err := l.db.UpdateProject(ctx, current); err != nil {
		return err
	}
	l.events.publish(Event{Kind: EventProjectChanged, ProjectID: current.ID})
	return nil
}

// knownRoute reports whether a ticket may ask for a route.
//
// A route is valid because the configuration defines it, not because it appears in a list
// compiled into Gravy — buckets are named by whoever is using them. A service with no
// configuration attached accepts any usable name, since it has nothing to check against and
// refusing would be worse than accepting.
func (l *Local) knownRoute(r core.Route) error {
	l.cfgMu.RLock()
	defer l.cfgMu.RUnlock()
	if !r.Named() {
		return fmt.Errorf("route %q is not a usable bucket name", r)
	}
	if l.home == "" || len(l.cfg.Routes) == 0 {
		return nil
	}
	if _, ok := l.cfg.Routes[r]; ok {
		return nil
	}

	names := make([]string, 0, len(l.cfg.Routes))
	for name := range l.cfg.Routes {
		names = append(names, string(name))
	}
	sort.Strings(names)
	return fmt.Errorf("there is no bucket called %q — configured buckets: %s",
		r, strings.Join(names, ", "))
}

// Routes lists the configured bucket names, so a client can offer them rather than guess.
func (l *Local) Routes() []core.Route {
	l.cfgMu.RLock()
	defer l.cfgMu.RUnlock()
	out := make([]core.Route, 0, len(l.cfg.Routes))
	for name := range l.cfg.Routes {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// copyConfig prevents in-process clients from mutating the live configuration through maps.
func copyConfig(c config.Config) config.Config {
	b, _ := json.Marshal(c)
	var out config.Config
	_ = json.Unmarshal(b, &out)
	return out
}
