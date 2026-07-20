package project

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	if recovered, projectError := p.recoverInterruptedFinalization(db, session, batchID); projectError != nil {
		return Batch{}, projectError
	} else if recovered {
		return Batch{}, invalidSession(session.ID, "finalization_recovered", "Interrupted finalization was rolled back; repair and validate the batch before retrying.")
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
	branch, err := gitOutput(p.Root, "branch", "--show-current")
	if err != nil {
		return Batch{}, sessionInternal(session.ID, "read finalization branch", err)
	}
	preBatchCommit, err := gitOutput(p.Root, "rev-parse", "HEAD")
	if err != nil {
		return Batch{}, sessionInternal(session.ID, "read pre-finalization commit", err)
	}
	if projectError := recordFinalizationJournal(db, session.ID, batchID, branch, preBatchCommit, members); projectError != nil {
		return Batch{}, projectError
	}
	crashFinalizationForTest("prepared")
	if projectError := beginFinalization(db, session.ID, batchID); projectError != nil {
		return Batch{}, projectError
	}
	if projectError := updateFinalizationJournalStep(db, session.ID, batchID, "committing"); projectError != nil {
		return Batch{}, p.rollbackFinalization(db, session, batchID, branch, preBatchCommit, projectError)
	}
	crashFinalizationForTest("committing")
	for _, member := range members {
		if !member.changed {
			continue
		}
		sha, hookChanged, projectError := p.commitTask(session, batchID, member)
		if projectError != nil {
			return Batch{}, p.rollbackFinalization(db, session, batchID, branch, preBatchCommit, projectError)
		}
		if hookChanged {
			if projectError := recordHookChange(db, session.ID, member.id); projectError != nil {
				return Batch{}, p.rollbackFinalization(db, session, batchID, branch, preBatchCommit, projectError)
			}
		}
		if projectError := recordTaskCommit(db, session.ID, batchID, member.id, sha); projectError != nil {
			return Batch{}, p.rollbackFinalization(db, session, batchID, branch, preBatchCommit, projectError)
		}
	}
	if output, err := gitOutput(p.Root, "status", "--porcelain=v1"); err != nil {
		return Batch{}, sessionInternal(session.ID, "verify committed worktree", err)
	} else if output != "" {
		return Batch{}, p.rollbackFinalization(db, session, batchID, branch, preBatchCommit, invalidSession(session.ID, "finalization_dirty_worktree", "Task commits left Git-visible changes in the worktree or index."))
	}
	if projectError := updateFinalizationJournalStep(db, session.ID, batchID, "validating"); projectError != nil {
		return Batch{}, p.rollbackFinalization(db, session, batchID, branch, preBatchCommit, projectError)
	}
	crashFinalizationForTest("validating")
	if projectError := p.runFinalValidation(db, session, batchID); projectError != nil {
		return Batch{}, p.rollbackFinalization(db, session, batchID, branch, preBatchCommit, projectError)
	}
	if projectError := p.completeFinalization(db, session.ID, batchID); projectError != nil {
		return Batch{}, p.rollbackFinalization(db, session, batchID, branch, preBatchCommit, projectError)
	}
	if projectError := p.StartIntegrityMonitor(session.ID); projectError != nil {
		return Batch{}, projectError
	}
	return inspectBatch(db, batchID)
}

// rollbackFinalization preserves every observed worktree edit while returning the
// branch and index to their pre-finalization state. A failed preservation is an
// ambiguous rollback, so it quarantines rather than offering unsafe repair.
// crashFinalizationForTest is a deliberately narrow subprocess fault-injection seam.
// It exits only when explicitly requested by an integration test.
func crashFinalizationForTest(step string) {
	if os.Getenv("BANDMASTER_TEST_CRASH_FINALIZATION_AT") == step {
		os.Exit(97)
	}
}

type finalizationJournal struct {
	Branch, PreBatchCommit string
}

