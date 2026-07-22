# Live Bandmaster Diagnostics

Status: `ready-for-human`

## Problem Statement

Bandmaster persists the coordination state needed to run parallel Codex workers safely, but that state is fragmented across session, task, lease, claim, batch, integrity, monitor, Git, and audit records. Existing inspection commands expose individual entities, `doctor` targets known recovery problems, and the live TUI provides only a compact session and task overview. A user or Codex agent debugging Bandmaster cannot obtain one coherent, machine-readable explanation of what every worker owns, why work is blocked, whether the observed state is current, or which Bandmaster build produced the observation.

This makes development feedback slow and unreliable. Agents must assemble multiple command responses, may accidentally use a stale binary, and may paste authority-bearing tokens, stored content, or environment secrets into diagnostic conversations. Debugging becomes especially difficult when Bandmaster has not been initialized, its state is partially unreadable, or concurrent workers change the repository during inspection.

The user needs one safe, read-only diagnostic command that works in healthy and degraded repositories, supports both one-shot and live observation, and gives Codex enough structured evidence to diagnose and improve Bandmaster without coupling itself to the private SQLite schema.

## Solution

Bandmaster will provide `bandmaster debug` as the canonical diagnostic interface. A one-shot invocation will return a human-readable diagnostic snapshot, while `--json` will return the stable machine contract. `bandmaster debug --watch` will provide the live human dashboard, and `bandmaster debug --watch --json` will emit an NDJSON stream containing an initial full snapshot, sanitized semantic changes, health heartbeats, transient collection errors, and recovery events. The existing `bandmaster tui` command will remain as a compatibility alias for the human watch experience.

Each snapshot will normalize current session, derived worker, task, lease, claim, batch, monitor, Git, integrity, audit, configuration, and runtime/build information. It will include structured diagnostics with stable codes, severity, affected identities, evidence, and suggested CLI actions. It will explicitly report collection completeness and cross-source consistency. Current domain records remain authoritative; workers are a derived view grouped by `worker_identity`, not a new persisted entity.

The command will be strictly read-only and safe by default. It will redact assignment tokens, environment values, and stored file contents while retaining useful presence, fingerprint, hash, size, status, and timing metadata. It will operate without initializing or repairing Bandmaster and will return partial evidence when configuration, database, or Git collection fails. An explicit unsafe option may reveal normalized secret-bearing fields, but the command will never expose raw SQLite rows or act as a database dump.

A new repository-local `debug-bandmaster` Codex skill will teach agents to use the diagnostic contract whenever they are asked to debug, diagnose, inspect, troubleshoot, or explain Bandmaster runtime behavior. The general Bandmaster orchestration skill will remain focused and will not absorb this debugging workflow.

## User Stories

