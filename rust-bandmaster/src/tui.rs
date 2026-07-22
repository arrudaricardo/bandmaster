//! Deterministic, read-only terminal rendering for Bandmaster status.
//!
//! The Go implementation uses an interactive terminal framework.  The Rust
//! port intentionally keeps presentation simple: callers collect a snapshot
//! and pass it to [`render_dashboard`], which returns plain text suitable for
//! a terminal, a log, or a snapshot test.

use std::collections::BTreeMap;

pub const DEFAULT_TASK_LIMIT: usize = 12;
pub const DEFAULT_DIAGNOSTIC_LIMIT: usize = 8;

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct DashboardSnapshot {
    pub session: Option<SessionSummary>,
    pub runtime: RuntimeSummary,
    pub repository: RepositorySummary,
    pub state: StateSummary,
    pub configuration_status: String,
    pub collection_status: String,
    pub tasks: Vec<TaskSummary>,
    pub workers: Vec<WorkerSummary>,
    pub batches: Vec<BatchSummary>,
    pub diagnostics: Vec<DiagnosticSummary>,
    pub integrity_violations: usize,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct SessionSummary {
    pub id: String,
    pub status: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct RuntimeSummary {
    pub version: String,
    pub language_version: String,
    pub target: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct RepositorySummary {
    pub branch: String,
    pub head: String,
    pub changed_paths: usize,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct StateSummary {
    pub initialization: String,
    pub schema_version: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct TaskSummary {
    pub id: String,
    pub title: String,
    pub status: String,
    pub worker_identity: String,
    pub claim_count: usize,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct WorkerSummary {
    pub identity: String,
    pub active_task_id: String,
    pub lease_status: String,
    pub lease_expires_at: String,
    pub claim_paths: Vec<String>,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct BatchSummary {
    pub id: String,
    pub status: String,
    pub member_count: usize,
    pub path_count: usize,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct DiagnosticSummary {
    pub code: String,
    pub severity: String,
    pub suggested_actions: Vec<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RenderOptions {
    pub task_limit: usize,
    pub diagnostic_limit: usize,
}

impl Default for RenderOptions {
    fn default() -> Self {
        Self {
            task_limit: DEFAULT_TASK_LIMIT,
            diagnostic_limit: DEFAULT_DIAGNOSTIC_LIMIT,
        }
    }
}

pub fn render_dashboard(snapshot: &DashboardSnapshot) -> String {
    render_dashboard_with_options(snapshot, RenderOptions::default())
}

pub fn render_dashboard_with_options(
    snapshot: &DashboardSnapshot,
    options: RenderOptions,
) -> String {
    let mut lines = vec!["BANDMASTER - status".to_owned()];

    let Some(session) = &snapshot.session else {
        lines.push(String::new());
        lines.push("No Bandmaster session has been recorded.".to_owned());
        lines.push(format!(
            "State: {} | Collection: {}",
            display_or(&snapshot.state.initialization, "unknown"),
            display_or(&snapshot.collection_status, "unknown")
        ));
        lines.push("Start when the repository is clean: bandmaster session start".to_owned());
        return finish(lines);
    };

    lines.push(format!(
        "Session: {} ({})",
        display_or(&session.status, "unknown"),
        short_id(&session.id)
    ));
    lines.push(format!(
        "Runtime: bandmaster {} | {} | {}",
        display_or(&snapshot.runtime.version, "unknown"),
        display_or(&snapshot.runtime.language_version, "unknown"),
        display_or(&snapshot.runtime.target, "unknown")
    ));
    lines.push(format!(
        "Branch: {} @ {} | {} changed path(s)",
        display_or(&snapshot.repository.branch, "unknown"),
        short_id(&snapshot.repository.head),
        snapshot.repository.changed_paths
    ));
    lines.push(format!(
        "State: {} (schema {}) | Config: {} | Collection: {}",
        display_or(&snapshot.state.initialization, "unknown"),
        display_or(&snapshot.state.schema_version, "unknown"),
        display_or(&snapshot.configuration_status, "unknown"),
        display_or(&snapshot.collection_status, "unknown")
    ));
    if snapshot.integrity_violations > 0 {
        lines.push(format!(
            "WARNING: {} integrity violation record(s)",
            snapshot.integrity_violations
        ));
    }

    lines.push(String::new());
    lines.push("Work overview".to_owned());
    let counts = task_counts(&snapshot.tasks);
    if counts.is_empty() {
        lines.push("  No tasks planned yet.".to_owned());
    } else {
        for (status, count) in counts {
            lines.push(format!("  {status}: {count}"));
        }
    }

    if !snapshot.tasks.is_empty() {
        lines.push(String::new());
        lines.push("Tasks".to_owned());
        for task in snapshot.tasks.iter().take(options.task_limit) {
            lines.push(format!(
                "  {} | {} | {} | {} claim(s)",
                display_or(&task.status, "unknown"),
                truncate(&task.title, 39),
                display_or(&task.worker_identity, "unassigned"),
                task.claim_count
            ));
        }
        append_overflow(
            &mut lines,
            snapshot.tasks.len(),
            options.task_limit,
            "task(s)",
        );
    }

    if !snapshot.workers.is_empty() {
        lines.push(String::new());
        lines.push("Workers, leases, and claims".to_owned());
        for worker in &snapshot.workers {
            let lease = if worker.lease_status.is_empty() {
                "no active lease".to_owned()
            } else if worker.lease_expires_at.is_empty() {
                format!("lease {}", worker.lease_status)
            } else {
                format!(
                    "lease {} until {}",
                    worker.lease_status, worker.lease_expires_at
                )
            };
            lines.push(format!(
                "  {} | {} | task {}",
                display_or(&worker.identity, "unknown"),
                lease,
                short_id(&worker.active_task_id)
            ));
            for path in &worker.claim_paths {
                lines.push(format!("    claim {path}"));
            }
        }
    }

    if !snapshot.batches.is_empty() {
        lines.push(String::new());
        lines.push("Batches".to_owned());
        for batch in &snapshot.batches {
            lines.push(format!(
                "  {} | {} | {} member(s) | {} path(s)",
                display_or(&batch.status, "unknown"),
                short_id(&batch.id),
                batch.member_count,
                batch.path_count
            ));
        }
    }

    if !snapshot.diagnostics.is_empty() {
        lines.push(String::new());
        lines.push("Actionable diagnostics".to_owned());
        for diagnostic in snapshot.diagnostics.iter().take(options.diagnostic_limit) {
            let action = diagnostic
                .suggested_actions
                .first()
                .map(String::as_str)
                .unwrap_or("inspect bandmaster debug --json");
            lines.push(format!(
                "  {} | {} | {}",
                diagnostic.code, diagnostic.severity, action
            ));
        }
        append_overflow(
            &mut lines,
            snapshot.diagnostics.len(),
            options.diagnostic_limit,
            "diagnostic(s)",
        );
    }

    finish(lines)
}

fn task_counts(tasks: &[TaskSummary]) -> BTreeMap<&str, usize> {
    let mut counts = BTreeMap::new();
    for task in tasks {
        *counts.entry(task.status.as_str()).or_insert(0) += 1;
    }
    counts
}

fn append_overflow(lines: &mut Vec<String>, total: usize, limit: usize, label: &str) {
    if total > limit {
        lines.push(format!(
            "  ... {} more {label}; use bandmaster debug --json for the complete record",
            total - limit
        ));
    }
}

fn short_id(value: &str) -> String {
    value.chars().take(12).collect()
}

fn truncate(value: &str, length: usize) -> String {
    let count = value.chars().count();
    if count <= length {
        return value.to_owned();
    }
    if length == 0 {
        return String::new();
    }
    let mut truncated: String = value.chars().take(length - 1).collect();
    truncated.push('…');
    truncated
}

fn display_or<'a>(value: &'a str, fallback: &'a str) -> &'a str {
    if value.is_empty() {
        fallback
    } else {
        value
    }
}

fn finish(lines: Vec<String>) -> String {
    let mut output = lines.join("\n");
    output.push('\n');
    output
}
