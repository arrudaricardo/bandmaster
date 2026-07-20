# Domain Docs

This repository uses a single-context domain-documentation layout.

## Before exploring

- Read `CONTEXT.md` at the repository root when it exists.
- Read relevant architectural decisions under `docs/adr/` when they exist.
- If either location does not exist, proceed silently. Domain-modeling workflows create these documents when terminology or architectural decisions need to be recorded.

## File structure

The repository may contain one root `CONTEXT.md` glossary and system-wide ADRs under `docs/adr/`.

## Vocabulary

Use terms as defined in `CONTEXT.md` in issue titles, specifications, hypotheses, test names, and implementation work. Do not replace defined terms with synonyms. Treat an undefined concept as either a vocabulary mistake to reconsider or a gap to record through domain modeling.

## Architectural decisions

Surface any conflict with an existing ADR explicitly rather than silently overriding it.