1. As a Bandmaster user, I want one command that summarizes orchestration state, so that I do not have to combine many inspection commands manually.
2. As a Bandmaster maintainer, I want a stable JSON diagnostic snapshot, so that Codex can reason about runtime evidence without parsing prose.
3. As a Bandmaster maintainer, I want the human diagnostic view and the machine diagnostic view to share one underlying model, so that they cannot silently disagree.
4. As an operator, I want to see the current open session by default, so that the most relevant orchestration state is immediately visible.
5. As an operator, I want the most recent terminal session shown when no session is open, so that post-incident evidence remains easy to inspect.
6. As an operator, I want to select an exact historical session, so that I can investigate a known incident deterministically.
7. As an operator, I want watch mode to remain attached to its selected session, so that a newly created session does not silently change the subject of my investigation.
8. As an operator, I want an explicit option to follow the latest session, so that I can monitor a sequence of session lifecycles when desired.
9. As a parent Codex agent, I want workers grouped by stable worker identity, so that I can see each worker's tasks, claims, leases, and last activity together.
10. As a parent Codex agent, I want worker information derived from authoritative task and lease records, so that debugging does not introduce a competing agent lifecycle model.
11. As a parent Codex agent, I want all active claims and their owning tasks visible, so that I can explain claim contention and path ownership.
12. As a parent Codex agent, I want lease renewal and expiry information visible, so that I can distinguish a working agent from abandoned or quarantined ownership.
13. As a parent Codex agent, I want task prerequisites and blocker relationships visible, so that I can explain why planned work is not ready.
14. As an operator, I want batch membership, status, validation, manifest, and finalization evidence visible, so that I can diagnose barrier and commit failures.
15. As an operator, I want monitor health and unresolved integrity violations visible, so that I can explain pauses and quarantines.
16. As a maintainer, I want Git branch, HEAD, index, and worktree observations correlated with claims, so that owned, unowned, and drifting changes are distinguishable.
17. As a maintainer, I want stable diagnostic codes with severity and evidence, so that fixes and automation do not depend on human wording.
18. As a maintainer, I want each actionable diagnostic to suggest a supported CLI command, so that recovery begins from explicit Bandmaster operations rather than database editing.
19. As a user, I want debugging to be strictly read-only, so that observing a failure cannot sweep leases, start monitors, pause sessions, repair records, or otherwise alter state.
20. As a user, I want debugging before initialization to create no files or tables, so that inspection never initializes Bandmaster implicitly.
21. As a maintainer, I want useful runtime, configuration, filesystem, and Git evidence even when state is missing or corrupt, so that initialization and database failures can be diagnosed.
22. As a maintainer, I want partial snapshots to identify unavailable sections and collection errors, so that missing evidence is never mistaken for an empty healthy state.
23. As an automation author, I want usable partial snapshots to exit successfully, so that recoverable evidence is not discarded solely because one collector failed.
24. As an automation author, I want a nonzero exit only when no useful snapshot can be produced or arguments are invalid, so that exit status has predictable meaning.
25. As a security-conscious user, I want assignment tokens, environment values, and stored content redacted by default, so that diagnostic output is safe to share with an agent.
26. As a maintainer, I want redacted fields represented by presence, fingerprint, hash, size, or metadata, so that the snapshot remains diagnostically useful.
27. As an authorized maintainer, I want an explicit unsafe option for normalized secret-bearing fields, so that rare local investigations are possible without exposing raw storage.
28. As a user, I want raw SQLite rows and internal blobs excluded from the CLI contract, so that diagnostics remain safe and schema-independent.
29. As a maintainer, I want every snapshot to identify the running Bandmaster version and executable, so that I can detect stale installed binaries.
30. As a maintainer, I want build revision, dirty status, Go version, target platform, project paths, and database schema version visible, so that environmental mismatches are evident.
31. As an automation author, I want a bounded default audit history, so that routine snapshots remain manageable.
32. As an automation author, I want configurable and complete audit-history options, so that deeper investigations can retrieve the necessary evidence.
33. As a maintainer, I want a consistency marker and revision boundaries, so that I know whether database and Git evidence describe one stable observation.
34. As a worker, I want debugging to avoid mutation locks, so that observation cannot stall active orchestration.
35. As an operator, I want a best-effort snapshot after bounded retry when state keeps changing, so that a busy repository remains observable.
36. As an operator, I want live human output to refresh automatically, so that I can monitor progress without rerunning commands.
37. As an automation author, I want JSON watch mode to begin with a full snapshot, so that each new stream is self-contained.
38. As an automation author, I want later watch records to contain semantic changes rather than repeated full snapshots, so that changes are efficient to consume.
39. As an automation author, I want a stream-local monotonic sequence, so that I can detect dropped or reordered records within one invocation.
40. As an automation author, I want periodic heartbeat records while state is unchanged, so that silence is distinguishable from a dead watcher.
41. As an automation author, I want transient collection errors and recovery represented as JSON records, so that watch mode can remain alive through temporary failures.
42. As an automation author, I want clean signal handling and JSON-only standard output, so that stopping watch mode does not corrupt the stream.
43. As a user, I want a configurable polling interval with a safe lower bound, so that I can trade responsiveness for resource usage.
44. As a user, I want adaptive Git inspection, so that live diagnostics remain responsive without continuously performing unnecessarily expensive scans.
45. As an existing Bandmaster user, I want `bandmaster tui` to keep working, so that the new canonical command does not break established workflows.
46. As a Codex user, I want a dedicated `debug-bandmaster` skill to trigger on Bandmaster debugging requests, so that Codex consistently begins with structured runtime evidence.
47. As a Codex user, I want that skill to distinguish diagnosis from implementation, so that asking what is wrong does not implicitly authorize code changes.
48. As a Bandmaster maintainer, I want the debugging skill to correlate snapshots with source and tests, so that runtime findings lead to evidence-based diagnoses.
49. As a Bandmaster maintainer, I want the debugging skill to rebuild and re-run a reproduction after an authorized fix, so that a stale binary cannot produce false verification.
50. As a Bandmaster maintainer, I want the general orchestration skill unchanged by this workflow, so that routine worker coordination does not load debugging instructions.

