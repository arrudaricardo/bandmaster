package project

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (p *Project) AssignTask(id, agentIdentity string) (Task, *Error) {
	if strings.TrimSpace(agentIdentity) == "" {
		return Task{}, invalid("invalid_agent_identity", "Agent identity must not be empty.")
	}
	db, projectError := p.openState()
	if projectError != nil {
		return Task{}, projectError
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return Task{}, internal("begin task assignment", err)
	}
	defer tx.Rollback()
	session, projectError := inspectActiveSession(tx)
	if projectError != nil {
		return Task{}, projectError
	}
	if projectError := sweepExpiredLeases(tx, session.ID, time.Now().UTC()); projectError != nil {
		return Task{}, projectError
	}
	var status string
	var currentAgent, currentToken sql.NullString
	if err := tx.QueryRow(`SELECT status, agent_identity, assignment_token FROM tasks WHERE session_id = ? AND id = ?`, session.ID, id).Scan(&status, &currentAgent, &currentToken); errors.Is(err, sql.ErrNoRows) {
		return Task{}, invalidSession(session.ID, "task_not_found", fmt.Sprintf("Task %s does not exist in the active session.", id))
	} else if err != nil {
		return Task{}, sessionInternal(session.ID, "read task assignment", err)
	}
	if status == "quarantined" {
		if err := tx.Commit(); err != nil {
			return Task{}, sessionInternal(session.ID, "commit agent lease expiry", err)
		}
		return Task{}, quarantined(session.ID, "lease_expired", fmt.Sprintf("The agent lease for task %s expired and its ownership is quarantined.", id))
	}
	leaseDuration, configDigest, projectError := p.agentLeaseConfiguration()
	if projectError != nil {
		projectError.SessionID = session.ID
		return Task{}, projectError
	}
	var approvedDigest string
	err = tx.QueryRow(`SELECT value FROM metadata WHERE key = 'approved_configuration_digest'`).Scan(&approvedDigest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Task{}, sessionInternal(session.ID, "read configuration approval", err)
	}
	if err != nil || approvedDigest != configDigest {
		return Task{}, invalidSession(session.ID, "configuration_not_approved", fmt.Sprintf("Validation configuration %s is not approved.", configDigest))
	}
	if (status == "assigned" || status == "editing") && currentAgent.String == agentIdentity && currentToken.String != "" {
		if err := tx.Rollback(); err != nil {
			return Task{}, sessionInternal(session.ID, "close idempotent task assignment", err)
		}
		return inspectTask(db, session.ID, id)
	}
	if status == "planned" {
		prerequisites, projectError := taskPrerequisites(tx, session.ID, id)
		if projectError != nil {
			return Task{}, projectError
		}
		readiness, projectError := taskReadiness(tx, prerequisites)
		if projectError != nil {
			return Task{}, projectError
		}
		if readiness == "ready" {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := tx.Exec(`UPDATE tasks SET status = 'ready', updated_at = ? WHERE id = ? AND status = 'planned'`, now, id); err != nil {
				return Task{}, sessionInternal(session.ID, "mark task ready", err)
			}
			if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'task_ready', 'planned', 'ready', ?)`, session.ID, id, now); err != nil {
				return Task{}, sessionInternal(session.ID, "record task readiness", err)
			}
			status = "ready"
		}
	}
	if status == "planned" {
		return Task{}, blocked(session.ID, "task_not_ready", fmt.Sprintf("Task %s has prerequisites that have not succeeded in a prior batch.", id))
	}
	if status == "ready" {
		var repairBatchID string
		err := tx.QueryRow(`SELECT id FROM batches WHERE session_id = ? AND status IN ('repair_pending', 'repairing') ORDER BY creation_order LIMIT 1`, session.ID).Scan(&repairBatchID)
		if err == nil {
			return Task{}, blocked(session.ID, "batch_repair_in_progress", fmt.Sprintf("Batch %s must complete repair before unrelated agents can be assigned.", repairBatchID))
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Task{}, sessionInternal(session.ID, "inspect batch repair before assignment", err)
		}
	}
	if status != "ready" && status != "repair_pending" {
		return Task{}, invalidSession(session.ID, "task_not_assignable", fmt.Sprintf("Task %s cannot be assigned from %s state.", id, status))
	}
	toStatus := "assigned"
	if status == "repair_pending" {
		var batchTaskCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM batch_tasks WHERE task_id = ?`, id).Scan(&batchTaskCount); err != nil {
			return Task{}, sessionInternal(session.ID, "inspect retained replacement ownership", err)
		}
		if batchTaskCount == 0 {
			return Task{}, sessionInternal(session.ID, "assign replacement Agent", errors.New("repair-pending Task has no retained Batch Task record"))
		}
		if projectError := validateTaskBatchStatus(tx, session.ID, id, "collecting", "repairing"); projectError != nil {
			return Task{}, projectError
		}
		toStatus = "editing"
	}
	token, err := newAssignmentToken()
	if err != nil {
		return Task{}, sessionInternal(session.ID, "generate assignment token", err)
	}
	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE tasks SET status = ?, agent_identity = ?, assignment_token = ?, updated_at = ? WHERE id = ? AND status = ?`, toStatus, agentIdentity, token, now, id, status); err != nil {
		return Task{}, sessionInternal(session.ID, "assign task", err)
	}
	if _, err := tx.Exec(`INSERT INTO task_leases(task_id, status, duration_nanos, renewed_at, expires_at) VALUES(?, 'active', ?, ?, ?) ON CONFLICT(task_id) DO UPDATE SET status = 'active', duration_nanos = excluded.duration_nanos, renewed_at = excluded.renewed_at, expires_at = excluded.expires_at`, id, leaseDuration.Nanoseconds(), now, nowTime.Add(leaseDuration).Format(time.RFC3339Nano)); err != nil {
		return Task{}, sessionInternal(session.ID, "create agent lease", err)
	}
	if status == "repair_pending" {
		if _, err := tx.Exec(`
			UPDATE task_recovery_events SET replacement_assignment_token = ?
			WHERE task_audit_sequence = (
				SELECT MAX(audit.sequence) FROM task_audit_events audit
				WHERE audit.task_id = ? AND audit.to_status = 'repair_pending'
			)`, token, id); err != nil {
			return Task{}, sessionInternal(session.ID, "link replacement token to recovery", err)
		}
		if _, err := tx.Exec(`
			UPDATE task_repair_events SET replacement_assignment_token = ?
			WHERE task_audit_sequence = (
				SELECT MAX(audit.sequence) FROM task_audit_events audit
				WHERE audit.task_id = ? AND audit.to_status = 'repair_pending'
			)`, token, id); err != nil {
			return Task{}, sessionInternal(session.ID, "link replacement token to repair", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, agent_identity, occurred_at) VALUES(?, ?, 'task_assigned', ?, ?, ?, ?)`, session.ID, id, status, toStatus, agentIdentity, now); err != nil {
		return Task{}, sessionInternal(session.ID, "record task assignment", err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, sessionInternal(session.ID, "commit task assignment", err)
	}
	return inspectTask(db, session.ID, id)
}

