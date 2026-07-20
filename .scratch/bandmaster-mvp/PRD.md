# Bandmaster MVP

Status: `ready-for-agent`

## Problem Statement

A parent Codex agent can divide coding work among subagents, but parallel agents sharing one Git working directory have no durable coordination mechanism. They can overwrite one another's files, mutate Git state unexpectedly, leave ambiguous partial work after failure, and produce changes that cannot be attributed confidently to one task. Conversation context alone is not a reliable source of orchestration state, and ordinary same-user filesystem controls cannot isolate one Codex subagent from another.

The user needs a local tool that makes cooperative parallel coding safe enough to inspect, resume, validate, and commit. It must prevent planned same-file editing, detect many unsafe shared-workspace changes, preserve work through failures, and produce deterministic task-attributed commits without claiming hard isolation that the operating system cannot provide.

## Solution

Bandmaster will be a local Go CLI that orchestrates parallel Codex workers in one supported Git working tree. A parent agent will plan independent tasks and dependencies; workers will atomically claim exact file paths before editing, maintain leases, and submit frozen path snapshots with structured handoffs. Bandmaster will persist sessions, tasks, batches, claims, validation, recovery journals, and audit events in local SQLite state under Git metadata.

Bandmaster will monitor Git-visible repository integrity, stop or quarantine workflows when ownership or Git state becomes ambiguous, validate completed work at a frozen batch barrier, and create one deterministic commit per changed task. Finalization will be recoverably transactional: either the complete validated batch remains committed, or the branch and index return to their pre-batch state while all observed Git-visible edits are restored as uncommitted work. A generated project-local Codex skill will teach parent and worker agents to use the protocol and stable JSON CLI contract.

## User Stories