## Implementation Decisions

- Introduce `bandmaster debug` as the canonical diagnostic command. With no output flag it renders one human-readable snapshot; `--json` returns one machine-readable snapshot; `--watch` starts the live human dashboard; and `--watch --json` emits NDJSON.
- Preserve `bandmaster tui` as a compatibility alias for `bandmaster debug --watch`. Maintain one diagnostic model and one human live renderer rather than two independent dashboards.
- Keep the command strictly read-only in every mode. Diagnostic collection must not call mutation preparation, sweep logically expired leases, start or restart monitors, initialize state, repair records, or trigger state transitions.
- Add a non-creating, read-only project and state discovery path. Missing initialization, absent state, locked state, malformed state, and incompatible schema must yield partial diagnostic evidence whenever repository, runtime, configuration, filesystem, or Git facts remain recoverable.
- Define a normalized debug snapshot contract under the existing versioned CLI envelope. Give the diagnostic result its own explicit schema version so its compatibility can evolve independently of unrelated command results.
- Include runtime identity in every snapshot: Bandmaster version, CLI and debug schema versions, executable path, Go version, target OS and architecture, embedded VCS revision and dirty-build status when available, project root, Git directory, state database path, and database schema version.
- Select the open session by default. If none is open, select the most recent completed or aborted session and mark it historical. Support exact session selection and an explicit follow-latest policy; ordinary watch mode remains pinned to its selected session.
- Include normalized session, tasks, leases, claims, ownership evidence metadata, batches, validation summaries, monitor, integrity violations, configuration status, Git observations, and recent audit events.
- Derive a top-level `workers` view by grouping authoritative task and lease state by `worker_identity`. Do not add or persist a separate Agent entity.
- Redact assignment tokens, environment values, stored file contents, captured command output that is classified as secret-bearing, and similar authority or content fields by default. Preserve safe metadata such as presence, short fingerprint, content hash, byte size, status, and timestamps.
- Provide an explicitly unsafe local option to reveal secret-bearing fields within the normalized model. The option does not expose raw tables, arbitrary database rows, internal blobs, or a database dump.
- Attach stable diagnostics to snapshots. Each diagnostic has a code, severity, affected session/worker/task/batch/path identities, structured evidence, and a supported suggested CLI action when one exists. Diagnostics cover at least lease timing, claim contention, dependency blocking, monitor health, unresolved integrity state, unowned changes, submitted drift, Git control-state drift, configuration drift, and invariant failures.
- Include complete current entity state and the latest 50 relevant audit or diagnostic events by default. Support a configurable history limit and an explicit complete-history option.
- Capture database state in one read transaction, inspect Git immediately afterward, and compare database revision boundaries. Retry once after an observed concurrent change; if the observation remains busy, return it as best effort rather than blocking mutations. Mark stable and best-effort snapshots explicitly.
- Mark snapshots complete or partial. Structured collection errors identify unavailable sections and causes. Return success when a usable snapshot exists, even when it reports unhealthy state or partial collection; return a nonzero status only for invalid arguments or inability to produce useful evidence.
- In JSON watch mode, emit an initial full snapshot followed by sanitized semantic change records. Use a stream-local monotonically increasing sequence; restarting a watcher begins a new stream with a new full snapshot rather than replaying a durable global change log.
- Emit a lightweight heartbeat every ten seconds with stream sequence, capture time, revision boundaries, and collection health. Emit collection-error and recovered records around transient failures, and continue retrying.
- Keep stdout strictly machine-readable in JSON watch mode. Handle interruption and termination signals cleanly with a successful exit when shutdown itself succeeds.
- Implement portable polling with a one-second default and a minimum interval of 250 milliseconds. Refresh inexpensive database observations each interval; trigger heavier Git inspection from relevant changes and at least every two seconds as a fallback.
- Add a repository-local skill named `debug-bandmaster`. Its trigger description covers requests to debug, diagnose, inspect, troubleshoot, or explain Bandmaster runtime behavior.
- Instruct the debugging skill to begin with a sanitized one-shot JSON snapshot, interpret its diagnostics and relationships, correlate evidence with source and tests, and use JSON watch mode for changing reproductions. It diagnoses without modifying unless the user asks for a fix; after an authorized fix it rebuilds, reproduces, and verifies with a fresh snapshot.
- Do not add the debug workflow or prompts to the existing general Bandmaster orchestration skill.

