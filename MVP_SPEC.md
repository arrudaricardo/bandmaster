# Bandmaster MVP Specification

## Summary

Bandmaster is a local orchestration CLI for parallel Codex coding agents. It
coordinates tasks, assigns exclusive file ownership, detects unsafe shared
workspace changes, validates completed work, and creates attributable Git
commits.

All agents share one Git working directory and one dependency installation.
Bandmaster does not use Git worktrees.

## Product Goals

- Let a parent Codex agent split suitable work into parallel tasks.
- Prevent cooperative agents from intentionally editing the same file at the
  same time.
- Attribute every committed file change to one task.
- Keep orchestration state outside the LLM conversation so interrupted work
  can be inspected and resumed.
- Validate a completed batch before committing it.
- Create one deterministic, reviewable commit per task that changes files.
- Recover safely from worker, parent, validation, hook, and commit failures.

## Non-Goals

- Hard filesystem isolation between agents.
- Git worktrees or per-task branches.
- Line-level or hunk-level ownership.
- Multiple concurrent Bandmaster sessions in one repository.
- Multiple agent hosts; the MVP supports Codex only.
- AI merge-conflict resolution.
- Semantic AI review of otherwise clean changes.
- Remote Git operations or automatic pushes.
- Git submodules, nested repositories, or sparse checkouts.
- Linked Git worktrees, including repositories whose common Git directory is
  shared with another worktree.
- Native Windows support.
- Rollback of side effects outside Git-visible repository paths, including
  network calls and writes to ignored paths.

## Key Constraint

Codex subagents run as the same Unix user. Unix permissions and advisory file
locks cannot reliably distinguish one subagent from another. Bandmaster uses a
cooperative protocol backed by detection rather than claiming hard process-level
isolation.

The protocol can detect many unsafe writes, but it cannot always identify which
same-user process modified a file owned by an active worker. Workers can also
read another worker's in-progress changes. These are accepted MVP limitations.

Continuous monitoring is best effort. A path changed and restored between
observations may not be detected. Full repository scans at workflow transitions
and batch barriers are authoritative for the state visible at those points, but
Bandmaster does not claim to detect every transient write.

## Supported Environment

- macOS and Linux.
- Standard Git repositories and monorepos.
- Repositories that are not using linked Git worktrees.
- A clean, non-detached current branch at session start.
- One active session per repository.
- Local commits to the branch that was current when the session started.
- Go implementation distributed through `go install` and release binaries.

## Actors

### Parent Agent

The parent Codex agent is the sole orchestrator. It:

- Loads the project-local Bandmaster skill.
- Decides whether work contains at least two independent tasks.
- Creates a simple task dependency graph.
- Spawns all currently unblocked workers without a Bandmaster concurrency cap.
- Requeues workers blocked on file claims.
- Waits for a batch barrier before finalization.
- Diagnoses failed validation and assigns repair workers.
- Confirms worker termination through the Codex worker handle before claims can
  be recovered. If that handle is unavailable, recovery requires explicit user
  confirmation and remains quarantined until then.
- Never delegates agent-spawning authority to workers.

### Worker Agent

A worker owns one task. It:

- Performs read-only preflight inspection.
- Declares its complete expected write set and focused validation commands.
- Acquires all initial file claims atomically before editing.
- Uses the opaque assignment token created by the parent assignment command on
  every mutating worker command, including the initial claim.
- Uses Bandmaster heartbeats during implementation.
- Does not run Git commands that mutate the index, HEAD, branches, or working
  tree.
- Reviews its complete claimed-file diff before submission.
- Submits a structured implementation summary.
- Stops editing after submission freezes its path snapshots.

Workers may run normal repository commands. Such commands remain subject to the
same ownership and integrity checks as direct edits.

### Bandmaster CLI

Bandmaster is the source of truth for:

- Sessions and task dependencies.
- Persisted batches and their membership.
- File claims and leases.
- Baseline and submitted path snapshots.
- Structured worker handoffs.
- Validation results.
- Git operations and task commits.
- Recovery journals and append-only audit events.

Only Bandmaster may mutate Git state during an active session.

## Codex Skill

