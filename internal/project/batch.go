package project

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Batch struct {
	SessionID     string                   `json:"-"`
	ID            string                   `json:"id"`
	CreationOrder int64                    `json:"creation_order"`
	BaseBranch    string                   `json:"base_branch"`
	BaseCommit    string                   `json:"base_commit"`
	Status        string                   `json:"status"`
	FrozenAt      string                   `json:"frozen_at,omitempty"`
	Tasks         []BatchTask              `json:"tasks"`
	Manifest      []BatchPath              `json:"manifest"`
	Validation    []BatchValidationAttempt `json:"validation"`
	AuditHistory  []BatchAuditEvent        `json:"audit_history"`
	CreatedAt     string                   `json:"created_at"`
	UpdatedAt     string                   `json:"updated_at"`
}

type BatchTask struct {
	TaskID        string `json:"task_id"`
	TaskOrder     int64  `json:"task_order"`
	CreationOrder int64  `json:"creation_order"`
	Status        string `json:"status"`
	Outcome       string `json:"submission_outcome,omitempty"`
}

type BatchPath struct {
	TaskID     string       `json:"task_id"`
	TaskOrder  int64        `json:"task_order"`
	ClaimOrder int64        `json:"claim_order"`
	Path       string       `json:"path"`
	Baseline   PathSnapshot `json:"baseline"`
	Submitted  PathSnapshot `json:"submitted"`
}

type BatchAuditEvent struct {
	Sequence   int64  `json:"sequence"`
	Event      string `json:"event"`
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status"`
	OccurredAt string `json:"occurred_at"`
}

func (p *Project) InspectBatch(id string) (Batch, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return Batch{}, projectError
	}
	defer db.Close()
	return inspectBatch(db, id)
}

