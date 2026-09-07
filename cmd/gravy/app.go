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

	"github.com/bobbybrady/gravy/internal/agentrun"
	"github.com/bobbybrady/gravy/internal/api"
	"github.com/bobbybrady/gravy/internal/config"
	"github.com/bobbybrady/gravy/internal/core"
	"github.com/bobbybrady/gravy/internal/daemon"
	"github.com/bobbybrady/gravy/internal/host"
	"github.com/bobbybrady/gravy/internal/provider/adapters/claudecode"
	"github.com/bobbybrady/gravy/internal/runlog"
	"github.com/bobbybrady/gravy/internal/scheduler"
	"github.com/bobbybrady/gravy/internal/store"
	"github.com/bobbybrady/gravy/internal/validate"
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

	// M0 runs a single hardcoded route. GR-016 replaces this with the real router behind the
	// same interface.
	route := scheduler.FixedRoute{ProviderID: claudecode.ID, Model: firstModel(cfg)}
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
	orch.RegisterProvider(claudecode.New(claudecode.WithCommand(providerCommand(cfg))))

	// The advisory review pass. It runs on the review route, which is deliberately a cheaper
	// model than the implementation route: its job is triage a human can skim, not a second
	// opinion worth paying for twice.
	orch = orch.WithReview(claudecode.ID, routeModel(cfg, core.RouteReview), cfg.Context.TokenBudget*4)

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

// routeModel returns the model a route resolves to for the claude-code provider.
func routeModel(cfg config.Config, route core.Route) string {
	choices, err := cfg.RouteChoices(route)
	if err != nil || len(choices) == 0 {
		return "sonnet"
	}
	for _, c := range choices {
		if c.ProviderID == claudecode.ID {
			return c.Model
		}
	}
	return choices[0].Model
}

// firstModel returns the model the implementation route resolves to.
func firstModel(cfg config.Config) string {
	choices, err := cfg.RouteChoices(core.RouteImplementation)
	if err != nil || len(choices) == 0 {
		return "sonnet"
	}
	for _, c := range choices {
		if c.ProviderID == claudecode.ID {
			return c.Model
		}
	}
	return choices[0].Model
}

func providerCommand(cfg config.Config) string {
	if p, ok := cfg.Providers[claudecode.ID]; ok && p.Command != "" {
		return p.Command
	}
	return claudecode.DefaultCommand
}

// newID returns a short random identifier.
func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("id-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
