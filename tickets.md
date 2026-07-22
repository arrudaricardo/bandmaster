# Tickets: Bandmaster MVP

These tickets build the Bandmaster MVP described in the [Bandmaster MVP PRD](.scratch/bandmaster-mvp/PRD.md).

Work the **frontier**: any ticket whose blockers are all done. Several branches can proceed in parallel after the regular-file claim and submission slice is complete.

## Initialize and approve a Bandmaster project

**What to build:** Deliver a usable Go CLI that initializes a supported repository, generates versioned configuration and the project-local Codex skill, detects validation commands, and gates sessions on explicit approval of the current configuration digest.

**Blocked by:** None — can start immediately.

- [x] The CLI is installable through standard Go distribution and provides human-readable output plus a stable versioned JSON envelope.
- [x] Initialization verifies repository support and generates versioned project configuration and the project-local Codex skill.
- [x] Detected validation commands remain unapproved until the user approves their exact configuration digest.
- [x] Configuration changes, new clones, and missing runtime approval state require fresh approval.
- [x] Unsupported repository layouts are rejected with typed errors and meaningful exit classes.

## Start and inspect a clean session

**What to build:** Let a user start, inspect, pause, resume, and finish a claimless persisted session while Bandmaster enforces repository, configuration, and single-session preconditions.

**Blocked by:** Initialize and approve a Bandmaster project.

- [x] Session start requires valid configuration, current validation approval, a clean attached branch, a clean index, and a supported repository layout.
- [x] The starting branch and commit are recorded, and a second active session in the same repository is rejected.
- [x] Fresh CLI invocations observe the same durable session state and append significant transitions to audit history.
- [x] Legal session transitions succeed and illegal transitions return stable typed errors without changing durable state.
- [x] A claimless session can be paused, resumed, and completed with a clean repository.

## Plan dependencies and assign ready tasks

**What to build:** Let a parent agent create a durable task graph, assign only dependency-ready tasks, and safely replan or cancel claimless work before editing begins.

**Blocked by:** Start and inspect a clean session.

- [x] Tasks persist stable identity, creation order, title, intent, expected outcome, prerequisites, status, Agent identity, and assignment token.
- [x] Only tasks whose prerequisites succeeded in prior batches can become ready and be assigned.
- [x] Assignment freezes planning fields for the Agent attempt, and the first claim permanently freezes core task meaning.
- [x] Replanning after assignment requires proven Agent termination, token revocation, and a return to planned or ready.
- [x] Canceling assigned claimless work requires proven termination and token revocation, and dependents must be canceled or replanned.

## Claim and submit one regular-file task

**What to build:** Let one Agent declare a regular-file write set, atomically claim it, edit it, review its complete baseline-to-current diff, and submit frozen snapshots with a structured handoff.

**Blocked by:** Plan dependencies and assign ready tasks.

- [x] Read-only preflight can declare the complete initial write set and focused validation commands before any edit.
- [x] Initial regular-file claims are acquired atomically only with the owning task's valid assignment token.
- [x] Baseline snapshots represent absence or presence, regular-file type, exact-content hash, and executable bit.
- [x] Every mutating Agent command validates assignment and ownership, and submission rejects changed paths without an owner.
- [x] Submission freezes all claimed path snapshots and records behavior changed, key decisions, validation expectations, and known risks.
- [x] A submission whose claims all match baseline is recorded as a pending no-op rather than creating an empty commit.

## Coordinate concurrent claims and Agent leases

**What to build:** Let multiple Agents progress safely on disjoint paths while overlapping requests block atomically, active Agents renew leases, and abandoned work remains quarantined until safe replacement.

**Blocked by:** Claim and submit one regular-file task.

- [x] Multiple Agent processes can edit disjoint exact-file claims concurrently in one working tree.
- [x] An unavailable initial or expanded write set grants no partial claims and returns a typed blocked outcome.
- [x] Blocked tasks retain no claims and can be requeued after relevant claims are released.
- [x] An unused claim can be released only while its complete current snapshot equals its baseline.
- [x] Every Agent action renews its lease, and an explicit heartbeat supports long implementation periods.
- [x] Lease expiry quarantines ownership without releasing it; replacement requires proven termination or audited user confirmation and receives a new token.

## Support the complete Git-visible path model

**What to build:** Let Agents safely own file creation, deletion, rename, symlink, and executable-bit changes while rejecting path aliases and traversal that could escape exact-file attribution.

**Blocked by:** Claim and submit one regular-file task.

- [x] Claims support existing paths and absent destinations, and a rename requires claims for both source and destination.
- [x] Path snapshots distinguish absence, regular files, symlinks, exact stored-content or target hash, and executable bit.
- [x] Canonicalization rejects invalid segments, worktree escape, directories, Git metadata, nested repositories, and parent-symlink traversal.
- [x] Existing tracked paths must use Git-index spelling, and unsupported file types are rejected.
- [x] Filesystem case-folding and Unicode-normalization aliases are detected for existing paths and safely resolvable absent destinations.
- [x] CLI integration tests cover creation, deletion, rename, symlink, and executable-bit behavior on supported platforms.

