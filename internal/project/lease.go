package project

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type WorkerLease struct {
	Status    string `json:"status"`
	RenewedAt string `json:"renewed_at"`
	ExpiresAt string `json:"expires_at"`
}

func (p *Project) HeartbeatTask(id, assignmentToken string) (Task, *Error) {
	if strings.TrimSpace(assignmentToken) == "" {
		return Task{}, invalid("assignment_token_required", "The current assignment token is required.")
	}
	sessionID, projectError := p.renewWorkerLease(id, assignmentToken, "assigned", "editing")
	if projectError != nil {
		return Task{}, projectError
	}
	db, projectError := p.openState()
	if projectError != nil {
		return Task{}, projectError
	}
	defer db.Close()
	return inspectTask(db, sessionID, id)
}

func (p *Project) renewWorkerLease(taskID, token string, allowedStatuses ...string) (string, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return "", projectError
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return "", internal("begin worker lease renewal", err)
	}
	defer tx.Rollback()
	session, projectError := inspectActiveSession(tx)
	if projectError != nil {
		return "", projectError
	}
	var status, currentToken, workerIdentity string
	var leaseStatus, expiresAt string
	var durationNanos int64
	err = tx.QueryRow(`
		SELECT task.status, COALESCE(task.assignment_token, ''), COALESCE(task.worker_identity, ''),
			lease.status, lease.duration_nanos, lease.expires_at
		FROM tasks task
		JOIN task_leases lease ON lease.task_id = task.id
		WHERE task.session_id = ? AND task.id = ?`, session.ID, taskID).Scan(&status, &currentToken, &workerIdentity, &leaseStatus, &durationNanos, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", invalidSession(session.ID, "task_not_found", fmt.Sprintf("Task %s does not have a worker lease.", taskID))
	}
	if err != nil {
		return "", sessionInternal(session.ID, "read worker lease", err)
	}
	if currentToken == "" || token != currentToken {
		return "", invalidSession(session.ID, "invalid_assignment_token", fmt.Sprintf("The assignment token for task %s is missing, stale, or belongs to another task.", taskID))
	}
	allowed := false
	for _, candidate := range allowedStatuses {
		if status == candidate {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", invalidSession(session.ID, "invalid_task_state", fmt.Sprintf("Task %s must be %s, not %s.", taskID, strings.Join(allowedStatuses, " or "), status))
	}
	if leaseStatus != "active" {
		return "", quarantined(session.ID, "lease_expired", fmt.Sprintf("The worker lease for task %s expired and its ownership is quarantined.", taskID))
	}
	expiry, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return "", sessionInternal(session.ID, "parse worker lease expiry", err)
	}
	now := time.Now().UTC()
	if !now.Before(expiry) {
		if projectError := expireTaskLease(tx, session.ID, taskID, status, workerIdentity, now); projectError != nil {
			return "", projectError
		}
		if err := tx.Commit(); err != nil {
			return "", sessionInternal(session.ID, "commit worker lease expiry", err)
		}
		return "", quarantined(session.ID, "lease_expired", fmt.Sprintf("The worker lease for task %s expired and its ownership is quarantined.", taskID))
	}
	renewedAt := now.Format(time.RFC3339Nano)
	expiresAt = now.Add(time.Duration(durationNanos)).Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE task_leases SET renewed_at = ?, expires_at = ? WHERE task_id = ? AND status = 'active'`, renewedAt, expiresAt, taskID); err != nil {
		return "", sessionInternal(session.ID, "renew worker lease", err)
	}
	if err := tx.Commit(); err != nil {
		return "", sessionInternal(session.ID, "commit worker lease renewal", err)
	}
	return session.ID, nil
}

func sweepExpiredLeases(tx *sql.Tx, sessionID string, now time.Time) *Error {
	rows, err := tx.Query(`
		SELECT task.id, task.status, COALESCE(task.worker_identity, ''), lease.expires_at
		FROM tasks task
		JOIN task_leases lease ON lease.task_id = task.id
		WHERE task.session_id = ? AND task.status IN ('assigned', 'editing') AND lease.status = 'active'`, sessionID)
	if err != nil {
		return sessionInternal(sessionID, "inspect worker lease expiry", err)
	}
	type candidate struct{ taskID, status, workerIdentity, expiresAt string }
	var candidates []candidate
	for rows.Next() {
		var current candidate
		if err := rows.Scan(&current.taskID, &current.status, &current.workerIdentity, &current.expiresAt); err != nil {
			rows.Close()
			return sessionInternal(sessionID, "read worker lease expiry", err)
		}
		candidates = append(candidates, current)
	}
	if err := rows.Close(); err != nil {
		return sessionInternal(sessionID, "close worker lease scan", err)
	}
	if err := rows.Err(); err != nil {
		return sessionInternal(sessionID, "inspect worker lease expiry", err)
	}
	for _, current := range candidates {
		expiresAt, err := time.Parse(time.RFC3339Nano, current.expiresAt)
		if err != nil {
			return sessionInternal(sessionID, "parse worker lease expiry", err)
		}
		if now.Before(expiresAt) {
			continue
		}
		if projectError := expireTaskLease(tx, sessionID, current.taskID, current.status, current.workerIdentity, now); projectError != nil {
			return projectError
		}
	}
	return nil
}

func expireTaskLease(tx *sql.Tx, sessionID, taskID, fromStatus, workerIdentity string, now time.Time) *Error {
	timestamp := now.Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE task_leases SET status = 'expired' WHERE task_id = ? AND status = 'active'`, taskID); err != nil {
		return sessionInternal(sessionID, "expire worker lease", err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET status = 'quarantined', updated_at = ? WHERE id = ? AND status = ?`, timestamp, taskID, fromStatus); err != nil {
		return sessionInternal(sessionID, "quarantine expired task", err)
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, worker_identity, occurred_at) VALUES(?, ?, 'lease_expired', ?, 'quarantined', ?, ?)`, sessionID, taskID, fromStatus, nullableString(workerIdentity), timestamp); err != nil {
		return sessionInternal(sessionID, "record worker lease expiry", err)
	}
	return nil
}