func (p *Project) RequeueTask(id string) (Task, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return Task{}, projectError
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return Task{}, internal("begin task requeue", err)
	}
	defer tx.Rollback()
	session, projectError := inspectActiveSession(tx)
	if projectError != nil {
		return Task{}, projectError
	}
	var status string
	var claimCount int
	if err := tx.QueryRow(`SELECT status, (SELECT COUNT(*) FROM claims WHERE task_id = tasks.id) FROM tasks WHERE session_id = ? AND id = ?`, session.ID, id).Scan(&status, &claimCount); errors.Is(err, sql.ErrNoRows) {
		return Task{}, invalidSession(session.ID, "task_not_found", fmt.Sprintf("Task %s does not exist in the active session.", id))
	} else if err != nil {
		return Task{}, sessionInternal(session.ID, "read blocked task", err)
	}
	if status != "blocked" {
		return Task{}, invalidSession(session.ID, "task_not_requeueable", fmt.Sprintf("Task %s cannot be requeued from %s state.", id, status))
	}
	if claimCount != 0 {
		return Task{}, sessionInternal(session.ID, "requeue blocked task", fmt.Errorf("blocked task retained %d claims", claimCount))
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE tasks SET status = 'ready', agent_identity = NULL, assignment_token = NULL, updated_at = ? WHERE id = ? AND status = 'blocked'`, now, id); err != nil {
		return Task{}, sessionInternal(session.ID, "requeue blocked task", err)
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'task_requeued', 'blocked', 'ready', ?)`, session.ID, id, now); err != nil {
		return Task{}, sessionInternal(session.ID, "record task requeue", err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, sessionInternal(session.ID, "commit task requeue", err)
	}
	return inspectTask(db, session.ID, id)
}

