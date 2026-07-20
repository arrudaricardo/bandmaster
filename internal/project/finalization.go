package project

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CommitBatch creates the task-attributed commits for a successfully validated batch.
// It deliberately leaves rollback and hook-integrity recovery to the finalization
// recovery workflow; this transition only accepts the known clean, validated state.
func (p *Project) CommitBatch() (Batch, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return Batch{}, projectError
	}
	defer db.Close()

	session, projectError := inspectOpenSessionWithQueryer(db)
	if projectError != nil {
		return Batch{}, projectError
	}
	var batchID, status string
	if err := db.QueryRow(`SELECT id, status FROM batches WHERE session_id = ? ORDER BY creation_order DESC LIMIT 1`, session.ID).Scan(&batchID, &status); err != nil {
		return Batch{}, sessionInternal(session.ID, "read commit batch", err)
	}
	if status == "committed" {
		return inspectBatch(db, batchID)
	}
	if session.Status != "finalizing" {
		return Batch{}, invalidSession(session.ID, "session_not_finalizing", "Committing requires a validated batch in a finalizing session.")
	}
	if status != "finalizing" {
		return Batch{}, invalidSession(session.ID, "batch_not_validated", fmt.Sprintf("Batch %s cannot commit from %s state.", batchID, status))
	}
	if observations, scanError := p.scanRepository(db, session); scanError != nil {
		return Batch{}, scanError
	} else if len(observations) != 0 {
		if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
			return Batch{}, projectError
		}
		return Batch{}, integrityError(session.ID, observations[0])
	}

	members, projectError := finalizationMembers(db, session.ID, batchID)
	if projectError != nil {
		return Batch{}, projectError
	}
	if projectError := beginFinalization(db, session.ID, batchID); projectError != nil {
		return Batch{}, projectError
	}
	for _, member := range members {
		if !member.changed {
			continue
		}
		sha, projectError := p.commitTask(session, batchID, member)
		if projectError != nil {
			return Batch{}, projectError
		}
		if projectError := recordTaskCommit(db, session.ID, batchID, member.id, sha); projectError != nil {
			return Batch{}, projectError
		}
	}
	if output, err := gitOutput(p.Root, "status", "--porcelain=v1"); err != nil {
		return Batch{}, sessionInternal(session.ID, "verify committed worktree", err)
	} else if output != "" {
		return Batch{}, invalidSession(session.ID, "finalization_dirty_worktree", "Task commits left Git-visible changes in the worktree or index.")
	}
	if projectError := p.runFinalValidation(db, session, batchID); projectError != nil {
		return Batch{}, projectError
	}
	if projectError := p.completeFinalization(db, session.ID, batchID); projectError != nil {
		return Batch{}, projectError
	}
	if projectError := p.StartIntegrityMonitor(session.ID); projectError != nil {
		return Batch{}, projectError
	}
	return inspectBatch(db, batchID)
}

type finalizationMember struct {
	id, title, intent string
	order             int64
	changed           bool
	paths             []string
}

func finalizationMembers(db *sql.DB, sessionID, batchID string) ([]finalizationMember, *Error) {
	rows, err := db.Query(`SELECT task.id, task.title, task.intent, task.creation_order, submission.no_changes
		FROM batch_members member JOIN tasks task ON task.id = member.task_id
		JOIN task_submissions submission ON submission.task_id = task.id
		WHERE member.batch_id = ? ORDER BY task.creation_order`, batchID)
	if err != nil {
		return nil, sessionInternal(sessionID, "read commit members", err)
	}
	defer rows.Close()
	var members []finalizationMember
	for rows.Next() {
		var member finalizationMember
		var noChanges int
		if err := rows.Scan(&member.id, &member.title, &member.intent, &member.order, &noChanges); err != nil {
			return nil, sessionInternal(sessionID, "read commit member", err)
		}
		member.changed = noChanges == 0
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, sessionInternal(sessionID, "read commit members", err)
	}
	if err := rows.Close(); err != nil {
		return nil, sessionInternal(sessionID, "close commit members", err)
	}
	for index := range members {
		pathRows, err := db.Query(`SELECT path FROM claims WHERE task_id = ? ORDER BY claim_order`, members[index].id)
		if err != nil {
			return nil, sessionInternal(sessionID, "read task commit claims", err)
		}
		for pathRows.Next() {
			var path string
			if err := pathRows.Scan(&path); err != nil {
				pathRows.Close()
				return nil, sessionInternal(sessionID, "read task commit claim", err)
			}
			members[index].paths = append(members[index].paths, path)
		}
		if err := pathRows.Close(); err != nil {
			return nil, sessionInternal(sessionID, "close task commit claims", err)
		}
	}
	return members, nil
}

