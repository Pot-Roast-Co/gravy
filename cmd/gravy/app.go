package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/pot-roast-co/gravy/internal/agentrun"
	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/daemon"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider/adapters/claudecode"
	"github.com/pot-roast-co/gravy/internal/provider/adapters/codex"
	"github.com/pot-roast-co/gravy/internal/runlog"
	"github.com/pot-roast-co/gravy/internal/scheduler"
	"github.com/pot-roast-co/gravy/internal/store"
	"github.com/pot-roast-co/gravy/internal/validate"
)

// app is the wiring every command shares.
//
// The CLI and, later, the daemon build the same object graph, so a behaviour that works from one
// works from the other. That is the point of routing everything through api.Service rather than
// letting commands reach into the store.
type app struct {
	cfg   config.Config
	home  string
	db    *store.DB
	host  *host.LocalHost
	svc   *api.Local
	sched *scheduler.Scheduler
	orch  *agentrun.Orchestrator
	logs  *runlog.Store
	log   *slog.Logger
}

// newApp opens the store and wires the object graph.
func newApp(ctx context.Context) (*app, error) {
	home, err := config.Home()
	if err != nil {
		return nil, err
	}
	cfg, created, err := config.Load(home)
	if err != nil {
		return nil, err
	}
	if created {
		fmt.Fprintf(os.Stderr, "wrote a default config to %s\n", config.Path(home))
	}

	db, err := store.Open(ctx, filepath.Join(home, "gravy.db"))
	if err != nil {
		return nil, err
	}

	h := host.NewLocal("local", cfg.Concurrency.Workers)

	pool, err := scheduler.NewStaticPool(ctx, h)
	if err != nil {
		db.Close()
		return nil, err
	}

	// Each route resolves independently, so a ticket asking for "strong" can run a different
	// agent from one asking for "cheap" — and two agents can be working at the same time.
	// This is not GR-016: there is no fallback and no cooldown, so a quota failure parks the
	// ticket rather than moving to the next choice.
	route := configRouter{cfg: cfg}
	sched := scheduler.New(db, pool, route)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	orch := agentrun.New(
		db,
		agentrun.LocalRepos{Host: h, Home: home},
		h,
		func(runID string) validate.Runner {
			return validate.NewRunner(filepath.Join(home, "runs", runID, "validation"))
		},
		agentrun.SimplePrompt{},
		agentrun.Config{
			SelfCorrectionBudget: cfg.Retry.SelfCorrectionBudget,
			RunTimeout:           cfg.Timeouts.Run.D(),
			MaxTurns:             60,
			RunsDir:              filepath.Join(home, "runs"),
			CooldownQuota:        cfg.Retry.CooldownQuota.D(),
			CooldownRateLimit:    cfg.Retry.CooldownRateLimit.D(),
			CooldownUnavailable:  cfg.Retry.CooldownUnavailable.D(),
		},
		newID,
	)
	logs := runlog.New(filepath.Join(home, "runs"))
	orch = orch.WithLogs(logs)
	orch.RegisterHost(h)
	orch.RegisterProvider(claudecode.New(claudecode.WithCommand(providerCommand(cfg, claudecode.ID, claudecode.DefaultCommand))))
	orch.RegisterProvider(codex.New(codex.WithCommand(providerCommand(cfg, codex.ID, codex.DefaultCommand))))

	// The advisory review pass. It runs on the review route, which is deliberately a cheaper
	// model than the implementation route: its job is triage a human can skim, not a second
	// opinion worth paying for twice.
	reviewRoute := reviewChoice(cfg)
	orch = orch.WithReview(reviewRoute.ProviderID, reviewRoute.Model, cfg.Context.TokenBudget*4)

	return &app{
		cfg: cfg, home: home, db: db, host: h,
		svc:   api.NewLocal(db, sched, []host.Host{h}, newID).WithLander(lander{orch}).WithLogs(logs).WithKiller(orch),
		logs:  logs,
		sched: sched, orch: orch, log: log,
	}, nil
}

func (a *app) Close() error { return a.db.Close() }

// lander adapts the orchestrator to the narrow interface api needs, so api states what it
// requires rather than depending on how landing works.
type lander struct{ orch *agentrun.Orchestrator }

func (l lander) Approve(ctx context.Context, ticketID string) error {
	_, err := l.orch.Land().Approve(ctx, ticketID)
	return err
}

func (l lander) Continue(ctx context.Context, ticketID string) error {
	_, err := l.orch.Land().Continue(ctx, ticketID)
	return err
}

// loop builds the scheduler loop.
func (a *app) loop() *daemon.Loop { return daemon.NewLoop(a.sched, a.orch, a.log) }

// configRouter resolves each route to its first usable choice from the config.
//
// It is the interim router: real fallback, cooldowns and quota handling are GR-016. What it adds
// over a single hardcoded choice is that routes resolve independently, so different tickets can
// run different agents — and therefore run different agents concurrently.
type configRouter struct{ cfg config.Config }

// Resolve picks the first choice for a route whose provider this build can actually run.
//
// A route naming a provider with no adapter is skipped rather than becoming the queue's default,
// because the config file can only select among what is compiled in.
func (r configRouter) Resolve(_ context.Context, route core.Route, _ string) (core.Choice, error) {
	choices, err := r.cfg.RouteChoices(route)
	if err == nil {
		for _, c := range choices {
			if _, ok := registeredProviders[c.ProviderID]; ok && enabled(r.cfg, c.ProviderID) {
				return c, nil
			}
		}
	}

	// An empty or unusable route falls back to the implementation route, then to claude-code:
	// a ticket must still run, and refusing to schedule it would be a worse answer than
	// running it on the default agent.
	if route != core.RouteImplementation {
		if c, ferr := r.Resolve(context.Background(), core.RouteImplementation, ""); ferr == nil {
			return c, nil
		}
	}
	return core.Choice{ProviderID: claudecode.ID, Model: "sonnet"}, nil
}

// registeredProviders is what this build can actually run. Adding an adapter means adding a
// package and an entry here, and nothing else.
var registeredProviders = map[string]string{
	claudecode.ID: claudecode.DefaultCommand,
	codex.ID:      codex.DefaultCommand,
}

func enabled(cfg config.Config, providerID string) bool {
	p, ok := cfg.Providers[providerID]
	return !ok || p.Enabled
}

// providerCommand returns the executable configured for a provider, or its default.
func providerCommand(cfg config.Config, providerID, fallback string) string {
	if p, ok := cfg.Providers[providerID]; ok && p.Command != "" {
		return p.Command
	}
	return fallback
}

// newID returns a short random identifier.
func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("id-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// reviewChoice resolves the review route for the advisory review pass.
func reviewChoice(cfg config.Config) core.Choice {
	c, _ := configRouter{cfg: cfg}.Resolve(context.Background(), core.RouteReview, "")
	return c
}
