# Changelog

All notable changes to this project are documented in this file.
The format follows Conventional Commits; versions follow Semantic Versioning.

## [0.1.0] - 2026-11-16

### Features

- Add Anthropic SSE reader, writer, and stream event types ([#3](https://github.com/NimbleEngineer21/tokenwarden/pull/3))
- Add mock Anthropic upstream with paced streams and fault API ([#4](https://github.com/NimbleEngineer21/tokenwarden/pull/4))
- Add gateway passthrough proxy with gateway-key auth ([#5](https://github.com/NimbleEngineer21/tokenwarden/pull/5))
- Add audit events and Prometheus metrics to the gateway ([#6](https://github.com/NimbleEngineer21/tokenwarden/pull/6))
- Add tokenwarden gateway binary with metrics listener and health check ([#7](https://github.com/NimbleEngineer21/tokenwarden/pull/7))

### Bug fixes

- Rename audit event time field to event_time ([#11](https://github.com/NimbleEngineer21/tokenwarden/pull/11))
- Wait for every Serve call before serve returns ([#12](https://github.com/NimbleEngineer21/tokenwarden/pull/12))

### Build

- Bump Go toolchain to 1.27.4 and CI matrix to 1.26.11/1.27.4 ([#9](https://github.com/NimbleEngineer21/tokenwarden/pull/9))
- Add container images, Compose stack, and smoke workflow ([#10](https://github.com/NimbleEngineer21/tokenwarden/pull/10))

### CI

- Add lint, test, vuln, and pr-title workflows ([#2](https://github.com/NimbleEngineer21/tokenwarden/pull/2))
- Bump jdx/mise-action from 5.2.6 to 5.2.9 ([#13](https://github.com/NimbleEngineer21/tokenwarden/pull/13))

### Chores

- Add repository scaffold, tooling, and contributor docs ([#1](https://github.com/NimbleEngineer21/tokenwarden/pull/1))