1. As a repository user, I want to install Bandmaster through standard Go distribution channels, so that I can use it without a bespoke runtime.
2. As a repository user, I want Bandmaster to support macOS and Linux, so that I can use it in common local development environments.
3. As a repository user, I want to initialize Bandmaster in a standard Git repository or monorepo, so that orchestration can follow project-local conventions.
4. As a repository user, I want initialization to reject unsupported repository layouts, so that Bandmaster does not make unsafe guarantees.
5. As a repository user, I want initialization to create versioned project configuration, so that configuration changes are reviewable and evolvable.
6. As a repository user, I want initialization to detect likely validation commands, so that setup starts from useful project-specific suggestions.
7. As a repository user, I want detected validation commands to remain unapproved until I approve their exact configuration digest, so that agents cannot silently choose the checks that authorize commits.
8. As a repository user, I want any validation configuration change to invalidate prior approval, so that approval always applies to the commands that will actually run.
9. As a repository user, I want a new clone or deleted runtime state to require fresh approval, so that trust is not inferred from a tracked configuration file alone.
10. As a repository user, I want initialization to install or update a project-local Codex skill, so that Codex knows how to use Bandmaster correctly.
11. As a repository user, I want generated skill updates to overwrite rather than merge generated content, so that generation remains deterministic.
12. As a parent Codex agent, I want the skill to invoke orchestration only when at least two tasks are independently implementable and testable, so that parallelism is used only when beneficial.
13. As a parent Codex agent, I want to remain the sole orchestrator, so that worker agents cannot recursively create uncontrolled work.
14. As a parent Codex agent, I want to express task prerequisites as a simple dependency graph, so that work starts only after required earlier outcomes exist.
15. As a parent Codex agent, I want to start every currently unblocked worker without an artificial Bandmaster concurrency cap, so that independent work can proceed in parallel.
16. As a parent Codex agent, I want blocked workers to exit without partial claims, so that I can safely requeue them later.
17. As a parent Codex agent, I want interrupted sessions to be reported and offered for explicit resumption, so that abandoned state is not mistaken for a new session.
18. As a parent Codex agent, I want workers from a lost parent session to remain quarantined unless termination is proven, so that replacements never race unknown workers.
19. As a repository user, I want a session to start only on a clean, attached branch with a clean index, so that every later change has an unambiguous baseline.
20. As a repository user, I want only one active Bandmaster session per repository, so that two orchestrators cannot claim the same workspace.
21. As a repository user, I want Bandmaster to record the starting branch and commit, so that branch or base drift can be detected.
22. As a repository user, I want external changes to branch, HEAD, index, or unclaimed working-tree paths to pause the session, so that Bandmaster never adapts silently to an unknown base.
23. As a parent Codex agent, I want to create tasks with stable IDs, titles, behavioral intent, expected outcomes, and prerequisites, so that work has durable meaning outside a transcript.
24. As a parent Codex agent, I want planning fields editable only before work materially starts, so that active ownership cannot be redefined retroactively.
25. As a parent Codex agent, I want assignment to freeze planning for that worker attempt, so that the worker receives a stable contract.
26. As a repository user, I want title, intent, dependencies, and expected outcome permanently immutable after the first claim, so that committed attribution remains trustworthy.
27. As a parent Codex agent, I want a task assignable only after all prerequisites succeeded in prior batches, so that workers never depend on uncommitted peer edits in the same batch.
28. As a parent Codex agent, I want prerequisite tasks with either committed changes or successful no-op outcomes to unblock dependents, so that unnecessary empty commits do not block progress.
29. As a parent Codex agent, I want cancelation limited to tasks without claims or batch membership, so that owned changes cannot disappear through ordinary planning actions.
30. As a parent Codex agent, I want canceling a task with dependents to require cancellation or replanning of those dependents, so that the dependency graph remains valid.
31. As a worker agent, I want to inspect the repository read-only before declaring my write set, so that I can plan without violating ownership.
32. As a worker agent, I want to declare my complete initial write set and focused validation commands before editing, so that ownership and expected checks are explicit.
33. As a worker agent, I want all requested paths claimed atomically, so that I never begin with only part of the write set I need.
34. As a worker agent, I want a failed claim request to grant no paths, so that I cannot deadlock while holding a partial set.
35. As a worker agent, I want to expand my write set through an atomic non-waiting request, so that newly discovered work remains safe without holding-and-waiting.
36. As a worker agent, I want to release an unused claim when its path still equals the recorded baseline, so that other tasks can proceed without losing changes.
37. As a worker agent, I want claims to cover existing files and absent destinations, so that creation, deletion, and rename workflows are attributable.
38. As a worker agent, I want renames to require both source and destination claims, so that the complete operation has one owner.
39. As a repository user, I want claims to use exact canonical repository-relative paths, so that ownership cannot be bypassed through alternate spellings.
40. As a repository user, I want paths through parent symlinks, directories, nested repositories, Git metadata, and outside-worktree locations rejected, so that claims cannot escape their intended scope.
41. As a repository user, I want case-folding and Unicode-normalization aliases rejected, so that two claims cannot refer to the same filesystem object.
42. As a repository user, I want existing tracked paths to use their Git-index spelling, so that path identity agrees with Git.
43. As a repository user, I want path snapshots to capture absence, file type, a hash of exact file bytes or symlink target, and executable bit, so that all Git-visible changes are represented.
44. As a repository user, I want unsupported file types rejected, so that snapshots never imply unsupported fidelity.
45. As a worker agent, I want to read paths claimed by peers, so that normal repository understanding remains possible despite accepted visibility of in-progress work.
46. As a worker agent, I want an opaque assignment token for every mutating command, so that accidental cross-task actions are rejected.
47. As a repository user, I want assignment tokens treated as coordination rather than a security boundary, so that same-user limitations are represented honestly.
48. As a worker agent, I want every Bandmaster action to renew my lease, so that active work remains distinguishable from abandoned work.
49. As a worker agent, I want an explicit heartbeat command for long implementation periods, so that I can retain ownership without unrelated CLI actions.
50. As a repository user, I want lease expiration to quarantine rather than release claims, so that a slow worker cannot overlap with a replacement.
51. As a parent Codex agent, I want to prove worker termination through my worker handle before recovery, so that reassignment is safe.
52. As a repository user, I want explicit user confirmation required when the worker handle is unavailable, so that ambiguous termination is resolved consciously and audibly.
53. As a replacement worker, I want to inherit retained claims, baselines, and current edits under a new token, so that repair continues without losing attribution.
54. As a repository user, I want one long-lived integrity monitor per active session, so that unsafe shared-workspace changes are detected between workflow commands when possible.
55. As a repository user, I want mutating commands to verify monitor health, so that orchestration pauses if monitoring silently stops.
56. As a repository user, I want a restarted monitor to complete a full scan before resumption, so that current visible state is authoritative.
57. As a repository user, I want filesystem notifications supplemented by transition and barrier scans, so that monitoring balances timely detection with authoritative checks.
58. As a repository user, I want unclaimed non-ignored changes detected, so that all committed paths have an owner.
59. As a repository user, I want post-submission changes detected, so that frozen work cannot be modified unnoticed.
60. As a repository user, I want unexpected index, HEAD, branch, and base changes detected, so that only Bandmaster controls Git state during a session.
61. As a repository user, I want integrity violations to quarantine affected work and pause the session, so that ambiguous state cannot advance automatically.
62. As a repository user, I want explicit audited recovery after every integrity violation, so that restoration alone does not erase evidence of unsafe behavior.
63. As a repository user, I want tracked paths monitored even when ignore rules match them, so that Git-visible ownership remains complete.
64. As a repository user, I want ignored untracked paths excluded from ownership and rollback guarantees, so that cache and generated-output limitations are explicit.
65. As a worker agent, I want to review the complete baseline-to-current diff of every claimed path before submission, so that my handoff describes all owned work.
66. As a worker agent, I want submission to reject changes outside my claims, so that task attribution remains exact.
67. As a worker agent, I want to submit behavior changed, key decisions, validation expectations, and known risks in structured form, so that recovery does not depend on a transcript.
68. As a worker agent, I want submission to freeze every claimed path snapshot, so that the batch barrier receives stable work.
69. As a worker agent, I want to stop editing after submission, so that finalization sees the work I reviewed.
70. As a worker agent, I want an unchanged submission to become a successful no-op only after its batch succeeds, so that no empty commit is needed and dependent tasks still wait for validation.
71. As a parent Codex agent, I want a first-class persisted collecting batch, so that concurrently completed independent tasks share one validation unit.
72. As a parent Codex agent, I want a task to join the collecting batch only when its initial claim succeeds, so that blocked work cannot complicate frozen membership.
73. As a parent Codex agent, I want the barrier to freeze batch membership and order, so that late tasks wait for a later base.
74. As a parent Codex agent, I want to reach the barrier only after every active worker has stopped editing, so that validation is not racing implementation.
75. As a repository user, I want the barrier to verify that every batch member submitted and every changed path has exactly one owner, so that validation starts from an attributable manifest.
76. As a repository user, I want the barrier to verify submitted snapshots, unclaimed paths, index state, branch state, base state, and quarantine state, so that no unsafe batch proceeds.
77. As a repository user, I want official validation to run against the combined frozen batch, so that checks reflect the shared working-tree result.
78. As a worker agent, I want my focused validation commands included in official validation, so that task-specific expectations are checked after editors stop.
79. As a repository user, I want approved repository-wide validation commands included, so that the complete project contract is checked.
80. As a repository user, I want official validation commands run sequentially with recorded command, directory, environment, timeout, exit status, duration, and bounded output, so that results are reproducible and inspectable.
81. As a repository user, I want validation commands represented as argument vectors unless a shell script is explicitly requested, so that command interpretation is controlled.
82. As a repository user, I want validation commands to inherit a documented minimal environment plus declared overrides, so that hidden ambient configuration is reduced.
83. As a repository user, I want pre-command and post-command scans to reject Git-visible mutations, so that tests cannot alter the work they authorize.
84. As a parent Codex agent, I want ordinary validation failures to preserve claims and edits without committing, so that I can diagnose and repair the same owned work.
85. As a parent Codex agent, I want validation mutations treated as integrity violations rather than ordinary failures, so that unsafe checks cause quarantine.
86. As a parent Codex agent, I want to select one or more original owning tasks for repair, so that changes remain attributed to their established owners.
87. As a parent Codex agent, I want repair to invalidate stale submissions while preserving original baselines, claims, and partial edits, so that attribution remains continuous.
88. As a parent Codex agent, I want repaired frozen batches to keep membership and base fixed, so that repair cannot smuggle unrelated work into validated scope.
89. As a repair worker, I want to edit only retained claims and atomically acquire currently unowned additional paths, so that repair follows the normal ownership protocol.
90. As a parent Codex agent, I want multi-owner repairs to reopen each owning task, so that claims are never silently transferred.
91. As a repository user, I want successful validation to create one provisional commit per changed task in task creation order, so that history is deterministic and attributable.
92. As a repository user, I want each commit to contain all and only its task's claimed changes, so that file attribution is exact.
93. As a repository user, I want deterministic commit subjects and bodies derived from task identity and intent, so that history is reviewable and reproducible.
94. As a repository user, I want normal Git commit hooks to run, so that repository commit policy is preserved.
95. As a repository user, I want finalization intent and recoverable content durably journaled before Git mutation, so that crashes can be recovered safely.
96. As a repository user, I want every provisional commit verified for parent, path set, content completeness, cleanliness, message, branch, and index integrity, so that hooks or external processes cannot silently alter the plan.
97. As a repository user, I want hook changes to an owning task's claimed paths accepted only when included in that task's commit, so that hook-produced content remains attributable.
98. As a repository user, I want hook changes outside the current task's claims, unstaged claimed-path changes, and message rewrites rejected, so that hooks cannot violate the deterministic transaction.
99. As a repository user, I want final repository validation rerun after all provisional commits, so that the committed batch is checked in its final state.
100. As a repository user, I want the complete batch either retained after final validation or rolled back to the pre-batch branch and clean index with observed edits restored, so that partial finalization never becomes the accepted outcome.
101. As a repository user, I want failed hook, commit, integrity, or final validation state captured before rollback, so that permitted hook edits and other visible work are not lost.
102. As a repository user, I want unambiguous ordinary failures to enter repair and ambiguous or integrity failures to enter quarantine, so that automation proceeds only when safe.
103. As a repository user, I want crash recovery to continue or roll back only from known journaled Git states, so that unknown HEAD, index, hook, or external activity pauses for inspection.
104. As a parent Codex agent, I want claims released and dependents unblocked only after complete transactional batch success, so that no task starts from an unvalidated base.
105. As a repository user, I want successful completion to require terminal tasks, no claims, passing final validation, and a clean working tree and index, so that the session leaves a trustworthy repository.
106. As a repository user, I want successful work left as local commits on the original branch without an automatic push, so that remote publication stays under my control.
107. As a repository user, I want abort to stop workers, confirm termination, clear safe claims, and preserve uncommitted changes, so that stopping orchestration does not discard work.
108. As a repository user, I want aborted ownership and audit records retained, so that I can inspect what happened before cleaning the repository.
109. As a repository user, I want current state persisted in SQLite with safe local concurrent access, so that separate Bandmaster processes share durable truth.
110. As a repository user, I want significant transitions recorded in an append-only audit history, so that sessions, tasks, claims, batches, validation, commits, hooks, violations, and recovery remain explainable.
111. As a repository user, I want structured handoffs retained without full Codex transcripts, so that recovery has useful context without storing entire conversations.
112. As a Codex agent, I want high-level workflow commands to hide transactions, snapshots, staging, commit order, and rollback, so that I use a small safe interface.
113. As a Codex agent, I want stable versioned JSON responses with typed results or typed errors, so that orchestration does not parse human prose.
114. As a Codex agent, I want typed errors to include stable codes and retryability, so that I can distinguish blocked work from invalid state, quarantine, validation failure, and internal failure.
115. As a human user, I want readable default command output, so that I can inspect and operate Bandmaster directly.
116. As a parent Codex agent, I want practical idempotency for state transitions, so that I can retry after ambiguous command delivery without duplicating effects.

