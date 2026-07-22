pub use bandmaster::*;

#[path = "../src/project.rs"]
mod project;

use project::Project;

fn validation() -> Vec<FocusedValidation> {
    vec![FocusedValidation {
        name: "focused".into(),
        argv: vec!["cargo".into(), "test".into()],
        working_directory: ".".into(),
        timeout: "1m".into(),
        ..FocusedValidation::default()
    }]
}

#[test]
fn task_ownership_batch_and_dependency_lifecycle() {
    let mut project = Project::in_memory().expect("project");
    let session = project.start_session("main", "abc123").expect("session");
    let first = project
        .create_task(
            &session.id,
            "foundation",
            "port state",
            "working state",
            &[],
        )
        .expect("first task");
    let dependent = project
        .create_task(
            &session.id,
            "cli",
            "port commands",
            "working cli",
            std::slice::from_ref(&first.id),
        )
        .expect("dependent task");
    assert_eq!(dependent.status, "planned");

    let owner = project
        .assign_task(&first.id, "worker-1", 300)
        .expect("assign");
    let intruder = project
        .claim_task(
            &first.id,
            "wrong-token",
            &["src/lib.rs".into()],
            &validation(),
        )
        .expect_err("wrong token rejected");
    assert!(intruder.to_string().contains("assignment_token_invalid"));

    let editing = project
        .claim_task(
            &first.id,
            &owner.token,
            &["src/lib.rs".into()],
            &validation(),
        )
        .expect("claim");
    assert_eq!(editing.status, "editing");
    project
        .heartbeat(&first.id, &owner.token, 300)
        .expect("heartbeat");
    let submitted = project
        .submit_task(&first.id, &owner.token)
        .expect("submit");
    assert_eq!(submitted.status, "submitted");

    let frozen = project.freeze_batch(&session.id).expect("freeze");
    assert_eq!(frozen.status, "frozen");
    project.validate_batch(&frozen.id, true).expect("validate");
    let committed = project.commit_batch(&frozen.id).expect("commit");
    assert_eq!(committed.status, "committed");
    assert_eq!(
        project.task(&dependent.id).expect("dependent").status,
        "ready"
    );

    // Committing releases ownership, so the dependent may claim the same path.
    let dependent_owner = project
        .assign_task(&dependent.id, "worker-2", 300)
        .expect("assign dependent");
    project
        .claim_task(
            &dependent.id,
            &dependent_owner.token,
            &["src/lib.rs".into()],
            &validation(),
        )
        .expect("dependent claim");
    project
        .submit_task(&dependent.id, &dependent_owner.token)
        .expect("dependent submit");
    let second_batch = project.freeze_batch(&session.id).expect("second freeze");
    project
        .validate_batch(&second_batch.id, true)
        .expect("second validate");
    project
        .commit_batch(&second_batch.id)
        .expect("second commit");
    assert_eq!(
        project.finish_session(&session.id).expect("finish").status,
        "completed"
    );
}

#[test]
fn active_claims_are_exclusive_and_integrity_pauses_the_session() {
    let mut project = Project::in_memory().expect("project");
    let session = project.start_session("main", "abc123").expect("session");
    let one = project
        .create_task(&session.id, "one", "one", "one", &[])
        .expect("one");
    let two = project
        .create_task(&session.id, "two", "two", "two", &[])
        .expect("two");
    let owner_one = project
        .assign_task(&one.id, "worker-1", 300)
        .expect("assign one");
    let owner_two = project
        .assign_task(&two.id, "worker-2", 300)
        .expect("assign two");
    project
        .claim_task(
            &one.id,
            &owner_one.token,
            &["shared.rs".into()],
            &validation(),
        )
        .expect("first claim");
    let conflict = project
        .claim_task(
            &two.id,
            &owner_two.token,
            &["shared.rs".into()],
            &validation(),
        )
        .expect_err("claim conflict");
    assert!(conflict.to_string().contains("claim_conflict"));

    let violation = project
        .record_integrity_violation(
            &session.id,
            "unexpected_change",
            "outside.rs",
            serde_json::json!({"changed": true}),
        )
        .expect("violation");
    assert_eq!(
        project.session(&session.id).expect("paused").status,
        "paused"
    );
    let recovered = project
        .recover_integrity(&session.id, violation.id, "operator verified restoration")
        .expect("recover");
    assert_eq!(recovered.status, "active");
    assert!(!recovered.integrity_violations[0].recovered_at.is_empty());
}
