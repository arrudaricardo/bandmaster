# Agent-Centered Go TUI and Domain Vocabulary

Status: `ready-for-human`

## Problem Statement

Bandmaster's Go terminal dashboard exposes a large amount of orchestration state, but it presents that state as a static status dump rather than helping an operator understand what needs attention and what to inspect next. Tasks, agents, batches, diagnostics, repository state, and health signals appear in one long view. The view is not navigable, does not adapt well to different terminal sizes, truncates the working set at fixed limits, and shows implementation-oriented status names without a friendlier operational grouping. Automatic refresh can also make a future interactive selection unstable unless identity and ordering are handled deliberately.

The Go implementation also uses misleading domain language. It calls executing AI agents "workers" and calls tasks included in a batch "members." This obscures the durable model: an agent executes a task, but the task remains in its batch if the agent disappears, is recovered, or is replaced. Renaming a batch member to an agent would make that distinction worse. The terminology is present throughout Go models, persistence, JSON, CLI options and output, diagnostics, generated skills, tests, and product documentation, so a cosmetic-only adjustment would leave the product internally inconsistent.

The user needs a friendlier, attention-oriented Go TUI and a coherent Go vocabulary in which an Orchestrator Agent coordinates Agents, Agents execute Tasks, and Batches contain Tasks. The change may break the Go API and CLI contract, but it must automatically preserve existing runtime state. The Rust port is not part of this work.

## Solution

Redesign the Go live dashboard as a responsive, read-only, task-first operations view. The main Tasks tab will group work into friendly operational states, place items needing attention first, preserve exact authoritative statuses in details, and provide a selectable list with a contextual detail panel. Secondary tabs will expose Agents, Batches, and Diagnostics. Operators will be able to navigate, filter, inspect dependencies and owned files, open contextual help, and copy or manually run suggested commands without allowing the TUI itself to mutate Bandmaster state.

The dashboard will adapt between side-by-side and single-panel layouts according to available terminal space. It will preserve selection, filters, tabs, details, and scroll position across refreshes by tracking stable domain IDs. Health and integrity problems will be prioritized in plain language, while exact diagnostic codes and authoritative states remain visible for troubleshooting. Guided screens will explain initialization, approval, inactive, paused, quarantined, and completed states with one primary next step.

Perform a breaking vocabulary migration throughout the Go implementation. Rename the `Worker` family to `Agent`, represent batch membership as `BatchTask`, and rename corresponding persistence columns, CLI flags, JSON fields, diagnostics, generated instructions, and documentation. Keep the precise domain terms Task, Batch, Claim, Lease, Session, and Manifest. In the human TUI, claims may be labeled "Owned files" and lease expiry may be labeled "Assignment expires," but their technical Go names and safety semantics remain unchanged.

Agents will remain a derived diagnostic and presentation view assembled from authoritative task assignment, lease, and claim evidence. No persisted Agent entity will be introduced. Existing Go state databases will migrate automatically from the old vocabulary without losing sessions, tasks, batch ordering, ownership evidence, submissions, or audit history. The Go JSON contract will advance to a new major schema version and will not emit legacy aliases. Root product documentation will describe Go as canonical and label the unchanged Rust implementation as an experimental port with a potentially different vocabulary and contract.

## User Stories

