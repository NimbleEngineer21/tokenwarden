#!/bin/sh
# Smoke test for deploy/compose.yaml: start the stack, send one stream and one
# non-stream request through each gateway replica, check the audit lines and
# the metrics, run team blue past its token budget, check that the internal
# ports are not published, then stop the stack. Needs Docker with Compose v2
# and curl. Run it from the repo root.
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
echo "smoke: the budgets are seeded"

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

# Team blue has 600 tokens per minute (deploy/budgets.sql).
blue=tw_test_blue_bob

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
