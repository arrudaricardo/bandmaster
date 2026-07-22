use serde_json::{json, Value};
use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Command, Output};
use std::time::{SystemTime, UNIX_EPOCH};

struct Workspace(PathBuf);

impl Workspace {
    fn new() -> Self {
        let nonce = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let path = std::env::temp_dir().join(format!(
            "bandmaster-rust-e2e-{}-{nonce}",
            std::process::id()
        ));
        fs::create_dir_all(&path).expect("create workspace");
        Self(path)
    }
}

impl Drop for Workspace {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

fn invoke(workspace: &Path, args: &[&str]) -> Output {
    Command::new(env!("CARGO_BIN_EXE_bandmaster"))
        .args(args)
        .current_dir(workspace)
        .output()
        .expect("run bandmaster")
}

fn success(workspace: &Path, args: &[&str]) -> Value {
    let output = invoke(workspace, args);
    assert!(
        output.status.success(),
        "command failed: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    let value: Value = serde_json::from_slice(&output.stdout).expect("JSON response");
    assert_eq!(value["schema_version"], "1");
    assert_eq!(value["success"], true);
    value
}

#[test]
fn runs_core_lifecycle_through_the_binary() {
    let workspace = Workspace::new();
    let session = success(&workspace.0, &["session", "start", "--json"]);
    assert_eq!(session["result"]["status"], "active");

    let task = success(
        &workspace.0,
        &[
            "task",
            "create",
            "--title",
            "Rust core",
            "--intent",
            "exercise lifecycle",
            "--expected-outcome",
            "committed task",
            "--json",
        ],
    );
    let task_id = task["result"]["id"].as_str().expect("task id");
    assert!(task["result"].get("assignment_token").is_none());

    let assignment = success(
        &workspace.0,
        &["task", "assign", task_id, "--worker", "worker-1", "--json"],
    );
    let token = assignment["result"]["assignment_token"]
        .as_str()
        .expect("assignment token");
    let validation = json!({
        "name": "focused",
        "argv": ["cargo", "test"],
        "working_directory": ".",
        "timeout": "1m"
    })
    .to_string();
    success(
        &workspace.0,
        &[
            "task",
            "claim",
            task_id,
            "--token",
            token,
            "--path",
            "src/lib.rs",
            "--validation",
            &validation,
            "--json",
        ],
    );
    success(
        &workspace.0,
        &[
            "task",
            "submit",
            task_id,
            "--token",
            token,
            "--behavior-changed",
            "core lifecycle",
            "--key-decisions",
            "simple state",
            "--validation-expectations",
            "focused test",
            "--known-risks",
            "initial port",
            "--json",
        ],
    );

    assert_eq!(
        success(&workspace.0, &["batch", "freeze", "--json"])["result"]["status"],
        "frozen"
    );
    assert_eq!(
        success(&workspace.0, &["batch", "validate", "--json"])["result"]["status"],
        "validated"
    );
    assert_eq!(
        success(&workspace.0, &["batch", "commit", "--json"])["result"]["status"],
        "committed"
    );
    assert_eq!(
        success(&workspace.0, &["session", "finish", "--json"])["result"]["status"],
        "completed"
    );
    let inspected = success(&workspace.0, &["task", "inspect", task_id, "--json"]);
    assert!(inspected["result"].get("assignment_token").is_none());
}

#[test]
fn unsupported_commands_return_structured_errors() {
    let workspace = Workspace::new();
    let output = invoke(&workspace.0, &["debug", "--json"]);
    assert_eq!(output.status.code(), Some(3));
    let value: Value = serde_json::from_slice(&output.stdout).expect("JSON response");
    assert_eq!(value["success"], false);
    assert_eq!(value["error"]["code"], "unsupported_command");
}
