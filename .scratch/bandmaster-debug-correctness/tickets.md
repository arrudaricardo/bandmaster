# Tickets: Correct Bandmaster debug health signals

These tickets implement the health-signal corrections specified in [Correct Bandmaster Debug Health Signals](PRD.md).

Work the **frontier**: the first three tickets can start independently; the final consistency ticket starts after all three are complete.

## Report coherent snapshot stability

Status: `ready-for-agent`

**What to build:** Make `bandmaster debug` identify an idle database and repository as one stable observation while preserving bounded retry and honest best-effort reporting when a real concurrent mutation occurs.

**Blocked by:** None — can start immediately.

- [ ] Consecutive snapshots of an idle initialized repository report complete, stable, non-best-effort collection.
- [ ] Database revision boundaries are captured through one pinned read-only connection and are meaningfully comparable across the coherent read.
- [ ] Database entities continue to be collected in one read transaction without mutation locks or orchestration side effects.
- [ ] A transient concurrent public mutation triggers the existing bounded retry and may produce a stable second observation.
- [ ] A persistently changing repository remains explicitly best effort rather than being mislabeled stable.
- [ ] Git revision changes continue to participate in the stability decision.
- [ ] Public CLI integration tests cover idle, transiently changing, and persistently changing observations without asserting private database layout.
- [ ] Existing redaction, partial-collection, and read-only guarantees remain intact.

## Correlate claimed files in new directories

Status: `ready-for-agent`

**What to build:** Make repository evidence enumerate new files at the same exact-path granularity as Bandmaster claims, so a fully claimed new directory is healthy and a genuinely unclaimed file remains actionable.

**Blocked by:** None — can start immediately.

- [ ] Git collection expands untracked directories into individual file paths.
- [ ] A task that claims and creates multiple files beneath a new directory receives no `unowned_worktree_drift` diagnostic.
- [ ] Adding one unclaimed file to that directory emits `unowned_worktree_drift` for only the exact unclaimed file.
- [ ] Ownership continues to derive from authoritative exact claims rather than inferred directory ownership.
- [ ] Tracked modifications, deletions, index changes, and rename destinations retain their existing representation and diagnostics.
- [ ] The diagnostic keeps its existing code, severity, evidence shape, and supported suggested actions for genuine violations.
- [ ] Compiled CLI integration tests exercise both the fully owned and mixed owned/unowned workflows.
- [ ] Debug collection remains strictly read-only and does not alter claims or Git state.

## Limit lease diagnostics to actionable tasks

Status: `ready-for-agent`

**What to build:** Report expiry and expiry warnings only for tasks that can still carry live worker ownership, while retaining historical lease evidence on terminal tasks without recommending impossible recovery actions.

**Blocked by:** None — can start immediately.

- [ ] An expired lease on an assigned or editing task still emits error-level `lease_expired` with the affected task and worker.
- [ ] A live lease approaching expiry still emits `lease_expiring` and a usable heartbeat suggestion.
- [ ] Canceled, committed, and no-op tasks do not receive actionable expiry or expiring diagnostics.
- [ ] Historical lease timestamps and status remain visible on terminal task snapshots.
- [ ] No debug collection path rewrites or normalizes persisted lease history.
- [ ] Recovery suggestions are emitted only when the task lifecycle supports recovery and the worker identity is present.
- [ ] No suggested command contains an empty worker identity.
- [ ] Compiled CLI integration tests cover live expired leases and terminal tasks with historical expired lease rows.
- [ ] Stable diagnostic codes and JSON compatibility remain unchanged for valid live findings.

## Keep diagnostic surfaces consistent

Status: `ready-for-agent`

**What to build:** Deliver the corrected snapshot semantics consistently through one-shot JSON and human output, watch mode, and the terminal dashboard, then verify the complete diagnostic workflow with a fresh executable.

**Blocked by:** Report coherent snapshot stability; Correlate claimed files in new directories; Limit lease diagnostics to actionable tasks.

- [ ] One-shot human and JSON output derive stability, ownership, and lease findings from the same corrected snapshot model.
- [ ] JSON watch snapshots, semantic changes, and health heartbeats reflect the corrected stability and diagnostic state.
- [ ] The terminal dashboard does not reintroduce independent ownership or lease classification logic.
- [ ] Debug command success remains distinct from reported runtime health: genuine unhealthy state remains a successful usable snapshot with diagnostics.
- [ ] Public schema versions, diagnostic codes, redaction, unsafe-mode boundaries, and session-selection behavior remain compatible.
- [ ] Existing watch-mode subprocess and terminal-rendering tests pass with the corrected model.
- [ ] The complete Go test suite passes.
- [ ] Verification builds a fresh Bandmaster executable from the implemented source and reproduces the three corrected scenarios with that exact executable.
- [ ] A stale user-level executable is not accepted as verification evidence.