func (p *Project) RecoverTask(id string, request RepairRequest) (Task, *Error) {
	if strings.TrimSpace(request.UserConfirmation) != "" && (strings.TrimSpace(request.TerminatedAgent) != "" || strings.TrimSpace(request.TerminationProof) != "") {
		return Task{}, invalid("conflicting_recovery_evidence", "Use either agent-handle termination evidence or explicit user confirmation, not both.")
	}
	db, projectError := p.openState()
	if projectError != nil {
		return Task{}, projectError
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return Task{}, internal("begin task recovery", err)
	}
	defer tx.Rollback()
	session, projectError := inspectActiveSession(tx)
	if projectError != nil {
		return Task{}, projectError
	}
	if projectError := sweepExpiredLeases(tx, session.ID, time.Now().UTC()); projectError != nil {
		return Task{}, projectError
	}
	var status string
	var agentIdentity sql.NullString
	var batchTaskCount int
	if err := tx.QueryRow(`SELECT status, agent_identity, (SELECT COUNT(*) FROM batch_tasks WHERE task_id = tasks.id) FROM tasks WHERE session_id = ? AND id = ?`, session.ID, id).Scan(&status, &agentIdentity, &batchTaskCount); errors.Is(err, sql.ErrNoRows) {
		return Task{}, invalidSession(session.ID, "task_not_found", fmt.Sprintf("Task %s does not exist in the active session.", id))
	} else if err != nil {
		return Task{}, sessionInternal(session.ID, "read quarantined task", err)
	}
	if status != "quarantined" {
		return Task{}, invalidSession(session.ID, "task_not_recoverable", fmt.Sprintf("Task %s cannot be recovered from %s state.", id, status))
	}
	recoveryMethod := "agent_handle"
	if strings.TrimSpace(request.UserConfirmation) != "" {
		recoveryMethod = "user_confirmation"
	} else if request.TerminatedAgent == "" {
		return Task{}, invalidSession(session.ID, "agent_termination_required", fmt.Sprintf("Recovering quarantined task %s requires the terminated agent identity %s or explicit user confirmation.", id, agentIdentity.String))
	} else if request.TerminatedAgent != agentIdentity.String {
		return Task{}, invalidSession(session.ID, "agent_termination_mismatch", fmt.Sprintf("Task %s is assigned to agent %s, not %s.", id, agentIdentity.String, request.TerminatedAgent))
	} else if strings.TrimSpace(request.TerminationProof) == "" {
		return Task{}, invalidSession(session.ID, "agent_termination_proof_required", fmt.Sprintf("Recovering quarantined task %s requires evidence from the parent-held agent handle that %s stopped.", id, agentIdentity.String))
	}
	toStatus := "ready"
	var repairSnapshots []capturedSnapshot
	if batchTaskCount != 0 {
		if strings.TrimSpace(request.Diagnosis) == "" || strings.TrimSpace(request.IntendedRepair) == "" {
			return Task{}, invalidSession(session.ID, "invalid_repair_handoff", "Recovering claimed work requires a non-empty diagnosis and intended repair.")
		}
		claims, projectError := loadStoredClaims(tx, session.ID, id)
		if projectError != nil {
			return Task{}, projectError
		}
		repairSnapshots, projectError = p.captureRepairSnapshots(session.ID, claims)
		if projectError != nil {
			return Task{}, projectError
		}
		verifiedSnapshots, projectError := p.captureRepairSnapshots(session.ID, claims)
		if projectError != nil {
			return Task{}, projectError
		}
		for index := range repairSnapshots {
			if !snapshotsEqual(repairSnapshots[index].PathSnapshot, verifiedSnapshots[index].PathSnapshot) {
				return Task{}, invalidSession(session.ID, "repair_snapshot_changed", fmt.Sprintf("Claimed path %s changed while its recovery snapshot was being recorded.", repairSnapshots[index].Path))
			}
		}
		repairSnapshots = verifiedSnapshots
		toStatus = "repair_pending"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE tasks SET status = ?, agent_identity = NULL, assignment_token = NULL, updated_at = ? WHERE id = ? AND status = 'quarantined'`, toStatus, now, id); err != nil {
		return Task{}, sessionInternal(session.ID, "recover quarantined task", err)
	}
	auditResult, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, agent_identity, termination_proof, occurred_at) VALUES(?, ?, 'task_recovered', 'quarantined', ?, ?, ?, ?)`, session.ID, id, toStatus, agentIdentity.String, nullableString(request.TerminationProof), now)
	if err != nil {
		return Task{}, sessionInternal(session.ID, "record task recovery", err)
	}
	auditSequence, err := auditResult.LastInsertId()
	if err != nil {
		return Task{}, sessionInternal(session.ID, "read task recovery audit identity", err)
	}
	if _, err := tx.Exec(`INSERT INTO task_recovery_events(task_audit_sequence, recovery_method, user_confirmation) VALUES(?, ?, ?)`, auditSequence, recoveryMethod, nullableString(request.UserConfirmation)); err != nil {
		return Task{}, sessionInternal(session.ID, "record task recovery evidence", err)
	}
	if batchTaskCount != 0 {
		if projectError := persistTaskRepairDetails(tx, session.ID, id, auditSequence, request, recoveryMethod, nil, repairSnapshots, nil); projectError != nil {
			return Task{}, projectError
		}
	}
	if err := tx.Commit(); err != nil {
		return Task{}, sessionInternal(session.ID, "commit task recovery", err)
	}
	return inspectTask(db, session.ID, id)
}

