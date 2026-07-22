//! Command-line parsing and the stable JSON response envelope.
//!
//! Parsing is intentionally data-oriented: the execution layer can dispatch on
//! `name()` and retrieve values without coupling the port to a large command
//! hierarchy. Assignment tokens remain available to that layer, but are
//! redacted from debug output and are never part of the response envelope.

use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::collections::{BTreeMap, BTreeSet};
use std::fmt;

pub const JSON_SCHEMA_VERSION: &str = "1";
pub const EXIT_INTERNAL: i32 = 1;
pub const EXIT_INVALID: i32 = 3;

#[derive(Clone, PartialEq, Eq)]
pub struct Invocation {
    name: String,
    positionals: Vec<String>,
    options: BTreeMap<String, Vec<String>>,
    pub json: bool,
    pub pretty: bool,
}

impl Invocation {
    pub fn name(&self) -> &str {
        &self.name
    }

    /// Returns the operation used by the execution layer. Legacy aliases keep
    /// their original `name()` for compatible response envelopes.
    pub fn canonical_name(&self) -> &str {
        match self.name.as_str() {
            "batch finalize" => "batch commit",
            "task preflight" => "task claim",
            name => name,
        }
    }

    pub fn positional(&self, index: usize) -> Option<&str> {
        self.positionals.get(index).map(String::as_str)
    }

    pub fn option(&self, name: &str) -> Option<&str> {
        self.options
            .get(name)
            .and_then(|values| values.first())
            .map(String::as_str)
    }

    pub fn options(&self, name: &str) -> &[String] {
        self.options.get(name).map(Vec::as_slice).unwrap_or(&[])
    }

    pub fn has_flag(&self, name: &str) -> bool {
        self.options.contains_key(name)
    }
}

impl fmt::Debug for Invocation {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        let options: BTreeMap<&str, Vec<&str>> = self
            .options
            .iter()
            .map(|(name, values)| {
                let values = if name == "--token" {
                    vec!["[REDACTED]"]
                } else {
                    values.iter().map(String::as_str).collect()
                };
                (name.as_str(), values)
            })
            .collect();
        formatter
            .debug_struct("Invocation")
            .field("name", &self.name)
            .field("positionals", &self.positionals)
            .field("options", &options)
            .field("json", &self.json)
            .field("pretty", &self.pretty)
            .finish()
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CliError {
    pub command: String,
    pub message: String,
}

impl CliError {
    fn invalid(command: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            command: command.into(),
            message: message.into(),
        }
    }
}

impl fmt::Display for CliError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.message)
    }
}

impl std::error::Error for CliError {}

#[derive(Clone, Copy)]
struct CommandSpec {
    name: &'static str,
    positional_count: usize,
    value_options: &'static [&'static str],
    repeatable: &'static [&'static str],
    flags: &'static [&'static str],
    required: &'static [&'static str],
}

