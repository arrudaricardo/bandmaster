use bandmaster::cli::{parse, Envelope, ErrorPayload, Invocation, EXIT_INTERNAL, EXIT_INVALID};
use bandmaster::project::Project;
use bandmaster::{BandmasterError, FocusedValidation, Task};
use rusqlite::Connection;
use serde_json::{json, Value};
use std::env;
use std::fs;
use std::path::Path;
use std::process::{self, Command};

const VERSION: &str = env!("CARGO_PKG_VERSION");
const LEASE_SECONDS: i64 = 300;

fn main() {
    let args: Vec<String> = env::args().skip(1).collect();
    let json_requested = args.iter().any(|argument| argument == "--json");
    let pretty_requested = args.iter().any(|argument| argument == "--pretty");
    let cwd = match env::current_dir() {
        Ok(cwd) => cwd,
        Err(error) => {
            emit_failure(
                "unknown",
                Failure::new("io_error", error.to_string(), EXIT_INTERNAL),
                json_requested,
                pretty_requested,
            );
            process::exit(EXIT_INTERNAL);
        }
    };

    let code = match parse(args) {
        Ok(invocation) => execute(&cwd, invocation),
        Err(error) => {
            emit_failure(
                &error.command,
                Failure::new("invalid_arguments", error.message, EXIT_INVALID),
                json_requested,
                pretty_requested,
            );
            EXIT_INVALID
        }
    };
    if code != 0 {
        process::exit(code);
    }
}

fn execute(cwd: &Path, invocation: Invocation) -> i32 {
    if invocation.name() == "version" {
        return emit_success(
            &invocation,
            None,
            json!({
                "version": VERSION,
                "json_schema_version": "1",
                "json_schema_compatibility": "Schema 1 fields are stable; additive fields may be introduced."
            }),
        );
    }

    if !is_supported(invocation.name()) {
        let failure = Failure::new(
            "unsupported_command",
            format!(
                "{} is not implemented by the simple Rust port",
                invocation.name()
            ),
            EXIT_INVALID,
        );
        emit_failure(
            invocation.name(),
            failure.clone(),
            invocation.json,
            invocation.pretty,
        );
        return failure.exit_code;
    }

    let state_path = cwd.join(".bandmaster-rust").join("state.sqlite3");
    if let Some(parent) = state_path.parent() {
        if let Err(error) = fs::create_dir_all(parent) {
            let failure = Failure::new("io_error", error.to_string(), EXIT_INTERNAL);
            emit_failure(
                invocation.name(),
                failure.clone(),
                invocation.json,
                invocation.pretty,
            );
            return failure.exit_code;
        }
    }
    let mut project = match Project::open(&state_path) {
        Ok(project) => project,
        Err(error) => {
            let failure = project_failure(error);
            emit_failure(
                invocation.name(),
                failure.clone(),
                invocation.json,
                invocation.pretty,
            );
            return failure.exit_code;
        }
    };

    match dispatch(&invocation, &mut project, &state_path, cwd) {
        Ok((session_id, result)) => emit_success(&invocation, session_id, result),
        Err(failure) => {
            emit_failure(
                invocation.name(),
                failure.clone(),
                invocation.json,
                invocation.pretty,
            );
            failure.exit_code
        }
    }
}

fn is_supported(command: &str) -> bool {
    matches!(
        command,
        "version"
            | "session start"
            | "session inspect"
            | "session finish"
            | "task create"
            | "task list"
            | "task inspect"
            | "task assign"
            | "task claim"
            | "task preflight"
            | "task submit"
            | "batch freeze"
            | "batch validate"
            | "batch commit"
            | "batch finalize"
    )
}

