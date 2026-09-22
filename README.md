# BackCheck

An unattended build harness. An implementation plan goes in; committed, reviewed
code comes out. One model **builds**, a different model **reviews** by re-running
the proofs, and the whole thing runs while you are somewhere else.

Single Go binary, standard library only.

---

## Requirements

| | To build BackCheck | To run BackCheck |
|---|---|---|
| Linux or macOS | yes | yes |
| `git` | yes | **yes** — builds are driven entirely through git |
| A CLI that speaks Claude Code `stream-json` (normally `claude`) | no | **yes** |
| Go 1.21 or newer | **yes** | no |
| `notify-send` (Linux) / `osascript` (macOS) | no | optional — desktop alerts |

**Supported platforms: Linux and macOS only**, on `amd64` or `arm64`. BackCheck
uses `flock` and POSIX process groups, so Windows is not supported (WSL2 works,
since that is Linux).

Go is needed to *build* the binary, not to *run* it: it is statically linked and
depends on nothing — no Go, no libc, no runtime. Build it on one machine and copy
it to another that has no toolchain.

---

## Install

```bash
git clone <this repo> && cd backcheck
./install.sh
```

The script checks the machine, runs the test suite, builds the binary and installs
it to `~/.local/bin` (or `/usr/local/bin` if that is what is on your `PATH`). It
refuses to install a binary that fails its own tests.

```
Checking this machine
  ok    Linux 7.0.0-31-generic
  ok    go 1.27.1
  ok    git version 2.53.0
  ok    claude 2.1.280 (Claude Code)
  ok    notify-send (desktop alerts for HALT and NEEDS_OPERATOR)
```

| flag | |
|---|---|
| `--check` | report what is missing and install nothing |
| `--prefix DIR` | install somewhere other than `~/.local/bin` |
| `--no-test` | skip the test suite |
| `--binary FILE` | install an already-built binary — **needs no Go** |

### Installing on a machine without Go

```bash
# on a machine that has Go
make dist
  dist/backcheck-linux-amd64     4.6M
  dist/backcheck-linux-arm64     4.4M
  dist/backcheck-darwin-amd64    4.6M
  dist/backcheck-darwin-arm64    4.3M

# copy the one file to the target machine, then
./install.sh --binary ./backcheck-linux-amd64
```

### Make targets

```bash
make check      # go vet ./... && go test ./...
make build      # ./backcheck
make dist       # static binaries for linux/macOS on amd64 + arm64
make install    # wraps ./install.sh
make uninstall  # removes the installed binary
```

If `~/.local/bin` is not on your `PATH`, add it:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

---

## Usage

### 1. Point it at a repo

```bash
cd ~/code/myapp        # a git repo with at least one commit
backcheck init
```

This writes `.backcheck/config.json` and git-excludes `/.backcheck/`, so none of
BackCheck's state is ever committed. The build itself happens in a separate git
worktree (`../myapp-backcheck` by default) on branch `backcheck/build` — your
working tree is never touched.

### 2. Write `providers` — the one part that is yours

Open `.backcheck/config.json`. Nothing automates this step, because it decides
**which model runs, with whose credentials, and what it may do to your machine.**

```jsonc
"providers": {
  "builder-sonnet": {
    "command": ["claude", "-p", "--output-format", "stream-json", "--verbose",
                "--permission-prompts", "none",
                "--allowedTools", "Bash Read Write Edit Glob Grep"],
    "model": "sonnet",
    "expect_model": "claude-sonnet-5",
    "probe": true
  },
  "judge-opus": {
    "command": ["claude", "-p", "--output-format", "stream-json", "--verbose",
                "--permission-prompts", "none",
                "--allowedTools", "Bash Read Glob Grep"],
    "model": "opus",
    "expect_model": "claude-opus-5",
    "probe": true
  }
},
"roles": {
  "builder":  { "provider": "builder-sonnet" },
  "reviewer": { "provider": "judge-opus" },
  "planner":  { "provider": "judge-opus" }
}
```

- **The reviewer has no `Write` or `Edit`.** A reviewer that changes the tree
  halts the build; leaving the tools out means it cannot happen at all.
