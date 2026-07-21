# Prove the complete incident recovery path

Status: `ready-for-human`

## Parent

[Production-Safe Bandmaster Failure Recovery](../PRD.md)

## What to build

Create the final end-to-end CLI scenario based on the production incident. Multiple workers must claim disjoint paths, submit a batch dominated by new files plus other supported path shapes, freeze it, and pass official validation. The scenario must then inject a finalization failure and prove that Bandmaster preserves the work, cleans the index, diagnoses the state, restores compatible recovery state, and reaches a supported successful retry or abort without direct database editing.

This is the integration proof that fail-closed safety now has liveness. It should reuse public commands and existing fault-injection seams wherever possible.

## Acceptance criteria

- [ ] The scenario uses multiple submitted tasks with disjoint claims and a frozen manifest containing many additions plus representative modification, deletion, rename, and symlink changes.
- [ ] Official validation passes before the injected finalization failure.
- [ ] The failure response retains its initiating cause and any recovery detail.
- [ ] Rollback restores pre-batch HEAD, preserves every expected working-tree path state, and leaves the cached diff empty.
- [ ] Doctor reports the expected recovery findings and supported next action without mutation.
- [ ] Integrity and finalization recovery produce a compatible, publicly operable session/batch pair.
- [ ] The scenario completes through a supported successful retry or dry-run followed by confirmed abort, with immutable attribution still inspectable.
- [ ] No step edits the state database directly or relies on private implementation behavior.
- [ ] The complete repository test suite passes after the scenario is added.

## Blocked by

- [01 — Finalize tasks from frozen manifests](01-finalize-from-frozen-manifests.md)
- [02 — Roll back to a clean index without losing edits](02-clean-index-rollback.md)
- [03 — Restore compatible session and batch states](03-compatible-recovery-states.md)
- [04 — Preserve ownership evidence while releasing claims](04-preserve-ownership-evidence.md)
- [05 — Make abort atomic, retryable, and previewable](05-atomic-previewable-abort.md)
- [06 — Expose explicit finalization recovery](06-explicit-finalization-recovery.md)
- [07 — Support safe batch abandonment](07-safe-batch-abandonment.md)
- [08 — Diagnose recovery blockers with doctor](08-doctor-recovery-diagnostics.md)
- [09 — Align worker validation with the frozen barrier](09-frozen-barrier-validation-guidance.md)

## Comments

- Added a public-CLI-only end-to-end reproduction with three independently
  assigned/submitted workers and ten disjoint claimed paths: five new modules, a
  tracked modification, deletion, jointly owned rename, and symlink retarget.
  The frozen manifest retains all ten paths and official configured validation
  passes before finalization.
- Added a narrow post-index-normalization rollback bookkeeping fault seam. The
  injected hook/rollback failure returns the complete causal JSON chain
  (`git_commit_failed` plus `after-normalize-index`) after Git restoration has
  already returned HEAD to the batch base, preserved every path shape, and
  verified an empty cached diff. Doctor then reports the contradictory journal
  and unresolved integrity evidence without changing publicly inspected
  session, batch, task, or Git state.
- The scenario completes entirely through supported commands: audited integrity
  recovery restores a compatible finalizing pair; explicit finalization recovery
  reaches `active`/`repair_pending`; abort dry-run reports its worker-confirmation
  blocker, active claims, and preserved artifacts; confirmed abort terminates the
  session. Public task and batch inspection proves claims were released while
  ownership history, submissions, submitted snapshots, frozen manifest, and
  audit history remain inspectable. No state-database access or private helper is
  used by the scenario.
- Focused validation passed: `go test ./integration -run
  'TestProductionIncidentRecoversSafelyThroughPublicCLI|TestCommitBatchRollbackFailureReportsInitiatingAndRecoveryErrors'
  -count=1`; `go test ./internal/project -count=1`; `git diff --check`. Per the
  orchestration plan, the root agent owns the final complete-suite run.
