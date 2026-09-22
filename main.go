// backcheck — an unattended build harness: a plan goes in, committed and
// reviewed code comes out. One model writes, a different model judges.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const usage = `backcheck — unattended builds, one model writes, another judges

setup (once):
  backcheck init [--worktree ../app-backcheck] [--branch backcheck/build]
  backcheck auto "what to build" [--plan F] [--edit] [--yes] [--no-run]
                                  plan, approve, rehearse and run, in one go;
                                  providers stay yours to write
  backcheck plan PLAN.md            planner drafts waves, rules, gate → .backcheck/draft/
  backcheck approve [--yes]         you bless the rails; creates the worktree + control file
  backcheck rehearse                preflight, probes, fingerprints, gate — no session spent

running:
  backcheck keeper                  supervise the driver (restart if it dies) — use this
  backcheck run                     the driver itself, in the foreground

watching and steering:
  backcheck status [--json]         where the build is
  backcheck watch [--interval 30] [--quiet 1200]
                                  one line per change; silence means nothing happened
  backcheck review [WAVE]           print the latest review (of WAVE)
  backcheck wake                    stop waiting — re-probe and dispatch now
  backcheck resume --because "…" [--accept] [--reset-rework]
                                  clear a HALTED / NEEDS_OPERATOR stop
  backcheck note "text"             one-shot note appended to the next session's brief

exporting:
  backcheck squash [--branch backcheck/export] [--wave N] [--dry-run]
                                  one commit per verified wave on a separate
                                  branch; the build branch is never rewritten
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "init":
		err = cmdInit(args)
	case "plan":
		err = cmdPlan(args)
	case "approve":
		err = cmdApprove(args)
	case "run":
		err = cmdRun(args)
	case "keeper":
		err = cmdKeeper(args)
	case "status":
		err = cmdStatus(args)
	case "watch":
		err = cmdWatch(args)
	case "review":
		err = cmdReview(args)
	case "wake":
		err = cmdWake(args)
	case "resume":
		err = cmdResume(args)
	case "note":
		err = cmdNote(args)
	case "rehearse":
		err = cmdRehearse(args)
	case "squash":
		err = cmdSquash(args)
	case "auto":
		err = cmdAuto(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "backcheck:", err)
		os.Exit(1)
	}
}

func loadHere() (*Config, error) {
	root, err := findRoot(".")
	if err != nil {
		return nil, err
	}
	return LoadConfig(root)
}

// ---------------------------------------------------------------- init

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	wt := fs.String("worktree", "", "build worktree path (default ../<repo>-backcheck)")
	br := fs.String("branch", "backcheck/build", "build branch")
	fs.Parse(args)

	top, err := git(".", "rev-parse", "--show-toplevel")
	if err != nil {
		return errors.New("run init inside your main git repository")
	}
	state := filepath.Join(top, ".backcheck")
	if _, err := os.Stat(filepath.Join(state, "config.json")); err == nil {
		return errors.New(".backcheck/config.json already exists")
	}
	if err := os.MkdirAll(filepath.Join(state, "sessions"), 0o755); err != nil {
		return err
	}
	c := defaultConfig()
	c.Branch = *br
	c.Worktree = *wt
	if c.Worktree == "" {
		c.Worktree = "../" + filepath.Base(top) + "-backcheck"
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(filepath.Join(state, "config.json"), b, 0o644); err != nil {
		return err
	}
	// keep state out of the repo without touching the tracked .gitignore
	gitDir, _ := git(top, "rev-parse", "--git-common-dir")
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(top, gitDir)
	}
	excl := filepath.Join(gitDir, "info", "exclude")
	if cur := readFileOr(excl, ""); !strings.Contains(cur, "/.backcheck/") {
		_ = os.MkdirAll(filepath.Dir(excl), 0o755)
		f, err := os.OpenFile(excl, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			f.WriteString("\n/.backcheck/\n")
			f.Close()
		}
	}
	fmt.Printf("created %s\nnext: edit providers/roles in config.json, then `backcheck plan PLAN.md`\n", filepath.Join(state, "config.json"))
	return nil
}

// ---------------------------------------------------------------- plan / approve

