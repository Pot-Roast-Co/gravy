package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pot-roast-co/gravy/internal/git"
	"github.com/pot-roast-co/gravy/internal/host"
	"github.com/pot-roast-co/gravy/internal/provider"
	"github.com/pot-roast-co/gravy/internal/provider/adapters/claudecode"
)

// runTry drives one ticket end to end: fetch, worktree, agent, commit, diff.
//
// It is a smoke test with a human at the keyboard, not the product. The scheduler, daemon,
// validation loop, review gate and TUI are all still to come; what this proves is that the
// pieces built so far actually work against a real repository and a real agent.
func runTry(args []string) error {
	fs := flag.NewFlagSet("gravy try", flag.ContinueOnError)
	repoPath := fs.String("repo", "", "existing git repository to work in (default: a throwaway scratch repo)")
	model := fs.String("model", "haiku", "model to run")
	timeout := fs.Duration("timeout", 5*time.Minute, "wall-clock cap on the run")
	maxTurns := fs.Int("max-turns", 30, "turn cap")
	keep := fs.Bool("keep", false, "keep the worktree instead of removing it")
	ticketID := fs.String("ticket", "GR-DEMO", "ticket id, used in the branch name")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gravy try [flags] \"<what the agent should do>\"")
		fmt.Fprintln(os.Stderr, "\nRuns one ticket end to end in an isolated worktree.")
		fmt.Fprintln(os.Stderr, "\nflags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		fs.Usage()
		return fmt.Errorf("no prompt given")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+2*time.Minute)
	defer cancel()

	h := host.NewLocal("local", 1)

	// 1. A repository to work in.
	repo := *repoPath
	scratch := repo == ""
	if scratch {
		var err error
		repo, err = makeScratchRepo(ctx, h)
		if err != nil {
			return err
		}
		defer os.RemoveAll(filepath.Dir(repo))
	}
	step("repository", repo)
	if scratch {
		note("throwaway scratch repo; pass --repo to use your own")
	}

	// 2. Is the provider actually usable?
	p := claudecode.New()
	av, err := p.Detect(ctx, h)
	if err != nil {
		return fmt.Errorf("detect: %w", err)
	}
	if !av.Installed {
		return fmt.Errorf("claude CLI not available: %s", av.Detail)
	}
	if !av.Authenticated {
		return fmt.Errorf("claude CLI not authenticated: %s", av.Detail)
	}
	step("provider", fmt.Sprintf("claude %s (%s), model %s", av.Version, av.Detail, *model))

	// 3. Fetch, then cut the worktree from freshly-fetched target state. Per ticket, at claim
	//    time — that ordering is what makes a queued ticket build on whatever merged before it.
	gravyHome, err := os.MkdirTemp("", "gravy-home-*")
	if err != nil {
		return fmt.Errorf("create gravy home: %w", err)
	}
	if !*keep {
		defer os.RemoveAll(gravyHome)
	}
	worktreeRoot := filepath.Join(gravyHome, "projects", "demo", "worktrees")
	r := git.NewLocalRepo(h, repo, worktreeRoot)

	if err := r.Fetch(ctx); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	step("fetch", "done")

	branch := git.BranchName(*ticketID, firstWords(prompt, 5))
	wt, err := r.CreateWorktree(ctx, branch, "HEAD")
	if err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}
	step("worktree", wt.Path)
	note("branch " + wt.Branch + ", based on " + wt.Base)

	// 4. Run the agent, streaming what it does.
	runDir := filepath.Join(gravyHome, "runs", *ticketID)
	step("run", "starting agent — this is live, not a replay")
	fmt.Println()

	started := time.Now()
	hd, err := p.Run(ctx, h, provider.AgentTask{
		RunID:        *ticketID,
		WorktreePath: wt.Path,
		Prompt:       prompt,
		Model:        *model,
		Timeout:      *timeout,
		MaxTurns:     *maxTurns,
		LogPath:      filepath.Join(runDir, "agent.log"),
		AskPath:      filepath.Join(runDir, "ask.json"),
	})
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	for e := range hd.Events() {
		printEvent(started, wt.Path, e)
	}

	out, err := hd.Wait()
	if err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	fmt.Println()

	// 5. What happened.
	step("outcome", fmt.Sprintf("%v in %s", out.Class, time.Since(started).Round(time.Millisecond)))
	note(out.Note)
	note(fmt.Sprintf("turns=%d tokens in=%d out=%d%s", out.Turns, out.TokensIn, out.TokensOut, costSuffix(out)))
	note("session " + out.Session.ID + " (resumable)")
	for _, d := range out.Denials {
		note("DENIED " + d.Summary())
	}

	// 6. Commit and show the diff. The summary a human reviews comes from this, never from
	//    the agent's own account of what it did.
	hash, err := r.CommitAll(ctx, wt, "gravy: "+firstWords(prompt, 10))
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if hash == "" {
		step("diff", "the agent changed nothing")
	} else {
		diff, err := r.Diff(ctx, wt, "HEAD~1")
		if err != nil {
			return fmt.Errorf("diff: %w", err)
		}
		adds, dels := diff.Totals()
		step("diff", fmt.Sprintf("%d file(s), +%d -%d, commit %s", len(diff.Files), adds, dels, shortHash(hash)))
		for _, f := range diff.Files {
			note(fmt.Sprintf("%-9s %-40s +%d -%d", f.Status, f.Path, f.Additions, f.Deletions))
		}
	}

	// 7. Tidy up, unless asked not to.
	if *keep {
		step("kept", wt.Path)
		note("worktree and logs left in place: " + gravyHome)
	} else {
		if err := r.RemoveWorktree(ctx, wt); err != nil {
			return fmt.Errorf("remove worktree: %w", err)
		}
		step("cleanup", "worktree removed")
	}

	fmt.Println()
	fmt.Println("Nothing was merged. In the real loop this would now go to validation,")
	fmt.Println("automated review, and your approval before it touched any branch.")
	return nil
}

