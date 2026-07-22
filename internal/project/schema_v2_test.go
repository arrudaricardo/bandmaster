package project

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver"
)

func TestAgentBatchTaskMigrationPreservesIdentityOrderAndRecoveryEvidence(t *testing.T) {
	db := openVocabularyMigrationFixture(t, false)
	if projectError := migrateAgentBatchTaskVocabulary(db); projectError != nil {
		t.Fatalf("migrate v1 vocabulary: %+v", projectError)
	}
	assertColumn(t, db, "tasks", "agent_identity", true)
	assertColumn(t, db, "tasks", "worker_identity", false)
	assertColumn(t, db, "task_audit_events", "agent_identity", true)
	assertColumn(t, db, "batch_tasks", "task_order", true)
	assertColumn(t, db, "frozen_batch_paths", "task_order", true)
	assertColumn(t, db, "frozen_batch_paths", "membership_order", false)
	if exists, err := schemaTableExists(db, "batch_members"); err != nil || exists {
		t.Fatalf("legacy Batch Member table remains: exists=%t err=%v", exists, err)
	}
	var identity, taskID, recoveryMethod string
	var order, schemaVersion int
	if err := db.QueryRow(`SELECT agent_identity FROM tasks WHERE id = 'task-1'`).Scan(&identity); err != nil || identity != "agent-one" {
		t.Fatalf("migrated Task identity = %q, err=%v", identity, err)
	}
	if err := db.QueryRow(`SELECT task_id, task_order FROM batch_tasks WHERE batch_id = 'batch-1'`).Scan(&taskID, &order); err != nil || taskID != "task-1" || order != 7 {
		t.Fatalf("migrated Batch Task = (%q,%d), err=%v", taskID, order, err)
	}
	if err := db.QueryRow(`SELECT recovery_method FROM task_recovery_events WHERE task_audit_sequence = 1`).Scan(&recoveryMethod); err != nil || recoveryMethod != "agent_handle" {
		t.Fatalf("migrated recovery method = %q, err=%v", recoveryMethod, err)
	}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&schemaVersion); err != nil || schemaVersion != 2 {
		t.Fatalf("schema version = %d, err=%v", schemaVersion, err)
	}
}

func TestAgentBatchTaskMigrationRollsBackOnFailure(t *testing.T) {
	db := openVocabularyMigrationFixture(t, true)
	if projectError := migrateAgentBatchTaskVocabulary(db); projectError == nil {
		t.Fatal("malformed legacy Batch Task table unexpectedly migrated")
	}
	assertColumn(t, db, "tasks", "worker_identity", true)
	assertColumn(t, db, "tasks", "agent_identity", false)
	if exists, err := schemaTableExists(db, "batch_members"); err != nil || !exists {
		t.Fatalf("rollback did not preserve legacy Batch Member table: exists=%t err=%v", exists, err)
	}
	if exists, err := schemaTableExists(db, "batch_tasks"); err != nil || exists {
		t.Fatalf("failed migration left a partial v2 Batch Task table: exists=%t err=%v", exists, err)
	}
	var identity string
	if err := db.QueryRow(`SELECT worker_identity FROM tasks WHERE id = 'task-1'`).Scan(&identity); err != nil || identity != "agent-one" {
		t.Fatalf("rollback did not preserve Task evidence: identity=%q err=%v", identity, err)
	}
}

func openVocabularyMigrationFixture(t *testing.T, malformedBatchTasks bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	statements := []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE tasks(id TEXT PRIMARY KEY, worker_identity TEXT)`,
		`CREATE TABLE task_audit_events(sequence INTEGER PRIMARY KEY, worker_identity TEXT)`,
		`CREATE TABLE batches(id TEXT PRIMARY KEY)`,
		`CREATE TABLE batch_tasks(batch_id TEXT, task_id TEXT, task_order INTEGER)`,
		`CREATE TABLE frozen_batch_paths(batch_id TEXT, task_id TEXT, membership_order INTEGER)`,
		`CREATE TABLE task_recovery_events(task_audit_sequence INTEGER PRIMARY KEY REFERENCES task_audit_events(sequence), recovery_method TEXT NOT NULL CHECK(recovery_method IN ('worker_handle','user_confirmation')), user_confirmation TEXT, replacement_assignment_token TEXT)`,
		`CREATE TABLE task_repair_events(task_audit_sequence INTEGER PRIMARY KEY REFERENCES task_audit_events(sequence), diagnosis TEXT NOT NULL, intended_repair TEXT NOT NULL, recovery_method TEXT NOT NULL CHECK(recovery_method IN ('worker_handle','user_confirmation')), user_confirmation TEXT, replacement_assignment_token TEXT, invalidated_submission_json TEXT)`,
		`CREATE TABLE task_repair_snapshots(task_audit_sequence INTEGER NOT NULL REFERENCES task_repair_events(task_audit_sequence), task_id TEXT NOT NULL REFERENCES tasks(id), path TEXT NOT NULL, presence TEXT NOT NULL, file_type TEXT NOT NULL, content_hash TEXT, executable INTEGER NOT NULL, content BLOB, invalidated_presence TEXT, invalidated_type TEXT, invalidated_content_hash TEXT, invalidated_executable INTEGER, invalidated_content BLOB, PRIMARY KEY(task_audit_sequence,path))`,
		`INSERT INTO tasks VALUES('task-1','agent-one')`,
		`INSERT INTO task_audit_events VALUES(1,'agent-one')`,
		`INSERT INTO batches VALUES('batch-1')`,
		`INSERT INTO task_recovery_events VALUES(1,'worker_handle',NULL,'replacement-token')`,
	}
	if malformedBatchTasks {
		statements = append(statements, `CREATE TABLE batch_members(batch_id TEXT, task_id TEXT)`, `INSERT INTO batch_members VALUES('batch-1','task-1')`)
	} else {
		statements = append(statements, `CREATE TABLE batch_members(batch_id TEXT, task_id TEXT, membership_order INTEGER)`, `INSERT INTO batch_members VALUES('batch-1','task-1',7)`)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("prepare migration fixture with %q: %v", statement, err)
		}
	}
	return db
}

func assertColumn(t *testing.T, db *sql.DB, table, column string, want bool) {
	t.Helper()
	found, projectError := schemaColumnExists(db, table, column)
	if projectError != nil || found != want {
		t.Fatalf("column %s.%s exists=%t, want %t, err=%+v", table, column, found, want, projectError)
	}
}