`bandmaster init` installs or overwrites the current project-local Codex skill.
It does not attempt to merge local edits to the generated skill.

The skill is model-invoked. It starts Bandmaster orchestration only when Codex
can identify at least two independently implementable and testable tasks.

The skill instructs the parent and workers to use stable JSON command output.
CLI commands remain human-readable by default and support `--json` for agent
use.

If a later Codex session discovers an interrupted Bandmaster session, the skill
reports it and offers to resume rather than spawning workers automatically.
Workers from the interrupted parent are treated as quarantined unless their
termination can still be proven. A new parent never assumes that loss of a
worker handle means the worker stopped.

## Project Initialization

`bandmaster init`:

- Verifies that the repository layout is supported.
- Creates a versioned `.bandmaster.yaml` project configuration.
- Detects likely repository validation commands and writes them as suggestions.
- Marks generated validation configuration as unapproved and requires the user
  to approve its exact configuration digest before a session can start. Any
  later validation configuration change invalidates that approval.
- Installs or updates the project-local Codex skill.

Runtime state is stored under the repository's common Git metadata directory,
for example `.git/bandmaster/state.db`. It is local, untracked, and shared by
all processes in the repository.

The local runtime state stores the approved configuration digest. Approval is
not embedded into the hashed configuration itself and therefore is not
self-referential. A new clone or removed runtime state requires fresh approval.

## Session Preconditions

A session may start only when:

- The current branch is attached and clean.
- The Git index is clean.
- No other Bandmaster session is active.
- The repository uses a supported layout.
- The Bandmaster configuration is valid.
- The configured validation commands have been explicitly approved.

The starting branch and commit SHA are recorded. If the branch, HEAD, index, or
unclaimed working tree changes outside Bandmaster, the session pauses rather
than adapting automatically.

## State Model

Bandmaster validates every state transition in one central transition module.
CLI handlers do not update SQLite, Git, claims, or journals directly. A
transition either commits its complete SQLite change and emits its audit events,
or leaves the prior durable state intact. Filesystem and Git operations that
cannot share the SQLite transaction use persisted intent and recovery records.

### Session States

A session is in exactly one of these states:

- `active`: tasks may be planned, assigned, or edited.
- `paused`: no new worker may be assigned; inspection and explicit recovery are
  allowed.
- `finalizing`: Bandmaster exclusively controls Git while a batch is being
  validated or committed.
- `completed`: all accepted work is committed and the repository is clean.
- `aborting`: workers are being stopped and claims are not yet safe to clear.
- `aborted`: orchestration has stopped and preserved changes remain available
  for inspection.

Integrity violations, missing monitor heartbeats, ambiguous Git state, and
unprovable worker termination move the session to `paused`. Resuming requires
the condition to be resolved or explicitly acknowledged through a recorded
recovery action.

Freezing a batch for validation moves `active -> finalizing`. A successful
intermediate batch moves `finalizing -> active`; if all completion conditions are
met it may instead move `finalizing -> completed`. An unambiguous validation or
finalization failure that is not an integrity violation moves the batch to
`repair_pending` and the session back to `active`. Every integrity violation and
every ambiguous failure moves the batch to `quarantined` and the session to
`paused`. Abort moves `active` or `paused -> aborting -> aborted` after worker
termination and claim cleanup are resolved.

### Task States

A task is in exactly one of these states:

- `planned`: its mutable planning fields or prerequisites are not ready.
- `ready`: all prerequisites completed successfully and it may be assigned.
- `assigned`: a worker may inspect and attempt its initial claim.
- `editing`: its initial claims were acquired and its worker may write.
- `blocked`: its all-or-nothing claim request failed and it has no claims.
- `submitted`: its claimed path snapshots and handoff are frozen.
- `repair_pending`: its submission is invalid or incomplete and it awaits a
  replacement worker.
- `quarantined`: its worker or file integrity is uncertain.
- `committed`: its changed paths were committed successfully.
- `no_op`: its no-change submission completed in a successful batch.
- `canceled`: it was deliberately removed before completion.

