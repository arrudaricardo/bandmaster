package project

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type IntegrityViolation struct {
	ID                   int64           `json:"id"`
	Kind                 string          `json:"kind"`
	Path                 string          `json:"path,omitempty"`
	ObservedState        json.RawMessage `json:"observed_state"`
	DetectedAt           string          `json:"detected_at"`
	RecoveredAt          string          `json:"recovered_at,omitempty"`
	RecoveryConfirmation string          `json:"recovery_confirmation,omitempty"`
}

type integrityObservation struct {
	Kind          string
	Path          string
	ObservedState any
	TaskID        string
	BatchID       string
}

// PrepareMutation is the common integrity gate for public state-changing commands.
func (p *Project) PrepareMutation(command string) *Error {
	db, projectError := p.openState()
	if projectError != nil {
		return projectError
	}
	defer db.Close()
	session, projectError := inspectOpenSessionWithQueryer(db)
	if projectError != nil {
		if projectError.Code == "session_not_active" {
			return nil
		}
		return projectError
	}
	if session.Status == "paused" {
		var kind, violationPath string
		err := db.QueryRow(`SELECT kind, path FROM integrity_violations WHERE session_id = ? AND recovered_at IS NULL ORDER BY id LIMIT 1`, session.ID).Scan(&kind, &violationPath)
		if err == nil {
			return integrityError(session.ID, integrityObservation{Kind: kind, Path: violationPath})
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return sessionInternal(session.ID, "read unresolved integrity violation", err)
		}
		if command == "session pause" || command == "session finish" || command == "session abort" {
			return nil
		}
		return invalidSession(session.ID, "session_not_active", fmt.Sprintf("Session %s is paused and has no healthy integrity monitor.", session.ID))
	}
	if session.Status == "finalizing" {
		if command == "session finish" || command == "batch freeze" || command == "batch validate" || command == "batch commit" || command == "batch finalize" {
			return nil
		}
		return invalidSession(session.ID, "session_finalizing", fmt.Sprintf("Session %s is finalizing and cannot accept another mutation.", session.ID))
	}
	if session.Status != "active" {
		return nil
	}
	monitor, projectError := inspectLatestMonitor(db, session.ID)
	if projectError != nil && projectError.Code != "monitor_unhealthy" {
		return projectError
	}
	if projectError != nil || !monitorHealthy(monitor, time.Now().UTC()) {
		observation := integrityObservation{Kind: "monitor_unhealthy", Path: ".git/bandmaster/monitor", ObservedState: map[string]any{"monitor": monitor}}
		if projectError := p.persistIntegrityViolations(session, []integrityObservation{observation}); projectError != nil {
			return projectError
		}
		return quarantined(session.ID, "monitor_unhealthy", "The session integrity monitor is not healthy; the session was paused and current work was quarantined.")
	}
	observations, projectError := p.scanRepository(db, session)
	if projectError != nil {
		return projectError
	}
	if len(observations) == 0 {
		return nil
	}
	if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
		return projectError
	}
	return integrityError(session.ID, observations[0])
}

func integrityError(sessionID string, observation integrityObservation) *Error {
	message := fmt.Sprintf("Repository integrity violation %s paused the session and quarantined affected work.", observation.Kind)
	if observation.Path != "" {
		message = fmt.Sprintf("Repository integrity violation %s at %s paused the session and quarantined affected work.", observation.Kind, observation.Path)
	}
	return quarantined(sessionID, observation.Kind, message)
}