const SPECS: &[CommandSpec] = &[
    spec("version", 0, &[], &[], &[], &[]),
    spec("doctor", 0, &[], &[], &[], &[]),
    spec(
        "debug",
        0,
        &["--session", "--history-limit", "--interval"],
        &[],
        &[
            "--watch",
            "--follow-latest",
            "--complete-history",
            "--unsafe",
            "--unsafe-show-secrets",
        ],
        &[],
    ),
    spec("tui", 0, &[], &[], &[], &[]),
    spec("init", 0, &[], &[], &["--debug-skill"], &[]),
    spec("config status", 0, &[], &[], &[], &[]),
    spec("config approve", 1, &[], &[], &[], &[]),
    spec("session start", 0, &[], &[], &[], &[]),
    spec("session inspect", 0, &[], &[], &[], &[]),
    spec("session pause", 0, &[], &[], &[], &[]),
    spec("session resume", 0, &[], &[], &[], &[]),
    spec("session finish", 0, &[], &[], &[], &[]),
    spec(
        "session abort",
        0,
        &["--termination-confirmation"],
        &[],
        &["--dry-run"],
        &[],
    ),
    spec(
        "integrity recover",
        0,
        &["--confirmation"],
        &[],
        &[],
        &["--confirmation"],
    ),
    spec(
        "finalization recover",
        0,
        &["--confirmation"],
        &[],
        &[],
        &[],
    ),
    spec("batch freeze", 0, &[], &[], &[], &[]),
    spec("batch validate", 0, &[], &[], &[], &[]),
    spec("batch commit", 0, &[], &[], &[], &[]),
    spec("batch finalize", 0, &[], &[], &[], &[]),
    spec("batch inspect", 0, &[], &[], &[], &[]),
    spec(
        "batch abandon",
        0,
        &["--reason", "--confirmation"],
        &[],
        &[],
        &["--reason", "--confirmation"],
    ),
    spec(
        "task create",
        0,
        &[
            "--title",
            "--intent",
            "--expected-outcome",
            "--prerequisite",
        ],
        &["--prerequisite"],
        &[],
        &["--title", "--intent", "--expected-outcome"],
    ),
    spec("task list", 0, &[], &[], &[], &[]),
    spec("task inspect", 1, &[], &[], &[], &[]),
    spec("task assign", 1, &["--worker"], &[], &[], &["--worker"]),
    spec(
        "task replan",
        1,
        &[
            "--title",
            "--intent",
            "--expected-outcome",
            "--prerequisite",
            "--terminated-worker",
            "--termination-proof",
        ],
        &["--prerequisite"],
        &[],
        &["--title", "--intent", "--expected-outcome"],
    ),
    spec(
        "task cancel",
        1,
        &["--terminated-worker", "--termination-proof"],
        &[],
        &[],
        &[],
    ),
    spec("task requeue", 1, &[], &[], &[], &[]),
    spec(
        "task recover",
        1,
        &[
            "--terminated-worker",
            "--termination-proof",
            "--user-confirmation",
            "--diagnosis",
            "--intended-repair",
        ],
        &[],
        &[],
        &["--diagnosis", "--intended-repair"],
    ),
    spec(
        "task repair",
        1,
        &[
            "--terminated-worker",
            "--termination-proof",
            "--user-confirmation",
            "--diagnosis",
            "--intended-repair",
        ],
        &[],
        &[],
        &["--diagnosis", "--intended-repair"],
    ),
    spec(
        "task preflight",
        1,
        &["--token", "--path", "--validation"],
        &["--path", "--validation"],
        &[],
        &["--token", "--path", "--validation"],
    ),
    spec(
        "task claim",
        1,
        &["--token", "--path", "--validation"],
        &["--path", "--validation"],
        &[],
        &["--token", "--path", "--validation"],
    ),
    spec(
        "task release",
        1,
        &["--token", "--path"],
        &["--path"],
        &[],
        &["--token", "--path"],
    ),
    spec("task heartbeat", 1, &["--token"], &[], &[], &["--token"]),
    spec("task diff", 1, &["--token"], &[], &[], &["--token"]),
    spec(
        "task submit",
        1,
        &[
            "--token",
            "--behavior-changed",
            "--key-decisions",
            "--validation-expectations",
            "--known-risks",
        ],
        &[],
        &[],
        &[
            "--token",
            "--behavior-changed",
            "--key-decisions",
            "--validation-expectations",
            "--known-risks",
        ],
    ),
    // Internal monitor subprocess; not advertised as a public command.
    spec("monitor run", 2, &[], &[], &[], &[]),
];

const fn spec(
    name: &'static str,
    positional_count: usize,
    value_options: &'static [&'static str],
    repeatable: &'static [&'static str],
    flags: &'static [&'static str],
    required: &'static [&'static str],
) -> CommandSpec {
    CommandSpec {
        name,
        positional_count,
        value_options,
        repeatable,
        flags,
        required,
    }
}

