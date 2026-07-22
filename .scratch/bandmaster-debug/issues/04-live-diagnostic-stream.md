# Stream live diagnostic changes

Status: `ready-for-human`

## What to build

Let agents consume a self-contained live NDJSON diagnostic stream that reports meaningful state changes and remains observable through idle periods and transient collection failures.

## Acceptance criteria

- [x] `bandmaster debug --watch --json` emits a complete initial snapshot followed by sanitized semantic change records rather than repeated full snapshots.
- [x] Every stream record has a stream-local monotonically increasing sequence; a restarted invocation begins with a fresh snapshot and makes no durable replay promise.
- [x] Idle streams emit a lightweight heartbeat every ten seconds with capture time, revision boundaries, and collection health.
- [x] Transient failures emit structured collection-error records, successful recovery emits a recovered record, and the watcher continues retrying.
- [x] Watch mode stays pinned to the initially selected session unless `--follow-latest` explicitly allows it to adopt a newer session.
- [x] Portable polling defaults to one second, rejects intervals below 250 milliseconds, refreshes inexpensive state each interval, and performs adaptive Git inspection with a two-second fallback.
- [x] JSON watch mode writes only valid NDJSON to stdout and exits cleanly with success on an ordinary interrupt or termination signal.
- [x] Subprocess integration tests observe the initial snapshot, public CLI mutations, semantic changes, heartbeats, transient failure and recovery, session selection behavior, and clean shutdown.

## Blocked by

- 02 — Explain complete orchestration state.
- 03 — Recover useful evidence from unstable or damaged state.

## Comments
