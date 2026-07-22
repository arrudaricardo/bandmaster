# Bandmaster Rust port

This directory contains a deliberately small Rust translation of Bandmaster.
It favors a clear SQLite-backed lifecycle over feature completeness or
performance. The original Go implementation remains the production reference.

Build and run it from the repository root:

```sh
cargo build --manifest-path rust-bandmaster/Cargo.toml --locked
cargo run --manifest-path rust-bandmaster/Cargo.toml -- version --json
```

The binary stores its independent state in `.bandmaster-rust/state.sqlite3` in
the current working directory, so it does not read or modify Go Bandmaster's
`.bandmaster/` orchestration state.

Implemented command flow:

```text
session start
  -> task create -> task assign -> task claim -> task submit
  -> batch freeze -> batch validate -> batch commit
  -> session finish
```

`session inspect`, `task list`, and `task inspect` are also available. Commands
accept the Go CLI's `--json`/`--pretty` envelope flags. `batch finalize` aliases
`batch commit`, and `task preflight` performs a lightweight non-mutating check.

Advanced recovery, diagnostics, configuration, monitoring, and interactive TUI
commands parse cleanly but return a structured `unsupported_command` error from
this initial port. Batch validation currently records a successful validation;
it does not execute repository-configured commands. Assignment tokens are only
returned by `task assign` and are redacted from all ordinary task responses.

Run all Rust checks with:

```sh
cargo fmt --check --manifest-path rust-bandmaster/Cargo.toml
cargo check --manifest-path rust-bandmaster/Cargo.toml --all-targets --all-features --locked
cargo test --manifest-path rust-bandmaster/Cargo.toml --all-targets --all-features --locked
```

