# Expose explicit finalization recovery

Status: `ready-for-human`

## Parent

[Production-Safe Bandmaster Failure Recovery](../PRD.md)

## What to build

Add a public finalization recovery command that inspects a journaled transaction and reports whether Bandmaster can resume it, roll it back, or must quarantine it. The command must operate only on recognized branch, HEAD, index, journal, and hook states, produce stable JSON, and append an auditable transition when it acts.

Recovery must no longer depend on reissuing an ambiguous batch commit command. Repeated invocation after a successful recovery must return a stable idempotent result.

## Acceptance criteria

- [ ] A documented CLI command exposes finalization recovery with stable JSON fields and error codes.
- [ ] Known prepared, committing, and validating journal steps select a safe resume or rollback outcome.
- [ ] Unknown branch, HEAD, index, journal, or possible hook activity quarantines with structured evidence.
- [ ] Successful rollback preserves edits, restores a clean index, and returns a compatible session/batch pair.
- [ ] Recovery actions record before/after state, journal evidence, operator confirmation where required, and outcome in audit history.
- [ ] Repeating the command after successful recovery is idempotent.
- [ ] Existing interrupted-finalization tests are migrated or extended to exercise the explicit command.

## Blocked by

- [02 — Roll back to a clean index without losing edits](02-clean-index-rollback.md)
- [03 — Restore compatible session and batch states](03-compatible-recovery-states.md)

## Comments

- Implemented `bandmaster finalization recover` at the public CLI seam. Recognized
  `prepared`, `committing`, and `validating` journals require explicit operator
  confirmation and roll back to the compatible `active`/`repair_pending` pair;
  unknown branch, HEAD, index, journal, monitor, or hook evidence quarantines.
  Recovery records before/after state and immutable evidence in
  `finalization_recovery_events`, and successful results replay byte-for-byte on
  repeated invocation. `batch commit` now returns
  `finalization_recovery_required` for an existing journal. Help, generated-agent
  guidance, and README JSON/error-code documentation were updated.
- Focused validation passed: `go test ./integration -run
  'TestFinalizationRecoverRollsBackKnownInterruptedStepsAndIsIdempotent|TestCommitBatchQuarantinesExternalGitStateAfterInterruption|TestCommitBatchQuarantinesUnknownStateAfterInterruptedFinalization'
  -count=1`; `go test ./internal/project -count=1`; `git diff --check`. A broader
  finalization-only run encountered an existing integrity-monitor startup race in
  `TestCommitBatchIncludesAndAuditsStagedClaimHookChange`; the ticket-focused
  recovery suite passed independently.
