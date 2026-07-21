package project

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type BatchAbandonmentState struct {
	SessionStatus string `json:"session_status"`
	BatchStatus   string `json:"batch_status"`
}

type ReleasedClaim struct {
	TaskID string `json:"task_id"`
	Path   string `json:"path"`
}

type BatchAbandonmentEvidence struct {
	ExpectedBranch  string   `json:"expected_branch"`
	ObservedBranch  string   `json:"observed_branch"`
	ExpectedHead    string   `json:"expected_head"`
	ObservedHead    string   `json:"observed_head"`
	StagedPaths     []string `json:"staged_paths"`
	JournalStep     string   `json:"journal_step,omitempty"`
	JournalPlanJSON string   `json:"journal_plan_json,omitempty"`
	Reasons         []string `json:"reasons"`
}

type BatchAbandonmentResult struct {
	SessionID            string                      `json:"-"`
	BatchID              string                      `json:"batch_id"`
	Reason               string                      `json:"reason"`
	Confirmation         string                      `json:"confirmation"`
	Outcome              string                      `json:"outcome"`
	NextAction           string                      `json:"next_action"`
	Idempotent           bool                        `json:"idempotent"`
	ReleasedClaims       []ReleasedClaim             `json:"released_claims"`
	Before               BatchAbandonmentState       `json:"before"`
	After                BatchAbandonmentState       `json:"after"`
	Evidence             BatchAbandonmentEvidence    `json:"evidence"`
	FinalizationRecovery *FinalizationRecoveryResult `json:"finalization_recovery,omitempty"`
}