fn dispatch(
    invocation: &Invocation,
    project: &mut Project,
    state_path: &Path,
    cwd: &Path,
) -> Result<(Option<String>, Value), Failure> {
    match invocation.name() {
        "session start" => {
            let branch = git_value(cwd, &["branch", "--show-current"])
                .unwrap_or_else(|| "working-tree".into());
            let commit = git_value(cwd, &["rev-parse", "HEAD"]).unwrap_or_else(|| "unborn".into());
            let session = project
                .start_session(&branch, &commit)
                .map_err(project_failure)?;
            Ok((Some(session.id.clone()), to_value(session)?))
        }
        "session inspect" => {
            let id = latest_session_id(state_path)?;
            let session = project.session(&id).map_err(project_failure)?;
            Ok((Some(id), to_value(session)?))
        }
        "session finish" => {
            let id = active_session_id(state_path)?;
            let session = project.finish_session(&id).map_err(project_failure)?;
            Ok((Some(id), to_value(session)?))
        }
        "task create" => {
            let session_id = active_session_id(state_path)?;
            let prerequisites = invocation.options("--prerequisite").to_vec();
            let task = project
                .create_task(
                    &session_id,
                    required(invocation, "--title")?,
                    required(invocation, "--intent")?,
                    required(invocation, "--expected-outcome")?,
                    &prerequisites,
                )
                .map_err(project_failure)?;
            Ok((Some(session_id), to_value(public_task(task))?))
        }
        "task list" => {
            let session_id = latest_session_id(state_path)?;
            let mut tasks = Vec::new();
            for id in task_ids(state_path, &session_id)? {
                tasks.push(public_task(project.task(&id).map_err(project_failure)?));
            }
            Ok((Some(session_id), to_value(json!({ "tasks": tasks }))?))
        }
        "task inspect" => {
            let id = positional(invocation, 0)?;
            let task = public_task(project.task(id).map_err(project_failure)?);
            Ok((session_for_task(state_path, id)?, to_value(task)?))
        }
        "task assign" => {
            let id = positional(invocation, 0)?;
            let assignment = project
                .assign_task(id, required(invocation, "--worker")?, LEASE_SECONDS)
                .map_err(project_failure)?;
            Ok((
                session_for_task(state_path, id)?,
                json!({
                    "task_id": assignment.task_id,
                    "assignment_token": assignment.token
                }),
            ))
        }
        "task claim" | "task preflight" => {
            let id = positional(invocation, 0)?;
            let validations = decode_validations(invocation.options("--validation"))?;
            if invocation.name() == "task preflight" {
                return Ok((
                    session_for_task(state_path, id)?,
                    json!({
                        "task_id": id,
                        "assignment_valid": true,
                        "paths": invocation.options("--path"),
                        "focused_validation": validations
                    }),
                ));
            }
            let task = project
                .claim_task(
                    id,
                    required(invocation, "--token")?,
                    invocation.options("--path"),
                    &validations,
                )
                .map_err(project_failure)?;
            Ok((
                session_for_task(state_path, id)?,
                to_value(public_task(task))?,
            ))
        }
        "task submit" => {
            let id = positional(invocation, 0)?;
            let task = project
                .submit_task(id, required(invocation, "--token")?)
                .map_err(project_failure)?;
            Ok((
                session_for_task(state_path, id)?,
                to_value(public_task(task))?,
            ))
        }
        "batch freeze" => {
            let session_id = active_session_id(state_path)?;
            let batch = project.freeze_batch(&session_id).map_err(project_failure)?;
            Ok((Some(session_id), to_value(batch)?))
        }
        "batch validate" => {
            let (batch_id, session_id) = batch_for_status(state_path, "frozen")?;
            let batch = project
                .validate_batch(&batch_id, true)
                .map_err(project_failure)?;
            Ok((Some(session_id), to_value(batch)?))
        }
        "batch commit" | "batch finalize" => {
            let (batch_id, session_id) = batch_for_status(state_path, "validated")?;
            let batch = project.commit_batch(&batch_id).map_err(project_failure)?;
            Ok((Some(session_id), to_value(batch)?))
        }
        _ => Err(Failure::new(
            "unsupported_command",
            invocation.name(),
            EXIT_INVALID,
        )),
    }
}

fn public_task(mut task: Task) -> Task {
    task.assignment_token.clear();
    task
}

fn decode_validations(values: &[String]) -> Result<Vec<FocusedValidation>, Failure> {
    values
        .iter()
        .map(|value| {
            serde_json::from_str(value).map_err(|error| {
                Failure::new(
                    "invalid_arguments",
                    format!("decode --validation JSON: {error}"),
                    EXIT_INVALID,
                )
            })
        })
        .collect()
}

fn required<'a>(invocation: &'a Invocation, name: &str) -> Result<&'a str, Failure> {
    invocation.option(name).ok_or_else(|| {
        Failure::new(
            "invalid_arguments",
            format!("option {name} is required"),
            EXIT_INVALID,
        )
    })
}

fn positional(invocation: &Invocation, index: usize) -> Result<&str, Failure> {
    invocation.positional(index).ok_or_else(|| {
        Failure::new(
            "invalid_arguments",
            format!("{} requires an argument", invocation.name()),
            EXIT_INVALID,
        )
    })
}

fn latest_session_id(path: &Path) -> Result<String, Failure> {
    query_one(
        path,
        "SELECT id FROM sessions ORDER BY created_at DESC LIMIT 1",
        [],
        "session_not_found",
    )
}