// printEvent renders one streamed event with the time it arrived, so that the stream is visibly
// live rather than replayed at exit.
//
// Paths are shown relative to the worktree. Absolute temp paths are both unreadable and
// misleading here: the interesting fact is which file inside the ticket's isolated tree the
// agent touched, not where that tree happens to live on disk.
func printEvent(started time.Time, worktree string, e provider.Event) {
	at := time.Since(started).Round(100 * time.Millisecond)
	stamp := fmt.Sprintf("  %6.1fs ", at.Seconds())
	text := relativize(e.Text, worktree)

	switch e.Kind {
	case provider.EventStarted:
		fmt.Println(stamp + "· session started")
	case provider.EventToolUse:
		fmt.Println(stamp + "→ " + oneLine(toolLine(e, worktree), 100))
	case provider.EventToolResult:
		// Results are frequent and uninformative on their own; the tool call above says it.
	case provider.EventMessage:
		fmt.Println(stamp + "  " + oneLine(text, 100))
	case provider.EventThinking:
		// Liveness only. Printing every one of these would drown the useful output.
	case provider.EventRateLimit:
		if u, ok := e.Fields["unifiedWindows"].(map[string]any); ok {
			if five, ok := u["five_hour"].(map[string]any); ok {
				if util, ok := five["utilization"].(float64); ok {
					fmt.Printf("%s· quota: %.0f%% of the 5-hour window used\n", stamp, util*100)
					return
				}
			}
		}
	case provider.EventError:
		fmt.Println(stamp + "! " + oneLine(text, 100))
	case provider.EventFinished:
		fmt.Println(stamp + "· finished")
	}
}

// makeScratchRepo builds a throwaway git repository with one commit.
func makeScratchRepo(ctx context.Context, h host.Host) (string, error) {
	dir, err := os.MkdirTemp("", "gravy-scratch-*")
	if err != nil {
		return "", fmt.Errorf("create scratch dir: %w", err)
	}
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		return "", fmt.Errorf("create scratch repo: %w", err)
	}
	files := map[string]string{
		"README.md": "# scratch\n\nA throwaway repository for `gravy try`.\n",
		"main.go":   "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			return "", fmt.Errorf("write %s: %w", name, err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "gravy"},
		{"config", "user.email", "gravy@localhost"},
		{"add", "-A"},
		{"commit", "-q", "-m", "initial"},
	} {
		if err := runGit(ctx, h, repo, args...); err != nil {
			return "", err
		}
	}
	return repo, nil
}

// runGit runs a git command through the Host, like everything else does.
func runGit(ctx context.Context, h host.Host, dir string, args ...string) error {
	p, err := h.Exec(ctx, host.ExecSpec{Cmd: "git", Args: args, Dir: dir, Timeout: time.Minute})
	if err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	outCh, errCh := slurp(p.Stdout()), slurp(p.Stderr())
	st, err := p.Wait()
	out, errOut := <-outCh, <-errCh
	if err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if st.Code != 0 {
		return fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), st.Code, strings.TrimSpace(out+errOut))
	}
	return nil
}

func slurp(r io.Reader) <-chan string {
	ch := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		ch <- string(b)
	}()
	return ch
}

func step(label, detail string) { fmt.Printf("%-11s %s\n", label+":", detail) }
func note(detail string)        { fmt.Printf("            %s\n", detail) }

func costSuffix(o provider.Outcome) string {
	if o.CostUSD == nil {
		return ""
	}
	return fmt.Sprintf(" cost=$%.4f", *o.CostUSD)
}

// toolLine renders a tool call as "Tool argument".
//
// It reads the structured fields rather than the adapter's pre-rendered summary, because that
// summary is truncated for storage — and a long worktree path gets cut before its prefix ends,
// leaving nothing for relativize to strip.
func toolLine(e provider.Event, worktree string) string {
	for _, key := range []string{"command", "file_path", "path", "pattern", "url", "description"} {
		v, ok := e.Fields[key]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		return e.Tool + " " + relativize(s, worktree)
	}
	if e.Tool != "" {
		return e.Tool
	}
	return relativize(e.Text, worktree)
}

// relativize strips the worktree prefix from a path.
//
// macOS resolves temporary directories through a /private symlink, so the agent reports
// /private/var/... while the worktree is known as /var/.... Both forms are tried, longest
// first: stripping the shorter one first would match inside the longer path and eat its middle,
// turning /private/var/…/worktree/main.go into /privatemain.go.
func relativize(s, worktree string) string {
	if worktree == "" {
		return s
	}
	candidates := []string{worktree}
	if resolved, err := filepath.EvalSymlinks(worktree); err == nil && resolved != worktree {
		candidates = append(candidates, resolved)
	}
	return relativizeCandidates(s, candidates)
}

// relativizeCandidates strips the longest matching prefix from s.
func relativizeCandidates(s string, candidates []string) string {
	sort.Slice(candidates, func(i, j int) bool { return len(candidates[i]) > len(candidates[j]) })
	for _, c := range candidates {
		if c == "" || !strings.Contains(s, c) {
			continue
		}
		s = strings.ReplaceAll(s, c+string(filepath.Separator), "")
		return strings.ReplaceAll(s, c, ".")
	}
	return s
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
