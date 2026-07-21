# Diagnose recovery blockers with doctor

Status: `ready-for-human`

## Parent

[Production-Safe Bandmaster Failure Recovery](../PRD.md)

## What to build

Add a read-only `bandmaster doctor --json` command that explains whether the repository and orchestration state are healthy and, when not, identifies supported recovery actions. Findings must cover incompatible session/batch pairs, dangling or contradictory finalization journals, staged rollback residue, unresolved integrity violations, and database relationships that would block cleanup.

Each finding must have a stable code, severity, affected entities or paths, structured evidence, and suggested supported commands. Doctor must never repair state itself.

## Acceptance criteria

- [ ] Healthy repositories return an explicit healthy result with no findings.
- [ ] Each specified inconsistent state produces a stable, independently testable finding code.
- [ ] Findings include severity, relevant session/task/batch identities or paths, evidence, and supported next actions.
- [ ] Doctor distinguishes staged rollback residue from generic index drift when journal evidence permits it.
- [ ] Doctor reports dependency blockers before abort or abandonment attempts cleanup.
- [ ] Running doctor makes no Git changes, database changes, monitor changes, or audit additions.
- [ ] JSON output follows existing CLI response conventions and is suitable for parent-agent automation.

## Blocked by

- [03 — Restore compatible session and batch states](03-compatible-recovery-states.md)
- [04 — Preserve ownership evidence while releasing claims](04-preserve-ownership-evidence.md)
- [06 — Expose explicit finalization recovery](06-explicit-finalization-recovery.md)
- [07 — Support safe batch abandonment](07-safe-batch-abandonment.md)

## Comments

- Implemented `bandmaster doctor --json` as a strictly read-only diagnostic
  command. It opens the existing SQLite state with `mode=ro` instead of the
  normal initialization/migration path and uses only read-only Git inspection.
  Healthy state returns `healthy: true` with an empty findings array. Every
  finding includes a stable code, severity, session/batch/task entities, paths,
  structured evidence, and supported commands.
- Added independent findings for `incompatible_session_batch_state`,
  `dangling_finalization_journal`, `contradictory_finalization_journal`,
  journal-backed `staged_rollback_residue`, generic `index_drift`,
  `unresolved_integrity_violation`, and `database_cleanup_blocker`. Cleanup
  diagnostics inspect live foreign-key relationships targeting active claims as
  well as existing foreign-key violations. CLI help, README automation guidance,
  and generated parent-agent instructions document that doctor diagnoses but
  never repairs.
- Focused validation passed: `go test ./integration -run '^TestDoctor' -count=1`;
  `go test ./internal/project -count=1`; `git diff --check`. The healthy scenario
  snapshots session status, audit count, monitor status/heartbeat, HEAD, index,
  and worktree before and after doctor to prove no Git, database, monitor, or
  audit mutation.
