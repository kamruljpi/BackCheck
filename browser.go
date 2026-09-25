package main

// Browser verification. A gate check can prove a route returns 200; it cannot
// prove the page is not a stack of overlapping divs, and neither can a model
// that only ever read the Blade template. This wires an unattended session to
// a real browser: the driver owns a dev server, the session drives Chromium
// through an MCP server, and the screenshots it takes land in .backcheck as
// evidence a human — or the reviewer — can look at afterwards.
//
// The split of trust is the same as everywhere else in backcheck. The driver
// owns what must be deterministic: the server is up, the MCP server is
// resolvable, the artefacts exist. The session owns the judgement, and the
// reviewer re-takes its own screenshots rather than trusting the builder's.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Browser is the whole feature, off unless enabled. Everything under it has a
// default, so `"browser": {"enabled": true}` alone is a working configuration
// for any app already served on http://127.0.0.1:8000.
type Browser struct {
	Enabled   bool                 `json:"enabled"`
	Serve     Serve                `json:"serve,omitempty"`
	MCP       map[string]MCPServer `json:"mcp,omitempty"`
	Viewports []Viewport           `json:"viewports,omitempty"`
	Routes    []string             `json:"routes,omitempty"`

	// Strict passes --strict-mcp-config, so a session sees exactly the servers
	// declared here and nothing the developer happens to have configured
	// globally. Default on: an unattended build that behaves differently on
	// two machines is not reproducible. Turn it off for a project whose own
	// .mcp.json the build genuinely needs.
	Strict *bool `json:"strict,omitempty"`
}

// Serve is the dev server the driver starts and stops around each session.
// Run empty means the server is somebody else's job and URL is only polled.
type Serve struct {
	Run                 string `json:"run,omitempty"`
	URL                 string `json:"url,omitempty"`
	ReadyPath           string `json:"ready_path,omitempty"` // polled instead of URL's own path
	ReadyTimeoutSeconds int    `json:"ready_timeout_seconds,omitempty"`
}

// MCPServer is one entry of the generated mcp.json, in Claude Code's shape.
type MCPServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type Viewport struct {
	Name   string `json:"name"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// UnmarshalJSON accepts the shapes a planner actually writes, not just the one
// we document. rails.json is produced by a model: when it has to guess it
// guesses plausibly — a bare width, a "1440x900" string, a [w,h] pair — and a
// strict decoder turns each of those into a build that cannot start, over a
// field that has a perfectly good default. Whatever is read is printed back by
// `approve` before a human blesses it, so a generous reading is never a silent
// one.
func (v *Viewport) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	switch {
	case strings.HasPrefix(trimmed, "{"):
		type plain Viewport // shed the method, or this recurses
		var p plain
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		*v = Viewport(p)
		if v.Name == "" {
			v.Name = viewportFromWidth(v.Width).Name
		}
		return nil

	case strings.HasPrefix(trimmed, "["):
		var pair []int
		if err := json.Unmarshal(b, &pair); err == nil && len(pair) == 2 {
			*v = Viewport{Name: viewportFromWidth(pair[0]).Name, Width: pair[0], Height: pair[1]}
			return nil
		}

	case strings.HasPrefix(trimmed, `"`):
		var s string
		if err := json.Unmarshal(b, &s); err == nil {
			if got, ok := parseViewportString(s); ok {
				*v = got
				return nil
			}
		}

	default:
		var w int
		if err := json.Unmarshal(b, &w); err == nil && w > 0 {
			*v = viewportFromWidth(w)
			return nil
		}
	}
	return fmt.Errorf(`browser.viewports: cannot read %s as a viewport — write `+
		`{"name":"desktop","width":1440,"height":900}, or "1440x900", or just 1440`, trimmed)
}

