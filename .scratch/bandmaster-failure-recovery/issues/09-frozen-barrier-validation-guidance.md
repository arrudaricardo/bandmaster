# Align worker validation with the frozen barrier

Status: `ready-for-human`

## Parent

[Production-Safe Bandmaster Failure Recovery](../PRD.md)

## What to build

Update generated parent and worker orchestration guidance so workers run focused checks while peers share a mutable working tree, while repository-wide validation runs authoritatively after all workers stop editing and the batch freezes. Preserve the existing final validation policy after provisional commits.

The guidance must remain deterministic, project-local, and explicit that a worker-observed full-suite failure during concurrent package movement is not an official batch result.

## Acceptance criteria

- [ ] Generated parent guidance schedules official repository-wide validation only at the frozen barrier and existing final-validation stage.
- [ ] Generated worker guidance requests focused validations scoped to the worker's owned behavior during mutable parallel work.
- [ ] Guidance explains that workers must not keep editing after submission or race the frozen barrier.
- [ ] Existing generated-skill safety rules for claims, Git mutation, leases, recovery, and sole orchestration remain unchanged.
- [ ] Configuration or generation tests assert the new deterministic guidance.

## Blocked by

- None — can start immediately.

## Comments

- Implemented generated parent/worker guidance at the `bandmaster init` seam. Workers now declare and run focused owned-behavior checks during mutable parallel work; repository-wide results become authoritative only after all workers stop and the batch freezes; submitted workers must not resume edits or race the barrier; and batch commit retains final validation after provisional commits. Existing claims, Git mutation, lease, recovery, and sole-orchestrator rules were preserved.
- TDD coverage: `go test ./integration -run '^TestInitGuidesValidationAroundTheFrozenBarrier$' -count=1` (red before implementation, then green) and `go test ./integration -run '^TestInit' -count=1` (pass).
