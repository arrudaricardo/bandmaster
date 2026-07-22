---
name: debug-bandmaster
description: Diagnose and explain Bandmaster runtime behavior from sanitized structured evidence. Use when asked to debug, diagnose, inspect, troubleshoot, or explain Bandmaster sessions, workers, tasks, claims, leases, batches, monitors, integrity, configuration, Git state, or orchestration failures.
---

# Debug Bandmaster

Begin with the installed executable's sanitized evidence:

1. Run 'bandmaster debug --json'. Do not initialize, repair, resume, sweep, or mutate state to obtain evidence.
2. Check 'runtime.bandmaster_version', 'runtime.executable', Go/build identity, repository location, state schema, collection status, and revision stability. Report partial or best-effort evidence honestly.
3. Interpret stable diagnostic codes, affected identities, evidence, and suggested supported CLI actions. Correlate derived workers with authoritative tasks, leases, and claims; do not invent a persisted Agent entity.
4. Correlate runtime evidence with source and public-interface tests. Never request or expose assignment tokens, environment values, stored content, raw SQLite rows, arbitrary blobs, or a database dump. Use the unsafe option only with explicit authorization and only when the secret itself is necessary.

For a changing reproduction, run 'bandmaster debug --watch --json' and consume the initial snapshot, semantic change records, collection errors/recovery, and heartbeats as NDJSON. Keep the selected session pinned unless the investigation explicitly needs '--follow-latest'.

If the request is diagnosis-only, stop after explaining the evidence and likely source path. Do not edit code. If the user authorizes a fix, implement and test it, build a fresh executable with 'go build -o <temporary-path>/bandmaster ./cmd/bandmaster', reproduce using that exact executable, and verify with a fresh '<temporary-path>/bandmaster debug --json'. Never treat an old installed binary's snapshot as proof of the fix.
