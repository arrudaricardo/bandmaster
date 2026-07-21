package project

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// FinalizationRecoveryState is the compatible session/batch state captured on
// either side of a recovery transition.
type FinalizationRecoveryState struct {
	SessionStatus string `json:"session_status"`
	BatchStatus   string `json:"batch_status"`
}

// FinalizationRecoveryEvidence is the immutable evidence used to classify an
// interrupted transaction. HookActivity is stopped, running, or unknown.
type FinalizationRecoveryEvidence struct {
	ExpectedBranch string   `json:"expected_branch"`
	ObservedBranch string   `json:"observed_branch"`
	PreBatchCommit string   `json:"pre_batch_commit"`
	ObservedHead   string   `json:"observed_head"`
	StagedPaths    []string `json:"staged_paths"`
	HookActivity   string   `json:"hook_activity"`
	MonitorStatus  string   `json:"monitor_status"`
	Reasons        []string `json:"reasons"`
}

// FinalizationRecoveryResult is the stable JSON contract for explicit recovery.
// Successful results are persisted verbatim and replayed on later invocations.
type FinalizationRecoveryResult struct {
	SessionID            string                       `json:"-"`
	BatchID              string                       `json:"batch_id"`
	JournalStep          string                       `json:"journal_step"`
	JournalCreatedAt     string                       `json:"journal_created_at,omitempty"`
	Classification       string                       `json:"classification"`
	Action               string                       `json:"action"`
	Outcome              string                       `json:"outcome"`
	Idempotent           bool                         `json:"idempotent"`
	OperatorConfirmation string                       `json:"operator_confirmation,omitempty"`
	Before               FinalizationRecoveryState    `json:"before"`
	After                FinalizationRecoveryState    `json:"after"`
	Evidence             FinalizationRecoveryEvidence `json:"evidence"`
}

type recoveryJournal struct {
	BatchID, Step, Branch, PreBatchCommit, CreatedAt string
}

// RecoverFinalization classifies and resolves one durable interrupted
// finalization. Recognized state is rolled back only with operator confirmation;
// ambiguity is quarantined immediately and audibly.
func (p *Project) RecoverFinalization(confirmation string) (FinalizationRecoveryResult, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return FinalizationRecoveryResult{}, projectError
	}
	defer db.Close()

	session, projectError := inspectOpenSessionWithQueryer(db)
	if projectError != nil {
		return FinalizationRecoveryResult{}, projectError
	}
	var journal recoveryJournal
	err := db.QueryRow(`SELECT batch_id, step, expected_branch, pre_batch_commit, created_at FROM finalization_journals WHERE session_id = ? ORDER BY updated_at DESC LIMIT 1`, session.ID).Scan(&journal.BatchID, &journal.Step, &journal.Branch, &journal.PreBatchCommit, &journal.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		if prior, ok, projectError := readLatestFinalizationRecovery(db, session.ID); projectError != nil {
			return FinalizationRecoveryResult{}, projectError
		} else if ok {
			if prior.Action == "rollback" {
				if projectError := p.ensureRecoveryMonitor(session.ID); projectError != nil {
					return FinalizationRecoveryResult{}, projectError
				}
			}
			return prior, nil
		}
		return FinalizationRecoveryResult{}, invalidSession(session.ID, "finalization_recovery_not_found", "No interrupted finalization journal is available to recover.")
	}
	if err != nil {
		return FinalizationRecoveryResult{}, sessionInternal(session.ID, "read finalization recovery journal", err)
	}
	if prior, ok, projectError := readFinalizationRecovery(db, session.ID, journal.BatchID, journal.CreatedAt); projectError != nil {
		return FinalizationRecoveryResult{}, projectError
	} else if ok {
		return prior, nil
	}

	before, projectError := readFinalizationRecoveryState(db, session.ID, journal.BatchID)
	if projectError != nil {
		return FinalizationRecoveryResult{}, projectError
	}
	evidence := p.inspectFinalizationRecoveryEvidence(db, session.ID, journal)
	recognizedStep := journal.Step == "prepared" || journal.Step == "committing" || journal.Step == "validating"
	if !recognizedStep {
		evidence.Reasons = append(evidence.Reasons, "journal_step_unknown")
	}
	classification := "recognized"
	if len(evidence.Reasons) != 0 {
		classification = "unknown"
	}

	if classification == "recognized" && strings.TrimSpace(confirmation) == "" {
		return FinalizationRecoveryResult{}, invalidSession(session.ID, "finalization_recovery_confirmation_required", "Recognized interrupted finalization requires --confirmation describing the operator's inspection before rollback.")
	}

	result := FinalizationRecoveryResult{
		SessionID: session.ID, BatchID: journal.BatchID, JournalStep: journal.Step, JournalCreatedAt: journal.CreatedAt,
		Classification: classification, Idempotent: true, OperatorConfirmation: confirmation,
		Before: before, Evidence: evidence,
	}
	if classification == "recognized" {
		result.Action, result.Outcome = "rollback", "rolled_back"
		cause := invalidSession(session.ID, "finalization_interrupted", "A previous finalization process stopped before completion.")
		result.After = FinalizationRecoveryState{SessionStatus: "active", BatchStatus: "repair_pending"}
		if rollbackError := p.rollbackFinalizationWithRecovery(db, session, journal.BatchID, journal.Branch, journal.PreBatchCommit, cause, &result); rollbackError == nil || rollbackError.Code != "finalization_failed" {
			if rollbackError == nil {
				rollbackError = sessionInternal(session.ID, "complete explicit finalization rollback", errors.New("rollback returned without its completion result"))
			}
			return FinalizationRecoveryResult{}, rollbackError
		}
		if projectError := p.ensureRecoveryMonitor(session.ID); projectError != nil {
			return FinalizationRecoveryResult{}, projectError
		}
	} else {
		result.Action, result.Outcome = "quarantine", "quarantined"
		observation := integrityObservation{Kind: "ambiguous_finalization_recovery", Path: ".git", BatchID: journal.BatchID, ObservedState: evidence}
		if projectError := p.persistIntegrityViolations(session, []integrityObservation{observation}); projectError != nil {
			return FinalizationRecoveryResult{}, projectError
		}
	}

	if classification != "recognized" {
		result.After, projectError = readFinalizationRecoveryState(db, session.ID, journal.BatchID)
		if projectError != nil {
			return FinalizationRecoveryResult{}, projectError
		}
		if projectError := recordFinalizationRecovery(db, result); projectError != nil {
			return FinalizationRecoveryResult{}, projectError
		}
	}
	return result, nil
}

