use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::collections::BTreeMap;

/// A repository path at the time it was claimed or submitted.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct PathSnapshot {
    pub presence: String,
    #[serde(rename = "type")]
    pub file_type: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub content_hash: String,
    #[serde(default)]
    pub executable: bool,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct WorkerLease {
    pub status: String,
    pub renewed_at: String,
    pub expires_at: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Claim {
    pub path: String,
    pub baseline: PathSnapshot,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub submitted_snapshot: Option<PathSnapshot>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct OwnershipEvidence {
    pub path: String,
    pub baseline: PathSnapshot,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub submitted_snapshot: Option<PathSnapshot>,
    pub claimed_at: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct FocusedValidation {
    pub name: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub argv: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub script: String,
    pub working_directory: String,
    pub timeout: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub environment: BTreeMap<String, String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Submission {
    pub outcome: String,
    pub no_changes: bool,
    pub behavior_changed: String,
    pub key_decisions: String,
    pub validation_expectations: String,
    pub known_risks: String,
    pub submitted_at: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RepairSnapshot {
    pub path: String,
    pub snapshot: PathSnapshot,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub invalidated_submitted_snapshot: Option<PathSnapshot>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct TaskAuditEvent {
    pub sequence: i64,
    pub event: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub from_status: String,
    pub to_status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub worker_identity: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub termination_proof: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub recovery_method: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub user_confirmation: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub replacement_assignment_token: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub diagnosis: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub intended_repair: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub invalidated_submission: Option<Submission>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub repair_snapshots: Vec<RepairSnapshot>,
    pub occurred_at: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Task {
    pub id: String,
    pub creation_order: i64,
    pub title: String,
    pub intent: String,
    pub expected_outcome: String,
    #[serde(default)]
    pub prerequisites: Vec<String>,
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub worker_identity: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub assignment_token: String,
    pub core_frozen: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub batch_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub commit_sha: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub lease: Option<WorkerLease>,
    #[serde(default)]
    pub claims: Vec<Claim>,
    #[serde(default)]
    pub ownership_evidence: Vec<OwnershipEvidence>,
    #[serde(default)]
    pub focused_validation: Vec<FocusedValidation>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub submission: Option<Submission>,
    pub created_at: String,
    pub updated_at: String,
    #[serde(default)]
    pub audit_history: Vec<TaskAuditEvent>,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct SessionAuditEvent {
    pub sequence: i64,
    pub event: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub from_status: String,
    pub to_status: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub integrity_violation_id: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub integrity_kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub integrity_path: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub observed_state: Option<Value>,
    pub occurred_at: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct IntegrityMonitor {
    pub generation: i64,
    pub process_id: i64,
    pub process_identity: String,
    pub process_start_identity: String,
    pub status: String,
    pub started_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub heartbeat_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub last_full_scan_at: String,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct IntegrityViolation {
    pub id: i64,
    pub kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub path: String,
    pub observed_state: Value,
    pub detected_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub recovered_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub recovery_confirmation: String,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Session {
    pub id: String,
    pub status: String,
    pub starting_branch: String,
    pub starting_commit: String,
    pub created_at: String,
    pub updated_at: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub monitor: Option<IntegrityMonitor>,
    #[serde(default)]
    pub integrity_violations: Vec<IntegrityViolation>,
    #[serde(default)]
    pub audit_history: Vec<SessionAuditEvent>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct BatchMember {
    pub task_id: String,
    pub membership_order: i64,
    pub task_creation_order: i64,
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub submission_outcome: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct BatchPath {
    pub task_id: String,
    pub membership_order: i64,
    pub claim_order: i64,
    pub path: String,
    pub baseline: PathSnapshot,
    pub submitted: PathSnapshot,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct BatchAuditEvent {
    pub sequence: i64,
    pub event: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub from_status: String,
    pub to_status: String,
    pub occurred_at: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct BatchValidationAttempt {
    pub attempt: i64,
    pub status: String,
    pub started_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub finished_at: String,
    #[serde(default)]
    pub commands: Vec<BatchValidationRun>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct BatchValidationRun {
    pub attempt: i64,
    pub command_order: i64,
    pub source: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub task_id: String,
    pub name: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub argv: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub script: String,
    pub resolved_argv: Vec<String>,
    pub working_directory: String,
    pub resolved_working_directory: String,
    pub timeout: String,
    #[serde(default)]
    pub environment_overrides: BTreeMap<String, String>,
    #[serde(default)]
    pub resolved_environment: BTreeMap<String, String>,
    pub status: String,
    pub exit_code: Option<i32>,
    pub duration_milliseconds: i64,
    pub stdout: String,
    pub stderr: String,
    pub stdout_truncated: bool,
    pub stderr_truncated: bool,
    pub started_at: String,
    pub finished_at: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Batch {
    pub id: String,
    pub creation_order: i64,
    pub base_branch: String,
    pub base_commit: String,
    pub status: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub frozen_at: String,
    #[serde(default)]
    pub members: Vec<BatchMember>,
    #[serde(default)]
    pub manifest: Vec<BatchPath>,
    #[serde(default)]
    pub validation: Vec<BatchValidationAttempt>,
    #[serde(default)]
    pub audit_history: Vec<BatchAuditEvent>,
    pub created_at: String,
    pub updated_at: String,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ConfigStatus {
    pub validation_digest: String,
    pub approved: bool,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ValidationCommand {
    pub name: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub argv: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub script: String,
    pub working_directory: String,
    pub timeout: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub environment: BTreeMap<String, String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ValidationConfig {
    #[serde(default)]
    pub commands: Vec<ValidationCommand>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Configuration {
    pub version: u32,
    pub worker_lease_duration: String,
    pub validation: ValidationConfig,
}

fn is_zero(value: &i64) -> bool {
    *value == 0
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn path_snapshot_uses_go_json_field_names() {
        let snapshot = PathSnapshot {
            presence: "present".into(),
            file_type: "regular_file".into(),
            content_hash: "sha256:abc".into(),
            executable: false,
        };

        let value = serde_json::to_value(snapshot).expect("serialize snapshot");
        assert_eq!(value["type"], "regular_file");
        assert!(value.get("file_type").is_none());
    }

    #[test]
    fn optional_go_fields_are_omitted() {
        let validation = FocusedValidation {
            name: "test".into(),
            working_directory: ".".into(),
            timeout: "1m".into(),
            ..FocusedValidation::default()
        };

        let value = serde_json::to_value(validation).expect("serialize validation");
        assert!(value.get("argv").is_none());
        assert!(value.get("script").is_none());
        assert!(value.get("environment").is_none());
    }

    #[test]
    fn configuration_uses_the_existing_yaml_contract() {
        let yaml = r#"
version: 1
worker_lease_duration: 5m
validation:
  commands:
    - name: tests
      argv: [cargo, test]
      working_directory: .
      timeout: 5m
"#;

        let config: Configuration = serde_yaml::from_str(yaml).expect("parse config");
        assert_eq!(config.worker_lease_duration, "5m");
        assert_eq!(config.validation.commands[0].argv, ["cargo", "test"]);
    }
}