1. As a Bandmaster operator, I want the live dashboard organized around tasks, so that I can understand the work rather than decode storage concepts.
2. As a Bandmaster operator, I want tasks needing attention placed first, so that failures and blockers are hard to miss.
3. As a Bandmaster operator, I want active work separated from waiting and finished work, so that progress is understandable at a glance.
4. As a Bandmaster operator, I want friendly lifecycle groups, so that I do not need to memorize every internal task state.
5. As a Bandmaster maintainer, I want exact authoritative states visible in task details, so that friendly grouping does not reduce diagnostic precision.
6. As a Bandmaster operator, I want a session health banner, so that the most important current condition is immediately visible.
7. As a Bandmaster operator, I want integrity violations to outrank ordinary blockers, so that safety-critical conditions receive the right attention.
8. As a Bandmaster operator, I want diagnostics described in plain language, so that I can understand their impact before reading an internal code.
9. As a Bandmaster maintainer, I want exact diagnostic codes retained, so that support and automation can refer to stable identifiers.
10. As a Bandmaster operator, I want one recommended next command for an actionable problem, so that I know the supported path forward.
11. As a security-conscious operator, I want the dashboard to remain read-only, so that exploration cannot mutate orchestration or Git state.
12. As a security-conscious operator, I want suggested commands displayed rather than executed, so that consequential actions remain explicit CLI operations.
13. As a Bandmaster operator, I want a Tasks tab, so that I can navigate the durable unit of work.
14. As a Bandmaster operator, I want an Agents tab, so that I can inspect which AI agents are active and what they own.
15. As a Bandmaster operator, I want a Batches tab, so that I can inspect barrier and finalization progress independently of task execution.
16. As a Bandmaster operator, I want a Diagnostics tab, so that I can focus on actionable health evidence.
17. As a Bandmaster operator, I want the selected task's details beside the task list on a wide terminal, so that inspection does not hide context.
18. As a Bandmaster operator, I want details to open as a full-width view on a smaller terminal, so that content remains readable.
19. As a Bandmaster operator, I want compact rows on a narrow terminal, so that the dashboard remains usable without unsafe truncation.
20. As a Bandmaster operator, I want an explicit minimum-size message only when no useful layout fits, so that small terminals fail clearly.
21. As a Bandmaster operator, I want critical warnings and suggested commands to remain available when space is constrained, so that responsive layout does not conceal safety information.
22. As a Bandmaster operator, I want arrow keys and `j`/`k` to move selection, so that navigation works for conventional terminal users.
23. As a Bandmaster operator, I want arrow keys and `h`/`l` to change tabs, so that switching perspectives is quick.
24. As a Bandmaster operator, I want `Enter` to open or focus details, so that inspection is discoverable.
25. As a Bandmaster operator, I want `Esc` to close details or clear a filter, so that backing out behaves consistently.
26. As a Bandmaster operator, I want `?` to show contextual help, so that I can discover controls without leaving the dashboard.
27. As an existing Bandmaster user, I want `r` to refresh and `q` to quit, so that established controls remain familiar.
28. As a Bandmaster operator, I want `/` to start filtering, so that I can find relevant records in a busy session.
29. As a Bandmaster operator, I want task filters to match title and ID, so that I can find known work quickly.
30. As a Bandmaster operator, I want task filters to match friendly and exact status, so that I can search using either vocabulary.
31. As a Bandmaster operator, I want task filters to match agent identity, owned paths, and batch ID, so that related evidence can be found from any identifier I have.
32. As a Bandmaster operator, I want filtering to be case-insensitive substring matching, so that simple searches are predictable.
33. As a Bandmaster operator, I want the active tab's match count visible, so that I know whether filtering hid records.
34. As a Bandmaster operator, I want the filter retained while viewing details, so that I can inspect several matching records efficiently.
35. As a Bandmaster operator, I want automatic refresh to preserve my selected domain object, so that live updates do not interrupt inspection.
36. As a Bandmaster operator, I want automatic refresh to preserve the active tab, filter, detail state, and scroll position, so that the interface remains stable.
37. As a Bandmaster operator, I want selection tracked by stable ID rather than row number, so that reordered data does not select the wrong object.
38. As a Bandmaster operator, I want a clear explanation when my selected object disappears, so that a lifecycle transition is not mistaken for a navigation bug.
39. As a Bandmaster operator, I want newly urgent records marked without stealing focus, so that I notice problems without losing my place.
40. As a Bandmaster operator, I want records ordered by operational urgency and then stable creation order, so that refreshes do not cause unnecessary movement.
41. As a Bandmaster operator, I want to see when the dashboard was last updated, so that I can judge the freshness of displayed state.
42. As a Bandmaster operator, I want a subtle refresh indicator, so that data collection is visible without distracting from the work.
43. As a Bandmaster operator, I want task details to show the assigned Agent, so that execution responsibility is clear.
44. As a Bandmaster operator, I want task details to show owned files, so that file-level coordination is understandable.
45. As a Bandmaster operator, I want task details to show lease state and assignment expiry, so that abandoned ownership can be distinguished from active work.
46. As a Bandmaster operator, I want task details to show prerequisites and unlocked tasks, so that dependency waiting is explainable.
47. As a Bandmaster operator, I want to jump from a dependency reference to that task, so that dependency exploration does not require a separate graph.
48. As a Bandmaster operator, I want task details to show batch membership and the latest relevant diagnostic, so that execution and barrier state can be correlated.
49. As a Bandmaster operator, I want an uninitialized screen that explains Bandmaster and shows the initialization command, so that setup has an obvious first step.
50. As a Bandmaster operator, I want an unapproved-configuration screen that explains the trust boundary and inspection step, so that approval is informed.
51. As a Bandmaster operator, I want a ready-without-session screen that shows repository readiness and the start command, so that beginning work is straightforward.
52. As a Bandmaster operator, I want paused and quarantined screens that explain why progress stopped, so that safety behavior is not mistaken for a crash.
53. As a Bandmaster operator, I want a completed screen that summarizes the outcome and offers an inspection or restart path, so that terminal sessions have a useful conclusion.
54. As a color-blind operator, I want icons and text to carry all meaning, so that color is never the only status signal.
55. As a terminal user, I want `NO_COLOR` respected, so that Bandmaster follows my accessibility preference.
56. As an Agent author, I want executing AI processes called Agents in the Go contract, so that the terminology matches the product domain.
57. As an Agent author, I want an Agent's identity exposed as `agent_identity`, so that JSON no longer describes an AI agent as a worker.
58. As an Agent author, I want Agent collections exposed as `agents`, so that machine consumers use the same vocabulary as operators.
59. As a Bandmaster maintainer, I want batches to contain Batch Tasks rather than Members or Agents, so that durable task membership remains distinct from replaceable execution.
60. As a Bandmaster maintainer, I want batch JSON to expose `tasks` and `task_order`, so that the public contract describes what is actually persisted.
61. As an Orchestrator Agent, I want CLI assignment and recovery options to use Agent terminology, so that generated commands and documentation are consistent.
62. As an Orchestrator Agent, I want generated orchestration instructions to distinguish the Orchestrator Agent from executing Agents, so that authority boundaries remain clear.
63. As a Bandmaster maintainer, I want Task, Batch, Claim, Lease, Session, and Manifest retained as technical terms, so that accurate safety concepts are not renamed for appearance.
64. As a Bandmaster maintainer, I want claims labeled as Owned files only in human presentation, so that usability improves without changing claim semantics.
65. As a Bandmaster maintainer, I want Agents derived from task, lease, and claim evidence, so that the redesign does not invent a competing persisted lifecycle.
66. As a Bandmaster maintainer, I want no Agent database table, so that recovery remains anchored to durable Tasks and ownership evidence.
67. As an existing Go user, I want my state database migrated automatically, so that a vocabulary change does not discard active or historical work.
68. As an existing Go user, I want sessions, task ordering, batch ordering, claims, submissions, leases, snapshots, and audit events preserved during migration, so that attribution remains trustworthy.
69. As an automation author, I want the Go JSON schema version advanced for the breaking contract, so that incompatible responses are detectable.
70. As an automation author, I want old Go JSON field aliases removed, so that there is one unambiguous vocabulary after the major change.
71. As an automation author, I want human output, one-shot debug output, watch streams, and the TUI to share the same Agent-centered model, so that Bandmaster does not expose divergent terminology.
72. As a Go user, I want root documentation to describe the canonical Go product, so that examples agree with the installed implementation.
73. As a Rust-port user, I want an explicit compatibility warning, so that I do not assume Rust and Go vocabulary or JSON are interchangeable.
74. As a Bandmaster maintainer, I want the Rust implementation left unchanged, so that the Go redesign does not expand into an incomplete cross-language migration.
75. As a Bandmaster maintainer, I want existing Go safety and orchestration behavior preserved, so that a presentation and vocabulary redesign does not weaken coordination guarantees.

