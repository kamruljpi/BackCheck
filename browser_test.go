package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// freePort asks the kernel for a port and hands it back. There is a race
// between closing and the server binding, which is why tests that need a port
// use one each rather than sharing.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestBrowserFillDefaults(t *testing.T) {
	t.Run("disabled stays empty", func(t *testing.T) {
		var b Browser
		b.fillDefaults()
		if b.Serve.URL != "" || len(b.MCP) != 0 || len(b.Viewports) != 0 {
			t.Fatalf("a disabled browser grew defaults: %+v", b)
		}
	})
	t.Run("enabled alone is usable", func(t *testing.T) {
		b := Browser{Enabled: true}
		b.fillDefaults()
		if b.Serve.URL == "" || b.Serve.ReadyTimeoutSeconds <= 0 {
			t.Errorf("serve not defaulted: %+v", b.Serve)
		}
		if len(b.MCP) == 0 || len(b.Routes) == 0 {
			t.Errorf("mcp/routes not defaulted: %+v", b)
		}
		// Both viewports matter: a model told only to look checks desktop.
		if len(b.Viewports) < 2 {
			t.Errorf("want a desktop and a mobile viewport by default, got %+v", b.Viewports)
		}
		var mobile bool
		for _, v := range b.Viewports {
			if v.Width < 500 {
				mobile = true
			}
		}
		if !mobile {
			t.Errorf("no narrow viewport in the defaults: %+v", b.Viewports)
		}
	})
	t.Run("explicit values survive", func(t *testing.T) {
		b := Browser{Enabled: true,
			Serve:     Serve{URL: "http://127.0.0.1:9999", ReadyTimeoutSeconds: 7},
			Viewports: []Viewport{{Name: "only", Width: 100, Height: 200}},
			Routes:    []string{"/a"},
			MCP:       map[string]MCPServer{"x": {Command: "true"}}}
		b.fillDefaults()
		if b.Serve.URL != "http://127.0.0.1:9999" || b.Serve.ReadyTimeoutSeconds != 7 {
			t.Errorf("serve overwritten: %+v", b.Serve)
		}
		if len(b.Viewports) != 1 || len(b.Routes) != 1 || len(b.MCP) != 1 {
			t.Errorf("explicit lists overwritten: %+v", b)
		}
	})
}

func TestBrowserValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    Browser
		want string
	}{
		{"not a url", Browser{Enabled: true, Serve: Serve{URL: "127.0.0.1:8000"}}, "not an http(s) URL"},
		{"no mcp command", Browser{Enabled: true, Serve: Serve{URL: "http://x"},
			MCP: map[string]MCPServer{"p": {}}}, "browser.mcp.p.command empty"},
		{"zero viewport", Browser{Enabled: true, Serve: Serve{URL: "http://x"},
			Viewports: []Viewport{{Name: "bad"}}}, "positive width and height"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := strings.Join(tc.b.validate(), "; ")
			if !strings.Contains(errs, tc.want) {
				t.Fatalf("validate() = %q, want it to mention %q", errs, tc.want)
			}
		})
	}
	t.Run("disabled is never invalid", func(t *testing.T) {
		b := Browser{Serve: Serve{URL: "nonsense"}}
		if errs := b.validate(); len(errs) != 0 {
			t.Fatalf("a disabled browser was validated: %v", errs)
		}
	})
}