func (p *Project) ensureRecoveryMonitor(sessionID string) *Error {
	db, projectError := p.openState()
	if projectError != nil {
		return projectError
	}
	monitor, inspectError := inspectLatestMonitor(db, sessionID)
	db.Close()
	if inspectError == nil && monitorHealthy(monitor, time.Now().UTC()) {
		return nil
	}
	return p.StartIntegrityMonitor(sessionID)
}

func (p *Project) inspectFinalizationRecoveryEvidence(db *sql.DB, sessionID string, journal recoveryJournal) FinalizationRecoveryEvidence {
	evidence := FinalizationRecoveryEvidence{
		ExpectedBranch: journal.Branch, PreBatchCommit: journal.PreBatchCommit,
		StagedPaths: []string{}, Reasons: []string{}, HookActivity: "unknown", MonitorStatus: "unknown",
	}
	var err error
	if evidence.ObservedBranch, err = gitOutput(p.Root, "branch", "--show-current"); err != nil {
		evidence.Reasons = append(evidence.Reasons, "branch_unreadable")
	} else if evidence.ObservedBranch != journal.Branch {
		evidence.Reasons = append(evidence.Reasons, "branch_mismatch")
	}
	if evidence.ObservedHead, err = gitOutput(p.Root, "rev-parse", "HEAD"); err != nil {
		evidence.Reasons = append(evidence.Reasons, "head_unreadable")
	}
	if staged, indexErr := gitOutput(p.Root, "diff", "--cached", "--name-only"); indexErr != nil {
		evidence.Reasons = append(evidence.Reasons, "index_unreadable")
	} else if staged != "" {
		evidence.StagedPaths = strings.Split(staged, "\n")
		evidence.Reasons = append(evidence.Reasons, "index_not_clean")
	}
	if running, hookErr := p.finalizationHookRunning(); hookErr != nil {
		evidence.Reasons = append(evidence.Reasons, "hook_activity_unknown")
	} else if running {
		evidence.HookActivity = "running"
		evidence.Reasons = append(evidence.Reasons, "hook_activity_possible")
	} else {
		evidence.HookActivity = "stopped"
	}
	if monitor, monitorError := inspectLatestMonitor(db, sessionID); monitorError != nil {
		evidence.Reasons = append(evidence.Reasons, "monitor_state_unknown")
	} else {
		evidence.MonitorStatus = monitor.Status
		if !monitorStopped(monitor) {
			evidence.Reasons = append(evidence.Reasons, "monitor_not_stopped")
		}
	}
	if evidence.ObservedHead != "" {
		knownHead := evidence.ObservedHead == journal.PreBatchCommit
		var lastCommit string
		err := db.QueryRow(`SELECT committed.commit_sha FROM task_commits committed JOIN tasks task ON task.id = committed.task_id WHERE committed.batch_id = ? ORDER BY task.creation_order DESC LIMIT 1`, journal.BatchID).Scan(&lastCommit)
		if err == nil && evidence.ObservedHead == lastCommit {
			knownHead = true
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			evidence.Reasons = append(evidence.Reasons, "journal_commits_unreadable")
		}
		if !knownHead {
			evidence.Reasons = append(evidence.Reasons, "head_mismatch")
		}
	}
	return evidence
}

