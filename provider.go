package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type OutcomeKind string

const (
	OutOK        OutcomeKind = "ok"
	OutRateLimit OutcomeKind = "rate-limit" // may carry ResetAt
	OutTransient OutcomeKind = "transient"  // overloaded, 5xx, network — retry soon
	OutAuth      OutcomeKind = "auth"       // will not fix itself
	OutTimeout   OutcomeKind = "timeout"    // wall clock watchdog
	OutIdle      OutcomeKind = "idle"       // no output for too long
	OutFailed    OutcomeKind = "failed"     // fail-safe: unrecognised
)

type Outcome struct {
	Kind    OutcomeKind
	Text    string // final result text (the handoff / the review)
	Model   string // model the CLI reported in its init line
	ResetAt *time.Time
	Detail  string
	Cost    float64
	Exit    int
}

type streamLine struct {
	Type    string  `json:"type"`
	Subtype string  `json:"subtype"`
	Model   string  `json:"model"`
	Result  string  `json:"result"`
	IsError bool    `json:"is_error"`
	CostUSD float64 `json:"total_cost_usd"`

	// system/api_retry — the CLI swallows HTTP failures and retries them
	// (ten times, with growing backoff). These lines are the only place the
	// status ever appears, so a session that dies on a 401 or a 429 looks
	// like silence unless we read them.
	Attempt     int             `json:"attempt"`
	MaxRetries  int             `json:"max_retries"`
	ErrorStatus int             `json:"error_status"`
	APIError    json.RawMessage `json:"error"`

	// result — set when the CLI gave up on the API rather than on the task.
	// subtype is "success" even for a failure, so neither of these is optional.
	APIErrorStatus json.RawMessage `json:"api_error_status"`
	TerminalReason json.RawMessage `json:"terminal_reason"`

	// rate_limit_event — the account's limit windows, mirrored from the
	// anthropic-ratelimit-unified-* response headers. This is the only place a
	// limit is stated as data rather than as prose, and the only place the
	// reset is exact, so it outranks every regex below.
	RateLimitInfo *struct {
		Status        string `json:"status"`        // allowed | allowed_warning | rejected
		RateLimitType string `json:"rateLimitType"` // five_hour | seven_day | seven_day_opus | overage
		ResetsAt      int64  `json:"resetsAt"`
	} `json:"rate_limit_info"`

	Message *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// apiErrorNote renders one api_retry / failed result line into something the
// classifier's patterns can match: "401 authentication_failed".
func (sl streamLine) apiErrorNote() string {
	status := sl.ErrorStatus
	var parts []string
	if status != 0 {
		parts = append(parts, strconv.Itoa(status))
	} else if s := jsonScalar(sl.APIErrorStatus); s != "" {
		parts = append(parts, s)
	}
	if s := jsonScalar(sl.APIError); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// jsonScalar unwraps a JSON string/number into plain text, and renders
// anything else verbatim. null and absent both come back empty.
func jsonScalar(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	return s
}

// Dispatch runs one session: prompt on stdin, stream-json on stdout, a wall
// clock and an idle watchdog armed. The raw stream is kept in logPath.
func Dispatch(ctx context.Context, p Provider, prompt, dir string, wall, idle time.Duration, logPath string) Outcome {
	args := append([]string{}, p.Command[1:]...)
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	cmd := exec.Command(p.Command[0], args...)
	cmd.Dir = dir
	cmd.Env = providerEnv(p)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Outcome{Kind: OutFailed, Detail: err.Error()}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 64 << 10}

	var logw io.Writer = io.Discard
	if logPath != "" {
		if f, err := os.Create(logPath); err == nil {
			defer f.Close()
			logw = f
		}
	}

	if err := cmd.Start(); err != nil {
		return Outcome{Kind: OutFailed, Detail: "start: " + err.Error()}
	}

	var (
		mu       sync.Mutex
		lastSeen = time.Now()
		o        Outcome
		texts    []string
		rawTail  []string
		apiErrs  []string // what api_retry lines said, newest last
		gotRes   bool

		limited     bool // a rate_limit_event said "rejected"
		limitReset  *time.Time
		limitDetail string
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			line := sc.Bytes()
			fmt.Fprintf(logw, "%s\n", line)
			mu.Lock()
			lastSeen = time.Now()
			var sl streamLine
			if json.Unmarshal(line, &sl) == nil && sl.Type != "" {
				// "allowed" and "allowed_warning" are the ordinary case — the
				// request went through. Only a rejection stops a session.
				if rl := sl.RateLimitInfo; rl != nil && rl.Status == "rejected" {
					limited = true
					limitDetail = "rate_limit_event: " + firstNonEmpty(rl.RateLimitType, "account") + " limit rejected the request"
					if rl.ResetsAt > 0 {
						t := time.Unix(rl.ResetsAt, 0)
						limitReset = &t
						limitDetail += ", resets " + t.Format("15:04 MST")
					}
				}
				switch sl.Type {
				case "system":
					if sl.Model != "" {
						o.Model = sl.Model
					}
					if sl.Subtype == "api_retry" {
						if note := sl.apiErrorNote(); note != "" {
							apiErrs = appendCapped(apiErrs, fmt.Sprintf("api_retry %d/%d: %s", sl.Attempt, sl.MaxRetries, note), 20)
						}
					}
				case "assistant":
					if sl.Message != nil {
						for _, c := range sl.Message.Content {
							if c.Type == "text" && c.Text != "" {
								texts = append(texts, c.Text)
							}
						}
					}
				case "result":
					gotRes = true
					o.Text = sl.Result
					o.Cost = sl.CostUSD
					if note := sl.apiErrorNote(); note != "" {
						apiErrs = appendCapped(apiErrs, "result "+sl.Subtype+": "+note, 20)
					}
					if sl.IsError {
						// subtype stays "success" even here; terminal_reason is
						// the field that actually says what ended the run.
						o.Kind = OutFailed
						o.Detail = firstNonEmpty(jsonScalar(sl.TerminalReason), sl.Subtype)
					}
				}
			} else {
				rawTail = append(rawTail, string(line))
				if len(rawTail) > 50 {
					rawTail = rawTail[1:]
				}
			}
			mu.Unlock()
		}
	}()

	var killedBy OutcomeKind
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	wallT := time.NewTimer(wall)
	defer wallT.Stop()
loop:
	for {
		select {
		case <-done:
			break loop
		case <-ctx.Done():
			killedBy = OutFailed
			killGroup(cmd)
		case <-wallT.C:
			killedBy = OutTimeout
			killGroup(cmd)
		case <-tick.C:
			mu.Lock()
			quiet := time.Since(lastSeen)
			mu.Unlock()
			if idle > 0 && quiet > idle && killedBy == "" {
				killedBy = OutIdle
				killGroup(cmd)
			}
		}
	}
	werr := cmd.Wait()
	if ee, ok := werr.(*exec.ExitError); ok {
		o.Exit = ee.ExitCode()
	} else if werr != nil {
		o.Exit = -1
	}

	apiBlob := strings.Join(apiErrs, "\n")

	// 1 · watchdog. A kill normally tells us nothing about why — but if the
	// CLI was sitting in its own retry loop on a 401 or a 429, it told us the
	// status before we killed it, and that answer outranks the stopwatch.
	// Otherwise a bad key looks like a slow session, forever.
	if killedBy != "" {
		o.Kind = killedBy
		o.Detail = fmt.Sprintf("killed by %s watchdog", killedBy)
		if limited {
			o.Kind, o.ResetAt = OutRateLimit, limitReset
			o.Detail = limitDetail + fmt.Sprintf(" (still waiting when the %s watchdog fired)", killedBy)
		} else if k, reset := classifyText(apiBlob, time.Now()); k != "" && k != OutTransient {
			o.Kind, o.ResetAt = k, reset
			o.Detail = fmt.Sprintf("%s, still retrying when the %s watchdog fired", firstLine(lastOf(apiErrs)), killedBy)
		}
		return o
	}
	if o.Text == "" && len(texts) > 0 {
		o.Text = texts[len(texts)-1]
	}
	// 2 · the result object
	if gotRes && o.Kind == "" && o.Exit == 0 {
		o.Kind = OutOK
		return o
	}
	// 3 · the limit stated as data, with an exact reset — better than any
	// wording we could pattern-match for, so it comes before the patterns.
	if limited {
		o.Kind, o.ResetAt, o.Detail = OutRateLimit, limitReset, limitDetail
		return o
	}
	// 4 · patterns over everything the provider said
	blob := strings.Join([]string{o.Text, stderr.String(), apiBlob, strings.Join(rawTail, "\n")}, "\n")
	if k, reset := classifyText(blob, time.Now()); k != "" {
		o.Kind, o.ResetAt = k, reset
		o.Detail = firstLine(strings.TrimSpace(blob))
		return o
	}
	// 5 · fail-safe
	o.Kind = OutFailed
	if o.Detail == "" {
		o.Detail = fmt.Sprintf("exit %d, no result object: %s", o.Exit, firstLine(strings.TrimSpace(blob)))
	}
	return o
}