func (p *Project) FreezeBatch() (Batch, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return Batch{}, projectError
	}
	defer db.Close()

	openSession, projectError := inspectOpenSessionWithQueryer(db)
	if projectError != nil {
		return Batch{}, projectError
	}
	if openSession.Status == "finalizing" {
		return p.inspectFinalizingBatch(db, openSession)
	}
	if openSession.Status != "active" {
		return Batch{}, invalidSession(openSession.ID, "session_not_active", fmt.Sprintf("Session %s must be active to freeze a batch.", openSession.ID))
	}

	tx, err := db.Begin()
	if err != nil {
		return Batch{}, sessionInternal(openSession.ID, "begin batch freeze", err)
	}
	defer tx.Rollback()
	session, projectError := inspectOpenSessionWithQueryer(tx)
	if projectError != nil {
		return Batch{}, projectError
	}
	if session.Status == "finalizing" {
		if err := tx.Rollback(); err != nil {
			return Batch{}, sessionInternal(session.ID, "close concurrent batch freeze", err)
		}
		return p.inspectFinalizingBatch(db, session)
	}
	if session.Status != "active" {
		return Batch{}, invalidSession(session.ID, "session_not_active", fmt.Sprintf("Session %s must be active to freeze a batch.", session.ID))
	}
	if projectError := sweepExpiredLeases(tx, session.ID, time.Now().UTC()); projectError != nil {
		return Batch{}, projectError
	}

	var quarantinedTask string
	err = tx.QueryRow(`SELECT id FROM tasks WHERE session_id = ? AND status = 'quarantined' ORDER BY creation_order LIMIT 1`, session.ID).Scan(&quarantinedTask)
	if err == nil {
		return Batch{}, commitBatchCheck(tx, session.ID, quarantined(session.ID, "task_quarantined", fmt.Sprintf("Task %s is quarantined; recover it before freezing a batch.", quarantinedTask)))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Batch{}, sessionInternal(session.ID, "inspect task quarantine at barrier", err)
	}
	var unresolvedIntegrity int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM integrity_violations WHERE session_id = ? AND recovered_at IS NULL`, session.ID).Scan(&unresolvedIntegrity); err != nil {
		return Batch{}, sessionInternal(session.ID, "inspect barrier integrity state", err)
	}
	if unresolvedIntegrity != 0 {
		return Batch{}, commitBatchCheck(tx, session.ID, quarantined(session.ID, "integrity_recovery_required", "Unresolved repository integrity violations prevent freezing the batch."))
	}

	var batchID, baseBranch, baseCommit, batchStatus string
	err = tx.QueryRow(`SELECT id, base_branch, base_commit, status FROM batches WHERE session_id = ? AND status IN ('collecting', 'repairing') ORDER BY CASE status WHEN 'repairing' THEN 0 ELSE 1 END, creation_order LIMIT 1`, session.ID).Scan(&batchID, &baseBranch, &baseCommit, &batchStatus)
	if errors.Is(err, sql.ErrNoRows) {
		var repairPendingID string
		if repairErr := tx.QueryRow(`SELECT id FROM batches WHERE session_id = ? AND status = 'repair_pending' ORDER BY creation_order LIMIT 1`, session.ID).Scan(&repairPendingID); repairErr == nil {
			return Batch{}, commitBatchCheck(tx, session.ID, blocked(session.ID, "batch_repair_required", fmt.Sprintf("Batch %s requires at least one original owner to be reopened for repair.", repairPendingID)))
		} else if !errors.Is(repairErr, sql.ErrNoRows) {
			return Batch{}, sessionInternal(session.ID, "inspect repair-pending batch", repairErr)
		}
		return Batch{}, commitBatchCheck(tx, session.ID, blocked(session.ID, "batch_not_collecting", "No collecting or repairing batch is ready to freeze."))
	}
	if err != nil {
		return Batch{}, sessionInternal(session.ID, "inspect barrier batch", err)
	}
	if baseBranch != session.StartingBranch || baseCommit != session.StartingCommit {
		return Batch{}, commitBatchCheck(tx, session.ID, invalidSession(session.ID, "batch_base_drift", "The batch base no longer matches the active session."))
	}

	var activeTask string
	err = tx.QueryRow(`SELECT id FROM tasks WHERE session_id = ? AND status IN ('assigned', 'editing') ORDER BY creation_order LIMIT 1`, session.ID).Scan(&activeTask)
	if err == nil {
		return Batch{}, commitBatchCheck(tx, session.ID, blocked(session.ID, "active_agents", fmt.Sprintf("Task %s still has an active agent; every agent must stop before the barrier.", activeTask)))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Batch{}, sessionInternal(session.ID, "inspect active agents at barrier", err)
	}

	var taskCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM batch_tasks WHERE batch_id = ?`, batchID).Scan(&taskCount); err != nil {
		return Batch{}, sessionInternal(session.ID, "count batch tasks", err)
	}
	if taskCount == 0 {
		return Batch{}, commitBatchCheck(tx, session.ID, blocked(session.ID, "batch_empty", "A batch must have at least one claimed task before it can freeze."))
	}
	var unsubmittedTask string
	err = tx.QueryRow(`
		SELECT task.id
		FROM batch_tasks batch_task JOIN tasks task ON task.id = batch_task.task_id
		WHERE batch_task.batch_id = ? AND task.status != 'submitted'
		ORDER BY batch_task.task_order LIMIT 1`, batchID).Scan(&unsubmittedTask)
	if err == nil {
		return Batch{}, commitBatchCheck(tx, session.ID, blocked(session.ID, "batch_task_not_submitted", fmt.Sprintf("Batch task %s has not submitted its work.", unsubmittedTask)))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Batch{}, sessionInternal(session.ID, "inspect batch submissions", err)
	}

	var missingSnapshotTask, missingSnapshotPath string
	err = tx.QueryRow(`
		SELECT claim.task_id, claim.path
		FROM claims claim
		LEFT JOIN submitted_snapshots snapshot ON snapshot.task_id = claim.task_id AND snapshot.path = claim.path
		WHERE claim.batch_id = ? AND snapshot.task_id IS NULL
		ORDER BY claim.task_id, claim.claim_order LIMIT 1`, batchID).Scan(&missingSnapshotTask, &missingSnapshotPath)
	if err == nil {
		return Batch{}, commitBatchCheck(tx, session.ID, blocked(session.ID, "submitted_snapshot_missing", fmt.Sprintf("Batch task %s has no submitted snapshot for %s.", missingSnapshotTask, missingSnapshotPath)))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Batch{}, sessionInternal(session.ID, "inspect submitted snapshot coverage", err)
	}

	var dependent, prerequisite string
	err = tx.QueryRow(`
		SELECT dependency.task_id, dependency.prerequisite_id
		FROM task_dependencies dependency
		JOIN batch_tasks dependent_task ON dependent_task.task_id = dependency.task_id
		JOIN batch_tasks prerequisite_task ON prerequisite_task.task_id = dependency.prerequisite_id
		WHERE dependent_task.batch_id = ? AND prerequisite_task.batch_id = ?
		LIMIT 1`, batchID, batchID).Scan(&dependent, &prerequisite)
	if err == nil {
		return Batch{}, commitBatchCheck(tx, session.ID, invalidSession(session.ID, "same_batch_dependency", fmt.Sprintf("Task %s cannot share a batch with prerequisite %s.", dependent, prerequisite)))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Batch{}, sessionInternal(session.ID, "validate batch dependency order", err)
	}

	observations, projectError := p.scanRepository(tx, session)
	if projectError != nil {
		return Batch{}, projectError
	}
	if len(observations) != 0 {
		if err := tx.Rollback(); err != nil {
			return Batch{}, sessionInternal(session.ID, "close failed barrier scan", err)
		}
		if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
			return Batch{}, projectError
		}
		return Batch{}, integrityError(session.ID, observations[0])
	}

	changedPaths, projectError := p.changedPaths()
	if projectError != nil {
		projectError.SessionID = session.ID
		return Batch{}, projectError
	}
	for _, changedPath := range changedPaths {
		var submittedOwners int
		if err := tx.QueryRow(`
			SELECT COUNT(*)
			FROM claims claim
			JOIN batch_tasks batch_task ON batch_task.batch_id = claim.batch_id AND batch_task.task_id = claim.task_id
			JOIN tasks task ON task.id = claim.task_id
			JOIN submitted_snapshots snapshot ON snapshot.task_id = claim.task_id AND snapshot.path = claim.path
			WHERE claim.session_id = ? AND claim.batch_id = ? AND claim.path = ? AND task.status = 'submitted'`, session.ID, batchID, changedPath).Scan(&submittedOwners); err != nil {
			return Batch{}, sessionInternal(session.ID, "verify changed path attribution", err)
		}
		if submittedOwners != 1 {
			return Batch{}, commitBatchCheck(tx, session.ID, blocked(session.ID, "changed_path_not_submitted", fmt.Sprintf("Changed path %s must have exactly one submitted owner in the batch.", changedPath)))
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`
		INSERT INTO frozen_batch_paths(
			batch_id, task_id, task_order, claim_order, path,
			baseline_presence, baseline_type, baseline_content_hash, baseline_executable, baseline_content,
			submitted_presence, submitted_type, submitted_content_hash, submitted_executable, submitted_content
		)
		SELECT claim.batch_id, claim.task_id, batch_task.task_order, claim.claim_order, claim.path,
			claim.baseline_presence, claim.baseline_type, claim.baseline_content_hash, claim.baseline_executable, claim.baseline_content,
			snapshot.presence, snapshot.file_type, snapshot.content_hash, snapshot.executable, snapshot.content
		FROM claims claim
		JOIN batch_tasks batch_task ON batch_task.batch_id = claim.batch_id AND batch_task.task_id = claim.task_id
		JOIN submitted_snapshots snapshot ON snapshot.task_id = claim.task_id AND snapshot.path = claim.path
		WHERE claim.batch_id = ?
		ORDER BY batch_task.task_order, claim.claim_order`, batchID); err != nil {
		return Batch{}, sessionInternal(session.ID, "persist frozen batch manifest", err)
	}
	if _, err := tx.Exec(`INSERT INTO batch_freezes(batch_id, frozen_at) VALUES(?, ?)`, batchID, now); err != nil {
		return Batch{}, sessionInternal(session.ID, "record batch freeze", err)
	}

	observations, projectError = p.scanRepository(tx, session)
	if projectError != nil {
		return Batch{}, projectError
	}
	if len(observations) != 0 {
		if err := tx.Rollback(); err != nil {
			return Batch{}, sessionInternal(session.ID, "close final barrier scan", err)
		}
		if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
			return Batch{}, projectError
		}
		return Batch{}, integrityError(session.ID, observations[0])
	}
	if _, err := tx.Exec(`UPDATE batches SET status = 'frozen', updated_at = ? WHERE id = ? AND status = ?`, now, batchID, batchStatus); err != nil {
		return Batch{}, sessionInternal(session.ID, "freeze batch", err)
	}
	if _, err := tx.Exec(`UPDATE sessions SET status = 'finalizing', updated_at = ? WHERE id = ? AND status = 'active'`, now, session.ID); err != nil {
		return Batch{}, sessionInternal(session.ID, "enter batch finalization", err)
	}
	if _, err := tx.Exec(`INSERT INTO batch_audit_events(session_id, batch_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'batch_frozen', ?, 'frozen', ?)`, session.ID, batchID, batchStatus, now); err != nil {
		return Batch{}, sessionInternal(session.ID, "audit batch freeze", err)
	}
	if _, err := tx.Exec(`INSERT INTO audit_events(session_id, event, from_status, to_status, occurred_at) VALUES(?, 'batch_frozen', 'active', 'finalizing', ?)`, session.ID, now); err != nil {
		return Batch{}, sessionInternal(session.ID, "audit batch finalization", err)
	}
	if err := tx.Commit(); err != nil {
		return Batch{}, sessionInternal(session.ID, "commit batch freeze", err)
	}
	return p.inspectFinalizingBatch(db, Session{
		ID:             session.ID,
		Status:         "finalizing",
		StartingBranch: session.StartingBranch,
		StartingCommit: session.StartingCommit,
	})
}

