package project

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type databaseQuerier interface {
	rowQuerier
	Query(query string, args ...any) (*sql.Rows, error)
}

func validateTaskAuthority(queryer rowQuerier, sessionID, taskID, token string, requiredStatuses ...string) *Error {
	var status string
	var currentToken sql.NullString
	if err := queryer.QueryRow(`SELECT status, assignment_token FROM tasks WHERE session_id = ? AND id = ?`, sessionID, taskID).Scan(&status, &currentToken); errors.Is(err, sql.ErrNoRows) {
		return invalidSession(sessionID, "task_not_found", fmt.Sprintf("Task %s does not exist in the active session.", taskID))
	} else if err != nil {
		return sessionInternal(sessionID, "validate task authority", err)
	}
	if currentToken.String == "" || token != currentToken.String {
		return invalidSession(sessionID, "invalid_assignment_token", fmt.Sprintf("The assignment token for task %s is missing, stale, or belongs to another task.", taskID))
	}
	for _, requiredStatus := range requiredStatuses {
		if status == requiredStatus {
			return nil
		}
	}
	return invalidSession(sessionID, "invalid_task_state", fmt.Sprintf("Task %s must be %s, not %s.", taskID, strings.Join(requiredStatuses, " or "), status))
}
func validateClaimsAvailable(queryer databaseQuerier, sessionID string, paths []string, semantics pathSemantics) *Error {
	rows, err := queryer.Query(`SELECT path, task_id FROM claims WHERE session_id = ?`, sessionID)
	if err != nil {
		return sessionInternal(sessionID, "inspect claim availability", err)
	}
	defer rows.Close()
	type ownedPath struct{ path, taskID string }
	var owned []ownedPath
	for rows.Next() {
		var current ownedPath
		if err := rows.Scan(&current.path, &current.taskID); err != nil {
			return sessionInternal(sessionID, "read claim availability", err)
		}
		owned = append(owned, current)
	}
	if err := rows.Err(); err != nil {
		return sessionInternal(sessionID, "inspect claim availability", err)
	}
	for _, claimPath := range paths {
		for _, current := range owned {
			if claimPathsConflict(claimPath, current.path, semantics) {
				return blocked(sessionID, "claim_unavailable", fmt.Sprintf("Path %s conflicts with path %s claimed by task %s; no paths were claimed.", claimPath, current.path, current.taskID))
			}
		}
	}
	return nil
}
func (p *Project) validateClaimRepositoryState(queryer databaseQuerier, session Session) *Error {
	branch, err := gitOutput(p.Root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch != session.StartingBranch {
		return invalidSession(session.ID, "branch_drift", "The current branch does not match the active session base.")
	}
	commit, err := gitOutput(p.Root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || commit != session.StartingCommit {
		return invalidSession(session.ID, "head_drift", "The current commit does not match the active session base.")
	}
	if clean, err := gitQuiet(p.Root, "diff", "--cached", "--quiet", "--exit-code"); err != nil {
		return sessionInternal(session.ID, "inspect Git index", err)
	} else if !clean {
		return invalidSession(session.ID, "index_drift", "The Git index changed during the active session.")
	}
	changed, projectError := p.changedPaths()
	if projectError != nil {
		projectError.SessionID = session.ID
		return projectError
	}
	for _, changedPath := range changed {
		var owner string
		err := queryer.QueryRow(`SELECT task_id FROM claims WHERE session_id = ? AND path = ?`, session.ID, changedPath).Scan(&owner)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return sessionInternal(session.ID, "inspect changed path ownership", err)
		}
		return invalidSession(session.ID, "unclaimed_change", fmt.Sprintf("Changed path %s has no owning task.", changedPath))
	}
	return nil
}
func ensureCollectingBatch(tx *sql.Tx, session Session) (string, *Error) {
	var repairBatchID string
	err := tx.QueryRow(`SELECT id FROM batches WHERE session_id = ? AND status IN ('repair_pending', 'repairing') ORDER BY creation_order LIMIT 1`, session.ID).Scan(&repairBatchID)
	if err == nil {
		return "", blocked(session.ID, "batch_repair_in_progress", fmt.Sprintf("Batch %s must complete repair before unrelated work can join a new batch.", repairBatchID))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", sessionInternal(session.ID, "inspect pending batch repair", err)
	}
	var id, baseBranch, baseCommit string
	err = tx.QueryRow(`SELECT id, base_branch, base_commit FROM batches WHERE session_id = ? AND status = 'collecting'`, session.ID).Scan(&id, &baseBranch, &baseCommit)
	if err == nil {
		if baseBranch != session.StartingBranch || baseCommit != session.StartingCommit {
			return "", invalidSession(session.ID, "batch_base_drift", "The collecting batch base no longer matches the active session.")
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", sessionInternal(session.ID, "inspect collecting batch", err)
	}
	id, err = newBatchID()
	if err != nil {
		return "", sessionInternal(session.ID, "generate batch identity", err)
	}
	var creationOrder int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(creation_order), 0) + 1 FROM batches WHERE session_id = ?`, session.ID).Scan(&creationOrder); err != nil {
		return "", sessionInternal(session.ID, "choose batch creation order", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO batches(id, session_id, creation_order, base_branch, base_commit, status, created_at, updated_at) VALUES(?, ?, ?, ?, ?, 'collecting', ?, ?)`, id, session.ID, creationOrder, session.StartingBranch, session.StartingCommit, now, now); err != nil {
		return "", sessionInternal(session.ID, "create collecting batch", err)
	}
	return id, nil
}

func newBatchID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "batch_" + hex.EncodeToString(value), nil
}
