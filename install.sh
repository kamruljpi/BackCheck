#!/usr/bin/env bash
# install.sh — build backcheck and put it on your PATH.
#
#   ./install.sh                 build, test and install to the best bin dir
#   ./install.sh --prefix DIR    install to DIR instead
#   ./install.sh --no-test       skip the test suite (not recommended)
#   ./install.sh --check         report what is missing and install nothing
#   ./install.sh --binary FILE   install an already-built binary; needs no Go
#
# Go is needed to BUILD backcheck, not to run it: the binary is statically linked
# and depends on nothing, so on a machine without Go you can install one built
# elsewhere (`make dist` produces them). git and a provider CLI are needed
# either way — those are what backcheck actually drives.
#
# The checks below are not ceremony: each one is something that, missing, makes
# a build fail in a way that is tedious to diagnose at 3am.

set -euo pipefail

PREFIX=""
RUN_TESTS=1
CHECK_ONLY=0
BINARY=""
MIN_GO_MAJOR=1
MIN_GO_MINOR=21

while [ $# -gt 0 ]; do
  case "$1" in
    --prefix)   PREFIX="${2:-}"; shift 2 ;;
    --prefix=*) PREFIX="${1#*=}"; shift ;;
    --no-test)  RUN_TESTS=0; shift ;;
    --check)    CHECK_ONLY=1; shift ;;
    --binary)   BINARY="${2:-}"; shift 2 ;;
    --binary=*) BINARY="${1#*=}"; shift ;;
    -h|--help)  sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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

# If no binary was handed to us, look for one built earlier — then Go is
# only needed as a last resort.
if [ -z "$BINARY" ]; then
  for cand in "./backcheck" "./dist/backcheck-$(uname -s | tr 'A-Z' 'a-z')-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"; do
    [ -x "$cand" ] && { BINARY="$cand"; break; }
  done
fi

if [ -n "$BINARY" ]; then
  if [ -x "$BINARY" ]; then
    ok "using the prebuilt binary $BINARY (no Go needed)"
  else
    fail "$BINARY is not an executable file"; note_problem
  fi
elif command -v go >/dev/null 2>&1; then
  GOVER="$(go env GOVERSION 2>/dev/null || echo unknown)"
  GV="${GOVER#go}"
  MAJ="${GV%%.*}"; REST="${GV#*.}"; MIN="${REST%%.*}"
  if [ "${MAJ:-0}" -gt "$MIN_GO_MAJOR" ] 2>/dev/null ||
     { [ "${MAJ:-0}" -eq "$MIN_GO_MAJOR" ] && [ "${MIN:-0}" -ge "$MIN_GO_MINOR" ]; } 2>/dev/null; then
    ok "go $GV"
  else
    fail "go $GV is older than $MIN_GO_MAJOR.$MIN_GO_MINOR"; note_problem
  fi
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
