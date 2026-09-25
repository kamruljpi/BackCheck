#!/usr/bin/env bash
# install.sh — build backcheck and put it on your PATH.
#
#   ./install.sh                 build, test and install to the best bin dir
#   ./install.sh --prefix DIR    install to DIR instead
#   ./install.sh --no-test       skip the test suite (not recommended)
#   ./install.sh --check         report what is missing and install nothing
#   ./install.sh --binary FILE   install an already-built binary; needs no Go
#   ./install.sh --browser       also download the Chromium the browser rails drive
#
# Go is needed to BUILD backcheck, not to run it: the binary is statically linked
# and depends on nothing, so on a machine without Go you can install one built
# elsewhere (`make dist` produces them). git and a provider CLI are needed
# either way — those are what backcheck actually drives.
#
# Browser verification ("browser" in config.json) is opt-in per project, so its
# prerequisites are reported and never enforced. --browser downloads Chromium
# now rather than letting the first session pay for it on the session clock.
#
# The checks below are not ceremony: each one is something that, missing, makes
# a build fail in a way that is tedious to diagnose at 3am.

set -euo pipefail

PREFIX=""
RUN_TESTS=1
CHECK_ONLY=0
BINARY=""
PREP_BROWSER=0
MIN_GO_MAJOR=1
MIN_GO_MINOR=21
MIN_NODE_MAJOR=18   # what @playwright/mcp needs

while [ $# -gt 0 ]; do
  case "$1" in
    --prefix)   PREFIX="${2:-}"; shift 2 ;;
    --prefix=*) PREFIX="${1#*=}"; shift ;;
    --no-test)  RUN_TESTS=0; shift ;;
    --check)    CHECK_ONLY=1; shift ;;
    --binary)   BINARY="${2:-}"; shift 2 ;;
    --binary=*) BINARY="${1#*=}"; shift ;;
    --browser)  PREP_BROWSER=1; shift ;;
    -h|--help)  awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "$0"; exit 0 ;;
    *)          echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

cd "$(dirname "$0")"

bold()  { printf '\033[1m%s\033[0m\n' "$*"; }
ok()    { printf '  \033[32mok\033[0m    %s\n' "$*"; }
warn()  { printf '  \033[33mwarn\033[0m  %s\n' "$*"; }
fail()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; }

PROBLEMS=0
note_problem() { PROBLEMS=$((PROBLEMS + 1)); }

# ----------------------------------------------------------------- checks

bold "Checking this machine"

case "$(uname -s)" in
  Linux|Darwin) ok "$(uname -s) $(uname -r)" ;;
  *) fail "backcheck needs Linux or macOS (it uses flock and process groups); found $(uname -s)"
     note_problem ;;
esac

# Where the binary comes from. An explicit --binary wins. Otherwise Go wins,
# because a checkout of this repo carries a COMMITTED ./backcheck that is
# whatever was last built — installing that instead of the source you are
# standing in is the kind of surprise that costs an afternoon. A prebuilt
# binary is the answer only when there is no Go to build with.
PREBUILT=""
for cand in "./backcheck" "./dist/backcheck-$(uname -s | tr 'A-Z' 'a-z')-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"; do
  [ -x "$cand" ] && { PREBUILT="$cand"; break; }
done

if [ -n "$BINARY" ]; then
  if [ -x "$BINARY" ]; then
    ok "installing the binary you named: $BINARY (no Go needed)"
  else
    fail "$BINARY is not an executable file"; note_problem
  fi
elif command -v go >/dev/null 2>&1; then
  GOVER="$(go env GOVERSION 2>/dev/null || echo unknown)"
  GV="${GOVER#go}"
  MAJ="${GV%%.*}"; REST="${GV#*.}"; MIN="${REST%%.*}"
  if [ "${MAJ:-0}" -gt "$MIN_GO_MAJOR" ] 2>/dev/null ||
     { [ "${MAJ:-0}" -eq "$MIN_GO_MAJOR" ] && [ "${MIN:-0}" -ge "$MIN_GO_MINOR" ]; } 2>/dev/null; then
    ok "go $GV — building from this source tree"
  else
    fail "go $GV is older than $MIN_GO_MAJOR.$MIN_GO_MINOR"; note_problem
  fi
elif [ -n "$PREBUILT" ]; then
  BINARY="$PREBUILT"
  ok "no Go here, so installing the prebuilt $PREBUILT"
  warn "that binary is whatever was last committed — it may be older than this source"
else
  fail "no Go, and no prebuilt binary to install."
  echo "        Either install Go (https://go.dev/dl/), or build backcheck on a"
  echo "        machine that has it (\`make dist\`) and copy the binary here:"
  echo "            ./install.sh --binary ./backcheck-linux-amd64"
  note_problem
fi

if command -v git >/dev/null 2>&1; then
  ok "$(git --version)"
else
  fail "git is not installed; backcheck drives builds entirely through git"; note_problem
fi

# The provider CLI. backcheck can drive any command that speaks Claude Code
# stream-json, so this is a warning rather than a hard requirement.
if command -v claude >/dev/null 2>&1; then
  ok "claude $(claude --version 2>/dev/null | head -1)"
else
  warn "the 'claude' CLI is not on PATH — install it, or point providers[].command at your own"
fi

case "$(uname -s)" in
  Linux)
    if command -v notify-send >/dev/null 2>&1; then
      ok "notify-send (desktop alerts for HALT and NEEDS_OPERATOR)"
    else
      warn "no notify-send — a stopped build will not raise an alert (apt install libnotify-bin)"
    fi ;;
  Darwin)
    if command -v osascript >/dev/null 2>&1; then
      ok "osascript available — see README for the notify wrapper"
    fi ;;
