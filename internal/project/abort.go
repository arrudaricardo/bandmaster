package project

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

type AbortPlan struct {
	SessionID          string              `json:"-"`
	SessionStatus      string              `json:"session_status"`
	AffectedTasks      []AbortTaskPlan     `json:"affected_tasks"`
	ActiveClaims       []AbortClaimPlan    `json:"active_claims"`
	PreservedArtifacts []AbortArtifactPlan `json:"preserved_artifacts"`
	Batches            []AbortBatchPlan    `json:"batches"`
	Journals           []AbortJournalPlan  `json:"journals"`
	Files              []string            `json:"files"`
	Blockers           []AbortBlocker      `json:"blockers"`
}

type AbortTaskPlan struct {
	TaskID        string `json:"task_id"`
	CurrentStatus string `json:"current_status"`
	TargetStatus  string `json:"target_status"`
	Agent         string `json:"agent_identity,omitempty"`
}

type AbortClaimPlan struct {
	TaskID string `json:"task_id"`
	Path   string `json:"path"`
}

type AbortArtifactPlan struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

type AbortBatchPlan struct {
	BatchID string `json:"batch_id"`
	Status  string `json:"status"`
}

type AbortJournalPlan struct {
	BatchID string `json:"batch_id"`
	Step    string `json:"step"`
}

type AbortBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (p *Project) PlanAbort(terminationConfirmation string) (AbortPlan, *Error) {
	db, projectError := p.openStateReadOnly()
	if projectError != nil {
		return AbortPlan{}, projectError
	}
	defer db.Close()
	return p.planAbort(db, terminationConfirmation)
}