func (p *Project) AbandonBatch(reason, confirmation string) (BatchAbandonmentResult, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return BatchAbandonmentResult{}, projectError
	}
	session, projectError := inspectOpenSessionWithQueryer(db)
	if projectError != nil {
		db.Close()
		return BatchAbandonmentResult{}, projectError
	}
	var batchID, batchStatus, baseBranch, baseCommit string
	err := db.QueryRow(`SELECT id, status, base_branch, base_commit FROM batches WHERE session_id = ? ORDER BY creation_order DESC LIMIT 1`, session.ID).Scan(&batchID, &batchStatus, &baseBranch, &baseCommit)
	if errors.Is(err, sql.ErrNoRows) {
		db.Close()
		return BatchAbandonmentResult{}, invalidSession(session.ID, "batch_abandonment_not_found", "No batch is available to abandon.")
	}
	if err != nil {
		db.Close()
		return BatchAbandonmentResult{}, sessionInternal(session.ID, "read batch abandonment target", err)
	}
	if prior, ok, projectError := readBatchAbandonment(db, session.ID, batchID); projectError != nil {
		db.Close()
		return BatchAbandonmentResult{}, projectError
	} else if ok {
		db.Close()
		return prior, nil
	}
	if strings.TrimSpace(reason) == "" {
		db.Close()
		return BatchAbandonmentResult{}, invalidSession(session.ID, "batch_abandonment_reason_required", "Batch abandonment requires --reason describing why the batch will not continue.")
	}
	if strings.TrimSpace(confirmation) == "" {
		db.Close()
		return BatchAbandonmentResult{}, invalidSession(session.ID, "batch_abandonment_confirmation_required", "Batch abandonment requires --confirmation that every worker and finalization process has stopped.")
	}
	if !abandonableBatchStatus(batchStatus) {
		db.Close()
		if batchStatus == "quarantined" {
			return BatchAbandonmentResult{}, quarantined(session.ID, "batch_abandonment_quarantined", "The batch has unresolved integrity evidence; inspect and recover it before abandonment.")
		}
		return BatchAbandonmentResult{}, invalidSession(session.ID, "batch_not_abandonable", "Only a recognized nonterminal batch can be abandoned.")
	}

	result := BatchAbandonmentResult{
		SessionID: session.ID, BatchID: batchID, Reason: reason, Confirmation: confirmation,
		Outcome: "abandoned", NextAction: "session abort or inspect preserved edits", Idempotent: true,
		ReleasedClaims: []ReleasedClaim{},
		Before:         BatchAbandonmentState{SessionStatus: session.Status, BatchStatus: batchStatus},
		Evidence:       BatchAbandonmentEvidence{ExpectedBranch: baseBranch, ExpectedHead: baseCommit, StagedPaths: []string{}, Reasons: []string{}},
	}

	var journalCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM finalization_journals WHERE batch_id = ?`, batchID).Scan(&journalCount); err != nil {
		db.Close()
		return BatchAbandonmentResult{}, sessionInternal(session.ID, "inspect abandonment finalization journal", err)
	}
	if journalCount != 0 {
		if err := db.QueryRow(`SELECT step, commit_plan_json FROM finalization_journals WHERE batch_id = ?`, batchID).Scan(&result.Evidence.JournalStep, &result.Evidence.JournalPlanJSON); err != nil {
			db.Close()
			return BatchAbandonmentResult{}, sessionInternal(session.ID, "read abandonment finalization journal", err)
		}
		db.Close()
		recovery, recoveryError := p.RecoverFinalization(confirmation)
		if recoveryError != nil {
			return BatchAbandonmentResult{}, recoveryError
		}
		result.FinalizationRecovery = &recovery
		if recovery.Outcome != "rolled_back" {
			return BatchAbandonmentResult{}, quarantined(session.ID, "batch_abandonment_quarantined", "Finalization recovery found ambiguous Git state; the batch remains quarantined.")
		}
		db, projectError = p.openState()
		if projectError != nil {
			return BatchAbandonmentResult{}, projectError
		}
		session, projectError = inspectOpenSessionWithQueryer(db)
		if projectError != nil {
			db.Close()
			return BatchAbandonmentResult{}, projectError
		}
		batchStatus = "repair_pending"
	}
	defer db.Close()
	if journalCount == 0 && batchStatus == "final_validating" {
		result.Evidence.Reasons = append(result.Evidence.Reasons, "finalization_journal_missing")
	}
	if journalCount == 0 {
		var provisionalCommits int
		if err := db.QueryRow(`SELECT COUNT(*) FROM task_commits WHERE batch_id = ?`, batchID).Scan(&provisionalCommits); err != nil {
			return BatchAbandonmentResult{}, sessionInternal(session.ID, "inspect abandonment provisional commits", err)
		} else if provisionalCommits != 0 {
			result.Evidence.Reasons = append(result.Evidence.Reasons, "finalization_journal_missing")
		}
	}
	if projectError := p.StopIntegrityMonitor(session.ID); projectError != nil {
		return BatchAbandonmentResult{}, projectError
	}

	if result.Evidence.ObservedBranch, err = gitOutput(p.Root, "branch", "--show-current"); err != nil {
		result.Evidence.Reasons = append(result.Evidence.Reasons, "branch_unreadable")
	} else if result.Evidence.ObservedBranch != baseBranch {
		result.Evidence.Reasons = append(result.Evidence.Reasons, "branch_mismatch")
	}
	if result.Evidence.ObservedHead, err = gitOutput(p.Root, "rev-parse", "HEAD"); err != nil {
		result.Evidence.Reasons = append(result.Evidence.Reasons, "head_unreadable")
	} else if result.Evidence.ObservedHead != baseCommit {
		result.Evidence.Reasons = append(result.Evidence.Reasons, "head_mismatch")
	}
	if staged, indexErr := gitOutput(p.Root, "diff", "--cached", "--name-only"); indexErr != nil {
		result.Evidence.Reasons = append(result.Evidence.Reasons, "index_unreadable")
	} else if staged != "" {
		result.Evidence.StagedPaths = strings.Split(staged, "\n")
		result.Evidence.Reasons = append(result.Evidence.Reasons, "index_not_clean")
	}
	if len(result.Evidence.Reasons) != 0 {
		observation := integrityObservation{Kind: "ambiguous_batch_abandonment", Path: ".git", BatchID: batchID, ObservedState: result.Evidence}
		if projectError := p.persistIntegrityViolations(session, []integrityObservation{observation}); projectError != nil {
			return BatchAbandonmentResult{}, projectError
		}
		return BatchAbandonmentResult{}, quarantined(session.ID, "batch_abandonment_quarantined", "Batch abandonment found unknown Git state and left the batch quarantined.")
	}

	claimRows, err := db.Query(`SELECT task_id, path FROM claims WHERE batch_id = ? ORDER BY task_id, claim_order`, batchID)
	if err != nil {
		return BatchAbandonmentResult{}, sessionInternal(session.ID, "read abandonment claims", err)
	}
	for claimRows.Next() {
		var claim ReleasedClaim
		if err := claimRows.Scan(&claim.TaskID, &claim.Path); err != nil {
			claimRows.Close()
			return BatchAbandonmentResult{}, sessionInternal(session.ID, "read abandonment claim", err)
		}
		result.ReleasedClaims = append(result.ReleasedClaims, claim)
	}
	if err := claimRows.Close(); err != nil {
		return BatchAbandonmentResult{}, sessionInternal(session.ID, "close abandonment claims", err)
	}
	result.After = BatchAbandonmentState{SessionStatus: "paused", BatchStatus: "abandoned"}
	if projectError := applyBatchAbandonment(db, result, session.Status, batchStatus); projectError != nil {
		return BatchAbandonmentResult{}, projectError
	}
	return result, nil
}

func abandonableBatchStatus(status string) bool {
	switch status {
	case "collecting", "frozen", "validating", "repair_pending", "repairing", "finalizing", "final_validating":
		return true
	default:
		return false
	}
}

func applyBatchAbandonment(db *sql.DB, result BatchAbandonmentResult, currentSessionStatus, currentBatchStatus string) *Error {
	tx, err := db.Begin()
	if err != nil {
		return sessionInternal(result.SessionID, "begin batch abandonment", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := tx.Query(`SELECT id, status, COALESCE(worker_identity, '') FROM tasks WHERE id IN (SELECT task_id FROM batch_members WHERE batch_id = ?) AND status NOT IN ('committed', 'no_op', 'canceled') ORDER BY creation_order`, result.BatchID)
	if err != nil {
		return sessionInternal(result.SessionID, "read abandonment tasks", err)
	}
	type taskState struct{ id, status, worker string }
	var tasks []taskState
	for rows.Next() {
		var task taskState
		if err := rows.Scan(&task.id, &task.status, &task.worker); err != nil {
			rows.Close()
			return sessionInternal(result.SessionID, "read abandonment task", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Close(); err != nil {
		return sessionInternal(result.SessionID, "close abandonment tasks", err)
	}
	for _, task := range tasks {
		if _, err := tx.Exec(`UPDATE tasks SET status = 'canceled', worker_identity = NULL, assignment_token = NULL, updated_at = ? WHERE id = ?`, now, task.id); err != nil {
			return sessionInternal(result.SessionID, "cancel abandoned task", err)
		}
		if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, worker_identity, termination_proof, occurred_at) VALUES(?, ?, 'batch_abandoned', ?, 'canceled', NULLIF(?, ''), ?, ?)`, result.SessionID, task.id, task.status, task.worker, result.Confirmation, now); err != nil {
			return sessionInternal(result.SessionID, "audit abandoned task", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM claims WHERE batch_id = ?`, result.BatchID); err != nil {
		return sessionInternal(result.SessionID, "release abandoned claims", err)
	}
	batchUpdate, err := tx.Exec(`UPDATE batches SET status = 'abandoned', updated_at = ? WHERE id = ? AND status = ?`, now, result.BatchID, currentBatchStatus)
	if err != nil {
		return sessionInternal(result.SessionID, "abandon batch", err)
	}
	updated, err := batchUpdate.RowsAffected()
	if err != nil {
		return sessionInternal(result.SessionID, "confirm batch abandonment", err)
	}
	if updated != 1 {
		return invalidSession(result.SessionID, "batch_abandonment_raced", "Batch state changed while abandonment was being recorded.")
	}
	sessionUpdate, err := tx.Exec(`UPDATE sessions SET status = 'paused', updated_at = ? WHERE id = ? AND status = ?`, now, result.SessionID, currentSessionStatus)
	if err != nil {
		return sessionInternal(result.SessionID, "pause abandoned session", err)
	}
	updated, err = sessionUpdate.RowsAffected()
	if err != nil {
		return sessionInternal(result.SessionID, "confirm abandoned session pause", err)
	}
	if updated != 1 {
		return invalidSession(result.SessionID, "batch_abandonment_raced", "Session state changed while abandonment was being recorded.")
	}
	if _, err := tx.Exec(`INSERT INTO batch_audit_events(session_id, batch_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'batch_abandoned', ?, 'abandoned', ?)`, result.SessionID, result.BatchID, currentBatchStatus, now); err != nil {
		return sessionInternal(result.SessionID, "audit abandoned batch", err)
	}
	if _, err := tx.Exec(`INSERT INTO audit_events(session_id, event, from_status, to_status, occurred_at) VALUES(?, 'batch_abandoned', ?, 'paused', ?)`, result.SessionID, result.Before.SessionStatus, now); err != nil {
		return sessionInternal(result.SessionID, "audit abandoned session", err)
	}
	beforeJSON, err := json.Marshal(result.Before)
	if err != nil {
		return sessionInternal(result.SessionID, "encode batch abandonment before state", err)
	}
	afterJSON, err := json.Marshal(result.After)
	if err != nil {
		return sessionInternal(result.SessionID, "encode batch abandonment after state", err)
	}
	evidenceJSON, err := json.Marshal(result.Evidence)
	if err != nil {
		return sessionInternal(result.SessionID, "encode batch abandonment evidence", err)
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return sessionInternal(result.SessionID, "encode batch abandonment", err)
	}
	if _, err := tx.Exec(`INSERT INTO batch_abandonment_events(batch_id, session_id, reason, confirmation, before_state_json, after_state_json, evidence_json, result_json, occurred_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, result.BatchID, result.SessionID, result.Reason, result.Confirmation, beforeJSON, afterJSON, evidenceJSON, resultJSON, now); err != nil {
		return sessionInternal(result.SessionID, "record batch abandonment", err)
	}
	if projectError := validateSessionBatchPair(tx, Session{ID: result.SessionID, Status: "paused"}); projectError != nil {
		return projectError
	}
	if err := tx.Commit(); err != nil {
		return sessionInternal(result.SessionID, "commit batch abandonment", err)
	}
	return nil
}

func readBatchAbandonment(db *sql.DB, sessionID, batchID string) (BatchAbandonmentResult, bool, *Error) {
	var encoded []byte
	err := db.QueryRow(`SELECT result_json FROM batch_abandonment_events WHERE session_id = ? AND batch_id = ?`, sessionID, batchID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return BatchAbandonmentResult{}, false, nil
	}
	if err != nil {
		return BatchAbandonmentResult{}, false, sessionInternal(sessionID, "read prior batch abandonment", err)
	}
	var result BatchAbandonmentResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		return result, false, sessionInternal(sessionID, "decode prior batch abandonment", err)
	}
	result.SessionID = sessionID
	if result.FinalizationRecovery != nil {
		result.FinalizationRecovery.SessionID = sessionID
	}
	return result, true, nil
}