// Probe spends one tiny request to learn whether the provider is awake —
// seconds, instead of a doomed hour-long session.
func ProbeProvider(ctx context.Context, p Provider, dir string, timeout time.Duration) Outcome {
	o := Dispatch(ctx, p, "Health check. Reply with exactly: OK", dir, timeout, timeout, "")
	return o
}

var (
	reUnix       = regexp.MustCompile(`\|(\d{10})\b`)
	reInMinutes  = regexp.MustCompile(`(?i)(?:in|after)\s+(\d+)\s*(second|sec|s|minute|min|m|hour|hr|h)s?\b`)
	reRetryAfter = regexp.MustCompile(`(?i)retry-after:?\s*(\d+)`)
	reClock      = regexp.MustCompile(`(?i)resets?\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)
	// "resets Sep 26, 1:20am (Asia/Dhaka)" — how the CLI names a reset more
	// than 24h out, which is every weekly limit.
	reDateClock = regexp.MustCompile(`(?i)resets?\s+(?:at\s+)?(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\s+(\d{1,2})(?:,\s*(\d{4}))?,?\s*(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)

	// The wordings the installed CLI really prints (claude 2.1.280). It names
	// the window rather than the mechanism — "You've hit your session limit"
	// contains none of "rate limit", "usage limit", "limit reached" or "429",
	// so without these a limit read as an ordinary failure and the build spent
	// its failure budget halting instead of waiting for the reset.
	reRate = regexp.MustCompile(`(?i)(rate.?limit|usage limit|limit reached|too many requests|\b429\b|quota` +
		`|you've (?:hit|used)[^.\n]{0,60}\blimit\b` +
		`|approaching[^.\n]{0,40}\blimit\b` +
		`|\b(?:session|weekly|opus|sonnet|fable|usage credit|spend)\s+limit\b)`)

	// …but not every "limit reached" is a limit you can wait out. These never
	// reset on a clock, so waiting on one would idle the build for nothing.
	reNotRate   = regexp.MustCompile(`(?i)\b(context|concurrent subagent|subagent|device|tool output)\s+limit\b`)
	reTransient = regexp.MustCompile(`(?i)(overloaded|\b529\b|\b50[0234]\b|internal server error|bad gateway|service unavailable|timed? ?out|connection (?:reset|refused)|ECONNRESET|ETIMEDOUT|network error|socket hang up)`)
	reAuth      = regexp.MustCompile(`(?i)(invalid api key|authentication_error|\b401\b|\b403\b|unauthori[sz]ed|permission_error|credit balance is too low)`)
)

func classifyText(s string, now time.Time) (OutcomeKind, *time.Time) {
	switch {
	case reRate.MatchString(s) && !reNotRate.MatchString(s):
		return OutRateLimit, parseReset(s, now)
	case reAuth.MatchString(s):
		return OutAuth, nil
	case reTransient.MatchString(s):
		return OutTransient, nil
	}
	return "", nil
}

// parseReset understands the ways providers name a reset: a unix stamp
// ("…limit reached|1712345678"), a relative delay, or a wall-clock time.
func parseReset(s string, now time.Time) *time.Time {
	if m := reUnix.FindStringSubmatch(s); m != nil {
		sec, _ := strconv.ParseInt(m[1], 10, 64)
		t := time.Unix(sec, 0)
		return &t
	}
	if m := reRetryAfter.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		t := now.Add(time.Duration(n) * time.Second)
		return &t
	}
	if m := reInMinutes.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := time.Minute
		switch strings.ToLower(m[2])[0] {
		case 's':
			unit = time.Second
		case 'h':
			unit = time.Hour
		}
		t := now.Add(time.Duration(n) * unit)
		return &t
	}
	if m := reDateClock.FindStringSubmatch(s); m != nil {
		day, _ := strconv.Atoi(m[2])
		year := now.Year()
		if m[3] != "" {
			year, _ = strconv.Atoi(m[3])
		}
		h, _ := strconv.Atoi(m[4])
		min := 0
		if m[5] != "" {
			min, _ = strconv.Atoi(m[5])
		}
		h = to24h(h, m[6])
		if mon, ok := months[strings.ToLower(m[1])]; ok && h <= 23 && min <= 59 {
			t := time.Date(year, mon, day, h, min, 0, 0, now.Location())
			// No year given and the date already passed ⇒ it means next year.
			if m[3] == "" && t.Before(now.Add(-24*time.Hour)) {
				t = t.AddDate(1, 0, 0)
			}
			return &t
		}
	}
	if m := reClock.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		min := 0
		if m[2] != "" {
			min, _ = strconv.Atoi(m[2])
		}
		h = to24h(h, m[3])
		if h > 23 || min > 59 {
			return nil
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		return &t
	}
	return nil
}

