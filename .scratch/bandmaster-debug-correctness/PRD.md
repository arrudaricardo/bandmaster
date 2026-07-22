# Correct Bandmaster Debug Health Signals

Status: `ready-for-agent`

## Problem Statement

Bandmaster users rely on `bandmaster debug --json` to decide whether an orchestration session is healthy, whether observed database and Git evidence belongs to one coherent snapshot, and which supported recovery action is appropriate. The current debug runner can produce incorrect health signals during ordinary, healthy workflows.

An idle repository with a completed session is repeatedly marked `stable: false` and `best_effort: true` even though Git is unchanged, every monitor is stopped, and no mutation is occurring. A task that correctly claims individual files beneath a newly created directory can receive an error-level `unowned_worktree_drift` diagnostic because Git collapses that directory into one untracked path. Terminal canceled tasks can receive `lease_expired` diagnostics because the runner considers lease timestamps without considering whether the task can still own active work; these findings can include unusable recovery commands with an empty worker identity.

These false signals weaken the diagnostic contract. Operators cannot confidently distinguish a real concurrent mutation or ownership breach from a collector artifact, and automation may recommend aborting or recovering a session that is already safe and terminal.

## Solution

Make the debug runner's stability, worktree-ownership, and lease diagnostics reflect externally observable Bandmaster state accurately.

Database revision boundaries will be captured through one pinned read-only SQLite connection using revision values that are meaningfully comparable across the coherent read. A truly idle snapshot will be stable, while an actual concurrent public mutation will still trigger the bounded retry and best-effort behavior.

Git collection will enumerate every untracked file rather than accepting Git's collapsed directory presentation. Worktree paths will therefore correlate directly with Bandmaster's exact claim paths, preserving error-level `unowned_worktree_drift` only for real unclaimed files.

Lease timing diagnostics will apply only to task states that can still carry live worker ownership. Terminal tasks will retain historical lease evidence in the snapshot but will not receive actionable expiry diagnostics. Suggested recovery commands will only be emitted when their required worker identity and lifecycle preconditions exist.

The public diagnostic schema and stable diagnostic codes will remain compatible. The fixes change incorrect classification, not the meaning of valid findings.

## User Stories

1. As a Bandmaster operator, I want an idle diagnostic snapshot marked stable, so that I can trust it as a coherent observation.
2. As a Bandmaster operator, I want a changing repository marked best effort only when a real concurrent change occurs, so that ordinary inspection does not look degraded.
3. As an automation author, I want database revision boundaries to be comparable, so that I can branch reliably on `collection.stable`.
4. As an automation author, I want the existing bounded retry behavior preserved, so that transient concurrent changes can still yield a stable second observation.
5. As an automation author, I want a persistently changing repository to remain explicitly best effort, so that the fix does not hide real instability.
6. As a parent Codex agent, I want newly created claimed files recognized individually, so that safe worker edits are not classified as unowned drift.
7. As a parent Codex agent, I want a new directory containing only claimed files to produce no ownership diagnostic, so that normal task scaffolding remains healthy.
8. As a parent Codex agent, I want a new directory containing one unclaimed file to retain an error diagnostic for that exact file, so that real ownership breaches remain visible.
9. As a Bandmaster maintainer, I want worktree evidence to use the same exact path vocabulary as claims, so that diagnostic correlation is deterministic.
10. As a Bandmaster maintainer, I want renamed and modified tracked paths to remain correctly represented, so that expanding untracked files does not regress other Git states.
11. As an operator, I want active expired leases reported, so that abandoned live ownership remains actionable.
12. As an operator, I want leases approaching expiry reported only for live tasks, so that warnings represent work that can still be heartbeated.
13. As an operator, I do not want canceled tasks diagnosed as having actionable expired leases, so that terminal history is not mistaken for current ownership.
14. As an operator, I do not want committed or no-op tasks diagnosed as having actionable expired leases, so that completed batches remain healthy.
15. As an incident investigator, I want terminal tasks to retain lease timestamps in the snapshot, so that historical evidence is preserved without false actionability.
16. As an incident investigator, I want suggested recovery commands to include all required identities, so that the runner never recommends a malformed command.
17. As an incident investigator, I want diagnostics to continue using stable codes and structured evidence, so that existing consumers remain compatible.
18. As a user, I want all fixes to remain strictly read-only, so that obtaining more accurate evidence never changes orchestration state.
19. As a security-conscious user, I want the fixes to preserve token and content redaction, so that more accurate diagnostics remain safe to share.
20. As a Bandmaster maintainer, I want regression tests at the compiled CLI boundary, so that future collector refactors cannot reintroduce these false signals.
21. As a Bandmaster maintainer, I want tests to distinguish health from command success, so that a successful debug command can still report genuine unhealthy state.
22. As a Bandmaster maintainer, I want existing watch mode to consume the corrected snapshot model automatically, so that one-shot and live diagnostics cannot disagree.
23. As a Bandmaster maintainer, I want no private database layout added to the public contract, so that the fix does not increase schema coupling.
24. As a Bandmaster maintainer, I want the complete Go test suite to remain green, so that diagnostic corrections do not regress orchestration, recovery, or TUI behavior.

