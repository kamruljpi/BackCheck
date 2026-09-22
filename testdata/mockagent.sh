#!/usr/bin/env bash
# A stand-in for `claude -p --output-format stream-json`: reads the prompt on
# stdin, takes its next scripted action from $MOCK_DIR/<role>.queue, and speaks
# stream-json. Lets the whole loop run end-to-end with no model and no key.
set -u
prompt="$(cat)"
n=$(( $(cat "$MOCK_DIR/count" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$MOCK_DIR/count"
mkdir -p "$MOCK_DIR/prompts"

emit_init()   { printf '{"type":"system","subtype":"init","model":"%s"}\n' "${MOCK_MODEL:-mock-model-1}"; }
emit_result() { # $1 text  $2 is_error
  python3 -c 'import json,sys; print(json.dumps({"type":"result","subtype":"success","is_error":sys.argv[2]=="true","result":sys.argv[1],"total_cost_usd":0.01}))' "$1" "${2:-false}"
}
emit_limit_event() { # $1 status  $2 resetsAt(unix)  $3 rateLimitType
  printf '{"type":"rate_limit_event","rate_limit_info":{"status":"%s","resetsAt":%s,"rateLimitType":"%s","isUsingOverage":false},"session_id":"mock"}\n' "$1" "$2" "$3"
}
emit_api_retry() { # $1 attempt  $2 status  $3 error
  printf '{"type":"system","subtype":"api_retry","attempt":%s,"max_retries":10,"retry_delay_ms":500,"error_status":%s,"error":"%s"}\n' "$1" "$2" "$3"
}
pop() { # pop first line of a queue file
  local q="$MOCK_DIR/$1.queue"; local a; a=$(head -n1 "$q" 2>/dev/null); sed -i '1d' "$q" 2>/dev/null; echo "${a:-}"
}
commit() {
  local w; w=$(grep -m1 -oE '^- Wave [0-9]+' <<<"$prompt" | grep -oE '[0-9]+')
  echo "step $n" >> "work-w$w.txt"
  git add -A >/dev/null && git -c user.email=m@x -c user.name=mock commit -q -m "mock step $n" -m "BackCheck-Wave: $w"
}

if grep -q "Health check" <<<"$prompt"; then
  pf="$MOCK_DIR/probe-fail-${MOCK_NAME:-x}"
  if [ -f "$pf" ]; then cat "$pf" >&2; exit 1; fi
  emit_init; emit_result "OK"; exit 0
fi

if grep -q "^ROLE: BUILDER" <<<"$prompt"; then role=builder
elif grep -q "^ROLE: REVIEWER" <<<"$prompt"; then role=reviewer
else role=planner; fi
printf '%s' "$prompt" > "$MOCK_DIR/prompts/$n-$role.txt"
act=$(pop "$role")
echo "$n $role $act ${MOCK_NAME:-x}" >> "$MOCK_DIR/log"
emit_init

case "$role:$act" in
  builder:commit-done)      commit; emit_result "### Steps
did it, commit $(git rev-parse --short HEAD), proof: ok
STATUS: DONE" ;;
  builder:commit-continue)  commit; emit_result "partial work
STATUS: CONTINUE" ;;
  builder:nocommit-done)    emit_result "all good trust me
STATUS: DONE" ;;
  builder:nocommit-continue) emit_result "thinking
STATUS: CONTINUE" ;;
  builder:touch-fp)         echo x >> "$MOCK_DIR/external.txt"; commit; emit_result "STATUS: DONE" ;;
  builder:write-readonly)   mkdir -p protected; echo hacked > protected/data.sql; commit; emit_result "STATUS: DONE" ;;
  builder:ratelimit|reviewer:ratelimit)
      emit_result "Claude AI usage limit reached|$(( $(date +%s) + 3 ))" true; exit 1 ;;
  # the limit as the CLI really reports it: a structured event with an exact
  # reset, and prose that names the window rather than the mechanism.
  builder:limit-event|reviewer:limit-event)
      emit_limit_event rejected "$(( $(date +%s) + 3 ))" five_hour
      emit_result "You've hit your session limit \u00b7 resets 3pm (UTC)" true; exit 1 ;;
  builder:limit-prose|reviewer:limit-prose)
      emit_result "You've hit your session limit \u00b7 contact your admin to increase it" true; exit 1 ;;
  # a weekly limit: real prose, and a reset days away rather than minutes
  builder:limit-weekly|reviewer:limit-weekly)
      emit_result "You've hit your weekly limit \u00b7 resets $(date -u -d '+3 days' '+%b %-d, 9am') (UTC)" true; exit 1 ;;
  builder:hang)             sleep 30 ;;
  builder:auth-retry-hang|reviewer:auth-retry-hang)
      for a in 1 2 3; do emit_api_retry "$a" 401 authentication_failed; done; sleep 30 ;;
  builder:rate-retry-hang|reviewer:rate-retry-hang)
      for a in 1 2 3; do emit_api_retry "$a" 429 rate_limit_error; done; sleep 30 ;;
  builder:crash)            echo "segfault-ish" >&2; exit 3 ;;
  reviewer:verified)        emit_result "### Re-run
all green
### Defects
none
VERDICT: VERIFIED" ;;
  reviewer:rework)          emit_result "### Defects
D1: tests assert nothing — foo_test.go — TestFoo has no assertions
D2: missing error path — foo.go:12 — returns nil on failure
VERDICT: REWORK" ;;
  reviewer:escalate)        emit_result "The rule contradicts the wave.
VERDICT: ESCALATE" ;;
  reviewer:noverdict)       emit_result "I looked around." ;;
  reviewer:commit)          echo sneaky >> sneaky.txt; git add -A; git -c user.email=m@x -c user.name=mock commit -qm sneaky; emit_result "VERDICT: VERIFIED" ;;
  planner:*)                emit_result "$(cat "$MOCK_DIR/planner.out")" ;;
  *)                        emit_result "no scripted action for $role (got '$act')"; exit 2 ;;
esac
