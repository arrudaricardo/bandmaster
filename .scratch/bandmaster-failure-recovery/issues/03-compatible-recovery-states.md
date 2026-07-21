# Restore compatible session and batch states

Status: `ready-for-human`

## Parent

[Production-Safe Bandmaster Failure Recovery](../PRD.md)

## What to build

Define and enforce one explicit transition table for compatible session and latest-batch states. Integrity recovery must restore the session, batch, affected tasks, violations, and audit records as one atomic transition, including restoring a finalizing session whenever the recovered batch requires finalization.

Public mutations must reject incompatible pairs before making further changes. Recovery retries must be idempotent, and each successfully restored pair must admit its documented next public command.

## Acceptance criteria

- [ ] Recovered frozen, validating, finalizing, and final-validating batches are paired with a finalizing session.
- [ ] A recovered repair-pending batch is paired with an active session.
- [ ] All other supported nonterminal and terminal pairs are documented in one transition table and covered by tests.
- [ ] Recovery changes session, batch, task, violation, and audit state atomically.
- [ ] Mutation preconditions reject incompatible pairs without changing durable state.
- [ ] Repeating recovery against an already recovered state returns a stable result without duplicate transitions.
- [ ] Each restored pair accepts its documented next CLI command.
- [ ] Corrupt-state fixtures may use controlled database setup, but outcomes are asserted through public CLI behavior.

## Blocked by

- None — can start immediately.

## Comments

- Implemented an authoritative session/latest-batch compatibility table, atomic recovery mappings (including normalization of interrupted validation states), idempotent recovery retries, active-monitor restoration, and mutation gating for incompatible pairs. Added public CLI recovery/state-corruption scenarios plus focused transition-table coverage. Verified with `go test ./internal/project ./internal/cli -run '^(TestRecoveryTransitionsProduceCompatibleRetryStates|TestCompatibilityTableDocumentsEveryPersistedState|TestMutatingCommand.*)$' -count=1` and `go test ./integration -run '(Integrity|IncompatibleSessionBatch)' -count=1`.
- Integration follow-up: explicit `integrity recover` and `finalization recover` commands now bypass the generic mutation gate so they can classify and repair the inconsistent states they own. The focused submitted-drift and recovery-pair scenarios pass together.
