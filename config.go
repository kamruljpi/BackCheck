package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Provider is one Anthropic-compatible endpoint + the CLI used to talk to it.
// The command receives the prompt on stdin and must emit Claude Code
// stream-json lines (claude -p --output-format stream-json --verbose).
type Provider struct {
	Command     []string          `json:"command"`
	Model       string            `json:"model,omitempty"`
	ExpectModel string            `json:"expect_model,omitempty"` // substring the reported model must contain
	Env         map[string]string `json:"env,omitempty"`          // literal env vars
	EnvFrom     map[string]string `json:"env_from,omitempty"`     // VAR -> name of the env var to copy the value from
	Probe       bool              `json:"probe"`
}

type Role struct {
	Provider string `json:"provider"`
	Fallback string `json:"fallback,omitempty"`
}

type Check struct {
	Name string `json:"name"`
	Run  string `json:"run"`
}

type Limits struct {
	SessionWallSeconds   int    `json:"session_wall_seconds"`
	SessionIdleSeconds   int    `json:"session_idle_seconds"`
	ProbeTimeoutSeconds  int    `json:"probe_timeout_seconds"`
	MaxConsecutiveRework int    `json:"max_consecutive_rework"`
	MaxStalledSessions   int    `json:"max_stalled_sessions"`
	MaxSessionFailures   int    `json:"max_session_failures"`
	RetryBackoffSeconds  int    `json:"retry_backoff_seconds"`
	UnknownWaitSeconds   int    `json:"unknown_wait_seconds"` // rate limit with no named reset
	ResetMarginSeconds   int    `json:"reset_margin_seconds"` // added to a named reset before retrying
	PollSeconds          int    `json:"poll_seconds"`         // how often an idle driver re-reads control
	StepsPerSession      string `json:"steps_per_session"`
	MinFreeDiskMB        int    `json:"min_free_disk_mb"`
	GateOutputChars      int    `json:"gate_output_chars"`

	// MaxSessionsPerWave caps how much a single wave may spend before a human
	// looks at it. A wave that keeps going without finishing is usually too
	// big or badly specified, and neither fixes itself.
	MaxSessionsPerWave int `json:"max_sessions_per_wave"`
	// MaxRetriesBeforeOperator bounds "retry later". Without it a build that
	// cannot pass preflight spins at the backoff interval forever, saying so
	// exactly once — silent, unattended, and going nowhere.
	MaxRetriesBeforeOperator int `json:"max_retries_before_operator"`
}

type Notify struct {
	Command     []string `json:"command,omitempty"` // event name + message appended as args
	HumanEvents []string `json:"human_events,omitempty"`
}

type Config struct {
	Worktree      string              `json:"worktree"` // build tree, relative to repo root
	Branch        string              `json:"branch"`
	Providers     map[string]Provider `json:"providers"`
	Roles         map[string]Role     `json:"roles"` // builder, reviewer, planner
	Gate          []Check             `json:"gate"`
	Preflight     []Check             `json:"preflight"`
	ReadonlyPaths []string            `json:"readonly_paths"`
	Fingerprints  []Check             `json:"fingerprints"`
	Limits        Limits              `json:"limits"`
	Notify        Notify              `json:"notify"`

	// resolved at load time
	root     string // main repo root (where .backcheck lives)
	stateDir string
}

func (c *Config) WorktreePath() string {
	if filepath.IsAbs(c.Worktree) {
		return c.Worktree
	}
	return filepath.Clean(filepath.Join(c.root, c.Worktree))
}

