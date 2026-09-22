package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- harness

type env struct {
	t       *testing.T
	root    string // main repo
	mock    string // MOCK_DIR
	cfg     *Config
	drv     *Driver
	origDir string
}

type opt func(c map[string]any)

func withLimits(kv map[string]int) opt {
	return func(c map[string]any) {
		l := c["limits"].(map[string]any)
		for k, v := range kv {
			l[k] = v
		}
	}
}

func withFallback(role, prov string) opt {
	return func(c map[string]any) {
		r := c["roles"].(map[string]any)[role].(map[string]any)
		r["fallback"] = prov
	}
}

func waveText(n int) string {
	var b strings.Builder
	b.WriteString("# Waves\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "## Wave %d: part %d\nBuild part %d.\n### Acceptance criteria\n- `test -f work-w%d.txt` exits 0\n", i, i, i, i)
	}
	return b.String()
}

func setup(t *testing.T, waves int, builderQ, reviewerQ []string, opts ...opt) *env {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "app")
	mock := filepath.Join(base, "mock")
	os.MkdirAll(root, 0o755)
	os.MkdirAll(mock, 0o755)
	os.WriteFile(filepath.Join(mock, "builder.queue"), []byte(strings.Join(builderQ, "\n")+"\n"), 0o644)
	os.WriteFile(filepath.Join(mock, "reviewer.queue"), []byte(strings.Join(reviewerQ, "\n")+"\n"), 0o644)
	os.WriteFile(filepath.Join(mock, "external.txt"), []byte("stable\n"), 0o644)
	t.Setenv("MOCK_DIR", mock)

	mustGit(t, root, "init", "-q", "-b", "main")
	os.MkdirAll(filepath.Join(root, "protected"), 0o755)
	os.WriteFile(filepath.Join(root, "protected", "data.sql"), []byte("original\n"), 0o644)
	os.WriteFile(filepath.Join(root, "README.md"), []byte("app\n"), 0o644)
	mustGit(t, root, "add", "-A")
	mustGit(t, root, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init")

	orig, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(root)
	if err := cmdInit(nil); err != nil {
		t.Fatal(err)
	}

	script, _ := filepath.Abs(filepath.Join(orig, "testdata", "mockagent.sh"))
	prov := func(name string) map[string]any {
		return map[string]any{"command": []string{"bash", script}, "probe": true,
			"env": map[string]string{"MOCK_NAME": name}}
	}
	c := map[string]any{
		"worktree": "../app-backcheck",
		"branch":   "backcheck/build",
		"providers": map[string]any{
			"builderP": prov("builderP"), "judgeP": prov("judgeP"), "spareP": prov("spareP"),
		},
		"roles": map[string]any{
			"builder":  map[string]any{"provider": "builderP"},
			"reviewer": map[string]any{"provider": "judgeP"},
		},
		"gate":           []map[string]string{{"name": "ls", "run": "ls"}},
		"readonly_paths": []string{"protected/"},
		"fingerprints":   []map[string]string{{"name": "external", "run": "cat " + filepath.Join(mock, "external.txt")}},
		"limits": map[string]any{
			"session_wall_seconds": 20, "session_idle_seconds": 3, "probe_timeout_seconds": 10,
			"max_consecutive_rework": 3, "max_stalled_sessions": 2, "max_session_failures": 3,
			"retry_backoff_seconds": 1, "unknown_wait_seconds": 2, "reset_margin_seconds": 1,
			"poll_seconds": 1, "min_free_disk_mb": 0,
		},
	}
	for _, o := range opts {
		o(c)
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	os.WriteFile(filepath.Join(root, ".backcheck", "config.json"), b, 0o644)
	os.MkdirAll(filepath.Join(root, ".backcheck", "draft"), 0o755)
	os.WriteFile(filepath.Join(root, ".backcheck", "draft", "waves.md"), []byte(waveText(waves)), 0o644)
	os.WriteFile(filepath.Join(root, ".backcheck", "draft", "rules.md"), []byte("- paste real output\n"), 0o644)
	if err := cmdApprove([]string{"--yes"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, root: root, mock: mock, cfg: cfg, drv: &Driver{root: root, cfg: cfg}, origDir: orig}
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// runUntil iterates the driver until it reaches a stop status (or max turns).
func (e *env) runUntil(max int, stops ...string) *Control {
	e.t.Helper()
	if len(stops) == 0 {
		stops = []string{StClosed, StHalted, StNeedsOperator}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i := 0; i < max; i++ {
		if e.drv.Iterate(ctx) {
			break
		}
		ctl, _ := e.cfg.LoadControl()
		for _, s := range stops {
			if ctl.Status == s {
				return ctl
			}
		}
	}
	ctl, _ := e.cfg.LoadControl()
	return ctl
}

func (e *env) events() []Event {
	var out []Event
	for _, l := range strings.Split(strings.TrimSpace(readFileOr(e.cfg.eventsPath(), "")), "\n") {
		var ev Event
		if json.Unmarshal([]byte(l), &ev) == nil {
			out = append(out, ev)
		}
	}
	return out
}

func (e *env) hasEvent(name, contains string) bool {
	for _, ev := range e.events() {
		if ev.Event == name && strings.Contains(ev.Msg, contains) {
			return true
		}
	}
	return false
}

func (e *env) mockLog() string { return readFileOr(filepath.Join(e.mock, "log"), "") }

func expectStatus(t *testing.T, ctl *Control, want, reasonHas string) {
	t.Helper()
	if ctl.Status != want || !strings.Contains(ctl.Reason, reasonHas) {
		t.Fatalf("status = %s (%q), want %s containing %q", ctl.Status, ctl.Reason, want, reasonHas)
	}
}

// ---------------------------------------------------------------- end-to-end

func TestFullBuildWithReworkCloses(t *testing.T) {
	e := setup(t, 2,
		[]string{"commit-continue", "commit-done", "commit-done", "commit-done"},
		[]string{"rework", "verified", "verified"})
	ctl := e.runUntil(30)
	expectStatus(t, ctl, StClosed, "all 2 waves signed")

	led := e.cfg.Ledger()
	if len(led) != 2 || led[0].Wave != 1 || led[1].Wave != 2 || led[0].Rounds != 2 || led[0].Reviewer != "mock-model-1" {
		t.Fatalf("ledger = %+v", led)
	}
	wt := e.cfg.WorktreePath()
	if led[1].SHA != gitHead(wt) {
		t.Fatalf("ledger does not pin final HEAD")
	}
	// the defects reached the next builder, verbatim, numbered
	var fixPrompt string
	files, _ := filepath.Glob(filepath.Join(e.mock, "prompts", "*-builder.txt"))
	for _, f := range files {
		if s := readFileOr(f, ""); strings.Contains(s, "REWORK round 1") {
			fixPrompt = s
		}
	}
	if !strings.Contains(fixPrompt, "D1: tests assert nothing") || !strings.Contains(fixPrompt, "D2: missing error path") {
		t.Fatalf("rework brief missing defects:\n%s", fixPrompt)
	}
	// resume is derived from git: the second session of wave 1 saw the first commit
	if !strings.Contains(fixPrompt, "mock step") {
		t.Fatalf("builder brief did not list prior commits")
	}
	// every commit carries the wave trailer
	if n := commitsSince(wt, ctl.BaseSHA, 1); n != 3 {
		t.Fatalf("wave-1 trailer commits = %d, want 3", n)
	}
	// reviewer prompt got the gate result and the handoff
	rp, _ := filepath.Glob(filepath.Join(e.mock, "prompts", "*-reviewer.txt"))
	r0 := readFileOr(rp[0], "")
	if !strings.Contains(r0, "### ls — PASS") || !strings.Contains(r0, "STATUS: DONE") {
		t.Fatalf("reviewer brief missing gate or handoff:\n%s", r0)
	}
	if !e.hasEvent("closed", "") || !e.hasEvent("verdict", "REWORK, 2 defect(s)") {
		t.Fatal("expected closed + rework verdict events")
	}
	// the build never touched the main worktree's branch
	if b := gitBranch(e.root); b != "main" {
		t.Fatalf("main repo moved to %s", b)
	}
}

func TestSessionsThatChangeNothingHalt(t *testing.T) {
	e := setup(t, 1, []string{"nocommit-continue", "nocommit-continue"}, nil)
	ctl := e.runUntil(10)
	expectStatus(t, ctl, StHalted, "sessions changed nothing")
}

func TestDoneWithStillHeadIsAStall(t *testing.T) {
	e := setup(t, 1, []string{"nocommit-done", "nocommit-done"}, nil)
	ctl := e.runUntil(10)
	expectStatus(t, ctl, StHalted, "DONE claimed with a still HEAD")
	if strings.Contains(e.mockLog(), "reviewer") {
		t.Fatal("a reviewer was dispatched for an empty DONE")
	}
}

func TestNonConvergenceNeedsOperator(t *testing.T) {
	e := setup(t, 1,
		[]string{"commit-done", "commit-done", "commit-done"},
		[]string{"rework", "rework", "rework"})
	ctl := e.runUntil(20)
	expectStatus(t, ctl, StNeedsOperator, "3 consecutive REWORK")
	if len(ctl.ReworkHistory) != 3 || ctl.ReworkHistory[2][0] != "D1: tests assert nothing" {
		t.Fatalf("history = %v", ctl.ReworkHistory)
	}
	// operator resets the counter and resumes; the next round verifies
	os.WriteFile(filepath.Join(e.mock, "builder.queue"), []byte("commit-done\n"), 0o644)
	os.WriteFile(filepath.Join(e.mock, "reviewer.queue"), []byte("verified\n"), 0o644)
	if err := cmdResume([]string{"--because", "narrowed the wave", "--reset-rework"}); err != nil {
		t.Fatal(err)
	}
	ctl = e.runUntil(10)
	expectStatus(t, ctl, StClosed, "")
}

func TestEscalateThenOperatorAccepts(t *testing.T) {
	e := setup(t, 1, []string{"commit-done"}, []string{"escalate"})
	ctl := e.runUntil(10)
	expectStatus(t, ctl, StNeedsOperator, "reviewer escalated")
	if err := cmdResume([]string{"--because", "rule was wrong, work is fine", "--accept"}); err != nil {
		t.Fatal(err)
	}
	ctl, _ = e.cfg.LoadControl()
	expectStatus(t, ctl, StClosed, "")
	if led := e.cfg.Ledger(); len(led) != 1 || led[0].By != "operator" {
		t.Fatalf("ledger = %+v", led)
	}
}

func TestResumeRequiresReason(t *testing.T) {
	e := setup(t, 1, []string{"nocommit-continue", "nocommit-continue"}, nil)
	e.runUntil(10)
	if err := cmdResume(nil); err == nil {
		t.Fatal("resume without --because must fail")
	}
}

func TestFingerprintMoveHalts(t *testing.T) {
	e := setup(t, 1, []string{"touch-fp"}, nil)
	ctl := e.runUntil(10)
	expectStatus(t, ctl, StHalted, "fingerprint moved: external")
}

func TestReadonlyPathHalts(t *testing.T) {
	e := setup(t, 1, []string{"write-readonly"}, nil)
	ctl := e.runUntil(10)
	expectStatus(t, ctl, StHalted, "read-only paths modified")
}

func TestReviewerThatCommitsHalts(t *testing.T) {
	e := setup(t, 1, []string{"commit-done"}, []string{"commit"})
	ctl := e.runUntil(10)
	expectStatus(t, ctl, StHalted, "reviewer modified the build tree")
	if len(e.cfg.Ledger()) != 0 {
		t.Fatal("a tampering reviewer must not sign")
	}
}

func TestRateLimitMidSessionWaitsThenFinishes(t *testing.T) {
	e := setup(t, 1, []string{"ratelimit", "commit-done"}, []string{"verified"})
	ctl := e.runUntil(20, StClosed, StHalted, StNeedsOperator)
	expectStatus(t, ctl, StClosed, "")
	if !e.hasEvent("wait", "rate-limited") {
		t.Fatal("expected a wait event naming the reset")
	}
}

func TestProbeLimitFallsBackToSpare(t *testing.T) {
	e := setup(t, 1, []string{"commit-done"}, []string{"verified"}, withFallback("reviewer", "spareP"))
	os.WriteFile(filepath.Join(e.mock, "probe-fail-judgeP"), []byte("429 Too Many Requests. resets in 45 minutes"), 0o644)
	ctl := e.runUntil(10)
	expectStatus(t, ctl, StClosed, "")
	if !strings.Contains(e.mockLog(), "reviewer verified spareP") {
		t.Fatalf("reviewer did not run on the fallback:\n%s", e.mockLog())
	}
	if !e.hasEvent("fallback", "spareP for reviewer") {
		t.Fatal("expected fallback event")
	}
}

func TestAllProvidersLimitedWaitsAndWakeCutsItShort(t *testing.T) {
	e := setup(t, 1, []string{"commit-done"}, []string{"verified"})
	pf := filepath.Join(e.mock, "probe-fail-builderP")
	os.WriteFile(pf, []byte("usage limit reached, resets in 2 hours"), 0o644)
	ctl := e.runUntil(3, StWaiting)
	if ctl.Status != StWaiting {
		t.Fatalf("status %s, want WAITING", ctl.Status)
	}
	until, _ := time.Parse(time.RFC3339, ctl.WaitUntil)
	if d := time.Until(until); d < 110*time.Minute || d > 125*time.Minute {
		t.Fatalf("wait until %s is not ~2h out", ctl.WaitUntil)
	}
	// the operator fixed it: wake instead of sleeping two hours
	os.Remove(pf)
	if err := cmdWake(nil); err != nil {
		t.Fatal(err)
	}
	ctl = e.runUntil(10)
	expectStatus(t, ctl, StClosed, "")
}

func TestAuthFailureWithNoFallbackHalts(t *testing.T) {
	e := setup(t, 1, []string{"commit-done"}, nil)
	os.WriteFile(filepath.Join(e.mock, "probe-fail-builderP"), []byte(`{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`), 0o644)
	ctl := e.runUntil(5)
	expectStatus(t, ctl, StHalted, "rejected our credentials")
}

func TestIdleWatchdogThenRecovers(t *testing.T) {
	e := setup(t, 1, []string{"hang", "commit-done"}, []string{"verified"})
	ctl := e.runUntil(10)
	expectStatus(t, ctl, StClosed, "")
	if !e.hasEvent("retry", "idle") {
		t.Fatal("expected the idle watchdog to fire and be reported")
	}
}

func TestRepeatedCrashesHalt(t *testing.T) {
	e := setup(t, 1, []string{"crash", "crash", "crash"}, nil)
	ctl := e.runUntil(10)
	expectStatus(t, ctl, StHalted, "3 sessions in a row failed")
	// "retry later" is said once, not per attempt
	n := 0
	for _, ev := range e.events() {
		if ev.Event == "retry" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("retry said %d times, want once", n)
	}
}

func TestReviewerWithoutVerdictIsRetriedThenHalts(t *testing.T) {
	e := setup(t, 1, []string{"commit-done"}, []string{"noverdict", "noverdict"})
	ctl := e.runUntil(10)
	expectStatus(t, ctl, StHalted, "no VERDICT")
}

func TestOperatorNoteDeliveredOnce(t *testing.T) {
	e := setup(t, 1, []string{"commit-continue", "commit-done"}, []string{"verified"})
	if err := cmdNote([]string{"prefer", "table-driven", "tests"}); err != nil {
		t.Fatal(err)
	}
	e.runUntil(10)
	files, _ := filepath.Glob(filepath.Join(e.mock, "prompts", "*-builder.txt"))
	with := 0
	for _, f := range files {
		if strings.Contains(readFileOr(f, ""), "prefer table-driven tests") {
			with++
		}
	}
	if with != 1 {
		t.Fatalf("note delivered %d times, want 1", with)
	}
}

func TestRailsAmendedMidBuildAreReread(t *testing.T) {
	e := setup(t, 1, []string{"commit-continue", "commit-done"}, []string{"verified"})
	e.drv.Iterate(context.Background()) // first builder session
	os.WriteFile(e.cfg.rulesPath(), []byte("- NEW RULE: no globals\n"), 0o644)
	e.runUntil(10)
	files, _ := filepath.Glob(filepath.Join(e.mock, "prompts", "*-builder.txt"))
	found := false
	for _, f := range files {
		if strings.Contains(readFileOr(f, ""), "NEW RULE: no globals") {
			found = true
		}
	}
	if !found {
		t.Fatal("amended rules never reached a session")
	}
}

func TestGateIsCachedPerTree(t *testing.T) {
	e := setup(t, 1, nil, nil)
	counter := filepath.Join(e.mock, "gate-runs")
	raw := map[string]any{}
	json.Unmarshal([]byte(readFileOr(filepath.Join(e.root, ".backcheck", "config.json"), "")), &raw)
	raw["gate"] = []map[string]string{{"name": "count", "run": "echo run >> " + counter}}
	b, _ := json.Marshal(raw)
	os.WriteFile(filepath.Join(e.root, ".backcheck", "config.json"), b, 0o644)
	cfg, _ := LoadConfig(e.root)
	d := &Driver{root: e.root, cfg: cfg}
	d.gate(context.Background())
	out := d.gate(context.Background())
	if runs := strings.Count(readFileOr(counter, ""), "run"); runs != 1 || !strings.Contains(out, "cached") {
		t.Fatalf("gate ran %d times for one tree", runs)
	}
}

// ---------------------------------------------------------------- units

func TestParseReset(t *testing.T) {
	now := time.Date(2026, 9, 22, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"Claude AI usage limit reached|1790000000", time.Unix(1790000000, 0)},
		{"Retry-After: 120", now.Add(120 * time.Second)},
		{"try again in 45 minutes", now.Add(45 * time.Minute)},
		{"limit resets at 3pm", time.Date(2026, 9, 22, 15, 0, 0, 0, time.UTC)},
		{"limit resets 1:30 pm", time.Date(2026, 9, 23, 13, 30, 0, 0, time.UTC)},
		{"resets at 09:15", time.Date(2026, 9, 23, 9, 15, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got := parseReset(c.in, now)
		if got == nil || !got.Equal(c.want) {
			t.Errorf("%q → %v, want %v", c.in, got, c.want)
		}
	}
	if parseReset("rate limited", now) != nil {
		t.Error("no reset named → nil")
	}
}

// The wordings below are not invented: they are the templates the installed
// CLI builds its limit messages from (claude 2.1.280), with the limit names it
// uses for each window —
//
//	fh(name, M)  = "You've hit your ${name}${M}",  M = " · resets ${xc(resetsAt)}"
//	five_hour→"session limit"  seven_day→"weekly limit"  seven_day_opus→"Opus limit"
//	seven_day_sonnet→"Sonnet limit"  overage→"usage credit limit"
//
// and the two shapes its clock formatter produces: a bare time within 24h
// ("3am", "3:30am"), a month and day beyond that ("Sep 26, 1:20am"), each with
// the local timezone appended.
func TestParseResetRealCLIWordings(t *testing.T) {
	now := time.Date(2026, 9, 22, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"You've hit your session limit · resets 3pm (Asia/Dhaka)", time.Date(2026, 9, 22, 15, 0, 0, 0, time.UTC)},
		{"You've hit your session limit · resets 3:30pm (Asia/Dhaka)", time.Date(2026, 9, 22, 15, 30, 0, 0, time.UTC)},
		{"You've used 90% of your session limit · resets 11am (UTC)", time.Date(2026, 9, 23, 11, 0, 0, 0, time.UTC)},
		// beyond 24h the CLI names the date — every weekly limit looks like this
		{"You've hit your weekly limit · resets Sep 26, 1:20am (Asia/Dhaka)", time.Date(2026, 9, 26, 1, 20, 0, 0, time.UTC)},
		{"You've hit your Opus limit · resets Oct 28, 2027, 1:20am (UTC)", time.Date(2027, 10, 28, 1, 20, 0, 0, time.UTC)},
		{"Approaching weekly limit · resets Sep 26, 3pm", time.Date(2026, 9, 26, 15, 0, 0, 0, time.UTC)},
		// a month already past means next year, not nine months ago
		{"You've hit your weekly limit · resets Jan 4, 9am", time.Date(2027, 1, 4, 9, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got := parseReset(c.in, now)
		if got == nil || !got.Equal(c.want) {
			t.Errorf("%q → %v, want %v", c.in, got, c.want)
		}
	}
}

// Every real wording must land on rate-limit: anything else spends the session
// failure budget and halts the build instead of waiting for the reset.
func TestClassifyRealCLILimitWordings(t *testing.T) {
	limits := []string{
		"You've hit your session limit · resets 3pm (Asia/Dhaka)",
		"You've hit your weekly limit · resets Sep 26, 1:20am (Asia/Dhaka)",
		"You've hit your Opus limit · resets Sep 26, 1:20am (UTC)",
		"You've hit your Sonnet limit · resets 3pm",
		"You've hit your Fable limit · resets 3pm",
		"You've hit your usage credit limit · contact your admin to increase it",
		"You've hit your team's shared budget · your weekly limit resets Sep 26, 3pm",
		"You've used 90% of your session limit · resets 3:30pm",
		"Approaching weekly limit · resets Sep 26, 1:20am",
		"spend limit reached (monthly; resets 2026-10-01 00:00 UTC)",
		"Claude AI usage limit reached|1790000000",
		"429 Too Many Requests",
	}
	for _, s := range limits {
		if got, _ := classifyText(s, time.Now()); got != OutRateLimit {
			t.Errorf("%q → %q, want %q", s, got, OutRateLimit)
		}
	}

	// …and the limits that no amount of waiting clears must not look like one.
	// "limit reached" alone is too broad to be trusted.
	notLimits := []string{
		"Context limit reached · 200k tokens",
		"Concurrent subagent limit reached. You can run 3 subagents at once.",
		"deviceRegistry: account device limit reached",
	}
	for _, s := range notLimits {
		if got, _ := classifyText(s, time.Now()); got == OutRateLimit {
			t.Errorf("%q → %q, must not be treated as a limit that resets", s, got)
		}
	}
}

func TestClassifyText(t *testing.T) {
	for in, want := range map[string]OutcomeKind{
		"429 Too Many Requests":                   OutRateLimit,
		"API Error: 529 Overloaded":               OutTransient,
		"authentication_error: invalid x-api-key": OutAuth,
		"Credit balance is too low":               OutAuth,
		"ECONNRESET":                              OutTransient,
		"something odd":                           "",
	} {
		if got, _ := classifyText(in, time.Now()); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestParseVerdictUsesLastLine(t *testing.T) {
	s := "Example output:\nVERDICT: VERIFIED\n...\n### Defects\nD1: broken — x.go — fails\n**VERDICT: REWORK**"
	if v := parseVerdict(s); v != "REWORK" {
		t.Fatalf("verdict %q", v)
	}
	block, heads := parseDefects(s)
	if len(heads) != 1 || heads[0] != "D1: broken" || !strings.Contains(block, "x.go") {
		t.Fatalf("defects %q %v", block, heads)
	}
	if parseStatus("STATUS: DONE\nactually\nSTATUS: CONTINUE") != "CONTINUE" {
		t.Fatal("status must use the last line")
	}
}

func TestParseWaves(t *testing.T) {
	ws, err := ParseWaves(waveText(3))
	if err != nil || len(ws) != 3 || ws[2].Title != "part 3" {
		t.Fatalf("%v %+v", err, ws)
	}
	if _, err := ParseWaves("## Wave 1: a\n### Acceptance criteria\n- x\n## Wave 3: c\n### Acceptance criteria\n- y"); err == nil {
		t.Fatal("gap in numbering must fail")
	}
	if _, err := ParseWaves("## Wave 1: a\nno criteria"); err == nil {
		t.Fatal("missing acceptance criteria must fail")
	}
}

func TestPlannerFiles(t *testing.T) {
	s := "blah\n<<<FILE waves.md>>>\n# Waves\n## Wave 1: x\n<<<END>>>\n<<<FILE rails.json>>>\n{\"gate\":[]}\n<<<END>>>"
	f := parsePlannerFiles(s)
	if !strings.HasPrefix(f["waves.md"], "# Waves") || strings.TrimSpace(f["rails.json"]) != `{"gate":[]}` {
		t.Fatalf("%q", f)
	}
}

func TestNextSession(t *testing.T) {
	for in, want := range map[string]string{"": "a", "a": "b", "z": "aa", "az": "ba"} {
		if got := nextSession(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestPromptsRender(t *testing.T) {
	w := Wave{N: 2, Title: "t", Body: "body\n### Acceptance criteria\n- x"}
	b := render("builder", PromptData{Wave: w, TotalWaves: 3, Rework: true, ReworkStreak: 1, Defects: "D1: x", Steps: "5 to 8"})
	for _, must := range []string{"ROLE: BUILDER", "BackCheck-Wave: 2", "D1: x", "STATUS: DONE"} {
		if !strings.Contains(b, must) {
			t.Errorf("builder prompt missing %q", must)
		}
	}
	r := render("reviewer", PromptData{Wave: w, TotalWaves: 3, WaveBase: "abc"})
	for _, must := range []string{"ROLE: REVIEWER", "git diff abc..HEAD", "VERDICT: VERIFIED"} {
		if !strings.Contains(r, must) {
			t.Errorf("reviewer prompt missing %q", must)
		}
	}
}

func replayFixture(t *testing.T, stem string, wall time.Duration) Outcome {
	t.Helper()
	jsonl := filepath.Join("testdata", stem+".jsonl")
	if _, err := os.Stat(jsonl); err != nil {
		t.Skipf("%s missing — re-capture it from the installed CLI", jsonl)
	}
	// stderr is replayed too: some failures only ever show up there.
	script := fmt.Sprintf("cat %q; cat %q >&2 2>/dev/null; true", jsonl, filepath.Join("testdata", stem+".stderr"))
	p := Provider{Command: []string{"bash", "-c", script}}
	return Dispatch(context.Background(), p, "(prompt is ignored by the replay)", ".", wall, wall, "")
}

func TestDispatchRealFixtures(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		o := replayFixture(t, "real-ok", 30*time.Second)
		if o.Kind != OutOK {
			t.Fatalf("Kind = %q (%s), want %q", o.Kind, o.Detail, OutOK)
		}
		if o.Model == "" || !strings.Contains(o.Model, "claude") {
			t.Errorf("Model = %q, want the model from the system/init line", o.Model)
		}
		if o.Text != "OK" {
			t.Errorf("Text = %q, want %q", o.Text, "OK")
		}
		if o.Cost <= 0 {
			t.Errorf("Cost = %v, want total_cost_usd off the result line", o.Cost)
		}
	})

	t.Run("auth", func(t *testing.T) {
		o := replayFixture(t, "real-auth", 30*time.Second)
		if o.Kind != OutAuth {
			t.Fatalf("Kind = %q (%s), want %q — a bad key must never look transient", o.Kind, o.Detail, OutAuth)
		}
	})
}

// The CLI does not surface an HTTP failure as a result: it retries internally,
// ten times with growing backoff, saying so only on system/api_retry lines. A
// probe times out long before that budget runs out, so without reading those
// lines a dead key is indistinguishable from a slow session and the build
// retries it forever instead of falling back.
func TestAuthDuringCLIRetriesBeatsTheWatchdog(t *testing.T) {
	e := setup(t, 1, []string{"auth-retry-hang"}, nil, withFallback("builder", "spareP"))
	os.WriteFile(filepath.Join(e.mock, "builder.queue"), []byte("auth-retry-hang\ncommit-done\n"), 0o644)
	os.WriteFile(filepath.Join(e.mock, "reviewer.queue"), []byte("verified\n"), 0o644)
	ctl := e.runUntil(10)
	expectStatus(t, ctl, StClosed, "")
	if !e.hasEvent("fallback", "rejected our credentials") {
		t.Fatalf("expected builderP to be called out as auth-failed, events: %+v", e.events())
	}
	if !strings.Contains(e.mockLog(), "spareP") {
		t.Fatalf("expected the fallback provider to run the wave, log:\n%s", e.mockLog())
	}
}

// Same shape, but a 429: the session must wait, not burn a failure budget.
func TestRateLimitDuringCLIRetriesBeatsTheWatchdog(t *testing.T) {
	e := setup(t, 1, []string{"rate-retry-hang", "commit-done"}, []string{"verified"})
	ctl := e.runUntil(12)
	expectStatus(t, ctl, StClosed, "")
	if !e.hasEvent("wait", "rate-limited mid-session") {
		t.Fatalf("expected a rate-limit wait, events: %+v", e.events())
	}
}

// A limit reported as data: rate_limit_event carries an exact unix reset, so
// the build waits to that moment rather than guessing unknown_wait_seconds.
// The prose on the same session names only the window ("session limit"), which
// is why the structured line is read first.
func TestStructuredRateLimitEventWaitsToTheExactReset(t *testing.T) {
	e := setup(t, 1, []string{"limit-event", "commit-done"}, []string{"verified"})
	ctl := e.runUntil(20, StClosed, StHalted, StNeedsOperator)
	expectStatus(t, ctl, StClosed, "")
	if !e.hasEvent("wait", "rate-limited mid-session") {
		t.Fatalf("expected a wait on the structured limit, events: %+v", e.events())
	}
}

// The same limit with no structured event: prose alone must still be enough.
// Before this, "You've hit your session limit" matched none of the rate-limit
// patterns and the wave burned its failure budget into a HALT.
func TestProseOnlyLimitStillWaits(t *testing.T) {
	e := setup(t, 1, []string{"limit-prose", "commit-done"}, []string{"verified"},
		withLimits(map[string]int{"max_session_failures": 2}))
	ctl := e.runUntil(20, StClosed, StHalted, StNeedsOperator)
	expectStatus(t, ctl, StClosed, "")
	if !e.hasEvent("wait", "rate-limited mid-session") {
		t.Fatalf("expected the prose limit to be read as a limit, events: %+v", e.events())
	}
}

// A weekly limit parks a provider for days. That used to be indistinguishable
// from a dead key, because "is this auth?" was answered by measuring how long
// the block had left to run — so the build halted saying our credentials were
// rejected, which was both wrong and the wrong thing to wake someone up for.
func TestWeeklyLimitIsNotMistakenForRejectedCredentials(t *testing.T) {
	e := setup(t, 1, []string{"limit-weekly", "commit-done"}, []string{"verified"})
	ctl := e.runUntil(6, StWaiting, StClosed, StHalted, StNeedsOperator)
	if ctl.Status != StWaiting {
		t.Fatalf("status = %s (%q), want WAITING on the named reset", ctl.Status, ctl.Reason)
	}
	if strings.Contains(ctl.Reason, "credentials") {
		t.Fatalf("a limit that resets was reported as an auth failure: %q", ctl.Reason)
	}
	until, err := time.Parse(time.RFC3339, ctl.WaitUntil)
	if err != nil || time.Until(until) < 47*time.Hour {
		t.Fatalf("wait_until = %q, want the reset days out that the CLI named", ctl.WaitUntil)
	}
	if len(ctl.ProviderAuthFailed) != 0 {
		t.Fatalf("no provider rejected our credentials, but %v is recorded as having done so", ctl.ProviderAuthFailed)
	}
}
func TestRateLimitEventAllowedIsNotALimit(t *testing.T) {
	reset := time.Now().Add(90 * time.Minute).Unix()
	for _, tc := range []struct {
		status string
		want   OutcomeKind
	}{
		{"allowed", OutOK},
		{"allowed_warning", OutOK},
		{"rejected", OutRateLimit},
	} {
		script := fmt.Sprintf(
			`printf '{"type":"system","subtype":"init","model":"m"}\n'
			 printf '{"type":"rate_limit_event","rate_limit_info":{"status":"%s","resetsAt":%d,"rateLimitType":"five_hour"}}\n'
			 printf '{"type":"result","subtype":"success","is_error":%t,"result":"done","total_cost_usd":0.01}\n'`,
			tc.status, reset, tc.status == "rejected")
		o := Dispatch(context.Background(), Provider{Command: []string{"bash", "-c", script}},
			"p", ".", 20*time.Second, 20*time.Second, "")
		if o.Kind != tc.want {
			t.Errorf("status %q → %q (%s), want %q", tc.status, o.Kind, o.Detail, tc.want)
		}
		if tc.want == OutRateLimit {
			if o.ResetAt == nil || o.ResetAt.Unix() != reset {
				t.Errorf("status %q → reset %v, want the exact resetsAt %d", tc.status, o.ResetAt, reset)
			}
		}
	}
}

// ---------------------------------------------------------------- providers

// Two providers, two names, two models — and one account. SingleProvider only
// compares the names in config.json, so this configuration looks correct while
// the property it exists to buy is half gone: one quota pool, one outage.
// Worth catching, because a logged-in CLI ignores ANTHROPIC_API_KEY outright,
// so "give the reviewer its own key" can silently do nothing at all.
func TestSharedAccountIsDetectedAcrossProviderNames(t *testing.T) {
	claude := []string{"claude", "-p", "--output-format", "stream-json"}
	cfg := func(builder, reviewer Provider) *Config {
		return &Config{
			Providers: map[string]Provider{"b": builder, "r": reviewer},
			Roles:     map[string]Role{"builder": {Provider: "b"}, "reviewer": {Provider: "r"}},
		}
	}

	// same binary, same endpoint, no credentials of its own — only the model differs
	sonnet := Provider{Command: claude, Model: "sonnet", Env: map[string]string{"ANTHROPIC_BASE_URL": "https://api.anthropic.com"}}
	opus := Provider{Command: claude, Model: "opus", Env: map[string]string{"ANTHROPIC_BASE_URL": "https://api.anthropic.com"}}
	if c := cfg(sonnet, opus); !c.SharedAccount() {
		t.Error("two models on one login must be reported as one account")
	}
	if c := cfg(sonnet, opus); c.SingleProvider() {
		t.Error("they are distinct providers; SingleProvider is the wrong warning here")
	}

	// a genuinely separate endpoint is a genuinely separate account
	elsewhere := opus
	elsewhere.Env = map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.example.invalid"}
	if c := cfg(sonnet, elsewhere); c.SharedAccount() {
		t.Error("different endpoints must not be reported as the same account")
	}

	// so is the same endpoint reached with a different key
	t.Setenv("BACKCHECK_TEST_JUDGE_KEY", "sk-second-account")
	keyed := opus
	keyed.EnvFrom = map[string]string{"ANTHROPIC_AUTH_TOKEN": "BACKCHECK_TEST_JUDGE_KEY"}
	if c := cfg(sonnet, keyed); c.SharedAccount() {
		t.Error("a second credential must not be reported as the same account")
	}

	// …but only while that variable actually holds something
	t.Setenv("BACKCHECK_TEST_JUDGE_KEY", "")
	if c := cfg(sonnet, keyed); !c.SharedAccount() {
		t.Error("env_from pointing at an unset variable buys no separation, and must not look like it does")
	}

	// the fingerprint is a hash, never the credential itself
	if fp := keyed.accountFingerprint(); strings.Contains(fp, "sk-") {
		t.Errorf("accountFingerprint leaked a credential: %q", fp)
	}
}

// The two shapes of a shared account are not equally bad, and the warning must
// not claim a difference that is not there: accountFingerprint ignores the
// model on purpose, so the same model on both sides looks identical to it.
func TestSharedAccountWarningDistinguishesSameModel(t *testing.T) {
	claude := []string{"claude", "-p"}
	cfg := func(bm, rm string) *Config {
		return &Config{
			Providers: map[string]Provider{
				"b": {Command: claude, Model: bm},
				"r": {Command: claude, Model: rm},
			},
			Roles: map[string]Role{"builder": {Provider: "b"}, "reviewer": {Provider: "r"}},
		}
	}
	two := cfg("sonnet", "opus")
	if !two.SharedAccount() {
		t.Fatal("same account expected")
	}
	if got := two.sharedAccountWarning(); !strings.Contains(got, "Only their models differ") {
		t.Errorf("two models → %q", got)
	}

	one := cfg("sonnet", "sonnet")
	if !one.SharedAccount() {
		t.Fatal("same account expected")
	}
	got := one.sharedAccountWarning()
	if strings.Contains(got, "Only their models differ") {
		t.Errorf("the models do not differ, but the warning says they do: %q", got)
	}
	if !strings.Contains(got, "no second opinion") {
		t.Errorf("same model → %q, want it to say what is actually lost", got)
	}
	// and an unset model on both sides is the same model, not two
	if strings.Contains(cfg("", "").sharedAccountWarning(), "Only their models differ") {
		t.Error("two providers with no model set are running the same default model")
	}
}

// env_from naming an unset variable used to inject "VAR=", which does not fall
// through to the ambient value — it blanks it. A typo would strip the very
// credential the provider was configured to use.
func TestEnvFromUnsetDoesNotBlankTheAmbientValue(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-token")
	p := Provider{Command: []string{"claude"}, EnvFrom: map[string]string{"ANTHROPIC_AUTH_TOKEN": "BACKCHECK_TEST_UNSET_KEY"}}
	if got := lastEnvValue(providerEnv(p), "ANTHROPIC_AUTH_TOKEN"); got != "ambient-token" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q, want the ambient value left alone", got)
	}
	t.Setenv("BACKCHECK_TEST_UNSET_KEY", "explicit-token")
	if got := lastEnvValue(providerEnv(p), "ANTHROPIC_AUTH_TOKEN"); got != "explicit-token" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q, want env_from to win once it is set", got)
	}
}

// ---------------------------------------------------------------- notify

// The alert carries the event and message as its last two arguments, and the
// same values in the environment, so `notify-send -a backcheck` works with no
// wrapper and a wrapper script can read them without parsing argv.
func TestNotifyPassesEventAndMessageBothWays(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "alert")
	script := filepath.Join(dir, "notify.sh")
	os.WriteFile(script, []byte("#!/usr/bin/env bash\n"+
		"printf 'argv:%s\\n' \"$*\" > \""+out+"\"\n"+
		"printf 'BACKCHECK_EVENT=%s\\nBACKCHECK_MSG=%s\\nBACKCHECK_WAVE=%s\\n' "+
		"\"$BACKCHECK_EVENT\" \"$BACKCHECK_MSG\" \"$BACKCHECK_WAVE\" >> \""+out+"\"\n"), 0o755)

	cfg := &Config{Notify: Notify{Command: []string{"bash", script, "-a", "backcheck"}}}
	if err := cfg.Notify1("halt", 2, "the reviewer modified the build tree", 15*time.Second); err != nil {
		t.Fatalf("notify: %v", err)
	}
	got := readFileOr(out, "")
	for _, want := range []string{
		"argv:-a backcheck halt the reviewer modified the build tree",
		"BACKCHECK_EVENT=halt",
		"BACKCHECK_MSG=the reviewer modified the build tree",
		"BACKCHECK_WAVE=2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("alert missing %q, got:\n%s", want, got)
		}
	}
}

// A notify command that is missing or fails must not stop the build — but it
// must not be swallowed either, or the HALT it was meant to announce is the
// one nobody ever hears about.
func TestNotifyFailureIsReportedButNotFatal(t *testing.T) {
	cfg := &Config{Notify: Notify{Command: []string{"backcheck-no-such-notifier"}}}
	if err := cfg.Notify1("halt", 1, "x", 15*time.Second); err == nil {
		t.Error("a missing notify command must be reported")
	}
	cfg = &Config{Notify: Notify{Command: []string{"bash", "-c", "exit 3"}}}
	if err := cfg.Notify1("halt", 1, "x", 15*time.Second); err == nil {
		t.Error("a notify command that exits non-zero must be reported")
	}
	// nothing configured is a choice, not a failure
	if err := (&Config{}).Notify1("halt", 1, "x", time.Second); err != nil {
		t.Errorf("no notify command configured → %v, want nil", err)
	}
}

// Only the events the operator asked to be woken for fire an alert.
func TestOnlyHumanEventsAlert(t *testing.T) {
	e := setup(t, 1, []string{"write-readonly"}, nil)
	dir := t.TempDir()
	log := filepath.Join(dir, "alerts")
	script := filepath.Join(dir, "notify.sh")
	os.WriteFile(script, []byte("#!/usr/bin/env bash\necho \"$BACKCHECK_EVENT\" >> \""+log+"\"\n"), 0o755)

	raw := map[string]any{}
	json.Unmarshal([]byte(readFileOr(filepath.Join(e.root, ".backcheck", "config.json"), "{}")), &raw)
	raw["notify"] = map[string]any{
		"command":      []string{"bash", script},
		"human_events": []string{"halt"},
	}
	b, _ := json.MarshalIndent(raw, "", "  ")
	os.WriteFile(filepath.Join(e.root, ".backcheck", "config.json"), b, 0o644)

	ctl := e.runUntil(6)
	expectStatus(t, ctl, StHalted, "read-only paths modified")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(readFileOr(log, ""), "halt") {
		time.Sleep(100 * time.Millisecond)
	}
	got := readFileOr(log, "")
	if !strings.Contains(got, "halt") {
		t.Fatalf("the HALT did not raise an alert, log:\n%s", got)
	}
	for _, quiet := range []string{"dispatch", "approved", "start"} {
		if strings.Contains(got, quiet) {
			t.Errorf("%q is not a human event but raised an alert:\n%s", quiet, got)
		}
	}
}

// ---------------------------------------------------------------- task 7 gaps

// A wave that never finishes must reach a human rather than spend forever.
func TestSessionCapPerWaveNeedsOperator(t *testing.T) {
	q := make([]string, 12)
	for i := range q {
		q[i] = "commit-continue" // always commits, so it never stalls or fails
	}
	e := setup(t, 1, q, nil, withLimits(map[string]int{"max_sessions_per_wave": 4}))
	ctl := e.runUntil(20)
	expectStatus(t, ctl, StNeedsOperator, "sessions spent and still not signed")
	if ctl.Sessions != 4 {
		t.Errorf("sessions = %d, want the cap of 4", ctl.Sessions)
	}
	// and the operator's resume gives the wave a fresh budget, or the stop
	// simply fires again on the next iteration
	os.Chdir(e.root)
	if err := cmdResume([]string{"--because", "split the wave"}); err != nil {
		t.Fatal(err)
	}
	ctl, _ = e.cfg.LoadControl()
	if ctl.Sessions != 0 || ctl.Status != StRunning {
		t.Fatalf("after resume: status=%s sessions=%d, want RUNNING with a fresh budget", ctl.Status, ctl.Sessions)
	}
}

// "retry later" used to be unbounded: one log line, then silence forever.
func TestRetryLaterEscalatesToOperator(t *testing.T) {
	e := setup(t, 1, []string{"commit-done"}, []string{"verified"},
		withLimits(map[string]int{"max_retries_before_operator": 3, "retry_backoff_seconds": 1}))
	// a preflight that cannot pass is exactly the shape that used to spin
	raw := map[string]any{}
	json.Unmarshal([]byte(readFileOr(filepath.Join(e.root, ".backcheck", "config.json"), "{}")), &raw)
	raw["preflight"] = []map[string]string{{"name": "never", "run": "exit 1"}}
	b, _ := json.MarshalIndent(raw, "", "  ")
	os.WriteFile(filepath.Join(e.root, ".backcheck", "config.json"), b, 0o644)

	ctl := e.runUntil(10)
	expectStatus(t, ctl, StNeedsOperator, "gave up retrying")
	if ctl.Retries != 3 {
		t.Errorf("retries = %d, want the cap of 3", ctl.Retries)
	}
	if !e.hasEvent("retry", "said once") {
		t.Error("the reason should still be logged once on the way there")
	}
}

// Every dispatch reports what it cost, and status adds them up. The event log
// is the durable record: control.json is rewritten and streams can be pruned.
func TestDispatchResultCarriesCostAndStatusTotalsIt(t *testing.T) {
	e := setup(t, 1, []string{"commit-done"}, []string{"verified"})
	ctl := e.runUntil(10)
	expectStatus(t, ctl, StClosed, "")

	var got []Event
	for _, ev := range e.events() {
		if ev.Event == "dispatch-result" {
			got = append(got, ev)
		}
	}
	if len(got) != 2 {
		t.Fatalf("dispatch-result events = %d, want one per session (builder + reviewer)", len(got))
	}
	for _, ev := range got {
		if c, _ := ev.Data["cost_usd"].(float64); c <= 0 {
			t.Errorf("%s has no cost_usd: %+v", ev.Msg, ev.Data)
		}
		if ev.Data["role"] == nil || ev.Data["kind"] == nil || ev.Data["model"] == nil {
			t.Errorf("dispatch-result is missing a field the watcher reads: %+v", ev.Data)
		}
	}
	sp := e.cfg.Spend()
	if sp.Sessions != 2 || sp.USD <= 0 {
		t.Fatalf("Spend() = %+v, want two sessions and a positive total", sp)
	}
	if sp.ByRole["builder"] <= 0 || sp.ByRole["reviewer"] <= 0 {
		t.Errorf("Spend().ByRole = %v, want both roles", sp.ByRole)
	}
}

// status --json is the watcher's structured view: keys may be added, never
// renamed or dropped. This is the contract, written down.
func TestStatusJSONKeysAreStable(t *testing.T) {
	e := setup(t, 1, []string{"commit-done"}, []string{"verified"})
	e.runUntil(10)
	os.Chdir(e.root)

	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	err := cmdStatus([]string{"--json"})
	w.Close()
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	io.Copy(&buf, r)

	var got map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &got); err != nil {
		t.Fatalf("status --json is not valid JSON: %v\n%s", err, buf.String())
	}
	for _, k := range []string{"control", "ledger", "driver_alive", "head", "waves", "spend"} {
		if _, ok := got[k]; !ok {
			t.Errorf("status --json lost the key %q — the watcher reads it", k)
		}
	}
	ctl, _ := got["control"].(map[string]any)
	for _, k := range []string{"status", "wave", "role", "session", "sessions_this_wave", "retries"} {
		if _, ok := ctl[k]; !ok {
			t.Errorf("control is missing %q", k)
		}
	}
}

// squash builds a second view of the same trees. The build branch, which the
// ledger pins and the reviews refer to, must come out of it untouched.
func TestSquashExportsWavesWithoutTouchingTheBuildBranch(t *testing.T) {
	e := setup(t, 2,
		[]string{"commit-continue", "commit-done", "commit-continue", "commit-done"},
		[]string{"verified", "verified"})
	ctl := e.runUntil(30)
	expectStatus(t, ctl, StClosed, "")

	wt := e.cfg.WorktreePath()
	buildHead := gitHead(wt)
	buildLog := mustGit(t, wt, "log", "--oneline", "backcheck/build")
	nBuild := len(strings.Split(strings.TrimSpace(buildLog), "\n"))
	if nBuild < 4 {
		t.Fatalf("expected several build commits to collapse, got:\n%s", buildLog)
	}

	os.Chdir(e.root)
	if err := cmdSquash([]string{"--branch", "backcheck/export"}); err != nil {
		t.Fatal(err)
	}

	// the build branch is byte-for-byte where it was
	if got := gitHead(wt); got != buildHead {
		t.Fatalf("squash moved the build branch: %s → %s", short(buildHead), short(got))
	}
	if got := mustGit(t, wt, "log", "--oneline", "backcheck/build"); got != buildLog {
		t.Fatalf("squash rewrote the build branch history")
	}

	// one export commit per verified wave, each with the wave's exact tree
	exp := strings.Split(strings.TrimSpace(mustGit(t, wt, "log", "--format=%H", "backcheck/export", "--not", ctl.BaseSHA)), "\n")
	if len(exp) != 2 {
		t.Fatalf("export has %d commit(s), want one per verified wave", len(exp))
	}
	led := e.cfg.Ledger()
	for i, row := range led {
		want := mustGit(t, wt, "rev-parse", row.SHA+"^{tree}")
		got := mustGit(t, wt, "rev-parse", exp[len(exp)-1-i]+"^{tree}")
		if got != want {
			t.Errorf("wave %d: export tree %s, want the verified tree %s", row.Wave, short(got), short(want))
		}
		tr := mustGit(t, wt, "log", "-1", "--format=%(trailers:key=BackCheck-Wave,valueonly)", exp[len(exp)-1-i])
		if strings.TrimSpace(tr) != fmt.Sprint(row.Wave) {
			t.Errorf("wave %d: export commit trailer = %q", row.Wave, strings.TrimSpace(tr))
		}
	}
	// and it refuses to aim at the build branch
	if err := cmdSquash([]string{"--branch", "backcheck/build"}); err == nil {
		t.Error("squash must refuse to write the build branch")
	}
}

// ---------------------------------------------------------------- rails lint

// The lint exists because of rails a real planner really drafted: both of
// these would have trapped an unattended build, and both read as sensible.
func TestLintRailsCatchesTheTrapsAPlannerDrafted(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "README.md"), []byte("x"), 0o644)

	rails := railsFile{
		Gate: []Check{{Name: "test", Run: "go test ./..."}},
		Preflight: []Check{
			{Name: "ok", Run: "go version"},
			// a builder killed mid-edit leaves the tree dirty, and this never passes again
			{Name: "no-uncommitted-changes", Run: `test -z "$(git status --porcelain)"`},
		},
		Fingerprints: []Check{
			{Name: "prod-db", Run: `psql "$PROD_RO_URL" -Atc "select md5(...)"`}, // the right kind
			{Name: "modcache", Run: `ls "$(go env GOMODCACHE)/cache/download" | wc -l`},
		},
		ReadonlyPaths: []string{"README.md", "does/not/exist/", ".git/"},
	}
	got := lintRails(rails, root)
	where := map[string]string{}
	for _, f := range got {
		where[f.Where] = f.What
	}
	for _, want := range []string{
		"preflight.no-uncommitted-changes",
		"fingerprints.modcache",
		"readonly_paths.does/not/exist/",
		"readonly_paths..git/",
	} {
		if _, ok := where[want]; !ok {
			t.Errorf("lint missed %s; got %v", want, where)
		}
	}
	// and it must not cry wolf: an external database is what fingerprints are FOR,
	// and a path that exists is fine.
	for _, quiet := range []string{"fingerprints.prod-db", "preflight.ok", "readonly_paths.README.md", "gate"} {
		if what, ok := where[quiet]; ok {
			t.Errorf("lint flagged %s, which is correct usage: %s", quiet, what)
		}
	}
}

func TestLintRailsFlagsAnEmptyGate(t *testing.T) {
	got := lintRails(railsFile{}, t.TempDir())
	if len(got) == 0 || got[0].Where != "gate" {
		t.Fatalf("a build with no gate verifies nothing; lint said %v", got)
	}
}

// auto is the one-prompt path, but the operator keeps every lever: a
// hand-written draft is honoured as-is, and nothing is approved silently.
func TestAutoUsesAHandWrittenDraftAndRefusesToBlessItAlone(t *testing.T) {
	e := setup(t, 1, []string{"commit-done"}, []string{"verified"})
	// setup() already approved a build; auto must refuse to trample it
	os.Chdir(e.root)
	if err := cmdAuto([]string{"--yes", "build something"}); err == nil {
		t.Fatal("auto must refuse when a build already exists")
	} else if !strings.Contains(err.Error(), "re-read before every session") {
		t.Errorf("the refusal should point at editing in place, got: %v", err)
	}

	// a draft written by hand, with no planner involved at all
	root2 := filepath.Join(t.TempDir(), "app2")
	os.MkdirAll(filepath.Join(root2, ".backcheck", "draft"), 0o755)
	mustGit(t, filepath.Dir(root2), "init", "-q")
	_ = os.WriteFile(filepath.Join(root2, ".backcheck", "draft", "waves.md"), []byte(waveText(1)), 0o644)
	_ = os.WriteFile(filepath.Join(root2, ".backcheck", "draft", "rails.json"), []byte(`{"gate":[]}`), 0o644)

	var rails railsFile
	json.Unmarshal([]byte(readFileOr(filepath.Join(root2, ".backcheck", "draft", "rails.json"), "")), &rails)
	if got := lintRails(rails, root2); len(got) == 0 {
		t.Error("a hand-written draft is linted too — an empty gate is still an empty gate")
	}
}
