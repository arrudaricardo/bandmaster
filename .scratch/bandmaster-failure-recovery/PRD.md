# Production-Safe Bandmaster Failure Recovery

Status: `ready-for-human`

## Problem Statement

Bandmaster's normal parallel orchestration protects worker attribution and user changes well, but uncommon Git and state-machine failures can leave a safe repository with no supported path forward. In the observed incident, five workers held disjoint claims, submitted frozen snapshots, and passed official validation, yet finalization could not commit newly created files because it derived its expected path set from a Git command that omitted untracked files. Rollback preserved the work but restored additions into the index, which triggered quarantine. Integrity recovery then restored a finalizing batch without restoring the session to finalizing, and abort cleanup failed against snapshot foreign keys after it had already moved the session to aborting.

The user needs Bandmaster to retain its fail-closed safety model while also guaranteeing liveness: every recognized failure state must have an idempotent, supported, auditable recovery path. Operators must not need to edit the state database, discard worker work, or invent unsupported state transitions to finish or abandon a session.

## Solution

Bandmaster will make finalization, rollback, integrity recovery, and abort recovery operate from immutable persisted manifests and explicit compatible state transitions. Task commits will be planned from frozen baseline and submitted snapshots, including additions, deletions, renames, symlinks, and mixed tracked/untracked claims. Staging will be verified against that manifest rather than a live unstaged diff.

Rollback will restore working-tree contents and the index as separate concerns. It will preserve all observed user and hook changes while returning the index to its known clean pre-finalization state. Failures will retain both the initiating finalization error and any rollback error so inspection identifies the root cause as well as the recovery symptom.

Integrity recovery will restore session and batch status as one validated transition. Abort will preserve immutable ownership and submission evidence without allowing foreign-key dependencies to block cleanup, and its durable state transition will be atomic or safely retryable.

Bandmaster will also provide structured diagnostics and supported recovery commands. A read-only doctor command will report incompatible session/batch states, dangling finalization journals, staged rollback residue, and database blockers. Explicit finalization recovery, batch abandonment, and abort dry-run workflows will allow an operator to understand and resolve recognized failures without editing internal state.

## User Stories

