#!/bin/sh
# Fails when go.mod or go.sum is not tidy. Leaves the working tree unchanged.
set -eu

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

cp go.mod "$tmp/go.mod"
[ -f go.sum ] && cp go.sum "$tmp/go.sum"

mise exec -- go mod tidy

status=0
cmp -s go.mod "$tmp/go.mod" || status=1
if [ -f go.sum ] || [ -f "$tmp/go.sum" ]; then
	cmp -s go.sum "$tmp/go.sum" 2>/dev/null || status=1
fi

cp "$tmp/go.mod" go.mod
if [ -f "$tmp/go.sum" ]; then cp "$tmp/go.sum" go.sum; else rm -f go.sum; fi

if [ "$status" -ne 0 ]; then
	echo "go.mod or go.sum is not tidy; run 'go mod tidy' and commit the result." >&2
	exit 1
fi