## Implementation Decisions

**Runtime and supported environment**

- Implement Bandmaster as a Go CLI distributed through `go install` and release binaries.
- Support macOS and Linux, standard Git repositories, and monorepos that use one ordinary working tree.
- Require a clean, attached current branch and clean index at session start.
- Require valid project configuration and explicit approval of its current validation digest before session start.
- Support one active session per repository and create local commits only on the branch current at session start.
- Reject linked worktrees, nested repositories, submodules, sparse checkouts, and other explicitly unsupported layouts.

**Actors and trust model**

- The parent Codex agent is the sole orchestrator and the only actor allowed to spawn workers.
- Each worker owns one task, performs read-only preflight, claims before writing, maintains its lease, reviews its complete diff, submits a structured handoff, and stops editing after submission.
- Only Bandmaster may mutate Git state during an active session. Workers may run ordinary repository commands but may not run Git commands that mutate the index, HEAD, branches, or working tree.
- Assignment tokens prevent accidental cross-task commands but are not treated as a security boundary because all agents run as the same Unix user.
- The generated skill uses stable JSON command output and starts orchestration only for at least two independent, testable tasks.

**Project initialization and configuration approval**

- Initialization verifies repository support, writes versioned project configuration, detects validation suggestions, and installs or overwrites the generated project-local Codex skill.
- Detected validation is unapproved until the user approves the exact configuration digest.
- Approval is stored in local runtime state rather than in the hashed configuration, avoiding a self-referential digest.
- Any validation configuration change, new clone, or loss of runtime state requires fresh approval.

