# Deliver a safe one-shot debug snapshot

Status: `ready-for-human`

## What to build

Give users and Codex one strictly read-only `bandmaster debug` command that produces a useful human or stable JSON snapshot of core Bandmaster state without initializing, repairing, or exposing secret-bearing data.

## Acceptance criteria

- [x] `bandmaster debug` renders one human-readable snapshot and `bandmaster debug --json` returns the same normalized information through a versioned machine contract.
- [x] Every snapshot identifies the Bandmaster version, executable, Go runtime and target, embedded VCS revision and dirty status when available, project and state locations, and known database schema version.
- [x] The snapshot includes the selected session and core task, derived worker, lease, and claim relationships without introducing a persisted Agent entity.
- [x] Assignment tokens, environment values, stored content, and other authority-bearing fields are redacted by default while safe presence, fingerprint, hash, size, status, and timing metadata remain available.
- [x] The command is strictly read-only: it does not prepare mutations, sweep leases, start monitors, create Bandmaster directories or tables, or alter Git and durable state.
- [x] An uninitialized supported Git repository returns useful runtime and repository evidence with an explicit uninitialized state and leaves the repository unchanged.
- [x] CLI integration tests exercise the compiled binary and assert output, exit behavior, redaction, and unchanged Git and Bandmaster state.

## Blocked by

None — can start immediately.

## Comments