// parseViewportString reads "1440x900", "mobile: 390x844", "mobile 390x844"
// and a bare "1440".
func parseViewportString(s string) (Viewport, bool) {
	s = strings.TrimSpace(s)
	name := ""
	if i := strings.IndexAny(s, ":="); i >= 0 {
		name, s = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	} else if f := strings.Fields(s); len(f) == 2 {
		name, s = f[0], f[1]
	}
	// One spelling of the separator, whichever was typed.
	s = strings.NewReplacer("X", "x", "\u00d7", "x", "*", "x").Replace(strings.TrimSpace(s))
	var w, h int
	switch parts := strings.Split(s, "x"); len(parts) {
	case 1:
		if n, err := strconv.Atoi(parts[0]); err == nil && n > 0 {
			v := viewportFromWidth(n)
			if name != "" {
				v.Name = name
			}
			return v, true
		}
	case 2:
		w, _ = strconv.Atoi(strings.TrimSpace(parts[0]))
		h, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
		if w > 0 && h > 0 {
			if name == "" {
				name = viewportFromWidth(w).Name
			}
			return Viewport{Name: name, Width: w, Height: h}, true
		}
	}
	return Viewport{}, false
}

// viewportFromWidth supplies a plausible device height and name for a width
// given on its own. The guess only decides how tall a screenshot is, not
// whether the layout is right, and it is shown at approval.
func viewportFromWidth(w int) Viewport {
	switch {
	case w >= 1280:
		return Viewport{Name: "desktop", Width: w, Height: 900}
	case w >= 768:
		return Viewport{Name: "tablet", Width: w, Height: 1024}
	default:
		return Viewport{Name: "mobile", Width: w, Height: 844}
	}
}

func (v Viewport) String() string { return fmt.Sprintf("%s %dx%d", v.Name, v.Width, v.Height) }

// On reports whether sessions should be wired to a browser at all.
func (c *Config) BrowserOn() bool { return c.Browser.Enabled }

func (b *Browser) StrictMCP() bool { return b.Strict == nil || *b.Strict }

// readyURL is what the driver polls to decide the app is actually serving.
func (s Serve) readyURL() string {
	if s.URL == "" {
		return ""
	}
	if s.ReadyPath == "" {
		return s.URL
	}
	return strings.TrimRight(s.URL, "/") + "/" + strings.TrimLeft(s.ReadyPath, "/")
}

func defaultBrowser() Browser {
	return Browser{
		Serve: Serve{URL: "http://127.0.0.1:8000", ReadyTimeoutSeconds: 60},
		MCP: map[string]MCPServer{
			"playwright": {Command: "npx", Args: []string{"-y", "@playwright/mcp@latest", "--headless", "--isolated"}},
		},
		// Both viewports are named explicitly because a model told only to
		// "check it looks right" checks desktop and stops.
		Viewports: []Viewport{
			{Name: "desktop", Width: 1440, Height: 900},
			{Name: "mobile", Width: 390, Height: 844},
		},
		Routes: []string{"/"},
	}
}

// fillDefaults leaves an explicitly configured field alone and supplies the
// rest, so a half-written browser block is still a usable one.
func (b *Browser) fillDefaults() {
	if !b.Enabled {
		return
	}
	d := defaultBrowser()
	if b.Serve.URL == "" {
		b.Serve.URL = d.Serve.URL
	}
	if b.Serve.ReadyTimeoutSeconds <= 0 {
		b.Serve.ReadyTimeoutSeconds = d.Serve.ReadyTimeoutSeconds
	}
	if len(b.MCP) == 0 {
		b.MCP = d.MCP
	}
	if len(b.Viewports) == 0 {
		b.Viewports = d.Viewports
	}
	if len(b.Routes) == 0 {
		b.Routes = d.Routes
	}
}

func (b *Browser) validate() []string {
	if !b.Enabled {
		return nil
	}
	var errs []string
	if b.Serve.URL == "" {
		errs = append(errs, "browser.serve.url empty")
	} else if !strings.HasPrefix(b.Serve.URL, "http://") && !strings.HasPrefix(b.Serve.URL, "https://") {
		errs = append(errs, fmt.Sprintf("browser.serve.url %q is not an http(s) URL", b.Serve.URL))
	}
	for name, s := range b.MCP {
		if s.Command == "" {
			errs = append(errs, "browser.mcp."+name+".command empty")
		}
	}
	for i, v := range b.Viewports {
		if v.Width <= 0 || v.Height <= 0 {
			errs = append(errs, fmt.Sprintf("browser.viewports[%d] (%s) needs a positive width and height", i, v.Name))
		}
	}
	return errs
}

