package project

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Task struct {
	SessionID         string              `json:"-"`
	ID                string              `json:"id"`
	CreationOrder     int64               `json:"creation_order"`
	Title             string              `json:"title"`
	Intent            string              `json:"intent"`
	ExpectedOutcome   string              `json:"expected_outcome"`
	Prerequisites     []string            `json:"prerequisites"`
	Status            string              `json:"status"`
	WorkerIdentity    string              `json:"worker_identity,omitempty"`
	AssignmentToken   string              `json:"assignment_token,omitempty"`
	CoreFrozen        bool                `json:"core_frozen"`
	BatchID           string              `json:"batch_id,omitempty"`
	CommitSHA         string              `json:"commit_sha,omitempty"`
	Lease             *WorkerLease        `json:"lease,omitempty"`
	Claims            []Claim             `json:"claims"`
	FocusedValidation []FocusedValidation `json:"focused_validation"`
	Submission        *Submission         `json:"submission,omitempty"`
	CreatedAt         string              `json:"created_at"`
	UpdatedAt         string              `json:"updated_at"`
	AuditHistory      []TaskAuditEvent    `json:"audit_history"`
}

type TaskAuditEvent struct {
	Sequence         int64            `json:"sequence"`
	Event            string           `json:"event"`
	FromStatus       string           `json:"from_status,omitempty"`
	ToStatus         string           `json:"to_status"`
	WorkerIdentity   string           `json:"worker_identity,omitempty"`
	TerminationProof string           `json:"termination_proof,omitempty"`
	RecoveryMethod   string           `json:"recovery_method,omitempty"`
	UserConfirmation string           `json:"user_confirmation,omitempty"`
	ReplacementToken string           `json:"replacement_assignment_token,omitempty"`
	Diagnosis        string           `json:"diagnosis,omitempty"`
	IntendedRepair   string           `json:"intended_repair,omitempty"`
	Invalidated      *Submission      `json:"invalidated_submission,omitempty"`
	RepairSnapshots  []RepairSnapshot `json:"repair_snapshots,omitempty"`
	OccurredAt       string           `json:"occurred_at"`
}

type RepairSnapshot struct {
	Path                 string        `json:"path"`
	Snapshot             PathSnapshot  `json:"snapshot"`
	InvalidatedSubmitted *PathSnapshot `json:"invalidated_submitted_snapshot,omitempty"`
}

type TaskPlan struct {
	Title           string
	Intent          string
	ExpectedOutcome string
	Prerequisites   []string
}

type TaskList struct {
	Tasks []Task `json:"tasks"`
}

func (p *Project) CreateTask(plan TaskPlan) (Task, *Error) {
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
		return Task{}, internal("begin task creation", err)
	}
	defer tx.Rollback()
	session, projectError := inspectActiveSession(tx)
	if projectError != nil {
		return Task{}, projectError
	}
	if projectError := validatePrerequisites(tx, session.ID, "", plan.Prerequisites); projectError != nil {
		projectError.SessionID = session.ID
		return Task{}, projectError
	}

	id, err := newTaskID()
	if err != nil {
		return Task{}, sessionInternal(session.ID, "generate task identity", err)
	}
	var creationOrder int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(creation_order), 0) + 1 FROM tasks WHERE session_id = ?`, session.ID).Scan(&creationOrder); err != nil {
		return Task{}, sessionInternal(session.ID, "choose task creation order", err)
	}
	status, projectError := taskReadiness(tx, plan.Prerequisites)
	if projectError != nil {
		projectError.SessionID = session.ID
		return Task{}, projectError
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO tasks(id, session_id, creation_order, title, intent, expected_outcome, status, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, session.ID, creationOrder, plan.Title, plan.Intent, plan.ExpectedOutcome, status, now, now); err != nil {
		return Task{}, sessionInternal(session.ID, "create task", err)
	}
	for index, prerequisite := range plan.Prerequisites {
		if _, err := tx.Exec(`INSERT INTO task_dependencies(task_id, prerequisite_id, dependency_order) VALUES(?, ?, ?)`, id, prerequisite, index+1); err != nil {
			return Task{}, sessionInternal(session.ID, "record task prerequisite", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, to_status, occurred_at) VALUES(?, ?, 'task_created', ?, ?)`, session.ID, id, status, now); err != nil {
		return Task{}, sessionInternal(session.ID, "record task creation", err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, sessionInternal(session.ID, "commit task creation", err)
	}
	return inspectTask(db, session.ID, id)
}

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

