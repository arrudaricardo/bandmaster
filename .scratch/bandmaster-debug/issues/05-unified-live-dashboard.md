# Unify the live terminal dashboard

Status: `ready-for-human`

## What to build

Present the shared debug model as the canonical live human dashboard while preserving the existing `bandmaster tui` entry point as a compatibility alias.

## Acceptance criteria

- [x] `bandmaster debug --watch` renders an interactive live view from the same normalized snapshot and diagnostics used by JSON output.
- [x] The dashboard exposes session and runtime identity, worker/task/claim/lease state, batches, monitor and integrity health, Git observations, and actionable diagnostics without exposing redacted secrets.
- [x] `bandmaster tui` invokes the same live behavior and no longer maintains a separate snapshot-loading path or divergent model.
- [x] Existing refresh, manual refresh, resize, and clean-exit behavior remains supported.
- [x] CLI-visible tests prove command compatibility, while focused renderer tests cover only terminal presentation that cannot be asserted reliably at the integration seam.

## Blocked by

- 04 — Stream live diagnostic changes.

## Comments
