package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/pot-roast-co/gravy/internal/agentrun"
	"github.com/pot-roast-co/gravy/internal/api"
	"github.com/pot-roast-co/gravy/internal/config"
	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/daemon"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/notify"
	"github.com/pot-roast-co/gravy/internal/provider"
	"github.com/pot-roast-co/gravy/internal/provider/adapters/claudecode"
	"github.com/pot-roast-co/gravy/internal/provider/adapters/codex"
	"github.com/pot-roast-co/gravy/internal/provider/adapters/copilot"
	"github.com/pot-roast-co/gravy/internal/router"
	"github.com/pot-roast-co/gravy/internal/runlog"
	"github.com/pot-roast-co/gravy/internal/scheduler"
	"github.com/pot-roast-co/gravy/internal/store"
	"github.com/pot-roast-co/gravy/internal/validate"
)

// hostProbeBudget caps how long startup will spend reaching remote machines.
//
// It is deliberately shorter than ssh's own ConnectTimeout: a host that has not answered by now
// is not going to, and the queue on this machine should not wait to find that out. Hosts that
// miss the budget are recorded as unreachable exactly as if ssh had refused, so work pinned to
// them waits with a reason attached rather than disappearing.
const hostProbeBudget = 8 * time.Second

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
	// notifier tells the human when a ticket needs them. Shared by the orchestrator and the
	// daemon's startup reconciliation.
	notifier *notify.Notifier
}

// newApp opens the store and wires the object graph.
// client is what a command that talks to the daemon needs, and nothing else: somewhere to find
// the socket, a local host to start a daemon with, and the connection itself.
//
// newApp builds the whole daemon-side graph — the database, every host, the scheduler pool, the
// orchestrator — and probes each remote machine's home directory and capabilities on the way.
// Every CLI command and the TUI were paying for that and then throwing it away to talk over the
// socket instead: with two ssh hosts configured, two seconds before `gravy` drew anything, most
// of it spent in login shells on other people's computers. Only `serve` and `run` need the graph.
type client struct {
	home string
	cfg  config.Config
	host *host.LocalHost
	svc  *api.Client
}

// newClient connects to the daemon, starting one if none is running.
func newClient(ctx context.Context) (*client, error) {
	home, err := config.Home()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Read(home)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return nil, err
	}

	h := host.NewLocal("local", cfg.Concurrency.Workers)
	c, err := connect(ctx, home, h, true)
	if err != nil {
		return nil, err
	}
	return &client{home: home, cfg: cfg, host: h, svc: c}, nil
}

// Close hangs up. There is no database to close: the daemon owns it.
func (c *client) Close() error { return c.svc.Close() }