- **`--permission-prompts none`** makes it unattended without a blanket bypass:
  anything that would prompt is denied rather than hanging.
- **`expect_model`** catches a provider that quietly ran the wrong model.

Keys never go in this file — name an environment variable instead:

```jsonc
"env_from": { "ANTHROPIC_AUTH_TOKEN": "MY_JUDGE_KEY" }
```

> A logged-in `claude` CLI **ignores `ANTHROPIC_API_KEY`**. If you are logged in,
> giving the reviewer "its own key" silently does nothing and it runs on your main
> account. A different `ANTHROPIC_BASE_URL` is honoured. BackCheck warns when both
> roles resolve to the same account.

See `config.example.json` for the full file.

### 3. Say what to build

```bash
backcheck auto "a CLI that counts words in stdin; wave 2 adds a --json flag with tests"
```

The planner drafts the waves, the rules and the rails; they are linted; you are
shown what they are and asked to approve:

```
drafted in .backcheck/draft
  wave 1  Count words from stdin
  wave 2  The --json flag
  gate        gofmt, build, vet, test, stdlib-only
  preflight   go-toolchain, module-is-wordcount
  fingerprint go-version
  read-only   README.md, go.mod

rails lint: 2 thing(s) to look at before this runs unattended
  ! preflight.no-uncommitted-changes   a session killed mid-edit leaves the tree
                                       dirty, and this can then never pass again …
  ! fingerprints.modcache-entries      samples a shared build cache; anything else
                                       on this machine moves it, and that HALTs …
Approve these rails and start a build? [y/N]
```

**Read the rails before saying yes.** The planner cannot bless its own rails.
Answering `n` leaves the draft in place — edit it and re-run with `--reuse-draft`.

| flag | |
|---|---|
| `--plan FILE` | take the plan from a file instead of the arguments (stdin works too) |
| `--edit` | open the draft in `$EDITOR`, then lint what comes back |
| `--reuse-draft` | use `.backcheck/draft` as it stands, spending no planner session |
| `--yes` | do not ask |
| `--no-run` | stop after `rehearse` |

`auto` is a convenience — each step still exists on its own:

```bash
backcheck plan PLAN.md      # planner → .backcheck/draft/{waves.md,rules.md,rails.json}
$EDITOR .backcheck/draft/*
backcheck approve           # merge the rails, cut the worktree
backcheck rehearse          # probe everything, spend no session
backcheck keeper            # run it
```

### 4. Read the rehearsal

`auto` runs this for you and refuses to start if it fails.

```
[ok  ] waves.md               2 waves
[ok  ] control.json
[ok  ] preflight
[ok  ] provider builder-sonnet ok model=claude-sonnet-5
[ok  ] provider judge-opus     ok model=claude-opus-5-5
[ok  ] notify                  notify-send -a backcheck -u critical
[ok  ] fingerprints            2 sampled
```

Rehearsal answers every cheap question before any money is spent: are the providers
awake, is the model the one you asked for, does the gate run, **does a desktop alert
actually appear**. It fires a real notification — if you do not see one, you would
not have seen the HALT either.

### 5. Let it run

```bash
nohup backcheck keeper > .backcheck/keeper.log 2>&1 &
```

The keeper restarts a dead driver and stands down by itself when the build closes.
BackCheck then alternates builder and reviewer sessions, wave by wave, until every
wave is signed.

```bash
backcheck watch --interval 30 --quiet 1800
```

One line per change; silence means nothing happened. It exits on its own when the
build closes.

### 6. Check on it

```bash
backcheck status          # where it is, what it has cost
backcheck status --json   # the same, for scripts
backcheck review          # the latest review, in full
backcheck review 2        # wave 2's review
```

```
status   RUNNING
wave     2/2 The --json flag
next     reviewer (session a, review round 1, rework streak 0)
driver   alive=true   head 6d7ec216   commits this wave: 1
spend    $1.5320 over 5 session(s)   (builder $0.5600, reviewer $0.6536)   this wave: 2/40
```

### 7. Steer it

Everything below takes effect before the next session — no restart, no resume.
The rails are re-read every iteration.