func cmdPlan(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: backcheck plan PLAN.md")
	}
	cfg, err := loadHere()
	if err != nil {
		return err
	}
	plan, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	role, ok := cfg.Roles["planner"]
	if !ok {
		role = cfg.Roles["reviewer"]
	}
	prov := cfg.Providers[role.Provider]
	files, _ := git(cfg.root, "ls-files")
	const limit = 400
	fl := strings.Split(files, "\n")
	if len(fl) > limit {
		fl = append(fl[:limit], fmt.Sprintf("… and %d more", len(fl)-limit))
	}
	prompt := render("planner", PromptData{Worktree: cfg.root, Plan: string(plan), Files: strings.Join(fl, "\n"), FileLimit: limit})
	draft := filepath.Join(cfg.stateDir, "draft")
	_ = os.MkdirAll(draft, 0o755)
	fmt.Printf("planning with %s (%s)…\n", role.Provider, prov.Model)
	out := Dispatch(context.Background(), prov, prompt, cfg.root, 30*time.Minute, 10*time.Minute, filepath.Join(draft, "planner.stream.jsonl"))
	_ = os.WriteFile(filepath.Join(draft, "planner.md"), []byte(out.Text), 0o644)
	if out.Kind != OutOK {
		return fmt.Errorf("planner session %s: %s", out.Kind, out.Detail)
	}
	got := parsePlannerFiles(out.Text)
	for _, name := range []string{"waves.md", "rules.md", "rails.json"} {
		body, ok := got[name]
		if !ok {
			return fmt.Errorf("planner output is missing %s — see %s", name, filepath.Join(draft, "planner.md"))
		}
		if err := os.WriteFile(filepath.Join(draft, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	if _, err := ParseWaves(got["waves.md"]); err != nil {
		return fmt.Errorf("drafted waves.md does not parse (%v) — edit %s and run approve", err, filepath.Join(draft, "waves.md"))
	}
	var rails railsFile
	if err := json.Unmarshal([]byte(got["rails.json"]), &rails); err != nil {
		return fmt.Errorf("drafted rails.json is not valid JSON (%v) — fix %s", err, filepath.Join(draft, "rails.json"))
	}
	fmt.Printf("draft written to %s — read and edit waves.md, rules.md, rails.json, then `backcheck approve`\n", draft)
	return nil
}

type railsFile struct {
	Gate          []Check  `json:"gate"`
	Preflight     []Check  `json:"preflight"`
	ReadonlyPaths []string `json:"readonly_paths"`
	Fingerprints  []Check  `json:"fingerprints"`
}

// approve: the planner drafted the rails; it cannot also bless them.
func cmdApprove(args []string) error {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	fs.Parse(args)
	cfg, err := loadHere()
	if err != nil {
		return err
	}
	if _, err := cfg.LoadControl(); err == nil {
		return errors.New("a build already exists (.backcheck/control.json) — amend the rails in place instead; they are re-read before every session")
	}
	draft := filepath.Join(cfg.stateDir, "draft")
	waves := readFileOr(filepath.Join(draft, "waves.md"), "")
	if waves == "" {
		waves = readFileOr(cfg.wavesPath(), "")
	}
	ws, err := ParseWaves(waves)
	if err != nil {
		return err
	}
	rules := readFileOr(filepath.Join(draft, "rules.md"), readFileOr(cfg.rulesPath(), ""))
	var rails railsFile
	if rj := readFileOr(filepath.Join(draft, "rails.json"), ""); rj != "" {
		if err := json.Unmarshal([]byte(rj), &rails); err != nil {
			return fmt.Errorf("draft/rails.json: %w", err)
		}
	}

	fmt.Printf("Waves (%d):\n", len(ws))
	for _, w := range ws {
		fmt.Printf("  %d. %s\n", w.N, w.Title)
	}
	fmt.Printf("Gate:        %s\n", checkNames(orChecks(rails.Gate, cfg.Gate)))
	fmt.Printf("Preflight:   %s\n", checkNames(orChecks(rails.Preflight, cfg.Preflight)))
	fmt.Printf("Fingerprint: %s\n", checkNames(orChecks(rails.Fingerprints, cfg.Fingerprints)))
	fmt.Printf("Read-only:   %s\n", strings.Join(orStrings(rails.ReadonlyPaths, cfg.ReadonlyPaths), ", "))
	fmt.Printf("Builder: %s   Reviewer: %s\n", cfg.Roles["builder"].Provider, cfg.Roles["reviewer"].Provider)
	if cfg.SingleProvider() {
		fmt.Println("WARNING: builder and reviewer share one provider — the strongest property is off.")
	} else if cfg.SharedAccount() {
		fmt.Println("WARNING: " + cfg.sharedAccountWarning())
	}
	if !*yes {
		fmt.Print("Approve these rails and start a build? [y/N] ")
		var a string
		fmt.Scanln(&a)
		if strings.ToLower(strings.TrimSpace(a)) != "y" {
			return errors.New("not approved")
		}
	}

	// merge rails into config
	raw := map[string]any{}
	_ = json.Unmarshal([]byte(readFileOr(filepath.Join(cfg.stateDir, "config.json"), "{}")), &raw)
	if rails.Gate != nil {
		raw["gate"] = rails.Gate
	}
	if rails.Preflight != nil {
		raw["preflight"] = rails.Preflight
	}
	if rails.ReadonlyPaths != nil {
		raw["readonly_paths"] = rails.ReadonlyPaths
	}
	if rails.Fingerprints != nil {
		raw["fingerprints"] = rails.Fingerprints
	}
	b, _ := json.MarshalIndent(raw, "", "  ")
	if err := os.WriteFile(filepath.Join(cfg.stateDir, "config.json"), b, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(cfg.wavesPath(), []byte(waves), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(cfg.rulesPath(), []byte(rules), 0o644); err != nil {
		return err
	}

	// worktree + branch, cut from the main repo's HEAD
	wt := cfg.WorktreePath()
	if _, err := os.Stat(wt); err != nil {
		if _, err := git(cfg.root, "rev-parse", "--verify", cfg.Branch); err == nil {
			_, err = git(cfg.root, "worktree", "add", wt, cfg.Branch)
			if err != nil {
				return err
			}
		} else if _, err := git(cfg.root, "worktree", "add", "-b", cfg.Branch, wt); err != nil {
			return err
		}
	}
	base := gitHead(wt)
	ctl := &Control{Status: StRunning, Wave: 1, Role: "builder", Session: "a", BaseSHA: base, WaveBase: base}
	if err := cfg.SaveControl(ctl); err != nil {
		return err
	}
	_ = os.MkdirAll(cfg.sessionsDir(), 0o755)
	cfg.Emit("approved", 1, fmt.Sprintf("%d waves on %s from %s", len(ws), cfg.Branch, short(base)), nil)
	fmt.Println("approved. next: `backcheck rehearse`, then `backcheck keeper`")
	return nil
}

func orChecks(a, b []Check) []Check {
	if a != nil {
		return a
	}
	return b
}
func orStrings(a, b []string) []string {
	if a != nil {
		return a
	}
	return b
}
func checkNames(cs []Check) string {
	var n []string
	for _, c := range cs {
		n = append(n, c.Name)
	}
	if len(n) == 0 {
		return "(none)"
	}
	return strings.Join(n, ", ")
}

// ---------------------------------------------------------------- run / keeper

func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func cmdRun(args []string) error {
	root, err := findRoot(".")
	if err != nil {
		return err
	}
	d, err := NewDriver(root)
	if err != nil {
		return err
	}
	ctx, cancel := signalCtx()
	defer cancel()
	return d.Run(ctx)
}

// keeper restarts a dead driver and stands down once the build is CLOSED.
func cmdKeeper(args []string) error {
	cfg, err := loadHere()
	if err != nil {
		return err
	}
	self, _ := os.Executable()
	ctx, cancel := signalCtx()
	defer cancel()
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		if ctl, err := cfg.LoadControl(); err == nil && ctl.Status == StClosed {
			fmt.Println("keeper: build CLOSED — standing down")
			return nil
		}
		if cfg.DriverAlive() {
			return errors.New("a driver is already running for this build")
		}
		started := time.Now()
		cmd := exec.CommandContext(ctx, self, "run")
		cmd.Dir = cfg.root
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err := cmd.Run()
		if ctx.Err() != nil {
			return nil
		}
		if ctl, e := cfg.LoadControl(); e == nil && ctl.Status == StClosed {
			fmt.Println("keeper: build CLOSED — standing down")
			return nil
		}
		if time.Since(started) > 10*time.Minute {
			backoff = 5 * time.Second
		}
		cfg.Emit("driver-restart", 0, fmt.Sprintf("driver exited (%v) — restarting in %s", err, backoff), nil)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 5*time.Minute {
			backoff *= 2
		}
	}
	return nil
}

// ---------------------------------------------------------------- status / review

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable")
	fs.Parse(args)
	cfg, err := loadHere()
	if err != nil {
		return err
	}
	ctl, err := cfg.LoadControl()
	if err != nil {
		return errors.New("no build yet — run `backcheck approve`")
	}
	waves, _ := cfg.LoadWaves()
	wt := cfg.WorktreePath()
	if *asJSON {
		// The watcher reads this. Keys are only ever added, never renamed or
		// removed; see TestStatusJSONKeysAreStable.
		b, _ := json.MarshalIndent(map[string]any{"control": ctl, "ledger": cfg.Ledger(), "driver_alive": cfg.DriverAlive(),
			"head": gitHead(wt), "waves": len(waves), "spend": cfg.Spend()}, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("status   %s", ctl.Status)
	if ctl.Reason != "" {
		fmt.Printf(" — %s", ctl.Reason)
	}
	if ctl.Status == StWaiting {
		fmt.Printf(" (until %s)", ctl.WaitUntil)
	}
	fmt.Println()
	title := ""
	if ctl.Wave >= 1 && ctl.Wave <= len(waves) {
		title = waves[ctl.Wave-1].Title
	}
	fmt.Printf("wave     %d/%d %s\n", min(ctl.Wave, len(waves)), len(waves), title)
	if ctl.Status != StClosed {
		fmt.Printf("next     %s (session %s, review round %d, rework streak %d)\n", ctl.Role, ctl.Session, ctl.Round, ctl.ReworkStreak)
	}
	fmt.Printf("driver   alive=%v   head %s   commits this wave: %d\n", cfg.DriverAlive(), short(gitHead(wt)), commitsSince(wt, ctl.WaveBase, ctl.Wave))
	sp := cfg.Spend()
	spent := fmt.Sprintf("spend    $%.4f over %d session(s)", sp.USD, sp.Sessions)
	if len(sp.ByRole) > 0 {
		var parts []string
		for _, r := range []string{"builder", "reviewer", "planner"} {
			if v, ok := sp.ByRole[r]; ok {
				parts = append(parts, fmt.Sprintf("%s $%.4f", r, v))
			}
		}
		if len(parts) > 0 {
			spent += "   (" + strings.Join(parts, ", ") + ")"
		}
	}
	if cfg.Limits.MaxSessionsPerWave > 0 {
		spent += fmt.Sprintf("   this wave: %d/%d", ctl.Sessions, cfg.Limits.MaxSessionsPerWave)
	}
	fmt.Println(spent)
	if len(ctl.ProviderLimited) > 0 {
		var ks []string
		for k, v := range ctl.ProviderLimited {
			ks = append(ks, k+" until "+v)
		}
		sort.Strings(ks)
		fmt.Printf("limited  %s\n", strings.Join(ks, "; "))
	}
	if len(ctl.ReworkHistory) > 0 {
		fmt.Println("rework rounds on this wave:")
		for i, r := range ctl.ReworkHistory {
			fmt.Printf("  %d. %s\n", i+1, strings.Join(r, " | "))
		}
	}
	fmt.Println("ledger:")
	for _, r := range cfg.Ledger() {
		fmt.Printf("  wave %d  %s  %s  rounds=%d  by=%s\n", r.Wave, short(r.SHA), r.VerifiedAt, r.Rounds, r.By)
	}
	fmt.Println("recent events:")
	for _, e := range tailEvents(cfg, 8) {
		fmt.Printf("  %s %-14s %s\n", e.Time, e.Event, e.Msg)
	}
	return nil
}

func tailEvents(cfg *Config, n int) []Event {
	lines := strings.Split(strings.TrimSpace(readFileOr(cfg.eventsPath(), "")), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	var out []Event
	for _, l := range lines {
		var e Event
		if json.Unmarshal([]byte(l), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func cmdReview(args []string) error {
	cfg, err := loadHere()
	if err != nil {
		return err
	}
	pattern := "w*-review-r*.md"
	if len(args) == 1 {
		var n int
		fmt.Sscan(args[0], &n)
		pattern = fmt.Sprintf("w%02d-review-r*.md", n)
	}
	files, _ := filepath.Glob(filepath.Join(cfg.sessionsDir(), pattern))
	if len(files) == 0 {
		return errors.New("no reviews yet")
	}
	sort.Slice(files, func(i, j int) bool {
		a, _ := os.Stat(files[i])
		b, _ := os.Stat(files[j])
		return a.ModTime().Before(b.ModTime())
	})
	last := files[len(files)-1]
	fmt.Printf("== %s ==\n%s\n", last, readFileOr(last, ""))
	return nil
}

// ---------------------------------------------------------------- steering

func cmdWake(args []string) error {
	cfg, err := loadHere()
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfg.wakePath(), []byte(time.Now().Format(time.RFC3339)), 0o644); err != nil {
		return err
	}
	ctl, _ := cfg.LoadControl()
	w := 0
	if ctl != nil {
		w = ctl.Wave
	}
	cfg.Emit("wake-requested", w, "operator: stop waiting, re-probe now", nil)
	return nil
}

func cmdResume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	because := fs.String("because", "", "what you fixed (required — resume only after the cause is fixed)")
	accept := fs.Bool("accept", false, "NEEDS_OPERATOR: accept the current wave as done and move to the next")
	resetRework := fs.Bool("reset-rework", false, "reset the consecutive-REWORK counter for this wave")
	fs.Parse(args)
	if strings.TrimSpace(*because) == "" {
		return errors.New(`resume needs --because "what you fixed"`)
	}
	cfg, err := loadHere()
	if err != nil {
		return err
	}
	ctl, err := cfg.LoadControl()
	if err != nil {
		return err
	}
	if ctl.Status != StHalted && ctl.Status != StNeedsOperator {
		return fmt.Errorf("nothing to resume: status is %s", ctl.Status)
	}
	prev := ctl.Status + ": " + ctl.Reason
	ctl.Status, ctl.Reason = StRunning, ""
	ctl.Stalled, ctl.Failures, ctl.ProviderLimited, ctl.RetryNote = 0, 0, nil, ""
	ctl.Retries, ctl.Sessions = 0, 0 // the operator said carry on; a stop that
	// re-fires on the next iteration is not a resume
	ctl.ProviderAuthFailed = nil // the operator says the cause is fixed; believe them and re-probe
	if *resetRework {
		ctl.ReworkStreak = 0
	}
	if *accept {
		waves, err := cfg.LoadWaves()
		if err != nil {
			return err
		}
		head := gitHead(cfg.WorktreePath())
		_ = cfg.AppendLedger(LedgerRow{Wave: ctl.Wave, SHA: head, VerifiedAt: time.Now().Format(time.RFC3339),
			Rounds: ctl.Round, By: "operator", Reviewer: "operator: " + *because})
		cfg.Emit("verdict", ctl.Wave, "operator ACCEPTED wave at "+short(head), map[string]any{"verdict": "ACCEPTED"})
		d := &Driver{root: cfg.root, cfg: cfg}
		d.advanceWave(ctl, head, len(waves))
		if ctl.Status == StClosed {
			return nil
		}
	}
	if err := cfg.SaveControl(ctl); err != nil {
		return err
	}
	cfg.Emit("resume", ctl.Wave, fmt.Sprintf("cleared (%s) because: %s", prev, *because), nil)
	_ = os.WriteFile(cfg.wakePath(), []byte("resume"), 0o644)
	return nil
}

func cmdNote(args []string) error {
	if len(args) == 0 {
		return errors.New(`usage: backcheck note "text for the next session"`)
	}
	cfg, err := loadHere()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(cfg.notePath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(args, " ") + "\n")
	cfg.Emit("note", 0, "operator note queued for the next session", nil)
	return err
}

// ---------------------------------------------------------------- auto

// auto chains plan → approve → rehearse → keeper from a single prompt, so the
// judge model drafts the waves, the rules and the rails and the build starts.
//
// What it deliberately does NOT generate is `providers`: which model runs,
// with whose credentials and which tools it is allowed to use. That is the one
// part of the config that decides what a model may do to this machine, and it
// is not something to hand to a model.
//
// The plan can equally be written by hand (--plan FILE, or --reuse-draft over
// a draft you wrote yourself), the drafted rails can be edited before they are
// approved (--edit), and everything stays editable afterwards, because the
// rails are re-read before every session.
func cmdAuto(args []string) error {
	fs := flag.NewFlagSet("auto", flag.ExitOnError)
	planFile := fs.String("plan", "", "read the plan from this file instead of the arguments")
	edit := fs.Bool("edit", false, "open the drafted rails in $EDITOR before approving")
	yes := fs.Bool("yes", false, "do not ask — approve the draft as it stands")
	noRun := fs.Bool("no-run", false, "stop after rehearse instead of starting the keeper")
	reuse := fs.Bool("reuse-draft", false, "keep the existing .backcheck/draft instead of planning again")
	fs.Parse(args)

	cfg, err := loadHere()
	if err != nil {
		return err
	}
	if _, err := cfg.LoadControl(); err == nil {
		return errors.New("a build already exists — amend .backcheck/waves.md, rules.md or config.json in place; they are re-read before every session")
	}
	draft := filepath.Join(cfg.stateDir, "draft")

	// 1 · where the plan comes from: a file, the arguments, or stdin.
	if !*reuse {
		text := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if *planFile != "" {
			b, err := os.ReadFile(*planFile)
			if err != nil {
				return err
			}
			text = string(b)
		} else if text == "" {
			if st, _ := os.Stdin.Stat(); st != nil && st.Mode()&os.ModeCharDevice == 0 {
				b, _ := io.ReadAll(os.Stdin)
				text = string(b)
			}
		}
		if strings.TrimSpace(text) == "" {
			return errors.New(`usage: backcheck auto "what to build"   (or --plan FILE, or pipe it in, or --reuse-draft)`)
		}
		_ = os.MkdirAll(draft, 0o755)
		planPath := filepath.Join(cfg.stateDir, "plan.md")
		if err := os.WriteFile(planPath, []byte(text), 0o644); err != nil {
			return err
		}
		// 2 · the judge model drafts waves.md, rules.md and rails.json.
		if err := cmdPlan([]string{planPath}); err != nil {
			return err
		}
	}

	// 3 · read back what it drafted, and say what is worth a second look.
	show := func() (railsFile, error) {
		var rails railsFile
		rj := readFileOr(filepath.Join(draft, "rails.json"), "")
		if rj == "" {
			return rails, fmt.Errorf("no rails.json in %s — write one, or drop --reuse-draft", draft)
		}
		if err := json.Unmarshal([]byte(rj), &rails); err != nil {
			return rails, fmt.Errorf("draft/rails.json: %w", err)
		}
		ws, err := ParseWaves(readFileOr(filepath.Join(draft, "waves.md"), ""))
		if err != nil {
			return rails, err
		}
		fmt.Printf("\ndrafted in %s\n", draft)
		for _, w := range ws {
			fmt.Printf("  wave %d  %s\n", w.N, w.Title)
		}
		fmt.Printf("  gate        %s\n", checkNames(rails.Gate))
		fmt.Printf("  preflight   %s\n", checkNames(rails.Preflight))
		fmt.Printf("  fingerprint %s\n", checkNames(rails.Fingerprints))
		fmt.Printf("  read-only   %s\n\n", strings.Join(orStrings(rails.ReadonlyPaths, []string{"(none)"}), ", "))
		printFindings(lintRails(rails, cfg.root))
		return rails, nil
	}
	if _, err := show(); err != nil {
		return err
	}

	// 4 · hand it over to be edited, then look again at what came back.
	if *edit {
		ed := os.Getenv("EDITOR")
		if ed == "" {
			return errors.New("--edit needs $EDITOR set; the files are in " + draft)
		}
		cmd := exec.Command(ed, filepath.Join(draft, "waves.md"), filepath.Join(draft, "rules.md"), filepath.Join(draft, "rails.json"))
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", ed, err)
		}
		if _, err := show(); err != nil {
			return err
		}
	}

	// 5 · the planner does not get to bless its own rails.
	if !*yes {
		fmt.Print("Approve these rails and start a build? [y/N] ")
		var a string
		fmt.Scanln(&a)
		if strings.ToLower(strings.TrimSpace(a)) != "y" {
			return fmt.Errorf("not approved — the draft is still in %s; edit it and run `backcheck auto --reuse-draft`", draft)
		}
	}
	if err := cmdApprove([]string{"--yes"}); err != nil {
		return err
	}
	// 6 · rehearse before spending anything: a bad config should cost nothing.
	if err := cmdRehearse(nil); err != nil {
		return fmt.Errorf("%w — fix it, then `backcheck rehearse` and `backcheck keeper`", err)
	}
	fmt.Println("\nEverything below stays editable while the build runs — the rails are")
	fmt.Println("re-read before every session:")
	fmt.Println("  .backcheck/waves.md    what each wave must achieve")
	fmt.Println("  .backcheck/rules.md    how the builder and reviewer must work")
	fmt.Println("  .backcheck/config.json gate, preflight, read-only paths, limits")
	fmt.Println("  backcheck note \"…\"     one-shot guidance for the next session")
	if *noRun {
		fmt.Println("\n--no-run: start it yourself with `backcheck keeper`")
		return nil
	}
	fmt.Println()
	return cmdKeeper(nil)
}

// ---------------------------------------------------------------- squash

// squash writes one commit per verified wave onto a separate export branch.
// It never touches the build branch: the ledger pins a SHA per wave, and the
// history that produced it is the evidence the reviews refer to. This only
// builds a second, tidier view of the same trees with commit-tree, so nothing
// is rewritten and the export can be thrown away and rebuilt at any time.
func cmdSquash(args []string) error {
	fs := flag.NewFlagSet("squash", flag.ExitOnError)
	branch := fs.String("branch", "backcheck/export", "export branch to write (created or replaced)")
	upto := fs.Int("wave", 0, "stop after this wave (default: every verified wave)")
	dryRun := fs.Bool("dry-run", false, "print what would be written and stop")
	fs.Parse(args)
	cfg, err := loadHere()
	if err != nil {
		return err
	}
	if *branch == cfg.Branch {
		return fmt.Errorf("--branch %q is the build branch; squash never rewrites it", *branch)
	}
	ctl, err := cfg.LoadControl()
	if err != nil {
		return errors.New("no build yet — run `backcheck approve`")
	}
	rows := cfg.Ledger()
	if len(rows) == 0 {
		return errors.New("nothing to squash: no wave has been verified yet")
	}
	waves, _ := cfg.LoadWaves()
	title := func(n int) string {
		if n >= 1 && n <= len(waves) {
			return waves[n-1].Title
		}
		return fmt.Sprintf("wave %d", n)
	}

	// Later rows win, so an operator --accept after a reviewer signature
	// exports the wave once, as it finally stood.
	latest := map[int]LedgerRow{}
	var order []int
	for _, r := range rows {
		if _, seen := latest[r.Wave]; !seen {
			order = append(order, r.Wave)
		}
		latest[r.Wave] = r
	}
	sort.Ints(order)

	wt := cfg.WorktreePath()
	parent := ctl.BaseSHA
	if parent == "" {
		return errors.New("control.json has no base_sha to export from")
	}
	var wrote []string
	for _, n := range order {
		if *upto > 0 && n > *upto {
			break
		}
		r := latest[n]
		tree, err := git(wt, "rev-parse", r.SHA+"^{tree}")
		if err != nil {
			return fmt.Errorf("wave %d: %w", n, err)
		}
		msg := fmt.Sprintf("wave %d: %s\n\nSquashed from %s, verified %s by %s.\nReview: %s\n\nBackCheck-Wave: %d\n",
			n, title(n), short(r.SHA), r.VerifiedAt, r.Reviewer, r.Review, n)
		if *dryRun {
			fmt.Printf("would write wave %d  tree %s  parent %s  (%s)\n", n, short(tree), short(parent), title(n))
			wrote = append(wrote, fmt.Sprintf("wave %d", n))
			continue
		}
		sha, err := gitInput(wt, msg, "commit-tree", tree, "-p", parent)
		if err != nil {
			return fmt.Errorf("wave %d: commit-tree: %w", n, err)
		}
		parent = sha
		wrote = append(wrote, fmt.Sprintf("wave %d → %s", n, short(sha)))
	}
	if *dryRun {
		fmt.Printf("dry run: %d wave(s), nothing written\n", len(wrote))
		return nil
	}
	// -f only ever points the export branch; the build branch is not an argument.
	if _, err := git(wt, "branch", "-f", *branch, parent); err != nil {
		return err
	}
	for _, w := range wrote {
		fmt.Println("  " + w)
	}
	fmt.Printf("%s now has %d commit(s) at %s; %s is untouched\n", *branch, len(wrote), short(parent), cfg.Branch)
	cfg.Emit("squash", ctl.Wave, fmt.Sprintf("exported %d verified wave(s) to %s at %s", len(wrote), *branch, short(parent)), nil)
	return nil
}

// ---------------------------------------------------------------- rehearse

// rehearse answers every cheap question in the loop without spending a session.
func cmdRehearse(args []string) error {
	cfg, err := loadHere()
	if err != nil {
		return err
	}
	ctx := context.Background()
	ok := true
	report := func(name string, good bool, msg string) {
		mark := "ok  "
		if !good {
			mark, ok = "FAIL", false
		}
		fmt.Printf("[%s] %-22s %s\n", mark, name, msg)
	}
	waves, err := cfg.LoadWaves()
	report("waves.md", err == nil, fmt.Sprintf("%d waves %v", len(waves), errStr(err)))
	ctl, err := cfg.LoadControl()
	report("control.json", err == nil, errStr(err))
	if ctl == nil {
		ctl = &Control{}
	}
	d := &Driver{root: cfg.root, cfg: cfg}
	msg, _ := d.preflight(ctx, ctl)
	report("preflight", msg == "", msg)
	for name, p := range cfg.Providers {
		o := ProbeProvider(ctx, p, cfg.WorktreePath(), time.Duration(cfg.Limits.ProbeTimeoutSeconds)*time.Second)
		detail := fmt.Sprintf("%s model=%s", o.Kind, o.Model)
		if o.Kind != OutOK {
			detail += " " + o.Detail
		}
		if p.ExpectModel != "" && o.Model != "" && !strings.Contains(o.Model, p.ExpectModel) {
			detail += " (expected " + p.ExpectModel + ")"
		}
		report("provider "+name, o.Kind == OutOK, detail)
	}
	// The alert path is checked here for the same reason as everything else in
	// rehearse: finding out it was never wired up at the moment of a HALT is
	// finding out too late. This really does pop an alert.
	switch {
	case len(cfg.Notify.Command) == 0:
		report("notify", true, "(none configured — HALT and NEEDS_OPERATOR will be silent)")
	default:
		err := cfg.Notify1("rehearse", 0, "backcheck rehearsal — this is what a HALT will look like", 15*time.Second)
		report("notify", err == nil, strings.Join(cfg.Notify.Command, " ")+" "+errStr(err))
	}

	fp := d.fingerprints(ctx)
	report("fingerprints", true, fmt.Sprintf("%d sampled", len(fp)))
	if moved := d.fingerprintsMoved(ctx, fp); len(moved) > 0 {
		report("fingerprints stable", false, "unstable between two samples: "+strings.Join(moved, ", ")+" — they would halt every session")
	}
	fmt.Println("gate:")
	fmt.Println(indent(d.gate(ctx)))
	if cfg.SingleProvider() {
		fmt.Println("WARNING: builder and reviewer share one provider.")
	} else if cfg.SharedAccount() {
		fmt.Println("WARNING: " + cfg.sharedAccountWarning())
	}
	if !ok {
		return errors.New("rehearsal found problems")
	}
	fmt.Println("rehearsal clean — `backcheck keeper` to start")
	return nil
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
