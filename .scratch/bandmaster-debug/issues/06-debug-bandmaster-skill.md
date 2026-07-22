# Teach Codex to debug Bandmaster

Status: `ready-for-human`

## What to build

Install a dedicated repository-local `debug-bandmaster` skill that makes Codex use structured runtime evidence whenever it is asked to diagnose or improve Bandmaster.

## Acceptance criteria

- [x] The repository-local skill is named `debug-bandmaster`, includes UI metadata, and triggers for requests to debug, diagnose, inspect, troubleshoot, or explain Bandmaster runtime behavior.
- [x] The workflow begins with sanitized `bandmaster debug --json`, interprets diagnostics and entity relationships, checks runtime/build identity, and correlates evidence with source and tests.
- [x] The workflow uses JSON watch mode for changing reproductions and distinguishes diagnosis-only requests from requests that authorize implementation.
- [x] After an authorized fix, the workflow rebuilds Bandmaster, reproduces the behavior, and verifies it with a fresh diagnostic snapshot so an old installed binary cannot provide false confidence.
- [x] `bandmaster init --debug-skill` installs or updates the dedicated debugging skill deterministically; default init neither creates nor modifies it, and its prompt remains separate from the general Bandmaster orchestration skill.
- [x] The skill folder passes the standard skill validator, and CLI integration tests assert generated content, triggering language, separation from the orchestration skill, and the documented diagnostic commands.

## Blocked by

- 04 — Stream live diagnostic changes.

## Comments