**Persistence and transition ownership**

- Store runtime state beneath the repository's common Git metadata directory in SQLite configured for safe concurrent local access.
- Persist sessions, tasks, dependencies, batches, claims, leases, baseline and submitted snapshots, worker handoffs, validation attempts, commit results, recovery journals, and append-only audit events.
- Route every state change through one central transition module. CLI handlers do not directly update SQLite, Git, claims, or journals.
- Make SQLite transitions atomic. Coordinate filesystem and Git effects through persisted intent and recovery records when they cannot share the database transaction.
- Make state transitions idempotent where practical.

**Session state model**

- A session is exactly one of `active`, `paused`, `finalizing`, `completed`, `aborting`, or `aborted`.
- Integrity violations, unhealthy monitoring, ambiguous Git state, and unprovable worker termination pause the session.
- Freezing a batch moves an active session into finalization. A successful intermediate batch returns it to active; a successful final batch may complete it.
- An ordinary unambiguous validation or finalization failure returns the session to active with repair pending.
- Integrity and ambiguous recovery failures quarantine the batch and pause the session.
- Abort proceeds through an explicit aborting state until worker termination and claim cleanup are resolved.

**Task state model**

- A task is exactly one of `planned`, `ready`, `assigned`, `editing`, `blocked`, `submitted`, `repair_pending`, `quarantined`, `committed`, `no_op`, or `canceled`.
- The normal path is planning, readiness, assignment, editing, submission, and commitment.
- Failed initial claim acquisition moves an assigned task to blocked without retaining claims; requeueing returns it to ready.
- Worker failure with edits and ordinary validation failure retain ownership and move the task to repair pending.
- Reassignment after proven termination creates a new token and reuses retained claims and baselines.
- No-change submissions remain submitted until batch success, then become no-op.
- Cancellation is allowed only before claims or batch membership and cannot discard owned edits.
- Canceling an assigned task requires proving its worker stopped and revoking its assignment token before changing task state.
- Replanning after assignment requires proving the worker stopped, revoking its token, and returning the task to planned or ready; core planning fields become permanently immutable after the first claim.

