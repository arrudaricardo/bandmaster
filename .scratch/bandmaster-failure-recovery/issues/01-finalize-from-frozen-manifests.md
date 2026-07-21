# Finalize tasks from frozen manifests

Status: `ready-for-human`

## Parent

[Production-Safe Bandmaster Failure Recovery](../PRD.md)

## What to build

Make task-attributed finalization use the frozen batch's immutable baseline and submitted snapshots as its commit manifest. A validated task must commit its exact owned changes regardless of whether Git presents them as tracked modifications, untracked additions, deletions, renames, symlinks, or executable-bit changes.

Before staging, Bandmaster must confirm that every owned path still matches its submitted snapshot. After staging, it must verify that the cached path states match all and only the manifest changes for the current task. Rename correctness must come from the jointly claimed source deletion and destination addition rather than Git rename-detection heuristics.

## Acceptance criteria

- [ ] Newly created claimed files pass the complete freeze, validate, and commit lifecycle.
- [ ] Modifications, deletions, source-and-destination renames, symlink changes, and executable-bit changes pass the same lifecycle.
- [ ] A task containing mixed tracked and untracked changes produces one exact task-attributed commit.
- [ ] A path whose live state differs from its submitted snapshot fails closed before commit.
- [ ] Staging an extra path or omitting a manifest path returns a stable path-mismatch error.
- [ ] Successful finalization leaves the index and working tree clean and preserves deterministic task commit behavior.
- [ ] Integration tests assert behavior through public CLI commands and Git-visible results.

## Blocked by

- None — can start immediately.

## Comments

- Implemented finalization from `frozen_batch_paths`, with live submitted-snapshot checks, NUL-safe/no-rename path comparisons, and cached index snapshot verification for regular files, symlinks, executable modes, and deletions. Added public CLI integration coverage for a mixed tracked/untracked lifecycle and post-validation snapshot drift. Focused tests passed: `go test ./integration -run '^(TestCommitBatchUsesFrozenManifestForMixedGitPathStates|TestCommitBatchRejectsPathDriftFromFrozenSubmittedSnapshot)$' -count=1` and nearby ordered/no-op and hook finalization regressions.