## Detect and quarantine repository integrity drift

**What to build:** Detect current unclaimed, post-submission, branch, HEAD, index, and base drift through a healthy long-lived monitor and authoritative scans, then pause and quarantine affected work.

**Blocked by:** Claim and submit one regular-file task.

- [x] One monitor persists its process identity and heartbeat for each active session, and every mutating command requires it to be healthy.
- [x] Restarting an unhealthy monitor requires a successful full scan before the session can resume.
- [x] Advisory filesystem notifications are supplemented by full scans at important transitions and barriers.
- [x] Current changes to unclaimed non-ignored paths, frozen submitted paths, index, HEAD, branch, and base are detected.
- [x] Tracked paths remain covered even when ignore rules match, while ignored untracked paths remain outside ownership and rollback guarantees.
- [x] Every violation pauses and quarantines affected work, records path, observed state, and timestamp, and requires explicit audited recovery.

## Freeze an attributable batch at the barrier

**What to build:** Turn submitted independent Tasks into a persisted frozen Batch whose ordered Tasks, order, base, path ownership, and submitted snapshots cannot change before validation.

**Blocked by:** Coordinate concurrent claims and Agent leases; Support the complete Git-visible path model; Detect and quarantine repository integrity drift.

- [x] A task joins the current collecting batch only after its initial claim succeeds.
- [x] The barrier can run only after every active Agent has submitted, blocked, failed, or been deliberately stopped.
- [x] Freezing closes and persists Task order; blocked tasks and tasks becoming ready later wait for another batch.
- [x] Every Batch Task is submitted and every changed non-ignored path has exactly one submitted owner.
- [x] Stale submitted snapshots, unclaimed changes, index or branch drift, active Agents, and quarantine prevent freezing.
- [x] Tasks in one batch cannot depend on one another because prerequisites must succeed in an earlier batch.

## Run official validation on a frozen batch

**What to build:** Run Agent-focused and approved repository checks sequentially against the combined frozen batch and record reproducible outcomes without allowing validation to alter the work it authorizes.

**Blocked by:** Freeze an attributable batch at the barrier.

- [x] Bandmaster, rather than Agents, executes focused and repository-wide validation after the barrier.
- [x] Each command has a stable name, argument vector or explicit shell script, canonical working directory, timeout, and declared environment overrides.
- [x] Execution records resolved command details, status, duration, and bounded standard output and error.
- [x] Commands use the repository root by default and inherit a documented minimal environment plus overrides.
- [x] A non-zero exit without integrity drift produces repair-pending work and creates no commit.
- [x] Pre-command and post-command scans treat any Git-visible mutation as an integrity violation and quarantine the batch.

## Repair failures without changing ownership

**What to build:** Let a parent replace failed Agents or reopen one or more original task owners after validation failure while retaining baselines, claims, partial edits, batch base, and attribution.

**Blocked by:** Run official validation on a frozen batch; Coordinate concurrent claims and Agent leases.

- [x] Ordinary Agent failure retains claims and partial edits and leaves an initially collecting batch open.
- [x] Repair after a frozen-Batch failure invalidates stale submissions and keeps the Batch's original ordered Tasks and base closed to unrelated work.
- [x] Replacement requires proven prior-Agent termination, records diagnosis and intended repair, and creates a new assignment token.
- [x] Replacement Agents inherit existing claims and baselines and may atomically expand only into currently unowned paths.
- [x] A repair requiring paths owned by multiple tasks reopens each original owner rather than transferring claims.
- [x] The batch cannot return to the barrier until every reopened task submits again.

## Commit a successful batch by task

**What to build:** Create one deterministic local commit per changed task after successful validation, skip no-op tasks, rerun final validation, release claims, and record the resulting task SHAs.

**Blocked by:** Run official validation on a frozen batch.

- [x] Changed tasks commit in task creation order, while no-change submissions create no commit.
- [x] Each commit contains all and only the Git-visible changes under that task's claims.
- [x] Commit subject and body are deterministic and derived from task identity, title, intent, and Bandmaster metadata.
- [x] Normal successful Git commit hooks run during each task commit.
- [x] After all task commits, the Git-visible tree and index are clean and required final validation runs against the committed batch.
- [x] Success records every SHA, marks changed tasks committed and unchanged tasks no-op, releases claims, and never pushes.

## Enforce hook integrity and roll back failed finalization

**What to build:** Preserve task attribution through Git hooks and restore the pre-batch branch and clean index with all observed Git-visible edits when hook, commit, integrity, or final validation fails.

**Blocked by:** Commit a successful batch by task; Detect and quarantine repository integrity drift.

