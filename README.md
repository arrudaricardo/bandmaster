# Bandmaster

Bandmaster is a local Go CLI for coordinating parallel Codex workers in one Git working tree. This repository currently implements project initialization, explicit validation-configuration approval, persisted sessions, durable task planning and assignment, and complete Git-visible file claim and submission.

## Install

Bandmaster requires Go 1.24 or newer:

```sh
go install github.com/bandmaster-dev/bandmaster/cmd/bandmaster@latest
```

## Initialize

Run initialization from a standard Git repository or any directory beneath its root:

```sh
bandmaster init
```

Initialization creates `.bandmaster.yaml`, detects likely validation commands from Go, Node, Rust, and Python project manifests, and overwrites `.agents/skills/bandmaster/SKILL.md` with the current generated Codex skill. An existing `.bandmaster.yaml` is validated and preserved.

Review the configuration, inspect its digest, and approve that exact digest:

```sh
bandmaster config status
bandmaster config approve <digest>
```

Approval is local runtime state in `.git/bandmaster/state.db`; it is not tracked or copied to new clones. Changing `.bandmaster.yaml` changes its digest and makes the current configuration unapproved.

## Sessions

After committing the initialized project files and approving the current validation digest, start a session from a clean, attached branch:

```sh
bandmaster session start
bandmaster session inspect
```

Only one session may be open in a repository. Session state, the starting branch and commit, monitor identity and heartbeat, integrity violations, and transition audit history persist under `.git/bandmaster/` across CLI invocations. A claimless session supports these lifecycle commands:

```sh
bandmaster session pause
bandmaster session resume
bandmaster session finish
```

An active session owns one long-lived integrity monitor. Filesystem notifications provide prompt advisory checks, periodic full scans remain authoritative, and every mutating command requires a healthy monitor and a successful current-state scan. Bandmaster pauses the session and quarantines affected task and batch state if it observes an unclaimed non-ignored path, a changed submitted snapshot, index drift, HEAD drift, branch drift, base drift, or an unhealthy monitor.

After inspecting and restoring every recorded violation, explicitly recover it while the session remains paused, then resume. Recovery records the confirmation and performs another full scan before a new monitor generation can become active:

```sh
bandmaster integrity recover --confirmation "Removed the unowned file after inspection"
bandmaster session resume
```

Ignored untracked paths remain outside Bandmaster ownership and rollback guarantees. Tracked paths are always covered even when an ignore rule matches them. Resume also requires approval of the current configuration digest.

## Tasks

Create tasks during an active session. Tasks without prerequisites are immediately ready; a dependent remains planned until every prerequisite is committed or completes as a successful no-op in an earlier batch:

```sh
bandmaster task create --title "Build parser" --intent "Accept task plans" --expected-outcome "Valid plans persist"
bandmaster task create --title "Wire command" --intent "Expose planning" --expected-outcome "Agents can create tasks" --prerequisite <task-id>
bandmaster task list
bandmaster task inspect <task-id>
```

Assign a ready task to a worker. The response includes the opaque assignment token used by later worker commands:

```sh
bandmaster task assign <task-id> --worker <worker-id>
```

Assignment freezes the plan for that worker attempt. To replan or cancel assigned claimless work, first terminate the worker, identify it with `--terminated-worker`, and pass opaque evidence from the parent-held worker handle with `--termination-proof`. Bandmaster records the evidence and atomically revokes the assignment token. Replanning supplies the complete replacement plan, and omitting `--prerequisite` clears the prerequisite list:

```sh
bandmaster task replan <task-id> --title "New title" --intent "New intent" --expected-outcome "New outcome" --terminated-worker <worker-id> --termination-proof <proof>
bandmaster task cancel <task-id> --terminated-worker <worker-id> --termination-proof <proof>
```

A task with active dependents cannot be canceled until those dependents are canceled or replanned. A session cannot finish while any task is nonterminal.

### Worker Claims

An assigned worker first performs a read-only preflight with its complete initial exact-file write set and any focused validation commands. Focused validation is supplied as JSON with a stable name, exactly one of `argv` or `script`, a repository-relative working directory, timeout, and optional environment overrides:

```sh
bandmaster task preflight <task-id> --token <assignment-token> --path internal/parser.go --validation '{"name":"parser-tests","argv":["go","test","./internal/parser"],"working_directory":".","timeout":"2m"}'
bandmaster task claim <task-id> --token <assignment-token> --path internal/parser.go --validation '{"name":"parser-tests","argv":["go","test","./internal/parser"],"working_directory":".","timeout":"2m"}'
```

The initial claim is all-or-nothing. Success records complete baseline snapshots, joins the collecting batch, moves the task to `editing`, and permanently freezes its core planning fields. A conflicting initial write set moves the task to `blocked` without claims; after the conflicting owner releases its claim, requeue and assign the blocked task with a new worker token. An editing worker uses the same claim command to atomically expand its write set:

```sh
bandmaster task claim <task-id> --token <assignment-token> --path newly-discovered.go
bandmaster task release <task-id> --token <assignment-token> --path unused.go
bandmaster task requeue <blocked-task-id>
```

