# Support safe batch abandonment

Status: `ready-for-human`

## Parent

[Production-Safe Bandmaster Failure Recovery](../PRD.md)

## What to build

Add a supported batch abandonment command for recognized nonterminal batches that cannot or should not continue. Abandonment must preserve all Git-visible edits and immutable evidence, release only safe active ownership, record the operator's reason and confirmation, and move the session and batch to a documented compatible state.

If provisional finalization Git state exists, abandonment must use the supported recovery machinery before releasing ownership. Unknown Git states must continue to quarantine and require inspection.

## Acceptance criteria

- [ ] A documented CLI command abandons each supported nonterminal batch state with stable JSON output.
- [ ] Abandonment preserves worker edits, handoffs, snapshots, frozen manifests, ownership history, journals needed as evidence, and audit history.
- [ ] Safe active claims are released without foreign-key failures or lost attribution.
- [ ] Provisional commits or staged residue are reconciled through finalization recovery before abandonment completes.
- [ ] Unknown or contradictory Git state refuses abandonment and remains quarantined.
- [ ] The resulting session/batch pair is compatible and admits abort, continued planning, or another documented next action.
- [ ] Repeated abandonment is idempotent and does not duplicate audit transitions.

## Blocked by

- [02 — Roll back to a clean index without losing edits](02-clean-index-rollback.md)
- [03 — Restore compatible session and batch states](03-compatible-recovery-states.md)
- [04 — Preserve ownership evidence while releasing claims](04-preserve-ownership-evidence.md)
- [06 — Expose explicit finalization recovery](06-explicit-finalization-recovery.md)

## Comments

- Implemented `bandmaster batch abandon --reason <text> --confirmation <text>`
  with stable JSON and byte-stable idempotent replay. Recognized collecting,
  frozen, validating, repair-pending, repairing, finalizing, and final-validating
  batches transition to the compatible `paused`/`abandoned` pair. The atomic
  database transition cancels unfinished batch tasks, releases only active
  claims, and preserves ownership history, submissions/handoffs, snapshots,
  frozen manifests, validation results, and audit evidence. The recorded event
  contains reason, confirmation, before/after state, released paths, Git state,
  and archived finalization journal plan evidence.
- Interrupted finalization is reconciled through `finalization recover` before
  ownership release. Provisional commits return to the batch base, the index is
  clean, worker edits remain in the worktree, and the recovery result is embedded
  in abandonment evidence. Unknown branch, HEAD, index, journal, or recovery state
  fails closed as `batch_abandonment_quarantined`. README, CLI help, generated
  parent-agent guidance, the compatibility table, and the persisted batch-state
  migration document the supported workflow.
- Focused validation passed: `go test ./integration -run '^TestBatchAbandon'
  -count=1`; `go test ./internal/project -run
  'TestMigrateBatchAbandonmentSchemaPreservesForeignKeyTargets|TestCompatibilityTableDocumentsEveryPersistedState'
  -count=1`; `git diff --check`.
