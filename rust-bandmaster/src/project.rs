//! A deliberately small SQLite-backed Bandmaster lifecycle engine.

use crate::{
    BandmasterError, Batch, BatchAuditEvent, BatchMember, Claim, FocusedValidation,
    IntegrityViolation, PathSnapshot, Session, Submission, Task, WorkerLease,
};
use chrono::{Duration, SecondsFormat, Utc};
use rand::{thread_rng, RngCore};
use rusqlite::{params, Connection, OptionalExtension, Transaction};
use std::path::Path;

pub type Result<T> = std::result::Result<T, BandmasterError>;

pub struct Project {
    connection: Connection,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Assignment {
    pub task_id: String,
    pub token: String,
}

impl Project {
    pub fn open(path: impl AsRef<Path>) -> Result<Self> {
        let connection = Connection::open(path)?;
        let project = Self { connection };
        project.initialize()?;
        Ok(project)
    }

    pub fn in_memory() -> Result<Self> {
        let connection = Connection::open_in_memory()?;
        let project = Self { connection };
        project.initialize()?;
        Ok(project)
    }

    fn initialize(&self) -> Result<()> {
        self.connection.execute_batch(
            "PRAGMA foreign_keys = ON;
             CREATE TABLE IF NOT EXISTS sessions (
               id TEXT PRIMARY KEY, status TEXT NOT NULL, starting_branch TEXT NOT NULL,
               starting_commit TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
             );
             CREATE UNIQUE INDEX IF NOT EXISTS one_open_session ON sessions((1))
               WHERE status IN ('active', 'paused', 'finalizing');
             CREATE TABLE IF NOT EXISTS tasks (
               id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id),
               creation_order INTEGER NOT NULL, title TEXT NOT NULL, intent TEXT NOT NULL,
               expected_outcome TEXT NOT NULL, prerequisites TEXT NOT NULL, status TEXT NOT NULL,
               worker_identity TEXT NOT NULL DEFAULT '', assignment_token TEXT NOT NULL DEFAULT '',
               lease_renewed_at TEXT NOT NULL DEFAULT '', lease_expires_at TEXT NOT NULL DEFAULT '',
               batch_id TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL
             );
             CREATE TABLE IF NOT EXISTS claims (
               task_id TEXT NOT NULL REFERENCES tasks(id), path TEXT NOT NULL, released INTEGER NOT NULL DEFAULT 0,
               PRIMARY KEY(task_id, path)
             );
             CREATE UNIQUE INDEX IF NOT EXISTS one_active_claim_per_path ON claims(path) WHERE released = 0;
             CREATE TABLE IF NOT EXISTS batches (
               id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id),
               creation_order INTEGER NOT NULL, base_branch TEXT NOT NULL, base_commit TEXT NOT NULL,
               status TEXT NOT NULL, frozen_at TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
               updated_at TEXT NOT NULL
             );
             CREATE TABLE IF NOT EXISTS batch_members (
               batch_id TEXT NOT NULL REFERENCES batches(id), task_id TEXT NOT NULL REFERENCES tasks(id),
               membership_order INTEGER NOT NULL, PRIMARY KEY(batch_id, task_id)
             );
             CREATE TABLE IF NOT EXISTS integrity_violations (
               id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id),
               kind TEXT NOT NULL, path TEXT NOT NULL, observed_state TEXT NOT NULL,
               detected_at TEXT NOT NULL, recovered_at TEXT NOT NULL DEFAULT '',
               recovery_confirmation TEXT NOT NULL DEFAULT ''
             );",
        )?;
        Ok(())
    }

    pub fn start_session(&mut self, branch: &str, commit: &str) -> Result<Session> {
        require_text("branch", branch)?;
        require_text("commit", commit)?;
        let now = now();
        let id = random_id("session", 16);
        self.connection.execute(
            "INSERT INTO sessions(id, status, starting_branch, starting_commit, created_at, updated_at)
             VALUES(?1, 'active', ?2, ?3, ?4, ?4)",
            params![id, branch, commit, now],
        ).map_err(|error| match error {
            rusqlite::Error::SqliteFailure(_, _) => invalid("session_already_active", "an open session already exists"),
            other => other.into(),
        })?;
        self.session(&id)
    }

    pub fn session(&self, id: &str) -> Result<Session> {
        let mut session = self.connection.query_row(
            "SELECT id, status, starting_branch, starting_commit, created_at, updated_at FROM sessions WHERE id = ?1",
            [id],
            |row| Ok(Session {
                id: row.get(0)?, status: row.get(1)?, starting_branch: row.get(2)?,
                starting_commit: row.get(3)?, created_at: row.get(4)?, updated_at: row.get(5)?,
                monitor: None, integrity_violations: Vec::new(), audit_history: Vec::new(),
            }),
        ).optional()?.ok_or_else(|| invalid("session_not_found", id))?;
        session.integrity_violations = self.integrity_violations(id)?;
        Ok(session)
    }

    pub fn create_task(
        &mut self,
        session_id: &str,
        title: &str,
        intent: &str,
        expected_outcome: &str,
        prerequisites: &[String],
    ) -> Result<Task> {
        require_text("title", title)?;
        require_text("intent", intent)?;
        require_text("expected outcome", expected_outcome)?;
        let tx = self.connection.transaction()?;
        require_session_status(&tx, session_id, "active")?;
        let mut all_committed = true;
        for prerequisite in prerequisites {
            let status: Option<String> = tx
                .query_row(
                    "SELECT status FROM tasks WHERE id = ?1 AND session_id = ?2",
                    params![prerequisite, session_id],
                    |row| row.get(0),
                )
                .optional()?;
            let status =
                status.ok_or_else(|| invalid("invalid_task_prerequisite", prerequisite))?;
            all_committed &= status == "committed";
        }
        let status = if all_committed { "ready" } else { "planned" };
        let order: i64 = tx.query_row(
            "SELECT COALESCE(MAX(creation_order), 0) + 1 FROM tasks WHERE session_id = ?1",
            [session_id],
            |row| row.get(0),
        )?;
        let id = random_id("task", 16);
        let now = now();
        tx.execute(
            "INSERT INTO tasks(id, session_id, creation_order, title, intent, expected_outcome,
             prerequisites, status, created_at, updated_at) VALUES(?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?9)",
            params![id, session_id, order, title, intent, expected_outcome,
                serde_json::to_string(prerequisites)?, status, now],
        )?;
        tx.commit()?;
        self.task(&id)
    }

    pub fn assign_task(
        &mut self,
        task_id: &str,
        worker: &str,
        lease_seconds: i64,
    ) -> Result<Assignment> {
        require_text("worker identity", worker)?;
        if lease_seconds <= 0 {
            return Err(invalid("invalid_lease", "lease duration must be positive"));
        }
        let token = random_id("assignment", 32);
        let renewed = Utc::now();
        let expires = renewed + Duration::seconds(lease_seconds);
        let changed = self.connection.execute(
            "UPDATE tasks SET status = 'assigned', worker_identity = ?2, assignment_token = ?3,
             lease_renewed_at = ?4, lease_expires_at = ?5, updated_at = ?4 WHERE id = ?1 AND status = 'ready'",
            params![task_id, worker, token, timestamp(renewed), timestamp(expires)],
        )?;
        if changed != 1 {
            return Err(invalid("task_not_ready", task_id));
        }
        Ok(Assignment {
            task_id: task_id.into(),
            token,
        })
    }

    pub fn claim_task(
        &mut self,
        task_id: &str,
        token: &str,
        paths: &[String],
        validations: &[FocusedValidation],
    ) -> Result<Task> {
        if paths.is_empty() {
            return Err(invalid("claim_required", "at least one path is required"));
        }
        if validations.is_empty() {
            return Err(invalid(
                "validation_required",
                "at least one focused validation is required",
            ));
        }
        let tx = self.connection.transaction()?;
        authorize(&tx, task_id, token, "assigned")?;
        let (session_id, branch, commit) = task_session_baseline(&tx, task_id)?;
        let batch_id =
            collecting_batch(&tx, &session_id)?.unwrap_or_else(|| random_id("batch", 16));
        let exists: bool = tx.query_row(
            "SELECT EXISTS(SELECT 1 FROM batches WHERE id = ?1)",
            [&batch_id],
            |row| row.get(0),
        )?;
        let now = now();
        if !exists {
            let order: i64 = tx.query_row(
                "SELECT COALESCE(MAX(creation_order), 0) + 1 FROM batches WHERE session_id = ?1",
                [&session_id],
                |row| row.get(0),
            )?;
            tx.execute("INSERT INTO batches(id, session_id, creation_order, base_branch, base_commit, status, created_at, updated_at) VALUES(?1, ?2, ?3, ?4, ?5, 'collecting', ?6, ?6)", params![batch_id, session_id, order, branch, commit, now])?;
        }
        for path in paths {
            require_text("claim path", path)?;
            tx.execute(
                "INSERT INTO claims(task_id, path) VALUES(?1, ?2)",
                params![task_id, path],
            )
            .map_err(|error| match error {
                rusqlite::Error::SqliteFailure(_, _) => invalid("claim_conflict", path),
                other => other.into(),
            })?;
        }
        let member_order: i64 = tx.query_row(
            "SELECT COUNT(*) + 1 FROM batch_members WHERE batch_id = ?1",
            [&batch_id],
            |row| row.get(0),
        )?;
        tx.execute(
            "INSERT INTO batch_members(batch_id, task_id, membership_order) VALUES(?1, ?2, ?3)",
            params![batch_id, task_id, member_order],
        )?;
        tx.execute(
            "UPDATE tasks SET status = 'editing', batch_id = ?2, updated_at = ?3 WHERE id = ?1",
            params![task_id, batch_id, now],
        )?;
        tx.commit()?;
        self.task(task_id)
    }

    pub fn heartbeat(&mut self, task_id: &str, token: &str, lease_seconds: i64) -> Result<()> {
        if lease_seconds <= 0 {
            return Err(invalid("invalid_lease", "lease duration must be positive"));
        }
        let tx = self.connection.transaction()?;
        authorize(&tx, task_id, token, "editing")?;
        let renewed = Utc::now();
        tx.execute("UPDATE tasks SET lease_renewed_at = ?2, lease_expires_at = ?3, updated_at = ?2 WHERE id = ?1",
            params![task_id, timestamp(renewed), timestamp(renewed + Duration::seconds(lease_seconds))])?;
        tx.commit()?;
        Ok(())
    }

    pub fn submit_task(&mut self, task_id: &str, token: &str) -> Result<Task> {
        let tx = self.connection.transaction()?;
        authorize(&tx, task_id, token, "editing")?;
        let count: i64 = tx.query_row(
            "SELECT COUNT(*) FROM claims WHERE task_id = ?1 AND released = 0",
            [task_id],
            |row| row.get(0),
        )?;
        if count == 0 {
            return Err(invalid("claim_required", task_id));
        }
        tx.execute("UPDATE tasks SET status = 'submitted', lease_expires_at = lease_renewed_at, updated_at = ?2 WHERE id = ?1", params![task_id, now()])?;
        tx.commit()?;
        self.task(task_id)
    }

    pub fn freeze_batch(&mut self, session_id: &str) -> Result<Batch> {
        let tx = self.connection.transaction()?;
        require_session_status(&tx, session_id, "active")?;
        let batch_id = collecting_batch(&tx, session_id)?
            .ok_or_else(|| invalid("batch_not_collecting", session_id))?;
        let unfinished: i64 = tx.query_row(
            "SELECT COUNT(*) FROM batch_members m JOIN tasks t ON t.id=m.task_id WHERE m.batch_id=?1 AND t.status!='submitted'",
            [&batch_id], |row| row.get(0),
        )?;
        if unfinished != 0 {
            return Err(invalid(
                "active_workers",
                "all batch tasks must be submitted",
            ));
        }
        let now = now();
        tx.execute(
            "UPDATE batches SET status='frozen', frozen_at=?2, updated_at=?2 WHERE id=?1",
            params![batch_id, now],
        )?;
        tx.commit()?;
        self.batch(&batch_id)
    }

    pub fn validate_batch(&mut self, batch_id: &str, passed: bool) -> Result<Batch> {
        let status = if passed {
            "validated"
        } else {
            "validation_failed"
        };
        let changed = self.connection.execute(
            "UPDATE batches SET status=?2, updated_at=?3 WHERE id=?1 AND status='frozen'",
            params![batch_id, status, now()],
        )?;
        if changed != 1 {
            return Err(invalid("batch_not_frozen", batch_id));
        }
        self.batch(batch_id)
    }

    pub fn commit_batch(&mut self, batch_id: &str) -> Result<Batch> {
        let tx = self.connection.transaction()?;
        let session_id: Option<String> = tx
            .query_row(
                "SELECT session_id FROM batches WHERE id=?1 AND status='validated'",
                [batch_id],
                |row| row.get(0),
            )
            .optional()?;
        let session_id = session_id.ok_or_else(|| invalid("batch_not_validated", batch_id))?;
        tx.execute("UPDATE tasks SET status='committed', assignment_token='', updated_at=?2 WHERE id IN (SELECT task_id FROM batch_members WHERE batch_id=?1)", params![batch_id, now()])?;
        tx.execute("UPDATE claims SET released=1 WHERE task_id IN (SELECT task_id FROM batch_members WHERE batch_id=?1)", [batch_id])?;
        tx.execute(
            "UPDATE batches SET status='committed', updated_at=?2 WHERE id=?1",
            params![batch_id, now()],
        )?;
        let mut statement = tx.prepare(
            "SELECT id, prerequisites FROM tasks WHERE session_id=?1 AND status='planned'",
        )?;
        let planned = statement
            .query_map([&session_id], |row| {
                Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?))
            })?
            .collect::<std::result::Result<Vec<_>, _>>()?;
        drop(statement);
        for (id, encoded) in planned {
            let prerequisites: Vec<String> = serde_json::from_str(&encoded)?;
            let mut ready = true;
            for prerequisite in prerequisites {
                let status: String = tx.query_row(
                    "SELECT status FROM tasks WHERE id=?1",
                    [prerequisite],
                    |row| row.get(0),
                )?;
                ready &= status == "committed";
            }
            if ready {
                tx.execute(
                    "UPDATE tasks SET status='ready', updated_at=?2 WHERE id=?1",
                    params![id, now()],
                )?;
            }
        }
        tx.commit()?;
        self.batch(batch_id)
    }

    pub fn record_integrity_violation(
        &mut self,
        session_id: &str,
        kind: &str,
        path: &str,
        observed_state: serde_json::Value,
    ) -> Result<IntegrityViolation> {
        require_text("integrity kind", kind)?;
        let tx = self.connection.transaction()?;
        require_session_status(&tx, session_id, "active")?;
        let detected = now();
        tx.execute("INSERT INTO integrity_violations(session_id, kind, path, observed_state, detected_at) VALUES(?1, ?2, ?3, ?4, ?5)", params![session_id, kind, path, observed_state.to_string(), detected])?;
        let id = tx.last_insert_rowid();
        tx.execute(
            "UPDATE sessions SET status='paused', updated_at=?2 WHERE id=?1",
            params![session_id, now()],
        )?;
        tx.commit()?;
        self.integrity_violation(id)
    }

    pub fn recover_integrity(
        &mut self,
        session_id: &str,
        violation_id: i64,
        confirmation: &str,
    ) -> Result<Session> {
        require_text("recovery confirmation", confirmation)?;
        let tx = self.connection.transaction()?;
        require_session_status(&tx, session_id, "paused")?;
        let changed = tx.execute("UPDATE integrity_violations SET recovered_at=?3, recovery_confirmation=?4 WHERE id=?1 AND session_id=?2 AND recovered_at=''", params![violation_id, session_id, now(), confirmation])?;
        if changed != 1 {
            return Err(invalid(
                "integrity_violation_not_found",
                violation_id.to_string(),
            ));
        }
        let unresolved: i64 = tx.query_row(
            "SELECT COUNT(*) FROM integrity_violations WHERE session_id=?1 AND recovered_at=''",
            [session_id],
            |row| row.get(0),
        )?;
        if unresolved == 0 {
            tx.execute(
                "UPDATE sessions SET status='active', updated_at=?2 WHERE id=?1",
                params![session_id, now()],
            )?;
        }
        tx.commit()?;
        self.session(session_id)
    }

    pub fn finish_session(&mut self, session_id: &str) -> Result<Session> {
        let tx = self.connection.transaction()?;
        require_session_status(&tx, session_id, "active")?;
        let incomplete: i64 = tx.query_row(
            "SELECT COUNT(*) FROM tasks WHERE session_id=?1 AND status!='committed'",
            [session_id],
            |row| row.get(0),
        )?;
        if incomplete != 0 {
            return Err(invalid("tasks_incomplete", "all tasks must be committed"));
        }
        let unresolved: i64 = tx.query_row(
            "SELECT COUNT(*) FROM integrity_violations WHERE session_id=?1 AND recovered_at=''",
            [session_id],
            |row| row.get(0),
        )?;
        if unresolved != 0 {
            return Err(invalid("integrity_recovery_required", session_id));
        }
        tx.execute(
            "UPDATE sessions SET status='completed', updated_at=?2 WHERE id=?1",
            params![session_id, now()],
        )?;
        tx.commit()?;
        self.session(session_id)
    }

    pub fn task(&self, id: &str) -> Result<Task> {
        let mut task = self.connection.query_row(
            "SELECT id, creation_order, title, intent, expected_outcome, prerequisites, status,
             worker_identity, assignment_token, batch_id, lease_renewed_at, lease_expires_at, created_at, updated_at
             FROM tasks WHERE id=?1", [id], |row| {
                let prerequisites: String = row.get(5)?;
                let renewed: String = row.get(10)?;
                let expires: String = row.get(11)?;
                Ok(Task {
                    id: row.get(0)?, creation_order: row.get(1)?, title: row.get(2)?, intent: row.get(3)?,
                    expected_outcome: row.get(4)?, prerequisites: serde_json::from_str(&prerequisites).unwrap_or_default(),
                    status: row.get(6)?, worker_identity: row.get(7)?, assignment_token: row.get(8)?, core_frozen: true,
                    batch_id: row.get(9)?, commit_sha: String::new(),
                    lease: if renewed.is_empty() { None } else { Some(WorkerLease { status: if expires <= now() { "closed".into() } else { "active".into() }, renewed_at: renewed, expires_at: expires }) },
                    claims: Vec::new(), ownership_evidence: Vec::new(), focused_validation: Vec::new(), submission: None,
                    created_at: row.get(12)?, updated_at: row.get(13)?, audit_history: Vec::new(),
                })
            },
        ).optional()?.ok_or_else(|| invalid("task_not_found", id))?;
        let mut statement = self
            .connection
            .prepare("SELECT path FROM claims WHERE task_id=?1 ORDER BY path")?;
        task.claims = statement
            .query_map([id], |row| row.get::<_, String>(0))?
            .map(|result| {
                result.map(|path| Claim {
                    path,
                    baseline: absent_snapshot(),
                    submitted_snapshot: None,
                })
            })
            .collect::<std::result::Result<Vec<_>, _>>()?;
        if task.status == "submitted" || task.status == "committed" {
            task.submission = Some(Submission {
                outcome: if task.status == "committed" {
                    "committed".into()
                } else {
                    "pending_changes".into()
                },
                no_changes: false,
                behavior_changed: "submitted changes".into(),
                key_decisions: "see task".into(),
                validation_expectations: "focused checks".into(),
                known_risks: "none recorded".into(),
                submitted_at: task.updated_at.clone(),
            });
        }
        Ok(task)
    }

    pub fn batch(&self, id: &str) -> Result<Batch> {
        let mut batch = self.connection.query_row("SELECT id, creation_order, base_branch, base_commit, status, frozen_at, created_at, updated_at FROM batches WHERE id=?1", [id], |row| Ok(Batch {
            id: row.get(0)?, creation_order: row.get(1)?, base_branch: row.get(2)?, base_commit: row.get(3)?, status: row.get(4)?, frozen_at: row.get(5)?, members: Vec::new(), manifest: Vec::new(), validation: Vec::new(), audit_history: Vec::<BatchAuditEvent>::new(), created_at: row.get(6)?, updated_at: row.get(7)?,
        })).optional()?.ok_or_else(|| invalid("batch_not_found", id))?;
        let mut statement = self.connection.prepare("SELECT t.id, m.membership_order, t.creation_order, t.status FROM batch_members m JOIN tasks t ON t.id=m.task_id WHERE m.batch_id=?1 ORDER BY m.membership_order")?;
        batch.members = statement
            .query_map([id], |row| {
                Ok(BatchMember {
                    task_id: row.get(0)?,
                    membership_order: row.get(1)?,
                    task_creation_order: row.get(2)?,
                    status: row.get(3)?,
                    submission_outcome: String::new(),
                })
            })?
            .collect::<std::result::Result<Vec<_>, _>>()?;
        Ok(batch)
    }

    fn integrity_violations(&self, session_id: &str) -> Result<Vec<IntegrityViolation>> {
        let mut statement = self.connection.prepare("SELECT id, kind, path, observed_state, detected_at, recovered_at, recovery_confirmation FROM integrity_violations WHERE session_id=?1 ORDER BY id")?;
        let violations = statement
            .query_map([session_id], map_violation)?
            .collect::<std::result::Result<Vec<_>, _>>()?;
        Ok(violations)
    }

    fn integrity_violation(&self, id: i64) -> Result<IntegrityViolation> {
        self.connection.query_row("SELECT id, kind, path, observed_state, detected_at, recovered_at, recovery_confirmation FROM integrity_violations WHERE id=?1", [id], map_violation).map_err(Into::into)
    }
}