func TestMCPConfigAndFlags(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{stateDir: dir, root: dir}
	cfg.Browser = Browser{Enabled: true,
		MCP: map[string]MCPServer{"playwright": {Command: "npx", Args: []string{"-y", "@playwright/mcp@latest", "--headless"}}}}
	path, err := cfg.writeMCPConfig()
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MCPServers map[string]MCPServer `json:"mcpServers"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("mcp.json is not the shape Claude Code reads: %v\n%s", err, b)
	}
	if got.MCPServers["playwright"].Command != "npx" {
		t.Fatalf("mcp.json lost the server: %s", b)
	}

	base := Provider{Command: []string{"claude", "-p"}}
	q := withMCP(base, path, true)
	joined := strings.Join(q.Command, " ")
	if !strings.Contains(joined, "--mcp-config "+path) || !strings.Contains(joined, "--strict-mcp-config") {
		t.Fatalf("flags missing: %q", joined)
	}
	if strings.Join(base.Command, " ") != "claude -p" {
		t.Fatalf("withMCP mutated the provider it was given: %v", base.Command)
	}
	if j := strings.Join(withMCP(base, path, false).Command, " "); strings.Contains(j, "--strict-mcp-config") {
		t.Fatalf("strict=false still passed --strict-mcp-config: %q", j)
	}
	if j := withMCP(base, "", true); len(j.Command) != 2 {
		t.Fatalf("no mcp path should leave the command alone, got %v", j.Command)
	}
}

func TestWaitURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500) // a broken app is still an app that is up
	}))
	defer srv.Close()
	if err := waitURL(context.Background(), srv.URL, 5*time.Second); err != nil {
		t.Fatalf("a 500 should count as serving: %v", err)
	}
	dead := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	err := waitURL(context.Background(), dead, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("want a timeout naming the URL, got %v", err)
	}
	if err := waitURL(context.Background(), "", time.Second); err != nil {
		t.Fatalf("no URL configured should be no wait: %v", err)
	}
}

// The driver must leave nothing listening: an orphaned dev server holds the
// port, and the next session then verifies the previous build's pages.
func TestStartServerStopsWhatItStarted(t *testing.T) {
	port := freePort(t)
	dir := t.TempDir()
	cfg := &Config{root: dir, stateDir: filepath.Join(dir, ".backcheck"), Worktree: "."}
	os.MkdirAll(cfg.sessionsDir(), 0o755)
	cfg.Browser = Browser{Enabled: true, Serve: Serve{
		Run:                 fmt.Sprintf("exec python3 -m http.server %d --bind 127.0.0.1", port),
		URL:                 fmt.Sprintf("http://127.0.0.1:%d/", port),
		ReadyTimeoutSeconds: 30,
	}}
	d := &Driver{root: dir, cfg: cfg}

	sv, msg := d.startServer(context.Background())
	if msg != "" {
		t.Fatalf("startServer: %s", msg)
	}
	if sv == nil {
		t.Fatal("a configured run command should have produced a server to stop")
	}
	if err := waitURL(context.Background(), cfg.Browser.Serve.URL, 2*time.Second); err != nil {
		t.Fatalf("server not actually serving: %v", err)
	}
	sv.Stop()

	// The port must come back. python3 -m http.server forks nothing, but the
	// `exec` matters: without it bash would hold the port after the SIGTERM.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := waitURL(context.Background(), cfg.Browser.Serve.URL, 200*time.Millisecond); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the dev server was still answering 10s after Stop()")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestStartServerReportsADeadURL(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{root: dir, stateDir: filepath.Join(dir, ".backcheck"), Worktree: "."}
	os.MkdirAll(cfg.sessionsDir(), 0o755)
	// No run command: an app someone else is supposed to be serving, and isn't.
	cfg.Browser = Browser{Enabled: true, Serve: Serve{
		URL: fmt.Sprintf("http://127.0.0.1:%d", freePort(t)), ReadyTimeoutSeconds: 1}}
	d := &Driver{root: dir, cfg: cfg}
	sv, msg := d.startServer(context.Background())
	defer sv.Stop()
	if msg == "" {
		t.Fatal("a dead URL was reported as ready — the session would review a connection error")
	}
}

func TestBrowserBriefReachesBothPrompts(t *testing.T) {
	cfg := &Config{root: "/r", stateDir: "/r/.backcheck"}
	cfg.Browser = Browser{Enabled: true,
		Serve:     Serve{URL: "http://127.0.0.1:8123"},
		Routes:    []string{"/", "/test"},
		Viewports: []Viewport{{Name: "desktop", Width: 1440, Height: 900}, {Name: "mobile", Width: 390, Height: 844}},
		MCP:       map[string]MCPServer{"playwright": {Command: "npx"}}}
	br := cfg.browserBrief("w1-a-builder")
	if br == nil {
		t.Fatal("browserBrief returned nil while enabled")
	}
	if !strings.HasPrefix(br.ShotsDir, cfg.sessionsDir()) {
		t.Fatalf("shots must live under .backcheck, not the worktree: %s", br.ShotsDir)
	}

	wave := Wave{N: 1, Title: "t", Body: "body\n### Acceptance criteria\n- x"}
	pd := PromptData{Worktree: "/wt", Branch: "b", Wave: wave, TotalWaves: 1, Browser: br}
	for _, role := range []string{"builder", "reviewer"} {
		out := render(role, pd)
		for _, want := range []string{"http://127.0.0.1:8123", "/test", "1440x900", "390x844", br.ShotsDir, "console"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s prompt missing %q", role, want)
			}
		}
	}
	// The reviewer must be told not to take the builder's PNGs as evidence.
	rev := render("reviewer", pd)
	if !strings.Contains(rev, "re-take") && !strings.Contains(rev, "yourself") {
		t.Errorf("reviewer prompt does not tell it to take its own screenshots:\n%s", rev)
	}

	off := render("builder", PromptData{Worktree: "/wt", Branch: "b", Wave: wave, TotalWaves: 1})
	if strings.Contains(off, "Browser verification") {
		t.Errorf("browser section rendered with no browser configured:\n%s", off)
	}
}

func TestLintBrowser(t *testing.T) {
	find := func(b *Browser) string {
		var got []string
		lintBrowser(b, func(where, what string) { got = append(got, where+": "+what) })
		return strings.Join(got, "\n")
	}
	t.Run("a developer's port", func(t *testing.T) {
		b := &Browser{Enabled: true, Serve: Serve{URL: "http://127.0.0.1:8000", Run: "x"}}
		if !strings.Contains(find(b), "browser.serve.url") {
			t.Fatalf("port 8000 not flagged: %s", find(b))
		}
	})
	t.Run("nobody starts the server", func(t *testing.T) {
		b := &Browser{Enabled: true, Serve: Serve{URL: "http://127.0.0.1:8123"}}
		if !strings.Contains(find(b), "browser.serve.run") {
			t.Fatalf("missing run not flagged: %s", find(b))
		}
	})
	t.Run("headed playwright", func(t *testing.T) {
		b := &Browser{Enabled: true, Serve: Serve{URL: "http://127.0.0.1:8123", Run: "x"},
			MCP: map[string]MCPServer{"pw": {Command: "npx", Args: []string{"@playwright/mcp@latest"}}}}
		if !strings.Contains(find(b), "browser.mcp.pw") {
			t.Fatalf("headed playwright not flagged: %s", find(b))
		}
	})
	t.Run("a sound config is quiet", func(t *testing.T) {
		b := &Browser{Enabled: true,
			Serve: Serve{URL: "http://127.0.0.1:8123", Run: "php artisan serve --port=8123"},
			MCP:   map[string]MCPServer{"pw": {Command: "npx", Args: []string{"@playwright/mcp@latest", "--headless"}}}}
		if s := find(b); s != "" {
			t.Fatalf("clean config flagged:\n%s", s)
		}
	})
}

// withBrowser wires the harness's build to a real (static) dev server.
func withBrowser(port int) opt {
	return func(c map[string]any) {
		c["browser"] = map[string]any{
			"enabled": true,
			"serve": map[string]any{
				"run":                   fmt.Sprintf("exec python3 -m http.server %d --bind 127.0.0.1", port),
				"url":                   fmt.Sprintf("http://127.0.0.1:%d/", port),
				"ready_timeout_seconds": 30,
			},
			"routes": []string{"/", "/test"},
			"mcp": map[string]any{"playwright": map[string]any{
				"command": "npx", "args": []string{"-y", "@playwright/mcp@latest", "--headless"}}},
		}
	}
}

// The whole feature, end to end: the driver serves the app, hands the session
// the MCP server, briefs it on routes and viewports, gives it somewhere to put
// the screenshots — and leaves nothing running afterwards.
func TestBrowserSessionIsActuallyWiredUp(t *testing.T) {
	port := freePort(t)
	e := setup(t, 1, []string{"commit-done"}, []string{"verified"}, withBrowser(port))
	ctl := e.runUntil(10)
	expectStatus(t, ctl, StClosed, "all 1 waves signed")

	for _, role := range []string{"builder", "reviewer"} {
		argvs, _ := filepath.Glob(filepath.Join(e.mock, "prompts", "*-"+role+".argv"))
		if len(argvs) == 0 {
			t.Fatalf("no %s session ran", role)
		}
		argv := readFileOr(argvs[0], "")
		if !strings.Contains(argv, "--mcp-config") || !strings.Contains(argv, "--strict-mcp-config") {
			t.Errorf("%s CLI was not given the browser: %q", role, argv)
		}
		mcpPath := e.cfg.mcpConfigPath()
		if !strings.Contains(argv, mcpPath) {
			t.Errorf("%s got --mcp-config but not %s: %q", role, mcpPath, argv)
		}
		if _, err := os.Stat(mcpPath); err != nil {
			t.Errorf("mcp.json was named on the command line but never written: %v", err)
		}

		prompts, _ := filepath.Glob(filepath.Join(e.mock, "prompts", "*-"+role+".txt"))
		brief := readFileOr(prompts[0], "")
		for _, want := range []string{"Browser verification", fmt.Sprintf("127.0.0.1:%d", port), "/test", "1440x900", "390x844"} {
			if !strings.Contains(brief, want) {
				t.Errorf("%s brief missing %q", role, want)
			}
		}
	}

	// Somewhere to put the screenshots, outside the worktree so the tree stays
	// clean — and created up front so a session cannot call it missing.
	shots, _ := filepath.Glob(filepath.Join(e.cfg.sessionsDir(), "*.shots"))
	if len(shots) == 0 {
		t.Error("no shots directory was created for any session")
	}
	if dirty := gitDirty(e.cfg.WorktreePath()); dirty {
		t.Error("the browser run left the worktree dirty")
	}

	// And nothing is still listening once the build is over.
	if err := waitURL(context.Background(), fmt.Sprintf("http://127.0.0.1:%d/", port), 300*time.Millisecond); err == nil {
		t.Error("the dev server outlived the build — the next one would verify these pages")
	}
}

// rehearse exists so a bad config costs nothing. The browser path has more
// ways to be wrong than most — a missing npx, a taken port, an app that never
// comes up — and all of them are cheaper to find here than mid-build.
func TestRehearseBrowser(t *testing.T) {
	newCfg := func(t *testing.T, port int, mcpCmd string) *Config {
		dir := t.TempDir()
		cfg := &Config{root: dir, stateDir: filepath.Join(dir, ".backcheck"), Worktree: "."}
		os.MkdirAll(cfg.sessionsDir(), 0o755)
		cfg.Browser = Browser{Enabled: true,
			Serve: Serve{
				Run:                 fmt.Sprintf("exec python3 -m http.server %d --bind 127.0.0.1", port),
				URL:                 fmt.Sprintf("http://127.0.0.1:%d/", port),
				ReadyTimeoutSeconds: 30},
			Routes:    []string{"/"},
			Viewports: []Viewport{{Name: "desktop", Width: 1440, Height: 900}},
			MCP:       map[string]MCPServer{"pw": {Command: mcpCmd}}}
		return cfg
	}

	t.Run("a working stack", func(t *testing.T) {
		cfg := newCfg(t, freePort(t), "echo") // stands in for npx: what matters is that it resolves
		d := &Driver{root: cfg.root, cfg: cfg}
		sv, detail, ok := d.rehearseBrowser(context.Background())
		defer sv.Stop()
		if !ok {
			t.Fatalf("rehearseBrowser failed on a sound config: %s", detail)
		}
		if !strings.Contains(detail, cfg.Browser.Serve.URL) {
			t.Errorf("detail does not say what answered: %q", detail)
		}
		// It leaves the server up for the gate that follows.
		if err := waitURL(context.Background(), cfg.Browser.Serve.URL, time.Second); err != nil {
			t.Errorf("rehearseBrowser stopped the server the gate still needs: %v", err)
		}
	})

	t.Run("no such MCP command", func(t *testing.T) {
		cfg := newCfg(t, freePort(t), "backcheck-no-such-mcp-binary")
		d := &Driver{root: cfg.root, cfg: cfg}
		sv, detail, ok := d.rehearseBrowser(context.Background())
		defer sv.Stop()
		if ok {
			t.Fatal("a missing MCP binary rehearsed clean")
		}
		if !strings.Contains(detail, "not on PATH") {
			t.Errorf("detail should name the missing binary, got %q", detail)
		}
	})
}

// rails.json is written by a model. When it has to guess a shape it guesses
// plausibly, and a strict decoder turns each plausible guess into a build that
// cannot start — over a field that has a perfectly good default. This is the
// exact payload that broke a real draft: "viewports":[1440,390].
func TestViewportAcceptsWhatAPlannerWrites(t *testing.T) {
	for _, tc := range []struct {
		json string
		want Viewport
	}{
		{`{"name":"desktop","width":1440,"height":900}`, Viewport{"desktop", 1440, 900}},
		{`{"width":1440,"height":900}`, Viewport{"desktop", 1440, 900}},
		{`{"width":390,"height":844}`, Viewport{"mobile", 390, 844}},
		{`1440`, Viewport{"desktop", 1440, 900}},
		{`390`, Viewport{"mobile", 390, 844}},
		{`800`, Viewport{"tablet", 800, 1024}},
		{`"1440x900"`, Viewport{"desktop", 1440, 900}},
		{`"390X844"`, Viewport{"mobile", 390, 844}},
		{`"1440×900"`, Viewport{"desktop", 1440, 900}},
		{`"mobile: 390x844"`, Viewport{"mobile", 390, 844}},
		{`"phone 390x844"`, Viewport{"phone", 390, 844}},
		{`"1440"`, Viewport{"desktop", 1440, 900}},
		{`[1440,900]`, Viewport{"desktop", 1440, 900}},
	} {
		var got Viewport
		if err := json.Unmarshal([]byte(tc.json), &got); err != nil {
			t.Errorf("%s: %v", tc.json, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %+v, want %+v", tc.json, got, tc.want)
		}
	}
}

func TestViewportRejectsNonsenseReadably(t *testing.T) {
	for _, bad := range []string{`"wide"`, `{"width":0,"height":0,"name":""}`, `true`, `[1,2,3]`, `0`} {
		var got Viewport
		err := json.Unmarshal([]byte(bad), &got)
		if bad == `{"width":0,"height":0,"name":""}` {
			// Structurally fine; validate() is what rejects a zero viewport.
			if err != nil {
				t.Errorf("%s should decode and fail validation, not decoding: %v", bad, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s decoded to %+v, want an error", bad, got)
			continue
		}
		// The message has to tell someone editing JSON what to type, not name
		// a Go type they have never heard of.
		if !strings.Contains(err.Error(), `"width"`) || strings.Contains(err.Error(), "main.Viewport") {
			t.Errorf("%s: unhelpful error %q", bad, err)
		}
	}
}

// The whole railsFile, as the planner emits it, with the bad viewports.
func TestRailsFileSurvivesBareWidths(t *testing.T) {
	var r railsFile
	src := `{"gate":[{"name":"t","run":"true"}],
	         "browser":{"enabled":true,"serve":{"run":"x","url":"http://127.0.0.1:8123"},
	                    "routes":["/"],"viewports":[1440,390]}}`
	if err := json.Unmarshal([]byte(src), &r); err != nil {
		t.Fatalf("the draft that broke a real build still does not parse: %v", err)
	}
	if len(r.Browser.Viewports) != 2 ||
		r.Browser.Viewports[0] != (Viewport{"desktop", 1440, 900}) ||
		r.Browser.Viewports[1] != (Viewport{"mobile", 390, 844}) {
		t.Fatalf("viewports = %+v", r.Browser.Viewports)
	}
	// And what was read is shown before anyone approves it.
	if s := browserSummary(orBrowser(r.Browser, &Browser{})); !strings.Contains(s, "desktop 1440x900") ||
		!strings.Contains(s, "mobile 390x844") {
		t.Fatalf("approve would not show the inferred viewports: %s", s)
	}
}