// recoverInterruptedFinalization accepts only a journaled branch and a HEAD that
// is either the pre-batch commit or the final recorded task commit. It rolls the
// known state back rather than guessing whether a partially completed batch is safe.
func (p *Project) recoverInterruptedFinalization(db *sql.DB, session Session, batchID string) (bool, *Error) {
	var journal finalizationJournal
	err := db.QueryRow(`SELECT expected_branch, pre_batch_commit FROM finalization_journals WHERE batch_id = ? AND session_id = ?`, batchID, session.ID).Scan(&journal.Branch, &journal.PreBatchCommit)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, sessionInternal(session.ID, "read finalization journal", err)
	}
	branch, branchErr := gitOutput(p.Root, "branch", "--show-current")
	head, headErr := gitOutput(p.Root, "rev-parse", "HEAD")
	index, indexErr := gitOutput(p.Root, "diff", "--cached", "--name-only")
	if branchErr != nil || headErr != nil || branch != journal.Branch {
		return false, p.quarantineFinalization(db, session, batchID, "verify interrupted finalization branch", fmt.Errorf("branch=%q head=%q", branch, head))
	}
	if indexErr != nil || index != "" {
		return false, p.quarantineFinalization(db, session, batchID, "verify interrupted finalization index", fmt.Errorf("staged paths %q", index))
	}
	if running, hookError := p.finalizationHookRunning(); hookError != nil || running {
		return false, p.quarantineFinalization(db, session, batchID, "verify interrupted finalization hook", fmt.Errorf("hook running=%t: %v", running, hookError))
	}
	monitor, monitorError := inspectLatestMonitor(db, session.ID)
	if monitorError != nil || !monitorStopped(monitor) {
		return false, p.quarantineFinalization(db, session, batchID, "verify interrupted finalization monitor", fmt.Errorf("monitor is not provably stopped: %v", monitorError))
	}
	rows, err := db.Query(`SELECT committed.commit_sha FROM task_commits committed JOIN tasks task ON task.id = committed.task_id WHERE committed.batch_id = ? ORDER BY task.creation_order`, batchID)
	if err != nil {
		return false, sessionInternal(session.ID, "read journaled task commits", err)
	}
	var commits []string
	for rows.Next() {
		var commit string
		if err := rows.Scan(&commit); err != nil {
			rows.Close()
			return false, sessionInternal(session.ID, "read journaled task commit", err)
		}
		commits = append(commits, commit)
	}
	if err := rows.Close(); err != nil {
		return false, sessionInternal(session.ID, "close journaled task commits", err)
	}
	if head != journal.PreBatchCommit && (len(commits) == 0 || head != commits[len(commits)-1]) {
		return false, p.quarantineFinalization(db, session, batchID, "verify interrupted finalization HEAD", fmt.Errorf("unexpected HEAD %s", head))
	}
	cause := invalidSession(session.ID, "finalization_interrupted", "A previous finalization process stopped before completion.")
	return true, p.rollbackFinalization(db, session, batchID, journal.Branch, journal.PreBatchCommit, cause)
}

// finalizationHookRunning refuses recovery while a hook from this repository may
// still mutate the worktree. Failure to inspect processes is not safe proof.
func (p *Project) finalizationHookRunning() (bool, error) {
	hooks, err := gitOutput(p.Root, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err != nil {
		return false, err
	}
	output, err := exec.Command("ps", "-axo", "command=").Output()
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, hooks+string(os.PathSeparator)) {
			return true, nil
		}
	}
	return false, nil
}

