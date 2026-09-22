package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type Driver struct {
	root string
	cfg  *Config
}

func NewDriver(root string) (*Driver, error) {
	cfg, err := LoadConfig(root)
	if err != nil {
		return nil, err
	}
	return &Driver{root: root, cfg: cfg}, nil
}

// Run is the driver: one session at a time, until CLOSED or ctx ends.
func (d *Driver) Run(ctx context.Context) error {
	lock, err := d.cfg.AcquireDriverLock()
	if err != nil {
		return err
	}
	defer lock.Release()
	_ = os.MkdirAll(d.cfg.sessionsDir(), 0o755)

	ctl, err := d.cfg.LoadControl()
	if err != nil {
		return fmt.Errorf("no control file — run `backcheck approve` first: %w", err)
	}
	d.cfg.Emit("start", ctl.Wave, fmt.Sprintf("driver pid %d, status %s", os.Getpid(), ctl.Status), nil)
	if d.cfg.SingleProvider() {
		d.cfg.Emit("warning", ctl.Wave, "builder and reviewer share one provider: one quota pool, one outage, and one model family tends to be wrong about the same things", nil)
	} else if d.cfg.SharedAccount() {
		d.cfg.Emit("warning", ctl.Wave, d.cfg.sharedAccountWarning(), nil)
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		if stop := d.Iterate(ctx); stop {
			return nil
		}
	}
}

