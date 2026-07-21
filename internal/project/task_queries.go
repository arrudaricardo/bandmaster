package project

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

func (p *Project) ListTasks() (string, TaskList, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return "", TaskList{}, projectError
	}
	defer db.Close()
	session, projectError := inspectLatestSession(db)
	if projectError != nil {
		return "", TaskList{}, projectError
	}
	rows, err := db.Query(`SELECT id FROM tasks WHERE session_id = ? ORDER BY creation_order`, session.ID)
	if err != nil {
		return session.ID, TaskList{}, sessionInternal(session.ID, "list tasks", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return session.ID, TaskList{}, sessionInternal(session.ID, "read task identity", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return session.ID, TaskList{}, sessionInternal(session.ID, "close task list", err)
	}
	if err := rows.Err(); err != nil {
		return session.ID, TaskList{}, sessionInternal(session.ID, "list tasks", err)
	}
	result := TaskList{Tasks: make([]Task, 0, len(ids))}
	for _, id := range ids {
		task, projectError := inspectTask(db, session.ID, id)
		if projectError != nil {
			return session.ID, TaskList{}, projectError
		}
		result.Tasks = append(result.Tasks, task)
	}
	return session.ID, result, nil
}

func (p *Project) InspectTask(id string) (Task, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return Task{}, projectError
	}
	defer db.Close()
	var sessionID string
	if err := db.QueryRow(`SELECT session_id FROM tasks WHERE id = ?`, id).Scan(&sessionID); errors.Is(err, sql.ErrNoRows) {
		return Task{}, invalid("task_not_found", fmt.Sprintf("Task %s does not exist.", id))
	} else if err != nil {
		return Task{}, internal("locate task", err)
	}
	return inspectTask(db, sessionID, id)
}

func inspectActiveSession(queryer rowQuerier) (Session, *Error) {
	session, projectError := inspectOpenSessionWithQueryer(queryer)
	if projectError != nil {
		return Session{}, projectError
	}
	if session.Status != "active" {
		return Session{}, invalidSession(session.ID, "session_not_active", fmt.Sprintf("Session %s must be active for task planning.", session.ID))
	}
	return session, nil
}

func inspectOpenSessionWithQueryer(queryer rowQuerier) (Session, *Error) {
	var session Session
	err := queryer.QueryRow(`SELECT id, status, starting_branch, starting_commit, created_at, updated_at FROM sessions WHERE status IN ('active', 'paused', 'finalizing', 'aborting')`).Scan(
		&session.ID, &session.Status, &session.StartingBranch, &session.StartingCommit, &session.CreatedAt, &session.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, invalid("session_not_active", "No open Bandmaster session exists in this repository.")
	}
	if err != nil {
		return Session{}, internal("read active session", err)
	}
	return session, nil
}

