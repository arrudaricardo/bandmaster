# Bandmaster domain context

Bandmaster coordinates parallel AI work in one Git working tree while keeping assignment, ownership, validation, and recovery evidence durable.

## Vocabulary

- **Orchestrator Agent**: the sole coordinating actor. It creates and assigns Tasks, manages barriers and recovery, and may spawn executing Agents.
- **Agent**: an executing AI process assigned to one Task. Agent identity is derived from authoritative Task assignment, Lease, Claim, and activity evidence; it is not a persisted entity.
- **Task**: the durable unit of planned and executed work.
- **Batch**: an ordered, frozen set of Tasks that crosses validation and finalization as one barrier.
- **Batch Task**: a Task's ordered inclusion in a Batch. A Batch contains Tasks, never Agents.
- **Claim**: exclusive ownership of an exact Git-visible path by a Task.
- **Lease**: time-bounded evidence that an assigned Agent remains active on its Task.
- **Session**: the durable orchestration lifecycle for a repository.
- **Manifest**: the frozen, attributable path set used to validate and finalize a Batch.

The Go implementation is canonical. The Rust port is experimental and may use different vocabulary and JSON contracts.