// Iterate is one turn of the loop. It returns true only when the build is over.
// Cheap questions first; the expensive session last.
func (d *Driver) Iterate(ctx context.Context) (stop bool) {
	// Rails are re-read every iteration: amend them mid-build with no restart.
	if cfg, err := LoadConfig(d.root); err == nil {
		d.cfg = cfg
	} else {
		d.cfg.Emit("retry", 0, "config.json unreadable, keeping previous: "+err.Error(), nil)
	}
	cfg := d.cfg
	poll := time.Duration(cfg.Limits.PollSeconds) * time.Second

	ctl, err := cfg.LoadControl()
	if err != nil {
		cfg.Emit("retry", 0, "control.json unreadable: "+err.Error(), nil)
		cfg.sleepInterruptible(poll)
		return false
	}

	// 1 · Is the build over? The control file's own status line, nothing else.
	switch ctl.Status {
	case StClosed:
		return true
	case StHalted, StNeedsOperator:
		cfg.sleepInterruptible(poll) // idle until `backcheck resume`
		return false
	case StWaiting:
		until, _ := time.Parse(time.RFC3339, ctl.WaitUntil)
		if rem := time.Until(until); rem > 0 {
			if cfg.sleepInterruptible(rem) {
				cfg.Emit("wake", ctl.Wave, "woken by operator before the named reset", nil)
			}
		}
		ctl, _ = cfg.LoadControl()
		if ctl.Status != StWaiting {
			return false
		}
		ctl.Status, ctl.WaitUntil, ctl.Reason = StRunning, "", ""
		ctl.ProviderLimited, ctl.ProviderAuthFailed = nil, nil // re-probe everything
		d.save(ctl)
	}

	waves, err := cfg.LoadWaves()
	if err != nil {
		d.stopFor(ctl, StNeedsOperator, "needs-operator", "rails unreadable: "+err.Error(), nil)
		return false
	}
	if ctl.Wave > len(waves) {
		d.stopFor(ctl, StClosed, "closed", "every wave in waves.md is signed", nil)
		return true
	}
	wave := waves[ctl.Wave-1]
	wt := cfg.WorktreePath()

	// 2 · Preflight — is the machine fit?
	if msg, fatal := d.preflight(ctx, ctl); msg != "" {
		if fatal {
			d.stopFor(ctl, StHalted, "halt", msg, nil)
		} else {
			d.retryLater(ctl, "preflight: "+msg)
		}
		return false
	}

	// 3 · Is this wave converging, and has it spent a reasonable amount?
	if cfg.Limits.MaxSessionsPerWave > 0 && ctl.Sessions >= cfg.Limits.MaxSessionsPerWave {
		d.stopFor(ctl, StNeedsOperator, "needs-operator",
			fmt.Sprintf("wave %d: %d sessions spent and still not signed — the wave is too large, or the rails do not say what finishing looks like", ctl.Wave, ctl.Sessions),
			map[string]any{"sessions": ctl.Sessions, "cap": cfg.Limits.MaxSessionsPerWave})
		return false
	}
	if ctl.ReworkStreak >= cfg.Limits.MaxConsecutiveRework {
		d.stopFor(ctl, StNeedsOperator, "needs-operator",
			fmt.Sprintf("wave %d: %d consecutive REWORK verdicts — the two models are not converging", ctl.Wave, ctl.ReworkStreak),
			map[string]any{"rounds": ctl.ReworkHistory})
		return false
	}

	// 4 · The control file says which session, which role, which brief.
	role := ctl.Role
	if role != "builder" && role != "reviewer" {
		role, ctl.Role = "builder", "builder"
	}

	// 5 · Resolve the provider and probe it.
	provName, prov, ok := d.resolveProvider(ctx, ctl, role)
	if !ok {
		return false
	}
	// 6 · Fingerprint sweep — before.
	fpBefore := d.fingerprints(ctx)

	// 7 · Run the gate (cached per tree).
	gate := d.gate(ctx)

	// 8 · Dispatch ONE session.
	headBefore := gitHead(wt)
	prompt := d.brief(ctl, wave, len(waves), gate, headBefore)
	tag := sessionTag(ctl, role)
	logPath := filepath.Join(cfg.sessionsDir(), tag+".stream.jsonl")
	cfg.Emit("dispatch", ctl.Wave, fmt.Sprintf("%s session %s on %s", role, tag, provName), map[string]any{"provider": provName, "role": role})
	d.consumeNote(tag)
	ctl.Sessions++
	started := time.Now()
	out := Dispatch(ctx, prov, prompt, wt,
		time.Duration(cfg.Limits.SessionWallSeconds)*time.Second,
		time.Duration(cfg.Limits.SessionIdleSeconds)*time.Second, logPath)
	cfg.Emit("dispatch-result", ctl.Wave,
		fmt.Sprintf("%s %s after %s, $%.4f (%s, %d this wave)", role, out.Kind, time.Since(started).Round(time.Second), out.Cost, provName, ctl.Sessions),
		map[string]any{"provider": provName, "role": role, "session": tag, "kind": string(out.Kind),
			"model": out.Model, "cost_usd": out.Cost, "seconds": int(time.Since(started).Seconds()),
			"sessions_this_wave": ctl.Sessions})
	textPath := filepath.Join(cfg.sessionsDir(), tag+".md")
	_ = os.WriteFile(textPath, []byte(out.Text), 0o644)
	if prov.ExpectModel != "" && out.Model != "" && !strings.Contains(out.Model, prov.ExpectModel) {
		cfg.Emit("model-mismatch", ctl.Wave, fmt.Sprintf("%s ran on %q, expected %q", provName, out.Model, prov.ExpectModel), nil)
	}

	// 10 · Fingerprint sweep — after. Whatever the outcome, check the damage first.
	if moved := d.fingerprintsMoved(ctx, fpBefore); len(moved) > 0 {
		d.stopFor(ctl, StHalted, "halt", "fingerprint moved: "+strings.Join(moved, ", ")+" — git cannot restore it; nothing runs until you look", nil)
		return false
	}
	headAfter := gitHead(wt)

	// 9 · Classify what came back.
	if out.Kind != OutOK {
		d.handleFailure(ctl, role, provName, out, headBefore != headAfter)
		return false
	}
	ctl.Failures, ctl.Retries = 0, 0
	if ctl.RetryNote != "" {
		cfg.Emit("recovered", ctl.Wave, "cleared: "+ctl.RetryNote, nil)
		ctl.RetryNote = ""
	}

	// 11 · Did anything actually happen?
	if role == "builder" {
		d.afterBuilder(ctl, out, textPath, headBefore, headAfter)
	} else {
		d.afterReviewer(ctl, out, textPath, headBefore, headAfter, len(waves), out.Model)
	}
	return false
}