func (p *Project) AssignTask(id, workerIdentity string) (Task, *Error) {
	if strings.TrimSpace(workerIdentity) == "" {
		return Task{}, invalid("invalid_worker_identity", "Worker identity must not be empty.")
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
	var currentWorker, currentToken sql.NullString
	if err := tx.QueryRow(`SELECT status, worker_identity, assignment_token FROM tasks WHERE session_id = ? AND id = ?`, session.ID, id).Scan(&status, &currentWorker, &currentToken); errors.Is(err, sql.ErrNoRows) {
		return Task{}, invalidSession(session.ID, "task_not_found", fmt.Sprintf("Task %s does not exist in the active session.", id))
	} else if err != nil {
		return Task{}, sessionInternal(session.ID, "read task assignment", err)
	}
	if status == "quarantined" {
		if err := tx.Commit(); err != nil {
			return Task{}, sessionInternal(session.ID, "commit worker lease expiry", err)
		}
		return Task{}, quarantined(session.ID, "lease_expired", fmt.Sprintf("The worker lease for task %s expired and its ownership is quarantined.", id))
	}
	leaseDuration, configDigest, projectError := p.workerLeaseConfiguration()
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
	if (status == "assigned" || status == "editing") && currentWorker.String == workerIdentity && currentToken.String != "" {
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
			return Task{}, blocked(session.ID, "batch_repair_in_progress", fmt.Sprintf("Batch %s must complete repair before unrelated workers can be assigned.", repairBatchID))
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
		var batchMembershipCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM batch_members WHERE task_id = ?`, id).Scan(&batchMembershipCount); err != nil {
			return Task{}, sessionInternal(session.ID, "inspect retained replacement ownership", err)
		}
		if batchMembershipCount == 0 {
			return Task{}, sessionInternal(session.ID, "assign replacement worker", errors.New("repair-pending task has no retained batch membership"))
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
	if _, err := tx.Exec(`UPDATE tasks SET status = ?, worker_identity = ?, assignment_token = ?, updated_at = ? WHERE id = ? AND status = ?`, toStatus, workerIdentity, token, now, id, status); err != nil {
		return Task{}, sessionInternal(session.ID, "assign task", err)
	}
	if _, err := tx.Exec(`INSERT INTO task_leases(task_id, status, duration_nanos, renewed_at, expires_at) VALUES(?, 'active', ?, ?, ?) ON CONFLICT(task_id) DO UPDATE SET status = 'active', duration_nanos = excluded.duration_nanos, renewed_at = excluded.renewed_at, expires_at = excluded.expires_at`, id, leaseDuration.Nanoseconds(), now, nowTime.Add(leaseDuration).Format(time.RFC3339Nano)); err != nil {
		return Task{}, sessionInternal(session.ID, "create worker lease", err)
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
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, worker_identity, occurred_at) VALUES(?, ?, 'task_assigned', ?, ?, ?, ?)`, session.ID, id, status, toStatus, workerIdentity, now); err != nil {
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
	if _, err := tx.Exec(`UPDATE tasks SET status = 'ready', worker_identity = NULL, assignment_token = NULL, updated_at = ? WHERE id = ? AND status = 'blocked'`, now, id); err != nil {
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
	var workerIdentity sql.NullString
	var batchMembershipCount int
	if err := tx.QueryRow(`SELECT status, worker_identity, (SELECT COUNT(*) FROM batch_members WHERE task_id = tasks.id) FROM tasks WHERE session_id = ? AND id = ?`, session.ID, id).Scan(&status, &workerIdentity, &batchMembershipCount); errors.Is(err, sql.ErrNoRows) {
		return Task{}, invalidSession(session.ID, "task_not_found", fmt.Sprintf("Task %s does not exist in the active session.", id))
	} else if err != nil {
		return Task{}, sessionInternal(session.ID, "read quarantined task", err)
	}
	if status != "quarantined" {
		return Task{}, invalidSession(session.ID, "task_not_recoverable", fmt.Sprintf("Task %s cannot be recovered from %s state.", id, status))
	}
	recoveryMethod := "worker_handle"
	if strings.TrimSpace(request.UserConfirmation) != "" {
		recoveryMethod = "user_confirmation"
	} else if request.TerminatedWorker == "" {
		return Task{}, invalidSession(session.ID, "worker_termination_required", fmt.Sprintf("Recovering quarantined task %s requires the terminated worker identity %s or explicit user confirmation.", id, workerIdentity.String))
	} else if request.TerminatedWorker != workerIdentity.String {
		return Task{}, invalidSession(session.ID, "worker_termination_mismatch", fmt.Sprintf("Task %s is assigned to worker %s, not %s.", id, workerIdentity.String, request.TerminatedWorker))
	} else if strings.TrimSpace(request.TerminationProof) == "" {
		return Task{}, invalidSession(session.ID, "worker_termination_proof_required", fmt.Sprintf("Recovering quarantined task %s requires evidence from the parent-held worker handle that %s stopped.", id, workerIdentity.String))
	}
	toStatus := "ready"
	var repairSnapshots []capturedSnapshot
	if batchMembershipCount != 0 {
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
	if _, err := tx.Exec(`UPDATE tasks SET status = ?, worker_identity = NULL, assignment_token = NULL, updated_at = ? WHERE id = ? AND status = 'quarantined'`, toStatus, now, id); err != nil {
		return Task{}, sessionInternal(session.ID, "recover quarantined task", err)
	}
	auditResult, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, worker_identity, termination_proof, occurred_at) VALUES(?, ?, 'task_recovered', 'quarantined', ?, ?, ?, ?)`, session.ID, id, toStatus, workerIdentity.String, nullableString(request.TerminationProof), now)
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
	if batchMembershipCount != 0 {
		if projectError := persistTaskRepairDetails(tx, session.ID, id, auditSequence, request, recoveryMethod, nil, repairSnapshots, nil); projectError != nil {
			return Task{}, projectError
		}
	}
	if err := tx.Commit(); err != nil {
		return Task{}, sessionInternal(session.ID, "commit task recovery", err)
	}
	return inspectTask(db, session.ID, id)
}

func (p *Project) ReplanTask(id string, plan TaskPlan, terminatedWorker, terminationProof string) (Task, *Error) {
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
	var workerIdentity sql.NullString
	var coreFrozen int
	if err := tx.QueryRow(`SELECT status, worker_identity, core_frozen FROM tasks WHERE session_id = ? AND id = ?`, session.ID, id).Scan(&status, &workerIdentity, &coreFrozen); errors.Is(err, sql.ErrNoRows) {
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
	if projectError := validateWorkerTermination(session.ID, id, status, workerIdentity.String, terminatedWorker, terminationProof, "Replanning"); projectError != nil {
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
	if _, err := tx.Exec(`UPDATE tasks SET title = ?, intent = ?, expected_outcome = ?, status = ?, worker_identity = NULL, assignment_token = NULL, updated_at = ? WHERE id = ?`, plan.Title, plan.Intent, plan.ExpectedOutcome, newStatus, now, id); err != nil {
		return Task{}, sessionInternal(session.ID, "update task plan", err)
	}
	if status == "assigned" {
		if _, err := tx.Exec(`UPDATE task_leases SET status = 'closed' WHERE task_id = ?`, id); err != nil {
			return Task{}, sessionInternal(session.ID, "close replanned worker lease", err)
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
	var auditWorker any
	var auditTerminationProof any
	if status == "assigned" {
		auditWorker = workerIdentity.String
		auditTerminationProof = terminationProof
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, worker_identity, termination_proof, occurred_at) VALUES(?, ?, 'task_replanned', ?, ?, ?, ?, ?)`, session.ID, id, status, newStatus, auditWorker, auditTerminationProof, now); err != nil {
		return Task{}, sessionInternal(session.ID, "record task replanning", err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, sessionInternal(session.ID, "commit task replanning", err)
	}
	return inspectTask(db, session.ID, id)
}

func (p *Project) CancelTask(id, terminatedWorker, terminationProof string) (Task, *Error) {
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
	var workerIdentity sql.NullString
	var coreFrozen int
	if err := tx.QueryRow(`SELECT status, worker_identity, core_frozen FROM tasks WHERE session_id = ? AND id = ?`, session.ID, id).Scan(&status, &workerIdentity, &coreFrozen); errors.Is(err, sql.ErrNoRows) {
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
	if projectError := validateWorkerTermination(session.ID, id, status, workerIdentity.String, terminatedWorker, terminationProof, "Canceling"); projectError != nil {
		return Task{}, projectError
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE tasks SET status = 'canceled', worker_identity = NULL, assignment_token = NULL, updated_at = ? WHERE id = ?`, now, id); err != nil {
		return Task{}, sessionInternal(session.ID, "cancel task", err)
	}
	if status == "assigned" {
		if _, err := tx.Exec(`UPDATE task_leases SET status = 'closed' WHERE task_id = ?`, id); err != nil {
			return Task{}, sessionInternal(session.ID, "close canceled worker lease", err)
		}
	}
	var auditWorker any
	var auditTerminationProof any
	if status == "assigned" {
		auditWorker = workerIdentity.String
		auditTerminationProof = terminationProof
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, worker_identity, termination_proof, occurred_at) VALUES(?, ?, 'task_canceled', ?, 'canceled', ?, ?, ?)`, session.ID, id, status, auditWorker, auditTerminationProof, now); err != nil {
		return Task{}, sessionInternal(session.ID, "record task cancellation", err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, sessionInternal(session.ID, "commit task cancellation", err)
	}
	return inspectTask(db, session.ID, id)
}

func validateWorkerTermination(sessionID, taskID, status, assignedWorker, terminatedWorker, terminationProof, action string) *Error {
	if status != "assigned" {
		return nil
	}
	if terminatedWorker == "" {
		return invalidSession(sessionID, "worker_termination_required", fmt.Sprintf("%s assigned task %s requires the terminated worker identity %s.", action, taskID, assignedWorker))
	}
	if terminatedWorker != assignedWorker {
		return invalidSession(sessionID, "worker_termination_mismatch", fmt.Sprintf("Task %s is assigned to worker %s, not %s.", taskID, assignedWorker, terminatedWorker))
	}
	if strings.TrimSpace(terminationProof) == "" {
		return invalidSession(sessionID, "worker_termination_proof_required", fmt.Sprintf("%s assigned task %s requires evidence from the parent-held worker handle that %s stopped.", action, taskID, assignedWorker))
	}
	return nil
}

func validateTaskPlan(plan TaskPlan) *Error {
	for name, value := range map[string]string{"title": plan.Title, "intent": plan.Intent, "expected outcome": plan.ExpectedOutcome} {
		if strings.TrimSpace(value) == "" {
			return invalid("invalid_task_plan", fmt.Sprintf("Task %s must not be empty.", name))
		}
	}
	seen := make(map[string]struct{}, len(plan.Prerequisites))
	for _, prerequisite := range plan.Prerequisites {
		if strings.TrimSpace(prerequisite) == "" {
			return invalid("invalid_task_plan", "Task prerequisite identities must not be empty.")
		}
		if _, exists := seen[prerequisite]; exists {
			return invalid("duplicate_task_prerequisite", fmt.Sprintf("Task prerequisite %s is duplicated.", prerequisite))
		}
		seen[prerequisite] = struct{}{}
	}
	return nil
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

func validatePrerequisites(queryer rowQuerier, sessionID, taskID string, prerequisites []string) *Error {
	for _, prerequisite := range prerequisites {
		if prerequisite == taskID {
			return invalid("task_dependency_cycle", "A task cannot depend on itself.")
		}
		var foundSession string
		if err := queryer.QueryRow(`SELECT session_id FROM tasks WHERE id = ?`, prerequisite).Scan(&foundSession); errors.Is(err, sql.ErrNoRows) {
			return invalid("task_not_found", fmt.Sprintf("Prerequisite task %s does not exist.", prerequisite))
		} else if err != nil {
			return internal("validate task prerequisite", err)
		}
		if foundSession != sessionID {
			return invalid("task_not_found", fmt.Sprintf("Prerequisite task %s does not belong to the active session.", prerequisite))
		}
	}
	return nil
}

func validateDependencyCycles(queryer rowQuerier, sessionID, taskID string, prerequisites []string) *Error {
	for _, prerequisite := range prerequisites {
		var cycle int
		err := queryer.QueryRow(`
			WITH RECURSIVE ancestors(id) AS (
				SELECT prerequisite_id FROM task_dependencies WHERE task_id = ?
				UNION
				SELECT dependency.prerequisite_id
				FROM task_dependencies dependency
				JOIN ancestors ON dependency.task_id = ancestors.id
			)
			SELECT 1 FROM ancestors WHERE id = ? LIMIT 1`, prerequisite, taskID).Scan(&cycle)
		if err == nil {
			return invalidSession(sessionID, "task_dependency_cycle", fmt.Sprintf("Making task %s depend on %s would create a dependency cycle.", taskID, prerequisite))
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return sessionInternal(sessionID, "validate task dependency graph", err)
		}
	}
	return nil
}

func taskReadiness(queryer rowQuerier, prerequisites []string) (string, *Error) {
	for _, prerequisite := range prerequisites {
		var status string
		if err := queryer.QueryRow(`SELECT status FROM tasks WHERE id = ?`, prerequisite).Scan(&status); err != nil {
			return "", internal("read prerequisite status", err)
		}
		if status != "committed" && status != "no_op" {
			return "planned", nil
		}
	}
	return "ready", nil
}

func taskPrerequisites(db *sql.Tx, sessionID, id string) ([]string, *Error) {
	rows, err := db.Query(`SELECT prerequisite_id FROM task_dependencies WHERE task_id = ? ORDER BY dependency_order`, id)
	if err != nil {
		return nil, sessionInternal(sessionID, "read task prerequisites", err)
	}
	defer rows.Close()
	var prerequisites []string
	for rows.Next() {
		var prerequisite string
		if err := rows.Scan(&prerequisite); err != nil {
			return nil, sessionInternal(sessionID, "read task prerequisite", err)
		}
		prerequisites = append(prerequisites, prerequisite)
	}
	if err := rows.Err(); err != nil {
		return nil, sessionInternal(sessionID, "read task prerequisites", err)
	}
	return prerequisites, nil
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

func newTaskID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "task_" + hex.EncodeToString(value), nil
}

func newAssignmentToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "assignment_" + hex.EncodeToString(value), nil
}
