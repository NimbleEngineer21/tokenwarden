#!/bin/sh
# Rejects commit messages whose subject is not a Conventional Commit.
set -eu

subject=$(head -n 1 "$1")

case "$subject" in
Merge* | Revert* | fixup!* | squash!*) exit 0 ;;
esac

pattern='^(build|chore|ci|docs|feat|fix|perf|refactor|revert|style|test)(\([a-z0-9._/-]+\))?!?: [^ ].{0,99}$'

if ! printf '%s' "$subject" | grep -Eq "$pattern"; then
	echo "commit-msg: subject is not a Conventional Commit:" >&2
	echo "  $subject" >&2
	echo "expected: <type>(<optional scope>): <description>, max 100 chars after the type" >&2
	exit 1
fi
