package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Wave struct {
	N     int
	Title string
	Body  string // everything under the heading, acceptance criteria included
}

var waveHead = regexp.MustCompile(`(?m)^##\s+Wave\s+(\d+)\s*[:.—–-]?\s*(.*)$`)

// ParseWaves reads waves.md: each wave is a "## Wave N: title" section.
// Numbers must run 1..n without gaps so "the last wave" is unambiguous.
func ParseWaves(text string) ([]Wave, error) {
	idx := waveHead.FindAllStringSubmatchIndex(text, -1)
	if len(idx) == 0 {
		return nil, fmt.Errorf("waves.md has no \"## Wave N: title\" headings")
	}
	var waves []Wave
	for i, m := range idx {
		n, _ := strconv.Atoi(text[m[2]:m[3]])
		end := len(text)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		waves = append(waves, Wave{N: n, Title: strings.TrimSpace(text[m[4]:m[5]]), Body: strings.TrimSpace(text[m[1]:end])})
	}
	for i, w := range waves {
		if w.N != i+1 {
			return nil, fmt.Errorf("waves.md: expected Wave %d, found Wave %d", i+1, w.N)
		}
		if !strings.Contains(strings.ToLower(w.Body), "acceptance criteria") {
			return nil, fmt.Errorf("waves.md: Wave %d has no \"Acceptance criteria\" section", w.N)
		}
	}
	return waves, nil
}

func (c *Config) LoadWaves() ([]Wave, error) {
	b, err := os.ReadFile(c.wavesPath())
	if err != nil {
		return nil, err
	}
	return ParseWaves(string(b))
}

func (c *Config) LoadRules() string {
	b, _ := os.ReadFile(c.rulesPath())
	return string(b)
}

// ---- shell + git ----

type RunResult struct {
	Out  string
	Code int
	Err  error
}

func runShell(ctx context.Context, dir, script string, timeout time.Duration) RunResult {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	return RunResult{Out: buf.String(), Code: code, Err: err}
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// gitInput runs git with stdin, for commit-tree's message.
func gitInput(dir, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func gitHead(dir string) string {
	h, _ := git(dir, "rev-parse", "HEAD")
	return h
}

func gitTree(dir string) string {
	h, _ := git(dir, "rev-parse", "HEAD^{tree}")
	return h
}

func gitDirty(dir string) bool {
	s, err := git(dir, "status", "--porcelain")
	return err != nil || s != ""
}

// dirtyBreakdown splits a dirty worktree into untracked and tracked changes.
// They mean different things: a tracked change is the build doing work, an
// untracked file is usually something the gate itself dropped there.
// A git error counts as tracked-dirty, so an unreadable tree is never cached.
func dirtyBreakdown(dir string) (untracked, tracked int) {
	s, err := git(dir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return 0, 1
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "??") {
			untracked++
		} else {
			tracked++
		}
	}
	return untracked, tracked
}

func gitBranch(dir string) string {
	b, _ := git(dir, "rev-parse", "--abbrev-ref", "HEAD")
	return b
}

// commitsSince counts commits in base..HEAD carrying this wave's trailer.
// Resume is derived from git, never from a file a model might forget to update.
func commitsSince(dir, base string, wave int) int {
	if base == "" {
		return 0
	}
	out, err := git(dir, "log", "--format=%(trailers:key=BackCheck-Wave,valueonly)", base+"..HEAD")
	if err != nil {
		return 0
	}
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == strconv.Itoa(wave) {
			n++
		}
	}
	return n
}

func gitLogSince(dir, base string, max int) string {
	if base == "" {
		return ""
	}
	out, _ := git(dir, "log", "--oneline", fmt.Sprintf("-%d", max), base+"..HEAD")
	return out
}