func (p *Project) inspectFinalizingBatch(db *sql.DB, session Session) (Batch, *Error) {
	var batchID string
	err := db.QueryRow(`SELECT id FROM batches WHERE session_id = ? AND status = 'frozen' ORDER BY creation_order DESC LIMIT 1`, session.ID).Scan(&batchID)
	if errors.Is(err, sql.ErrNoRows) {
		return Batch{}, invalidSession(session.ID, "session_finalizing", "The session is finalizing without a frozen batch available for this request.")
	}
	if err != nil {
		return Batch{}, sessionInternal(session.ID, "inspect finalizing batch", err)
	}
	if projectError := p.StopIntegrityMonitor(session.ID); projectError != nil {
		observation := integrityObservation{Kind: "monitor_unhealthy", Path: ".git/bandmaster/monitor", ObservedState: map[string]any{"error": projectError.Message}}
		if persistError := p.persistIntegrityViolations(session, []integrityObservation{observation}); persistError != nil {
			return Batch{}, persistError
		}
		return Batch{}, integrityError(session.ID, observation)
	}
	observations, projectError := p.scanRepository(db, session)
	if projectError != nil {
		return Batch{}, projectError
	}
	if len(observations) != 0 {
		if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
			return Batch{}, projectError
		}
		return Batch{}, integrityError(session.ID, observations[0])
	}
	var sessionStatus, batchStatus string
	if err := db.QueryRow(`SELECT session.status, batch.status FROM sessions session JOIN batches batch ON batch.session_id = session.id WHERE session.id = ? AND batch.id = ?`, session.ID, batchID).Scan(&sessionStatus, &batchStatus); err != nil {
		return Batch{}, sessionInternal(session.ID, "verify frozen batch state", err)
	}
	if sessionStatus != "finalizing" || batchStatus != "frozen" {
		var kind, violationPath string
		err := db.QueryRow(`SELECT kind, path FROM integrity_violations WHERE session_id = ? AND recovered_at IS NULL ORDER BY id LIMIT 1`, session.ID).Scan(&kind, &violationPath)
		if err == nil {
			return Batch{}, integrityError(session.ID, integrityObservation{Kind: kind, Path: violationPath})
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Batch{}, sessionInternal(session.ID, "inspect frozen batch integrity state", err)
		}
		return Batch{}, quarantined(session.ID, "batch_state_changed", fmt.Sprintf("Batch %s changed to %s while its barrier was being verified.", batchID, batchStatus))
	}
	return inspectBatch(db, batchID)
}