func (p *Project) persistIntegrityViolations(session Session, observations []integrityObservation) *Error {
	db, projectError := p.openState()
	if projectError != nil {
		return projectError
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return sessionInternal(session.ID, "begin integrity quarantine", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, observation := range observations {
		encoded, err := json.Marshal(observation.ObservedState)
		if err != nil {
			return sessionInternal(session.ID, "encode integrity observation", err)
		}
		result, err := tx.Exec(`INSERT OR IGNORE INTO integrity_violations(session_id, kind, path, observed_state_json, detected_at) VALUES(?, ?, ?, ?, ?)`, session.ID, observation.Kind, observation.Path, encoded, now)
		if err != nil {
			return sessionInternal(session.ID, "record integrity violation", err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return sessionInternal(session.ID, "confirm integrity violation", err)
		}
		if inserted == 0 {
			continue
		}
		violationID, err := result.LastInsertId()
		if err != nil {
			return sessionInternal(session.ID, "read integrity violation identity", err)
		}
		auditResult, err := tx.Exec(`INSERT INTO audit_events(session_id, event, to_status, occurred_at) VALUES(?, 'integrity_violation_observed', 'paused', ?)`, session.ID, now)
		if err != nil {
			return sessionInternal(session.ID, "append integrity violation audit", err)
		}
		auditSequence, err := auditResult.LastInsertId()
		if err != nil {
			return sessionInternal(session.ID, "read integrity audit identity", err)
		}
		if _, err := tx.Exec(`INSERT INTO integrity_audit_events(audit_sequence, violation_id, kind, path, observed_state_json) VALUES(?, ?, ?, ?, ?)`, auditSequence, violationID, observation.Kind, observation.Path, encoded); err != nil {
			return sessionInternal(session.ID, "record integrity audit evidence", err)
		}
		if observation.TaskID != "" {
			if projectError := quarantineTaskForIntegrity(tx, session.ID, violationID, observation.TaskID, now); projectError != nil {
				return projectError
			}
		}
		if observation.BatchID != "" {
			if projectError := quarantineBatchForIntegrity(tx, session.ID, violationID, observation.BatchID, now); projectError != nil {
				return projectError
			}
		}
		if observation.TaskID == "" && observation.BatchID == "" {
			if projectError := quarantineCurrentBatches(tx, session.ID, violationID, now); projectError != nil {
				return projectError
			}
		}
	}
	var currentStatus string
	if err := tx.QueryRow(`SELECT status FROM sessions WHERE id = ?`, session.ID).Scan(&currentStatus); err != nil {
		return sessionInternal(session.ID, "read integrity session state", err)
	}
	if currentStatus == "active" || currentStatus == "finalizing" || currentStatus == "completed" {
		if _, err := tx.Exec(`UPDATE sessions SET status = 'paused', updated_at = ? WHERE id = ? AND status = ?`, now, session.ID, currentStatus); err != nil {
			return sessionInternal(session.ID, "pause session for integrity violation", err)
		}
		if _, err := tx.Exec(`INSERT INTO audit_events(session_id, event, from_status, to_status, occurred_at) VALUES(?, 'integrity_violation', ?, 'paused', ?)`, session.ID, currentStatus, now); err != nil {
			return sessionInternal(session.ID, "audit integrity pause", err)
		}
	}
	if _, err := tx.Exec(`UPDATE session_monitors SET status = 'unhealthy' WHERE session_id = ? AND generation = (SELECT MAX(generation) FROM session_monitors WHERE session_id = ?)`, session.ID, session.ID); err != nil {
		return sessionInternal(session.ID, "mark integrity monitor unhealthy", err)
	}
	if err := tx.Commit(); err != nil {
		return sessionInternal(session.ID, "commit integrity quarantine", err)
	}
	return nil
}

func quarantineTaskForIntegrity(tx *sql.Tx, sessionID string, violationID int64, taskID, now string) *Error {
	var status, worker string
	if err := tx.QueryRow(`SELECT status, COALESCE(worker_identity, '') FROM tasks WHERE id = ?`, taskID).Scan(&status, &worker); err != nil {
		return sessionInternal(sessionID, "read affected task", err)
	}
	if status == "quarantined" || status == "committed" || status == "no_op" || status == "canceled" {
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO integrity_quarantines(violation_id, task_id, previous_status) VALUES(?, ?, ?)`, violationID, taskID, status); err != nil {
		return sessionInternal(sessionID, "record task integrity quarantine", err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET status = 'quarantined', updated_at = ? WHERE id = ?`, now, taskID); err != nil {
		return sessionInternal(sessionID, "quarantine affected task", err)
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, worker_identity, occurred_at) VALUES(?, ?, 'integrity_violation', ?, 'quarantined', ?, ?)`, sessionID, taskID, status, nullableString(worker), now); err != nil {
		return sessionInternal(sessionID, "audit task integrity quarantine", err)
	}
	return nil
}

func quarantineBatchForIntegrity(tx *sql.Tx, sessionID string, violationID int64, batchID, now string) *Error {
	var status string
	if err := tx.QueryRow(`SELECT status FROM batches WHERE id = ?`, batchID).Scan(&status); err != nil {
		return sessionInternal(sessionID, "read affected batch", err)
	}
	if status == "quarantined" || status == "committed" {
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO integrity_quarantines(violation_id, batch_id, previous_status) VALUES(?, ?, ?)`, violationID, batchID, status); err != nil {
		return sessionInternal(sessionID, "record batch integrity quarantine", err)
	}
	if _, err := tx.Exec(`UPDATE batches SET status = 'quarantined', updated_at = ? WHERE id = ?`, now, batchID); err != nil {
		return sessionInternal(sessionID, "quarantine affected batch", err)
	}
	return nil
}

func quarantineCurrentBatches(tx *sql.Tx, sessionID string, violationID int64, now string) *Error {
	rows, err := tx.Query(`SELECT id FROM batches WHERE session_id = ? AND status != 'committed'`, sessionID)
	if err != nil {
		return sessionInternal(sessionID, "inspect affected batches", err)
	}
	var batchIDs []string
	for rows.Next() {
		var batchID string
		if err := rows.Scan(&batchID); err != nil {
			rows.Close()
			return sessionInternal(sessionID, "read affected batch", err)
		}
		batchIDs = append(batchIDs, batchID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return sessionInternal(sessionID, "inspect affected batches", err)
	}
	if err := rows.Close(); err != nil {
		return sessionInternal(sessionID, "close affected batch scan", err)
	}
	for _, batchID := range batchIDs {
		taskRows, err := tx.Query(`SELECT task_id FROM batch_members WHERE batch_id = ?`, batchID)
		if err != nil {
			return sessionInternal(sessionID, "inspect affected batch tasks", err)
		}
		var taskIDs []string
		for taskRows.Next() {
			var taskID string
			if err := taskRows.Scan(&taskID); err != nil {
				taskRows.Close()
				return sessionInternal(sessionID, "read affected batch task", err)
			}
			taskIDs = append(taskIDs, taskID)
		}
		if err := taskRows.Err(); err != nil {
			taskRows.Close()
			return sessionInternal(sessionID, "inspect affected batch tasks", err)
		}
		if err := taskRows.Close(); err != nil {
			return sessionInternal(sessionID, "close affected batch task scan", err)
		}
		for _, taskID := range taskIDs {
			if projectError := quarantineTaskForIntegrity(tx, sessionID, violationID, taskID, now); projectError != nil {
				return projectError
			}
		}
		if projectError := quarantineBatchForIntegrity(tx, sessionID, violationID, batchID, now); projectError != nil {
			return projectError
		}
	}
	return nil
}
