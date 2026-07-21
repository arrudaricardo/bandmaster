# Roll back to a clean index without losing edits

Status: `ready-for-human`

## Parent

[Production-Safe Bandmaster Failure Recovery](../PRD.md)

## What to build

Make failed finalization restore Git state transactionally: return HEAD and the branch to the journaled pre-finalization position, preserve every observed worker and permitted hook edit in the working tree, and independently normalize the index to its known clean pre-finalization state.

Rollback must retain the initiating finalization error even if recovery also fails. Structured output and integrity evidence must distinguish the root failure from the rollback symptom so an operator can understand the complete causal chain.

## Acceptance criteria

- [ ] Rollback preserves additions, modifications, deletions, renames, symlinks, executable changes, and permitted hook edits.
- [ ] Preserved additions return as unstaged working-tree files rather than staged index entries.
- [ ] Successful rollback restores the journaled branch and pre-batch HEAD and leaves the cached diff empty.
- [ ] Repeating rollback or recovery from an already restored known state is safe and does not duplicate or lose edits.
- [ ] Unknown or contradictory Git state continues to quarantine rather than being guessed into a clean state.
- [ ] When finalization and rollback both fail, stable JSON output and audit evidence expose both errors and identify the initiating cause.
- [ ] Integration tests cover the supported path-shape matrix and inspect public command responses and Git-visible state.

## Blocked by

- None — can start immediately.

## Comments

- Implemented transactional rollback index normalization after worktree restoration. Rollback now preserves the initiating finalization error alongside structured rollback failure details in CLI JSON and integrity audit evidence. Added public CLI integration coverage for additions, modifications, deletions, rename endpoints, symlinks, executable changes, permitted hook edits, restored branch/HEAD, an empty index, and compound failure reporting.
- Focused tests passed: `go test ./integration -run '^TestCommitBatch(RollsBackHookFailureAndPreservesEdits|RollbackPreservesMixedPathStatesWithCleanIndex|RollbackFailureReportsInitiatingAndRecoveryErrors|RecoversKnownInterruptedFinalizationSteps|QuarantinesExternalGitStateAfterInterruption)$' -count=1`.
