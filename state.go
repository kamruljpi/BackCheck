package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Status values of the control file. Only the driver moves between them,
// except `resume` (HALTED/NEEDS_OPERATOR -> RUNNING) and `approve` (-> RUNNING).
const (
	StRunning       = "RUNNING"
	StWaiting       = "WAITING"
	StHalted        = "HALTED"
	StNeedsOperator = "NEEDS_OPERATOR"
	StClosed        = "CLOSED"
)

type Control struct {
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Wave     int    `json:"wave"`
	Role     string `json:"role"`    // builder | reviewer
	Session  string `json:"session"` // a, b, c … within the wave (builder sessions)
	Round    int    `json:"round"`   // reviewer rounds on this wave
	BaseSHA  string `json:"base_sha"`
	WaveBase string `json:"wave_base"` // HEAD when the wave started

	ReworkStreak  int        `json:"rework_streak"`
	ReworkHistory [][]string `json:"rework_history,omitempty"` // defect headlines per REWORK round
	Defects       string     `json:"defects,omitempty"`        // last REWORK's defect block, handed to the builder
	LastHandoff   string     `json:"last_handoff,omitempty"`   // path of last builder handoff

	Stalled   int    `json:"stalled"`
	Failures  int    `json:"failures"`
	Retries   int    `json:"retries"`            // consecutive "retry later" rounds
	Sessions  int    `json:"sessions_this_wave"` // dispatches spent on this wave
	WaitUntil string `json:"wait_until,omitempty"`
	RetryNote string `json:"retry_note,omitempty"` // last "retry later" reason, so we say it once

	// provider name -> RFC3339 time it is believed usable again
	ProviderLimited map[string]string `json:"provider_limited,omitempty"`
	// …of those, the ones blocked because the provider rejected our
	// credentials. A weekly limit and a dead key both park a provider for
	// longer than a day, and only one of them fixes itself, so the reason is
	// recorded rather than inferred from how long the block has left to run.
	ProviderAuthFailed map[string]string `json:"provider_auth_failed,omitempty"`

	Updated string `json:"updated"`
}

func (c *Config) controlPath() string { return filepath.Join(c.stateDir, "control.json") }
func (c *Config) ledgerPath() string  { return filepath.Join(c.stateDir, "ledger.jsonl") }
func (c *Config) eventsPath() string  { return filepath.Join(c.stateDir, "events.jsonl") }
func (c *Config) wakePath() string    { return filepath.Join(c.stateDir, "wake") }
func (c *Config) lockPath() string    { return filepath.Join(c.stateDir, "driver.lock") }
func (c *Config) sessionsDir() string { return filepath.Join(c.stateDir, "sessions") }
func (c *Config) wavesPath() string   { return filepath.Join(c.stateDir, "waves.md") }
func (c *Config) rulesPath() string   { return filepath.Join(c.stateDir, "rules.md") }
func (c *Config) notePath() string    { return filepath.Join(c.stateDir, "note.md") }

func (c *Config) LoadControl() (*Control, error) {
	b, err := os.ReadFile(c.controlPath())
	if err != nil {
		return nil, err
	}
	var ctl Control
	if err := json.Unmarshal(b, &ctl); err != nil {
		return nil, fmt.Errorf("control.json: %w", err)
	}
	return &ctl, nil
}

// SaveControl writes atomically so a crash never leaves half a control file.
func (c *Config) SaveControl(ctl *Control) error {
	ctl.Updated = time.Now().Format(time.RFC3339)
	b, _ := json.MarshalIndent(ctl, "", "  ")
	tmp := c.controlPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.controlPath())
}

type LedgerRow struct {
	Wave       int    `json:"wave"`
	SHA        string `json:"sha"`
	VerifiedAt string `json:"verified_at"`
	Rounds     int    `json:"rounds"`
	Reviewer   string `json:"reviewer"` // model that signed
	Review     string `json:"review"`   // path to the signed review
	By         string `json:"by"`       // "reviewer" or "operator"
}

func (c *Config) AppendLedger(r LedgerRow) error {
	return appendJSONL(c.ledgerPath(), r)
}

