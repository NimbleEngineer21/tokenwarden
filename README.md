# tokenwarden

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

tokenwarden is a Go library and reference gateway that governs LLM traffic to
the Anthropic Messages API. It enforces token budgets and concurrency caps per
team across many gateway replicas, applies default-deny policy to models and
tools, and records an audit event for every request.

> **Status:** pre-release. The API is not stable until v1.0.0.

## Why

A gateway that checks a budget only before it forwards a request cannot know
what a streaming response will cost. When several requests from one team are in
flight on several replicas, they can all pass the check and overspend together.

tokenwarden reserves tokens before a request, tops up the reservation while the
response streams, and settles to the real usage at the end. Overspend has a
fixed upper limit, and the gateway stops a stream when the budget runs out.

## Features

Planned for the first releases:

- **Distributed token budgets** per team and window (day, week, month), backed
  by Postgres, with a tested overspend limit.
- **Mid-stream enforcement:** the stream ends with an Anthropic-format error
  event when a top-up fails.
- **Crash safety:** reservations are leases. Any replica releases the leases of
  a replica that dies.
- **Concurrency caps** on in-flight streams per team.
- **Default-deny policy** on models and tools, written in
  [CEL](https://cel.dev), with policy unit tests.
- **Break-glass grants:** time-limited exceptions with a reason, recorded in
  the audit log.
- **Library first:** use the `http.Handler` middleware and its interfaces in
  your own gateway, or run the reference binary.

## Run the gateway

```sh
go run ./cmd/mockanthropic &   # mock upstream on 127.0.0.1:8081
TW_UPSTREAM_URL=http://127.0.0.1:8081 TW_UPSTREAM_KEY=sk-ant-test-mock \
  go run ./cmd/tokenwarden -keys keys.json
```

The upstream URL and key come from the environment only. The key file holds
SHA-256 hashes of gateway keys. See [docs/tokenwarden.md](docs/tokenwarden.md)
for the key-file format, how to hash a key, the flags, and the metric names.

## Development

```sh
make setup   # install pinned tools and git hooks
make check   # run every check that CI runs
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## How this was built

This project is developed with AI coding assistance (Claude Code). Design
decisions, review, and every merged change are the maintainer's.

## License

[Apache License 2.0](LICENSE)