func (p *Project) planAbort(queryer databaseQuerier, terminationConfirmation string) (AbortPlan, *Error) {
	session, projectError := inspectOpenSessionWithQueryer(queryer)
	if projectError != nil {
		return AbortPlan{}, projectError
	}
	plan := AbortPlan{
		SessionID:          session.ID,
		SessionStatus:      session.Status,
		AffectedTasks:      []AbortTaskPlan{},
		ActiveClaims:       []AbortClaimPlan{},
		PreservedArtifacts: []AbortArtifactPlan{},
		Batches:            []AbortBatchPlan{},
		Journals:           []AbortJournalPlan{},
		Files:              []string{},
		Blockers:           []AbortBlocker{},
	}

	taskRows, err := queryer.Query(`SELECT id, status, COALESCE(agent_identity, '') FROM tasks WHERE session_id = ? AND status NOT IN ('committed', 'no_op', 'canceled') ORDER BY creation_order`, session.ID)
	if err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "plan abort tasks", err)
	}
	agents := 0
	for taskRows.Next() {
		var task AbortTaskPlan
		if err := taskRows.Scan(&task.TaskID, &task.CurrentStatus, &task.Agent); err != nil {
			taskRows.Close()
			return AbortPlan{}, sessionInternal(session.ID, "read abort task plan", err)
		}
		task.TargetStatus = "quarantined"
		if task.Agent != "" {
			agents++
		}
		plan.AffectedTasks = append(plan.AffectedTasks, task)
	}
	if err := taskRows.Close(); err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "close abort task plan", err)
	}
	if err := taskRows.Err(); err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "read abort task plan", err)
	}

	fileSet := map[string]struct{}{}
	claimRows, err := queryer.Query(`SELECT task_id, path FROM claims WHERE session_id = ? ORDER BY task_id, claim_order`, session.ID)
	if err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "plan abort claims", err)
	}
	for claimRows.Next() {
		var claim AbortClaimPlan
		if err := claimRows.Scan(&claim.TaskID, &claim.Path); err != nil {
			claimRows.Close()
			return AbortPlan{}, sessionInternal(session.ID, "read abort claim plan", err)
		}
		plan.ActiveClaims = append(plan.ActiveClaims, claim)
		fileSet[claim.Path] = struct{}{}
	}
	if err := claimRows.Close(); err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "close abort claim plan", err)
	}
	if err := claimRows.Err(); err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "read abort claim plan", err)
	}

	batchRows, err := queryer.Query(`SELECT id, status FROM batches WHERE session_id = ? ORDER BY creation_order`, session.ID)
	if err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "plan abort batches", err)
	}
	for batchRows.Next() {
		var batch AbortBatchPlan
		if err := batchRows.Scan(&batch.BatchID, &batch.Status); err != nil {
			batchRows.Close()
			return AbortPlan{}, sessionInternal(session.ID, "read abort batch plan", err)
		}
		plan.Batches = append(plan.Batches, batch)
	}
	if err := batchRows.Close(); err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "close abort batch plan", err)
	}
	if err := batchRows.Err(); err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "read abort batch plan", err)
	}

	journalRows, err := queryer.Query(`SELECT batch_id, step FROM finalization_journals WHERE session_id = ? ORDER BY batch_id`, session.ID)
	if err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "plan abort journals", err)
	}
	for journalRows.Next() {
		var journal AbortJournalPlan
		if err := journalRows.Scan(&journal.BatchID, &journal.Step); err != nil {
			journalRows.Close()
			return AbortPlan{}, sessionInternal(session.ID, "read abort journal plan", err)
		}
		plan.Journals = append(plan.Journals, journal)
	}
	if err := journalRows.Close(); err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "close abort journal plan", err)
	}
	if err := journalRows.Err(); err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "read abort journal plan", err)
	}

	frozenRows, err := queryer.Query(`SELECT frozen.path FROM frozen_batch_paths frozen JOIN batches batch ON batch.id = frozen.batch_id WHERE batch.session_id = ? ORDER BY frozen.path`, session.ID)
	if err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "plan preserved abort files", err)
	}
	for frozenRows.Next() {
		var path string
		if err := frozenRows.Scan(&path); err != nil {
			frozenRows.Close()
			return AbortPlan{}, sessionInternal(session.ID, "read preserved abort file", err)
		}
		fileSet[path] = struct{}{}
	}
	if err := frozenRows.Close(); err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "close preserved abort files", err)
	}
	if err := frozenRows.Err(); err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "read preserved abort files", err)
	}
	for path := range fileSet {
		plan.Files = append(plan.Files, path)
	}
	sort.Strings(plan.Files)

	ownershipQuery := `SELECT COUNT(*) FROM task_path_ownership WHERE session_id = ?`
	var ownershipTableCount int
	if err := queryer.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'task_path_ownership'`).Scan(&ownershipTableCount); err != nil {
		return AbortPlan{}, sessionInternal(session.ID, "inspect ownership evidence schema", err)
	}
	if ownershipTableCount == 0 {
		// Before ownership evidence was split from active locking, claims carried
		// the same immutable baseline fields. Preview that legacy evidence without
		// running the migration a real abort will apply before cleanup.
		ownershipQuery = `SELECT COUNT(*) FROM claims WHERE session_id = ?`
	}
	for _, artifact := range []struct {
		kind  string
		query string
	}{
		{kind: "ownership_evidence", query: ownershipQuery},
		{kind: "submissions", query: `SELECT COUNT(*) FROM task_submissions submission JOIN tasks task ON task.id = submission.task_id WHERE task.session_id = ?`},
		{kind: "submitted_snapshots", query: `SELECT COUNT(*) FROM submitted_snapshots snapshot JOIN tasks task ON task.id = snapshot.task_id WHERE task.session_id = ?`},
		{kind: "frozen_batch_paths", query: `SELECT COUNT(*) FROM frozen_batch_paths frozen JOIN batches batch ON batch.id = frozen.batch_id WHERE batch.session_id = ?`},
		{kind: "task_audit_events", query: `SELECT COUNT(*) FROM task_audit_events WHERE session_id = ?`},
		{kind: "failure_evidence", query: `SELECT COUNT(*) FROM integrity_violations WHERE session_id = ?`},
	} {
		var count int
		if err := queryer.QueryRow(artifact.query, session.ID).Scan(&count); err != nil {
			return AbortPlan{}, sessionInternal(session.ID, "count preserved abort artifacts", err)
		}
		plan.PreservedArtifacts = append(plan.PreservedArtifacts, AbortArtifactPlan{Kind: artifact.kind, Count: count})
	}

	if agents != 0 && strings.TrimSpace(terminationConfirmation) == "" {
		plan.Blockers = append(plan.Blockers, AbortBlocker{Code: "agent_termination_confirmation_required", Message: "Abort requires proof that every assigned agent has stopped; provide --termination-confirmation."})
	}
	if session.Status == "finalizing" || len(plan.Journals) != 0 {
		plan.Blockers = append(plan.Blockers, AbortBlocker{Code: "finalization_recovery_required", Message: "Finalizing Git state must be reconciled with finalization recover before abort can release ownership."})
	} else if session.Status != "active" && session.Status != "paused" && session.Status != "aborting" {
		plan.Blockers = append(plan.Blockers, AbortBlocker{Code: "invalid_session_transition", Message: fmt.Sprintf("Cannot abort a session in %s state.", session.Status)})
	}
	return plan, nil
}

// AbortSession preserves working-tree and immutable evidence while releasing
// only active orchestration state. External liveness preconditions are completed
// before the single durable cleanup transaction begins.
func (p *Project) AbortSession(terminationConfirmation string) (Session, *Error) {
	plan, projectError := p.PlanAbort(terminationConfirmation)
	if projectError != nil {
		return Session{}, projectError
	}
	if len(plan.Blockers) != 0 {
		blocker := plan.Blockers[0]
		return Session{}, invalidSession(plan.SessionID, blocker.Code, blocker.Message)
	}
	if projectError := p.StopIntegrityMonitor(plan.SessionID); projectError != nil {
		return Session{}, projectError
	}

	db, projectError := p.openState()
	if projectError != nil {
		return Session{}, projectError
	}
	defer db.Close()
	currentPlan, projectError := p.planAbort(db, terminationConfirmation)
	if projectError != nil {
		return Session{}, projectError
	}
	if len(currentPlan.Blockers) != 0 {
		blocker := currentPlan.Blockers[0]
		return Session{}, invalidSession(plan.SessionID, blocker.Code, blocker.Message)
	}
	tx, err := db.Begin()
	if err != nil {
		return Session{}, abortCleanupError(plan.SessionID, "begin cleanup", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, task := range currentPlan.AffectedTasks {
		if task.CurrentStatus == "quarantined" {
			continue
		}
		result, err := tx.Exec(`UPDATE tasks SET status = 'quarantined', updated_at = ? WHERE id = ? AND status = ?`, now, task.TaskID, task.CurrentStatus)
		if err != nil {
			return Session{}, abortCleanupError(plan.SessionID, "quarantine task", err)
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			if err == nil {
				err = fmt.Errorf("task %s changed during abort", task.TaskID)
			}
			return Session{}, abortCleanupError(plan.SessionID, "confirm task quarantine", err)
		}
		if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, agent_identity, termination_proof, occurred_at) VALUES(?, ?, 'session_aborted', ?, 'quarantined', ?, ?, ?)`, plan.SessionID, task.TaskID, task.CurrentStatus, nullableString(task.Agent), nullableString(terminationConfirmation), now); err != nil {
			return Session{}, abortCleanupError(plan.SessionID, "audit task abort", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM claims WHERE session_id = ?`, plan.SessionID); err != nil {
		return Session{}, abortCleanupError(plan.SessionID, "release active claims", err)
	}
	if os.Getenv("BANDMASTER_TEST_FAIL_ABORT_AT") == "after-claim-release" {
		return Session{}, abortCleanupError(plan.SessionID, "injected cleanup failure", fmt.Errorf("test failure after claim release"))
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO session_abort_events(session_id, termination_confirmation, occurred_at) VALUES(?, ?, ?)`, plan.SessionID, terminationConfirmation, now); err != nil {
		return Session{}, abortCleanupError(plan.SessionID, "record abort confirmation", err)
	}
	result, err := tx.Exec(`UPDATE sessions SET status = 'aborted', updated_at = ? WHERE id = ? AND status = ?`, now, plan.SessionID, currentPlan.SessionStatus)
	if err != nil {
		return Session{}, abortCleanupError(plan.SessionID, "complete abort", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err == nil {
			err = fmt.Errorf("session changed during abort")
		}
		return Session{}, abortCleanupError(plan.SessionID, "confirm abort transition", err)
	}
	if _, err := tx.Exec(`INSERT INTO audit_events(session_id, event, from_status, to_status, occurred_at) VALUES(?, 'session_aborted', ?, 'aborted', ?)`, plan.SessionID, currentPlan.SessionStatus, now); err != nil {
		return Session{}, abortCleanupError(plan.SessionID, "audit completed abort", err)
	}
	if err := tx.Commit(); err != nil {
		return Session{}, abortCleanupError(plan.SessionID, "commit abort", err)
	}
	return p.inspectSession(db, plan.SessionID)
}

func abortCleanupError(sessionID, action string, err error) *Error {
	return &Error{Code: "abort_cleanup_failed", Message: fmt.Sprintf("Abort cleanup can be retried after %s failed: %v", action, err), Retryable: true, ExitCode: 2, SessionID: sessionID}
}