1. As a repository user, I want finalization to commit newly created claimed files, so that batches dominated by new modules can complete.
2. As a repository user, I want finalization to commit modifications to tracked claimed files, so that existing behavior remains reliable.
3. As a repository user, I want finalization to commit claimed deletions, so that removing obsolete files is attributable and supported.
4. As a repository user, I want finalization to commit claimed renames when both source and destination are owned, so that moves are not mistaken for missing or foreign paths.
5. As a repository user, I want finalization to commit claimed symlink additions, changes, and deletions, so that snapshot fidelity is preserved for every supported file type.
6. As a repository user, I want one task to contain a mixture of tracked and untracked claimed changes, so that realistic refactors finalize as one attributed commit.
7. As a repository user, I want the expected commit contents derived from the frozen batch manifest, so that live Git presentation details cannot redefine validated work.
8. As a repository user, I want the staged index compared with the immutable task manifest before each commit, so that extra, missing, or misattributed paths fail closed.
9. As a repository user, I want finalization to reject a current path whose content or type differs from its submitted snapshot, so that validation cannot authorize different bytes.
10. As a repository user, I want task commits to contain all and only the manifest changes owned by that task, so that deterministic attribution remains intact.
11. As a repository user, I want a failed finalization to restore every worker edit, so that orchestration failure never discards useful work.
12. As a repository user, I want rollback to restore hook-produced Git-visible edits when policy permits preserving them, so that failure evidence and user work are not lost.
13. As a repository user, I want rollback to return the index to its known clean pre-finalization state, so that restored additions do not cause a second integrity failure.
14. As a repository user, I want rollback to preserve restored files as working-tree changes rather than staged changes, so that the repository matches its pre-finalization contract.
15. As a repository user, I want rollback to handle additions, modifications, deletions, renames, and symlinks, so that recovery has the same fidelity as capture.
16. As a repository user, I want retrying rollback or finalization recovery to be idempotent, so that interruption during recovery does not compound damage.
17. As an operator, I want an error response to retain the original finalization failure and any rollback failure, so that a rollback symptom does not hide the initiating defect.
18. As an operator, I want integrity events to include the original cause and recovery observations, so that the audit history explains the complete failure chain.
19. As an operator, I want a recovered finalizing batch to belong to a finalizing session, so that public commands can continue the workflow.
20. As an operator, I want a recovered frozen batch to belong to a finalizing session, so that validation and commit commands remain available.
21. As an operator, I want validation interruption recovery to restore a documented compatible session/batch pair, so that no half-transition is exposed.
22. As a repository user, I want incompatible session and batch states rejected at mutation boundaries, so that invalid combinations cannot spread.
23. As a repository user, I want state recovery to update session, batch, tasks, violations, and audit events atomically, so that a partial database transition is never committed.
24. As an operator, I want repeated integrity recovery against an already recovered state to return a stable result, so that retries are safe.
25. As a repository user, I want abort to preserve immutable ownership history, so that attribution remains inspectable after active claims are released.
26. As a repository user, I want abort to preserve submitted and frozen evidence needed to explain worker work, so that cleanup does not erase incident context.
27. As a repository user, I want active claims separated from immutable ownership evidence, so that releasing locks does not violate snapshot foreign keys.
28. As a repository user, I want aborting a session with submitted tasks to succeed, so that submission does not become an undeletable lock.
29. As a repository user, I want aborting frozen, quarantined, and finalizing work to follow explicit supported rules, so that every terminal choice is predictable.
30. As a repository user, I want abort database cleanup and its durable status transition to be atomic, so that a cleanup failure does not strand the session in aborting.
31. As an operator, I want a failed abort to be safely retryable, so that transient failures do not require database editing.
32. As an operator, I want an abort dry run to list affected tasks, claims, snapshots, batches, journals, and preserved files, so that I can understand the result before changing state.
33. As an operator, I want a read-only JSON doctor command, so that automation can diagnose inconsistent orchestration state without parsing prose.
34. As an operator, I want doctor to detect incompatible session/batch pairs, so that state-machine failures are identified directly.
35. As an operator, I want doctor to detect dangling or contradictory finalization journals, so that interrupted Git transactions are visible.
36. As an operator, I want doctor to detect staged rollback residue, so that a nonempty index is explained rather than reported as generic drift.
37. As an operator, I want doctor to detect foreign-key blockers before cleanup, so that abort can explain exactly which immutable artifacts depend on active records.
38. As an operator, I want doctor findings to use stable codes and structured evidence, so that supported recovery tooling can act on them.
39. As an operator, I want an explicit finalization recovery command, so that I can resume or roll back a journaled transaction without reissuing an ambiguous commit command.
40. As an operator, I want a supported batch abandonment command, so that an unrecoverable batch can stop blocking the session while retaining its evidence and edits.
41. As an operator, I want recovery commands to refuse unknown Git states, so that improved liveness does not weaken fail-closed safety.
42. As an operator, I want every recovery and abandonment action appended to audit history with confirmation and before/after states, so that intervention remains accountable.
43. As a parent Codex agent, I want workers to run focused checks while peers are still editing, so that shared-worktree package movement does not create misleading full-suite failures.
44. As a parent Codex agent, I want repository-wide validation to run once after the batch is frozen, so that the authoritative suite sees a stable combined snapshot.
45. As a repository user, I want an end-to-end incident test with multiple submitted tasks and many new files, so that the original failure cannot regress unnoticed.
46. As a repository user, I want that incident test to continue through failed finalization, rollback, integrity recovery, and abort, so that the entire recovery chain proves liveness.
47. As a repository user, I want all recovery assertions made through public CLI behavior and Git-visible results, so that tests remain stable across internal refactors.
48. As a repository user, I want successful finalization after a recoverable failure to remain possible where policy permits, so that preserving work is not the only successful outcome.
49. As a repository user, I want safe abandonment to leave the repository understandable and operable, so that choosing not to finish a batch is also a terminal supported outcome.
50. As a repository user, I want `go test ./...` to pass at the frozen barrier and after implementation, so that recovery changes do not regress Bandmaster's established safety model.

