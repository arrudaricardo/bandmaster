# Tickets: Agent-centered Go TUI and domain vocabulary

These tickets implement the [Agent-Centered Go TUI and Domain Vocabulary](PRD.md) specification.

Implementation is complete and verified in the final Go v2 state. The expansion-phase compatibility criteria below record intermediate migration milestones; the later contraction ticket intentionally supersedes them by removing the temporary legacy aliases. `ready-for-human` records the completed Agent handoff for maintainer acceptance.

Work the **frontier**: any ticket whose blockers are complete. The compiled TUI harness and the two vocabulary expansions can start immediately; subsequent migration, contract, and dashboard slices follow their declared edges.

## Add compiled TUI interaction coverage

Status: `ready-for-human`

**What to build:** Make the current live Go dashboard testable through the compiled CLI in a controlled pseudo-terminal, so later interaction and responsive-layout changes can be verified at the same boundary users operate.

**Blocked by:** None — can start immediately.

- [x] The integration harness launches the freshly compiled Go CLI with a real pseudo-terminal rather than calling renderer helpers directly.
- [x] Tests can set and change terminal width and height, send key sequences, wait for deterministic output, and terminate the program cleanly.
- [x] The harness can observe both `bandmaster debug --watch` and its `bandmaster tui` compatibility alias.
- [x] Coverage proves the existing manual refresh, resize, and quit controls still work through the compiled executable.
- [x] A read-only assertion proves opening, refreshing, resizing, and closing the dashboard does not change repository files, Git state, or persisted orchestration state.
- [x] ANSI handling is robust enough to assert meaningful visible content without coupling tests to incidental escape-sequence ordering.
- [x] Existing renderer tests remain focused on deterministic presentation details that cannot be tested reliably at the compiled boundary.
- [x] The complete Go test suite remains green without changing dashboard behavior in this prefactor.

## Expand the Go Agent vocabulary

Status: `ready-for-human`

**What to build:** Introduce Agent-oriented Go names and temporarily compatible public forms beside the legacy Worker vocabulary, giving subsequent migrations a green path without changing orchestration semantics.

**Blocked by:** None — can start immediately.

- [x] Agent-named domain and diagnostic types, fields, and helpers are available for new call sites without creating a persisted Agent entity.
- [x] Agent identity continues to derive from authoritative Task assignment, Lease, Claim, and activity evidence.
- [x] Agent-oriented assignment, recovery, and repair option names are accepted alongside the legacy spellings during the expansion phase.
- [x] Agent-oriented configuration and generated-guidance vocabulary can be produced without invalidating existing configuration during the expansion phase.
- [x] Transitional JSON shapes can expose the new Agent fields while existing tests and consumers remain operable until the contract ticket removes aliases.
- [x] Existing Worker forms delegate to the same behavior rather than creating a second assignment or recovery path.
- [x] Orchestration states, assignment authority, token handling, Lease behavior, Claim behavior, and recovery safety are unchanged.
- [x] Focused tests demonstrate that legacy and new entry points describe and operate on the same Task evidence.

## Expand the Go Batch Task vocabulary

Status: `ready-for-human`

**What to build:** Introduce `BatchTask`, `tasks`, and `task_order` forms beside the legacy Member vocabulary, so Batch call sites can migrate incrementally while preserving one authoritative ordered Task set.

**Blocked by:** None — can start immediately.

- [x] Batch Task types, collections, ordering fields, and helpers are available to new Go call sites.
- [x] Transitional legacy Member forms delegate to the same underlying Batch Task records and cannot diverge.
- [x] Batch Task ordering remains deterministic and retains the existing uniqueness guarantees.
- [x] A Batch continues to contain Tasks rather than executing Agents.
- [x] Transitional JSON output can expose the new Batch Task fields without breaking existing tests before the contract cutover.
- [x] No duplicate Batch membership lifecycle or alternate persisted source of truth is introduced.
- [x] Existing freeze, repair, finalization, and inspection behavior remains unchanged during expansion.
- [x] Focused tests prove the new and legacy forms observe identical Task IDs, order, status, and outcomes.

## Migrate task execution and recovery to Agents

Status: `ready-for-human`

**What to build:** Make Go task execution, ownership, Lease renewal, recovery, repair, integrity handling, and attribution consistently use Agent terminology internally while preserving every existing safety transition.

**Blocked by:** Expand the Go Agent vocabulary.

- [x] Task assignment and inspection use Agent identity internally and through the new Agent-oriented public forms.
- [x] Claim acquisition, expansion, release, heartbeat, submission, and diff workflows attribute work to the assigned Agent.
- [x] Lease creation, renewal, expiry, quarantine, and closure preserve current behavior under Agent terminology.
- [x] Lost-Agent recovery and repair require the same termination proof or audited user confirmation as the former Worker workflow.
- [x] Integrity checks, abort, abandonment, validation, and finalization preserve Agent attribution without weakening fail-closed behavior.
- [x] Task and audit history retain all prior assignment identities and recovery evidence.
- [x] No Agent table or independently mutable Agent state is added.
- [x] Core project tests and compiled CLI workflows use the Agent form while temporary compatibility forms remain green for unmigrated callers.

