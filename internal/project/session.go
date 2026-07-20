package project

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type Session struct {
	ID                  string               `json:"id"`
	Status              string               `json:"status"`
	StartingBranch      string               `json:"starting_branch"`
	StartingCommit      string               `json:"starting_commit"`
	CreatedAt           string               `json:"created_at"`
	UpdatedAt           string               `json:"updated_at"`
	Monitor             *IntegrityMonitor    `json:"monitor,omitempty"`
	IntegrityViolations []IntegrityViolation `json:"integrity_violations"`
	AuditHistory        []AuditEvent         `json:"audit_history"`
}

type AuditEvent struct {
	Sequence             int64           `json:"sequence"`
	Event                string          `json:"event"`
	FromStatus           string          `json:"from_status,omitempty"`
	ToStatus             string          `json:"to_status"`
	IntegrityViolationID int64           `json:"integrity_violation_id,omitempty"`
	IntegrityKind        string          `json:"integrity_kind,omitempty"`
	IntegrityPath        string          `json:"integrity_path,omitempty"`
	ObservedState        json.RawMessage `json:"observed_state,omitempty"`
	OccurredAt           string          `json:"occurred_at"`
}

func (p *Project) StartSession() (Session, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return Session{}, projectError
	}
	defer db.Close()
	var activeID string
	err := db.QueryRow(`SELECT id FROM sessions WHERE status IN ('active', 'paused', 'finalizing', 'aborting')`).Scan(&activeID)
	if err == nil {
		return Session{}, invalidSession(activeID, "session_already_active", fmt.Sprintf("Session %s is already open in this repository.", activeID))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Session{}, internal("inspect active session", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return Session{}, internal("begin session transition", err)
	}
	defer tx.Rollback()

	err = tx.QueryRow(`SELECT id FROM sessions WHERE status IN ('active', 'paused', 'finalizing', 'aborting')`).Scan(&activeID)
	if err == nil {
		return Session{}, invalidSession(activeID, "session_already_active", fmt.Sprintf("Session %s is already open in this repository.", activeID))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Session{}, internal("inspect active session", err)
	}
	status, projectError := p.configStatus(tx)
	if projectError != nil {
		return Session{}, projectError
	}
	if !status.Approved {
		return Session{}, invalid("configuration_not_approved", fmt.Sprintf("Validation configuration %s is not approved.", status.ValidationDigest))
	}
	branch, commit, projectError := p.cleanBaseline()
	if projectError != nil {
		return Session{}, projectError
	}
	id, err := newSessionID()
	if err != nil {
		return Session{}, internal("generate session identity", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.Exec(`INSERT INTO sessions(id, status, starting_branch, starting_commit, created_at, updated_at) VALUES(?, 'active', ?, ?, ?, ?)`, id, branch, commit, now, now); err != nil {
		return Session{}, internal("create session", err)
	}
	if _, err = tx.Exec(`INSERT INTO audit_events(session_id, event, to_status, occurred_at) VALUES(?, 'session_started', 'active', ?)`, id, now); err != nil {
		return Session{}, internal("record session start", err)
	}
	if err = tx.Commit(); err != nil {
		return Session{}, internal("commit session start", err)
	}
	if projectError := p.StartIntegrityMonitor(id); projectError != nil {
		return Session{}, projectError
	}
	return p.inspectSession(db, id)
}

// AbortSession preserves working-tree edits while ending orchestration. Since this
// CLI has no parent-held worker handles, clearing claims requires an explicit,
// durable user confirmation that every assigned worker has stopped.
func (p *Project) AbortSession(terminationConfirmation string) (Session, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return Session{}, projectError
	}
	defer db.Close()
	current, projectError := p.inspectOpenSession(db)
	if projectError != nil {
		return Session{}, projectError
	}
	if current.Status == "finalizing" {
		return Session{}, invalidSession(current.ID, "session_finalizing", "A finalizing session must recover or finish before it can be aborted.")
	}
	if current.Status != "active" && current.Status != "paused" && current.Status != "aborting" {
		return Session{}, invalidSession(current.ID, "invalid_session_transition", fmt.Sprintf("Cannot abort a session in %s state.", current.Status))
	}
	if current.Status != "aborting" {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := db.Exec(`UPDATE sessions SET status = 'aborting', updated_at = ? WHERE id = ? AND status IN ('active', 'paused')`, now, current.ID); err != nil {
			return Session{}, sessionInternal(current.ID, "begin abort", err)
		}
		if _, err := db.Exec(`INSERT INTO audit_events(session_id, event, from_status, to_status, occurred_at) VALUES(?, 'session_aborting', ?, 'aborting', ?)`, current.ID, current.Status, now); err != nil {
			return Session{}, sessionInternal(current.ID, "audit abort", err)
		}
	}
	if projectError := p.StopIntegrityMonitor(current.ID); projectError != nil {
		return Session{}, projectError
	}
	var workers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE session_id = ? AND status NOT IN ('committed', 'no_op', 'canceled') AND worker_identity IS NOT NULL`, current.ID).Scan(&workers); err != nil {
		return Session{}, sessionInternal(current.ID, "inspect abort workers", err)
	}
	if workers != 0 && strings.TrimSpace(terminationConfirmation) == "" {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := db.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, worker_identity, occurred_at) SELECT session_id, id, 'abort_termination_unproven', status, 'quarantined', worker_identity, ? FROM tasks WHERE session_id = ? AND status NOT IN ('committed', 'no_op', 'canceled', 'quarantined') AND worker_identity IS NOT NULL`, now, current.ID); err != nil {
			return Session{}, sessionInternal(current.ID, "audit unproven abort workers", err)
		}
		if _, err := db.Exec(`UPDATE tasks SET status = 'quarantined', updated_at = ? WHERE session_id = ? AND status NOT IN ('committed', 'no_op', 'canceled', 'quarantined') AND worker_identity IS NOT NULL`, now, current.ID); err != nil {
			return Session{}, sessionInternal(current.ID, "quarantine unproven abort workers", err)
		}
		return Session{}, invalidSession(current.ID, "worker_termination_confirmation_required", "Abort is waiting for proof that assigned workers have stopped; provide --termination-confirmation.")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := db.Begin()
	if err != nil {
		return Session{}, sessionInternal(current.ID, "begin abort cleanup", err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, status, COALESCE(worker_identity, '') FROM tasks WHERE session_id = ? AND status NOT IN ('committed', 'no_op', 'canceled')`, current.ID)
	if err != nil {
		return Session{}, sessionInternal(current.ID, "inspect abort tasks", err)
	}
	defer rows.Close()
	type abortTask struct{ id, status, worker string }
	var tasks []abortTask
	for rows.Next() {
		var task abortTask
		if err := rows.Scan(&task.id, &task.status, &task.worker); err != nil {
			return Session{}, sessionInternal(current.ID, "read abort task", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return Session{}, sessionInternal(current.ID, "read abort tasks", err)
	}
	for _, task := range tasks {
		if task.status == "quarantined" {
			continue
		}
		if _, err := tx.Exec(`UPDATE tasks SET status = 'quarantined', updated_at = ? WHERE id = ?`, now, task.id); err != nil {
			return Session{}, sessionInternal(current.ID, "quarantine aborted task", err)
		}
		if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, worker_identity, termination_proof, occurred_at) VALUES(?, ?, 'session_aborted', ?, 'quarantined', ?, ?, ?)`, current.ID, task.id, task.status, nullableString(task.worker), nullableString(terminationConfirmation), now); err != nil {
			return Session{}, sessionInternal(current.ID, "audit aborted task", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM claims WHERE session_id = ?`, current.ID); err != nil {
		return Session{}, sessionInternal(current.ID, "clear aborted claims", err)
	}
	if _, err := tx.Exec(`INSERT INTO session_abort_events(session_id, termination_confirmation, occurred_at) VALUES(?, ?, ?)`, current.ID, terminationConfirmation, now); err != nil {
		return Session{}, sessionInternal(current.ID, "record abort confirmation", err)
	}
	if _, err := tx.Exec(`UPDATE sessions SET status = 'aborted', updated_at = ? WHERE id = ? AND status = 'aborting'`, now, current.ID); err != nil {
		return Session{}, sessionInternal(current.ID, "complete abort", err)
	}
	if _, err := tx.Exec(`INSERT INTO audit_events(session_id, event, from_status, to_status, occurred_at) VALUES(?, 'session_aborted', 'aborting', 'aborted', ?)`, current.ID, now); err != nil {
		return Session{}, sessionInternal(current.ID, "audit completed abort", err)
	}
	if err := tx.Commit(); err != nil {
		return Session{}, sessionInternal(current.ID, "commit abort", err)
	}
	return p.inspectSession(db, current.ID)
}

func (p *Project) InspectSession() (Session, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return Session{}, projectError
	}
	defer db.Close()
	return p.inspectSession(db, "")
}

func (p *Project) TransitionSession(action string) (Session, *Error) {
	transition, exists := map[string]struct {
		from  string
		to    string
		event string
	}{
		"pause":  {from: "active", to: "paused", event: "session_paused"},
		"resume": {from: "paused", to: "active", event: "session_resumed"},
		"finish": {from: "active", to: "finalizing", event: "session_finalizing"},
	}[action]
	if !exists {
		return Session{}, invalid("invalid_arguments", fmt.Sprintf("Unknown session transition %q.", action))
	}

	db, projectError := p.openState()
	if projectError != nil {
		return Session{}, projectError
	}
	defer db.Close()
	if action == "resume" {
		current, projectError := p.inspectOpenSession(db)
		if projectError != nil {
			return Session{}, projectError
		}
		status, projectError := p.configStatus(db)
		if projectError != nil {
			projectError.SessionID = current.ID
			return Session{}, projectError
		}
		if !status.Approved {
			return Session{}, invalidSession(current.ID, "configuration_not_approved", fmt.Sprintf("Validation configuration %s is not approved.", status.ValidationDigest))
		}
		var unresolved int
		if err := db.QueryRow(`SELECT COUNT(*) FROM integrity_violations WHERE session_id = ? AND recovered_at IS NULL`, current.ID).Scan(&unresolved); err != nil {
			return Session{}, sessionInternal(current.ID, "inspect integrity recovery state", err)
		}
		if unresolved != 0 {
			return Session{}, invalidSession(current.ID, "integrity_recovery_required", "Explicit audited integrity recovery is required before this session can resume.")
		}
		observations, projectError := p.scanRepository(db, current)
		if projectError != nil {
			return Session{}, projectError
		}
		if len(observations) != 0 {
			if projectError := p.persistIntegrityViolations(current, observations); projectError != nil {
				return Session{}, projectError
			}
			return Session{}, integrityError(current.ID, observations[0])
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return Session{}, internal("begin session transition", err)
	}
	defer tx.Rollback()
	current, projectError := p.inspectOpenSession(tx)
	if projectError != nil {
		if action == "finish" && projectError.Code == "session_not_active" {
			latest, inspectError := inspectLatestSession(tx)
			if inspectError == nil && latest.Status == "completed" {
				if err := tx.Rollback(); err != nil {
					return Session{}, sessionInternal(latest.ID, "close idempotent session transition", err)
				}
				if projectError := p.completeSessionFinish(db, latest); projectError != nil {
					return Session{}, projectError
				}
				return p.inspectSession(db, latest.ID)
			}
		}
		return Session{}, projectError
	}
	if action == "finish" && current.Status == "finalizing" {
		var frozenBatches int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM batches WHERE session_id = ? AND status IN ('frozen', 'validating', 'repair_pending', 'repairing', 'finalizing', 'final_validating', 'quarantined')`, current.ID).Scan(&frozenBatches); err != nil {
			return Session{}, sessionInternal(current.ID, "inspect batch finalization state", err)
		}
		if frozenBatches != 0 {
			return Session{}, invalidSession(current.ID, "batch_finalization_in_progress", "A frozen batch must complete validation and finalization before the session can finish.")
		}
		if err := tx.Rollback(); err != nil {
			return Session{}, sessionInternal(current.ID, "close resumed session finalization", err)
		}
		if projectError := p.completeSessionFinish(db, current); projectError != nil {
			return Session{}, projectError
		}
		return p.inspectSession(db, current.ID)
	}
	if current.Status == transition.to {
		if err := tx.Rollback(); err != nil {
			return Session{}, sessionInternal(current.ID, "close idempotent session transition", err)
		}
		return p.inspectSession(db, current.ID)
	}
	if current.Status != transition.from {
		return Session{}, invalidSession(current.ID, "invalid_session_transition", fmt.Sprintf("Cannot %s a session in %s state.", action, current.Status))
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if action == "resume" {
		status, projectError := p.configStatus(tx)
		if projectError != nil {
			projectError.SessionID = current.ID
			return Session{}, projectError
		}
		if !status.Approved {
			return Session{}, invalidSession(current.ID, "configuration_not_approved", fmt.Sprintf("Validation configuration %s is not approved.", status.ValidationDigest))
		}
	}
	if action == "finish" {
		if projectError := p.verifySessionBaseline(current); projectError != nil {
			projectError.SessionID = current.ID
			return Session{}, projectError
		}
	}
	if action == "finish" {
		var incomplete, claims int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE session_id = ? AND status NOT IN ('committed', 'no_op', 'canceled')`, current.ID).Scan(&incomplete); err != nil {
			return Session{}, sessionInternal(current.ID, "inspect incomplete session tasks", err)
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM claims WHERE session_id = ?`, current.ID).Scan(&claims); err != nil {
			return Session{}, sessionInternal(current.ID, "inspect unreleased session claims", err)
		}
		if incomplete != 0 || claims != 0 {
			return Session{}, invalidSession(current.ID, "session_tasks_incomplete", fmt.Sprintf("Session %s has %d incomplete task(s) and %d unreleased claim(s).", current.ID, incomplete, claims))
		}
	}

	result, err := tx.Exec(`UPDATE sessions SET status = ?, updated_at = ? WHERE id = ? AND status = ?`, transition.to, now, current.ID, transition.from)
	if err != nil {
		return Session{}, sessionInternal(current.ID, "update session state", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return Session{}, sessionInternal(current.ID, "confirm session transition", err)
	}
	if updated != 1 {
		return Session{}, invalidSession(current.ID, "invalid_session_transition", "Session state changed before the transition could be applied.")
	}
	if _, err := tx.Exec(`INSERT INTO audit_events(session_id, event, from_status, to_status, occurred_at) VALUES(?, ?, ?, ?, ?)`, current.ID, transition.event, transition.from, transition.to, now); err != nil {
		return Session{}, sessionInternal(current.ID, "record session transition", err)
	}
	if err := tx.Commit(); err != nil {
		return Session{}, sessionInternal(current.ID, "commit session transition", err)
	}
	if action == "finish" {
		if projectError := p.completeSessionFinish(db, current); projectError != nil {
			return Session{}, projectError
		}
	}
	if action == "pause" {
		if projectError := p.StopIntegrityMonitor(current.ID); projectError != nil {
			observation := integrityObservation{Kind: "monitor_unhealthy", Path: ".git/bandmaster/monitor", ObservedState: map[string]any{"error": projectError.Message}}
			if persistError := p.persistIntegrityViolations(current, []integrityObservation{observation}); persistError != nil {
				return Session{}, persistError
			}
			return Session{}, integrityError(current.ID, observation)
		}
	}
	if action == "resume" {
		if projectError := p.StartIntegrityMonitor(current.ID); projectError != nil {
			return Session{}, projectError
		}
	}
	return p.inspectSession(db, current.ID)
}

func (p *Project) completeSessionFinish(db *sql.DB, session Session) *Error {
	var durableStatus string
	if err := db.QueryRow(`SELECT status FROM sessions WHERE id = ?`, session.ID).Scan(&durableStatus); err != nil {
		return sessionInternal(session.ID, "read session completion state", err)
	}
	if durableStatus == "completed" {
		var completedAt string
		err := db.QueryRow(`SELECT full_scan_at FROM session_completion_checks WHERE session_id = ?`, session.ID).Scan(&completedAt)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return sessionInternal(session.ID, "inspect session completion check", err)
		}
	}
	observations, projectError := p.scanRepository(db, session)
	if projectError != nil {
		return projectError
	}
	if len(observations) != 0 {
		if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
			return projectError
		}
		_ = p.StopIntegrityMonitor(session.ID)
		return integrityError(session.ID, observations[0])
	}
	if projectError := p.runSessionFinalValidation(db, session); projectError != nil {
		return projectError
	}
	if projectError := p.StopIntegrityMonitor(session.ID); projectError != nil {
		observation := integrityObservation{Kind: "monitor_unhealthy", Path: ".git/bandmaster/monitor", ObservedState: map[string]any{"error": projectError.Message}}
		if persistError := p.persistIntegrityViolations(session, []integrityObservation{observation}); persistError != nil {
			return persistError
		}
		return integrityError(session.ID, observation)
	}
	return markSessionCompleted(db, session.ID)
}