The normal path is `planned -> ready -> assigned -> editing -> submitted ->
committed`. A failed initial claim moves `assigned -> blocked`; requeueing moves
`blocked -> ready`. Worker failure while edits exist and failed validation move
the owning task to `repair_pending`. Assigning a replacement to retained claims
moves `repair_pending -> editing` with a new token; it does not reacquire the
claims or change their baselines. Explicit recovery moves `quarantined ->
repair_pending` only after the prior worker is proven stopped. A no-change task
remains `submitted` until its batch succeeds, then moves `submitted -> no_op`.
A prerequisite is satisfied by `committed` or `no_op`, but only after its batch
has succeeded. Canceling a task with dependents requires canceling or replanning
all dependents that have not started.

Cancellation is allowed only from `planned`, `ready`, `blocked`, or `assigned`
before the task has acquired any claim or joined a batch. An assigned worker must
be proven stopped and its token revoked first. An `editing`, `submitted`,
`repair_pending`, or `quarantined` task must be completed, repaired, or handled
by aborting the session; it cannot discard owned changes through cancellation.

### Batch States

A batch is a first-class persisted record in exactly one of these states:

- `collecting`: independently ready tasks may acquire claims and join it.
- `frozen`: membership is closed and all member workers have stopped editing.
- `validating`: official validation is running.
- `repair_pending`: one or more member tasks must be reopened for repair.
- `repairing`: membership is closed and reopened member tasks may edit.
- `finalizing`: provisional task commits are being created.
- `final_validating`: required validation is running against the provisional
  committed batch.
- `committed`: every changed member task committed, every no-change member task
  became `no_op`, and final validation passed.
- `quarantined`: integrity or recovery is ambiguous.

The normal path is `collecting -> frozen -> validating -> finalizing ->
final_validating -> committed`. A command failure without an integrity violation
moves the batch to `repair_pending`. Every integrity violation and ambiguous
recovery moves it to `quarantined`. Reopening selected members moves
`repair_pending -> repairing`; their resubmission and a new barrier move
`repairing -> frozen`. Explicit recovery may move `quarantined ->
repair_pending` only after the ambiguity is resolved and audited. Repair never
reopens batch membership, changes its base SHA, or admits unrelated tasks.

## Batch Model

Each batch records at least:

- Stable batch ID and creation order.
- Base branch name and base commit SHA.
- Member task IDs and frozen membership order.
- Status and integrity state.
- Frozen pre-validation path manifest.
- Validation commands, attempts, and results.
- Current validation phase, either pre-commit or final.
- Pre-finalization index state and working-tree manifest.
- Commit order, provisional commit SHAs, and current journal step.
- Final commit SHA or rollback result.

A task joins the current collecting batch only when its initial claim succeeds.
A blocked task does not become a member. Freezing a batch closes membership;
tasks that become ready later wait for a subsequent batch. Tasks in the same
batch cannot depend on each other because prerequisites must have completed
successfully in an earlier batch before assignment.

## Task Model

Each task records at least:

- Stable task ID.
- Creation order.
- Title.
- Behavioral intent and expected outcome.
- Prerequisite task IDs.
- Status.
- Current batch ID, when assigned to a batch.
- Worker identity, opaque assignment token, and lease state.
- Claimed files and their baseline path snapshots.
- Focused validation commands.
- Structured submission summary.
- Resulting commit SHA, when committed.

Planning fields can change only while a task is `planned`, `ready`, or `blocked`.
Assignment freezes them for that worker attempt, and acquiring the first claim
makes title, intent, dependencies, and expected outcome permanently immutable.
Changing planning fields after assignment requires first stopping the worker,
revoking its token, and returning the task to `planned` or `ready`.

Only tasks whose prerequisites are `committed` or `no_op` in a successful prior
batch may be assigned to workers.

Assignment tokens prevent accidental cross-task commands but are not a security
boundary because all agents run as the same Unix user. Reassignment revokes the
old token and creates a new token only after the previous worker has been proven
stopped.

## File Claims

### Granularity

Claims apply to exact, canonical, repository-relative file paths. They may
refer to existing files or absent destination paths for file creation and
renames. Renames require claims for both source and destination paths.

For the MVP, a canonical claim path:

- Is valid UTF-8 and uses `/` separators.
- Is relative to the worktree root and contains no empty, `.` or `..` segment.
- Does not name `.git`, a directory, a nested repository, or a path outside the
  worktree.
- Uses the path spelling known to the Git index for an existing tracked path.
- Does not traverse a symlink in a parent segment. A tracked symlink itself may
  be claimed, but a path reached through it may not.
- Does not alias another claim through filesystem case folding or Unicode
  normalization. Bandmaster detects the worktree filesystem behavior and
  rejects ambiguous aliases, including absent destinations it cannot resolve
  safely.

The Git-visible state of a claimed path is represented by a path snapshot, not
only a content hash. A snapshot records absence or presence, regular-file or
symlink type, a hash of the exact stored bytes or symlink target, and the Git
executable bit. Unsupported file types are rejected.

The MVP does not support directory globs, symbols, line ranges, or hunks.

Claims protect writes only. Workers may read files claimed by another worker,
accepting the risk of seeing in-progress content.

### Acquisition

During preflight, a worker declares its complete initial write set and focused
validation commands. The complete set is acquired in one SQLite transaction.
Bandmaster serializes claim operations, captures each baseline path snapshot
before granting the claims, and verifies the assignment token created when the
parent assigned the task. The transaction rechecks that every prerequisite
completed successfully in a prior batch and that the selected collecting batch
still has the expected base SHA. Cooperative workers must not write until the
claim transaction returns successfully.

If any requested file is unavailable:

- No requested claim is granted.
- The worker reports that it is blocked and exits.
- The parent requeues it after relevant claims are released.

A worker may expand its write set only through an atomic, non-waiting request.
Every additional file must be immediately available or the expansion grants
nothing. This avoids workers waiting on each other while holding partial claim
sets.

A worker may release an unused claim early only when Bandmaster verifies that
the path still matches its complete recorded baseline snapshot.

### Leases

Every worker CLI action renews its lease. The skill requires explicit heartbeat
commands during long periods without Bandmaster activity.

Lease expiration does not make files immediately available. It moves the task
and its claims into quarantine. The parent must confirm that the old worker has
stopped before a replacement can inherit the task, claims, baselines, and
current edits.

This prevents a slow original worker and a replacement from concurrently
writing the same shared file.

Lease expiry alone is never proof of termination. Recovery requires either a
successful cancellation and termination result from the parent-held Codex
worker handle or explicit user confirmation when that handle was lost. The
confirmation is recorded in the audit history with the new assignment token.

## Integrity Monitor

Bandmaster starts one long-lived repository integrity monitor for an active
session. The monitor records a process identity and heartbeat in SQLite. Every
mutating CLI command checks that the monitor is healthy; loss of the monitor
pauses the session until it is restarted and a full scan succeeds. The monitor
complements SQLite claims but does not attempt to enforce Unix permissions.

The monitor combines filesystem notifications with full scans before and after
important workflow transitions. Notifications are advisory and improve the
chance of observing transient writes. Full scans are authoritative for current
Git-visible state, but a write restored between observations may go undetected.

It detects and records:

- Changes to unclaimed, non-ignored paths.
- Changes to submitted paths after their snapshots are frozen.
- Unexpected Git index, HEAD, or branch changes.
- Base branch drift.

Every integrity violation quarantines the affected task or batch, pauses the
session, and prevents finalization, even when Bandmaster can restore Git state
unambiguously. The append-only audit history records the path, observed state,
and timestamp. Only explicit audited recovery can move the batch from
`quarantined` to `repair_pending`.

The monitor cannot reliably identify a process that writes a file currently
claimed by another active worker. Cooperative compliance with the generated
skill remains required.

Integrity monitoring covers every tracked path and every untracked path not
excluded by standard Git ignore rules. An untracked ignored path is outside
claim, attribution, validation-mutation, and rollback guarantees. A tracked path
remains covered even if an ignore rule also matches it. Changes to ignored
caches or generated output may affect workers, so repository validation commands
should avoid depending on concurrently mutable ignored state where practical.

## Worker Submission

Before submission, the worker must:

1. Review the complete Git-visible diff from each claimed path's baseline
   snapshot to its current snapshot.