var months = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April,
	"may": time.May, "jun": time.June, "jul": time.July, "aug": time.August,
	"sep": time.September, "oct": time.October, "nov": time.November, "dec": time.December,
}

func to24h(h int, meridiem string) int {
	switch strings.ToLower(meridiem) {
	case "pm":
		if h < 12 {
			h += 12
		}
	case "am":
		if h == 12 {
			h = 0
		}
	}
	return h
}

func providerEnv(p Provider) []string {
	env := os.Environ()
	for k, v := range p.Env {
		env = append(env, k+"="+v)
	}
	for k, from := range p.EnvFrom {
		// An unset source used to inject "VAR=", which does not fall through to
		// the ambient value — it blanks it. A typo in env_from would silently
		// strip the credential the provider was meant to use.
		if v := os.Getenv(from); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	go func(pid int) {
		time.Sleep(5 * time.Second)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}(cmd.Process.Pid)
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	_, _ = l.w.Write(q)
	return len(p), nil
}

// appendCapped keeps the newest max entries; a hundred retry lines would
// drown the one line an operator needs to read.
func appendCapped(xs []string, s string, max int) []string {
	xs = append(xs, s)
	if len(xs) > max {
		xs = xs[len(xs)-max:]
	}
	return xs
}

func firstNonEmpty(xs ...string) string {
	for _, s := range xs {
		if s != "" {
			return s
		}
	}
	return ""
}

func lastOf(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	return xs[len(xs)-1]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
