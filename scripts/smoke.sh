#!/bin/sh
# Smoke test for deploy/compose.yaml: start the stack, check that an unknown
# key gets 401, send one stream and one non-stream request through each
# gateway replica, check the audit lines and the metrics, list the budgets
# with wardenctl, run team blue past its token budget, create and revoke a
# key with wardenctl, check the key's usage ledger row, check that the
# internal ports are not published, then stop the stack. Needs Docker with Compose v2 and curl. Run it from the
# repo root.
set -eu

compose() { docker compose -f deploy/compose.yaml "$@"; }
probe() { compose --profile tools run --rm -q probe -sS -m 5 "$@"; }
fail() {
	echo "smoke: FAIL: $*" >&2
	exit 1
}

cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		compose logs --no-color >&2 || true
	fi
	compose --profile tools down -v --remove-orphans >/dev/null 2>&1 || true
	exit "$status"
}
trap cleanup EXIT INT TERM

# Each replica has a host port and a service name.
replicas='8080:gateway-a 8082:gateway-b'
key=tw_test_red_alice

compose up -d --build --quiet-pull

for r in $replicas; do
	port=${r%%:*}
	n=0
	until curl -fsS -m 2 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; do
		n=$((n + 1))
		[ "$n" -lt 60 ] || fail "127.0.0.1:$port/healthz did not answer in 60 s"
		sleep 1
	done
done
echo "smoke: both replicas are healthy"
# Older Compose releases (the CI runner) skip exited containers in
# "compose wait", and seed may finish before this line. "docker wait" works
# on a stopped container and prints its exit code.
seed_id=$(compose ps -aq seed)
[ -n "$seed_id" ] || fail "the seed service has no container"
[ "$(docker wait "$seed_id")" = 0 ] || fail "the seed service failed"
echo "smoke: the keys and budgets are seeded"

budgets=$(compose --profile tools run --rm -q --build wardenctl budgets list) ||
	fail "wardenctl budgets list failed"
printf '%s\n' "$budgets" | grep -Eq '^blue +600 +1m +2$' ||
	fail "wardenctl does not list the blue budget:
$budgets"
echo "smoke: wardenctl lists the budgets"

request() { # port key stream max_tokens [curl options]
	# sh has no local variables; the names must not clash with the callers'.
	r_port=$1 r_key=$2 r_stream=$3 r_max=$4
	shift 4
	curl -sSN -m 60 "$@" "http://127.0.0.1:$r_port/v1/messages" \
		-H "x-api-key: $r_key" \
		-H 'anthropic-version: 2023-06-01' \
		-H 'content-type: application/json' \
		-d "{\"model\":\"claude-sonnet-4-5\",\"max_tokens\":$r_max,\"stream\":$r_stream,\"messages\":[{\"role\":\"user\",\"content\":\"smoke\"}]}"
}
send() { request "$1" "$key" "$2" 32 -f; } # port stream

# The gateways read their keys from Postgres (-db-keys): a seeded key works
# on both replicas (the steps below), and a key that is not there gets 401.
for r in $replicas; do
	port=${r%%:*}
	code=$(request "$port" tw_test_not_seeded false 32 -o /dev/null -w '%{http_code}') ||
		fail "127.0.0.1:$port: the unknown-key request failed"
	[ "$code" = 401 ] || fail "127.0.0.1:$port: an unknown key got $code, want 401"
done
echo "smoke: an unknown key gets 401 on both replicas"

# wait_audit fails unless the logs of service $1 get an audit line that
# matches each of the other arguments.
wait_audit() {
	w_svc=$1
	shift
	w_n=0
	while :; do
		w_lines=$(compose logs --no-color --no-log-prefix "$w_svc" | grep '"msg":"audit"') || true
		for w_m in "$@"; do
			w_lines=$(echo "$w_lines" | grep -F -- "$w_m") || true
		done
		[ -z "$w_lines" ] || return 0
		w_n=$((w_n + 1))
		[ "$w_n" -lt 10 ] || fail "$w_svc: no audit line with $*"
		sleep 1
	done
}