func readFinalizationRecoveryState(db *sql.DB, sessionID, batchID string) (FinalizationRecoveryState, *Error) {
	var state FinalizationRecoveryState
	if err := db.QueryRow(`SELECT session.status, batch.status FROM sessions session JOIN batches batch ON batch.session_id = session.id WHERE session.id = ? AND batch.id = ?`, sessionID, batchID).Scan(&state.SessionStatus, &state.BatchStatus); err != nil {
		return state, sessionInternal(sessionID, "read finalization recovery state", err)
	}
	return state, nil
}

type recoveryEventExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func recordFinalizationRecovery(db recoveryEventExecutor, result FinalizationRecoveryResult) *Error {
	beforeJSON, err := json.Marshal(result.Before)
	if err != nil {
		return sessionInternal(result.SessionID, "encode finalization recovery before state", err)
	}
	afterJSON, err := json.Marshal(result.After)
	if err != nil {
		return sessionInternal(result.SessionID, "encode finalization recovery after state", err)
	}
	evidenceJSON, err := json.Marshal(result.Evidence)
	if err != nil {
		return sessionInternal(result.SessionID, "encode finalization recovery evidence", err)
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return sessionInternal(result.SessionID, "encode finalization recovery result", err)
	}
	_, err = db.Exec(`INSERT INTO finalization_recovery_events(batch_id, session_id, journal_created_at, journal_step, classification, action, outcome, operator_confirmation, before_state_json, after_state_json, journal_evidence_json, result_json, occurred_at) VALUES(?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?)`, result.BatchID, result.SessionID, result.JournalCreatedAt, result.JournalStep, result.Classification, result.Action, result.Outcome, result.OperatorConfirmation, beforeJSON, afterJSON, evidenceJSON, resultJSON, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return sessionInternal(result.SessionID, "audit finalization recovery", err)
	}
	return nil
}

func readFinalizationRecovery(db *sql.DB, sessionID, batchID, journalCreatedAt string) (FinalizationRecoveryResult, bool, *Error) {
	var encoded []byte
	err := db.QueryRow(`SELECT result_json FROM finalization_recovery_events WHERE session_id = ? AND batch_id = ? AND journal_created_at = ?`, sessionID, batchID, journalCreatedAt).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return FinalizationRecoveryResult{}, false, nil
	}
	if err != nil {
		return FinalizationRecoveryResult{}, false, sessionInternal(sessionID, "read prior finalization recovery", err)
	}
	var result FinalizationRecoveryResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		return result, false, sessionInternal(sessionID, "decode prior finalization recovery", err)
	}
	result.SessionID = sessionID
	return result, true, nil
}

func readLatestFinalizationRecovery(db *sql.DB, sessionID string) (FinalizationRecoveryResult, bool, *Error) {
	var encoded []byte
	err := db.QueryRow(`SELECT result_json FROM finalization_recovery_events WHERE session_id = ? ORDER BY sequence DESC LIMIT 1`, sessionID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return FinalizationRecoveryResult{}, false, nil
	}
	if err != nil {
		return FinalizationRecoveryResult{}, false, sessionInternal(sessionID, "read latest finalization recovery", err)
	}
	var result FinalizationRecoveryResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		return result, false, sessionInternal(sessionID, "decode latest finalization recovery", err)
	}
	result.SessionID = sessionID
	return result, true, nil
}