func inspectTask(db *sql.DB, sessionID, id string) (Task, *Error) {
	task := Task{SessionID: sessionID}
	var coreFrozen int
	var workerIdentity, assignmentToken sql.NullString
	err := db.QueryRow(`SELECT id, creation_order, title, intent, expected_outcome, status, worker_identity, assignment_token, core_frozen, created_at, updated_at FROM tasks WHERE session_id = ? AND id = ?`, sessionID, id).Scan(
		&task.ID, &task.CreationOrder, &task.Title, &task.Intent, &task.ExpectedOutcome, &task.Status, &workerIdentity, &assignmentToken, &coreFrozen, &task.CreatedAt, &task.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, invalidSession(sessionID, "task_not_found", fmt.Sprintf("Task %s does not exist in session %s.", id, sessionID))
	}
	if err != nil {
		return Task{}, sessionInternal(sessionID, "read task", err)
	}
	task.WorkerIdentity = workerIdentity.String
	task.AssignmentToken = assignmentToken.String
	task.CoreFrozen = coreFrozen != 0
	task.Claims = []Claim{}
	task.FocusedValidation = []FocusedValidation{}
	task.Prerequisites = []string{}
	rows, err := db.Query(`SELECT prerequisite_id FROM task_dependencies WHERE task_id = ? ORDER BY dependency_order`, id)
	if err != nil {
		return Task{}, sessionInternal(sessionID, "read task prerequisites", err)
	}
	for rows.Next() {
		var prerequisite string
		if err := rows.Scan(&prerequisite); err != nil {
			rows.Close()
			return Task{}, sessionInternal(sessionID, "read task prerequisite", err)
		}
		task.Prerequisites = append(task.Prerequisites, prerequisite)
	}
	if err := rows.Close(); err != nil {
		return Task{}, sessionInternal(sessionID, "close task prerequisites", err)
	}
	if err := rows.Err(); err != nil {
		return Task{}, sessionInternal(sessionID, "read task prerequisites", err)
	}
	task.AuditHistory = []TaskAuditEvent{}
	auditRows, err := db.Query(`
		SELECT audit.sequence, audit.event, audit.from_status, audit.to_status, audit.worker_identity, audit.termination_proof,
			COALESCE(recovery.recovery_method, repair.recovery_method),
			COALESCE(recovery.user_confirmation, repair.user_confirmation),
			COALESCE(recovery.replacement_assignment_token, repair.replacement_assignment_token),
			repair.diagnosis, repair.intended_repair, repair.invalidated_submission_json, audit.occurred_at
		FROM task_audit_events audit
		LEFT JOIN task_recovery_events recovery ON recovery.task_audit_sequence = audit.sequence
		LEFT JOIN task_repair_events repair ON repair.task_audit_sequence = audit.sequence
		WHERE audit.task_id = ? ORDER BY audit.sequence`, id)
	if err != nil {
		return Task{}, sessionInternal(sessionID, "read task audit history", err)
	}
	for auditRows.Next() {
		var event TaskAuditEvent
		var fromStatus, eventWorker, terminationProof, recoveryMethod, userConfirmation, replacementToken, diagnosis, intendedRepair, invalidatedSubmissionJSON sql.NullString
		if err := auditRows.Scan(&event.Sequence, &event.Event, &fromStatus, &event.ToStatus, &eventWorker, &terminationProof, &recoveryMethod, &userConfirmation, &replacementToken, &diagnosis, &intendedRepair, &invalidatedSubmissionJSON, &event.OccurredAt); err != nil {
			auditRows.Close()
			return Task{}, sessionInternal(sessionID, "read task audit event", err)
		}
		event.FromStatus = fromStatus.String
		event.WorkerIdentity = eventWorker.String
		event.TerminationProof = terminationProof.String
		event.RecoveryMethod = recoveryMethod.String
		event.UserConfirmation = userConfirmation.String
		event.ReplacementToken = replacementToken.String
		event.Diagnosis = diagnosis.String
		event.IntendedRepair = intendedRepair.String
		if invalidatedSubmissionJSON.Valid {
			var submission Submission
			if err := json.Unmarshal([]byte(invalidatedSubmissionJSON.String), &submission); err != nil {
				auditRows.Close()
				return Task{}, sessionInternal(sessionID, "decode invalidated task submission", err)
			}
			event.Invalidated = &submission
		}
		task.AuditHistory = append(task.AuditHistory, event)
	}
	if err := auditRows.Close(); err != nil {
		return Task{}, sessionInternal(sessionID, "close task audit history", err)
	}
	if err := auditRows.Err(); err != nil {
		return Task{}, sessionInternal(sessionID, "read task audit history", err)
	}
	for index := range task.AuditHistory {
		event := &task.AuditHistory[index]
		if event.Diagnosis == "" {
			continue
		}
		event.RepairSnapshots = []RepairSnapshot{}
		snapshotRows, err := db.Query(`SELECT path, presence, file_type, content_hash, executable, invalidated_presence, invalidated_type, invalidated_content_hash, invalidated_executable FROM task_repair_snapshots WHERE task_audit_sequence = ? ORDER BY path`, event.Sequence)
		if err != nil {
			return Task{}, sessionInternal(sessionID, "read task repair snapshots", err)
		}
		for snapshotRows.Next() {
			var snapshot RepairSnapshot
			var contentHash, invalidatedPresence, invalidatedType, invalidatedHash sql.NullString
			var executable int
			var invalidatedExecutable sql.NullInt64
			if err := snapshotRows.Scan(&snapshot.Path, &snapshot.Snapshot.Presence, &snapshot.Snapshot.Type, &contentHash, &executable, &invalidatedPresence, &invalidatedType, &invalidatedHash, &invalidatedExecutable); err != nil {
				snapshotRows.Close()
				return Task{}, sessionInternal(sessionID, "read task repair snapshot", err)
			}
			snapshot.Snapshot.ContentHash = contentHash.String
			snapshot.Snapshot.Executable = executable != 0
			if invalidatedPresence.Valid {
				snapshot.InvalidatedSubmitted = &PathSnapshot{Presence: invalidatedPresence.String, Type: invalidatedType.String, ContentHash: invalidatedHash.String, Executable: invalidatedExecutable.Int64 != 0}
			}
			event.RepairSnapshots = append(event.RepairSnapshots, snapshot)
		}
		if err := snapshotRows.Close(); err != nil {
			return Task{}, sessionInternal(sessionID, "close task repair snapshots", err)
		}
		if err := snapshotRows.Err(); err != nil {
			return Task{}, sessionInternal(sessionID, "read task repair snapshots", err)
		}
	}
	if err := db.QueryRow(`SELECT batch_id FROM batch_members WHERE task_id = ?`, id).Scan(&task.BatchID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Task{}, sessionInternal(sessionID, "read task batch", err)
	}
	if err := db.QueryRow(`SELECT commit_sha FROM task_commits WHERE task_id = ? ORDER BY committed_at DESC LIMIT 1`, id).Scan(&task.CommitSHA); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Task{}, sessionInternal(sessionID, "read task commit", err)
	}
	var lease WorkerLease
	if err := db.QueryRow(`SELECT status, renewed_at, expires_at FROM task_leases WHERE task_id = ?`, id).Scan(&lease.Status, &lease.RenewedAt, &lease.ExpiresAt); err == nil {
		task.Lease = &lease
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Task{}, sessionInternal(sessionID, "read task worker lease", err)
	}
	claimRows, err := db.Query(`
		SELECT claim.path, claim.baseline_presence, claim.baseline_type, claim.baseline_content_hash, claim.baseline_executable,
			submitted.presence, submitted.file_type, submitted.content_hash, submitted.executable
		FROM claims claim
		LEFT JOIN submitted_snapshots submitted ON submitted.task_id = claim.task_id AND submitted.path = claim.path
		WHERE claim.task_id = ?
		ORDER BY claim.claim_order`, id)
	if err != nil {
		return Task{}, sessionInternal(sessionID, "read task claims", err)
	}
	for claimRows.Next() {
		var claim Claim
		var contentHash, submittedPresence, submittedType, submittedHash sql.NullString
		var executable int
		var submittedExecutable sql.NullInt64
		if err := claimRows.Scan(&claim.Path, &claim.Baseline.Presence, &claim.Baseline.Type, &contentHash, &executable, &submittedPresence, &submittedType, &submittedHash, &submittedExecutable); err != nil {
			claimRows.Close()
			return Task{}, sessionInternal(sessionID, "read task claim", err)
		}
		claim.Baseline.ContentHash = contentHash.String
		claim.Baseline.Executable = executable != 0
		if submittedPresence.Valid {
			claim.SubmittedSnapshot = &PathSnapshot{Presence: submittedPresence.String, Type: submittedType.String, ContentHash: submittedHash.String, Executable: submittedExecutable.Int64 != 0}
		}
		task.Claims = append(task.Claims, claim)
	}
	if err := claimRows.Close(); err != nil {
		return Task{}, sessionInternal(sessionID, "close task claims", err)
	}
	if err := claimRows.Err(); err != nil {
		return Task{}, sessionInternal(sessionID, "read task claims", err)
	}
	validationRows, err := db.Query(`SELECT name, argv_json, script, working_directory, timeout, environment_json FROM focused_validations WHERE task_id = ? ORDER BY validation_order`, id)
	if err != nil {
		return Task{}, sessionInternal(sessionID, "read focused validation", err)
	}
	defer validationRows.Close()
	for validationRows.Next() {
		var validation FocusedValidation
		var argvJSON, script sql.NullString
		var environmentJSON string
		if err := validationRows.Scan(&validation.Name, &argvJSON, &script, &validation.WorkingDirectory, &validation.Timeout, &environmentJSON); err != nil {
			return Task{}, sessionInternal(sessionID, "read focused validation command", err)
		}
		validation.Script = script.String
		if argvJSON.Valid && json.Unmarshal([]byte(argvJSON.String), &validation.Argv) != nil {
			return Task{}, sessionInternal(sessionID, "decode focused validation arguments", errors.New("invalid stored argument JSON"))
		}
		if err := json.Unmarshal([]byte(environmentJSON), &validation.Environment); err != nil {
			return Task{}, sessionInternal(sessionID, "decode focused validation environment", err)
		}
		task.FocusedValidation = append(task.FocusedValidation, validation)
	}
	if err := validationRows.Err(); err != nil {
		return Task{}, sessionInternal(sessionID, "read focused validation", err)
	}
	var submission Submission
	var noChanges int
	if err := db.QueryRow(`SELECT outcome, no_changes, behavior_changed, key_decisions, validation_expectations, known_risks, submitted_at FROM task_submissions WHERE task_id = ?`, id).Scan(&submission.Outcome, &noChanges, &submission.BehaviorChanged, &submission.KeyDecisions, &submission.ValidationExpectations, &submission.KnownRisks, &submission.SubmittedAt); err == nil {
		submission.NoChanges = noChanges != 0
		task.Submission = &submission
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Task{}, sessionInternal(sessionID, "read task submission", err)
	}
	return task, nil
}