func (p *Project) ReplanTask(id string, plan TaskPlan, terminatedAgent, terminationProof string) (Task, *Error) {
	if projectError := validateTaskPlan(plan); projectError != nil {
		return Task{}, projectError
	}
	db, projectError := p.openState()
	if projectError != nil {
		return Task{}, projectError
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return Task{}, internal("begin task replanning", err)
	}
	defer tx.Rollback()
	session, projectError := inspectActiveSession(tx)
	if projectError != nil {
		return Task{}, projectError
	}
	var status string
	var agentIdentity sql.NullString
	var coreFrozen int
	if err := tx.QueryRow(`SELECT status, agent_identity, core_frozen FROM tasks WHERE session_id = ? AND id = ?`, session.ID, id).Scan(&status, &agentIdentity, &coreFrozen); errors.Is(err, sql.ErrNoRows) {
		return Task{}, invalidSession(session.ID, "task_not_found", fmt.Sprintf("Task %s does not exist in the active session.", id))
	} else if err != nil {
		return Task{}, sessionInternal(session.ID, "read task plan", err)
	}
	if coreFrozen != 0 {
		return Task{}, invalidSession(session.ID, "task_core_frozen", fmt.Sprintf("Task %s planning fields are permanently frozen because its first claim was acquired.", id))
	}
	if status != "planned" && status != "ready" && status != "blocked" && status != "assigned" {
		return Task{}, invalidSession(session.ID, "task_not_replannable", fmt.Sprintf("Task %s cannot be replanned from %s state.", id, status))
	}
	if projectError := validateAgentTermination(session.ID, id, status, agentIdentity.String, terminatedAgent, terminationProof, "Replanning"); projectError != nil {
		return Task{}, projectError
	}
	if projectError := validatePrerequisites(tx, session.ID, id, plan.Prerequisites); projectError != nil {
		projectError.SessionID = session.ID
		return Task{}, projectError
	}
	if projectError := validateDependencyCycles(tx, session.ID, id, plan.Prerequisites); projectError != nil {
		return Task{}, projectError
	}
	newStatus, projectError := taskReadiness(tx, plan.Prerequisites)
	if projectError != nil {
		projectError.SessionID = session.ID
		return Task{}, projectError
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE tasks SET title = ?, intent = ?, expected_outcome = ?, status = ?, agent_identity = NULL, assignment_token = NULL, updated_at = ? WHERE id = ?`, plan.Title, plan.Intent, plan.ExpectedOutcome, newStatus, now, id); err != nil {
		return Task{}, sessionInternal(session.ID, "update task plan", err)
	}
	if status == "assigned" {
		if _, err := tx.Exec(`UPDATE task_leases SET status = 'closed' WHERE task_id = ?`, id); err != nil {
			return Task{}, sessionInternal(session.ID, "close replanned agent lease", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM task_dependencies WHERE task_id = ?`, id); err != nil {
		return Task{}, sessionInternal(session.ID, "replace task prerequisites", err)
	}
	for index, prerequisite := range plan.Prerequisites {
		if _, err := tx.Exec(`INSERT INTO task_dependencies(task_id, prerequisite_id, dependency_order) VALUES(?, ?, ?)`, id, prerequisite, index+1); err != nil {
			return Task{}, sessionInternal(session.ID, "record replanned task prerequisite", err)
		}
	}
	var auditAgent any
	var auditTerminationProof any
	if status == "assigned" {
		auditAgent = agentIdentity.String
		auditTerminationProof = terminationProof
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, agent_identity, termination_proof, occurred_at) VALUES(?, ?, 'task_replanned', ?, ?, ?, ?, ?)`, session.ID, id, status, newStatus, auditAgent, auditTerminationProof, now); err != nil {
		return Task{}, sessionInternal(session.ID, "record task replanning", err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, sessionInternal(session.ID, "commit task replanning", err)
	}
	return inspectTask(db, session.ID, id)
}

func (p *Project) CancelTask(id, terminatedAgent, terminationProof string) (Task, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return Task{}, projectError
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return Task{}, internal("begin task cancellation", err)
	}
	defer tx.Rollback()
	session, projectError := inspectActiveSession(tx)
	if projectError != nil {
		return Task{}, projectError
	}
	var status string
	var agentIdentity sql.NullString
	var coreFrozen int
	if err := tx.QueryRow(`SELECT status, agent_identity, core_frozen FROM tasks WHERE session_id = ? AND id = ?`, session.ID, id).Scan(&status, &agentIdentity, &coreFrozen); errors.Is(err, sql.ErrNoRows) {
		return Task{}, invalidSession(session.ID, "task_not_found", fmt.Sprintf("Task %s does not exist in the active session.", id))
	} else if err != nil {
		return Task{}, sessionInternal(session.ID, "read task cancellation state", err)
	}
	if status == "canceled" {
		if err := tx.Rollback(); err != nil {
			return Task{}, sessionInternal(session.ID, "close idempotent task cancellation", err)
		}
		return inspectTask(db, session.ID, id)
	}
	if coreFrozen != 0 || (status != "planned" && status != "ready" && status != "blocked" && status != "assigned") {
		return Task{}, invalidSession(session.ID, "task_not_cancelable", fmt.Sprintf("Task %s cannot be canceled from %s state after material work has started.", id, status))
	}
	var dependentID string
	err = tx.QueryRow(`
		SELECT dependent.id
		FROM task_dependencies dependency
		JOIN tasks dependent ON dependent.id = dependency.task_id
		WHERE dependency.prerequisite_id = ? AND dependent.status != 'canceled'
		ORDER BY dependent.creation_order
		LIMIT 1`, id).Scan(&dependentID)
	if err == nil {
		return Task{}, invalidSession(session.ID, "task_has_dependents", fmt.Sprintf("Task %s cannot be canceled while dependent task %s still references it; cancel or replan the dependent first.", id, dependentID))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Task{}, sessionInternal(session.ID, "inspect task dependents", err)
	}
	if projectError := validateAgentTermination(session.ID, id, status, agentIdentity.String, terminatedAgent, terminationProof, "Canceling"); projectError != nil {
		return Task{}, projectError
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE tasks SET status = 'canceled', agent_identity = NULL, assignment_token = NULL, updated_at = ? WHERE id = ?`, now, id); err != nil {
		return Task{}, sessionInternal(session.ID, "cancel task", err)
	}
	if status == "assigned" {
		if _, err := tx.Exec(`UPDATE task_leases SET status = 'closed' WHERE task_id = ?`, id); err != nil {
			return Task{}, sessionInternal(session.ID, "close canceled agent lease", err)
		}
	}
	var auditAgent any
	var auditTerminationProof any
	if status == "assigned" {
		auditAgent = agentIdentity.String
		auditTerminationProof = terminationProof
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, agent_identity, termination_proof, occurred_at) VALUES(?, ?, 'task_canceled', ?, 'canceled', ?, ?, ?)`, session.ID, id, status, auditAgent, auditTerminationProof, now); err != nil {
		return Task{}, sessionInternal(session.ID, "record task cancellation", err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, sessionInternal(session.ID, "commit task cancellation", err)
	}
	return inspectTask(db, session.ID, id)
}

func validateAgentTermination(sessionID, taskID, status, assignedAgent, terminatedAgent, terminationProof, action string) *Error {
	if status != "assigned" {
		return nil
	}
	if terminatedAgent == "" {
		return invalidSession(sessionID, "agent_termination_required", fmt.Sprintf("%s assigned task %s requires the terminated agent identity %s.", action, taskID, assignedAgent))
	}
	if terminatedAgent != assignedAgent {
		return invalidSession(sessionID, "agent_termination_mismatch", fmt.Sprintf("Task %s is assigned to agent %s, not %s.", taskID, assignedAgent, terminatedAgent))
	}
	if strings.TrimSpace(terminationProof) == "" {
		return invalidSession(sessionID, "agent_termination_proof_required", fmt.Sprintf("%s assigned task %s requires evidence from the parent-held agent handle that %s stopped.", action, taskID, assignedAgent))
	}
	return nil
}