func newApp(ctx context.Context) (*app, error) {
	home, err := config.Home()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Read(home)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return nil, err
	}

	db, err := store.Open(ctx, filepath.Join(home, "gravy.db"))
	if err != nil {
		return nil, err
	}

	h := host.NewLocal("local", cfg.Concurrency.Workers)
	// The local machine is always present; configured ones join it. A host that cannot be
	// reached is still registered: the scheduler explains an unreachable host as one whose
	// capabilities could not be read, which is more useful than it silently not existing.
	hosts := []host.Host{h}
	for _, hc := range cfg.Hosts {
		hosts = append(hosts, host.NewSSH(hc.ID, hc.Target, hc.Workers))
	}
	// Worktrees for a project on another machine live under that machine's home directory,
	// which is probed rather than guessed: the user is rarely called the same thing on both.
	//
	// This probe and the capability probe below both run here, concurrently and under one
	// deadline, because both are ssh round trips that a machine which is off does not refuse —
	// it simply never answers, and the probe costs its whole connect timeout. Run one after
	// another they were additive, and every second of them is spent before the socket is bound:
	// with two hosts asleep the daemon took over a minute to start listening, long past the
	// point where the client gives up and reports that Gravy would not start. A laptop being
	// shut is not a reason for the local queue to stop.
	probeCtx, cancelProbe := context.WithTimeout(ctx, hostProbeBudget)
	defer cancelProbe()

	var (
		remoteHomes = map[string]string{}
		homesMu     sync.Mutex
		pool        *scheduler.StaticPool
		poolErr     error
		probes      sync.WaitGroup
	)

	probes.Add(1)
	go func() {
		defer probes.Done()
		pool, poolErr = scheduler.NewStaticPool(probeCtx, hosts...)
	}()

	for _, rh := range hosts {
		sh, ok := rh.(*host.SSHHost)
		if !ok {
			continue
		}
		probes.Add(1)
		go func() {
			defer probes.Done()
			remote, herr := sh.Home(probeCtx)
			if herr != nil {
				return // unreachable hosts are reported below, with their probe failure
			}
			homesMu.Lock()
			defer homesMu.Unlock()
			remoteHomes[sh.ID()] = filepath.Join(remote, ".gravy")
		}()
	}
	probes.Wait()

	if poolErr != nil {
		db.Close()
		return nil, poolErr
	}

	// Each route resolves independently, so a ticket asking for "strong" can run a different
	// agent from one asking for "cheap" — and two agents can be working at the same time.
	// This is not GR-016: there is no fallback and no cooldown, so a quota failure parks the
	// ticket rather than moving to the next choice.
	// The router reads from liveConfig, so a bucket edited in the TUI takes effect on the next
	// tick rather than the next restart. It walks each bucket's choices in order and skips any
	// model that is cooling down after a quota or rate-limit failure, which is what lets a
	// bucket keep working on its second choice instead of stopping.
	live := newLiveConfig(cfg)
	rtr := router.New(db, live.choices, live.usable)
	// Route caps are the buckets: how many agents of each kind may run at once, fleet-wide.
	sched := scheduler.New(db, pool, rtr).WithRouteCaps(cfg.Concurrency.Routes)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	for id, perr := range pool.ProbeErrors() {
		log.Warn("host could not be reached; work pinned to it will wait", "host", id, "error", perr)
	}

	// The orchestrator writes through a store that announces what it did, so a run moving
	// through its states reaches a connected TUI. The publisher is the service, which does not
	// exist yet, so it is attached below.
	events := daemon.WithEvents(db, nil)

	orch := agentrun.New(
		events,
		agentrun.LocalRepos{Hosts: byID(hosts), Default: h, Home: home, Homes: remoteHomes},
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
	for _, rh := range hosts {
		orch.RegisterHost(rh)
	}
	providers := []provider.Provider{
		claudecode.New(claudecode.WithCommand(providerCommand(cfg, claudecode.ID, claudecode.DefaultCommand))),
		codex.New(codex.WithCommand(providerCommand(cfg, codex.ID, codex.DefaultCommand))),
		copilot.New(copilot.WithCommand(providerCommand(cfg, copilot.ID, copilot.DefaultCommand))),
	}
	for _, p := range providers {
		orch.RegisterProvider(p)
	}

	// The advisory review pass. It runs on the review bucket, which is deliberately a cheaper
	// model than the implementation bucket: its job is triage a human can skim, not a second
	// opinion worth paying for twice.
	orch = orch.WithReview(reviewResolver(rtr), cfg.Context.TokenBudget*4)

	// Play through desktop audio so detached daemons remain audible.
	opts := []notify.Option{notify.WithRunner(hostRunner{h: h}), notify.WithLogger(log)}
	if play, err := notify.Sound(h.FS(), hostRunner{h: h}, filepath.Join(home, "sounds"), runtime.GOOS); err == nil {
		opts = append(opts, notify.WithSound(play))
	} else {
		log.Warn("could not prepare notification sound", "error", err)
	}
	if runtime.GOOS == "linux" && h.FS().Exists("/usr/share/omarchy/shell/plugins/notifications/Service.qml") {
		if exe, err := os.Executable(); err == nil {
			opts = append(opts, notify.WithClickArgs(func(ticketID string) []string {
				argv := []string{"env", config.EnvHome + "=" + home, exe, "activate"}
				if ticketID != "" {
					argv = append(argv, ticketID)
				}
				return argv
			}))
		}
	}
	notifier := notify.New(cfg.Notifications, os.Stderr, opts...)
	orch = orch.WithNotifier(notifier)

	svc := api.NewLocal(db, sched, hosts, newID).
		WithLander(lander{orch}).WithLogs(logs).WithKiller(orch).
		WithCheckouts(orch.Checkouts()).WithRereviewer(orch).
		WithPlanner(orch.Plan(
			func(ctx context.Context, route core.Route, c core.Constraints) (core.Choice, error) {
				return rtr.Resolve(ctx, route, c)
			},
			// Planning reads the project's documents, so it gets the same budget a run's
			// context does rather than a number of its own to drift out of step.
			cfg.Context.TokenBudget,
		)).
		WithDiscusser(orch.Discuss(
			func(ctx context.Context, route core.Route, c core.Constraints) (core.Choice, error) {
				return rtr.Resolve(ctx, route, c)
			},
		)).
		WithSettings(home, cfg, applyConfig(live, sched, cfg)).
		WithAgents(agentOptions(ctx, providers)).
		WithDetector(detector{providers: providers, host: h, cfg: cfg})

	// Now that the service exists, the daemon's own writes can reach its subscribers.
	events.SetPublisher(svc)

	return &app{
		cfg: cfg, home: home, db: db, host: h,
		svc:   svc,
		logs:  logs,
		sched: sched, orch: orch, log: log, notifier: notifier,
	}, nil
}

// byID indexes hosts for the repository factory, which resolves a project to the machine its
// clone is on.
func byID(hosts []host.Host) map[string]host.Host {
	out := make(map[string]host.Host, len(hosts))
	for _, h := range hosts {
		out[h.ID()] = h
	}
	return out
}