**Task dependencies and batch model**

- Record stable task identity, creation order, title, intent, expected outcome, prerequisites, status, batch, worker, token, lease, claims, focused validation, handoff, and resulting commit.
- Keep planning mutable only before material work starts. Acquiring the first claim permanently freezes core task meaning.
- A prerequisite is satisfied only by a committed or no-op task from a successful prior batch.
- Persist each batch as a first-class record with stable identity, base branch and SHA, frozen membership order, status, integrity state, manifests, validation attempts, commit plan, journal position, and rollback result.
- A batch is exactly one of `collecting`, `frozen`, `validating`, `repair_pending`, `repairing`, `finalizing`, `final_validating`, `committed`, or `quarantined`.
- A task joins the collecting batch only when its initial claim succeeds. Freezing closes membership permanently.
- Tasks in one batch cannot depend on each other.

**Claims and path snapshots**

- Claim exact canonical repository-relative paths, including absent paths needed for creation and both endpoints of a rename.
- Validate UTF-8, slash separators, path segments, worktree containment, Git-index spelling, parent symlinks, nested repositories, directories, Git metadata, filesystem case folding, and Unicode normalization before granting a claim.
- Represent Git-visible state as a complete path snapshot containing absence or presence, regular-file or symlink type, a hash of exact stored bytes or symlink target, and executable bit.
- Reject unsupported file types and ambiguous absent destinations.
- Acquire the complete initial write set in one serialized SQLite transaction after capturing baselines and rechecking the assignment token, prerequisites, collecting batch, and base SHA.
- Grant no claims when any requested path is unavailable.
- Use the same all-or-nothing, non-waiting rule for claim expansion.
- Permit early release only when the current complete snapshot still equals the baseline.
- Claims protect writes, not reads; workers may observe peers' in-progress content.