func (p *Project) commitTask(session Session, batchID string, member finalizationMember) (string, *Error) {
	parent, err := gitOutput(p.Root, "rev-parse", "HEAD")
	if err != nil {
		return "", sessionInternal(session.ID, "read commit parent", err)
	}
	if len(member.paths) == 0 {
		return "", invalidSession(session.ID, "task_has_no_claims", "A changed submitted task has no claims.")
	}
	changed, err := gitOutput(p.Root, append([]string{"diff", "--name-only", "--"}, member.paths...)...)
	if err != nil {
		return "", sessionInternal(session.ID, "inspect task changes", err)
	}
	expectedPaths := strings.Fields(changed)
	if len(expectedPaths) == 0 {
		return "", invalidSession(session.ID, "submission_snapshot_mismatch", "A changed submission has no current Git-visible changes.")
	}
	args := append([]string{"add", "--"}, member.paths...)
	if _, err := gitOutput(p.Root, args...); err != nil {
		return "", sessionInternal(session.ID, "stage task changes", err)
	}
	staged, err := gitOutput(p.Root, "diff", "--cached", "--name-only", "--")
	if err != nil {
		return "", sessionInternal(session.ID, "inspect staged task paths", err)
	}
	if !samePaths(strings.Fields(staged), expectedPaths) {
		return "", invalidSession(session.ID, "commit_path_mismatch", "Staging included paths outside the task's claims or missed a claimed path.")
	}
	message := deterministicCommitMessage(member)
	if _, err := gitOutput(p.Root, "commit", "-m", message); err != nil {
		return "", invalidSession(session.ID, "git_commit_failed", fmt.Sprintf("Commit for task %s failed: %v", member.id, err))
	}
	sha, err := gitOutput(p.Root, "rev-parse", "HEAD")
	if err != nil {
		return "", sessionInternal(session.ID, "read task commit", err)
	}
	actualParent, err := gitOutput(p.Root, "rev-parse", "HEAD^")
	if err != nil || actualParent != parent {
		return "", invalidSession(session.ID, "commit_parent_mismatch", "Task commit did not advance HEAD by exactly one expected commit.")
	}
	committed, err := gitOutput(p.Root, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if err != nil {
		return "", sessionInternal(session.ID, "inspect task commit paths", err)
	}
	if !samePaths(strings.Fields(committed), expectedPaths) {
		return "", invalidSession(session.ID, "commit_path_mismatch", "Task commit does not contain exactly its claimed paths.")
	}
	return sha, nil
}

func deterministicCommitMessage(member finalizationMember) string {
	return fmt.Sprintf("Bandmaster task %s: %s\n\nIntent: %s\nBandmaster-Task-ID: %s\n", member.id, member.title, member.intent, member.id)
}

func samePaths(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	sort.Strings(actual)
	expected = append([]string(nil), expected...)
	sort.Strings(expected)
	for i := range actual {
		if actual[i] != expected[i] {
			return false
		}
	}
	return true
}

func (p *Project) runFinalValidation(db *sql.DB, session Session, batchID string) *Error {
	config, _, projectError := p.readApprovedConfiguration(db)
	if projectError != nil {
		projectError.SessionID = session.ID
		return projectError
	}
	commands, projectError := loadOfficialValidationCommands(db, session.ID, batchID, config)
	if projectError != nil {
		return projectError
	}
	attempt, projectError := beginFinalValidationAttempt(db, session.ID, batchID)
	if projectError != nil {
		return projectError
	}
	for index, command := range commands {
		run := p.runOfficialValidationCommand(attempt, int64(index+1), command)
		if projectError := persistValidationRun(db, session.ID, batchID, run); projectError != nil {
			return projectError
		}
		if run.Status != "passed" {
			_ = finishValidationAttempt(db, session.ID, batchID, attempt, "failed")
			return validationFailure(session.ID, run)
		}
	}
	return passFinalValidationAttempt(db, session.ID, batchID, attempt)
}

func beginFinalValidationAttempt(db *sql.DB, sessionID, batchID string) (int64, *Error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, sessionInternal(sessionID, "begin final validation", err)
	}
	defer tx.Rollback()
	var attempt int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(attempt), 0) + 1 FROM batch_validation_attempts WHERE batch_id = ?`, batchID).Scan(&attempt); err != nil {
		return 0, sessionInternal(sessionID, "allocate final validation attempt", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO batch_validation_attempts(batch_id, attempt, status, started_at) VALUES(?, ?, 'running', ?)`, batchID, attempt, now); err != nil {
		return 0, sessionInternal(sessionID, "record final validation attempt", err)
	}
	if _, err := tx.Exec(`UPDATE batches SET status = 'final_validating', updated_at = ? WHERE id = ? AND status = 'finalizing'`, now, batchID); err != nil {
		return 0, sessionInternal(sessionID, "begin final validating batch", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, sessionInternal(sessionID, "commit final validation start", err)
	}
	return attempt, nil
}