func recordFinalizationJournal(db *sql.DB, sessionID, batchID, branch, preBatchCommit string, members []finalizationMember) *Error {
	plan := make([]string, 0, len(members))
	for _, member := range members {
		plan = append(plan, member.id)
	}
	encodedPlan, err := json.Marshal(plan)
	if err != nil {
		return sessionInternal(sessionID, "encode finalization commit plan", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO finalization_journals(batch_id, session_id, expected_branch, pre_batch_commit, commit_plan_json, step, created_at, updated_at) VALUES(?, ?, ?, ?, ?, 'prepared', ?, ?)`, batchID, sessionID, branch, preBatchCommit, encodedPlan, now, now); err != nil {
		return sessionInternal(sessionID, "record finalization journal", err)
	}
	return nil
}

func updateFinalizationJournalStep(db *sql.DB, sessionID, batchID, step string) *Error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE finalization_journals SET step = ?, updated_at = ? WHERE batch_id = ? AND session_id = ?`, step, now, batchID, sessionID); err != nil {
		return sessionInternal(sessionID, "update finalization journal", err)
	}
	return nil
}

func (p *Project) rollbackFinalization(db *sql.DB, session Session, batchID, branch, preBatchCommit string, cause *Error) *Error {
	failureStatus, statusErr := gitOutput(p.Root, "status", "--porcelain=v1")
	if statusErr != nil {
		return p.quarantineFinalization(db, session, batchID, "capture finalization failure state", statusErr)
	}
	committedPatchBytes, patchErr := gitBytes(p.Root, "diff", "--binary", preBatchCommit+"..HEAD")
	if patchErr != nil {
		return p.quarantineFinalization(db, session, batchID, "capture provisional finalization commits", patchErr)
	}
	committedPatch := string(committedPatchBytes)
	stashOutput, stashErr := gitOutput(p.Root, "stash", "push", "--include-untracked", "-m", "bandmaster finalization rollback")
	if stashErr != nil {
		return p.quarantineFinalization(db, session, batchID, "capture finalization edits", stashErr)
	}
	stashed := !strings.Contains(stashOutput, "No local changes to save")
	if _, err := gitOutput(p.Root, "checkout", "--force", branch); err != nil {
		return p.quarantineFinalization(db, session, batchID, "restore finalization branch", err)
	}
	if _, err := gitOutput(p.Root, "reset", "--hard", preBatchCommit); err != nil {
		return p.quarantineFinalization(db, session, batchID, "restore pre-finalization commit", err)
	}
	if committedPatch != "" {
		patch, err := os.CreateTemp("", "bandmaster-finalization-*.patch")
		if err != nil {
			return p.quarantineFinalization(db, session, batchID, "persist provisional finalization commits", err)
		}
		patchName := patch.Name()
		if _, err := patch.WriteString(committedPatch); err != nil || patch.Close() != nil {
			_ = os.Remove(patchName)
			return p.quarantineFinalization(db, session, batchID, "persist provisional finalization commits", err)
		}
		defer os.Remove(patchName)
		command := exec.Command("git", "-C", p.Root, "apply", "--binary", patchName)
		if output, err := command.CombinedOutput(); err != nil {
			return p.quarantineFinalization(db, session, batchID, "restore provisional finalization commits", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output))))
		}
	}
	if stashed {
		if _, err := gitOutput(p.Root, "stash", "pop"); err != nil {
			return p.quarantineFinalization(db, session, batchID, "restore finalization edits", err)
		}
	}
	if index, err := gitOutput(p.Root, "diff", "--cached", "--name-only"); err != nil || index != "" {
		if err == nil {
			err = fmt.Errorf("restored index contains %q", index)
		}
		return p.quarantineFinalization(db, session, batchID, "verify restored index", err)
	}
	if finalizationCauseIsIntegrity(cause) {
		observation := integrityObservation{Kind: "finalization_integrity_violation", Path: ".", BatchID: batchID, ObservedState: map[string]string{"cause": cause.Code, "status": failureStatus}}
		if projectError := p.persistIntegrityViolations(session, []integrityObservation{observation}); projectError != nil {
			return projectError
		}
		return integrityError(session.ID, observation)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := db.Begin()
	if err != nil {
		return sessionInternal(session.ID, "begin finalization failure recovery", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM task_commits WHERE batch_id = ?`, batchID); err != nil {
		return sessionInternal(session.ID, "clear rolled-back task commits", err)
	}
	if _, err := tx.Exec(`UPDATE batches SET status = 'repair_pending', updated_at = ? WHERE id = ? AND status IN ('finalizing', 'final_validating')`, now, batchID); err != nil {
		return sessionInternal(session.ID, "mark finalization repair pending", err)
	}
	if _, err := tx.Exec(`DELETE FROM finalization_journals WHERE batch_id = ?`, batchID); err != nil {
		return sessionInternal(session.ID, "clear finalization journal", err)
	}
	if _, err := tx.Exec(`UPDATE sessions SET status = 'active', updated_at = ? WHERE id = ? AND status = 'finalizing'`, now, session.ID); err != nil {
		return sessionInternal(session.ID, "return failed finalization session to active", err)
	}
	if _, err := tx.Exec(`INSERT INTO batch_audit_events(session_id, batch_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'finalization_rolled_back', 'finalizing', 'repair_pending', ?)`, session.ID, batchID, now); err != nil {
		return sessionInternal(session.ID, "audit finalization rollback", err)
	}
	if err := tx.Commit(); err != nil {
		return sessionInternal(session.ID, "commit finalization failure recovery", err)
	}
	return invalidSession(session.ID, "finalization_failed", fmt.Sprintf("Finalization was rolled back: %s", cause.Message))
}

func finalizationCauseIsIntegrity(cause *Error) bool {
	return cause.Code == "integrity_violation" || cause.Code == "finalization_dirty_worktree" || strings.HasPrefix(cause.Code, "commit_")
}

func (p *Project) quarantineFinalization(db *sql.DB, session Session, batchID, operation string, cause error) *Error {
	observation := integrityObservation{Kind: "ambiguous_finalization_rollback", Path: ".git", BatchID: batchID, ObservedState: map[string]string{"operation": operation, "error": cause.Error()}}
	if projectError := p.persistIntegrityViolations(session, []integrityObservation{observation}); projectError != nil {
		return projectError
	}
	return integrityError(session.ID, observation)
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

func (p *Project) commitTask(session Session, batchID string, member finalizationMember) (string, bool, *Error) {
	parent, err := gitOutput(p.Root, "rev-parse", "HEAD")
	if err != nil {
		return "", false, sessionInternal(session.ID, "read commit parent", err)
	}
	if len(member.paths) == 0 {
		return "", false, invalidSession(session.ID, "task_has_no_claims", "A changed submitted task has no claims.")
	}
	before, projectError := p.captureFinalizationPaths(member.paths)
	if projectError != nil {
		return "", false, projectError
	}
	changed, err := gitOutput(p.Root, append([]string{"diff", "--name-only", "--"}, member.paths...)...)
	if err != nil {
		return "", false, sessionInternal(session.ID, "inspect task changes", err)
	}
	expectedPaths := strings.Fields(changed)
	if len(expectedPaths) == 0 {
		return "", false, invalidSession(session.ID, "submission_snapshot_mismatch", "A changed submission has no current Git-visible changes.")
	}
	args := append([]string{"add", "--"}, member.paths...)
	if _, err := gitOutput(p.Root, args...); err != nil {
		return "", false, sessionInternal(session.ID, "stage task changes", err)
	}
	staged, err := gitOutput(p.Root, "diff", "--cached", "--name-only", "--")
	if err != nil {
		return "", false, sessionInternal(session.ID, "inspect staged task paths", err)
	}
	if !samePaths(strings.Fields(staged), expectedPaths) {
		return "", false, invalidSession(session.ID, "commit_path_mismatch", "Staging included paths outside the task's claims or missed a claimed path.")
	}
	message := deterministicCommitMessage(member)
	if _, err := gitOutput(p.Root, "commit", "-m", message); err != nil {
		return "", false, invalidSession(session.ID, "git_commit_failed", fmt.Sprintf("Commit for task %s failed: %v", member.id, err))
	}
	sha, err := gitOutput(p.Root, "rev-parse", "HEAD")
	if err != nil {
		return "", false, sessionInternal(session.ID, "read task commit", err)
	}
	actualParent, err := gitOutput(p.Root, "rev-parse", "HEAD^")
	if err != nil || actualParent != parent {
		return "", false, invalidSession(session.ID, "commit_parent_mismatch", "Task commit did not advance HEAD by exactly one expected commit.")
	}
	if branch, err := gitOutput(p.Root, "branch", "--show-current"); err != nil || branch != session.StartingBranch {
		return "", false, invalidSession(session.ID, "commit_branch_mismatch", "Task commit changed the finalization branch.")
	}
	if message, err := gitOutput(p.Root, "log", "-1", "--format=%B", "HEAD"); err != nil || message != deterministicCommitMessage(member) {
		return "", false, invalidSession(session.ID, "commit_message_mismatch", "Task commit message differs from the deterministic Bandmaster message.")
	}
	if index, err := gitOutput(p.Root, "diff", "--cached", "--name-only"); err != nil || index != "" {
		return "", false, invalidSession(session.ID, "commit_index_mismatch", "Task commit left staged changes in the index.")
	}
	if dirty, err := gitOutput(p.Root, append([]string{"diff", "--name-only", "--"}, member.paths...)...); err != nil || dirty != "" {
		return "", false, invalidSession(session.ID, "commit_claim_dirty", "Task commit left claimed changes unstaged or dirty.")
	}
	committed, err := gitOutput(p.Root, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if err != nil {
		return "", false, sessionInternal(session.ID, "inspect task commit paths", err)
	}
	if !samePaths(strings.Fields(committed), expectedPaths) {
		return "", false, invalidSession(session.ID, "commit_path_mismatch", "Task commit does not contain exactly its claimed paths.")
	}
	after, projectError := p.captureFinalizationPaths(member.paths)
	if projectError != nil {
		return "", false, projectError
	}
	return sha, !finalizationSnapshotsEqual(before, after), nil
}

func (p *Project) captureFinalizationPaths(paths []string) ([]capturedSnapshot, *Error) {
	snapshots := make([]capturedSnapshot, 0, len(paths))
	for _, path := range paths {
		snapshot, projectError := p.capturePath(path)
		if projectError != nil {
			return nil, projectError
		}
		snapshot.Path = path
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func finalizationSnapshotsEqual(before, after []capturedSnapshot) bool {
	if len(before) != len(after) {
		return false
	}
	for index := range before {
		if before[index].Path != after[index].Path || !snapshotsEqual(before[index].PathSnapshot, after[index].PathSnapshot) {
			return false
		}
	}
	return true
}

func recordHookChange(db *sql.DB, sessionID, taskID string) *Error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'hook_change_committed', 'submitted', 'submitted', ?)`, sessionID, taskID, now); err != nil {
		return sessionInternal(sessionID, "audit committed hook change", err)
	}
	return nil
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
		if status, err := gitOutput(p.Root, "status", "--porcelain=v1"); err != nil {
			return sessionInternal(session.ID, "inspect final validation worktree", err)
		} else if status != "" {
			observation := integrityObservation{Kind: "final_validation_dirty_worktree", Path: ".", BatchID: batchID, ObservedState: map[string]string{"status": status}}
			if projectError := p.persistIntegrityViolations(session, []integrityObservation{observation}); projectError != nil {
				return projectError
			}
			return integrityError(session.ID, observation)
		}
		run := p.runOfficialValidationCommand(attempt, int64(index+1), command)
		if status, err := gitOutput(p.Root, "status", "--porcelain=v1"); err != nil {
			return sessionInternal(session.ID, "inspect final validation worktree", err)
		} else if status != "" {
			run.Status = "integrity_violation"
			if projectError := persistValidationRun(db, session.ID, batchID, run); projectError != nil {
				return projectError
			}
			_ = finishValidationAttempt(db, session.ID, batchID, attempt, "integrity_violation")
			observation := integrityObservation{Kind: "final_validation_mutation", Path: ".", BatchID: batchID, ObservedState: map[string]string{"status": status}}
			if projectError := p.persistIntegrityViolations(session, []integrityObservation{observation}); projectError != nil {
				return projectError
			}
			return integrityError(session.ID, observation)
		}
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
	if projectError := advanceDependentTasks(tx, sessionID, batchID, now); projectError != nil {
		return projectError
	}
	if _, err := tx.Exec(`INSERT INTO batch_audit_events(session_id, batch_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'batch_committed', 'final_validating', 'committed', ?)`, sessionID, batchID, now); err != nil {
		return sessionInternal(sessionID, "audit committed batch", err)
	}
	if _, err := tx.Exec(`DELETE FROM finalization_journals WHERE batch_id = ?`, batchID); err != nil {
		return sessionInternal(sessionID, "clear completed finalization journal", err)
	}
	if _, err := tx.Exec(`UPDATE sessions SET status = 'active', starting_commit = ?, updated_at = ? WHERE id = ? AND status = 'finalizing'`, commit, now, sessionID); err != nil {
		return sessionInternal(sessionID, "return session to active", err)
	}
	if err := tx.Commit(); err != nil {
		return sessionInternal(sessionID, "commit finalization completion", err)
	}
	return nil
}

func advanceDependentTasks(tx *sql.Tx, sessionID, batchID, now string) *Error {
	rows, err := tx.Query(`SELECT DISTINCT dependent.id FROM tasks dependent JOIN task_dependencies dependency ON dependency.task_id = dependent.id JOIN batch_members prerequisite_member ON prerequisite_member.task_id = dependency.prerequisite_id WHERE dependent.session_id = ? AND dependent.status = 'planned' AND prerequisite_member.batch_id = ? ORDER BY dependent.creation_order`, sessionID, batchID)
	if err != nil {
		return sessionInternal(sessionID, "find dependent tasks", err)
	}
	defer rows.Close()
	var candidates []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return sessionInternal(sessionID, "read dependent task", err)
		}
		candidates = append(candidates, id)
	}
	if err := rows.Err(); err != nil {
		return sessionInternal(sessionID, "scan dependent tasks", err)
	}
	for _, id := range candidates {
		prerequisites, projectError := taskPrerequisites(tx, sessionID, id)
		if projectError != nil {
			return projectError
		}
		ready, projectError := taskReadiness(tx, prerequisites)
		if projectError != nil {
			return projectError
		}
		if ready != "ready" {
			continue
		}
		if _, err := tx.Exec(`UPDATE tasks SET status = 'ready', updated_at = ? WHERE id = ? AND status = 'planned'`, now, id); err != nil {
			return sessionInternal(sessionID, "advance dependent task", err)
		}
		if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'task_ready', 'planned', 'ready', ?)`, sessionID, id, now); err != nil {
			return sessionInternal(sessionID, "audit dependent task readiness", err)
		}
	}
	return nil
}