fn map_violation(row: &rusqlite::Row<'_>) -> rusqlite::Result<IntegrityViolation> {
    let observed: String = row.get(3)?;
    Ok(IntegrityViolation {
        id: row.get(0)?,
        kind: row.get(1)?,
        path: row.get(2)?,
        observed_state: serde_json::from_str(&observed).unwrap_or(serde_json::Value::Null),
        detected_at: row.get(4)?,
        recovered_at: row.get(5)?,
        recovery_confirmation: row.get(6)?,
    })
}

fn authorize(
    tx: &Transaction<'_>,
    task_id: &str,
    token: &str,
    expected_status: &str,
) -> Result<()> {
    let authority: Option<(String, String)> = tx
        .query_row(
            "SELECT status, assignment_token FROM tasks WHERE id=?1",
            [task_id],
            |row| Ok((row.get(0)?, row.get(1)?)),
        )
        .optional()?;
    match authority {
        Some((status, stored))
            if status == expected_status && !token.is_empty() && token == stored =>
        {
            Ok(())
        }
        Some((status, _)) if status != expected_status => {
            Err(invalid("invalid_task_status", status))
        }
        _ => Err(invalid("assignment_token_invalid", task_id)),
    }
}

fn require_session_status(tx: &Transaction<'_>, id: &str, expected: &str) -> Result<()> {
    let status: Option<String> = tx
        .query_row("SELECT status FROM sessions WHERE id=?1", [id], |row| {
            row.get(0)
        })
        .optional()?;
    match status {
        Some(status) if status == expected => Ok(()),
        Some(status) => Err(invalid("invalid_session_status", status)),
        None => Err(invalid("session_not_found", id)),
    }
}

