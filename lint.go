package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A finding is something about the drafted rails that is worth a human's eye
// before a build runs unattended on them. None of these are certainly wrong —
// that is the point. They are the shapes that have actually trapped a build,
// so the planner's word alone should not be enough to ship them.
type finding struct {
	Where string // "preflight.no-uncommitted-changes"
	What  string
}

var (
	// preflight runs before every session, and a builder killed mid-edit
	// leaves the worktree dirty. A preflight that refuses to pass on a dirty
	// tree therefore cannot recover on its own.
	reSelfDirty = regexp.MustCompile(`git\s+status`)

	// Fingerprints exist to watch what git cannot restore — an external
	// database is exactly right. A shared build cache is not: unrelated work
	// anywhere on the machine moves it, and every session then halts.
	reSharedCache = regexp.MustCompile(`(?i)(GOMODCACHE|GOCACHE|GOPATH|npm\s+root|\.npm\b|\.cache\b|\.composer\b|\.m2\b|\.gradle\b|node_modules|/tmp/|/var/tmp)`)

	// Output that changes on its own makes every session look like tampering.
	reNonDeterministic = regexp.MustCompile(`(?i)(\bdate\b|\buptime\b|\bps\s|free\s+-|df\s+-|\$RANDOM|uuidgen)`)
)

// The ports a developer's own server is already sitting on. A build that
// verifies one of these is not necessarily verifying the worktree.
var busyDevPorts = map[string]string{
	"3000": "Node/Next", "5173": "Vite", "8000": "Laravel/Django", "8080": "the usual spare",
	"4200": "Angular", "5000": "Flask", "3001": "the usual second Node app",
}

// lintRails reads the rails a planner drafted and reports what a human should
// look at. repoRoot is the main repo, used to tell an inert path from a real one.
func lintRails(r railsFile, repoRoot string) []finding {
	var out []finding
	add := func(where, what string) { out = append(out, finding{where, what}) }

	if len(r.Gate) == 0 {
		add("gate", "empty — nothing mechanical checks the builder's work, and the reviewer has nothing to re-run")
	}
	for _, c := range r.Preflight {
		if reSelfDirty.MatchString(c.Run) {
			add("preflight."+c.Name,
				"inspects the worktree's own cleanliness. A session killed mid-edit leaves the tree dirty, "+
					"and this can then never pass again — the build retries until it gives up at NEEDS_OPERATOR")
		}
		if reNonDeterministic.MatchString(c.Run) {
			add("preflight."+c.Name, "looks non-deterministic; preflight should answer \"is the machine fit\", not \"what time is it\"")
		}
	}
	for _, c := range r.Fingerprints {
		if reSharedCache.MatchString(c.Run) {
			add("fingerprints."+c.Name,
				"samples a shared build cache. Anything else on this machine moves it, and a moved fingerprint HALTs the build. "+
					"Fingerprints are for state git cannot restore — a database, a deployed artefact — not for caches")
		}
		if reNonDeterministic.MatchString(c.Run) {
			add("fingerprints."+c.Name, "looks non-deterministic — it would halt the build on its own, every session")
		}
	}
	if b := r.Browser; b != nil && b.Enabled {
		lintBrowser(b, add)
	}
	for _, p := range r.ReadonlyPaths {
		clean := strings.TrimSuffix(p, "/")
		if clean == "" {
			continue
		}
		if clean == ".git" || strings.HasPrefix(clean, ".git/") {
			add("readonly_paths."+p, "git does not track .git/, so this entry checks nothing")
			continue
		}
		if _, err := os.Stat(filepath.Join(repoRoot, clean)); err != nil {
			add("readonly_paths."+p, "does not exist in the repo — the entry is inert, which reads like protection but is not")
		}
	}
	return out
}

// lintBrowser covers the ways a browser-backed build passes while proving
// nothing. All three have the same shape: the session really did drive a real
// browser, and what it looked at was not this build's work.
func lintBrowser(b *Browser, add func(where, what string)) {
	if u, err := url.Parse(b.Serve.URL); err == nil {
		if port := u.Port(); port != "" {
			if what, busy := busyDevPorts[port]; busy {
				add("browser.serve.url",
					"port "+port+" is where "+what+" usually listens. The build runs beside a developer who "+
						"may already have that port: the session would then screenshot THEIR app, "+
						"see it working, and sign a wave that was never built. Pick a port nothing else uses")
			}
		}
	}
	if strings.TrimSpace(b.Serve.Run) == "" {
		add("browser.serve.run",
			"empty, so the build depends on a server someone starts by hand. Unattended, the first "+
				"time it is not running every session retries until NEEDS_OPERATOR")
	}
	for name, m := range b.MCP {
		joined := m.Command + " " + strings.Join(m.Args, " ")
		if strings.Contains(joined, "playwright") && !strings.Contains(joined, "--headless") {
			add("browser.mcp."+name,
				"drives Playwright without --headless. Unattended there is usually no display to open "+
					"a window on, and the session hangs on its first navigation until the idle watchdog kills it")
		}
	}
}

func printFindings(fs []finding) {
	if len(fs) == 0 {
		fmt.Println("rails lint: nothing to flag")
		return
	}
	fmt.Printf("rails lint: %d thing(s) to look at before this runs unattended\n", len(fs))
	for _, f := range fs {
		fmt.Printf("  ! %-38s %s\n", f.Where, f.What)
	}
}