func defaultConfig() Config {
	claude := []string{"claude", "-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}
	return Config{
		Worktree: "../" + "PROJECT-backcheck",
		Branch:   "backcheck/build",
		Providers: map[string]Provider{
			"primary": {Command: claude, Model: "sonnet", Probe: true},
			"judge": {Command: claude, Model: "opus", Probe: true,
				Env:     map[string]string{"ANTHROPIC_BASE_URL": "https://api.anthropic.com"},
				EnvFrom: map[string]string{"ANTHROPIC_API_KEY": "BACKCHECK_JUDGE_KEY"}},
		},
		Roles: map[string]Role{
			"builder":  {Provider: "primary"},
			"reviewer": {Provider: "judge", Fallback: ""},
			"planner":  {Provider: "judge"},
		},
		Gate:          []Check{{Name: "tests", Run: "echo 'set your test command in .backcheck/config.json'"}},
		Preflight:     []Check{},
		ReadonlyPaths: []string{},
		Fingerprints:  []Check{},
		Limits:        defaultLimits(),
		Notify:        Notify{HumanEvents: []string{"halt", "needs-operator", "closed", "model-mismatch"}},
	}
}

func defaultLimits() Limits {
	return Limits{
		SessionWallSeconds:   90 * 60,
		SessionIdleSeconds:   15 * 60,
		ProbeTimeoutSeconds:  90,
		MaxConsecutiveRework: 4,
		MaxStalledSessions:   2,
		MaxSessionFailures:   5,
		RetryBackoffSeconds:  300,
		UnknownWaitSeconds:   1800,
		ResetMarginSeconds:   30,
		PollSeconds:          10,
		StepsPerSession:      "5 to 8",
		MinFreeDiskMB:        2048,
		GateOutputChars:      6000,

		MaxSessionsPerWave:       40,
		MaxRetriesBeforeOperator: 20,
	}
}

func (l *Limits) fillDefaults() {
	d := defaultLimits()
	set := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	set(&l.SessionWallSeconds, d.SessionWallSeconds)
	set(&l.SessionIdleSeconds, d.SessionIdleSeconds)
	set(&l.ProbeTimeoutSeconds, d.ProbeTimeoutSeconds)
	set(&l.MaxConsecutiveRework, d.MaxConsecutiveRework)
	set(&l.MaxStalledSessions, d.MaxStalledSessions)
	set(&l.MaxSessionFailures, d.MaxSessionFailures)
	set(&l.RetryBackoffSeconds, d.RetryBackoffSeconds)
	set(&l.UnknownWaitSeconds, d.UnknownWaitSeconds)
	set(&l.ResetMarginSeconds, d.ResetMarginSeconds)
	set(&l.PollSeconds, d.PollSeconds)
	set(&l.GateOutputChars, d.GateOutputChars)
	// Both default ON: a config written before they existed should gain the
	// bound, not silently keep the unbounded behaviour. Raise them to loosen.
	set(&l.MaxSessionsPerWave, d.MaxSessionsPerWave)
	set(&l.MaxRetriesBeforeOperator, d.MaxRetriesBeforeOperator)
	if l.StepsPerSession == "" {
		l.StepsPerSession = d.StepsPerSession
	}
	// MinFreeDiskMB may legitimately be 0 (disabled); negative means default.
	if l.MinFreeDiskMB < 0 {
		l.MinFreeDiskMB = d.MinFreeDiskMB
	}
}

// findRoot walks up from dir to the directory holding .backcheck/.
func findRoot(dir string) (string, error) {
	d, _ := filepath.Abs(dir)
	for {
		if st, err := os.Stat(filepath.Join(d, ".backcheck")); err == nil && st.IsDir() {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("no .backcheck/ directory found above %s — run `backcheck init` in your main repo", dir)
		}
		d = parent
	}
}

func LoadConfig(root string) (*Config, error) {
	stateDir := filepath.Join(root, ".backcheck")
	b, err := os.ReadFile(filepath.Join(stateDir, "config.json"))
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	c.root = root
	c.stateDir = stateDir
	c.Limits.fillDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []string
	for _, r := range []string{"builder", "reviewer"} {
		role, ok := c.Roles[r]
		if !ok {
			errs = append(errs, "roles."+r+" missing")
			continue
		}
		if _, ok := c.Providers[role.Provider]; !ok {
			errs = append(errs, fmt.Sprintf("roles.%s.provider %q not in providers", r, role.Provider))
		}
		if role.Fallback != "" {
			if _, ok := c.Providers[role.Fallback]; !ok {
				errs = append(errs, fmt.Sprintf("roles.%s.fallback %q not in providers", r, role.Fallback))
			}
		}
	}
	for name, p := range c.Providers {
		if len(p.Command) == 0 {
			errs = append(errs, "providers."+name+".command empty")
		}
	}
	if c.Branch == "" {
		errs = append(errs, "branch empty")
	}
	if len(errs) > 0 {
		return fmt.Errorf("config.json: %s", strings.Join(errs, "; "))
	}
	return nil
}

// SingleProvider reports whether builder and reviewer share a provider —
// the adversarial property is weaker and we say so at every start.
func (c *Config) SingleProvider() bool {
	return c.Roles["builder"].Provider == c.Roles["reviewer"].Provider
}

// authRelevantEnv decides *which account* a provider's command reaches. Two
// providers that agree on every one of these, and on the binary, land in the
// same place no matter what we called them in config.json.
var authRelevantEnv = []string{
	"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
	"AWS_PROFILE", "AWS_REGION", "ANTHROPIC_VERTEX_PROJECT_ID",
}

// accountFingerprint hashes where a provider's credentials point — never the
// credentials themselves, so it is safe to log. The model is deliberately not
// part of it: changing the model does not change whose quota you spend.
func (p Provider) accountFingerprint() string {
	h := sha256.New()
	if len(p.Command) > 0 {
		h.Write([]byte(p.Command[0]))
	}
	h.Write([]byte{0})
	env := providerEnv(p) // literal env + env_from, resolved over the real environment
	for _, k := range authRelevantEnv {
		h.Write([]byte(k + "=" + lastEnvValue(env, k) + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// lastEnvValue reads a variable the way exec does: the last assignment wins.
func lastEnvValue(env []string, key string) string {
	pre := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], pre) {
			return env[i][len(pre):]
		}
	}
	return ""
}

// SharedAccount catches the quieter version of SingleProvider: two providers
// with different names and different models that nonetheless reach the same
// endpoint with the same credentials. A logged-in CLI ignores ANTHROPIC_API_KEY
// outright, so a reviewer "configured" with a second key can spend the
// builder's own quota and nothing in the config looks wrong.
func (c *Config) SharedAccount() bool {
	b, r := c.Roles["builder"].Provider, c.Roles["reviewer"].Provider
	if b == r {
		return false // SingleProvider already says this, and says it better
	}
	pb, okb := c.Providers[b]
	pr, okr := c.Providers[r]
	if !okb || !okr {
		return false
	}
	return pb.accountFingerprint() == pr.accountFingerprint()
}

// sharedAccountWarning says which of the two shapes this is, because they are
// not equally bad. Two models on one account still buy a second opinion; the
// same model twice buys only a second run of the proofs, and the warning must
// not claim a difference that is not there.
func (c *Config) sharedAccountWarning() string {
	const lead = "builder and reviewer are different providers but resolve to the same account — " +
		"same endpoint, same credentials, so one quota pool and one outage"
	b := c.Providers[c.Roles["builder"].Provider]
	r := c.Providers[c.Roles["reviewer"].Provider]
	if strings.EqualFold(b.Model, r.Model) {
		return lead + ", running the same model on both sides. The reviewer re-runs the proofs, " +
			"which is worth having, but it brings no second opinion: one model family is wrong " +
			"about the same things twice."
	}
	return lead + ". Only their models differ."
}

func (c *Config) isHumanEvent(ev string) bool {
	for _, e := range c.Notify.HumanEvents {
		if e == ev {
			return true
		}
	}
	return false
}
