# Recover useful evidence from unstable or damaged state

Status: `ready-for-human`

## What to build

Keep `bandmaster debug` useful when collectors fail or concurrent workers change state, while honestly qualifying what was and was not observed.

## Acceptance criteria

- [x] Missing, locked, malformed, and schema-incompatible state returns every runtime, repository, configuration, filesystem, or Git section that can still be collected safely.
- [x] Every snapshot declares complete or partial collection, and structured collection errors distinguish unavailable sections from valid empty state.
- [x] Database state is observed in one read transaction, Git is inspected immediately afterward, and revision boundaries detect changes during collection without acquiring mutation locks.
- [x] Collection retries once after concurrent change and then returns an explicitly best-effort snapshot if the repository remains busy; coherent observations are marked stable.
- [x] A usable complete or partial snapshot exits successfully even when it describes unhealthy state, while invalid arguments or inability to produce useful evidence return the appropriate nonzero exit class.
- [x] Degraded and concurrent integration scenarios prove that diagnostic collection neither blocks public mutations nor changes repository or Bandmaster state.

## Blocked by

- 01 — Deliver a safe one-shot debug snapshot.

## Comments