// agentOptions asks each compiled-in provider what it can run, so Settings can refuse a route
// naming something that does not exist rather than letting a ticket discover it.
//
// A provider that cannot answer contributes its name with no model list, which is read as
// "accepts anything": being unable to enumerate is not evidence that a model is wrong.
func agentOptions(ctx context.Context, providers []provider.Provider) []api.AgentOption {
	out := make([]api.AgentOption, 0, len(providers))
	for _, p := range providers {
		opt := api.AgentOption{ProviderID: p.ID()}
		if models, err := p.Models(ctx); err == nil {
			for _, m := range models {
				opt.Models = append(opt.Models, m.ID)
			}
		}
		// An optional interface rather than a change to Provider: only an adapter whose list
		// is hand-transcribed needs to say so.
		if open, ok := p.(interface{ ModelsAreOpen() bool }); ok {
			opt.Open = open.ModelsAreOpen()
		}
		out = append(out, opt)
	}
	return out
}

// detector probes the agent CLIs on demand, for onboarding.
//
// On demand rather than at startup: a login expires, and a status cached when the daemon started
// would confidently report an agent that stopped working an hour ago.
type detector struct {
	providers []provider.Provider
	host      host.Host
	cfg       config.Config
}

func (d detector) DetectAgents(ctx context.Context) []api.AgentStatus {
	out := make([]api.AgentStatus, 0, len(d.providers))
	for _, p := range d.providers {
		st := api.AgentStatus{
			ProviderID: p.ID(),
			Command:    providerCommand(d.cfg, p.ID(), ""),
		}
		av, err := p.Detect(ctx, d.host)
		if err != nil {
			st.Detail = err.Error()
			out = append(out, st)
			continue
		}
		st.Installed, st.Authenticated, st.Detail = av.Installed, av.Authenticated, av.Detail
		out = append(out, st)
	}
	return out
}

// hostRunner runs a notification command through the host.
//
// internal/host is the only package permitted to execute anything, so the notifier is handed
// this rather than reaching for os/exec — which is also what makes an OS notification on a
// remote host a matter of supplying a different Host.
type hostRunner struct{ h host.Host }

func (r hostRunner) Run(ctx context.Context, cmd string, args ...string) error {
	p, err := r.h.Exec(ctx, host.ExecSpec{Cmd: cmd, Args: args, Timeout: 10 * time.Second})
	if err != nil {
		return err
	}
	// Output is drained so a notifier that writes to stdout cannot fill a pipe and wedge.
	go func() { _, _ = io.Copy(io.Discard, p.Stdout()) }()
	go func() { _, _ = io.Copy(io.Discard, p.Stderr()) }()

	st, err := p.Wait()
	if err != nil {
		return err
	}
	if st.Code != 0 {
		return fmt.Errorf("%s: exit %d", cmd, st.Code)
	}
	return nil
}

func (a *app) Close() error { return a.db.Close() }

// lander adapts the orchestrator to the narrow interface api needs, so api states what it
// requires rather than depending on how landing works.
type lander struct{ orch *agentrun.Orchestrator }

func (l lander) Approve(ctx context.Context, ticketID string) (core.State, error) {
	res, err := l.orch.Land().Approve(ctx, ticketID)
	return res.State, err
}

func (l lander) Continue(ctx context.Context, ticketID string) (core.State, error) {
	res, err := l.orch.Land().Continue(ctx, ticketID)
	return res.State, err
}

// loop builds the scheduler loop.
func (a *app) loop() *daemon.Loop { return daemon.NewLoop(a.sched, a.orch, a.log) }

// registeredProviders is what this build can actually run. Adding an adapter means adding a
// package and an entry here, and nothing else.
var registeredProviders = map[string]string{
	claudecode.ID: claudecode.DefaultCommand,
	codex.ID:      codex.DefaultCommand,
	copilot.ID:    copilot.DefaultCommand,
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

// reviewResolver resolves the review bucket each time the advisory pass runs.
//
// Resolving per review rather than at startup is what lets a project pin its own reviewer, and
// it means an edit in Settings takes effect on the next review rather than the next restart.
//
// The fall back to the implementation bucket is what resolving at startup used to do: a review
// bucket nobody configured should still produce a review, since an advisory opinion on the
// working model beats no opinion at all. An explicit project review bucket must
// never fall back outside its configured choices, even while those choices are unavailable.
func reviewResolver(rtr *router.Router) agentrun.RouteResolver {
	return func(ctx context.Context, route core.Route, c core.Constraints) (core.Choice, error) {
		choice, err := rtr.Resolve(ctx, route, c)
		if err == nil || route == core.RouteImplementation || len(c.Routes[route]) > 0 {
			return choice, err
		}
		if fallback, ferr := rtr.Resolve(ctx, core.RouteImplementation, c); ferr == nil {
			fallback.Why = append(fallback.Why,
				fmt.Sprintf("bucket %q resolved to nothing (%v), so the review ran on the implementation bucket", route, err))
			return fallback, nil
		}
		return core.Choice{}, err
	}
}