func (d *Driver) afterBuilder(ctl *Control, out Outcome, textPath, before, after string) {
	cfg, wt := d.cfg, d.cfg.WorktreePath()
	moved := before != after
	if v := d.readonlyViolation(ctl); v != "" {
		d.stopFor(ctl, StHalted, "halt", v, nil)
		return
	}
	status := parseStatus(out.Text)
	ctl.LastHandoff = textPath
	if moved {
		ctl.Stalled = 0
	}
	switch {
	case status == "DONE" && (moved || (ctl.ReworkStreak == 0 && commitsSince(wt, ctl.WaveBase, ctl.Wave) > 0)):
		ctl.Role, ctl.Round = "reviewer", ctl.Round+1
		cfg.Emit("done", ctl.Wave, fmt.Sprintf("builder %s says DONE at %s — review round %d next", ctl.Session, short(after), ctl.Round), nil)
		d.save(ctl)
	case status != "DONE" && moved:
		cfg.Emit("continue", ctl.Wave, fmt.Sprintf("builder %s committed to %s, more steps needed", ctl.Session, short(after)), nil)
		ctl.Session = nextSession(ctl.Session)
		d.save(ctl)
	default:
		ctl.Stalled++
		why := "no commits"
		if status == "DONE" {
			why = "DONE claimed with a still HEAD"
		}
		if ctl.Stalled >= cfg.Limits.MaxStalledSessions {
			d.stopFor(ctl, StHalted, "halt", fmt.Sprintf("sessions changed nothing: %d in a row (%s)", ctl.Stalled, why), nil)
			return
		}
		cfg.Emit("stall", ctl.Wave, fmt.Sprintf("builder %s: %s (%d/%d)", ctl.Session, why, ctl.Stalled, cfg.Limits.MaxStalledSessions), nil)
		ctl.Session = nextSession(ctl.Session)
		d.save(ctl)
	}
}

func (d *Driver) afterReviewer(ctl *Control, out Outcome, textPath, before, after string, total int, model string) {
	cfg, wt := d.cfg, d.cfg.WorktreePath()
	if before != after || trackedDirty(wt) {
		d.stopFor(ctl, StHalted, "halt", "the reviewer modified the build tree — a reviewer never writes code; inspect and reset before resuming", nil)
		return
	}
	verdict := parseVerdict(out.Text)
	block, heads := parseDefects(out.Text)
	switch verdict {
	case "VERIFIED":
		_ = cfg.AppendLedger(LedgerRow{Wave: ctl.Wave, SHA: after, VerifiedAt: time.Now().Format(time.RFC3339),
			Rounds: ctl.Round, Reviewer: model, Review: textPath, By: "reviewer"})
		cfg.Emit("verdict", ctl.Wave, fmt.Sprintf("VERIFIED at %s after %d round(s)", short(after), ctl.Round),
			map[string]any{"verdict": "VERIFIED", "sha": after, "review": textPath})
		d.advanceWave(ctl, after, total)
	case "REWORK":
		ctl.ReworkStreak++
		ctl.ReworkHistory = append(ctl.ReworkHistory, heads)
		ctl.Defects = block
		if ctl.Defects == "" {
			ctl.Defects = "(the reviewer gave no numbered defects — read its review:)\n" + out.Text
		}
		ctl.Role, ctl.Session, ctl.Stalled = "builder", nextSession(ctl.Session), 0
		cfg.Emit("verdict", ctl.Wave, fmt.Sprintf("REWORK, %d defect(s): %s", len(heads), strings.Join(heads, " | ")),
			map[string]any{"verdict": "REWORK", "defects": len(heads), "headlines": heads, "streak": ctl.ReworkStreak, "review": textPath})
		d.save(ctl)
	case "ESCALATE":
		d.stopFor(ctl, StNeedsOperator, "needs-operator", "reviewer escalated wave "+fmt.Sprint(ctl.Wave)+" — read "+textPath, map[string]any{"review": textPath})
	default:
		ctl.Stalled++
		if ctl.Stalled >= cfg.Limits.MaxStalledSessions {
			d.stopFor(ctl, StHalted, "halt", "reviewer returned no VERDICT line, repeatedly", nil)
			return
		}
		cfg.Emit("stall", ctl.Wave, "reviewer returned no VERDICT line — re-dispatching", nil)
		d.save(ctl)
	}
}

