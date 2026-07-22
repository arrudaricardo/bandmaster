package project

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

func (p *Project) openState() (*sql.DB, *Error) {
	if projectError := validateLocalPath(p.GitDir, filepath.Join("bandmaster", "state.db")); projectError != nil {
		return nil, projectError
	}
	stateDir := filepath.Join(p.GitDir, "bandmaster")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, internal("create runtime state directory", err)
	}
	stateURL := (&url.URL{Scheme: "file", Path: filepath.Join(stateDir, "state.db")}).String() + "?_txlock=immediate"
	db, err := sql.Open("sqlite3", stateURL)
	if err != nil {
		return nil, internal("open runtime state", err)
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			status TEXT NOT NULL CHECK (status IN ('active', 'paused', 'finalizing', 'completed', 'aborting', 'aborted')),
			starting_branch TEXT NOT NULL,
			starting_commit TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS one_open_session ON sessions((1)) WHERE status IN ('active', 'paused', 'finalizing', 'aborting')`,
		`CREATE TABLE IF NOT EXISTS audit_events (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			event TEXT NOT NULL,
			from_status TEXT,
			to_status TEXT NOT NULL,
			occurred_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS session_monitors (
			session_id TEXT NOT NULL REFERENCES sessions(id),
			generation INTEGER NOT NULL,
			process_id INTEGER,
			process_identity TEXT NOT NULL,
			process_start_identity TEXT,
			status TEXT NOT NULL CHECK (status IN ('starting', 'healthy', 'unhealthy', 'stopped')),
			started_at TEXT NOT NULL,
			heartbeat_at TEXT,
			last_full_scan_at TEXT,
			PRIMARY KEY(session_id, generation),
			UNIQUE(process_identity)
		)`,
		`CREATE TABLE IF NOT EXISTS integrity_violations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			kind TEXT NOT NULL,
			path TEXT NOT NULL DEFAULT '',
			observed_state_json TEXT NOT NULL,
			detected_at TEXT NOT NULL,
			recovered_at TEXT,
			recovery_confirmation TEXT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS one_unresolved_integrity_violation ON integrity_violations(session_id, kind, path) WHERE recovered_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS integrity_audit_events (
			audit_sequence INTEGER PRIMARY KEY REFERENCES audit_events(sequence),
			violation_id INTEGER NOT NULL UNIQUE REFERENCES integrity_violations(id),
			kind TEXT NOT NULL,
			path TEXT NOT NULL,
			observed_state_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS session_completion_checks (
			session_id TEXT PRIMARY KEY REFERENCES sessions(id),
			full_scan_at TEXT NOT NULL,
			monitor_stopped_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS session_abort_events (
			session_id TEXT PRIMARY KEY REFERENCES sessions(id),
			termination_confirmation TEXT NOT NULL,
			occurred_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS integrity_quarantines (
			violation_id INTEGER NOT NULL REFERENCES integrity_violations(id),
			task_id TEXT REFERENCES tasks(id),
			batch_id TEXT REFERENCES batches(id),
			previous_status TEXT NOT NULL,
			CHECK ((task_id IS NULL) != (batch_id IS NULL)),
			UNIQUE(violation_id, task_id),
			UNIQUE(violation_id, batch_id)
		)`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			creation_order INTEGER NOT NULL,
			title TEXT NOT NULL,
			intent TEXT NOT NULL,
			expected_outcome TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('planned', 'ready', 'assigned', 'editing', 'blocked', 'submitted', 'repair_pending', 'quarantined', 'committed', 'no_op', 'canceled')),
			agent_identity TEXT,
			assignment_token TEXT,
			core_frozen INTEGER NOT NULL DEFAULT 0 CHECK (core_frozen IN (0, 1)),
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(session_id, creation_order)
		)`,
		`CREATE TABLE IF NOT EXISTS task_dependencies (
			task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			prerequisite_id TEXT NOT NULL REFERENCES tasks(id),
			dependency_order INTEGER NOT NULL,
			PRIMARY KEY(task_id, prerequisite_id),
			UNIQUE(task_id, dependency_order)
		)`,
		`CREATE TABLE IF NOT EXISTS task_audit_events (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			event TEXT NOT NULL,
			from_status TEXT,
			to_status TEXT NOT NULL,
			agent_identity TEXT,
			termination_proof TEXT,
			occurred_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_recovery_events (
			task_audit_sequence INTEGER PRIMARY KEY REFERENCES task_audit_events(sequence),
			recovery_method TEXT NOT NULL CHECK (recovery_method IN ('agent_handle', 'user_confirmation')),
			user_confirmation TEXT,
			replacement_assignment_token TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS task_leases (
			task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
			status TEXT NOT NULL CHECK (status IN ('active', 'expired', 'closed')),
			duration_nanos INTEGER NOT NULL CHECK (duration_nanos > 0),
			renewed_at TEXT NOT NULL,
			expires_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS batches (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			creation_order INTEGER NOT NULL,
			base_branch TEXT NOT NULL,
			base_commit TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('collecting', 'frozen', 'validating', 'repair_pending', 'repairing', 'finalizing', 'final_validating', 'committed', 'quarantined', 'abandoned')),
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(session_id, creation_order)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS one_collecting_batch ON batches(session_id) WHERE status = 'collecting'`,
		`CREATE TABLE IF NOT EXISTS batch_tasks (
			batch_id TEXT NOT NULL REFERENCES batches(id),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			task_order INTEGER NOT NULL,
			PRIMARY KEY(batch_id, task_id),
			UNIQUE(batch_id, task_order),
			UNIQUE(task_id)
		)`,
		`CREATE TABLE IF NOT EXISTS batch_freezes (
			batch_id TEXT PRIMARY KEY REFERENCES batches(id),
			frozen_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS batch_audit_events (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			batch_id TEXT NOT NULL REFERENCES batches(id),
			event TEXT NOT NULL,
			from_status TEXT,
			to_status TEXT NOT NULL,
			occurred_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS claims (
			session_id TEXT NOT NULL REFERENCES sessions(id),
			batch_id TEXT NOT NULL REFERENCES batches(id),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			claim_order INTEGER NOT NULL,
			path TEXT NOT NULL,
			baseline_presence TEXT NOT NULL CHECK (baseline_presence IN ('absent', 'present')),
			baseline_type TEXT NOT NULL CHECK (baseline_type IN ('absent', 'regular_file', 'symlink')),
			baseline_content_hash TEXT,
			baseline_executable INTEGER NOT NULL CHECK (baseline_executable IN (0, 1)),
			baseline_content BLOB,
			claimed_at TEXT NOT NULL,
			PRIMARY KEY(session_id, path),
			UNIQUE(task_id, path),
			UNIQUE(task_id, claim_order)
		)`,
		`CREATE TABLE IF NOT EXISTS task_path_ownership (
			session_id TEXT NOT NULL REFERENCES sessions(id),
			batch_id TEXT NOT NULL REFERENCES batches(id),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			claim_order INTEGER NOT NULL,
			path TEXT NOT NULL,
			baseline_presence TEXT NOT NULL CHECK (baseline_presence IN ('absent', 'present')),
			baseline_type TEXT NOT NULL CHECK (baseline_type IN ('absent', 'regular_file', 'symlink')),
			baseline_content_hash TEXT,
			baseline_executable INTEGER NOT NULL CHECK (baseline_executable IN (0, 1)),
			baseline_content BLOB,
			claimed_at TEXT NOT NULL,
			PRIMARY KEY(task_id, path)
		)`,
		`CREATE TABLE IF NOT EXISTS focused_validations (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			validation_order INTEGER NOT NULL,
			name TEXT NOT NULL,
			argv_json TEXT,
			script TEXT,
			working_directory TEXT NOT NULL,
			timeout TEXT NOT NULL,
			environment_json TEXT NOT NULL,
			PRIMARY KEY(task_id, validation_order),
			UNIQUE(task_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS task_submissions (
			task_id TEXT PRIMARY KEY REFERENCES tasks(id),
			outcome TEXT NOT NULL CHECK (outcome IN ('pending_changes', 'pending_no_op')),
			no_changes INTEGER NOT NULL CHECK (no_changes IN (0, 1)),
			behavior_changed TEXT NOT NULL,
			key_decisions TEXT NOT NULL,
			validation_expectations TEXT NOT NULL,
			known_risks TEXT NOT NULL,
			submitted_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS submitted_snapshots (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			path TEXT NOT NULL,
			presence TEXT NOT NULL CHECK (presence IN ('absent', 'present')),
			file_type TEXT NOT NULL CHECK (file_type IN ('absent', 'regular_file', 'symlink')),
			content_hash TEXT,
			executable INTEGER NOT NULL CHECK (executable IN (0, 1)),
			content BLOB,
			PRIMARY KEY(task_id, path),
			FOREIGN KEY(task_id, path) REFERENCES task_path_ownership(task_id, path)
		)`,
		`CREATE TABLE IF NOT EXISTS frozen_batch_paths (
			batch_id TEXT NOT NULL REFERENCES batches(id),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			task_order INTEGER NOT NULL,
			claim_order INTEGER NOT NULL,
			path TEXT NOT NULL,
			baseline_presence TEXT NOT NULL CHECK (baseline_presence IN ('absent', 'present')),
			baseline_type TEXT NOT NULL CHECK (baseline_type IN ('absent', 'regular_file', 'symlink')),
			baseline_content_hash TEXT,
			baseline_executable INTEGER NOT NULL CHECK (baseline_executable IN (0, 1)),
			baseline_content BLOB,
			submitted_presence TEXT NOT NULL CHECK (submitted_presence IN ('absent', 'present')),
			submitted_type TEXT NOT NULL CHECK (submitted_type IN ('absent', 'regular_file', 'symlink')),
			submitted_content_hash TEXT,
			submitted_executable INTEGER NOT NULL CHECK (submitted_executable IN (0, 1)),
			submitted_content BLOB,
			PRIMARY KEY(batch_id, path),
			UNIQUE(batch_id, task_id, claim_order)
		)`,
		`CREATE TABLE IF NOT EXISTS batch_validation_attempts (
			batch_id TEXT NOT NULL REFERENCES batches(id),
			attempt INTEGER NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('running', 'passed', 'failed', 'integrity_violation')),
			started_at TEXT NOT NULL,
			finished_at TEXT,
			PRIMARY KEY(batch_id, attempt)
		)`,
		`CREATE TABLE IF NOT EXISTS batch_validation_runs (
			batch_id TEXT NOT NULL REFERENCES batches(id),
			attempt INTEGER NOT NULL,
			command_order INTEGER NOT NULL,
			source TEXT NOT NULL CHECK (source IN ('focused', 'repository')),
			task_id TEXT REFERENCES tasks(id),
			name TEXT NOT NULL,
			argv_json TEXT,
			script TEXT,
			resolved_argv_json TEXT NOT NULL,
			working_directory TEXT NOT NULL,
			resolved_working_directory TEXT NOT NULL,
			timeout TEXT NOT NULL,
			environment_overrides_json TEXT NOT NULL,
			resolved_environment_json TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('passed', 'failed', 'timed_out', 'start_failed', 'integrity_violation')),
			exit_code INTEGER,
			duration_nanos INTEGER NOT NULL,
			stdout TEXT NOT NULL,
			stderr TEXT NOT NULL,
			stdout_truncated INTEGER NOT NULL CHECK (stdout_truncated IN (0, 1)),
			stderr_truncated INTEGER NOT NULL CHECK (stderr_truncated IN (0, 1)),
			started_at TEXT NOT NULL,
			finished_at TEXT NOT NULL,
			PRIMARY KEY(batch_id, attempt, command_order),
			FOREIGN KEY(batch_id, attempt) REFERENCES batch_validation_attempts(batch_id, attempt)
		)`,
		`CREATE TABLE IF NOT EXISTS finalization_journals (
			batch_id TEXT PRIMARY KEY REFERENCES batches(id),
			session_id TEXT NOT NULL REFERENCES sessions(id),
			expected_branch TEXT NOT NULL,
			pre_batch_commit TEXT NOT NULL,
			commit_plan_json TEXT NOT NULL,
			step TEXT NOT NULL CHECK (step IN ('prepared', 'committing', 'validating')),
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS finalization_recovery_events (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			batch_id TEXT NOT NULL REFERENCES batches(id),
			session_id TEXT NOT NULL REFERENCES sessions(id),
			journal_created_at TEXT NOT NULL,
			journal_step TEXT NOT NULL,
			classification TEXT NOT NULL CHECK (classification IN ('recognized', 'unknown')),
			action TEXT NOT NULL CHECK (action IN ('rollback', 'quarantine')),
			outcome TEXT NOT NULL CHECK (outcome IN ('rolled_back', 'quarantined')),
			operator_confirmation TEXT,
			before_state_json TEXT NOT NULL,
			after_state_json TEXT NOT NULL,
			journal_evidence_json TEXT NOT NULL,
			result_json TEXT NOT NULL,
			occurred_at TEXT NOT NULL,
			UNIQUE(batch_id, journal_created_at)
		)`,
		`CREATE TABLE IF NOT EXISTS batch_abandonment_events (
			batch_id TEXT PRIMARY KEY REFERENCES batches(id),
			session_id TEXT NOT NULL REFERENCES sessions(id),
			reason TEXT NOT NULL,
			confirmation TEXT NOT NULL,
			before_state_json TEXT NOT NULL,
			after_state_json TEXT NOT NULL,
			evidence_json TEXT NOT NULL,
			result_json TEXT NOT NULL,
			occurred_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_commits (
			batch_id TEXT NOT NULL REFERENCES batches(id),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			commit_sha TEXT NOT NULL,
			committed_at TEXT NOT NULL,
			PRIMARY KEY(batch_id, task_id)
		)`,
		`CREATE TABLE IF NOT EXISTS task_diff_reviews (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			path TEXT NOT NULL,
			presence TEXT NOT NULL CHECK (presence IN ('absent', 'present')),
			file_type TEXT NOT NULL CHECK (file_type IN ('absent', 'regular_file', 'symlink')),
			content_hash TEXT,
			executable INTEGER NOT NULL CHECK (executable IN (0, 1)),
			reviewed_at TEXT NOT NULL,
			PRIMARY KEY(task_id, path),
			FOREIGN KEY(task_id, path) REFERENCES task_path_ownership(task_id, path)
		)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, internal("initialize runtime state", err)
		}
	}
	if projectError := migrateIntegrityMonitorSchema(db); projectError != nil {
		db.Close()
		return nil, projectError
	}
	if projectError := migratePathSnapshotSchema(db); projectError != nil {
		db.Close()
		return nil, projectError
	}
	if projectError := migrateOwnershipEvidenceSchema(db); projectError != nil {
		db.Close()
		return nil, projectError
	}
	if projectError := migrateBatchAbandonmentSchema(db); projectError != nil {
		db.Close()
		return nil, projectError
	}
	if projectError := migrateFinalizationRecoverySchema(db); projectError != nil {
		db.Close()
		return nil, projectError
	}
	if projectError := initializeRepairSchema(db); projectError != nil {
		db.Close()
		return nil, projectError
	}
	if projectError := migrateAgentBatchTaskVocabulary(db); projectError != nil {
		db.Close()
		return nil, projectError
	}
	return db, nil
}

// migrateAgentBatchTaskVocabulary performs the v2 vocabulary cutover in one
// transaction. Agents remain derived from Task evidence; this migration only
// renames persisted attribution and ordered Batch Task records.
func migrateAgentBatchTaskVocabulary(db *sql.DB) *Error {
	legacyTasks, projectError := schemaColumnExists(db, "tasks", "worker_identity")
	if projectError != nil {
		return projectError
	}
	legacyBatchTasks, err := schemaTableExists(db, "batch_members")
	if err != nil {
		return internal("inspect legacy Batch Task schema", err)
	}
	if !legacyTasks && !legacyBatchTasks {
		if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
			return internal("record state schema version 2", err)
		}
		return nil
	}

	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return internal("prepare Agent and Batch Task schema migration", err)
	}
	defer func() { _, _ = db.Exec(`PRAGMA foreign_keys = ON`) }()
	tx, err := db.Begin()
	if err != nil {
		return internal("begin Agent and Batch Task schema migration", err)
	}
	defer tx.Rollback()

	statements := []string{}
	if legacyTasks {
		statements = append(statements,
			`ALTER TABLE tasks RENAME COLUMN worker_identity TO agent_identity`,
			`ALTER TABLE task_audit_events RENAME COLUMN worker_identity TO agent_identity`,
			`ALTER TABLE task_recovery_events RENAME TO task_recovery_events_v1`,
			`CREATE TABLE task_recovery_events (
				task_audit_sequence INTEGER PRIMARY KEY REFERENCES task_audit_events(sequence),
				recovery_method TEXT NOT NULL CHECK (recovery_method IN ('agent_handle', 'user_confirmation')),
				user_confirmation TEXT,
				replacement_assignment_token TEXT
			)`,
			`INSERT INTO task_recovery_events(task_audit_sequence, recovery_method, user_confirmation, replacement_assignment_token)
			 SELECT task_audit_sequence, CASE recovery_method WHEN 'worker_handle' THEN 'agent_handle' ELSE recovery_method END, user_confirmation, replacement_assignment_token FROM task_recovery_events_v1`,
			`DROP TABLE task_recovery_events_v1`,
			`ALTER TABLE task_repair_snapshots RENAME TO task_repair_snapshots_v1`,
			`ALTER TABLE task_repair_events RENAME TO task_repair_events_v1`,
			repairEventsTableSQL,
			repairSnapshotsTableSQL,
			`INSERT INTO task_repair_events(task_audit_sequence, diagnosis, intended_repair, recovery_method, user_confirmation, replacement_assignment_token, invalidated_submission_json)
			 SELECT task_audit_sequence, diagnosis, intended_repair, CASE recovery_method WHEN 'worker_handle' THEN 'agent_handle' ELSE recovery_method END, user_confirmation, replacement_assignment_token, invalidated_submission_json FROM task_repair_events_v1`,
			`INSERT INTO task_repair_snapshots(task_audit_sequence, task_id, path, presence, file_type, content_hash, executable, content, invalidated_presence, invalidated_type, invalidated_content_hash, invalidated_executable, invalidated_content)
			 SELECT task_audit_sequence, task_id, path, presence, file_type, content_hash, executable, content, invalidated_presence, invalidated_type, invalidated_content_hash, invalidated_executable, invalidated_content FROM task_repair_snapshots_v1`,
			`DROP TABLE task_repair_snapshots_v1`,
			`DROP TABLE task_repair_events_v1`,
		)
	}
	if legacyBatchTasks {
		statements = append(statements,
			`INSERT INTO batch_tasks(batch_id, task_id, task_order) SELECT batch_id, task_id, membership_order FROM batch_members`,
			`ALTER TABLE frozen_batch_paths RENAME COLUMN membership_order TO task_order`,
			`DROP TABLE batch_members`,
		)
	}
	statements = append(statements, `PRAGMA user_version = 2`)
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			_ = tx.Rollback()
			removeV2BatchTaskTableAfterFailedMigration(db, legacyBatchTasks)
			return internal("migrate Agent and Batch Task vocabulary", err)
		}
	}
	rows, err := tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		_ = tx.Rollback()
		removeV2BatchTaskTableAfterFailedMigration(db, legacyBatchTasks)
		return internal("verify Agent and Batch Task migration", err)
	}
	if rows.Next() {
		_ = rows.Close()
		_ = tx.Rollback()
		removeV2BatchTaskTableAfterFailedMigration(db, legacyBatchTasks)
		return internal("verify Agent and Batch Task migration", errors.New("foreign-key integrity check failed"))
	}
	if err := rows.Close(); err != nil {
		_ = tx.Rollback()
		removeV2BatchTaskTableAfterFailedMigration(db, legacyBatchTasks)
		return internal("close Agent and Batch Task migration verification", err)
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		removeV2BatchTaskTableAfterFailedMigration(db, legacyBatchTasks)
		return internal("commit Agent and Batch Task schema migration", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return internal("restore foreign keys after Agent and Batch Task migration", err)
	}
	return nil
}

func removeV2BatchTaskTableAfterFailedMigration(db *sql.DB, legacyBatchTasks bool) {
	if legacyBatchTasks {
		_, _ = db.Exec(`DROP TABLE IF EXISTS batch_tasks`)
	}
}

func schemaTableExists(db *sql.DB, table string) (bool, error) {
	var found int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&found)
	return found == 1, err
}

func migrateFinalizationRecoverySchema(db *sql.DB) *Error {
	found, projectError := schemaColumnExists(db, "finalization_recovery_events", "journal_created_at")
	if projectError != nil || found {
		return projectError
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return internal("prepare finalization recovery schema migration", err)
	}
	defer func() { _, _ = db.Exec(`PRAGMA foreign_keys = ON`) }()
	tx, err := db.Begin()
	if err != nil {
		return internal("begin finalization recovery schema migration", err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`ALTER TABLE finalization_recovery_events RENAME TO finalization_recovery_events_legacy`,
		`CREATE TABLE finalization_recovery_events (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			batch_id TEXT NOT NULL REFERENCES batches(id),
			session_id TEXT NOT NULL REFERENCES sessions(id),
			journal_created_at TEXT NOT NULL,
			journal_step TEXT NOT NULL,
			classification TEXT NOT NULL CHECK (classification IN ('recognized', 'unknown')),
			action TEXT NOT NULL CHECK (action IN ('rollback', 'quarantine')),
			outcome TEXT NOT NULL CHECK (outcome IN ('rolled_back', 'quarantined')),
			operator_confirmation TEXT,
			before_state_json TEXT NOT NULL,
			after_state_json TEXT NOT NULL,
			journal_evidence_json TEXT NOT NULL,
			result_json TEXT NOT NULL,
			occurred_at TEXT NOT NULL,
			UNIQUE(batch_id, journal_created_at)
		)`,
		`INSERT INTO finalization_recovery_events(batch_id, session_id, journal_created_at, journal_step, classification, action, outcome, operator_confirmation, before_state_json, after_state_json, journal_evidence_json, result_json, occurred_at)
		 SELECT batch_id, session_id, occurred_at, journal_step, classification, action, outcome, operator_confirmation, before_state_json, after_state_json, journal_evidence_json, result_json, occurred_at FROM finalization_recovery_events_legacy`,
		`DROP TABLE finalization_recovery_events_legacy`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return internal("migrate finalization recovery schema", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return internal("commit finalization recovery schema migration", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return internal("restore foreign keys after finalization recovery migration", err)
	}
	return nil
}

const repairEventsTableSQL = `CREATE TABLE IF NOT EXISTS task_repair_events (
			task_audit_sequence INTEGER PRIMARY KEY REFERENCES task_audit_events(sequence),
			diagnosis TEXT NOT NULL,
			intended_repair TEXT NOT NULL,
			recovery_method TEXT NOT NULL CHECK (recovery_method IN ('agent_handle', 'user_confirmation')),
			user_confirmation TEXT,
			replacement_assignment_token TEXT,
			invalidated_submission_json TEXT
		)`

const repairSnapshotsTableSQL = `CREATE TABLE IF NOT EXISTS task_repair_snapshots (
			task_audit_sequence INTEGER NOT NULL REFERENCES task_repair_events(task_audit_sequence),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			path TEXT NOT NULL,
			presence TEXT NOT NULL CHECK (presence IN ('absent', 'present')),
			file_type TEXT NOT NULL CHECK (file_type IN ('absent', 'regular_file', 'symlink')),
			content_hash TEXT,
			executable INTEGER NOT NULL CHECK (executable IN (0, 1)),
			content BLOB,
			invalidated_presence TEXT CHECK (invalidated_presence IN ('absent', 'present')),
			invalidated_type TEXT CHECK (invalidated_type IN ('absent', 'regular_file', 'symlink')),
			invalidated_content_hash TEXT,
			invalidated_executable INTEGER CHECK (invalidated_executable IN (0, 1)),
			invalidated_content BLOB,
			PRIMARY KEY(task_audit_sequence, path)
		)`

func initializeRepairSchema(db *sql.DB) *Error {
	if _, err := db.Exec(repairEventsTableSQL); err != nil {
		return internal("initialize task repair events", err)
	}
	found, projectError := schemaColumnExists(db, "task_repair_events", "invalidated_submission_json")
	if projectError != nil {
		return projectError
	}
	if !found {
		if _, err := db.Exec(`ALTER TABLE task_repair_events ADD COLUMN invalidated_submission_json TEXT`); err != nil {
			found, inspectError := schemaColumnExists(db, "task_repair_events", "invalidated_submission_json")
			if inspectError != nil || !found {
				return internal("migrate task repair events", err)
			}
		}
	}
	if _, err := db.Exec(repairSnapshotsTableSQL); err != nil {
		return internal("initialize task repair snapshots", err)
	}
	return migrateRepairSnapshotsSchema(db)
}

func migrateRepairSnapshotsSchema(db *sql.DB) *Error {
	var tableSQL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'task_repair_snapshots'`).Scan(&tableSQL); err != nil {
		return internal("inspect task repair snapshot schema", err)
	}
	if strings.Contains(tableSQL, "invalidated_presence") && !strings.Contains(tableSQL, "REFERENCES claims") {
		return nil
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return internal("disable foreign keys for task repair snapshot migration", err)
	}
	foreignKeysDisabled := true
	defer func() {
		if foreignKeysDisabled {
			_, _ = db.Exec(`PRAGMA foreign_keys = ON`)
		}
	}()
	tx, err := db.Begin()
	if err != nil {
		return internal("begin task repair snapshot migration", err)
	}
	defer tx.Rollback()
	var lockedTableSQL string
	if err := tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'task_repair_snapshots'`).Scan(&lockedTableSQL); err != nil {
		return internal("recheck task repair snapshot schema", err)
	}
	if strings.Contains(lockedTableSQL, "invalidated_presence") && !strings.Contains(lockedTableSQL, "REFERENCES claims") {
		if err := tx.Rollback(); err != nil {
			return internal("close completed task repair snapshot migration", err)
		}
		if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
			return internal("restore foreign keys after concurrent task repair snapshot migration", err)
		}
		foreignKeysDisabled = false
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE task_repair_snapshots RENAME TO task_repair_snapshots_legacy`); err != nil {
		return internal("rename legacy task repair snapshots", err)
	}
	if _, err := tx.Exec(repairSnapshotsTableSQL); err != nil {
		return internal("create migrated task repair snapshots", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO task_repair_snapshots(
			task_audit_sequence, task_id, path, presence, file_type, content_hash, executable, content
		)
		SELECT task_audit_sequence, task_id, path, presence, file_type, content_hash, executable, content
		FROM task_repair_snapshots_legacy`); err != nil {
		return internal("copy legacy task repair snapshots", err)
	}
	if _, err := tx.Exec(`DROP TABLE task_repair_snapshots_legacy`); err != nil {
		return internal("drop legacy task repair snapshots", err)
	}
	if err := tx.Commit(); err != nil {
		return internal("commit task repair snapshot migration", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return internal("restore foreign keys after task repair snapshot migration", err)
	}
	foreignKeysDisabled = false
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return internal("verify task repair snapshot migration", err)
	}
	defer rows.Close()
	if rows.Next() {
		return internal("verify task repair snapshot migration", errors.New("foreign key violation after migration"))
	}
	if err := rows.Err(); err != nil {
		return internal("verify task repair snapshot migration", err)
	}
	return nil
}

func schemaColumnExists(db *sql.DB, table, column string) (bool, *Error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, internal("inspect runtime state schema", err)
	}
	found := false
	for rows.Next() {
		var sequence, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&sequence, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return false, internal("read runtime state schema", err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return false, internal("close runtime state schema", err)
	}
	if err := rows.Err(); err != nil {
		return false, internal("inspect runtime state schema", err)
	}
	return found, nil
}

func migrateIntegrityMonitorSchema(db *sql.DB) *Error {
	found, projectError := integrityMonitorStartIdentityColumnExists(db)
	if projectError != nil || found {
		return projectError
	}
	if _, err := db.Exec(`ALTER TABLE session_monitors ADD COLUMN process_start_identity TEXT`); err != nil {
		// Another process may have completed the additive migration while this
		// connection waited for SQLite's schema lock.
		found, inspectError := integrityMonitorStartIdentityColumnExists(db)
		if inspectError == nil && found {
			return nil
		}
		return internal("migrate integrity monitor schema", err)
	}
	return nil
}

func integrityMonitorStartIdentityColumnExists(db *sql.DB) (bool, *Error) {
	rows, err := db.Query(`PRAGMA table_info(session_monitors)`)
	if err != nil {
		return false, internal("inspect integrity monitor schema", err)
	}
	found := false
	for rows.Next() {
		var sequence, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&sequence, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return false, internal("read integrity monitor schema", err)
		}
		if name == "process_start_identity" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, internal("inspect integrity monitor schema", err)
	}
	if err := rows.Close(); err != nil {
		return false, internal("close integrity monitor schema", err)
	}
	return found, nil
}

func migratePathSnapshotSchema(db *sql.DB) *Error {
	var claimsSQL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'claims'`).Scan(&claimsSQL); err != nil {
		return internal("inspect path snapshot schema", err)
	}
	if strings.Contains(claimsSQL, "'symlink'") {
		return nil
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return internal("disable foreign keys for path snapshot migration", err)
	}
	foreignKeysDisabled := true
	defer func() {
		if foreignKeysDisabled {
			_, _ = db.Exec(`PRAGMA foreign_keys = ON`)
		}
	}()
	tx, err := db.Begin()
	if err != nil {
		return internal("begin path snapshot migration", err)
	}
	defer tx.Rollback()
	statements := []string{
		`ALTER TABLE submitted_snapshots RENAME TO submitted_snapshots_regular_only`,
		`ALTER TABLE task_diff_reviews RENAME TO task_diff_reviews_regular_only`,
		`CREATE TABLE claims_with_symlinks (
			session_id TEXT NOT NULL REFERENCES sessions(id),
			batch_id TEXT NOT NULL REFERENCES batches(id),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			claim_order INTEGER NOT NULL,
			path TEXT NOT NULL,
			baseline_presence TEXT NOT NULL CHECK (baseline_presence IN ('absent', 'present')),
			baseline_type TEXT NOT NULL CHECK (baseline_type IN ('absent', 'regular_file', 'symlink')),
			baseline_content_hash TEXT,
			baseline_executable INTEGER NOT NULL CHECK (baseline_executable IN (0, 1)),
			baseline_content BLOB,
			claimed_at TEXT NOT NULL,
			PRIMARY KEY(session_id, path),
			UNIQUE(task_id, path),
			UNIQUE(task_id, claim_order)
		)`,
		`INSERT INTO claims_with_symlinks SELECT * FROM claims`,
		`DROP TABLE claims`,
		`ALTER TABLE claims_with_symlinks RENAME TO claims`,
		`CREATE TABLE submitted_snapshots (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			path TEXT NOT NULL,
			presence TEXT NOT NULL CHECK (presence IN ('absent', 'present')),
			file_type TEXT NOT NULL CHECK (file_type IN ('absent', 'regular_file', 'symlink')),
			content_hash TEXT,
			executable INTEGER NOT NULL CHECK (executable IN (0, 1)),
			content BLOB,
			PRIMARY KEY(task_id, path),
			FOREIGN KEY(task_id, path) REFERENCES claims(task_id, path)
		)`,
		`INSERT INTO submitted_snapshots SELECT * FROM submitted_snapshots_regular_only`,
		`DROP TABLE submitted_snapshots_regular_only`,
		`CREATE TABLE task_diff_reviews (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			path TEXT NOT NULL,
			presence TEXT NOT NULL CHECK (presence IN ('absent', 'present')),
			file_type TEXT NOT NULL CHECK (file_type IN ('absent', 'regular_file', 'symlink')),
			content_hash TEXT,
			executable INTEGER NOT NULL CHECK (executable IN (0, 1)),
			reviewed_at TEXT NOT NULL,
			PRIMARY KEY(task_id, path),
			FOREIGN KEY(task_id, path) REFERENCES claims(task_id, path)
		)`,
		`INSERT INTO task_diff_reviews SELECT * FROM task_diff_reviews_regular_only`,
		`DROP TABLE task_diff_reviews_regular_only`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return internal("migrate path snapshot schema", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return internal("commit path snapshot migration", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return internal("restore foreign keys after path snapshot migration", err)
	}
	foreignKeysDisabled = false
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return internal("verify path snapshot migration", err)
	}
	defer rows.Close()
	if rows.Next() {
		return internal("verify path snapshot migration", errors.New("foreign key violation after migration"))
	}
	if err := rows.Err(); err != nil {
		return internal("verify path snapshot migration", err)
	}
	return nil
}

func migrateOwnershipEvidenceSchema(db *sql.DB) *Error {
	var submittedSQL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'submitted_snapshots'`).Scan(&submittedSQL); err != nil {
		return internal("inspect ownership evidence schema", err)
	}
	if strings.Contains(submittedSQL, "REFERENCES task_path_ownership") {
		return nil
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return internal("disable foreign keys for ownership evidence migration", err)
	}
	foreignKeysDisabled := true
	defer func() {
		if foreignKeysDisabled {
			_, _ = db.Exec(`PRAGMA foreign_keys = ON`)
		}
	}()
	tx, err := db.Begin()
	if err != nil {
		return internal("begin ownership evidence migration", err)
	}
	defer tx.Rollback()
	statements := []string{
		`INSERT OR IGNORE INTO task_path_ownership(
			session_id, batch_id, task_id, claim_order, path,
			baseline_presence, baseline_type, baseline_content_hash, baseline_executable, baseline_content, claimed_at
		) SELECT session_id, batch_id, task_id, claim_order, path,
			baseline_presence, baseline_type, baseline_content_hash, baseline_executable, baseline_content, claimed_at
		FROM claims`,
		`ALTER TABLE submitted_snapshots RENAME TO submitted_snapshots_claim_backed`,
		`CREATE TABLE submitted_snapshots (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			path TEXT NOT NULL,
			presence TEXT NOT NULL CHECK (presence IN ('absent', 'present')),
			file_type TEXT NOT NULL CHECK (file_type IN ('absent', 'regular_file', 'symlink')),
			content_hash TEXT,
			executable INTEGER NOT NULL CHECK (executable IN (0, 1)),
			content BLOB,
			PRIMARY KEY(task_id, path),
			FOREIGN KEY(task_id, path) REFERENCES task_path_ownership(task_id, path)
		)`,
		`INSERT INTO submitted_snapshots SELECT * FROM submitted_snapshots_claim_backed`,
		`DROP TABLE submitted_snapshots_claim_backed`,
		`ALTER TABLE task_diff_reviews RENAME TO task_diff_reviews_claim_backed`,
		`CREATE TABLE task_diff_reviews (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			path TEXT NOT NULL,
			presence TEXT NOT NULL CHECK (presence IN ('absent', 'present')),
			file_type TEXT NOT NULL CHECK (file_type IN ('absent', 'regular_file', 'symlink')),
			content_hash TEXT,
			executable INTEGER NOT NULL CHECK (executable IN (0, 1)),
			reviewed_at TEXT NOT NULL,
			PRIMARY KEY(task_id, path),
			FOREIGN KEY(task_id, path) REFERENCES task_path_ownership(task_id, path)
		)`,
		`INSERT INTO task_diff_reviews SELECT * FROM task_diff_reviews_claim_backed`,
		`DROP TABLE task_diff_reviews_claim_backed`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return internal("migrate ownership evidence schema", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return internal("commit ownership evidence migration", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return internal("restore foreign keys after ownership evidence migration", err)
	}
	foreignKeysDisabled = false
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return internal("verify ownership evidence migration", err)
	}
	defer rows.Close()
	if rows.Next() {
		return internal("verify ownership evidence migration", errors.New("foreign key violation after migration"))
	}
	if err := rows.Err(); err != nil {
		return internal("verify ownership evidence migration", err)
	}
	return nil
}

// migrateBatchAbandonmentSchema widens the batch state check without changing
// child foreign-key targets. legacy_alter_table keeps references pointed at the
// replacement `batches` table while foreign-key enforcement is suspended.
func migrateBatchAbandonmentSchema(db *sql.DB) *Error {
	var tableSQL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'batches'`).Scan(&tableSQL); err != nil {
		return internal("inspect batch abandonment schema", err)
	}
	if strings.Contains(tableSQL, "'abandoned'") {
		return nil
	}
	for _, pragma := range []string{`PRAGMA foreign_keys = OFF`, `PRAGMA legacy_alter_table = ON`} {
		if _, err := db.Exec(pragma); err != nil {
			return internal("prepare batch abandonment schema migration", err)
		}
	}
	defer func() {
		_, _ = db.Exec(`PRAGMA legacy_alter_table = OFF`)
		_, _ = db.Exec(`PRAGMA foreign_keys = ON`)
	}()
	tx, err := db.Begin()
	if err != nil {
		return internal("begin batch abandonment schema migration", err)
	}
	defer tx.Rollback()
	statements := []string{
		`ALTER TABLE batches RENAME TO batches_without_abandoned`,
		`CREATE TABLE batches (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			creation_order INTEGER NOT NULL,
			base_branch TEXT NOT NULL,
			base_commit TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('collecting', 'frozen', 'validating', 'repair_pending', 'repairing', 'finalizing', 'final_validating', 'committed', 'quarantined', 'abandoned')),
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(session_id, creation_order)
		)`,
		`INSERT INTO batches SELECT * FROM batches_without_abandoned`,
		`DROP TABLE batches_without_abandoned`,
		`CREATE UNIQUE INDEX IF NOT EXISTS one_collecting_batch ON batches(session_id) WHERE status = 'collecting'`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return internal("migrate batch abandonment schema", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return internal("commit batch abandonment schema migration", err)
	}
	if _, err := db.Exec(`PRAGMA legacy_alter_table = OFF`); err != nil {
		return internal("restore legacy alter-table behavior", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return internal("restore foreign keys after batch abandonment migration", err)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return internal("verify batch abandonment schema migration", err)
	}
	defer rows.Close()
	if rows.Next() {
		return internal("verify batch abandonment schema migration", errors.New("foreign key violation after migration"))
	}
	return nil
}