## Implementation Decisions

- The Go implementation is the canonical product for this specification. The Rust port remains unchanged and will be described as experimental and potentially contract-incompatible.
- Preserve `bandmaster debug --watch` as the canonical live human dashboard and `bandmaster tui` as its compatibility alias. Both entry points must use one normalized debug model and one interactive renderer.
- Keep the TUI strictly read-only. Navigation, filtering, detail inspection, help, and manual refresh are allowed; session control, recovery, validation, finalization, and any other mutations remain explicit CLI commands.
- Organize the TUI into Tasks, Agents, Batches, and Diagnostics tabs. Tasks are the initial and primary tab.
- Use a task-first main view with a top-level session health summary, a selectable task list, contextual details, and a persistent help/status footer.
- Group Task states for presentation as follows: `planned` and `ready` are Waiting; `assigned` and `editing` are In progress; `submitted` is Ready for batch; `blocked`, `repair_pending`, and `quarantined` are Needs attention; `committed`, `no_op`, and `canceled` are Finished. Preserve and display the exact state in details.
- Sort task groups by operational priority, with Needs attention first, followed by In progress, Ready for batch, Waiting, and Finished. Within a group, retain stable creation order.
- Make the layout responsive. Wide terminals use a list and detail panel side by side; medium terminals use a full-width list with a separate detail view; narrow terminals use compact rows. Below a practical minimum, render an explicit minimum-size explanation rather than corrupted output.
- Never make critical warnings or suggested commands permanently inaccessible because of terminal dimensions. Scrolling or a focused detail view may be used when content does not fit.
- Support `up`/`down` and `j`/`k` for selection, `left`/`right` and `h`/`l` for tabs, `Enter` for details, `/` for filtering, `Esc` for backing out or clearing a filter, `r` for refresh, `?` for help, and `q` for quit.
- Do not add mouse input or configurable keybindings in this iteration.
- Apply a case-insensitive substring filter to only the active tab. Task filtering searches title, ID, friendly state, exact state, Agent identity, claim paths, and Batch ID. Show a match count and retain the query while details are open.
- Track selection by stable Task, Agent, Batch, or Diagnostic identity. Preserve active tab, filter, selection, scroll position, and open details across refreshes. If an item disappears, select the nearest remaining item and briefly explain the change.
- Mark newly urgent records without moving keyboard focus. Show both last-update age and active refresh state.
- Use an attention-first health hierarchy: integrity violations outrank error diagnostics, error diagnostics outrank blocked work, and blocked work outranks ordinary progress information.
- Present diagnostics with a plain-language title, exact code, severity, impact, evidence, and one recommended supported command when available. Commands remain selectable text and are never executed or copied to the clipboard automatically.
- Do not add a terminal dependency graph. Show `Blocked by` and `Unlocks` relationships in Task details and allow navigation to a referenced Task.
- Provide guided screens for uninitialized, unapproved configuration, ready without a session, paused, quarantined, and completed states. Each screen explains the state and presents one primary safe next step.
- Respect `NO_COLOR`. Combine text and symbols so that status, selection, severity, and freshness never rely on color alone.
- Perform the breaking rename only in Go. Rename Go types, fields, functions, variables, CLI flags, configuration keys, JSON fields, human output, error messages, diagnostic evidence, generated skills, tests, and Go-facing documentation in the `Worker` family to the corresponding `Agent` vocabulary.
- Use `Orchestrator Agent` for the sole coordinating actor and `Agent` for an executing AI agent. Do not use `Agent` for a Task included in a Batch.
- Rename the Go Batch `Member` family to `BatchTask`. Public batch collections become `tasks`; ordering becomes `task_order`; human output says Batch tasks rather than members.
- Keep Task, Batch, Claim, Lease, Session, and Manifest unchanged in the technical domain. The TUI may render claims as Owned files and lease expiry as Assignment expires.
- Agents remain a derived view grouped from authoritative Task assignment identity, lease, claim, and activity evidence. Do not create a persisted Agent entity or an independent Agent lifecycle.
- Rename persisted Go columns and tables whose names encode Worker or Member vocabulary, including task and audit Agent identity, Batch Tasks, and Task order. Update all related constraints, indexes, joins, migrations, diagnostics, and integrity checks coherently.
- Automatically migrate existing Go state databases in one verified migration. Preserve every domain record and ordering relationship, maintain foreign-key integrity, and fail without committing a partial migration if verification fails.
- Advance the Go public JSON envelope schema to version 2. Advance the normalized debug contract and live stream contract wherever their shapes change. Do not emit deprecated `worker`, `workers`, `member`, `members`, `worker_identity`, or `membership_order` aliases.
- Rename Go CLI options that encode Worker terminology to Agent terminology, including assignment, recovery, and repair options. Old option spellings are not compatibility aliases after the breaking release.
- Advance the tracked Go configuration format for renamed Agent settings. Do not silently rewrite a tracked configuration file. An older configuration receives an actionable incompatibility message and may be regenerated and re-approved through the existing initialization and approval workflow.
- Update root product documentation, specifications, examples, generated orchestration instructions, and generated debugging instructions to the canonical vocabulary. Clearly label Rust-specific behavior and documentation as experimental and potentially different.
- Preserve the current orchestration state machine, claim exclusivity, lease behavior, recovery authority, batch barriers, Git safety, redaction, and read-only diagnostic guarantees except where names or presentation explicitly change.