func (c *Config) Ledger() []LedgerRow {
	var out []LedgerRow
	f, err := os.Open(c.ledgerPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r LedgerRow
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

type Event struct {
	Time  string         `json:"time"`
	Event string         `json:"event"`
	Wave  int            `json:"wave,omitempty"`
	Msg   string         `json:"msg"`
	Data  map[string]any `json:"data,omitempty"`
}

// Emit appends to the event log (the watcher's feed) and, for events that
// need a human, fires the notify command. Notify failures never stop a build.
func (c *Config) Emit(ev string, wave int, msg string, data map[string]any) {
	e := Event{Time: time.Now().Format(time.RFC3339), Event: ev, Wave: wave, Msg: msg, Data: data}
	if err := appendJSONL(c.eventsPath(), e); err != nil {
		fmt.Fprintln(os.Stderr, "backcheck: event log:", err)
	}
	fmt.Printf("[%s] %-15s w%d %s\n", time.Now().Format("15:04:05"), ev, wave, msg)
	if c.isHumanEvent(ev) {
		go func() {
			if err := c.Notify1(ev, wave, msg, 15*time.Second); err != nil {
				// Never fatal — but never silent either. An unheard HALT is
				// the one failure this whole mechanism exists to prevent.
				fmt.Fprintln(os.Stderr, "backcheck: notify failed:", err)
			}
		}()
	}
}

// Notify1 fires the operator's alert command once. The event name and message
// are appended as the last two arguments (so `notify-send -a backcheck` gets
// them as summary and body) and are also exported as BACKCHECK_EVENT,
// BACKCHECK_MSG and BACKCHECK_WAVE for a wrapper script to use instead.
//
// wait > 0 collects the exit status; the command is still not allowed to hold
// the build up, so outliving the wait counts as success.
func (c *Config) Notify1(ev string, wave int, msg string, wait time.Duration) error {
	if len(c.Notify.Command) == 0 {
		return nil
	}
	args := append(append([]string{}, c.Notify.Command[1:]...), ev, msg)
	cmd := exec.Command(c.Notify.Command[0], args...)
	cmd.Env = append(os.Environ(),
		"BACKCHECK_EVENT="+ev, "BACKCHECK_MSG="+msg, fmt.Sprintf("BACKCHECK_WAVE=%d", wave))
	var errb bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &errb, n: 8 << 10}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", c.Notify.Command[0], err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if wait <= 0 {
		return nil
	}
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %v: %s", c.Notify.Command[0], err, firstLine(strings.TrimSpace(errb.String())))
		}
		return nil
	case <-time.After(wait):
		return nil // started and still running: slow, not broken
	}
}

// Spend sums what the build has cost from the event log, which is the only
// durable record of it — control.json is rewritten constantly and a session's
// stream log can be pruned.
type Spend struct {
	USD      float64            `json:"usd"`
	Sessions int                `json:"sessions"`
	ByRole   map[string]float64 `json:"by_role,omitempty"`
}

func (c *Config) Spend() Spend {
	s := Spend{ByRole: map[string]float64{}}
	f, err := os.Open(c.eventsPath())
	if err != nil {
		return s
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Event != "dispatch-result" {
			continue
		}
		s.Sessions++
		cost, _ := e.Data["cost_usd"].(float64)
		s.USD += cost
		if role, ok := e.Data["role"].(string); ok && role != "" {
			s.ByRole[role] += cost
		}
	}
	return s
}

func appendJSONL(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// sleepInterruptible sleeps for d, returning early (true) if `backcheck wake`
// dropped the wake file.
func (c *Config) sleepInterruptible(d time.Duration) (woken bool) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c.consumeWake() {
			return true
		}
		step := 2 * time.Second
		if rem := time.Until(deadline); rem < step {
			step = rem
		}
		time.Sleep(step)
	}
	return c.consumeWake()
}

func (c *Config) consumeWake() bool {
	if _, err := os.Stat(c.wakePath()); err == nil {
		_ = os.Remove(c.wakePath())
		return true
	}
	return false
}

// DriverLock holds an exclusive flock so only one driver runs per build.
type DriverLock struct{ f *os.File }

func (c *Config) AcquireDriverLock() (*DriverLock, error) {
	f, err := os.OpenFile(c.lockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another driver already holds %s", c.lockPath())
	}
	_ = f.Truncate(0)
	_, _ = f.WriteString(fmt.Sprintf("%d\n", os.Getpid()))
	return &DriverLock{f}, nil
}

func (l *DriverLock) Release() {
	if l != nil && l.f != nil {
		_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
		l.f.Close()
	}
}

// DriverAlive tests the lock without taking it.
func (c *Config) DriverAlive() bool {
	f, err := os.OpenFile(c.lockPath(), os.O_RDWR, 0o644)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
