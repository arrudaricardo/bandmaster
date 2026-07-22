# Explain complete orchestration state

Status: `ready-for-human`

## What to build

Expand the one-shot snapshot into a complete explanation of current or historical orchestration, including why work is blocked and which supported Bandmaster action can address it.

## Acceptance criteria

- [x] Snapshots include normalized batches, manifests, validation summaries, monitor health, integrity violations, configuration status, Git observations, and relevant audit events alongside the core entities.
- [x] The derived worker view groups authoritative task, lease, claim, last-activity, and diagnostic information by `worker_identity` without becoming authoritative state.
- [x] The default selects the open session or otherwise the latest terminal session and marks it historical; `--session <id>` selects an exact session.
- [x] The latest 50 relevant events are included by default, with supported history-limit and complete-history options.
- [x] Diagnostics use stable codes, severity, affected identities, structured evidence, and supported suggested CLI actions for lease timing, claim contention, dependencies, monitor health, integrity state, unowned or submitted drift, Git control-state drift, configuration drift, and invariant failures.
- [x] An explicit unsafe option reveals only authorized secret-bearing fields in the normalized model and never exposes raw SQLite rows, arbitrary blobs, or a database dump.
- [x] Integration tests arrange representative public workflows and assert complete normalized relationships and diagnostics through the compiled CLI rather than private storage details.

## Blocked by

- 01 — Deliver a safe one-shot debug snapshot.

## Comments
