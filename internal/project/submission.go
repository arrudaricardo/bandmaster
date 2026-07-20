package project

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type SubmissionHandoff struct {
	BehaviorChanged        string
	KeyDecisions           string
	ValidationExpectations string
	KnownRisks             string
}

type Submission struct {
	Outcome                string `json:"outcome"`
	NoChanges              bool   `json:"no_changes"`
	BehaviorChanged        string `json:"behavior_changed"`
	KeyDecisions           string `json:"key_decisions"`
	ValidationExpectations string `json:"validation_expectations"`
	KnownRisks             string `json:"known_risks"`
	SubmittedAt            string `json:"submitted_at"`
}

func (p *Project) SubmitTask(id, assignmentToken string, handoff SubmissionHandoff) (Task, *Error) {
	if strings.TrimSpace(assignmentToken) == "" {
		return Task{}, invalid("assignment_token_required", "The current assignment token is required.")
	}
	for name, value := range map[string]string{
		"behavior changed":        handoff.BehaviorChanged,
		"key decisions":           handoff.KeyDecisions,
		"validation expectations": handoff.ValidationExpectations,
		"known risks":             handoff.KnownRisks,
	} {
		if strings.TrimSpace(value) == "" {
			return Task{}, invalid("invalid_submission_handoff", fmt.Sprintf("Submission %s must not be empty; use an explicit value such as None when appropriate.", name))
		}
	}
	if _, projectError := p.renewWorkerLease(id, assignmentToken, "editing"); projectError != nil {
		return Task{}, projectError
	}
	db, projectError := p.openState()
	if projectError != nil {
		return Task{}, projectError
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return Task{}, internal("begin task submission", err)
	}
	defer tx.Rollback()
	session, projectError := inspectActiveSession(tx)
	if projectError != nil {
		return Task{}, projectError
	}
	if projectError := validateTaskAuthority(tx, session.ID, id, assignmentToken, "editing"); projectError != nil {
		return Task{}, projectError
	}
	if projectError := validateTaskBatchStatus(tx, session.ID, id, "collecting", "repairing"); projectError != nil {
		return Task{}, projectError
	}
	if projectError := p.validateClaimRepositoryState(tx, session); projectError != nil {
		return Task{}, projectError
	}
	claims, projectError := loadStoredClaims(tx, session.ID, id)
	if projectError != nil {
		return Task{}, projectError
	}
	currentSnapshots := make([]capturedSnapshot, 0, len(claims))
	reviewedSnapshots, projectError := loadReviewedSnapshots(tx, session.ID, id)
	if projectError != nil {
		return Task{}, projectError
	}
	noChanges := true
	for _, claim := range claims {
		current, projectError := p.capturePath(claim.Path)
		if projectError != nil {
			projectError.SessionID = session.ID
			return Task{}, projectError
		}
		current.Path = claim.Path
		reviewed, exists := reviewedSnapshots[claim.Path]
		if !exists {
			return Task{}, invalidSession(session.ID, "diff_review_required", fmt.Sprintf("Task %s must review every claimed path with task diff before submission.", id))
		}
		if !snapshotsEqual(reviewed, current.PathSnapshot) {
			return Task{}, invalidSession(session.ID, "diff_review_stale", fmt.Sprintf("Claimed path %s changed after its last task diff review.", claim.Path))
		}
		currentSnapshots = append(currentSnapshots, current)
		if !snapshotsEqual(claim.Baseline.PathSnapshot, current.PathSnapshot) {
			noChanges = false
		}
	}
	if projectError := p.validateClaimRepositoryState(tx, session); projectError != nil {
		return Task{}, projectError
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, snapshot := range currentSnapshots {
		if _, err := tx.Exec(`INSERT INTO submitted_snapshots(task_id, path, presence, file_type, content_hash, executable, content) VALUES(?, ?, ?, ?, ?, ?, ?)`, id, snapshot.Path, snapshot.Presence, snapshot.Type, nullableString(snapshot.ContentHash), snapshot.Executable, nullableBytes(snapshot.content)); err != nil {
			return Task{}, sessionInternal(session.ID, "freeze submitted path snapshot", err)
		}
	}
	for _, snapshot := range currentSnapshots {
		current, projectError := p.capturePath(snapshot.Path)
		if projectError != nil {
			projectError.SessionID = session.ID
			return Task{}, projectError
		}
		if !snapshotsEqual(snapshot.PathSnapshot, current.PathSnapshot) {
			return Task{}, invalidSession(session.ID, "submission_snapshot_changed", fmt.Sprintf("Claimed path %s changed while submission snapshots were being frozen.", snapshot.Path))
		}
	}
	outcome := "pending_changes"
	if noChanges {
		outcome = "pending_no_op"
	}
	if _, err := tx.Exec(`INSERT INTO task_submissions(task_id, outcome, no_changes, behavior_changed, key_decisions, validation_expectations, known_risks, submitted_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, id, outcome, noChanges, handoff.BehaviorChanged, handoff.KeyDecisions, handoff.ValidationExpectations, handoff.KnownRisks, now); err != nil {
		return Task{}, sessionInternal(session.ID, "record task handoff", err)
	}
	result, err := tx.Exec(`UPDATE tasks SET status = 'submitted', updated_at = ? WHERE id = ? AND status = 'editing'`, now, id)
	if err != nil {
		return Task{}, sessionInternal(session.ID, "submit task", err)
	}
	if updated, err := result.RowsAffected(); err != nil || updated != 1 {
		if err == nil {
			err = fmt.Errorf("updated %d tasks", updated)
		}
		return Task{}, sessionInternal(session.ID, "confirm task submission", err)
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'task_submitted', 'editing', 'submitted', ?)`, session.ID, id, now); err != nil {
		return Task{}, sessionInternal(session.ID, "record task submission", err)
	}
	if _, err := tx.Exec(`UPDATE task_leases SET status = 'closed' WHERE task_id = ?`, id); err != nil {
		return Task{}, sessionInternal(session.ID, "close submitted worker lease", err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, sessionInternal(session.ID, "commit task submission", err)
	}
	if projectError := p.PrepareMutation("post-task submission"); projectError != nil {
		return Task{}, projectError
	}
	return inspectTask(db, session.ID, id)
}

func loadReviewedSnapshots(queryer databaseQuerier, sessionID, taskID string) (map[string]PathSnapshot, *Error) {
	rows, err := queryer.Query(`SELECT path, presence, file_type, content_hash, executable FROM task_diff_reviews WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, sessionInternal(sessionID, "read task diff review", err)
	}
	defer rows.Close()
	reviewed := make(map[string]PathSnapshot)
	for rows.Next() {
		var claimPath string
		var snapshot PathSnapshot
		var contentHash sql.NullString
		var executable int
		if err := rows.Scan(&claimPath, &snapshot.Presence, &snapshot.Type, &contentHash, &executable); err != nil {
			return nil, sessionInternal(sessionID, "read reviewed path snapshot", err)
		}
		snapshot.ContentHash = contentHash.String
		snapshot.Executable = executable != 0
		reviewed[claimPath] = snapshot
	}
	if err := rows.Err(); err != nil {
		return nil, sessionInternal(sessionID, "read task diff review", err)
	}
	return reviewed, nil
}