Release succeeds only while every requested path still exactly matches its recorded baseline. Claims accept canonical existing regular files and symlinks plus safely resolvable absent destinations. Snapshots preserve exact file bytes or symlink targets and executable state; directories, parent-symlink traversal, Git metadata, nested repositories, path aliases, and unsupported file types are rejected.

Assignment creates a worker lease using the approved `worker_lease_duration` from `.bandmaster.yaml`. Every token-bearing worker command renews it; workers should also heartbeat during long editing periods:

```sh
bandmaster task heartbeat <task-id> --token <assignment-token>
```

An expired lease moves the task to `quarantined` without releasing claims. Recover it only after proving termination through the parent-held worker handle, or after obtaining explicit user confirmation when that handle is unavailable. Recovery revokes the old token; assigning the repair-pending task gives the replacement a new token while retaining claims, baselines, and edits:

```sh
bandmaster task recover <task-id> --terminated-worker <worker-id> --termination-proof <proof> --diagnosis "Worker lease expired with partial edits" --intended-repair "Continue the retained work"
bandmaster task recover <task-id> --user-confirmation "<confirmation>" --diagnosis "Worker lease expired with partial edits" --intended-repair "Continue the retained work"
bandmaster task assign <task-id> --worker <replacement-worker-id>
```

After editing, review every claimed path from its recorded baseline, then submit a structured handoff:

```sh
bandmaster task diff <task-id> --token <assignment-token>
bandmaster task submit <task-id> --token <assignment-token> --behavior-changed "Parser accepts quoted fields" --key-decisions "Kept tokenization streaming" --validation-expectations "Parser tests and repository checks pass" --known-risks "None"
```

Submission rejects any Git-visible changed path without an owner and freezes every claimed current snapshot. An unchanged submission remains `submitted` with outcome `pending_no_op`; it does not create an empty commit.

## Batch Barrier

After every active worker has submitted, blocked, failed, or been deliberately stopped, freeze the current collecting batch:

```sh
bandmaster batch freeze
bandmaster batch inspect [batch-id]
```

The barrier performs authoritative repository scans, rejects active workers and unsubmitted members, verifies every changed path has exactly one submitted owner, and copies the ordered membership, ownership, baselines, and submitted snapshots into a persisted frozen manifest. Preflight and blocked tasks never join the batch, and dependent tasks remain planned until their prerequisites succeed in an earlier batch. A successful freeze moves the session to `finalizing` and stops the active-session monitor so official validation can take exclusive control in the next workflow stage.

## Batch Validation

Run official validation only after the batch barrier:

```sh
bandmaster batch validate
bandmaster batch inspect [batch-id]
```

Bandmaster runs each worker-declared focused command in frozen membership order, then each approved repository command in configuration order. Commands run sequentially with authoritative scans before and after each command. Any Git-visible mutation quarantines the batch and pauses the session, even when the command exits successfully. An ordinary non-zero exit or timeout creates no commit, retains claims and edits, moves the batch to `repair_pending`, and returns the session to monitored active state for repair selection.

Reopen each original owner whose paths require repair, recording why the prior attempt failed and what the replacement should change. Worker-handle termination evidence is required unless the user explicitly confirms that the old worker stopped:

```sh
bandmaster task repair <task-id> --terminated-worker <worker-id> --termination-proof <proof> --diagnosis "Combined validation failed" --intended-repair "Correct the owned parser paths"
bandmaster task repair <task-id> --user-confirmation "I confirmed the old worker stopped" --diagnosis "Combined validation failed" --intended-repair "Correct the owned parser paths"
bandmaster task assign <task-id> --worker <replacement-worker-id>
```

Repair invalidates only the selected owners' stale submissions while retaining their claims, original baselines, current edits, batch membership, and batch base. Replacement workers receive fresh tokens and may atomically expand into currently unowned paths. If several owners are involved, reopen each one separately; claims are never transferred. Unrelated work cannot join the closed repairing batch, and `batch freeze` remains blocked until every reopened owner reviews and resubmits.

Validation uses a documented minimal ambient environment: `CI`, `HOME`, `LANG`, `LC_ALL`, `PATH`, `TEMP`, `TMP`, `TMPDIR`, and `TZ` are inherited when present, with a standard system `PATH` fallback. Each command's declared `environment` values override that set. The repository root is the default working directory when `working_directory` is omitted. `batch inspect` records the declared and resolved command, canonical directory, timeout, environment and overrides, status, exit code, duration, and up to 64 KiB each of standard output and error with truncation flags.

Bandmaster rejects bare repositories, linked worktrees, external Git directories, sparse checkouts, submodules, repositories nested inside other repositories, and repositories containing nested Git metadata.

## Agent Output

Every command supports `--json`. Schema version 1 responses contain `schema_version`, `command`, `success`, and exactly one of `result` or `error`. Error responses contain a stable `code`, a human-readable `message`, and `retryable`.

Schema 1 fields will not be removed or change meaning. Additive fields may be introduced. Consumers must ignore fields they do not recognize.

Exit classes are stable:

| Exit code | Class |
| --- | --- |
| `0` | Success |
| `1` | Internal failure |
| `2` | Blocked work |
| `3` | Invalid input, state, or unsupported repository |
| `4` | Integrity quarantine |
| `5` | Validation failure |