fn task_session_baseline(tx: &Transaction<'_>, task_id: &str) -> Result<(String, String, String)> {
    tx.query_row("SELECT s.id, s.starting_branch, s.starting_commit FROM tasks t JOIN sessions s ON s.id=t.session_id WHERE t.id=?1", [task_id], |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?))).map_err(Into::into)
}

fn collecting_batch(tx: &Transaction<'_>, session_id: &str) -> Result<Option<String>> {
    Ok(tx.query_row("SELECT id FROM batches WHERE session_id=?1 AND status='collecting' ORDER BY creation_order LIMIT 1", [session_id], |row| row.get(0)).optional()?)
}

fn absent_snapshot() -> PathSnapshot {
    PathSnapshot {
        presence: "absent".into(),
        file_type: "absent".into(),
        content_hash: String::new(),
        executable: false,
    }
}

fn require_text(name: &str, value: &str) -> Result<()> {
    if value.trim().is_empty() {
        Err(invalid(
            "invalid_arguments",
            format!("{name} must not be empty"),
        ))
    } else {
        Ok(())
    }
}

fn invalid(code: &str, message: impl Into<String>) -> BandmasterError {
    BandmasterError::Invalid(format!("{code}: {}", message.into()))
}

fn now() -> String {
    timestamp(Utc::now())
}

fn timestamp(value: chrono::DateTime<Utc>) -> String {
    value.to_rfc3339_opts(SecondsFormat::Nanos, true)
}

fn random_id(prefix: &str, bytes: usize) -> String {
    let mut value = vec![0_u8; bytes];
    thread_rng().fill_bytes(&mut value);
    let encoded: String = value.iter().map(|byte| format!("{byte:02x}")).collect();
    format!("{prefix}_{encoded}")
}