## Implementation Decisions

- Keep the compiled `bandmaster debug --json` command as the primary behavioral seam and preserve the current versioned response contract.
- Pin diagnostic database collection to one read-only SQLite connection. Capture revision boundaries in a way that compares the same connection's view before and after the coherent read transaction.
- Preserve the current two-attempt collection policy: return immediately when stable, retry once after observed change, and mark only the second unstable result as best effort.
- Continue collecting all database entities inside one coherent read transaction. Do not acquire mutation locks or invoke mutation preparation.
- Collect Git status with full untracked-file expansion so repository paths and exact claim paths use the same granularity.
- Preserve existing handling for tracked modifications, index changes, deletions, and rename destinations while changing only untracked-directory expansion.
- Continue deriving owned paths from authoritative task claims. Do not infer ownership merely because a changed file is below a claimed directory; claims remain exact according to existing path semantics.
- Emit `unowned_worktree_drift` for each genuinely unclaimed Git-visible file, using the exact affected path and existing severity and suggested actions.
- Gate `lease_expired` and `lease_expiring` diagnostics on task states that can carry live worker ownership. Terminal states such as committed, no-op, and canceled are non-actionable even when historical lease rows remain active in old state.
- Preserve historical lease objects in task snapshots. Suppress only incorrect actionable diagnostics; do not rewrite or normalize persisted lease history during debug collection.
- Construct recovery and heartbeat suggestions only when the task state supports the action and every required identity is present. Never emit a command with an empty worker identity.
- Keep the fixes read-only and additive to collector correctness. Do not alter task transitions, lease expiry enforcement, monitor behavior, claim semantics, database schema, or recovery commands.
- Let one-shot human output, JSON watch snapshots, semantic change records, heartbeats, and the TUI benefit from the same corrected normalized snapshot rather than adding parallel fixes in each renderer.

## Testing Decisions

- Use compiled CLI integration tests in temporary Git repositories as the single primary seam. Tests will assert public JSON fields and diagnostic codes rather than private helper results or raw SQLite rows.
- Add an idle initialized-repository scenario that runs consecutive snapshots with no intervening mutation and asserts complete, stable, non-best-effort collection with unchanged database and Git revision boundaries.
- Add a concurrent public-mutation scenario to prove that real revision changes still cause retry and, when change persists, an explicitly best-effort result.
- Add a task workflow that claims multiple absent files beneath a new directory, writes those files, and asserts that no `unowned_worktree_drift` diagnostic is emitted.
- Extend that workflow with one unclaimed file and assert that the diagnostic identifies only the exact unclaimed file.
- Exercise tracked modifications, deletions, and renames through the existing Git diagnostic integration coverage to ensure full untracked expansion does not change their behavior.
- Arrange a live assigned or editing task with an expired lease and assert that `lease_expired` remains error-level, identifies the task and worker, and suggests a complete supported recovery command.
- Arrange canceled, committed, and no-op tasks with historical expired lease evidence and assert that their lease data remains visible without `lease_expired` or `lease_expiring` diagnostics.
- Assert that no suggested action contains an empty worker identity or unresolved identity placeholder other than documented operator-supplied proof or token placeholders.
- Preserve existing redaction and read-only assertions around every new scenario.
- Reuse the repository's existing debug integration harness and public workflow helpers as prior art; add focused internal tests only if a concurrency condition cannot be made deterministic through the compiled CLI.
- Run the complete Go test suite as final acceptance because debug snapshots are shared with watch mode and the terminal dashboard.

## Out of Scope

- Changing the persisted task, lease, claim, batch, monitor, or integrity state machines.
- Repairing or deleting historical findings from previously aborted sessions.
- Changing `doctor` aggregation across historical sessions.
- Fixing the separately observed historical monitor process that recorded findings after an aborted retry session.
- Installing or replacing a stale user-level Bandmaster executable; authorized verification must still build and run a fresh executable from the fixed source.
- Changing diagnostic codes, JSON schema versions, unsafe secret access, redaction policy, history limits, watch polling, or TUI presentation.
- Automatically recovering, aborting, heartbeating, or otherwise mutating orchestration state in response to a diagnostic.
- Changes to the separate Rust migration.

## Further Notes

- Runtime evidence reproduced all three failures with a fresh executable built from the repository source. The user-level installed executable was older and did not support `debug`, so it must not be used as verification evidence.
- The revision-stability failure reproduced repeatedly after session completion with all monitors stopped and a clean Git worktree: Git revisions matched while the reported database revisions differed on every snapshot.
- The unowned-drift failure reproduced twice during normal Bandmaster batches, once for a new crate directory and once for a new tests directory. In each case every file was claimed and the integrity monitor reported no violation.
- The terminal-lease failure appeared on canceled tasks from an aborted historical batch and produced suggested recovery commands without a worker identity.
- Debug command success and runtime health remain separate concepts: a complete snapshot can legitimately succeed while reporting genuine error diagnostics.