## Testing Decisions

- Treat externally observable behavior as the contract. Tests should assert what a user or automation consumer sees through the compiled Go CLI, not private rendering helpers, SQL implementation details, or transient internal struct layout.
- Use the compiled Go `bandmaster` CLI in temporary Git repositories as the primary behavioral seam. Extend the existing integration harness with pseudo-terminal support so the same executable can be tested with real terminal dimensions and keyboard input.
- Through the pseudo-terminal seam, cover wide, medium, narrow, and below-minimum layouts; initial focus; movement; tab switching; opening and closing details; filtering; help; manual refresh; automatic refresh; scrolling; and clean exit.
- Assert selection stability by arranging semantic changes between refreshes and proving that selection follows the stable domain ID rather than the previous row index.
- Assert that newly urgent items are surfaced without taking focus and that removal of a selected item produces a deterministic fallback and explanation.
- Cover task grouping and ordering with every Task state, including blocked, repair-pending, quarantined, submitted, no-op, and canceled cases. Assert both friendly grouping and exact-state availability.
- Cover guided views for uninitialized, unapproved, no-session, paused, quarantined, and completed states through public repository and CLI workflows.
- Cover attention hierarchy using real normalized diagnostics and integrity evidence rather than renderer-only fixtures wherever practical.
- Assert that displayed suggested commands are never executed and that observing the TUI does not change repository files, Git state, leases, sessions, tasks, batches, or audit history.
- Run terminal scenarios with normal color and `NO_COLOR`, asserting that plain text and symbols still communicate status and severity without ANSI color.
- Use focused renderer or model tests only for presentation details that cannot be asserted reliably through a pseudo-terminal, such as exact boundary calculations or deterministic truncation. Keep these tests subordinate to the compiled-CLI seam.
- Add compiled-CLI JSON contract tests covering Task, Batch, debug snapshot, debug watch stream, diagnostics, errors, and version reporting. Assert schema version 2, new Agent and Batch Task fields, and absence of all retired aliases.
- Add compiled-CLI tests proving new Agent-oriented flags and configuration names work and retired Worker-oriented spellings fail with normal unknown-option or incompatible-configuration behavior.
- Add database migration integration tests that begin from a representative database produced by the pre-rename Go release. Exercise active and terminal sessions, assigned Tasks, Agent identity history, active and closed leases, claims, Batch Tasks, frozen manifests, submissions, repair evidence, integrity evidence, and audit history.
- After migration, exercise public inspect, debug, task, batch, recovery, and finalization operations to prove migrated state is operational rather than merely readable.
- Verify migration atomicity and foreign-key integrity. A forced migration failure must leave the original database usable and must not expose a partially renamed schema.
- Preserve and update the repository's existing compiled CLI integration tests, TUI renderer tests, debug integration tests, Batch tests, ownership migration tests, and safety tests as prior art.
- Run the complete Go test suite as the final regression gate. Rust tests are not an acceptance gate for renamed output because Rust is intentionally unchanged, but the work must not edit or accidentally break the Rust port.