## Migrate diagnostics and generated guidance to Agents

Status: `ready-for-human`

**What to build:** Make every Go observability and instruction surface describe Orchestrator Agents and Agents consistently, while deriving that presentation from the existing authoritative Task evidence.

**Blocked by:** Expand the Go Agent vocabulary.

- [x] One-shot debug snapshots expose derived Agents and Agent identities through the new vocabulary.
- [x] Human debug output and live watch records use Agent terminology without changing collection, redaction, or stability semantics.
- [x] Diagnostic evidence, affected identities, and suggested commands use complete Agent-oriented forms.
- [x] The configuration generator uses the new Agent setting names and clearly distinguishes the Orchestrator Agent from executing Agents.
- [x] Generated orchestration guidance preserves the sole-orchestrator authority rule and prevents Agents from spawning or orchestrating peers.
- [x] Generated debugging guidance continues to state that Agents are derived rather than persisted entities.
- [x] CLI usage and error prose use Agent terminology for assignment, recovery, repair, and termination evidence.
- [x] Debug, watch, configuration, generated-skill, and compiled CLI tests cover the new forms while expansion compatibility remains available.

## Migrate batch workflows to Batch Tasks

Status: `ready-for-human`

**What to build:** Make the complete Go Batch lifecycle operate on ordered Batch Tasks rather than Members, preserving barrier immutability, ownership attribution, and deterministic finalization.

**Blocked by:** Expand the Go Batch Task vocabulary.

- [x] Batch collection and inspection expose ordered Batch Tasks, Task status, and Task outcomes through the new Go model.
- [x] Freeze closes the ordered Batch Task set and prevents later Tasks from entering the frozen Batch.
- [x] Manifest construction and validation continue to associate every path with exactly one Batch Task.
- [x] Repair reopens only original owning Tasks and retains the frozen Batch Task set and order.
- [x] Abandonment, integrity quarantine, and recovery preserve immutable Batch Task evidence.
- [x] Validation and finalization retain deterministic Task order and attributable commits.
- [x] Human Batch output describes Task counts rather than Member counts.
- [x] Batch, repair, abandonment, validation, finalization, and incident-recovery tests use the new vocabulary while temporary compatibility remains green.

## Cut over the Go v2 contract and persisted vocabulary

Status: `ready-for-human`

**What to build:** Complete the breaking Go rename with an atomic state migration, versioned Agent and Batch Task contracts, Agent-oriented CLI/configuration, and removal of every temporary Worker and Member compatibility form.

**Blocked by:** Migrate task execution and recovery to Agents; Migrate diagnostics and generated guidance to Agents; Migrate batch workflows to Batch Tasks.

- [x] Existing state automatically migrates persisted Worker identity to Agent identity and ordered Batch Members to ordered Batch Tasks.
- [x] Migration preserves active and terminal Sessions, Tasks, ordering, Leases, Claims, submissions, snapshots, repair evidence, integrity evidence, manifests, and audit history.
- [x] Migration is atomic, verifies foreign-key integrity, and leaves the original state usable if migration cannot complete.
- [x] Migrated state remains operational through public inspect, debug, Task, Batch, recovery, validation, and finalization workflows.
- [x] The Go JSON envelope advances to schema version 2; changed debug snapshots and watch streams advance their own contract versions coherently.
- [x] Public output contains `agent`, `agents`, `agent_identity`, `tasks`, and `task_order` as appropriate and contains no retired Worker, Member, or membership aliases.
- [x] Only Agent-oriented CLI options remain; retired Worker-oriented spellings fail normally rather than acting as hidden aliases.
- [x] The tracked configuration format advances to Agent settings; an older file is not silently rewritten and receives actionable regeneration and re-approval guidance.
- [x] Version reporting and release-facing compatibility text identify the new breaking contract.
- [x] Compiled CLI contract and migration tests cover both successful and forced-failure paths, and the complete Go suite is green at contraction.

## Deliver the responsive task-first dashboard

Status: `ready-for-human`

**What to build:** Replace the static Go status dump with a read-only, navigable Tasks dashboard that prioritizes operational meaning and remains usable across wide, medium, and narrow terminals.

**Blocked by:** Add compiled TUI interaction coverage; Cut over the Go v2 contract and persisted vocabulary.