- [x] Each provisional commit is verified for expected parent, one-commit advance, exact path set, complete content, cleanliness, message, branch, and index state.
- [x] A hook-produced change to the current task's claim is accepted only when included in that task's commit and recorded in audit history.
- [x] Hook changes outside current claims, unstaged or dirty claimed changes, unexpected Git state, and message rewrites fail finalization.
- [x] Failure state is captured before restoring the original branch to the pre-batch commit and restoring a clean index.
- [x] Every Git-visible edit observed at failure is recreated as uncommitted work, including permitted hook changes.
- [x] Unambiguous ordinary failures enter repair pending; integrity violations and ambiguous rollback enter quarantine and pause the session.

## Recover interrupted finalization safely

**What to build:** Let a fresh CLI invocation recover or safely quarantine a process crash at any persisted finalization step without accepting unknown Git state.

**Blocked by:** Enforce hook integrity and roll back failed finalization.

- [x] Expected branch and pre-batch commit, clean index state, frozen manifest, recoverable content, ordered commit plan, and journal step are durably recorded before Git mutation.
- [x] Crash tests terminate finalization at every persisted journal step and resume through a fresh CLI process.
- [x] A known journaled Git state continues or rolls back to the same all-or-nothing outcome as an uninterrupted failure.
- [x] Unknown HEAD, index, branch, external Git activity, monitor state, or unprovably stopped hook process pauses and quarantines the batch.
- [x] Provisional commits may remain as unreachable objects after rollback but never remain as a partially accepted batch.

## Advance dependent batches and complete a session

**What to build:** Unblock dependent tasks only after committed or no-op prerequisites succeed in earlier batches, then complete a fully processed session cleanly on its original branch.

**Blocked by:** Plan dependencies and assign ready tasks; Recover interrupted finalization safely.

- [x] Claims release and dependent tasks become ready only after complete transactional batch success.
- [x] Later collecting batches use the updated branch base and cannot admit dependencies from their own batch.
- [x] A successful no-op satisfies prerequisites without creating an empty commit.
- [x] Session completion requires every task to be committed, no-op, or validly canceled and every claim to be released.
- [x] Final repository validation passes and the working tree and index are clean before completion.
- [x] Successful completion leaves validated local commits on the original branch and performs no remote Git operation.

## Abort active work without losing edits

**What to build:** Let a user stop orchestration while preserving uncommitted Git-visible work and durable ownership and audit records for inspection.

**Blocked by:** Coordinate concurrent claims and Agent leases; Detect and quarantine repository integrity drift.

- [x] Abort enters an explicit aborting state and stops all Agents before claims can be cleared.
- [x] Agent termination is proven through parent-held handles or explicit audited user confirmation; ambiguous Agents remain quarantined.
- [x] Safe claims are cleared only after termination while all uncommitted Git-visible changes remain in the working tree.
- [x] Task ownership, structured handoffs, recovery decisions, and audit events remain inspectable after abort.
- [x] A new session is rejected until the preserved working tree is made clean.

## Drive Bandmaster through the generated Codex skill

**What to build:** Use the generated project-local skill in an actual Codex-driven project to identify parallel work, orchestrate Agents, requeue contention, repair failures, finalize batches, and discover interrupted sessions.

**Blocked by:** Repair failures without changing ownership; Advance dependent batches and complete a session; Abort active work without losing edits.

- [x] The generated skill invokes Bandmaster only when at least two tasks are independently implementable and testable.
- [x] The parent remains the sole orchestrator, starts all currently unblocked Agents, and never delegates agent-spawning authority.
- [x] Agents use stable JSON commands and tokens, claim before writing, heartbeat, avoid Git mutation, review their diffs, submit handoffs, and stop editing.
- [x] The parent requeues blocked Agents, waits for barriers, diagnoses validation failures, and assigns repairs to original owners.
- [x] Interrupted sessions are reported with an offer to resume instead of silently starting new Agents.
- [x] Lost Agent handles preserve quarantine and require explicit user confirmation before replacement.

## Prove MVP acceptance across supported platforms

**What to build:** Demonstrate the complete release contract on macOS through the canonical CLI integration harness and an actual Codex-driven project.

**Blocked by:** Support the complete Git-visible path model; Recover interrupted finalization safely; Advance dependent batches and complete a session; Drive Bandmaster through the generated Codex skill.

- [ ] All eighteen acceptance scenarios from the source PRD pass in temporary Git repositories and the Codex-driven project.
- [x] Platform-sensitive path aliases, Unicode behavior, executable bits, notifications, process liveness, and hook behavior run on both macOS and Linux.
- [x] Release binaries and installation through standard Go distribution produce consistent CLI behavior.
- [x] Stable JSON compatibility, typed errors, retryability, and distinct exit classes are verified across the complete workflow.
- [ ] Tests assert public CLI and Git-visible behavior rather than private SQLite layouts, transition helpers, or Git subprocess choreography.
- [ ] Any lower test seam is limited to deterministic clock, monitor-event, process, Git, hook, or crash fault injection and still asserts public state outcomes.
