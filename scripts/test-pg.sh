#!/bin/sh
# Run the full test suite against a throwaway Postgres in Docker. The
# server listens on a random loopback port, trusts every local connection,
# and is removed on exit. Extra arguments go to `go test`. Run it from the
# repo root.
set -eu

image=postgres:18.7@sha256:2ea8630df176ad090ec92136dee850397c5af8781e4bcf2e4162593691c61bbc

id=$(docker run -d --rm -p 127.0.0.1::5432 -e POSTGRES_HOST_AUTH_METHOD=trust "$image")
trap 'docker stop "$id" >/dev/null' EXIT INT TERM

i=0
until docker exec "$id" pg_isready -q -U postgres -h 127.0.0.1; do
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		echo "test-pg: Postgres did not start" >&2
		exit 1
	fi
	sleep 0.5
done

port=$(docker port "$id" 5432/tcp | head -n 1 | sed 's/.*://')
TW_TEST_DATABASE_URL="postgres://postgres@127.0.0.1:$port/postgres?sslmode=disable"
export TW_TEST_DATABASE_URL
mise exec -- go test -race -count=1 "$@" ./...