- [x] Tasks are the initial view and are grouped as Needs attention, In progress, Ready for batch, Waiting, and Finished according to the specification.
- [x] Exact authoritative Task state remains visible in the selected Task's details.
- [x] Groups are ordered by operational urgency and Tasks retain stable creation order within each group.
- [x] Wide terminals show the Task list and details side by side; medium terminals open full-width details; narrow terminals use compact rows.
- [x] Below the practical minimum size, the dashboard renders a clear size requirement rather than malformed content.
- [x] Critical warnings and supported next commands remain reachable through scrolling or details at every supported size.
- [x] `up`/`down` and `j`/`k` move selection, `Enter` opens details, `Esc` backs out, `?` toggles help, `r` refreshes, and `q` quits.
- [x] The footer presents only controls relevant to the current view and shows last-update age and refresh state.
- [x] The compiled pseudo-terminal tests cover the layouts and controls; observing the dashboard remains read-only.

## Add stable live inspection and filtering

Status: `ready-for-human`

**What to build:** Let operators investigate live Tasks without losing their place as snapshots change, using stable selection, cross-field filtering, rich Task details, and navigable dependency relationships.

**Blocked by:** Deliver the responsive task-first dashboard.

- [x] Selection follows stable Task ID across refreshes and never silently moves to a different Task because row positions changed.
- [x] Active tab, filter, selection, scroll position, and open detail state survive automatic and manual refreshes.
- [x] When a selected Task disappears, the nearest remaining Task is selected deterministically and a brief explanation is shown.
- [x] Newly urgent Tasks are marked without stealing keyboard focus.
- [x] `/` opens a case-insensitive substring filter and `Esc` clears it when filtering is active.
- [x] Task filtering matches title, ID, friendly group, exact state, Agent identity, owned paths, and Batch ID and displays the match count.
- [x] Task details show Agent, owned files, Lease state, assignment expiry, Batch, and latest relevant diagnostic.
- [x] Task details show `Blocked by` and `Unlocks` relationships, and selecting a referenced dependency jumps to that Task.
- [x] Filtering is retained while details are open and is scoped to the active view.
- [x] Compiled pseudo-terminal scenarios exercise refresh mutation, filtering, disappearance, urgency, details, and dependency navigation.

## Add operational Agent, Batch, and Diagnostic views

Status: `ready-for-human`

**What to build:** Give operators navigable Agent, Batch, and Diagnostic perspectives with an attention-first health model and safe, evidence-led next steps.

**Blocked by:** Add stable live inspection and filtering.

- [x] `left`/`right` and `h`/`l` navigate Tasks, Agents, Batches, and Diagnostics tabs while retaining per-tab selection and scroll state.
- [x] The Agents tab shows each derived Agent's active Task, Lease state, assignment expiry, owned files, and latest activity without implying persisted Agent state.
- [x] The Batches tab shows exact status, ordered Batch Tasks, path count, validation state, and finalization context.
- [x] The Diagnostics tab and persistent health banner rank integrity violations above error diagnostics, error diagnostics above blocked work, and blocked work above ordinary progress.
- [x] Each actionable diagnostic presents a plain-language title, exact code, severity, impact, evidence, and one supported next command when available.
- [x] Suggested commands are selectable terminal text but are never executed and do not access the clipboard automatically.
- [x] Filtering applies to the active tab and uses the identifiers and visible evidence relevant to that tab.
- [x] Stable-ID selection and refresh preservation work consistently for Agents, Batches, and Diagnostics.
- [x] Compiled pseudo-terminal tests cover tab navigation, attention ordering, details, filtering, and the absence of mutation side effects.

## Complete guided states, accessibility, and release readiness

Status: `ready-for-human`

**What to build:** Finish the Go experience with guided lifecycle screens, non-color accessibility, canonical documentation, and end-to-end verification of the complete breaking release.

**Blocked by:** Add operational Agent, Batch, and Diagnostic views.

- [x] Uninitialized state briefly explains Bandmaster and presents initialization as the single primary next step.
- [x] Unapproved configuration explains the trust boundary and presents the supported inspection and approval path.
- [x] Ready-without-session, paused, quarantined, and completed states each explain their meaning and present one safe primary next step.
- [x] `NO_COLOR` removes ANSI color while text and symbols continue to communicate selection, status, severity, and freshness.
- [x] Color-enabled output never relies on color alone and remains readable at all supported responsive sizes.
- [x] Root product documentation, specifications, examples, and generated instructions consistently use Orchestrator Agent, Agent, Task, and Batch Task vocabulary.
- [x] Go is documented as canonical; the unchanged Rust port is labeled experimental and potentially different in vocabulary and JSON contract.
- [x] Documentation and examples contain no obsolete Go Worker or Batch Member commands, fields, or configuration names except explicit migration notes.
- [x] Fresh-executable verification covers state migration, JSON v2, both live-dashboard entry points, navigation, refresh stability, filtering, all tabs, guided states, and read-only guarantees.
- [x] The complete Go test suite passes, and no Rust implementation files are modified by this effort.