| | |
|---|---|
| `backcheck note "…"` | one-shot guidance, delivered to exactly one session |
| edit `.backcheck/rules.md` | change how the builder and reviewer must work |
| edit `.backcheck/waves.md` | re-scope a wave that is not landing |
| edit `.backcheck/config.json` | change the gate, limits, read-only paths |
| `backcheck wake` | stop waiting on a limit and try now |

### 8. When it stops

BackCheck stops rather than guessing. `halt`, `needs-operator`, `closed` and
`model-mismatch` also raise a desktop alert.

| status | what happened | what to do |
|---|---|---|
| `WAITING` | rate limited; sleeping until the reset it was told | nothing. `backcheck wake` only if you know the limit lifted |
| `NEEDS_OPERATOR` | the models are not converging, a wave spent its session budget, or a retry gave up | re-scope the wave, amend a rule, `resume --reset-rework`, or `resume --accept` |
| `HALTED` | something git cannot undo: a read-only path changed, a fingerprint moved, the reviewer wrote to the tree, credentials rejected everywhere | **fix the named cause first**, then resume |
| `CLOSED` | every wave signed | review the branch yourself |

```bash
backcheck resume --because "what you actually fixed"
```

`--because` is required. Resuming without knowing the cause is how a build
destroys something twice.

### 9. Take delivery

```bash
git -C ../myapp-backcheck log backcheck/build   # what it did, commit by commit
backcheck review                                # what the reviewer checked
cat .backcheck/ledger.jsonl                     # one row per signed wave
backcheck squash                                # one commit per wave on backcheck/export
```

Every commit carries a `BackCheck-Wave: N` trailer. `squash` writes a tidier export
branch and never rewrites `backcheck/build` — that branch is the evidence the
reviews refer to.

**Read the code before you merge it.** Two models agreeing is evidence, not proof.

---

## The gate is the whole contract

The reviewer mostly **re-runs** your gate and your acceptance criteria. If they
cannot fail, nothing can. Write acceptance criteria as commands, not prose:

```
✗  "the output should be correct"
✓  diff <(printf 'a b c\n' | go run . --json) <(printf '{"words":3}\n')   exits 0
```

The build happens in a fresh git worktree, so **anything not committed is not
there** — `node_modules/`, `vendor/`, `.venv/`, `.env`. Install it in `preflight`,
which runs before every session and costs no model time.

**Go**
```jsonc
"gate": [
  { "name": "build", "run": "go build ./..." },
  { "name": "vet",   "run": "go vet ./..." },
  { "name": "test",  "run": "go test -count=1 ./... 2>&1 | tail -40" }
]
```

**Node**
```jsonc
"preflight": [{ "name": "deps", "run": "test -d node_modules || npm ci" }],
"gate": [
  { "name": "types", "run": "npx tsc --noEmit" },
  { "name": "test",  "run": "npm test 2>&1 | tail -40" }
],
"readonly_paths": ["package-lock.json"]
```

**Python**
```jsonc
"preflight": [{ "name": "venv", "run": "test -d .venv || (python3 -m venv .venv && .venv/bin/pip install -q -r requirements.txt)" }],
"gate": [
  { "name": "lint", "run": ".venv/bin/ruff check ." },
  { "name": "test", "run": ".venv/bin/pytest -q 2>&1 | tail -40" }
]
```

**Laravel / PHP**
```jsonc
"preflight": [
  { "name": "env",    "run": "test -f .env || cp ../myapp/.env.backcheck .env" },
  { "name": "vendor", "run": "test -d vendor || composer install --no-interaction -q" }
],
"gate": [
  { "name": "pint", "run": "vendor/bin/pint --test" },
  { "name": "stan", "run": "vendor/bin/phpstan analyse --no-progress 2>&1 | tail -40" },
  { "name": "test", "run": "php artisan test 2>&1 | tail -60" }
],
"readonly_paths": ["database/migrations/", ".env", "composer.lock"]
```

> **Laravel: isolate the database.** `.env` is not in git, so the worktree has
> none — give it an `.env.backcheck` with `DB_CONNECTION=sqlite` and
> `DB_DATABASE=:memory:`, never production credentials. If the build must reach a
> real database, put its schema in `fingerprints`: a builder running
> `migrate:fresh` on your dev database is exactly the damage git cannot undo.