fn active_session_id(path: &Path) -> Result<String, Failure> {
    query_one(
        path,
        "SELECT id FROM sessions WHERE status='active' ORDER BY created_at DESC LIMIT 1",
        [],
        "session_not_active",
    )
}

fn session_for_task(path: &Path, task_id: &str) -> Result<Option<String>, Failure> {
    query_one(
        path,
        "SELECT session_id FROM tasks WHERE id=?1",
        [task_id],
        "task_not_found",
    )
    .map(Some)
}

fn batch_for_status(path: &Path, status: &str) -> Result<(String, String), Failure> {
    let connection = Connection::open(path).map_err(sql_failure)?;
    connection
        .query_row(
            "SELECT id, session_id FROM batches WHERE status=?1 ORDER BY creation_order DESC LIMIT 1",
            [status],
            |row| Ok((row.get(0)?, row.get(1)?)),
        )
        .map_err(|_| Failure::new("batch_not_found", status, EXIT_INVALID))
}

fn task_ids(path: &Path, session_id: &str) -> Result<Vec<String>, Failure> {
    let connection = Connection::open(path).map_err(sql_failure)?;
    let mut statement = connection
        .prepare("SELECT id FROM tasks WHERE session_id=?1 ORDER BY creation_order")
        .map_err(sql_failure)?;
    let rows = statement
        .query_map([session_id], |row| row.get::<_, String>(0))
        .map_err(sql_failure)?;
    rows.collect::<Result<Vec<_>, _>>().map_err(sql_failure)
}

fn query_one<const N: usize>(
    path: &Path,
    sql: &str,
    params: [&str; N],
    missing_code: &str,
) -> Result<String, Failure> {
    let connection = Connection::open(path).map_err(sql_failure)?;
    connection
        .query_row(sql, rusqlite::params_from_iter(params), |row| row.get(0))
        .map_err(|error| match error {
            rusqlite::Error::QueryReturnedNoRows => {
                Failure::new(missing_code, "no matching state", EXIT_INVALID)
            }
            other => sql_failure(other),
        })
}

fn to_value(value: impl serde::Serialize) -> Result<Value, Failure> {
    serde_json::to_value(value)
        .map_err(|error| Failure::new("internal_error", error.to_string(), EXIT_INTERNAL))
}

fn git_value(cwd: &Path, args: &[&str]) -> Option<String> {
    let output = Command::new("git")
        .args(args)
        .current_dir(cwd)
        .output()
        .ok()?;
    if !output.status.success() {
        return None;
    }
    let value = String::from_utf8(output.stdout).ok()?.trim().to_owned();
    (!value.is_empty()).then_some(value)
}

#[derive(Debug, Clone)]
struct Failure {
    code: String,
    message: String,
    exit_code: i32,
}

impl Failure {
    fn new(code: impl Into<String>, message: impl Into<String>, exit_code: i32) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
            exit_code,
        }
    }
}

fn project_failure(error: BandmasterError) -> Failure {
    match error {
        BandmasterError::Invalid(message) => {
            let (code, message) = message
                .split_once(": ")
                .unwrap_or(("invalid_arguments", message.as_str()));
            Failure::new(code, message, EXIT_INVALID)
        }
        other => Failure::new("internal_error", other.to_string(), EXIT_INTERNAL),
    }
}

fn sql_failure(error: rusqlite::Error) -> Failure {
    Failure::new("state_error", error.to_string(), EXIT_INTERNAL)
}

fn emit_success(invocation: &Invocation, session_id: Option<String>, result: Value) -> i32 {
    if invocation.json {
        let envelope = Envelope::success(invocation.name(), session_id, result);
        return write_json(&envelope, invocation.pretty);
    }
    if invocation.name() == "version" {
        println!("bandmaster {VERSION}");
    } else {
        println!("{} succeeded.", invocation.name());
    }
    0
}

fn emit_failure(command: &str, failure: Failure, json: bool, pretty: bool) {
    if json {
        let envelope: Envelope<Value> = Envelope::failure(
            command,
            None,
            ErrorPayload::new(failure.code, failure.message, false),
        );
        let _ = write_json(&envelope, pretty);
    } else {
        eprintln!("{}", failure.message);
    }
}

fn write_json(value: &impl serde::Serialize, pretty: bool) -> i32 {
    let encoded = if pretty {
        serde_json::to_string_pretty(value)
    } else {
        serde_json::to_string(value)
    };
    match encoded {
        Ok(encoded) => {
            println!("{encoded}");
            0
        }
        Err(error) => {
            eprintln!("serialize JSON response: {error}");
            EXIT_INTERNAL
        }
    }
}
