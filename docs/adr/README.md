# Architecture decision records

Each ADR records one decision that is hard to reverse: the context, the
decision, and its consequences.

## Format

File name: `NNNN-short-title.md`, numbered in order.

```markdown
# NNNN. Title

Status: proposed | accepted | superseded by NNNN
Date: YYYY-MM-DD

## Context
## Decision
## Consequences
## Alternatives
```

## Index

| ADR | Title | Status |
|---|---|---|
| [0001](0001-reservation-design.md) | Reserve, top up, and settle token budgets | accepted |
| [0002](0002-fail-closed.md) | Fail closed when the budget is unknown | accepted |
| [0003](0003-hot-row-and-leases.md) | One counter row for each team window, with leased reservations | accepted |