---

## Desktop alerts

`notify.command` fires for `halt`, `needs-operator`, `closed` and `model-mismatch`
— the four that mean the build has stopped needing a model and started needing you.
The event name and message are appended as the last two arguments, and exported as
`BACKCHECK_EVENT`, `BACKCHECK_MSG` and `BACKCHECK_WAVE`.

```jsonc
// Linux
"notify": { "command": ["notify-send", "-a", "backcheck", "-u", "critical"] }
```

```bash
# macOS — osascript takes a script, not argv, so it needs a wrapper
#!/usr/bin/env bash
osascript -e "display notification \"$BACKCHECK_MSG\" with title \"BackCheck: $BACKCHECK_EVENT\""
```
```jsonc
"notify": { "command": ["/usr/local/bin/backcheck-notify"] }
```

---

## Limits

| limit | default | what it stops |
|---|---|---|
| `max_sessions_per_wave` | 40 | a wave that keeps committing without ever finishing |
| `max_retries_before_operator` | 20 | "retry later" spinning forever on something that will never come back |

Both raise `NEEDS_OPERATOR` and alert you. Raise them to loosen; a `resume` clears
both.

---

## Command reference

```
backcheck init            set up .backcheck/ in this repo
backcheck auto "…"        plan, lint, approve, rehearse, run
backcheck plan FILE       planner drafts the rails
backcheck approve         you bless them; cuts the worktree
backcheck rehearse        probe everything, spend no session
backcheck keeper          run the build (restarts a dead driver)
backcheck run             the driver itself, in the foreground
backcheck status [--json] where it is and what it cost
backcheck watch           one line per change
backcheck review [WAVE]   print a review
backcheck note "…"        guidance for the next session
backcheck wake            stop waiting, try now
backcheck resume --because "…" [--accept] [--reset-rework]
backcheck squash          one commit per wave on an export branch
```

## Files

```
.backcheck/                 (in your main repo, never inside the build tree)
  config.json     providers, roles, gate, preflight, limits — re-read every iteration
  waves.md        what each wave must achieve
  rules.md        how the builder and reviewer must work
  control.json    the state machine (driver-owned; never edit by hand)
  ledger.jsonl    one row per signed wave
  events.jsonl    append-only log — the watcher's feed, and the cost record
  sessions/       every handoff, review, raw stream and delivered note
  keeper.log      the driver's own output
```

---

## How the loop works

| # | Step | On failure |
|---|------|-----------|
| 1 | Is the build over? (control file status) | CLOSED → keeper stands down |
| 2 | Preflight: worktree, branch, disk, your checks, read-only paths | retry later · read-only touched → HALTED |
| 3 | Rework streak on this wave < cap | NEEDS_OPERATOR with each round's defect headlines |
| 4 | Read control: which role, which session, which brief | |
| 5 | Resolve provider (fallback if limited), probe with one tiny call | WAITING to the named reset · no creds → HALTED |
| 6 | Fingerprint sweep — before | |
| 7 | Gate (cached per tree hash) | result goes into the brief |
| 8 | Dispatch ONE session (wall + idle watchdog, process-group kill) | |
| 9 | Classify: watchdog → result object → text patterns → fail-safe | retry; N in a row → HALTED |
| 10 | Fingerprint sweep — after | moved → HALTED |
| 11 | Did anything happen? HEAD moved / verdict parsed | N stalls → HALTED |

The builder ends its handoff with `STATUS: DONE|CONTINUE`, and every commit carries
a `BackCheck-Wave: N` trailer. The reviewer ends with
`VERDICT: VERIFIED|REWORK|ESCALATE` and numbered defects. A reviewer that changes
the tree halts the build. `VERIFIED` writes a ledger row pinning HEAD; the last
wave verified ⇒ `CLOSED`.

---

## Safety note

Providers are normally configured with `--permission-prompts none` or
`--dangerously-skip-permissions`, which is what makes a build unattended. Run
builds only in a worktree, VM or container you are happy for a model to have a
shell in.
