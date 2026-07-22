#[path = "../src/cli.rs"]
mod cli;

use cli::{parse, Envelope, ErrorPayload, EXIT_INTERNAL, EXIT_INVALID};
use serde_json::{json, Value};

#[test]
fn parses_public_command_families_and_aliases() {
    let cases = [
        (vec!["version"], "version", "version"),
        (vec!["doctor"], "doctor", "doctor"),
        (vec!["debug", "--watch"], "debug", "debug"),
        (vec!["init", "--debug-skill"], "init", "init"),
        (vec!["config", "status"], "config status", "config status"),
        (
            vec!["config", "approve", "sha256:abc"],
            "config approve",
            "config approve",
        ),
        (
            vec!["session", "inspect"],
            "session inspect",
            "session inspect",
        ),
        (vec!["batch", "freeze"], "batch freeze", "batch freeze"),
        (
            vec!["batch", "inspect", "batch_1"],
            "batch inspect",
            "batch inspect",
        ),
        (vec!["batch", "finalize"], "batch finalize", "batch commit"),
        (vec!["task", "list"], "task list", "task list"),
        (
            vec!["task", "inspect", "task_1"],
            "task inspect",
            "task inspect",
        ),
    ];

    for (args, expected, canonical) in cases {
        let parsed = parse(args).unwrap();
        assert_eq!(parsed.name(), expected);
        assert_eq!(parsed.canonical_name(), canonical);
    }
}

#[test]
fn parses_repeatable_claim_options_and_redacts_tokens() {
    let parsed = parse([
        "task",
        "claim",
        "task_1",
        "--token",
        "assignment_secret",
        "--path",
        "src/a.rs",
        "--path",
        "src/b.rs",
        "--validation",
        r#"{"name":"focused"}"#,
        "--json",
    ])
    .unwrap();

    assert!(parsed.json);
    assert!(!parsed.has_flag("--watch"));
    assert_eq!(parsed.positional(0), Some("task_1"));
    assert_eq!(parsed.option("--token"), Some("assignment_secret"));
    assert_eq!(parsed.options("--path"), ["src/a.rs", "src/b.rs"]);
    let debug = format!("{parsed:?}");
    assert!(debug.contains("[REDACTED]"));
    assert!(!debug.contains("assignment_secret"));
}

#[test]
fn enforces_required_options_and_rejects_unknown_or_duplicate_options() {
    let missing = parse(["task", "assign", "task_1"]).unwrap_err();
    assert_eq!(missing.command, "task assign");
    assert_eq!(missing.message, "option --worker is required");

    let unknown = parse(["session", "abort", "--force"]).unwrap_err();
    assert_eq!(unknown.message, "unknown option --force");

    let duplicate = parse([
        "task", "assign", "task_1", "--worker", "one", "--worker", "two",
    ])
    .unwrap_err();
    assert_eq!(
        duplicate.message,
        "option --worker may be specified only once"
    );
}

#[test]
fn validates_global_and_debug_flag_relationships() {
    assert_eq!(
        parse(["doctor", "--pretty"]).unwrap_err().message,
        "--pretty requires --json."
    );
    assert_eq!(
        parse(["debug", "--follow-latest", "--json"])
            .unwrap_err()
            .message,
        "--follow-latest requires --watch"
    );
    assert_eq!(
        parse(["debug", "--watch", "--pretty", "--json"])
            .unwrap_err()
            .message,
        "--pretty is not supported with --watch."
    );
}

#[test]
fn serializes_stable_success_and_error_envelopes() {
    assert_eq!((EXIT_INTERNAL, EXIT_INVALID), (1, 3));
    let success = Envelope::success(
        "version",
        None,
        json!({"version":"dev","json_schema_version":"1"}),
    );
    let success_value = serde_json::to_value(success).unwrap();
    assert_eq!(
        success_value,
        json!({
            "schema_version": "1",
            "command": "version",
            "success": true,
            "result": {"version":"dev","json_schema_version":"1"}
        })
    );

    let failure: Envelope<Value> = Envelope::failure(
        "task claim",
        Some("session_1".into()),
        ErrorPayload::new("invalid_arguments", "option --path is required", false),
    );
    let failure_json = serde_json::to_string(&failure).unwrap();
    assert_eq!(
        serde_json::from_str::<Value>(&failure_json).unwrap(),
        json!({
            "schema_version": "1",
            "command": "task claim",
            "success": false,
            "session_id": "session_1",
            "error": {
                "code": "invalid_arguments",
                "message": "option --path is required",
                "retryable": false
            }
        })
    );
    assert!(!failure_json.contains("token"));
}
