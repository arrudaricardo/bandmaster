package project

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type RepairRequest struct {
	TerminatedWorker string
	TerminationProof string
	UserConfirmation string
	Diagnosis        string
	IntendedRepair   string
}

func (p *Project) RepairTask(id string, request RepairRequest) (Task, *Error) {
	if strings.TrimSpace(request.Diagnosis) == "" || strings.TrimSpace(request.IntendedRepair) == "" {
		return Task{}, invalid("invalid_repair_handoff", "Repair diagnosis and intended repair must not be empty.")
	}
	if strings.TrimSpace(request.UserConfirmation) != "" && (strings.TrimSpace(request.TerminatedWorker) != "" || strings.TrimSpace(request.TerminationProof) != "") {
		return Task{}, invalid("conflicting_recovery_evidence", "Use either worker-handle termination evidence or explicit user confirmation, not both.")
	}

	db, projectError := p.openState()
	if projectError != nil {
		return Task{}, projectError
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return Task{}, internal("begin task repair", err)
	}
	defer tx.Rollback()
	session, projectError := inspectActiveSession(tx)
	if projectError != nil {
		return Task{}, projectError
	}

	var status, workerIdentity, batchID, batchStatus string
	err = tx.QueryRow(`
		SELECT task.status, COALESCE(task.worker_identity, ''), batch.id, batch.status
		FROM tasks task
		JOIN batch_members member ON member.task_id = task.id
		JOIN batches batch ON batch.id = member.batch_id
		WHERE task.session_id = ? AND task.id = ?`, session.ID, id).Scan(&status, &workerIdentity, &batchID, &batchStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, invalidSession(session.ID, "task_not_found", fmt.Sprintf("Task %s does not exist or has no retained ownership in the active session.", id))
	}
	if err != nil {
		return Task{}, sessionInternal(session.ID, "read task repair state", err)
	}
	if status != "editing" && status != "submitted" {
		return Task{}, invalidSession(session.ID, "task_not_repairable", fmt.Sprintf("Task %s cannot be repaired from %s state.", id, status))
	}
	if status == "editing" && batchStatus != "collecting" && batchStatus != "repairing" {
		return Task{}, invalidSession(session.ID, "batch_not_repairable", fmt.Sprintf("Task %s cannot report worker failure while batch %s is %s.", id, batchID, batchStatus))
	}
	if status == "submitted" && batchStatus != "repair_pending" && batchStatus != "repairing" {
		return Task{}, invalidSession(session.ID, "batch_not_repairable", fmt.Sprintf("Submitted task %s can be reopened only after its batch requires repair.", id))
	}

	recoveryMethod := "worker_handle"
	if strings.TrimSpace(request.UserConfirmation) != "" {
		recoveryMethod = "user_confirmation"
	} else if strings.TrimSpace(request.TerminatedWorker) == "" {
		return Task{}, invalidSession(session.ID, "worker_termination_required", fmt.Sprintf("Repairing task %s requires the terminated worker identity %s or explicit user confirmation.", id, workerIdentity))
	} else if request.TerminatedWorker != workerIdentity {
		return Task{}, invalidSession(session.ID, "worker_termination_mismatch", fmt.Sprintf("Task %s is assigned to worker %s, not %s.", id, workerIdentity, request.TerminatedWorker))
	} else if strings.TrimSpace(request.TerminationProof) == "" {
		return Task{}, invalidSession(session.ID, "worker_termination_proof_required", fmt.Sprintf("Repairing task %s requires evidence from the parent-held worker handle that %s stopped.", id, workerIdentity))
	}

	claims, projectError := loadStoredClaims(tx, session.ID, id)
	if projectError != nil {
		return Task{}, projectError
	}
	currentSnapshots, projectError := p.captureRepairSnapshots(session.ID, claims)
	if projectError != nil {
		return Task{}, projectError
	}
	invalidatedSubmission, invalidatedSnapshots, projectError := loadInvalidatedSubmission(tx, session.ID, id)
	if projectError != nil {
		return Task{}, projectError
	}
	if observations, scanError := p.scanRepository(tx, session); scanError != nil {
		return Task{}, scanError
	} else if len(observations) != 0 {
		if err := tx.Rollback(); err != nil {
			return Task{}, sessionInternal(session.ID, "close unsafe task repair", err)
		}
		if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
			return Task{}, projectError
		}
		return Task{}, integrityError(session.ID, observations[0])
	}
	verifiedSnapshots, projectError := p.captureRepairSnapshots(session.ID, claims)
	if projectError != nil {
		return Task{}, projectError
	}
	for index := range currentSnapshots {
		if !snapshotsEqual(currentSnapshots[index].PathSnapshot, verifiedSnapshots[index].PathSnapshot) {
			if status == "submitted" {
				observation, found, observationError := p.submittedPathObservation(tx, session.ID, id, batchID, currentSnapshots[index].Path)
				if observationError != nil {
					return Task{}, observationError
				}
				if found {
					if err := tx.Rollback(); err != nil {
						return Task{}, sessionInternal(session.ID, "close changed task repair snapshot", err)
					}
					if projectError := p.persistIntegrityViolations(session, []integrityObservation{observation}); projectError != nil {
						return Task{}, projectError
					}
					return Task{}, integrityError(session.ID, observation)
				}
			}
			return Task{}, invalidSession(session.ID, "repair_snapshot_changed", fmt.Sprintf("Claimed path %s changed while its repair snapshot was being recorded.", currentSnapshots[index].Path))
		}
	}
	currentSnapshots = verifiedSnapshots

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if batchStatus == "repair_pending" {
		if _, err := tx.Exec(`DELETE FROM frozen_batch_paths WHERE batch_id = ?`, batchID); err != nil {
			return Task{}, sessionInternal(session.ID, "invalidate stale frozen manifest", err)
		}
		if _, err := tx.Exec(`DELETE FROM batch_freezes WHERE batch_id = ?`, batchID); err != nil {
			return Task{}, sessionInternal(session.ID, "invalidate stale batch freeze", err)
		}
		if _, err := tx.Exec(`UPDATE batches SET status = 'repairing', updated_at = ? WHERE id = ? AND status = 'repair_pending'`, now, batchID); err != nil {
			return Task{}, sessionInternal(session.ID, "start frozen batch repair", err)
		}
		if _, err := tx.Exec(`INSERT INTO batch_audit_events(session_id, batch_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'batch_repair_started', 'repair_pending', 'repairing', ?)`, session.ID, batchID, now); err != nil {
			return Task{}, sessionInternal(session.ID, "audit frozen batch repair", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM task_diff_reviews WHERE task_id = ?`, id); err != nil {
		return Task{}, sessionInternal(session.ID, "invalidate stale repair diff review", err)
	}
	if _, err := tx.Exec(`DELETE FROM submitted_snapshots WHERE task_id = ?`, id); err != nil {
		return Task{}, sessionInternal(session.ID, "invalidate stale submitted snapshots", err)
	}
	if _, err := tx.Exec(`DELETE FROM task_submissions WHERE task_id = ?`, id); err != nil {
		return Task{}, sessionInternal(session.ID, "invalidate stale task submission", err)
	}
	if _, err := tx.Exec(`UPDATE task_leases SET status = 'closed' WHERE task_id = ?`, id); err != nil {
		return Task{}, sessionInternal(session.ID, "close failed worker lease", err)
	}
	result, err := tx.Exec(`UPDATE tasks SET status = 'repair_pending', worker_identity = NULL, assignment_token = NULL, updated_at = ? WHERE id = ? AND status = ?`, now, id, status)
	if err != nil {
		return Task{}, sessionInternal(session.ID, "mark task repair pending", err)
	}
	if updated, err := result.RowsAffected(); err != nil || updated != 1 {
		if err == nil {
			err = fmt.Errorf("updated %d tasks", updated)
		}
		return Task{}, sessionInternal(session.ID, "confirm task repair", err)
	}
	auditResult, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, worker_identity, termination_proof, occurred_at) VALUES(?, ?, 'task_repair_requested', ?, 'repair_pending', ?, ?, ?)`, session.ID, id, status, nullableString(workerIdentity), nullableString(request.TerminationProof), now)
	if err != nil {
		return Task{}, sessionInternal(session.ID, "audit task repair", err)
	}
	auditSequence, err := auditResult.LastInsertId()
	if err != nil {
		return Task{}, sessionInternal(session.ID, "read task repair audit identity", err)
	}
	if projectError := persistTaskRepairDetails(tx, session.ID, id, auditSequence, request, recoveryMethod, invalidatedSubmission, currentSnapshots, invalidatedSnapshots); projectError != nil {
		return Task{}, projectError
	}
	if err := tx.Commit(); err != nil {
		return Task{}, sessionInternal(session.ID, "commit task repair", err)
	}
	if projectError := p.PrepareMutation("post-task repair"); projectError != nil {
		return Task{}, projectError
	}
	return inspectTask(db, session.ID, id)
}

func (p *Project) captureRepairSnapshots(sessionID string, claims []storedClaim) ([]capturedSnapshot, *Error) {
	snapshots := make([]capturedSnapshot, 0, len(claims))
	for _, claim := range claims {
		current, projectError := p.capturePath(claim.Path)
		if projectError != nil {
			projectError.SessionID = sessionID
			return nil, projectError
		}
		current.Path = claim.Path
		snapshots = append(snapshots, current)
	}
	return snapshots, nil
}

func loadInvalidatedSubmission(queryer databaseQuerier, sessionID, taskID string) (*Submission, map[string]capturedSnapshot, *Error) {
	var submission Submission
	var noChanges int
	err := queryer.QueryRow(`SELECT outcome, no_changes, behavior_changed, key_decisions, validation_expectations, known_risks, submitted_at FROM task_submissions WHERE task_id = ?`, taskID).Scan(&submission.Outcome, &noChanges, &submission.BehaviorChanged, &submission.KeyDecisions, &submission.ValidationExpectations, &submission.KnownRisks, &submission.SubmittedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, map[string]capturedSnapshot{}, nil
	}
	if err != nil {
		return nil, nil, sessionInternal(sessionID, "read invalidated task submission", err)
	}
	submission.NoChanges = noChanges != 0
	rows, err := queryer.Query(`SELECT path, presence, file_type, content_hash, executable, content FROM submitted_snapshots WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, nil, sessionInternal(sessionID, "read invalidated submitted snapshots", err)
	}
	defer rows.Close()
	snapshots := make(map[string]capturedSnapshot)
	for rows.Next() {
		var snapshot capturedSnapshot
		var contentHash sql.NullString
		var executable int
		if err := rows.Scan(&snapshot.Path, &snapshot.Presence, &snapshot.Type, &contentHash, &executable, &snapshot.content); err != nil {
			return nil, nil, sessionInternal(sessionID, "read invalidated submitted snapshot", err)
		}
		snapshot.ContentHash = contentHash.String
		snapshot.Executable = executable != 0
		snapshots[snapshot.Path] = snapshot
	}
	if err := rows.Err(); err != nil {
		return nil, nil, sessionInternal(sessionID, "read invalidated submitted snapshots", err)
	}
	return &submission, snapshots, nil
}

func persistTaskRepairDetails(tx *sql.Tx, sessionID, taskID string, auditSequence int64, request RepairRequest, recoveryMethod string, invalidatedSubmission *Submission, currentSnapshots []capturedSnapshot, invalidatedSnapshots map[string]capturedSnapshot) *Error {
	var invalidatedSubmissionJSON any
	if invalidatedSubmission != nil {
		encoded, err := json.Marshal(invalidatedSubmission)
		if err != nil {
			return sessionInternal(sessionID, "encode invalidated task submission", err)
		}
		invalidatedSubmissionJSON = string(encoded)
	}
	if _, err := tx.Exec(`INSERT INTO task_repair_events(task_audit_sequence, diagnosis, intended_repair, recovery_method, user_confirmation, invalidated_submission_json) VALUES(?, ?, ?, ?, ?, ?)`, auditSequence, request.Diagnosis, request.IntendedRepair, recoveryMethod, nullableString(request.UserConfirmation), invalidatedSubmissionJSON); err != nil {
		return sessionInternal(sessionID, "record task repair handoff", err)
	}
	for _, snapshot := range currentSnapshots {
		invalidated, hadSubmission := invalidatedSnapshots[snapshot.Path]
		var invalidatedPresence, invalidatedType, invalidatedHash, invalidatedExecutable, invalidatedContent any
		if hadSubmission {
			invalidatedPresence = invalidated.Presence
			invalidatedType = invalidated.Type
			invalidatedHash = nullableString(invalidated.ContentHash)
			invalidatedExecutable = invalidated.Executable
			invalidatedContent = nullableBytes(invalidated.content)
		}
		if _, err := tx.Exec(`INSERT INTO task_repair_snapshots(task_audit_sequence, task_id, path, presence, file_type, content_hash, executable, content, invalidated_presence, invalidated_type, invalidated_content_hash, invalidated_executable, invalidated_content) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, auditSequence, taskID, snapshot.Path, snapshot.Presence, snapshot.Type, nullableString(snapshot.ContentHash), snapshot.Executable, nullableBytes(snapshot.content), invalidatedPresence, invalidatedType, invalidatedHash, invalidatedExecutable, invalidatedContent); err != nil {
			return sessionInternal(sessionID, "record task repair snapshot", err)
		}
	}
	return nil
}