2. Confirm that all changed paths are claimed by its task.
3. Provide a structured summary containing behavior changed, key decisions,
   validation expectations, and known risks.
4. Submit through a high-level Bandmaster command.

Submission records and freezes the complete snapshot of every claimed path. Any
later change before finalization invalidates the submission and quarantines the
batch. Changes made by a Git hook during the owning task's commit are handled by
the transactional finalization rules below.

If every claimed path still matches its baseline, submission records a no-change
flag while the task remains `submitted`. It creates no commit and joins no
finalization order. The task becomes `no_op` only when its containing batch
succeeds; its handoff and audit history remain recorded.

Because the session starts clean and claims are exclusive, all changes in a
claimed file are attributed to its owning task. Bandmaster does not attempt
line-level attribution.

## Batch Barrier

Agents edit concurrently, but validation and commits happen at a batch barrier.
The parent reaches the barrier only after every active worker has stopped
editing by submitting, blocking, failing, or being deliberately stopped.

Reaching the barrier freezes the current `collecting` or `repairing` batch.
Blocked tasks with no claims and tasks that never joined the batch wait for a
later batch.

Before validation, Bandmaster verifies:

- No worker remains active.
- Every batch member is `submitted`; no member remains `assigned`, `editing`,
  `repair_pending`, or `quarantined`.
- Every changed non-ignored path has exactly one submitted owner.
- Every submitted path still matches its frozen snapshot.
- No unclaimed path changed.
- The Git index remains under Bandmaster's control.
- The current branch and base state have not drifted.
- No claim or task in the batch is quarantined.

Tests run against the combined completed batch because all agents share one
working tree.

## Validation

Bandmaster, not the agents, executes and records official validation commands.
It runs:

- Focused commands declared by workers during preflight.
- Repository-wide commands from `.bandmaster.yaml`.

Commands run sequentially after the batch barrier so their results are not
affected by active editors.

Each command has a stable name, an argument vector or explicitly requested shell
script, a canonical repository-relative working directory, a timeout, and
declared environment overrides. Bandmaster records the resolved command,
working directory, exit status, duration, and bounded stdout and stderr. Commands
run with the repository root as the default working directory and inherit a
documented minimal environment plus their overrides.

Official validation commands must not change Git-visible paths. Bandmaster scans
before and after each command. A mutation is an integrity failure even if the
command exits successfully; ignored untracked output remains outside this
guarantee.

If a validation command exits unsuccessfully without an integrity violation:

- No task is committed.
- Claims and working changes remain in place.
- The parent diagnoses the combined failure.
- The parent assigns repair workers to one or more existing owning tasks and
  reruns the barrier.

A validation command that mutates a Git-visible path follows the integrity
violation path instead: the batch is quarantined and the session pauses before
any repair assignment.

## Repair Protocol

Repairs preserve the original ownership attribution. They apply both to a
submitted task reopened after validation or finalization failure and to an
unsubmitted task whose worker stopped with partial edits. The parent selects the
task or tasks that own the paths requiring changes. For each selected task,
Bandmaster:

1. Verifies that the selected task's prior worker has stopped.
2. Invalidates any frozen submission, records the current partial-edit snapshot,
   and moves the task to `repair_pending` without releasing its claims or
   changing its baselines.
3. Records the diagnosis and intended repair in the task handoff history.
4. Creates a new assignment token and moves the task directly to `editing` with
   one replacement worker because its claims are already held.
5. Leaves an initially collecting batch open after an ordinary worker failure,
   or moves a previously frozen `repair_pending` batch to `repairing` with its
   membership closed.

A repair worker may edit only the claims owned by its task and may use normal
atomic claim expansion for currently unowned paths. Claims are never silently
transferred between tasks. If a repair requires paths owned by multiple tasks,
the parent reopens each owning task and repairs them separately, sequentially or
in parallel when their write sets remain disjoint. The batch cannot return to
the barrier until every reopened task has submitted again.

## Transactional Finalization

After validation succeeds, Bandmaster creates one provisional commit per
submitted task with Git-visible changes, in task creation order. Submitted tasks
with the no-change flag do not create commits.

Each commit:

- Contains all and only the Git-visible changes under that task's claims.
- Uses an exact deterministic message derived from task title and task ID.
- Includes task intent and Bandmaster metadata in the body.
- Runs the repository's normal Git commit hooks.

The batch is transactionally all-or-nothing: after recovery, either every
provisional commit remains on the starting branch and final validation passed,
or the branch and index return to the pre-batch state with the batch changes
restored as uncommitted working-tree changes. Intermediate commits are visible
while finalization runs, so this is recoverable transactional behavior rather
than strict isolation or atomic visibility.

Before changing Git state, Bandmaster records and durably flushes:

- The expected branch and pre-batch commit SHA.
- The clean pre-finalization index state.
- The frozen batch manifest and recoverable content objects needed to recreate
  every Git-visible working-tree change.
- The ordered task commit plan and the first journal step.

For each task, Bandmaster starts from the expected HEAD, stages only that task's
claimed changes, verifies the staged path set, records its intent in the journal,
and invokes normal `git commit` behavior. After Git returns, Bandmaster verifies:

- HEAD advanced by exactly one commit from the expected parent.
- The commit contains no path outside the task's claims.
- Every claimed task change is contained in the commit and no claimed path is
  left dirty relative to the new HEAD.
- The final commit message exactly matches the deterministic message.
- No other task's path, unclaimed path, branch, or unexpected index entry
  changed.

While a task's commit hooks run, the integrity monitor permits changes only to
that task's claimed paths and expected Git metadata. A hook change to another
path is an integrity violation. A claimed-path hook change is accepted and
becomes the task's new recorded snapshot only when it is included in the
resulting commit, and Bandmaster records the hook-produced diff in the task audit
history. If a successful hook leaves a claimed path unstaged or dirty,
finalization fails rather than silently amending or creating an extra commit.
Hooks that alter the commit message cause the deterministic-message check to
fail.

Hooks execute in the shared working tree and can observe other submitted tasks'
uncommitted changes. Individual provisional commits are not required to pass
repository validation in isolation; the frozen combined batch is the validated
unit. Hooks and validation commands may also have network, process, or ignored-
path side effects that Bandmaster records where observable but cannot roll back.

After all provisional task commits, Bandmaster verifies a clean Git-visible
working tree and index, then reruns required final validation against the
committed batch in `final_validating`. Successful final validation advances the
batch to `committed`, records every resulting SHA, moves changed tasks to
`committed`, moves submitted no-change tasks to `no_op`, releases the claims,
and ends the transaction.

If a hook, commit, integrity check, or final validation fails, Bandmaster first
captures the Git-visible working state observed at failure, including permitted
hook changes. It then restores the original branch to the pre-batch SHA, restores
a clean index, and recreates that captured state as uncommitted working-tree
changes. Provisional commits may remain as unreachable Git objects. Claims stay
held and affected submissions are invalidated. An ordinary hook, commit, or
validation command failure enters `repair_pending` when rollback is unambiguous.
Every integrity violation and every ambiguous rollback enters `quarantined` and
pauses the session.

If Bandmaster crashes during finalization, the next invocation checks the
persisted journal, expected commit chain, branch, index, and monitor state before
continuing recovery. It automatically performs the same rollback only when the
observed Git state is a known journaled state. External Git activity, a running
or unprovably stopped hook process, or an unknown HEAD or index state pauses and
quarantines the batch for manual inspection.

Claims are released only after the complete transactional batch succeeds. Newly
unblocked dependent tasks may then start against the updated base branch.

## Session Completion And Abort

A session can finish successfully only when:

- Every task is `committed`, `no_op`, or explicitly `canceled` with its
  dependents resolved.
- No claim remains.
- Final repository validation passes.
- The Git-visible working tree and Git index are clean.

Successful completion leaves validated local commits on the original current
branch. Bandmaster never pushes them.

Aborting a session stops orchestration, clears claims after workers are
confirmed stopped, preserves uncommitted working changes, and retains ownership
and audit records for inspection. A new session cannot start until the working
tree is made clean.

## Persistence And Audit

SQLite is configured for safe concurrent local access and stores current
orchestration state. An append-only event table records significant transitions,
including:

- Session creation, pause, resume, completion, and abort.
- Task creation and status changes.
- Claim acquisition, expansion, release, expiration, and quarantine.
- Baseline and submitted path snapshots.
- Batch creation, membership, freezing, repair, and finalization.
- Integrity violations.
- Validation commands and results.
- Commit attempts, hooks, rollback, and resulting SHAs.

Full Codex transcripts are not stored. Structured task intent and worker
handoffs provide the durable context needed for diagnosis and recovery.

## CLI Design Principles

- Prefer a small set of high-level workflow commands.
- Hide SQLite transactions, snapshots, validation orchestration, staging,
  commit ordering, and rollback behind those commands.
- Produce readable output by default.
- Support stable `--json` output and meaningful exit codes for every command
  used by the Codex skill.
- Make every state transition idempotent where practical so the parent can
  safely retry after ambiguous command failures.

Every JSON response includes a schema version, command name, success indicator,
session ID when applicable, and either a typed result or a typed error with a
stable code and retryability indicator. Schema changes follow an explicit
compatibility policy. Exit codes distinguish success, blocked work, invalid
input or state, integrity quarantine, validation failure, and internal failure.

The exact command names beyond `bandmaster init` are an implementation design
decision. The required capabilities are:

- Start, inspect, pause, resume, finish, and abort a session.
- Create and inspect tasks and dependencies.
- Atomically claim, expand, and release exact paths.
- Heartbeat workers and recover expired or quarantined tasks.
- Review and submit task diffs and structured handoffs.
- Inspect integrity violations and audit history.
- Validate and transactionally finalize a completed batch.
- Install or update the project-local Codex skill.

## Deferred Direction

A future isolated or patch-queue mode may produce independent task branches and
real Git integration conflicts. That mode can add a specialized resolver agent
which receives the base, current version, incoming version, task intents,
worker summaries, and validation commands.

The resolver is deliberately excluded from this MVP. With one shared branch,
exclusive same-file claims, batch barriers, and serialized direct commits,
Bandmaster avoids the merge operation that would create ordinary Git merge
conflicts.

## MVP Acceptance Criteria

The MVP is complete when it can demonstrate all of the following in temporary
Git repositories and an actual Codex-driven project:

1. Initialize a project, detect validation commands, and install the generated
   Codex skill, then refuse session start until the detected configuration digest
   is approved.
2. Start one clean session and reject dirty, detached, linked-worktree,
   submodule, nested-repository, and otherwise unsupported states.
3. Persist explicit session, task, claim, lease, and batch state transitions and
   return versioned JSON results with stable error classes.
4. Run multiple workers against disjoint exact-file claims in one working tree.
5. Atomically reject overlapping claim sets and requeue blocked work without
   granting partial claims.
6. Correctly snapshot creation, deletion, rename, symlink, and executable-bit
   changes, and reject unsafe case, Unicode, or symlink path aliases.
7. Detect unclaimed and post-submission Git-visible modifications at workflow
   scans while documenting that transient writes between observations may be
   missed.
8. Quarantine expired claims until Codex worker termination is proven or an
   explicit user recovery confirmation is audited.
9. Freeze persisted batch membership at the barrier and prevent late tasks from
   joining that batch.
10. Validate a frozen combined batch with focused and project checks, and reject
    a validation command that mutates a Git-visible path.
11. Reopen and resubmit the original owning tasks for single-owner and
    multiple-owner repairs without transferring claims.
12. Complete a `no_op` task without creating an empty commit.
13. Create one file-attributed commit per changed task in deterministic order
    with the exact deterministic message.
14. Accept a hook-produced claimed-path change only when the hook includes it in
    the owning task's commit; reject outside-claim, unstaged, and commit-message
    hook mutations.
15. Roll back the complete batch to the pre-batch branch and clean index while
    preserving all Git-visible edits observed at failure when any hook, commit,
    integrity check, or validation step fails.
16. Recover or safely quarantine an interrupted finalization at every persisted
    journal step, including unknown external HEAD or index changes.
17. Release claims and unblock dependent tasks only after transactional batch
    success.
18. Finish with a clean Git-visible repository and complete append-only audit
    history while leaving ignored-path and external side effects explicitly
    outside rollback guarantees.