## Testing Decisions

- Use the compiled public CLI in temporary Git repositories as the primary and highest test seam, matching the repository's existing integration harness. Tests assert JSON/NDJSON, exit status, signals, Git-visible state, and durable public behavior rather than private query helpers or SQLite layouts.
- Prove one-shot human and JSON snapshots across uninitialized, initialized-without-session, open-session, completed-session, aborted-session, explicitly selected historical-session, healthy, and unhealthy scenarios.
- Construct representative tasks, workers, active and expired leases, disjoint and contended claims, dependency blockers, collecting and frozen batches, validations, monitor states, and integrity findings through public CLI workflows wherever possible; assert their normalized relationships in the snapshot.
- Verify safe redaction using distinctive assignment tokens, environment values, command output, and stored content. Assert that default output contains no raw secret and that safe fingerprints and metadata remain useful. Verify the explicit unsafe mode only against the normalized fields it authorizes.
- Verify strict read-only behavior by capturing Git state and database bytes or observable durable state before and after one-shot and watch collection. Debugging must not create Bandmaster directories or tables in an uninitialized repository and must not sweep expired leases or change monitors and sessions.
- Verify degraded collection with missing, locked, malformed, and schema-incompatible state. Assert partial completeness, section-specific errors, available runtime/Git/configuration evidence, and the agreed success or failure exit semantics.
- Verify runtime identity from both development and version-stamped builds where the harness permits it, including executable identity and embedded VCS metadata behavior.
- Verify session selection defaults, historical marking, exact selection, pinned watch behavior, and explicit follow-latest behavior.
- Verify stable diagnostic codes, severity, affected identities, evidence, and suggested supported commands for each required diagnostic family. Human prose may change without breaking tests.
- Exercise JSON watch mode as a subprocess. Read the initial snapshot, mutate state through separate public CLI processes, observe ordered semantic change records, observe idle heartbeats, inject transient collection failure, observe collection-error and recovered records, and terminate cleanly.
- Verify watch output is valid NDJSON with no human text on stdout and a strictly increasing sequence within the stream. Restarting produces a new initial snapshot and does not promise durable replay.
- Verify stable versus best-effort consistency by arranging concurrent public mutations during collection without requiring debug to block those mutations.
- Verify default, custom, minimum-invalid, and accepted polling intervals through externally observable timing with tolerant bounds rather than implementation-specific timers.
- Verify `bandmaster tui` reaches the same live human behavior as `bandmaster debug --watch`; retain focused renderer tests only where terminal presentation cannot be asserted reliably through the CLI seam.
- Verify initialization or repository setup installs the dedicated `debug-bandmaster` skill with trigger metadata and the agreed diagnostic workflow while leaving the general Bandmaster skill free of the debugging prompt.
- Run the complete Go test suite as final acceptance because command routing, JSON compatibility, session behavior, integrity protection, recovery, and the existing TUI must remain compatible.

## Out of Scope

- Watching Bandmaster's Go source, rebuilding the executable, or hot-restarting the running debug process when source files change.
- Persisting a global diagnostic event log or replaying missed watch events across separate command invocations.
- Advanced server-side filtering by worker, task, path, or severity in the first release.
- Diagnostic export bundles, portable support archives, remote streaming, network listeners, or browser dashboards.
- A new persisted Agent entity or changes to task assignment, claim ownership, worker leases, or orchestration authority.
- Raw SQLite row output, database dumps, arbitrary SQL access, or treating the private database schema as a public API.
- Automatic recovery, repair, monitor restart, prompt modification, source modification, issue creation, or other mutation initiated by the debug command.
- Adding the debugging workflow to the general Bandmaster orchestration skill.

## Further Notes

- The word `worker` remains the canonical domain term because Bandmaster already persists worker identity on tasks. Human presentation may label the section “Agents / workers” for discoverability.
- Debug health is not command success. A complete snapshot describing a severely unhealthy session is still a successful diagnostic result.
- Partial evidence must remain honest: unavailable collectors are errors, not empty arrays, and best-effort cross-source state must never be labeled stable.
- Suggested actions are guidance grounded in supported Bandmaster commands. They must not embed redacted assignment tokens or imply authority the caller does not possess.
- The dedicated debugging skill is part of the user-facing feedback loop: it ensures Codex first observes the running system, checks build identity, and reproduces behavior before proposing or validating changes.