for r in $replicas; do
	port=${r%%:*}
	svc=${r#*:}

	# Read the whole reply before grep: grep -q exits at the first match and
	# would cut the request.
	reply=$(send "$port" true) || fail "$svc: the stream request failed"
	echo "$reply" | grep -q '^event: message_stop' ||
		fail "$svc: the stream has no message_stop"
	reply=$(send "$port" false) || fail "$svc: the non-stream request failed"
	echo "$reply" | grep -q '"type":"message"' ||
		fail "$svc: the non-stream reply is not a message"

	# The gateway writes the audit line after the handler returns, which can
	# be after the client has the whole reply.
	for s in true false; do
		wait_audit "$svc" '"team":"red"' "\"stream\":$s," '"outcome":"ok"'
	done
	logs=$(compose logs --no-color --no-log-prefix "$svc")
	# The key must never reach the logs.
	if echo "$logs" | grep -q "$key"; then
		fail "$svc: the gateway key is in the logs"
	fi

	metrics=$(probe "http://$svc:9090/metrics")
	for s in true false; do
		echo "$metrics" | grep -q "^tokenwarden_requests_total{outcome=\"ok\",stream=\"$s\"} 1\$" ||
			fail "$svc: tokenwarden_requests_total for stream=$s is not 1"
	done
	echo "smoke: $svc passed"
done

# Team blue has 600 tokens per minute and at most 2 requests in flight
# (deploy/budgets.sql).
blue=tw_test_blue_bob

# Two requests that the mock holds for 20 s take both slots; a third gets
# 429 with no Retry-After. Cutting the held ones before the upstream
# answers costs no tokens.
probe -f -X POST http://mock:8081/_faults -H 'x-api-key: sk-ant-test-mock' \
	-H 'content-type: application/json' -d '{"delay":"20s","count":2}' >/dev/null ||
	fail "could not set the delay fault"
request 8080 "$blue" false 32 -o /dev/null 2>/dev/null &
held1=$!
request 8082 "$blue" false 32 -o /dev/null 2>/dev/null &
held2=$!
n=0
while :; do
	sleep 1
	headers=$(request 8080 "$blue" false 32 -o /dev/null -D -) ||
		fail "the third in-flight request failed"
	echo "$headers" | grep -q '^HTTP/[0-9.]* 429' && break
	n=$((n + 1))
	[ "$n" -lt 10 ] || fail "a third request in flight did not get 429:
$headers"
done
# Each held request is a subshell; killing only the subshell leaves its
# curl running, and that curl then gets the reply and spends tokens.
pkill -P "$held1" 2>/dev/null || true
pkill -P "$held2" 2>/dev/null || true
kill "$held1" "$held2" 2>/dev/null || true
wait "$held1" "$held2" 2>/dev/null || true
probe -f -X DELETE http://mock:8081/_faults -H 'x-api-key: sk-ant-test-mock' >/dev/null ||
	fail "could not clear the faults"
if echo "$headers" | grep -qi '^retry-after:'; then
	fail "the in-flight 429 has a Retry-After:
$headers"
fi
# The metric, not the audit line, tells this 429 apart from the
# over-budget ones below.
probe "http://gateway-a:9090/metrics" |
	grep -q '^tokenwarden_budget_denials_total{stage="concurrency",team="blue"} [1-9]' ||
	fail "gateway-a counted no in-flight denial for blue"
echo "smoke: a third request in flight gets 429"

# The first hold, input plus max_tokens, does not fit: 429 with Retry-After.
headers=$(request 8080 "$blue" false 1000 -o /dev/null -D -) ||
	fail "the over-budget request failed"
echo "$headers" | grep -q '^HTTP/[0-9.]* 429' ||
	fail "the over-budget request did not get 429:
$headers"
echo "$headers" | grep -Eqi '^retry-after: [0-9]+' ||
	fail "the 429 reply has no Retry-After:
$headers"
wait_audit gateway-a '"team":"blue"' '"stream":false,' '"outcome":"budget_exceeded"'
echo "smoke: an over-budget request gets 429 with Retry-After"

# A long stream fits its first hold of 512 output tokens, but not the
# second, so the gateway cuts it with an error event.
probe -f -X POST http://mock:8081/_faults -H 'x-api-key: sk-ant-test-mock' \
	-H 'content-type: application/json' -d '{"long_reply":true,"count":1}' >/dev/null ||
	fail "could not set the long_reply fault"
reply=$(request 8080 "$blue" true 2000 -f) || fail "the long stream request failed"
echo "$reply" | grep -q '^event: error' && echo "$reply" | grep -q '"rate_limit_error"' ||
	fail "the long stream has no rate_limit_error event"
if echo "$reply" | grep -q '^event: message_stop'; then
	fail "the long stream was not cut"
fi
wait_audit gateway-a '"team":"blue"' '"stream":true,' '"outcome":"budget_exceeded"'
echo "smoke: a stream past the budget is cut with a rate_limit_error event"

# A key that wardenctl creates works at once on both replicas, and gets 401
# on both after wardenctl revokes it. -T: a TTY would add \r to the key.
ctl() { compose --profile tools run --rm -q -T wardenctl "$@"; }
new_key=$(ctl keys create -team red -user smoke 2>/dev/null) ||
	fail "wardenctl keys create failed"
for r in $replicas; do
	port=${r%%:*}
	request "$port" "$new_key" false 32 -f -o /dev/null ||
		fail "127.0.0.1:$port: the new key did not work"
done
new_id=$(ctl keys list -team red | awk '$3 == "smoke" { print $1 }')
[ -n "$new_id" ] || fail "wardenctl keys list does not list the new key"
ctl keys revoke -id "$new_id" >/dev/null || fail "wardenctl keys revoke failed"
for r in $replicas; do
	port=${r%%:*}
	code=$(request "$port" "$new_key" false 32 -o /dev/null -w '%{http_code}') ||
		fail "127.0.0.1:$port: the revoked-key request failed"
	[ "$code" = 401 ] || fail "127.0.0.1:$port: a revoked key got $code, want 401"
done
if compose logs --no-color | grep -qF -- "$new_key"; then
	fail "the new key is in the logs"
fi
echo "smoke: wardenctl creates a key that works and revokes it"

# The two requests of the new key, one on each replica, are in the usage
# ledger under its key ID and model.
ledger=$(ctl usage -team red -by key,model) || fail "wardenctl usage -by failed"
echo "$ledger" | awk -v id="$new_id" \
	'$2 == id && $3 == "claude-sonnet-4-5" && $4 == 2 && $NF > 0 { found = 1 } END { exit !found }' ||
	fail "the usage ledger does not hold the new key's requests:
$ledger"
echo "smoke: the usage ledger counts the requests per key and model"

# Only the proxy ports may be published, and only on 127.0.0.1. Ask Compose,
# not the host: another local process can hold 9090 or 8081. `compose port`
# is not usable here: for a port that the image exposes but nothing
# publishes, it prints ":0" and exits 0.
published=$(compose ps --format \
	'{{.Service}}{{range .Publishers}}{{if .PublishedPort}} {{.URL}}:{{.PublishedPort}}->{{.TargetPort}}{{end}}{{end}}' |
	sort)
want='gateway-a 127.0.0.1:8080->8080
gateway-b 127.0.0.1:8082->8080
mock
postgres'
[ "$published" = "$want" ] ||
	fail "unexpected published ports:
$published"
echo "smoke: internal ports are not published"
echo "smoke: PASS"
