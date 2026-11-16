#!/bin/sh
# Smoke test for deploy/compose.yaml: start the stack, send one stream and one
# non-stream request through each gateway replica, check the audit lines and
# the metrics, check that the internal ports are not published, then stop the
# stack. Needs Docker with Compose v2 and curl. Run it from the repo root.
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

send() { # port stream
	curl -fsSN -m 30 "http://127.0.0.1:$1/v1/messages" \
		-H "x-api-key: $key" \
		-H 'anthropic-version: 2023-06-01' \
		-H 'content-type: application/json' \
		-d "{\"model\":\"claude-sonnet-4-5\",\"max_tokens\":32,\"stream\":$2,\"messages\":[{\"role\":\"user\",\"content\":\"smoke\"}]}"
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
		n=0
		until compose logs --no-color --no-log-prefix "$svc" |
			grep '"msg":"audit"' | grep '"team":"red"' |
			grep "\"stream\":$s," | grep -q '"outcome":"ok"'; do
			n=$((n + 1))
			[ "$n" -lt 10 ] || fail "$svc: no ok audit line for stream=$s"
			sleep 1
		done
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

# Only the proxy ports may be published, and only on 127.0.0.1. Ask Compose,
# not the host: another local process can hold 9090 or 8081. `compose port`
# is not usable here: for a port that the image exposes but nothing
# publishes, it prints ":0" and exits 0.
published=$(compose ps --format \
	'{{.Service}}{{range .Publishers}}{{if .PublishedPort}} {{.URL}}:{{.PublishedPort}}->{{.TargetPort}}{{end}}{{end}}' |
	sort)
want='gateway-a 127.0.0.1:8080->8080
gateway-b 127.0.0.1:8082->8080
mock'
[ "$published" = "$want" ] ||
	fail "unexpected published ports:
$published"
echo "smoke: internal ports are not published"
echo "smoke: PASS"