// advanceWave: VERIFIED → next wave; the last one signed ⇒ CLOSED.
func (d *Driver) advanceWave(ctl *Control, head string, total int) {
	ctl.Wave++
	ctl.Role, ctl.Session, ctl.Round = "builder", "a", 0
	ctl.ReworkStreak, ctl.ReworkHistory, ctl.Defects, ctl.LastHandoff = 0, nil, "", ""
	ctl.Stalled, ctl.Failures, ctl.WaveBase = 0, 0, head
	ctl.Sessions, ctl.Retries = 0, 0
	if ctl.Wave > total {
		ctl.Wave = total // keep the last signed wave on record
		d.stopFor(ctl, StClosed, "closed", fmt.Sprintf("all %d waves signed — final SHA %s; review the branch yourself", total, short(head)), nil)
		return
	}
	d.save(ctl)
}

func (d *Driver) handleFailure(ctl *Control, role, provName string, out Outcome, headMoved bool) {
	cfg := d.cfg
	switch out.Kind {
	case OutRateLimit:
		until := time.Now().Add(time.Duration(cfg.Limits.UnknownWaitSeconds) * time.Second)
		if out.ResetAt != nil {
			until = out.ResetAt.Add(time.Duration(cfg.Limits.ResetMarginSeconds) * time.Second)
		}
		d.markLimited(ctl, provName, until)
		cfg.Emit("wait", ctl.Wave, fmt.Sprintf("%s rate-limited mid-session until %s", provName, until.Format("15:04")), nil)
		if headMoved && role == "builder" {
			ctl.Session = nextSession(ctl.Session) // keep what was committed
		}
		d.save(ctl)
		return
	case OutAuth:
		d.markAuthRejected(ctl, provName)
		cfg.Emit("fallback", ctl.Wave, provName+" rejected our credentials: "+out.Detail, nil)
		d.save(ctl)
		return
	}
	// transient, timeout, idle, failed
	if headMoved && role == "builder" {
		// A killed session loses at most the step in flight; what it committed stands.
		ctl.Failures, ctl.Stalled = 0, 0
		ctl.Session = nextSession(ctl.Session)
		cfg.Emit("continue", ctl.Wave, fmt.Sprintf("builder session ended (%s) after committing — resuming from git log", out.Kind), nil)
		d.save(ctl)
		return
	}
	ctl.Failures++
	if ctl.Failures >= cfg.Limits.MaxSessionFailures {
		d.stopFor(ctl, StHalted, "halt", fmt.Sprintf("%d sessions in a row failed (%s: %s)", ctl.Failures, out.Kind, out.Detail), nil)
		return
	}
	d.retryLater(ctl, fmt.Sprintf("%s session %s: %s", role, out.Kind, out.Detail))
}

// resolveProvider picks the role's provider (or its fallback), skipping any
// known to be limited, and probes it. ok=false means the iteration is spent.
func (d *Driver) resolveProvider(ctx context.Context, ctl *Control, role string) (string, Provider, bool) {
	cfg := d.cfg
	r := cfg.Roles[role]
	cands := []string{r.Provider}
	if r.Fallback != "" && r.Fallback != r.Provider {
		cands = append(cands, r.Fallback)
	}
	var earliest time.Time
	for i, name := range cands {
		if until, ok := limitedUntil(ctl, name); ok {
			if earliest.IsZero() || until.Before(earliest) {
				earliest = until
			}
			continue
		}
		p := cfg.Providers[name]
		if p.Probe {
			o := ProbeProvider(ctx, p, cfg.WorktreePath(), time.Duration(cfg.Limits.ProbeTimeoutSeconds)*time.Second)
			switch o.Kind {
			case OutOK:
			case OutRateLimit:
				until := time.Now().Add(time.Duration(cfg.Limits.UnknownWaitSeconds) * time.Second)
				if o.ResetAt != nil {
					until = o.ResetAt.Add(time.Duration(cfg.Limits.ResetMarginSeconds) * time.Second)
				}
				d.markLimited(ctl, name, until)
				if earliest.IsZero() || until.Before(earliest) {
					earliest = until
				}
				continue
			case OutAuth:
				d.markAuthRejected(ctl, name)
				cfg.Emit("fallback", ctl.Wave, name+" rejected our credentials: "+o.Detail, nil)
				continue
			default:
				if i+1 < len(cands) {
					cfg.Emit("fallback", ctl.Wave, fmt.Sprintf("%s probe %s (%s) — trying %s", name, o.Kind, o.Detail, cands[i+1]), nil)
					continue
				}
				d.retryLater(ctl, fmt.Sprintf("provider %s probe: %s %s", name, o.Kind, o.Detail))
				return "", Provider{}, false
			}
		}
		if i > 0 {
			cfg.Emit("fallback", ctl.Wave, fmt.Sprintf("%s for %s: %s unavailable", name, role, cands[0]), nil)
		}
		return name, p, true
	}
	if allAuth(ctl, cands) {
		d.stopFor(ctl, StHalted, "halt", "every provider for "+role+" rejected our credentials", nil)
		return "", Provider{}, false
	}
	ctl.Status, ctl.WaitUntil = StWaiting, earliest.Format(time.RFC3339)
	ctl.Reason = "every provider for " + role + " is rate-limited"
	cfg.Emit("wait", ctl.Wave, fmt.Sprintf("%s — sleeping to %s", ctl.Reason, earliest.Format("15:04")), map[string]any{"until": ctl.WaitUntil})
	d.save(ctl)
	return "", Provider{}, false
}