esac

# Browser verification drives a real Chromium through an MCP server. It is
# opt-in per project, so nothing here is fatal — but a missing piece does not
# announce itself: it surfaces as a session that sits there until the idle
# watchdog kills it, which reads exactly like a hung model.
echo
bold "Browser verification (only if you set \"browser\": {\"enabled\": true})"
BROWSER_READY=1
if command -v node >/dev/null 2>&1; then
  NODE_MAJOR="$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0)"
  if [ "${NODE_MAJOR:-0}" -ge "$MIN_NODE_MAJOR" ] 2>/dev/null; then
    ok "node $(node -v)"
  else
    warn "node $(node -v) is older than v$MIN_NODE_MAJOR, which @playwright/mcp needs"
    BROWSER_READY=0
  fi
else
  warn "no node — needed only for browser verification"
  BROWSER_READY=0
fi
if command -v npx >/dev/null 2>&1; then
  ok "npx (runs @playwright/mcp)"
else
  warn "no npx — needed only for browser verification"
  BROWSER_READY=0
fi

case "$(uname -s)" in
  Darwin) PW_CACHE="$HOME/Library/Caches/ms-playwright" ;;
  *)      PW_CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/ms-playwright" ;;
esac

if [ "$BROWSER_READY" -eq 1 ]; then
  if ls -d "$PW_CACHE"/chromium-* >/dev/null 2>&1; then
    ok "Chromium is downloaded ($PW_CACHE)"
  elif [ "$PREP_BROWSER" -eq 1 ]; then
    echo "  downloading Chromium (a few hundred MB, once) …"
    if npx -y playwright@latest install chromium; then
      ok "Chromium downloaded"
    else
      warn "the Chromium download failed — run 'npx playwright install chromium' yourself"
    fi
    # Warm the MCP package into the npx cache too, so the first session does
    # not spend its clock fetching that either.
    npx -y @playwright/mcp@latest --help >/dev/null 2>&1 && ok "@playwright/mcp cached" || true
    if [ "$(uname -s)" = "Linux" ]; then
      echo "  if a headless page never loads, the system libraries are missing:"
      echo "        sudo npx playwright install-deps chromium"
    fi
  else
    warn "Chromium is not downloaded yet. The first browser session would fetch it"
    echo "        on the session clock, and the idle watchdog cannot tell a download"
    echo "        from a hung model. Get it out of the way now:"
    echo "            ./install.sh --browser"
  fi
elif [ "$PREP_BROWSER" -eq 1 ]; then
  fail "--browser needs node and npx"
  note_problem
fi

if [ "$PROBLEMS" -gt 0 ]; then
  echo
  fail "$PROBLEMS problem(s) above must be fixed first"
  exit 1
fi

# ----------------------------------------------------------------- where

if [ -z "$PREFIX" ]; then
  case ":$PATH:" in
    *":$HOME/.local/bin:"*) PREFIX="$HOME/.local/bin" ;;
    *":/usr/local/bin:"*)   PREFIX="/usr/local/bin" ;;
    *)                      PREFIX="$HOME/.local/bin" ;;
  esac
fi
TARGET="$PREFIX/backcheck"

if [ "$CHECK_ONLY" -eq 1 ]; then
  echo
  bold "Would install to $TARGET"
  exit 0
fi

# ----------------------------------------------------------------- build

if [ -z "$BINARY" ]; then
  echo
  bold "Building"
  go vet ./... && ok "go vet"
  if [ "$RUN_TESTS" -eq 1 ]; then
    echo "  running the test suite (about 40s) …"
    if go test ./... >/tmp/backcheck-test.$$ 2>&1; then
      ok "go test: all tests pass"
      rm -f /tmp/backcheck-test.$$
    else
      fail "tests failed — not installing a binary that does not pass its own suite"
      cat /tmp/backcheck-test.$$; rm -f /tmp/backcheck-test.$$
      exit 1
    fi
  fi
  CGO_ENABLED=0 go build -trimpath -o ./backcheck . && ok "built ./backcheck ($(du -h ./backcheck | cut -f1))"
  BINARY=./backcheck
fi

# ----------------------------------------------------------------- install

echo
bold "Installing"
mkdir -p "$PREFIX" 2>/dev/null || true
if [ -w "$PREFIX" ]; then
  install -m 0755 "$BINARY" "$TARGET"
else
  echo "  $PREFIX is not writable; using sudo"
  sudo install -m 0755 "$BINARY" "$TARGET"
fi
ok "installed $TARGET"

case ":$PATH:" in
  *":$PREFIX:"*) ok "$PREFIX is on your PATH" ;;
  *) warn "$PREFIX is NOT on your PATH — add this to your shell profile:"
     echo "        export PATH=\"$PREFIX:\$PATH\"" ;;
esac

echo
if "$TARGET" --help >/dev/null 2>&1; then
  ok "$("$TARGET" --help | head -1)"
fi

cat <<'NEXT'

Next, in the repo you want built:

  cd ~/code/myapp
  backcheck init                      # writes .backcheck/config.json
  $EDITOR .backcheck/config.json      # set providers — see README.md step 2
  backcheck auto "what to build"      # plan, lint, approve, rehearse, run

README.md has the full walkthrough, including per-language gate examples
(Go, Node, Python, Laravel) and what to do when a build stops.
NEXT