**Leases and worker recovery**

- Renew the worker lease on every Bandmaster action and expose an explicit heartbeat command.
- Lease expiration quarantines the task and claims; it never proves termination or makes paths available.
- Require successful termination through the parent-held Codex worker handle before replacement.
- If the handle is unavailable, require explicit user confirmation and append it to the audit history before creating a replacement token.
- Never assume a worker stopped merely because a parent session ended or a handle was lost.

**Integrity monitoring**

- Run one long-lived integrity monitor per active session and persist its process identity and heartbeat.
- Require a healthy monitor for every mutating command. Restart requires an authoritative full scan before resume.
- Combine advisory filesystem notifications with full scans before and after important transitions and at batch barriers.
- Detect current changes to unclaimed non-ignored paths, frozen submitted paths, index, HEAD, current branch, and base branch.
- Cover every tracked path and every untracked non-ignored path. Keep ignored untracked paths outside ownership, validation-mutation, attribution, and rollback guarantees.
- Quarantine affected tasks or batches and pause on every integrity violation, even when the visible Git state can be restored.
- Record the affected path, observed state, and timestamp for every integrity violation in the append-only audit history.
- Require an explicit audited recovery to leave quarantine.
- Do not claim reliable process attribution for writes to paths owned by active workers or complete detection of changes made and restored between observations.

**Submission and barrier**

- Provide a high-level submission command that compares every claim's baseline to its current snapshot, verifies ownership, stores a structured handoff, and freezes all claimed path snapshots.
- Treat any later submitted-path change as an integrity violation, except controlled hook behavior during the owning task's finalization.
- Record a no-change flag when every claimed path remains at baseline; do not create an empty commit.
- Freeze a collecting or repairing batch only after all active workers have submitted, blocked, failed, or been deliberately stopped.
- At the barrier, require every member to be submitted, every changed path to have exactly one submitted owner, every snapshot to remain frozen, every unclaimed path to remain unchanged, Git state to remain controlled, and no relevant quarantine to exist.

**Validation and repair**

- Bandmaster executes official validation after the barrier, never workers.
- Run worker-declared focused commands and approved repository-wide commands sequentially against the combined frozen batch.
- Model each command with a stable name, argument vector or explicitly requested shell script, canonical working directory, timeout, and declared environment overrides.
- Record resolved execution details, status, duration, and bounded standard output and error.
- Use the repository root by default and inherit a documented minimal environment plus overrides.
- Scan before and after every validation command. Any Git-visible mutation is an integrity violation regardless of exit status.
- Preserve claims and working changes after an ordinary failed command, reopen selected owning tasks, and rerun the barrier after repair.
- Preserve original ownership, baselines, batch base, and frozen membership throughout repair.
- Leave an initially collecting batch open after an ordinary worker failure. Move a previously frozen repair-pending batch to repairing with its existing membership closed.
- Repair multiple owners by reopening each owning task rather than transferring claims.

**Transactional finalization**

- After pre-commit validation, create one provisional commit per changed submitted task in creation order. Skip no-change submissions.
- Derive an exact deterministic subject and body from task title, task ID, intent, and Bandmaster metadata.
- Stage all and only the owning task's changes and invoke normal Git commit behavior with repository hooks enabled.
- Before Git mutation, durably record the expected branch and pre-batch SHA, clean pre-finalization index, frozen manifest, recoverable content objects, ordered commit plan, and first journal step.
- After each commit, verify the exact parent, one-commit advance, claimed path set, complete task content, clean claimed paths, deterministic message, branch, index, unclaimed paths, and other tasks' paths.
- During hooks, permit only expected Git metadata and the current task's claimed paths.
- Accept a hook-produced claimed-path change only when the resulting commit includes it, then update the task snapshot and audit the hook diff.
- Reject hook changes outside current claims, claimed changes left dirty or unstaged, and altered commit messages.
- Permit hooks to observe other submitted tasks' uncommitted changes; individual provisional commits need not pass validation independently because the combined batch is the validation unit.
- After all provisional commits, require a clean Git-visible tree and index and rerun required final validation against the committed batch.
- On success, mark the batch committed, record SHAs, transition changed tasks to committed and no-change tasks to no-op, release claims, and unblock dependents.
- On failure, first capture the observed Git-visible state, reset the original branch to the pre-batch SHA, restore a clean index, and recreate captured edits as uncommitted working-tree changes.
- Treat transactionality as recoverable all-or-nothing behavior rather than atomic visibility; provisional commits can be visible temporarily and may remain as unreachable objects after rollback.
- Enter repair pending after an unambiguous ordinary failure and quarantine after an integrity violation or ambiguous rollback.
- On restart, inspect the journal, expected commit chain, branch, index, monitor, and possible hook process. Continue or roll back automatically only from known journaled states.