// allAuth: every provider is parked *and* every one of them is parked for
// credentials. Never infer this from the length of the block — a weekly limit
// outlasts a day too, and halting the build over it would be a lie about why.
func allAuth(ctl *Control, names []string) bool {
	for _, n := range names {
		if _, ok := limitedUntil(ctl, n); !ok {
			return false
		}
		if _, bad := ctl.ProviderAuthFailed[n]; !bad {
			return false
		}
	}
	return true
}

func limitedUntil(ctl *Control, name string) (time.Time, bool) {
	s, ok := ctl.ProviderLimited[name]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || !time.Now().Before(t) {
		delete(ctl.ProviderLimited, name)
		delete(ctl.ProviderAuthFailed, name)
		return time.Time{}, false
	}
	return t, true
}

func (d *Driver) markLimited(ctl *Control, name string, until time.Time) {
	if ctl.ProviderLimited == nil {
		ctl.ProviderLimited = map[string]string{}
	}
	ctl.ProviderLimited[name] = until.Format(time.RFC3339)
	delete(ctl.ProviderAuthFailed, name)
}

// markAuthRejected parks a provider *and* records that waiting will not help.
func (d *Driver) markAuthRejected(ctl *Control, name string) {
	d.markLimited(ctl, name, time.Now().Add(24*time.Hour))
	if ctl.ProviderAuthFailed == nil {
		ctl.ProviderAuthFailed = map[string]string{}
	}
	ctl.ProviderAuthFailed[name] = time.Now().Format(time.RFC3339)
}

// preflight returns ("", false) when fit; fatal=true for problems the build
// itself caused and that will not fix themselves.
func (d *Driver) preflight(ctx context.Context, ctl *Control) (msg string, fatal bool) {
	cfg, wt := d.cfg, d.cfg.WorktreePath()
	if _, err := os.Stat(wt); err != nil {
		return "worktree missing: " + wt, false
	}
	if b := gitBranch(wt); b != cfg.Branch {
		return fmt.Sprintf("worktree is on %q, expected %q", b, cfg.Branch), false
	}
	if cfg.Limits.MinFreeDiskMB > 0 {
		var st syscall.Statfs_t
		if syscall.Statfs(wt, &st) == nil {
			if free := int(st.Bavail * uint64(st.Bsize) >> 20); free < cfg.Limits.MinFreeDiskMB {
				return fmt.Sprintf("only %d MB free, need %d", free, cfg.Limits.MinFreeDiskMB), false
			}
		}
	}
	if v := d.readonlyViolation(ctl); v != "" {
		return v, true
	}
	for _, c := range cfg.Preflight {
		if r := runShell(ctx, wt, c.Run, 2*time.Minute); r.Code != 0 {
			return fmt.Sprintf("%s failed (exit %d): %s", c.Name, r.Code, firstLine(strings.TrimSpace(r.Out))), false
		}
	}
	return "", false
}

