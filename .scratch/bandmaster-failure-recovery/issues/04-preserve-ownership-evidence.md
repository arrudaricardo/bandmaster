# Preserve ownership evidence while releasing claims

Status: `ready-for-human`

## Parent

[Production-Safe Bandmaster Failure Recovery](../PRD.md)

## What to build

Separate active path locking from immutable ownership history so claims can be released without deleting or invalidating submitted snapshots, frozen batch evidence, structured handoffs, and attribution. Existing repositories must migrate safely, and task inspection after release must still explain who owned each preserved change.

The resulting lifecycle must allow a submitted task to be aborted without foreign-key failures while retaining the evidence required for audit and later diagnosis.

## Acceptance criteria

- [ ] Active claims can be released independently of immutable ownership and submission evidence.
- [ ] Submitted snapshots and frozen batch evidence no longer require an active claim row to remain valid.
- [ ] Existing state databases migrate without losing claims, baselines, submissions, or attribution.
- [ ] A claim, submission, and abort flow completes without foreign-key violations.
- [ ] Inspecting an aborted task still exposes immutable ownership and submission evidence while reporting no active claims.
- [ ] Ordinary claim acquisition, expansion, release, repair, finalization, and integrity scanning continue to enforce exclusive active ownership.
- [ ] Schema and CLI integration tests verify preservation and active-lock release.

## Blocked by

- None — can start immediately.

## Comments

- Implemented immutable `task_path_ownership` records while retaining `claims` as the exclusive active-lock table. Submitted snapshots and diff reviews now reference ownership evidence, so abort and finalization can release claims without losing baselines, submitted bytes, handoffs, or attribution.
- Added a safe legacy-schema migration that copies existing claims before rebuilding claim-backed snapshot tables; public task inspection now reports `ownership_evidence` separately from active `claims`.
- Added CLI/schema integration coverage for submitted-task abort, ordinary claim release, successful finalization, frozen-manifest retention, and migration of an existing claim-backed database.
- Focused checks passed: `go test ./internal/project -count=1`; core ownership migration/release/abort/finalization integration cases passed. A separate integrity-recovery gate regression owned by ticket 03 was reported to that worker.