func (p *Project) runSessionFinalValidation(db *sql.DB, session Session) *Error {
	config, _, projectError := p.readApprovedConfiguration(db)
	if projectError != nil {
		projectError.SessionID = session.ID
		return projectError
	}
	for index, configured := range config.Validation.Commands {
		if status, err := gitOutput(p.Root, "status", "--porcelain=v1"); err != nil || status != "" {
			return invalidSession(session.ID, "session_completion_dirty_worktree", "The repository changed before final session validation.")
		}
		run := p.runOfficialValidationCommand(1, int64(index+1), officialValidationCommand{source: "repository", validationCommand: configured})
		if status, err := gitOutput(p.Root, "status", "--porcelain=v1"); err != nil || status != "" {
			observation := integrityObservation{Kind: "session_final_validation_mutation", Path: ".", ObservedState: map[string]string{"status": status}}
			if projectError := p.persistIntegrityViolations(session, []integrityObservation{observation}); projectError != nil {
				return projectError
			}
			return integrityError(session.ID, observation)
		}
		if run.Status != "passed" {
			return validationFailure(session.ID, run)
		}
	}
	return nil
}

func markSessionCompleted(db *sql.DB, sessionID string) *Error {
	tx, err := db.Begin()
	if err != nil {
		return sessionInternal(sessionID, "begin session completion", err)
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRow(`SELECT status FROM sessions WHERE id = ?`, sessionID).Scan(&status); err != nil {
		return sessionInternal(sessionID, "read session completion state", err)
	}
	if status == "completed" {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.Exec(`INSERT INTO session_completion_checks(session_id, full_scan_at, monitor_stopped_at) VALUES(?, ?, ?) ON CONFLICT(session_id) DO UPDATE SET full_scan_at = excluded.full_scan_at, monitor_stopped_at = excluded.monitor_stopped_at`, sessionID, now, now); err != nil {
			return sessionInternal(sessionID, "record recovered session completion check", err)
		}
		if err := tx.Commit(); err != nil {
			return sessionInternal(sessionID, "commit recovered session completion check", err)
		}
		return nil
	}
	if status != "finalizing" {
		return invalidSession(sessionID, "invalid_session_transition", fmt.Sprintf("Cannot complete a session in %s state.", status))
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO session_completion_checks(session_id, full_scan_at, monitor_stopped_at) VALUES(?, ?, ?) ON CONFLICT(session_id) DO UPDATE SET full_scan_at = excluded.full_scan_at, monitor_stopped_at = excluded.monitor_stopped_at`, sessionID, now, now); err != nil {
		return sessionInternal(sessionID, "record session completion check", err)
	}
	result, err := tx.Exec(`UPDATE sessions SET status = 'completed', updated_at = ? WHERE id = ? AND status = 'finalizing'`, now, sessionID)
	if err != nil {
		return sessionInternal(sessionID, "complete session", err)
	}
	if updated, err := result.RowsAffected(); err != nil || updated != 1 {
		if err == nil {
			err = fmt.Errorf("updated %d sessions", updated)
		}
		return sessionInternal(sessionID, "confirm session completion", err)
	}
	if _, err := tx.Exec(`INSERT INTO audit_events(session_id, event, from_status, to_status, occurred_at) VALUES(?, 'session_completed', 'finalizing', 'completed', ?)`, sessionID, now); err != nil {
		return sessionInternal(sessionID, "record session completion", err)
	}
	if err := tx.Commit(); err != nil {
		return sessionInternal(sessionID, "commit session completion", err)
	}
	return nil
}

func (p *Project) inspectOpenSession(queryer rowQuerier) (Session, *Error) {
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

func inspectLatestSession(queryer rowQuerier) (Session, *Error) {
	var session Session
	err := queryer.QueryRow(`SELECT id, status, starting_branch, starting_commit, created_at, updated_at FROM sessions ORDER BY rowid DESC LIMIT 1`).Scan(
		&session.ID, &session.Status, &session.StartingBranch, &session.StartingCommit, &session.CreatedAt, &session.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, invalid("session_not_found", "No Bandmaster session exists in this repository.")
	}
	if err != nil {
		return Session{}, internal("read latest session", err)
	}
	return session, nil
}

func (p *Project) verifySessionBaseline(session Session) *Error {
	branch, commit, projectError := p.cleanBaseline()
	if projectError != nil {
		return projectError
	}
	if branch != session.StartingBranch {
		return invalid("branch_drift", fmt.Sprintf("Current branch %s does not match session branch %s.", branch, session.StartingBranch))
	}
	if commit != session.StartingCommit {
		return invalid("head_drift", fmt.Sprintf("Current commit %s does not match session commit %s.", commit, session.StartingCommit))
	}
	return nil
}

func (p *Project) inspectSession(db *sql.DB, id string) (Session, *Error) {
	query := `SELECT id, status, starting_branch, starting_commit, created_at, updated_at FROM sessions`
	var row *sql.Row
	if id == "" {
		row = db.QueryRow(query + ` ORDER BY rowid DESC LIMIT 1`)
	} else {
		row = db.QueryRow(query+` WHERE id = ?`, id)
	}
	var session Session
	if err := row.Scan(&session.ID, &session.Status, &session.StartingBranch, &session.StartingCommit, &session.CreatedAt, &session.UpdatedAt); errors.Is(err, sql.ErrNoRows) {
		return Session{}, invalid("session_not_found", "No Bandmaster session exists in this repository.")
	} else if err != nil {
		return Session{}, internal("read session", err)
	}

	rows, err := db.Query(`
		SELECT audit.sequence, audit.event, audit.from_status, audit.to_status, audit.occurred_at,
			integrity.violation_id, integrity.kind, integrity.path, integrity.observed_state_json
		FROM audit_events audit
		LEFT JOIN integrity_audit_events integrity ON integrity.audit_sequence = audit.sequence
		WHERE audit.session_id = ? ORDER BY audit.sequence`, session.ID)
	if err != nil {
		return Session{}, sessionInternal(session.ID, "read session audit history", err)
	}
	defer rows.Close()
	for rows.Next() {
		var event AuditEvent
		var fromStatus, integrityKind, integrityPath, observedState sql.NullString
		var violationID sql.NullInt64
		if err := rows.Scan(&event.Sequence, &event.Event, &fromStatus, &event.ToStatus, &event.OccurredAt, &violationID, &integrityKind, &integrityPath, &observedState); err != nil {
			return Session{}, sessionInternal(session.ID, "read session audit event", err)
		}
		event.FromStatus = fromStatus.String
		event.IntegrityViolationID = violationID.Int64
		event.IntegrityKind = integrityKind.String
		event.IntegrityPath = integrityPath.String
		if observedState.Valid {
			event.ObservedState = json.RawMessage(observedState.String)
		}
		session.AuditHistory = append(session.AuditHistory, event)
	}
	if err := rows.Err(); err != nil {
		return Session{}, sessionInternal(session.ID, "read session audit history", err)
	}
	session.IntegrityViolations = []IntegrityViolation{}
	monitor, monitorError := inspectLatestMonitor(db, session.ID)
	if monitorError == nil {
		session.Monitor = &monitor
	} else if monitorError.Code != "monitor_unhealthy" {
		return Session{}, monitorError
	}
	violationRows, err := db.Query(`SELECT id, kind, path, observed_state_json, detected_at, recovered_at, recovery_confirmation FROM integrity_violations WHERE session_id = ? ORDER BY id`, session.ID)
	if err != nil {
		return Session{}, sessionInternal(session.ID, "read integrity violations", err)
	}
	defer violationRows.Close()
	for violationRows.Next() {
		var violation IntegrityViolation
		var observed string
		var recoveredAt, confirmation sql.NullString
		if err := violationRows.Scan(&violation.ID, &violation.Kind, &violation.Path, &observed, &violation.DetectedAt, &recoveredAt, &confirmation); err != nil {
			return Session{}, sessionInternal(session.ID, "read integrity violation", err)
		}
		violation.ObservedState = json.RawMessage(observed)
		violation.RecoveredAt = recoveredAt.String
		violation.RecoveryConfirmation = confirmation.String
		session.IntegrityViolations = append(session.IntegrityViolations, violation)
	}
	if err := violationRows.Err(); err != nil {
		return Session{}, sessionInternal(session.ID, "read integrity violations", err)
	}
	return session, nil
}

func (p *Project) cleanBaseline() (string, string, *Error) {
	branch, err := gitOutput(p.Root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", "", invalid("detached_head", "A Bandmaster session requires an attached Git branch.")
	}
	commit, err := gitOutput(p.Root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", "", invalid("repository_has_no_commits", "A Bandmaster session requires an existing Git commit.")
	}
	if clean, err := gitQuiet(p.Root, "diff", "--cached", "--quiet", "--exit-code"); err != nil {
		return "", "", internal("inspect Git index", err)
	} else if !clean {
		return "", "", invalid("index_not_clean", "A Bandmaster session requires a clean Git index.")
	}
	workingTree, err := gitOutput(p.Root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return "", "", internal("inspect Git working tree", err)
	}
	if workingTree != "" {
		return "", "", invalid("working_tree_not_clean", "A Bandmaster session requires a clean Git working tree.")
	}
	return branch, commit, nil
}

func newSessionID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "session_" + hex.EncodeToString(value), nil
}

func gitQuiet(dir string, args ...string) (bool, error) {
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	err := command.Run()
	if err == nil {
		return true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func invalidSession(sessionID, code, message string) *Error {
	err := invalid(code, message)
	err.SessionID = sessionID
	return err
}

func sessionInternal(sessionID, action string, cause error) *Error {
	err := internal(action, cause)
	err.SessionID = sessionID
	return err
}