## Implementation Decisions

- Frozen batch paths are the authoritative finalization manifest. For each task, compare baseline and submitted snapshots to derive the exact changed path set; do not derive expected paths from a live unstaged Git diff.
- Before staging, verify each claimed path still equals its submitted snapshot. After staging, compare the complete cached change set and path states with the immutable per-task manifest before creating a commit.
- Model a rename as the jointly owned source deletion and destination addition recorded in the manifest. Git may display rename metadata, but correctness does not depend on rename detection heuristics.
- Preserve the existing exact snapshot model for absence, regular files, symlinks, content hashes, executable bits, and captured content. Additions, deletions, and type changes must therefore use the same comparison rules as freeze and submission.
- Treat the clean pre-finalization index as an explicit rollback invariant. Restore preserved working-tree content separately, then normalize the index to that invariant and verify both index and working tree before changing durable recovery state.
- Keep rollback fail-closed when the journal, branch, HEAD, hook activity, or preserved content cannot be reconciled with a known state. Improving recovery must not authorize guessing.
- Represent compound failures with a primary initiating error and optional recovery error details. Stable JSON error codes must identify both the root finalization failure and the rollback/quarantine outcome; audit evidence must retain the same causal chain.
- Define one explicit transition table for compatible session and latest-batch states. Integrity recovery and every recovery command must select and apply a whole compatible pair rather than update each entity opportunistically.
- A batch restored to frozen, validating, finalizing, or final-validating requires a finalizing session. A batch restored to repair-pending requires an active session. Terminal and abandoned batches must map only to documented session states.
- Enforce state compatibility at all public mutation preconditions. Add database-level enforcement where SQLite can express a reliable invariant without making legitimate multi-row transitions impossible; otherwise use a transaction-scoped invariant check immediately before commit.
- Separate active claim locking from immutable ownership history. Abort releases active claims while retaining task attribution, submitted snapshots, frozen batch paths, handoffs, audits, and failure evidence in records that do not depend on the active-lock row.
- Make the durable abort transition, task quarantine, claim release, artifact disposition, and audit append one database transaction after external worker-termination and monitor-stop preconditions have been satisfied. Re-entry from aborting must be safe and must complete the same plan.
- Define explicit abort behavior for submitted, frozen, quarantined, finalizing, and final-validating batches. Finalizing Git state must first be reconciled through finalization recovery, or an explicit abandonment workflow must preserve edits and clean the index before abort completes.
- Add a read-only `bandmaster doctor --json` contract with stable finding codes, severity, affected entity IDs and paths, evidence, and supported next actions. It must inspect state compatibility, journals, Git branch/HEAD/index state, rollback residue, unresolved integrity violations, and database dependency blockers.
- Add an explicit finalization recovery command that reports whether it can resume, roll back, or must quarantine. Repeated invocation after a completed recovery must be idempotent.
- Add a batch abandonment command for recognized nonterminal batches. It must preserve working-tree edits and immutable evidence, release only safe active ownership, record the reason and confirmation, and never silently convert unknown Git state into a clean state.
- Add `session abort --dry-run` with the same structured planning logic used by real abort. Dry-run performs no durable or Git mutation and reports blockers and preserved artifacts.
- Keep the parent-agent workflow guidance aligned with the recovery model: workers use focused validation during mutable parallel work; official repository-wide validation runs at the frozen barrier and again where finalization policy already requires it.
- Retain the current stable JSON CLI conventions, exit-code categories, append-only audit history, deterministic task commits, and no-automatic-push behavior.

