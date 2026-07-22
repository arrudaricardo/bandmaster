# ADR 0001: Agents execute Tasks and Batches contain Batch Tasks

Status: accepted

## Decision

The Go product uses **Orchestrator Agent** for the sole coordinator and **Agent** for an executing AI process. Agent presentation is derived from Task assignment, Lease, Claim, and activity records; no independently mutable Agent entity is persisted.

A Batch exposes an ordered collection of **Batch Tasks** through `tasks` and `task_order`. The durable Task remains in its Batch when an Agent stops or is replaced.

The Go JSON contract and state schema are version 2. Version 1 Worker and Member vocabulary is migrated atomically in state, is rejected in tracked configuration, and is not emitted as a public alias. The Rust port remains experimental and outside this migration.

## Consequences

- Assignment and recovery authority remains anchored to Task evidence.
- Batch ordering cannot drift with Agent lifecycle changes.
- Existing Go state is preserved by a verified migration.
- Existing tracked configuration must be regenerated, reviewed, and approved because trust-bearing setting names changed.
