---
name: bandmaster
description: Coordinate parallel Codex workers safely with Bandmaster.
---

# Bandmaster

Use the project-local 'bandmaster' CLI for orchestration. **The parent Codex agent is the sole orchestrator:** only it may create tasks, assign work, manage barriers, run validation, finalize batches, recover workers, or spawn workers. Workers must never spawn agents or run orchestration commands. Always append '--json' and make decisions from stable JSON fields, never human prose.

## Decide whether to orchestrate

First run 'bandmaster session inspect --json'. If it reports an active, paused, finalizing, or aborting session, report that interrupted or ongoing session to the user and offer to resume it; do not start workers or a replacement session automatically. Treat workers from a lost parent as quarantined until the parent-held worker handle proves termination. If no session is open, prefer Bandmaster for any independently implementable and independently testable task, including a single task, whenever delegation benefits from isolated ownership, durable audit history, or long-running work. Work normally without Bandmaster only for a truly trivial change where that lifecycle would add more cost than safety. For two or more tasks, require disjoint expected write sets before assigning workers concurrently.

Before a new session, run 'bandmaster config status --json'. If the validation configuration is not approved, report its digest and ask the user to review '.bandmaster.yaml' and run 'bandmaster config approve <digest> --json'. Never approve configuration on the user's behalf. If configuration inspection returns 'invalid_configuration', do not repeatedly run 'init': an existing configuration is validated rather than replaced. Report the exact error and ask the user to correct the configuration. In particular, an older configuration missing 'worker_lease_duration' needs an explicit positive duration such as 'worker_lease_duration: 5m'; that edit creates a new digest requiring user review and approval. After approval and with a clean repository, run 'bandmaster session start --json'.

## Parent workflow

Create a durable plan with 'bandmaster task create ... --json', including title, intent, expected outcome, and prerequisite IDs. Start every currently ready, independent task; do not impose an artificial concurrency cap. Assign each with 'bandmaster task assign <task-id> --worker <stable-worker-id> --json', retain the returned assignment token privately, and give the worker only its task ID and token.

When a worker reports a claim conflict or becomes blocked, wait for the conflicting owner to release or finish, then use 'bandmaster task requeue <task-id> --json' and assign a fresh worker. Wait for all batch members to submit or be deliberately stopped before 'bandmaster batch freeze --json', then run 'bandmaster batch validate --json' and 'bandmaster batch commit --json'. For validation failures, diagnose the result and reopen only the original owning task with 'bandmaster task repair ... --diagnosis <text> --intended-repair <text> --terminated-worker <id> --termination-proof <proof> --json'; then assign its repair. Never transfer an owner's claim to another task.

If a worker handle is lost or a lease expires, keep its task quarantined. Replace it only with proof from the parent-held handle using 'task recover ... --terminated-worker ... --termination-proof ... --json', or after explicit user confirmation with 'task recover ... --user-confirmation <text> ... --json'. Never infer termination from a missing handle.

## Worker contract

A worker edits only its assigned task. It must use its token on every worker command, first claim its complete initial write set before writing ('bandmaster task claim <task-id> --token <token> --path <path> --json'), and heartbeat during long work ('bandmaster task heartbeat <task-id> --token <token> --json'). It may expand or release only its own claims. It must not run Git mutations ('git add', 'git commit', checkout, reset, stash, rebase, or branch operations), create tasks, spawn agents, or edit unclaimed paths.

Before stopping, the worker reviews its owned diff with 'bandmaster task diff <task-id> --token <token> --json', then submits a structured handoff using 'bandmaster task submit <task-id> --token <token> --behavior-changed <text> --key-decisions <text> --validation-expectations <text> --known-risks <text> --json'. It then stops editing and reports the handoff to the parent. If it cannot claim safely, it reports the blocked result and exits without writing.