/// Parses arguments after the executable name.
pub fn parse<I, S>(args: I) -> Result<Invocation, CliError>
where
    I: IntoIterator<Item = S>,
    S: Into<String>,
{
    let mut json = false;
    let mut pretty = false;
    let filtered: Vec<String> = args
        .into_iter()
        .map(Into::into)
        .filter(|argument| match argument.as_str() {
            "--json" => {
                json = true;
                false
            }
            "--pretty" => {
                pretty = true;
                false
            }
            _ => true,
        })
        .collect();

    let guessed_command = filtered
        .iter()
        .take(2)
        .cloned()
        .collect::<Vec<_>>()
        .join(" ");
    if pretty && !json {
        return Err(CliError::invalid(
            guessed_command,
            "--pretty requires --json.",
        ));
    }

    let (spec, consumed) = find_spec(&filtered).ok_or_else(|| {
        CliError::invalid(
            "unknown",
            "invalid command arguments; run bandmaster --help",
        )
    })?;
    let mut tail = filtered[consumed..].iter();
    let mut positionals = Vec::with_capacity(spec.positional_count);
    for _ in 0..spec.positional_count {
        let positional = tail.next().ok_or_else(|| {
            CliError::invalid(spec.name, format!("{} requires an argument", spec.name))
        })?;
        if positional.starts_with("--") {
            return Err(CliError::invalid(
                spec.name,
                format!("{} requires an argument", spec.name),
            ));
        }
        positionals.push(positional.clone());
    }
    // The Go CLI accepts either the current batch or one optional batch ID.
    if spec.name == "batch inspect" {
        if let Some(batch_id) = tail.clone().next().filter(|value| !value.starts_with("--")) {
            positionals.push(batch_id.clone());
            tail.next();
        }
    }

    let values: BTreeSet<&str> = spec.value_options.iter().copied().collect();
    let flags: BTreeSet<&str> = spec.flags.iter().copied().collect();
    let repeatable: BTreeSet<&str> = spec.repeatable.iter().copied().collect();
    let mut options: BTreeMap<String, Vec<String>> = BTreeMap::new();
    while let Some(name) = tail.next() {
        if flags.contains(name.as_str()) {
            if options.contains_key(name) {
                return Err(CliError::invalid(
                    spec.name,
                    format!("option {name} may be specified only once"),
                ));
            }
            options.insert(name.clone(), Vec::new());
            continue;
        }
        if !values.contains(name.as_str()) {
            return Err(CliError::invalid(
                spec.name,
                format!("unknown option {name}"),
            ));
        }
        if options.contains_key(name) && !repeatable.contains(name.as_str()) {
            return Err(CliError::invalid(
                spec.name,
                format!("option {name} may be specified only once"),
            ));
        }
        let value = tail.next().ok_or_else(|| {
            CliError::invalid(spec.name, format!("option {name} requires a value"))
        })?;
        options.entry(name.clone()).or_default().push(value.clone());
    }

    for required in spec.required {
        if !options.contains_key(*required) {
            return Err(CliError::invalid(
                spec.name,
                format!("option {required} is required"),
            ));
        }
    }
    if spec.name == "debug" {
        if options.contains_key("--follow-latest") && !options.contains_key("--watch") {
            return Err(CliError::invalid(
                spec.name,
                "--follow-latest requires --watch",
            ));
        }
        if pretty && options.contains_key("--watch") {
            return Err(CliError::invalid(
                spec.name,
                "--pretty is not supported with --watch.",
            ));
        }
    }

    Ok(Invocation {
        name: spec.name.to_owned(),
        positionals,
        options,
        json,
        pretty,
    })
}

fn find_spec(args: &[String]) -> Option<(&'static CommandSpec, usize)> {
    SPECS.iter().find_map(|spec| {
        let words: Vec<&str> = spec.name.split(' ').collect();
        (args.len() >= words.len()
            && args
                .iter()
                .take(words.len())
                .map(String::as_str)
                .eq(words.iter().copied()))
        .then_some((spec, words.len()))
    })
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ErrorPayload {
    pub code: String,
    pub message: String,
    pub retryable: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub initiating_error: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub rollback_error: Option<Value>,
}

impl ErrorPayload {
    pub fn new(code: impl Into<String>, message: impl Into<String>, retryable: bool) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
            retryable,
            initiating_error: None,
            rollback_error: None,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Envelope<T> {
    pub schema_version: String,
    pub command: String,
    pub success: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub session_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub result: Option<T>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<ErrorPayload>,
}

impl<T> Envelope<T> {
    pub fn success(command: impl Into<String>, session_id: Option<String>, result: T) -> Self {
        Self {
            schema_version: JSON_SCHEMA_VERSION.to_owned(),
            command: command.into(),
            success: true,
            session_id,
            result: Some(result),
            error: None,
        }
    }

    pub fn failure(
        command: impl Into<String>,
        session_id: Option<String>,
        error: ErrorPayload,
    ) -> Self {
        Self {
            schema_version: JSON_SCHEMA_VERSION.to_owned(),
            command: command.into(),
            success: false,
            session_id,
            result: None,
            error: Some(error),
        }
    }
}