**Completion, abort, and audit**

- Complete only when all tasks are committed, no-op, or validly canceled; no claims remain; final validation passes; and working tree and index are clean.
- Leave successful local commits on the original branch and never push automatically.
- Abort by stopping workers, proving termination, clearing safe claims, preserving uncommitted Git-visible changes, and retaining state for inspection.
- Prevent a new session after abort until the user cleans the working tree.
- Append audit events for session transitions, task transitions, claims, leases, snapshots, batches, integrity violations, validation, commits, hooks, rollback, and resulting SHAs.
- Retain structured task intent and worker handoffs rather than full Codex transcripts.

**CLI contract**

- Prefer a small set of high-level commands for project initialization, session lifecycle, planning, assignment, claims, heartbeats, recovery, submission, inspection, validation, finalization, and skill installation.
- Keep exact command names beyond `bandmaster init` as an implementation design decision.
- Produce human-readable output by default and stable JSON for agents.
- Include schema version, command, success indicator, applicable session ID, and either a typed result or typed error in every JSON response.
- Give typed errors stable codes and a retryability indicator.
- Use distinct exit classes for success, blocked work, invalid input or state, integrity quarantine, validation failure, and internal failure.
- Define an explicit JSON schema compatibility policy.

## Testing Decisions

- Use one dominant behavioral seam: execute the installed `bandmaster` CLI as an external process against temporary Git repositories.
- A good test asserts externally observable behavior rather than internal implementation. Observe process exit status, versioned JSON, human-readable output where relevant, Git-visible files and modes, branch and index state, generated configuration and skill artifacts, local commits, persisted resumability across fresh CLI invocations, and documented audit/inspection output.
- Do not couple behavioral tests to SQLite table layouts, internal transition functions, monitor implementation, SQL statement order, Git subprocess choreography, or private Go types.
- Exercise initialization through the CLI seam, including supported repository setup, validation detection, generated skill installation, configuration validation, configuration digest approval, invalidation after configuration changes, and rejection of invalid configuration, dirty, detached, linked-worktree, submodule, nested-repository, sparse-checkout, and unsupported states.
- Exercise the session and task state machines through high-level commands, including legal progress, illegal transition rejection, dependencies, assigned-worker cancellation and replanning safeguards, no-op completion, pause, resume, abort, and successful completion.
- Exercise claims through real filesystem and Git-visible path behavior, including all-or-nothing contention, expansion, baseline-safe release, creation, deletion, rename, symlink, executable bit, case aliases, Unicode aliases, parent symlinks, absent destinations, and unsupported file types.
- Exercise multiple worker processes against disjoint and overlapping claims in one working tree to prove cooperative concurrency, blocking, requeueing, token checks, and lease renewal.
- Exercise lease expiry and quarantine through process-level time control where reliable, proving that expiry never releases ownership and that reassignment requires termination evidence or recorded user confirmation.
- Exercise integrity behavior by changing unclaimed paths, submitted paths, index, HEAD, branch, and base between commands and workflow barriers, then assert quarantine, pause, the audited path, observed state, timestamp, and explicit recovery requirements.
- Treat current-state scans as the testable authority. Do not write a flaky test claiming guaranteed detection of a transient write that is restored between monitor observations.
- Exercise batch persistence and frozen membership by running independent tasks, blocking late tasks, restarting the CLI between transitions, and verifying that same-batch dependencies are impossible.
- Exercise validation with focused and repository-wide commands, deterministic timeouts, bounded output, environment overrides, working directories, non-zero exits, and commands that mutate tracked or untracked non-ignored paths.
- Exercise repair through the original owners for single-owner and multiple-owner failures, proving retained claims, unchanged baselines, replacement tokens, and resubmission before a new barrier. Assert that ordinary worker failure leaves an initially collecting batch open while repair of a previously frozen batch keeps membership closed.
- Exercise task-attributed finalization against real Git commits and hooks, asserting deterministic order and messages, exact path sets, no empty commits, accepted in-claim staged hook edits, and rejection of outside-claim, unstaged, dirty, branch, index, and message mutations.
- Exercise final validation failure and commit or hook failure by asserting restoration of the pre-batch branch and clean index while all Git-visible edits observed at failure remain as uncommitted changes.
- Exercise crash recovery at every persisted finalization journal step by terminating the CLI process and invoking a fresh process. Assert automatic recovery only for known states and quarantine for unknown HEAD, index, external Git activity, or unprovably running hook state.
- Exercise the generated Codex skill end to end in at least one actual Codex-driven project, proving parent-only orchestration, parallel disjoint workers, barrier validation, task-attributed commits, and interrupted-session discovery.
- Add narrower deterministic seams only where the CLI cannot reliably inject filesystem-notification ordering, process crashes at exact journal boundaries, clock advancement, or hook-process liveness. Keep these seams at the highest module boundary that can inject the fault, and continue asserting the resulting public state transition rather than private operations.
- Prefer controlled clocks, monitor event sources, process runners, and Git fault injection over tests of individual helper functions.
- There is no implementation or existing test suite in the current codebase, so no repository test prior art exists yet. The first CLI integration harness becomes the canonical prior art for subsequent tests.
- Use the eighteen MVP acceptance scenarios from the source specification as the initial end-to-end suite, expanding each into parameterized cases where path types, failure points, or state transitions vary.
- Run platform-sensitive path alias, executable-bit, notification, and process-liveness cases on both macOS and Linux rather than assuming one filesystem's behavior.

