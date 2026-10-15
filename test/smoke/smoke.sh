#!/usr/bin/env bash
# Smoke test against a real Docker engine. Needs Docker with Compose, curl and
# internet access (Drawbridge downloads its ASN database on start). Takes about
# two minutes and removes everything it created.
#
#   test/smoke/smoke.sh
#
# In GitLab CI with Docker-in-Docker, published ports live on the "docker"
# service host: SMOKE_HOST=docker SMOKE_BIND=0.0.0.0 test/smoke/smoke.sh
# SMOKE_JUNIT=/abs/path.xml also writes each step as a JUnit test case.
set -euo pipefail
cd "$(dirname "$0")"

export SMOKE_BIND=${SMOKE_BIND:-127.0.0.1}
URL=http://${SMOKE_HOST:-127.0.0.1}:18080
JAR=$(mktemp)
# JUnit report: one test case per step, timed from the previous step.
CASES="" COUNT=0 FAILURES=0 LAST=$(date +%s)
xml() { tr -d '\000-\010\013\014\016-\037' | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g'; }
record() { # <step> [failure details]
  local now name; now=$(date +%s); name=$(printf %s "$1" | xml)
  COUNT=$((COUNT + 1))
  CASES+="<testcase classname=\"smoke\" name=\"$name\" time=\"$((now - LAST))\">"
  if [ $# -gt 1 ]; then
    FAILURES=$((FAILURES + 1))
    CASES+="<failure message=\"$name\">$(printf %s "$2" | xml)</failure>"
  fi
  CASES+="</testcase>"
  LAST=$now
}
write_junit() {
  [ -n "${SMOKE_JUNIT:-}" ] || return 0
  printf '<?xml version="1.0" encoding="UTF-8"?>\n<testsuites><testsuite name="smoke" tests="%d" failures="%d">%s</testsuite></testsuites>\n' \
    "$COUNT" "$FAILURES" "$CASES" >"$SMOKE_JUNIT"
}

pass() { record "$*"; printf '\033[32mok\033[0m   %s\n' "$*"; }
fail() {
  local logs; logs=$(docker compose logs --tail 60 drawbridge 2>&1 || true)
  record "$*" "$logs"
  printf '\033[31mFAIL\033[0m %s\n%s\n' "$*" "$logs"
  exit 1
}

cleanup() {
  local status=$?
  if [ "$status" -ne 0 ] && [ "$FAILURES" -eq 0 ]; then
    record "smoke test" "aborted with exit status $status"
  fi
  write_junit
  docker compose down -v --remove-orphans >/dev/null 2>&1 || true
  docker compose -f victims.yaml down -v --remove-orphans >/dev/null 2>&1 || true
  rm -f "$JAR"
}
trap cleanup EXIT

running() { docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null; }

# wait_for <seconds> <description> <command...>
wait_for() {
  local secs=$1 what=$2; shift 2
  for _ in $(seq "$secs"); do
    if "$@" >/dev/null 2>&1; then pass "$what"; return; fi
    sleep 1
  done
  fail "$what (gave up after ${secs}s)"
}

metric() { curl -fsS -H 'Authorization: Bearer smoke' "$URL/metrics" | grep -qF "$1"; }
both_stopped() { [ "$(running dbsmoke-victims-db-1)" = false ] && [ "$(running dbsmoke-victims-app-1)" = false ]; }
both_running() { [ "$(running dbsmoke-victims-db-1)" = true ] && [ "$(running dbsmoke-victims-app-1)" = true ]; }
set_ip() { docker compose exec -T fakeip sh -c "echo $1 > /www/ip"; }
post() { curl -fsS -b "$JAR" -c "$JAR" -o /dev/null -H 'HX-Request: true' -H "X-CSRF-Token: $CSRF" "$@"; }

echo "Starting containers…"
docker compose -f victims.yaml up -d --quiet-pull
docker stop dbsmoke-victims-idle-1 >/dev/null # already stopped: must stay stopped
docker compose up -d --build --quiet-pull

wait_for 120 "Drawbridge is up" curl -fsS "$URL/healthz"
curl -fsS -c "$JAR" -o /dev/null -X POST "$URL/setup/admin" \
  --data-urlencode username=smoke --data-urlencode 'password=smoke test password' \
  --data-urlencode 'confirm=smoke test password' || fail "create admin"
# sed quits after the first token but its input is read whole, so nothing in the pipe gets SIGPIPE.
CSRF=$(curl -fsS -b "$JAR" "$URL/settings" | sed -n '/name="_csrf"/{s/.*name="_csrf" value="\([^"]*\)".*/\1/p;q;}')
[ -n "$CSRF" ] || fail "no CSRF token"
pass "admin created"

wait_for 60 "victims stack discovered" sh -c "curl -fsS -b '$JAR' '$URL/stacks' | grep -q dbsmoke-victims"
post -X POST "$URL/stacks/1/dbsmoke-victims/policy" -d disable=on || fail "set stack policy"
post -X POST "$URL/setup/finish" || fail "finish setup"
pass "dbsmoke-victims set to stop on failover; automation on"

wait_for 90 "on primary (AS15169)" metric 'drawbridge_wan_state{state="primary"} 1'

set_ip 1.1.1.1
wait_for 90 "failover stopped db and app" both_stopped
[ "$(running dbsmoke-drawbridge-1)" = true ] || fail "Drawbridge stopped itself"
[ "$(running dbsmoke-fakeip-1)" = true ] || fail "a container outside the stack was stopped"
metric 'drawbridge_containers_stopped 2' || fail "metrics: expected 2 stopped"
pass "nothing else was touched"

set_ip 8.8.8.8
wait_for 90 "failback started db and app" both_running
[ "$(running dbsmoke-victims-idle-1)" = false ] || fail "idle was started although it was stopped before failover"
pass "idle stayed stopped"

echo
printf '\033[32mSmoke test passed.\033[0m\n'