func (d *Driver) readonlyViolation(ctl *Control) string {
	cfg, wt := d.cfg, d.cfg.WorktreePath()
	if len(cfg.ReadonlyPaths) == 0 || ctl.BaseSHA == "" {
		return ""
	}
	args := append([]string{"diff", "--name-only", ctl.BaseSHA, "--"}, cfg.ReadonlyPaths...)
	changed, _ := git(wt, args...)
	sargs := append([]string{"status", "--porcelain", "--"}, cfg.ReadonlyPaths...)
	dirty, _ := git(wt, sargs...)
	if s := strings.TrimSpace(changed + "\n" + dirty); s != "" {
		return "read-only paths modified: " + strings.ReplaceAll(firstLine(s), "\n", ", ")
	}
	return ""
}

type gateEntry struct {
	Pass bool   `json:"pass"`
	Text string `json:"text"`
	At   string `json:"at"`
}

// gate runs your checks. Cached per tree hash, so a reviewer round on an
// unchanged tree reuses the builder-side run instead of recomputing.
func (d *Driver) gate(ctx context.Context) string {
	cfg, wt := d.cfg, d.cfg.WorktreePath()
	cachePath := filepath.Join(cfg.stateDir, "gate-cache.json")
	cache := map[string]gateEntry{}
	_ = json.Unmarshal([]byte(readFileOr(cachePath, "{}")), &cache)
	// The cache is keyed on the exact tree, so a dirty worktree cannot use it:
	// untracked files change what the gate sees (a new .go file is compiled
	// whether or not it is staged), so skipping them would cache a lie. That
	// is correct but easy to hit by accident — a gate that compiles a binary
	// into the tree makes every later run uncacheable — so say why.
	key, why := "", ""
	if untracked, tracked := dirtyBreakdown(wt); tracked+untracked == 0 {
		key = gitTree(wt) + "|" + checksKey(cfg.Gate)
		if e, ok := cache[key]; ok {
			return e.Text + "\n(cached result for this exact tree)"
		}
	} else if tracked == 0 {
		why = fmt.Sprintf("\n(not cached: %d untracked file(s) in the tree. If they are build output, "+
			"adding them to .gitignore lets a review round reuse the builder's gate run.)", untracked)
	}
	var b strings.Builder
	pass := true
	for _, c := range cfg.Gate {
		r := runShell(ctx, wt, c.Run, 20*time.Minute)
		mark := "PASS"
		if r.Code != 0 {
			mark, pass = fmt.Sprintf("FAIL (exit %d)", r.Code), false
		}
		fmt.Fprintf(&b, "### %s — %s\n$ %s\n%s\n\n", c.Name, mark, c.Run, truncate(r.Out, cfg.Limits.GateOutputChars))
	}
	if len(cfg.Gate) == 0 {
		b.WriteString("(no gate configured)\n")
	}
	text := strings.TrimSpace(b.String()) + why
	if key != "" {
		cache[key] = gateEntry{Pass: pass, Text: text, At: time.Now().Format(time.RFC3339)}
		if len(cache) > 30 {
			var ks []string
			for k := range cache {
				ks = append(ks, k)
			}
			sort.Slice(ks, func(i, j int) bool { return cache[ks[i]].At < cache[ks[j]].At })
			for _, k := range ks[:len(ks)-30] {
				delete(cache, k)
			}
		}
		if bs, err := json.Marshal(cache); err == nil {
			_ = os.WriteFile(cachePath, bs, 0o644)
		}
	}
	return text
}

