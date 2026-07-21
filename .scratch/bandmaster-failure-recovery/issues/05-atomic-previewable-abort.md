# Make abort atomic, retryable, and previewable

Status: `ready-for-human`

## Parent

[Production-Safe Bandmaster Failure Recovery](../PRD.md)

## What to build

Provide one abort planning and execution workflow for active, submitted, frozen, quarantined, repair-pending, and reconciled finalizing work. After worker-termination and monitor-stop preconditions are satisfied, task quarantine, active-claim release, artifact disposition, session transition, and audit append must commit atomically.

Add a dry-run mode that returns the same plan without changing Git or durable state. A failed execution must remain safely retryable instead of stranding the session in aborting.

## Acceptance criteria

- [ ] Abort behavior is explicit and tested for active, submitted, frozen, quarantined, repair-pending, and reconciled finalizing batches.
- [ ] Abort preserves working-tree edits and immutable ownership, submission, freeze, handoff, audit, and failure evidence.
- [ ] Durable cleanup and the transition to aborted occur in one database transaction after external preconditions succeed.
- [ ] An injected cleanup failure leaves a state from which repeating abort can complete safely.
- [ ] `session abort --dry-run --json` lists affected tasks, active claims, preserved artifacts, batches, journals, files, and blockers.
- [ ] Dry-run does not stop the monitor, mutate Git, change database state, or append audit events.
- [ ] Finalizing Git state must be reconciled before destructive cleanup is allowed.
- [ ] Existing termination-confirmation and fail-closed guarantees remain intact.

## Blocked by

- [02 — Roll back to a clean index without losing edits](02-clean-index-rollback.md)
- [03 — Restore compatible session and batch states](03-compatible-recovery-states.md)
- [04 — Preserve ownership evidence while releasing claims](04-preserve-ownership-evidence.md)

## Comments

- Added one shared abort plan for preview and execution. `session abort --dry-run --json` reports affected tasks, released active claims, preserved artifact counts, batches, finalization journals, files, and stable blockers without passing through a mutating integrity gate.
- Abort now checks worker confirmation and finalization reconciliation before stopping the monitor, then atomically quarantines tasks, appends task/session audits, releases active claims, records confirmation, and transitions directly to `aborted` in one transaction. It no longer strands new attempts in `aborting`.
- Added a narrow cleanup fault-injection seam proving that task/audit/claim/session changes roll back together and that retry succeeds after the monitor was already stopped.
- Public CLI integration coverage now includes active, submitted, frozen/finalizing, quarantined, repair-pending after finalization recovery, claimless, dry-run, confirmation-gated, and retry scenarios. Focused checks passed: `go test ./internal/project -count=1` and `go test ./integration -run '^TestSessionAbort' -count=1`.
