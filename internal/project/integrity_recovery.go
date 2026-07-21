package project

import (
	"database/sql"
	"strings"
	"time"
)

func (p *Project) RecoverIntegrity(confirmation string) (Session, *Error) {
	if strings.TrimSpace(confirmation) == "" {
		return Session{}, invalid("recovery_confirmation_required", "Integrity recovery requires an explicit confirmation describing the inspection and restoration performed.")
	}
	db, projectError := p.openState()
	if projectError != nil {
		return Session{}, projectError
	}
	defer db.Close()
	session, projectError := p.inspectOpenSession(db)
	if projectError != nil {
		return Session{}, projectError
	}
	var unresolved int
	if err := db.QueryRow(`SELECT COUNT(*) FROM integrity_violations WHERE session_id = ? AND recovered_at IS NULL`, session.ID).Scan(&unresolved); err != nil {
		return Session{}, sessionInternal(session.ID, "inspect unresolved integrity violations", err)
	}
	if unresolved == 0 {
		var recovered int
		if err := db.QueryRow(`SELECT COUNT(*) FROM integrity_violations WHERE session_id = ? AND recovered_at IS NOT NULL`, session.ID).Scan(&recovered); err != nil {
			return Session{}, sessionInternal(session.ID, "inspect recovered integrity violations", err)
		}
		if recovered != 0 {
			if projectError := validateSessionBatchPair(db, session); projectError != nil {
				return Session{}, projectError
			}
			return p.inspectSession(db, session.ID)
		}
		return Session{}, invalidSession(session.ID, "no_integrity_violation", "The paused session has no unresolved integrity violation.")
	}
	if session.Status != "paused" {
		return Session{}, invalidSession(session.ID, "integrity_recovery_requires_paused_session", "Integrity recovery requires a paused session.")
	}
	observations, projectError := p.scanRepository(db, session)
	if projectError != nil {
		return Session{}, projectError
	}
	if len(observations) != 0 {
		if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
			return Session{}, projectError
		}
		return Session{}, invalidSession(session.ID, "integrity_not_restored", "Repository integrity is still inconsistent; restore every observed violation before recovery.")
	}
	if projectError := p.StopIntegrityMonitor(session.ID); projectError != nil {
		return Session{}, projectError
	}

	tx, err := db.Begin()
	if err != nil {
		return Session{}, sessionInternal(session.ID, "begin integrity recovery", err)
	}
	defer tx.Rollback()
	type quarantine struct {
		violationID   int64
		taskID        sql.NullString
		batchID       sql.NullString
		previousState string
	}
	rows, err := tx.Query(`
		SELECT quarantine.violation_id, quarantine.task_id, quarantine.batch_id, quarantine.previous_status
		FROM integrity_quarantines quarantine
		JOIN integrity_violations violation ON violation.id = quarantine.violation_id
		WHERE violation.session_id = ? AND violation.recovered_at IS NULL
		ORDER BY quarantine.violation_id`, session.ID)
	if err != nil {
		return Session{}, sessionInternal(session.ID, "read integrity quarantines", err)
	}
	var quarantines []quarantine
	for rows.Next() {
		var current quarantine
		if err := rows.Scan(&current.violationID, &current.taskID, &current.batchID, &current.previousState); err != nil {
			rows.Close()
			return Session{}, sessionInternal(session.ID, "read integrity quarantine", err)
		}
		quarantines = append(quarantines, current)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Session{}, sessionInternal(session.ID, "read integrity quarantines", err)
	}
	if err := rows.Close(); err != nil {
		return Session{}, sessionInternal(session.ID, "close integrity quarantines", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	restoredSessionStatus := "paused"
	for _, current := range quarantines {
		if !current.batchID.Valid {
			continue
		}
		targetSession, _, ok := recoveryTransition(current.previousState)
		if !ok {
			return Session{}, invalidSession(session.ID, "unsupported_recovery_state", "The quarantined batch's previous state cannot be recovered safely.")
		}
		if restoredSessionStatus != "paused" && restoredSessionStatus != targetSession {
			return Session{}, invalidSession(session.ID, "incompatible_recovery_states", "Quarantined batches require incompatible restored session states.")
		}
		restoredSessionStatus = targetSession
	}
	for _, current := range quarantines {
		if current.taskID.Valid {
			if _, err := tx.Exec(`UPDATE tasks SET status = ?, updated_at = ? WHERE id = ? AND status = 'quarantined'`, current.previousState, now, current.taskID.String); err != nil {
				return Session{}, sessionInternal(session.ID, "restore task after integrity recovery", err)
			}
			if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'integrity_recovered', 'quarantined', ?, ?)`, session.ID, current.taskID.String, current.previousState, now); err != nil {
				return Session{}, sessionInternal(session.ID, "audit task integrity recovery", err)
			}
		}
		if current.batchID.Valid {
			_, restoredStatus, ok := recoveryTransition(current.previousState)
			if !ok {
				return Session{}, invalidSession(session.ID, "unsupported_recovery_state", "The quarantined batch's previous state cannot be recovered safely.")
			}
			if _, err := tx.Exec(`UPDATE batches SET status = ?, updated_at = ? WHERE id = ? AND status = 'quarantined'`, restoredStatus, now, current.batchID.String); err != nil {
				return Session{}, sessionInternal(session.ID, "restore batch after integrity recovery", err)
			}
			if _, err := tx.Exec(`INSERT INTO batch_audit_events(session_id, batch_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'integrity_recovered', 'quarantined', ?, ?)`, session.ID, current.batchID.String, restoredStatus, now); err != nil {
				return Session{}, sessionInternal(session.ID, "audit batch integrity recovery", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE integrity_violations SET recovered_at = ?, recovery_confirmation = ? WHERE session_id = ? AND recovered_at IS NULL`, now, confirmation, session.ID); err != nil {
		return Session{}, sessionInternal(session.ID, "resolve integrity violations", err)
	}
	if restoredSessionStatus != "paused" {
		if _, err := tx.Exec(`UPDATE sessions SET status = ?, updated_at = ? WHERE id = ? AND status = 'paused'`, restoredSessionStatus, now, session.ID); err != nil {
			return Session{}, sessionInternal(session.ID, "restore compatible session after integrity recovery", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO audit_events(session_id, event, from_status, to_status, occurred_at) VALUES(?, 'integrity_recovered', 'paused', ?, ?)`, session.ID, restoredSessionStatus, now); err != nil {
		return Session{}, sessionInternal(session.ID, "audit integrity recovery", err)
	}
	if err := tx.Commit(); err != nil {
		return Session{}, sessionInternal(session.ID, "commit integrity recovery", err)
	}
	if restoredSessionStatus == "active" {
		if projectError := p.StartIntegrityMonitor(session.ID); projectError != nil {
			return Session{}, projectError
		}
	}
	return p.inspectSession(db, session.ID)
}