## Out of Scope

- Hard filesystem or process isolation between same-user agents.
- Git worktrees, per-task branches, linked worktrees, submodules, nested repositories, and sparse checkouts.
- Line-level, hunk-level, symbol-level, directory-glob, or semantic ownership.
- More than one concurrent Bandmaster session in a repository.
- Agent hosts other than Codex.
- Delegated worker-to-worker or worker-to-child orchestration.
- AI merge-conflict resolution or semantic AI review of otherwise clean changes.
- Remote Git operations, automatic pushes, and remote branch management.
- Native Windows support.
- Guaranteed detection of every transient write between observations.
- Reliable process attribution for a same-user write to a path currently owned by an active worker.
- Prevention of workers reading peers' in-progress changes.
- Rollback of network calls, spawned-process effects, ignored untracked files, caches, or other side effects outside Git-visible repository paths.
- Individual provisional commits passing repository validation in isolation.
- Strictly atomic visibility of provisional commits during finalization.
- A patch-queue mode, independent task branches, and a specialized resolver agent.

## Further Notes

- Cooperative compliance is fundamental. Same-user Unix permissions and advisory locks cannot provide the isolation Bandmaster needs, so the MVP combines explicit ownership with detection and quarantine.
- Full scans are authoritative only for the state visible when they run. Filesystem notifications improve detection but do not make transient-write detection complete.
- Claims apply to writes only. A worker can observe another worker's uncommitted content, and repository commands can be affected by concurrently mutable ignored state.
- Hook and validation processes may have network, process, or ignored-path side effects that Bandmaster can record where observable but cannot roll back.
- The validated unit is the combined frozen batch. Deterministic per-task commits provide attribution and reviewability after that combined result passes.
- The future isolated or patch-queue direction may introduce independent task branches and a resolver that receives base, current, incoming, task intent, worker handoff, and validation context. That direction is deliberately deferred because exclusive exact-file claims and serialized commits avoid ordinary merge conflicts in the MVP.
- Exact workflow command names, apart from `bandmaster init`, remain available to implementation design as long as the required capabilities and JSON contract are preserved.