// ---------------------------------------------------------------- mcp.json

func (c *Config) mcpConfigPath() string { return filepath.Join(c.stateDir, "mcp.json") }

// writeMCPConfig regenerates mcp.json from config.json on every use, so
// editing the rails mid-build changes what the next session can reach —
// the same promise the rest of the rails make.
func (c *Config) writeMCPConfig() (string, error) {
	if !c.BrowserOn() || len(c.Browser.MCP) == 0 {
		return "", nil
	}
	b, err := json.MarshalIndent(map[string]any{"mcpServers": c.Browser.MCP}, "", "  ")
	if err != nil {
		return "", err
	}
	path := c.mcpConfigPath()
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// withMCP hands a provider the browser servers for one dispatch. The provider
// in config.json is left alone: this is a copy, so a role that should not get
// a browser simply is not passed through here.
func withMCP(p Provider, mcpPath string, strict bool) Provider {
	if mcpPath == "" {
		return p
	}
	q := p
	q.Command = append(append([]string{}, p.Command...), "--mcp-config", mcpPath)
	if strict {
		q.Command = append(q.Command, "--strict-mcp-config")
	}
	return q
}

// ---------------------------------------------------------------- dev server

// server is a dev server the driver owns for the length of one session.
type server struct {
	cmd *exec.Cmd
	log *os.File
}

// Stop ends the server's whole process group. It is safe to call on a nil
// server, so the caller can defer it before knowing whether one was started.
func (s *server) Stop() {
	if s == nil || s.cmd == nil {
		return
	}
	stop := killGroup(s.cmd)
	_ = s.cmd.Wait()
	stop() // the pid is reaped; call off the escalation before it is reused
	if s.log != nil {
		_ = s.log.Close()
	}
}

// startServer brings the app up and waits for it to answer. It returns a
// server to stop (possibly nil) and a message describing what went wrong;
// an empty message means the URL responded.
//
// A configured URL with no run command is an app somebody else is serving:
// nothing is started, but it is still polled, because a session pointed at a
// dead port produces a confident review of a connection error.
func (d *Driver) startServer(ctx context.Context) (*server, string) {
	cfg := d.cfg
	if !cfg.BrowserOn() || cfg.Browser.Serve.URL == "" {
		return nil, ""
	}
	sv := cfg.Browser.Serve
	wait := time.Duration(sv.ReadyTimeoutSeconds) * time.Second

	var s *server
	if strings.TrimSpace(sv.Run) != "" {
		logPath := filepath.Join(cfg.sessionsDir(), "serve.log")
		_ = os.MkdirAll(cfg.sessionsDir(), 0o755)
		f, err := os.Create(logPath)
		if err != nil {
			return nil, "serve log: " + err.Error()
		}
		cmd := exec.Command("bash", "-c", sv.Run)
		cmd.Dir = cfg.WorktreePath()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// Bounded: a dev server that logs every request would otherwise fill
		// the disk over a long unattended build.
		w := &limitedWriter{w: f, n: 4 << 20}
		cmd.Stdout, cmd.Stderr = w, w
		if err := cmd.Start(); err != nil {
			f.Close()
			return nil, "serve: " + err.Error()
		}
		s = &server{cmd: cmd, log: f}
	}

	if err := waitURL(ctx, sv.readyURL(), wait); err != nil {
		s.Stop()
		return nil, err.Error()
	}
	return s, ""
}

// waitURL polls until something answers or the deadline passes. Any HTTP
// response counts as up, including a 500: whether the app is healthy is the
// gate's question, and a readiness check that demanded 200 would hide the
// very failure the session is there to look at.
func waitURL(ctx context.Context, url string, timeout time.Duration) error {
	if url == "" {
		return nil
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 5 * time.Second}
	var last error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("serve: %s: %w", url, err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			return nil
		}
		last = err
		if ctx.Err() != nil {
			return fmt.Errorf("serve: cancelled while waiting for %s", url)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("serve: %s did not answer within %s (%v)", url, timeout, last)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("serve: cancelled while waiting for %s", url)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------- evidence

// shotsDir is where a session is told to save its screenshots. It lives under
// .backcheck, never in the worktree: screenshots dropped in the tree would
// make it dirty, and a dirty tree is a preflight failure and an uncacheable
// gate. The directory is created up front so a session cannot decide it does
// not exist and skip the step.
func (c *Config) shotsDir(tag string) string {
	return filepath.Join(c.sessionsDir(), tag+".shots")
}

// BrowserBrief is what the prompt templates render. It is built once per
// session so builder and reviewer are told exactly the same URL, viewports
// and routes, and disagreement between them cannot come from the briefing.
type BrowserBrief struct {
	URL       string
	Routes    []string
	Viewports []Viewport
	ShotsDir  string
	Servers   []string // MCP server names the session can drive
}

func (c *Config) browserBrief(tag string) *BrowserBrief {
	if !c.BrowserOn() {
		return nil
	}
	b := &BrowserBrief{
		URL: c.Browser.Serve.URL, Routes: c.Browser.Routes,
		Viewports: c.Browser.Viewports, ShotsDir: c.shotsDir(tag),
	}
	for name := range c.Browser.MCP {
		b.Servers = append(b.Servers, name)
	}
	sort.Strings(b.Servers)
	return b
}

// orBrowser prefers a browser block the planner drafted over the one already
// in config.json, matching how every other rail is merged at approve time.
func orBrowser(drafted, current *Browser) *Browser {
	if drafted != nil {
		d := *drafted
		d.fillDefaults()
		return &d
	}
	return current
}

// browserSummary is the one line `approve` and `auto` print, so a human can
// see what a session will be pointed at before blessing it.
func browserSummary(b *Browser) string {
	if b == nil || !b.Enabled {
		return "off"
	}
	var vs []string
	for _, v := range b.Viewports {
		vs = append(vs, v.String())
	}
	serve := "already running"
	if strings.TrimSpace(b.Serve.Run) != "" {
		serve = b.Serve.Run
	}
	var names []string
	for n := range b.MCP {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Sprintf("%s (%s) · routes %s · %s · via %s",
		b.Serve.URL, serve, strings.Join(b.Routes, " "), strings.Join(vs, ", "), strings.Join(names, "+"))
}

// rehearseBrowser proves the whole browser path before a build spends a penny:
// mcp.json can be written, every MCP command exists on PATH, the dev server
// starts, and the URL answers. Discovering mid-build that the port was already
// taken is discovering it too late — the same reason rehearse really fires the
// notify command. The server is left running for the gate that follows; the
// caller stops it.
func (d *Driver) rehearseBrowser(ctx context.Context) (*server, string, bool) {
	cfg := d.cfg
	path, err := cfg.writeMCPConfig()
	if err != nil {
		return nil, "mcp.json: " + err.Error(), false
	}
	var names []string
	for n := range cfg.Browser.MCP {
		names = append(names, n)
	}
	sort.Strings(names)

	var bad []string
	for _, name := range names {
		m := cfg.Browser.MCP[name]
		if _, err := exec.LookPath(m.Command); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %q not on PATH", name, m.Command))
		}
	}
	sv, msg := d.startServer(ctx)
	if msg != "" {
		bad = append(bad, msg)
	}
	if len(bad) > 0 {
		return sv, strings.Join(bad, "; "), false
	}
	serve := "already running"
	if strings.TrimSpace(cfg.Browser.Serve.Run) != "" {
		serve = "started by the driver"
	}
	return sv, fmt.Sprintf("%s answering (%s), %d route(s) x %d viewport(s), mcp %s via %s",
		cfg.Browser.Serve.URL, serve, len(cfg.Browser.Routes), len(cfg.Browser.Viewports),
		filepath.Base(path), strings.Join(names, "+")), true
}