## Testing Decisions

- The primary seam is a public CLI end-to-end integration scenario reproducing the incident: multiple independently claimed and submitted tasks; a frozen batch containing many additions plus representative modification, deletion, rename, and symlink changes; passing official validation; an injected finalization failure; rollback; integrity recovery; and a successful supported abort or retry. Assertions cover command responses, session/task/batch states, audit causality, HEAD, index cleanliness, preserved working-tree snapshots, and immutable ownership evidence.
- Tests should assert external behavior and durable contracts rather than private helper calls. Prefer invoking Bandmaster commands and inspecting their JSON results and Git-visible repository state.
- Extend the existing finalization integration seam, which already covers ordered commits, no-ops, hook failures, rollback preservation, hook ownership violations, crash injection at journal steps, unknown Git state, and committed hook changes.
- Add table-driven finalization integration cases for a new file, deletion, source-and-destination rename, symlink creation/change/deletion, executable-bit change, and mixed tracked/untracked claims. Each case verifies the exact committed path states and a clean index/worktree.
- Add rollback integration cases using the same path-shape matrix. Each case verifies that HEAD returns to the pre-batch commit, all observed content is preserved in the working tree, and the cached diff is empty.
- Add an error-causality case where finalization and rollback both fail. The structured response and integrity audit must expose both errors while leaving the batch quarantined.
- Extend existing integrity integration tests with every restorable batch state. Each case asserts the compatible session/batch pair after recovery and confirms that the next documented public command is accepted.
- Add mutation-precondition tests that arrange incompatible session/batch pairs through a controlled test fixture and verify that public commands fail without further state changes. Direct database setup is acceptable only for constructing otherwise unreachable corrupt states; outcomes are asserted through the CLI.
- Extend existing session abort integration tests beyond active edited work to submitted, frozen, quarantined, repair-pending, finalizing, and final-validating cases. Verify preservation policy, claim release, audit retention, atomicity, and retry behavior.
- Add schema migration and foreign-key tests proving that immutable snapshot and ownership evidence survives active-claim release without constraint violations.
- Add doctor integration tests for each finding code and for a healthy repository. Verify read-only behavior by comparing Git and database state before and after invocation.
- Add recovery-command idempotency tests for interrupted invocations and repeated successful invocations.
- Run focused package tests during implementation. Run the complete repository suite against stable, frozen fixtures as the authoritative acceptance check.

## Out of Scope

- Replacing cooperative same-user coordination with operating-system security isolation.
- Changing claim granularity from exact repository-relative paths.
- Weakening branch, HEAD, index, hook, monitor, or unclaimed-path integrity checks.
- Automatically pushing finalized commits or otherwise changing remote Git state.
- Automatically discarding preserved worker or hook changes to make recovery succeed.
- Running every worker in an isolated Git worktree. Snapshot-isolated worker validation remains a possible later enhancement; this spec standardizes focused worker checks and authoritative frozen-batch validation.
- Redesigning task planning, dependency gating, leases, heartbeats, assignment tokens, or ordinary submission behavior except where recovery must preserve their evidence.
- General-purpose repair of arbitrary manually edited state databases or unknown Git histories. Unknown states continue to quarantine and require human inspection.

## Further Notes

- This work strengthens liveness without changing Bandmaster's core safety posture. The observed incident demonstrated that fail-closed behavior successfully protected attribution and all worker changes; those guarantees are acceptance criteria, not defects to relax.
- The existing frozen batch path records already contain baseline and submitted snapshots and are the natural source of truth for commit planning and recovery verification.
- The highest-value regression test is the complete incident chain, because each individual defect amplified the next: untracked-path omission caused finalization failure, staged rollback residue caused quarantine, incompatible integrity restoration removed the public recovery path, and foreign-key cleanup prevented abort.
- Recovery is complete only when the operator can reach a supported terminal or forward-progress state using public commands. Merely preserving files while leaving an impossible session/batch pair does not satisfy this specification.
