#[path = "../src/tui.rs"]
mod tui;

use tui::{
    render_dashboard, render_dashboard_with_options, BatchSummary, DashboardSnapshot,
    DiagnosticSummary, RenderOptions, RepositorySummary, RuntimeSummary, SessionSummary,
    StateSummary, TaskSummary, WorkerSummary,
};

#[test]
fn healthy_view_summarizes_all_runtime_entities() {
    let snapshot = DashboardSnapshot {
        session: Some(SessionSummary {
            id: "session_1234567890".into(),
            status: "active".into(),
        }),
        runtime: RuntimeSummary {
            version: "0.1.0".into(),
            language_version: "rustc 1.88".into(),
            target: "aarch64-apple-darwin".into(),
        },
        repository: RepositorySummary {
            branch: "main".into(),
            head: "abcdef1234567890".into(),
            changed_paths: 1,
        },
        state: StateSummary {
            initialization: "initialized".into(),
            schema_version: "1".into(),
        },
        configuration_status: "approved".into(),
        collection_status: "complete".into(),
        tasks: vec![
            TaskSummary {
                id: "task_parser".into(),
                title: "Build parser".into(),
                status: "editing".into(),
                worker_identity: "worker-parser".into(),
                claim_count: 1,
            },
            TaskSummary {
                id: "task_docs".into(),
                title: "Write docs".into(),
                status: "planned".into(),
                ..TaskSummary::default()
            },
        ],
        workers: vec![WorkerSummary {
            identity: "worker-parser".into(),
            active_task_id: "task_parser_123456".into(),
            lease_status: "active".into(),
            lease_expires_at: "2030-01-01T00:00:00Z".into(),
            claim_paths: vec!["parser.rs".into()],
        }],
        batches: vec![BatchSummary {
            id: "batch_1234567890".into(),
            status: "collecting".into(),
            member_count: 1,
            path_count: 1,
        }],
        diagnostics: Vec::new(),
        integrity_violations: 0,
    };

    let view = render_dashboard(&snapshot);
    for expected in [
        "BANDMASTER - status",
        "Session: active (session_1234)",
        "editing: 1",
        "planned: 1",
        "Build parser",
        "worker-parser | lease active until 2030-01-01T00:00:00Z",
        "claim parser.rs",
        "collecting | batch_123456 | 1 member(s) | 1 path(s)",
    ] {
        assert!(view.contains(expected), "missing {expected:?} in:\n{view}");
    }
    assert!(!view.contains("WARNING"));
    assert!(!view.contains('\u{1b}'), "renderer must be plain text");
}

#[test]
fn degraded_view_surfaces_integrity_diagnostics_and_overflow() {
    let tasks = (0..4)
        .map(|number| TaskSummary {
            id: format!("task_{number}"),
            title: format!("Task {number}"),
            status: "quarantined".into(),
            ..TaskSummary::default()
        })
        .collect();
    let diagnostics = (0..3)
        .map(|number| DiagnosticSummary {
            code: format!("diagnostic_{number}"),
            severity: "error".into(),
            suggested_actions: if number == 0 {
                vec!["bandmaster task inspect task_0 --json".into()]
            } else {
                Vec::new()
            },
        })
        .collect();
    let snapshot = DashboardSnapshot {
        session: Some(SessionSummary {
            id: "session_degraded".into(),
            status: "paused".into(),
        }),
        state: StateSummary {
            initialization: "initialized".into(),
            schema_version: "1".into(),
        },
        configuration_status: "unapproved".into(),
        collection_status: "partial".into(),
        tasks,
        diagnostics,
        integrity_violations: 2,
        ..DashboardSnapshot::default()
    };

    let view = render_dashboard_with_options(
        &snapshot,
        RenderOptions {
            task_limit: 2,
            diagnostic_limit: 1,
        },
    );

    for expected in [
        "Session: paused",
        "Config: unapproved | Collection: partial",
        "WARNING: 2 integrity violation record(s)",
        "quarantined: 4",
        "... 2 more task(s)",
        "diagnostic_0 | error | bandmaster task inspect task_0 --json",
        "... 2 more diagnostic(s)",
    ] {
        assert!(view.contains(expected), "missing {expected:?} in:\n{view}");
    }
    assert!(!view.contains("Task 2"));
    assert!(!view.contains("diagnostic_1"));
}

#[test]
fn empty_view_explains_how_to_start() {
    let snapshot = DashboardSnapshot {
        state: StateSummary {
            initialization: "initialized".into(),
            ..StateSummary::default()
        },
        collection_status: "complete".into(),
        ..DashboardSnapshot::default()
    };

    let view = render_dashboard(&snapshot);
    assert!(view.contains("No Bandmaster session has been recorded."));
    assert!(view.contains("bandmaster session start"));
}

#[test]
fn unicode_titles_are_truncated_without_panicking() {
    let snapshot = DashboardSnapshot {
        session: Some(SessionSummary {
            id: "session_unicode".into(),
            status: "active".into(),
        }),
        tasks: vec![TaskSummary {
            title: "🎶".repeat(50),
            status: "ready".into(),
            ..TaskSummary::default()
        }],
        ..DashboardSnapshot::default()
    };

    let view = render_dashboard(&snapshot);
    assert!(view.contains('…'));
}