func commitBatchCheck(tx *sql.Tx, sessionID string, projectError *Error) *Error {
	if err := tx.Commit(); err != nil {
		return sessionInternal(sessionID, "commit batch barrier check", err)
	}
	return projectError
}

func validateTaskBatchStatus(queryer rowQuerier, sessionID, taskID string, allowed ...string) *Error {
	var batchID, status string
	err := queryer.QueryRow(`
		SELECT batch.id, batch.status
		FROM batch_tasks task JOIN batches batch ON batch.id = task.batch_id
		WHERE task.task_id = ?`, taskID).Scan(&batchID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionInternal(sessionID, "validate task batch state", errors.New("material task is not included in a Batch"))
	}
	if err != nil {
		return sessionInternal(sessionID, "validate task batch state", err)
	}
	for _, candidate := range allowed {
		if status == candidate {
			return nil
		}
	}
	return invalidSession(sessionID, "batch_closed", fmt.Sprintf("Batch %s is %s and no longer accepts agent mutations.", batchID, status))
}

func inspectBatch(db *sql.DB, id string) (Batch, *Error) {
	query := `
		SELECT batch.session_id, batch.id, batch.creation_order, batch.base_branch, batch.base_commit,
			batch.status, freeze.frozen_at, batch.created_at, batch.updated_at
		FROM batches batch LEFT JOIN batch_freezes freeze ON freeze.batch_id = batch.id`
	var row *sql.Row
	if id == "" {
		row = db.QueryRow(query + ` ORDER BY batch.rowid DESC LIMIT 1`)
	} else {
		row = db.QueryRow(query+` WHERE batch.id = ?`, id)
	}
	var batch Batch
	var frozenAt sql.NullString
	if err := row.Scan(&batch.SessionID, &batch.ID, &batch.CreationOrder, &batch.BaseBranch, &batch.BaseCommit, &batch.Status, &frozenAt, &batch.CreatedAt, &batch.UpdatedAt); errors.Is(err, sql.ErrNoRows) {
		return Batch{}, invalid("batch_not_found", "No matching Bandmaster batch exists in this repository.")
	} else if err != nil {
		return Batch{}, internal("read batch", err)
	}
	batch.FrozenAt = frozenAt.String
	batch.Tasks = []BatchTask{}
	batch.Manifest = []BatchPath{}
	batch.Validation = []BatchValidationAttempt{}
	batch.AuditHistory = []BatchAuditEvent{}

	rows, err := db.Query(`
		SELECT batch_task.task_id, batch_task.task_order, task.creation_order, task.status, submission.outcome
		FROM batch_tasks batch_task
		JOIN tasks task ON task.id = batch_task.task_id
		LEFT JOIN task_submissions submission ON submission.task_id = batch_task.task_id
		WHERE batch_task.batch_id = ? ORDER BY batch_task.task_order`, batch.ID)
	if err != nil {
		return Batch{}, sessionInternal(batch.SessionID, "read batch tasks", err)
	}
	for rows.Next() {
		var task BatchTask
		var outcome sql.NullString
		if err := rows.Scan(&task.TaskID, &task.TaskOrder, &task.CreationOrder, &task.Status, &outcome); err != nil {
			rows.Close()
			return Batch{}, sessionInternal(batch.SessionID, "read batch task", err)
		}
		task.Outcome = outcome.String
		batch.Tasks = append(batch.Tasks, task)
	}
	if err := rows.Close(); err != nil {
		return Batch{}, sessionInternal(batch.SessionID, "close batch tasks", err)
	}
	if err := rows.Err(); err != nil {
		return Batch{}, sessionInternal(batch.SessionID, "read batch tasks", err)
	}

	pathRows, err := db.Query(`
		SELECT task_id, task_order, claim_order, path,
			baseline_presence, baseline_type, baseline_content_hash, baseline_executable,
			submitted_presence, submitted_type, submitted_content_hash, submitted_executable
		FROM frozen_batch_paths WHERE batch_id = ? ORDER BY task_order, claim_order`, batch.ID)
	if err != nil {
		return Batch{}, sessionInternal(batch.SessionID, "read frozen batch manifest", err)
	}
	for pathRows.Next() {
		var batchPath BatchPath
		var baselineHash, submittedHash sql.NullString
		var baselineExecutable, submittedExecutable int
		if err := pathRows.Scan(&batchPath.TaskID, &batchPath.TaskOrder, &batchPath.ClaimOrder, &batchPath.Path,
			&batchPath.Baseline.Presence, &batchPath.Baseline.Type, &baselineHash, &baselineExecutable,
			&batchPath.Submitted.Presence, &batchPath.Submitted.Type, &submittedHash, &submittedExecutable); err != nil {
			pathRows.Close()
			return Batch{}, sessionInternal(batch.SessionID, "read frozen batch path", err)
		}
		batchPath.Baseline.ContentHash = baselineHash.String
		batchPath.Baseline.Executable = baselineExecutable != 0
		batchPath.Submitted.ContentHash = submittedHash.String
		batchPath.Submitted.Executable = submittedExecutable != 0
		batch.Manifest = append(batch.Manifest, batchPath)
	}
	if err := pathRows.Close(); err != nil {
		return Batch{}, sessionInternal(batch.SessionID, "close frozen batch manifest", err)
	}
	if err := pathRows.Err(); err != nil {
		return Batch{}, sessionInternal(batch.SessionID, "read frozen batch manifest", err)
	}

	validationRows, err := db.Query(`SELECT attempt, status, started_at, finished_at FROM batch_validation_attempts WHERE batch_id = ? ORDER BY attempt`, batch.ID)
	if err != nil {
		return Batch{}, sessionInternal(batch.SessionID, "read batch validation attempts", err)
	}
	for validationRows.Next() {
		var attempt BatchValidationAttempt
		var finishedAt sql.NullString
		if err := validationRows.Scan(&attempt.Attempt, &attempt.Status, &attempt.StartedAt, &finishedAt); err != nil {
			validationRows.Close()
			return Batch{}, sessionInternal(batch.SessionID, "read batch validation attempt", err)
		}
		attempt.FinishedAt = finishedAt.String
		attempt.Commands = []BatchValidationRun{}
		batch.Validation = append(batch.Validation, attempt)
	}
	if err := validationRows.Close(); err != nil {
		return Batch{}, sessionInternal(batch.SessionID, "close batch validation attempts", err)
	}
	if err := validationRows.Err(); err != nil {
		return Batch{}, sessionInternal(batch.SessionID, "read batch validation attempts", err)
	}
	for index := range batch.Validation {
		attempt := &batch.Validation[index]
		commandRows, err := db.Query(`
			SELECT attempt, command_order, source, task_id, name, argv_json, script, resolved_argv_json,
				working_directory, resolved_working_directory, timeout, environment_overrides_json,
				resolved_environment_json, status, exit_code, duration_nanos, stdout, stderr,
				stdout_truncated, stderr_truncated, started_at, finished_at
			FROM batch_validation_runs WHERE batch_id = ? AND attempt = ? ORDER BY command_order`, batch.ID, attempt.Attempt)
		if err != nil {
			return Batch{}, sessionInternal(batch.SessionID, "read validation commands", err)
		}
		for commandRows.Next() {
			var run BatchValidationRun
			var taskID, argvJSON, script sql.NullString
			var resolvedArgvJSON, overridesJSON, environmentJSON string
			var exitCode sql.NullInt64
			var durationNanos int64
			var stdoutTruncated, stderrTruncated int
			if err := commandRows.Scan(&run.Attempt, &run.CommandOrder, &run.Source, &taskID, &run.Name, &argvJSON, &script, &resolvedArgvJSON,
				&run.WorkingDirectory, &run.ResolvedWorkingDirectory, &run.Timeout, &overridesJSON, &environmentJSON,
				&run.Status, &exitCode, &durationNanos, &run.Stdout, &run.Stderr, &stdoutTruncated, &stderrTruncated,
				&run.StartedAt, &run.FinishedAt); err != nil {
				commandRows.Close()
				return Batch{}, sessionInternal(batch.SessionID, "read validation command", err)
			}
			run.TaskID = taskID.String
			run.Script = script.String
			if argvJSON.Valid && json.Unmarshal([]byte(argvJSON.String), &run.Argv) != nil {
				commandRows.Close()
				return Batch{}, sessionInternal(batch.SessionID, "decode validation arguments", errors.New("invalid stored argument JSON"))
			}
			if err := json.Unmarshal([]byte(resolvedArgvJSON), &run.ResolvedArgv); err != nil {
				commandRows.Close()
				return Batch{}, sessionInternal(batch.SessionID, "decode resolved validation arguments", err)
			}
			if err := json.Unmarshal([]byte(overridesJSON), &run.EnvironmentOverrides); err != nil {
				commandRows.Close()
				return Batch{}, sessionInternal(batch.SessionID, "decode validation environment overrides", err)
			}
			if err := json.Unmarshal([]byte(environmentJSON), &run.ResolvedEnvironment); err != nil {
				commandRows.Close()
				return Batch{}, sessionInternal(batch.SessionID, "decode resolved validation environment", err)
			}
			if exitCode.Valid {
				value := int(exitCode.Int64)
				run.ExitCode = &value
			}
			run.DurationMilliseconds = time.Duration(durationNanos).Milliseconds()
			run.StdoutTruncated = stdoutTruncated != 0
			run.StderrTruncated = stderrTruncated != 0
			attempt.Commands = append(attempt.Commands, run)
		}
		if err := commandRows.Close(); err != nil {
			return Batch{}, sessionInternal(batch.SessionID, "close validation commands", err)
		}
		if err := commandRows.Err(); err != nil {
			return Batch{}, sessionInternal(batch.SessionID, "read validation commands", err)
		}
	}

	auditRows, err := db.Query(`SELECT sequence, event, from_status, to_status, occurred_at FROM batch_audit_events WHERE batch_id = ? ORDER BY sequence`, batch.ID)
	if err != nil {
		return Batch{}, sessionInternal(batch.SessionID, "read batch audit history", err)
	}
	defer auditRows.Close()
	for auditRows.Next() {
		var event BatchAuditEvent
		var fromStatus sql.NullString
		if err := auditRows.Scan(&event.Sequence, &event.Event, &fromStatus, &event.ToStatus, &event.OccurredAt); err != nil {
			return Batch{}, sessionInternal(batch.SessionID, "read batch audit event", err)
		}
		event.FromStatus = fromStatus.String
		batch.AuditHistory = append(batch.AuditHistory, event)
	}
	if err := auditRows.Err(); err != nil {
		return Batch{}, sessionInternal(batch.SessionID, "read batch audit history", err)
	}
	return batch, nil
}