func passFinalValidationAttempt(db *sql.DB, sessionID, batchID string, attempt int64) *Error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE batch_validation_attempts SET status = 'passed', finished_at = ? WHERE batch_id = ? AND attempt = ? AND status = 'running'`, now, batchID, attempt); err != nil {
		return sessionInternal(sessionID, "record final validation success", err)
	}
	return nil
}

func beginFinalization(db *sql.DB, sessionID, batchID string) *Error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE batches SET status = 'finalizing', updated_at = ? WHERE id = ? AND status = 'finalizing'`, now, batchID); err != nil {
		return sessionInternal(sessionID, "begin finalization", err)
	}
	return nil
}

func recordTaskCommit(db *sql.DB, sessionID, batchID, taskID, sha string) *Error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT OR REPLACE INTO task_commits(batch_id, task_id, commit_sha, committed_at) VALUES(?, ?, ?, ?)`, batchID, taskID, sha, now); err != nil {
		return sessionInternal(sessionID, "record task commit", err)
	}
	return nil
}

func (p *Project) completeFinalization(db *sql.DB, sessionID, batchID string) *Error {
	commit, err := gitOutput(p.Root, "rev-parse", "HEAD")
	if err != nil {
		return sessionInternal(sessionID, "read committed batch HEAD", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return sessionInternal(sessionID, "begin finalization completion", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE batches SET status = 'committed', updated_at = ? WHERE id = ? AND status = 'final_validating'`, now, batchID); err != nil {
		return sessionInternal(sessionID, "complete batch", err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET status = CASE WHEN (SELECT no_changes FROM task_submissions WHERE task_id = tasks.id) = 1 THEN 'no_op' ELSE 'committed' END, worker_identity = NULL, assignment_token = NULL, updated_at = ? WHERE id IN (SELECT task_id FROM batch_members WHERE batch_id = ?) AND status = 'submitted'`, now, batchID); err != nil {
		return sessionInternal(sessionID, "complete tasks", err)
	}
	if _, err := tx.Exec(`DELETE FROM task_diff_reviews WHERE task_id IN (SELECT task_id FROM batch_members WHERE batch_id = ?)`, batchID); err != nil {
		return sessionInternal(sessionID, "clear committed diff reviews", err)
	}
	if _, err := tx.Exec(`DELETE FROM submitted_snapshots WHERE task_id IN (SELECT task_id FROM batch_members WHERE batch_id = ?)`, batchID); err != nil {
		return sessionInternal(sessionID, "clear committed snapshots", err)
	}
	if _, err := tx.Exec(`DELETE FROM claims WHERE batch_id = ?`, batchID); err != nil {
		return sessionInternal(sessionID, "release committed claims", err)
	}
	if _, err := tx.Exec(`INSERT INTO batch_audit_events(session_id, batch_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'batch_committed', 'final_validating', 'committed', ?)`, sessionID, batchID, now); err != nil {
		return sessionInternal(sessionID, "audit committed batch", err)
	}
	if _, err := tx.Exec(`UPDATE sessions SET status = 'active', starting_commit = ?, updated_at = ? WHERE id = ? AND status = 'finalizing'`, commit, now, sessionID); err != nil {
		return sessionInternal(sessionID, "return session to active", err)
	}
	if err := tx.Commit(); err != nil {
		return sessionInternal(sessionID, "commit finalization completion", err)
	}
	return nil
}