func checksKey(cs []Check) string {
	h := sha256.New()
	for _, c := range cs {
		h.Write([]byte(c.Name + "\x00" + c.Run + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// fingerprints: read-only samples of everything git cannot restore.
func (d *Driver) fingerprints(ctx context.Context) map[string]string {
	out := map[string]string{}
	for _, c := range d.cfg.Fingerprints {
		r := runShell(ctx, d.cfg.root, c.Run, time.Minute)
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", r.Code, r.Out)))
		out[c.Name] = hex.EncodeToString(sum[:])
	}
	return out
}

// fingerprintsMoved re-samples before believing a change.
func (d *Driver) fingerprintsMoved(ctx context.Context, before map[string]string) []string {
	if len(before) == 0 {
		return nil
	}
	after := d.fingerprints(ctx)
	var suspect []string
	for k, v := range before {
		if after[k] != v {
			suspect = append(suspect, k)
		}
	}
	if len(suspect) == 0 {
		return nil
	}
	time.Sleep(2 * time.Second)
	again := d.fingerprints(ctx)
	var moved []string
	for _, k := range suspect {
		if again[k] != before[k] {
			moved = append(moved, k)
		}
	}
	sort.Strings(moved)
	return moved
}

func (d *Driver) brief(ctl *Control, w Wave, total int, gate, head string) string {
	cfg, wt := d.cfg, d.cfg.WorktreePath()
	pd := PromptData{
		Worktree: wt, Branch: cfg.Branch, Wave: w, TotalWaves: total,
		Session: ctl.Session, Round: ctl.Round, ReworkStreak: ctl.ReworkStreak,
		Rework: ctl.ReworkStreak > 0 && ctl.Defects != "", Defects: ctl.Defects,
		PriorCommits: gitLogSince(wt, ctl.WaveBase, 60), Gate: gate,
		Rules: cfg.LoadRules(), ReadonlyPaths: cfg.ReadonlyPaths,
		Steps: cfg.Limits.StepsPerSession, OperatorNote: readFileOr(cfg.notePath(), ""),
		Head: head, WaveBase: ctl.WaveBase,
	}
	if pd.PriorCommits == "" {
		pd.PriorCommits = "(none yet)"
	}
	if ctl.LastHandoff != "" {
		pd.PrevHandoff = readFileOr(ctl.LastHandoff, "")
	}
	if ctl.Role == "reviewer" {
		return render("reviewer", pd)
	}
	return render("builder", pd)
}

// retryLater waits for something outside the build to get better. It says so
// once, so a slow disk does not fill the log — but it counts, because the
// thing it is waiting for may never come back, and an unattended build that
// spins forever in silence is indistinguishable from one that is working.
func (d *Driver) retryLater(ctl *Control, note string) {
	cfg := d.cfg
	if note != ctl.RetryNote {
		cfg.Emit("retry", ctl.Wave, note+fmt.Sprintf(" — retrying every %ds (said once)", cfg.Limits.RetryBackoffSeconds), nil)
		ctl.RetryNote, ctl.Retries = note, 0
	}
	ctl.Retries++
	if cfg.Limits.MaxRetriesBeforeOperator > 0 && ctl.Retries >= cfg.Limits.MaxRetriesBeforeOperator {
		d.stopFor(ctl, StNeedsOperator, "needs-operator",
			fmt.Sprintf("gave up retrying after %d rounds: %s", ctl.Retries, note),
			map[string]any{"retries": ctl.Retries, "note": note})
		return
	}
	d.save(ctl)
	cfg.sleepInterruptible(time.Duration(cfg.Limits.RetryBackoffSeconds) * time.Second)
}

func (d *Driver) stopFor(ctl *Control, status, ev, reason string, data map[string]any) {
	ctl.Status, ctl.Reason = status, reason
	d.cfg.Emit(ev, ctl.Wave, reason, data)
	d.save(ctl)
}

// consumeNote moves the operator's one-shot note next to the session that
// received it, so it is delivered exactly once and stays on record.
func (d *Driver) consumeNote(tag string) {
	if _, err := os.Stat(d.cfg.notePath()); err == nil {
		_ = os.Rename(d.cfg.notePath(), filepath.Join(d.cfg.sessionsDir(), tag+".note.md"))
	}
}

// save persists the control file. Only the driver writes it while a session
// runs; operator commands that act on a live build (note, wake) use their own
// files, and `resume` only acts while the driver is idling on a halt.
func (d *Driver) save(ctl *Control) {
	if err := d.cfg.SaveControl(ctl); err != nil {
		fmt.Fprintln(os.Stderr, "backcheck: saving control:", err)
	}
}

func trackedDirty(dir string) bool {
	s, err := git(dir, "status", "--porcelain", "--untracked-files=no")
	return err != nil || s != ""
}

func sessionTag(ctl *Control, role string) string {
	if role == "reviewer" {
		return fmt.Sprintf("w%02d-review-r%d", ctl.Wave, ctl.Round)
	}
	return fmt.Sprintf("w%02d-build-%s", ctl.Wave, ctl.Session)
}

// nextSession: a → b → … → z → aa → ab …
func nextSession(s string) string {
	if s == "" {
		return "a"
	}
	b := []byte(s)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 'z' {
			b[i]++
			return string(b)
		}
		b[i] = 'a'
	}
	return "a" + string(b)
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