## Out of Scope

- Mutating Bandmaster state from the TUI.
- Executing suggested recovery, validation, finalization, pause, resume, or abort commands from a keybinding.
- Automatic clipboard access.
- Mouse support.
- Configurable keybindings.
- Advanced query syntax or fuzzy matching.
- A graphical terminal dependency graph.
- A new persisted Agent entity or Agent lifecycle.
- Renaming Task, Batch, Claim, Lease, Session, or Manifest in the technical Go domain.
- Changing task, batch, lease, claim, recovery, integrity, validation, finalization, or Git safety semantics beyond the required schema and contract vocabulary migration.
- Preserving aliases for the retired Go JSON fields or CLI options.
- Silently modifying existing tracked configuration during state migration.
- Applying the rename or TUI redesign to the Rust port.
- Guaranteeing Go and Rust JSON compatibility after this breaking change.

## Further Notes

- The repository currently has no root domain glossary or architectural decision records defining competing terminology. This specification establishes the vocabulary needed for this feature: the Orchestrator Agent coordinates Agents; an Agent executes a Task; a Batch contains ordered Batch Tasks.
- The prior live-dashboard work established the normalized debug snapshot as the shared source for one-shot diagnostics, watch mode, and the `tui` alias. This redesign should deepen that existing seam rather than introduce a second presentation data model.
- The rename is intentionally breaking for Go automation consumers. The major schema marker and release notes must make the incompatibility obvious.
- Automatic state migration protects durable user evidence, while explicit configuration regeneration and re-approval preserve the existing trust model for tracked validation settings.
