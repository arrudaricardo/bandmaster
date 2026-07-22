package project

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	var interrupted int
	if err := db.QueryRow(`SELECT COUNT(*) FROM finalization_journals WHERE batch_id = ? AND session_id = ?`, batchID, session.ID).Scan(&interrupted); err != nil {
		return Batch{}, sessionInternal(session.ID, "inspect interrupted finalization", err)
	}
	if interrupted != 0 {
		return Batch{}, invalidSession(session.ID, "finalization_recovery_required", "Interrupted finalization requires `bandmaster finalization recover --json`; do not reissue batch commit.")
	}
	if observations, scanError := p.scanRepository(db, session); scanError != nil {
		return Batch{}, scanError
	} else if len(observations) != 0 {
		if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
			return Batch{}, projectError
		}
		return Batch{}, integrityError(session.ID, observations[0])
	}

	tasks, projectError := finalizationTasks(db, session.ID, batchID)
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
	if projectError := recordFinalizationJournal(db, session.ID, batchID, branch, preBatchCommit, tasks); projectError != nil {
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
	for _, task := range tasks {
		if !task.changed {
			continue
		}
		sha, hookChanged, projectError := p.commitTask(session, batchID, task)
		if projectError != nil {
			return Batch{}, p.rollbackFinalization(db, session, batchID, branch, preBatchCommit, projectError)
		}
		if hookChanged {
			if projectError := recordHookChange(db, session.ID, task.id); projectError != nil {
				return Batch{}, p.rollbackFinalization(db, session, batchID, branch, preBatchCommit, projectError)
			}
		}
		if projectError := recordTaskCommit(db, session.ID, batchID, task.id, sha); projectError != nil {
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

func recordFinalizationJournal(db *sql.DB, sessionID, batchID, branch, preBatchCommit string, tasks []finalizationTask) *Error {
	plan := make([]string, 0, len(tasks))
	for _, task := range tasks {
		plan = append(plan, task.id)
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
	return p.rollbackFinalizationWithRecovery(db, session, batchID, branch, preBatchCommit, cause, nil)
}

func (p *Project) rollbackFinalizationWithRecovery(db *sql.DB, session Session, batchID, branch, preBatchCommit string, cause *Error, recovery *FinalizationRecoveryResult) *Error {
	failureStatus, statusErr := gitOutput(p.Root, "status", "--porcelain=v1")
	if statusErr != nil {
		return p.quarantineFinalizationRollback(db, session, batchID, "capture-failure-state", statusErr, cause)
	}
	committedPatchBytes, patchErr := gitBytes(p.Root, "diff", "--binary", preBatchCommit+"..HEAD")
	if patchErr != nil {
		return p.quarantineFinalizationRollback(db, session, batchID, "capture-provisional-commits", patchErr, cause)
	}
	committedPatch := string(committedPatchBytes)
	stashOutput, stashErr := gitOutput(p.Root, "stash", "push", "--include-untracked", "-m", "bandmaster finalization rollback")
	if stashErr != nil {
		return p.quarantineFinalizationRollback(db, session, batchID, "capture-edits", stashErr, cause)
	}
	stashed := !strings.Contains(stashOutput, "No local changes to save")
	if _, err := gitOutput(p.Root, "checkout", "--force", branch); err != nil {
		return p.quarantineFinalizationRollback(db, session, batchID, "restore-branch", err, cause)
	}
	if _, err := gitOutput(p.Root, "reset", "--hard", preBatchCommit); err != nil {
		return p.quarantineFinalizationRollback(db, session, batchID, "restore-head", err, cause)
	}
	if committedPatch != "" {
		patch, err := os.CreateTemp("", "bandmaster-finalization-*.patch")
		if err != nil {
			return p.quarantineFinalizationRollback(db, session, batchID, "persist-provisional-commits", err, cause)
		}
		patchName := patch.Name()
		if _, err := patch.WriteString(committedPatch); err != nil || patch.Close() != nil {
			_ = os.Remove(patchName)
			return p.quarantineFinalizationRollback(db, session, batchID, "persist-provisional-commits", err, cause)
		}
		defer os.Remove(patchName)
		command := exec.Command("git", "-C", p.Root, "apply", "--binary", patchName)
		if output, err := command.CombinedOutput(); err != nil {
			return p.quarantineFinalizationRollback(db, session, batchID, "restore-provisional-commits", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output))), cause)
		}
	}
	if stashed {
		if _, err := gitOutput(p.Root, "stash", "pop"); err != nil {
			return p.quarantineFinalizationRollback(db, session, batchID, "restore-edits", err, cause)
		}
	}
	// Stash restoration can recreate index entries for additions that were staged
	// by finalization before it failed. The pre-finalization index is known clean,
	// so restore it independently from the preserved working-tree contents.
	if os.Getenv("BANDMASTER_TEST_FAIL_ROLLBACK_AT") == "normalize-index" {
		return p.quarantineFinalizationRollback(db, session, batchID, "normalize-index", errors.New("injected rollback failure"), cause)
	}
	if _, err := gitOutput(p.Root, "reset", "--mixed", preBatchCommit); err != nil {
		return p.quarantineFinalizationRollback(db, session, batchID, "normalize-index", err, cause)
	}
	if index, err := gitOutput(p.Root, "diff", "--cached", "--name-only"); err != nil || index != "" {
		if err == nil {
			err = fmt.Errorf("restored index contains %q", index)
		}
		return p.quarantineFinalizationRollback(db, session, batchID, "verify-index", err, cause)
	}
	// Fault injection after every Git restoration invariant has passed proves
	// that durable recovery can resume from a clean repository even if recording
	// the rollback outcome itself is interrupted.
	if os.Getenv("BANDMASTER_TEST_FAIL_ROLLBACK_AT") == "after-normalize-index" {
		return p.quarantineFinalizationRollback(db, session, batchID, "after-normalize-index", errors.New("injected rollback bookkeeping failure"), cause)
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
	if recovery != nil {
		if projectError := recordFinalizationRecovery(tx, *recovery); projectError != nil {
			return projectError
		}
		if os.Getenv("BANDMASTER_TEST_FAIL_ROLLBACK_AT") == "before-recovery-commit" {
			return sessionInternal(session.ID, "commit explicit finalization recovery", errors.New("injected recovery audit failure"))
		}
	}
	if err := tx.Commit(); err != nil {
		return sessionInternal(session.ID, "commit finalization failure recovery", err)
	}
	return invalidSession(session.ID, "finalization_failed", fmt.Sprintf("Finalization was rolled back: %s", cause.Message))
}

func finalizationCauseIsIntegrity(cause *Error) bool {
	return cause.Code == "integrity_violation" || cause.Code == "finalization_dirty_worktree" || strings.HasPrefix(cause.Code, "commit_")
}

func (p *Project) quarantineFinalizationRollback(db *sql.DB, session Session, batchID, operation string, rollbackCause error, initiating *Error) *Error {
	rollbackError := &RollbackErrorDetail{Operation: operation, Message: rollbackCause.Error()}
	evidence := struct {
		InitiatingError *ErrorDetail         `json:"initiating_error,omitempty"`
		RollbackError   *RollbackErrorDetail `json:"rollback_error"`
	}{RollbackError: rollbackError}
	if initiating != nil {
		evidence.InitiatingError = &ErrorDetail{Code: initiating.Code, Message: initiating.Message}
	}
	observation := integrityObservation{Kind: "ambiguous_finalization_rollback", Path: ".git", BatchID: batchID, ObservedState: evidence}
	if projectError := p.persistIntegrityViolations(session, []integrityObservation{observation}); projectError != nil {
		return projectError
	}
	projectError := integrityError(session.ID, observation)
	projectError.InitiatingError = evidence.InitiatingError
	projectError.RollbackError = rollbackError
	return projectError
}

func (p *Project) quarantineFinalization(db *sql.DB, session Session, batchID, operation string, cause error) *Error {
	observation := integrityObservation{Kind: "ambiguous_finalization_rollback", Path: ".git", BatchID: batchID, ObservedState: map[string]string{"operation": operation, "error": cause.Error()}}
	if projectError := p.persistIntegrityViolations(session, []integrityObservation{observation}); projectError != nil {
		return projectError
	}
	return integrityError(session.ID, observation)
}

type finalizationTask struct {
	id, title, intent string
	order             int64
	changed           bool
	paths             []string
	manifest          []finalizationPath
}

type finalizationPath struct {
	path                string
	baseline, submitted capturedSnapshot
}

func finalizationTasks(db *sql.DB, sessionID, batchID string) ([]finalizationTask, *Error) {
	rows, err := db.Query(`SELECT task.id, task.title, task.intent, task.creation_order, submission.no_changes
		FROM batch_tasks batch_task JOIN tasks task ON task.id = batch_task.task_id
		JOIN task_submissions submission ON submission.task_id = task.id
		WHERE batch_task.batch_id = ? ORDER BY batch_task.task_order`, batchID)
	if err != nil {
		return nil, sessionInternal(sessionID, "read commit tasks", err)
	}
	defer rows.Close()
	var tasks []finalizationTask
	for rows.Next() {
		var task finalizationTask
		var noChanges int
		if err := rows.Scan(&task.id, &task.title, &task.intent, &task.order, &noChanges); err != nil {
			return nil, sessionInternal(sessionID, "read commit task", err)
		}
		task.changed = noChanges == 0
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, sessionInternal(sessionID, "read commit tasks", err)
	}
	if err := rows.Close(); err != nil {
		return nil, sessionInternal(sessionID, "close commit tasks", err)
	}
	for index := range tasks {
		pathRows, err := db.Query(`
			SELECT path,
				baseline_presence, baseline_type, baseline_content_hash, baseline_executable, baseline_content,
				submitted_presence, submitted_type, submitted_content_hash, submitted_executable, submitted_content
			FROM frozen_batch_paths
			WHERE batch_id = ? AND task_id = ?
			ORDER BY claim_order`, batchID, tasks[index].id)
		if err != nil {
			return nil, sessionInternal(sessionID, "read task frozen manifest", err)
		}
		for pathRows.Next() {
			var manifest finalizationPath
			var baselineHash, submittedHash sql.NullString
			var baselineExecutable, submittedExecutable int
			if err := pathRows.Scan(
				&manifest.path,
				&manifest.baseline.Presence, &manifest.baseline.Type, &baselineHash, &baselineExecutable, &manifest.baseline.content,
				&manifest.submitted.Presence, &manifest.submitted.Type, &submittedHash, &submittedExecutable, &manifest.submitted.content,
			); err != nil {
				pathRows.Close()
				return nil, sessionInternal(sessionID, "read task frozen manifest", err)
			}
			manifest.baseline.Path = manifest.path
			manifest.baseline.ContentHash = baselineHash.String
			manifest.baseline.Executable = baselineExecutable != 0
			manifest.submitted.Path = manifest.path
			manifest.submitted.ContentHash = submittedHash.String
			manifest.submitted.Executable = submittedExecutable != 0
			tasks[index].paths = append(tasks[index].paths, manifest.path)
			tasks[index].manifest = append(tasks[index].manifest, manifest)
		}
		if err := pathRows.Close(); err != nil {
			return nil, sessionInternal(sessionID, "close task frozen manifest", err)
		}
	}
	return tasks, nil
}

func (p *Project) commitTask(session Session, batchID string, task finalizationTask) (string, bool, *Error) {
	parent, err := gitOutput(p.Root, "rev-parse", "HEAD")
	if err != nil {
		return "", false, sessionInternal(session.ID, "read commit parent", err)
	}
	if len(task.paths) == 0 {
		return "", false, invalidSession(session.ID, "task_has_no_claims", "A changed submitted task has no claims.")
	}
	before := make([]capturedSnapshot, 0, len(task.manifest))
	expectedPaths := make([]string, 0, len(task.manifest))
	for _, manifest := range task.manifest {
		current, projectError := p.capturePath(manifest.path)
		if projectError != nil {
			return "", false, projectError
		}
		current.Path = manifest.path
		if !snapshotsEqual(current.PathSnapshot, manifest.submitted.PathSnapshot) {
			return "", false, invalidSession(session.ID, "submission_snapshot_mismatch", fmt.Sprintf("Claimed path %s no longer matches its frozen submitted snapshot.", manifest.path))
		}
		before = append(before, current)
		if !snapshotsEqual(manifest.baseline.PathSnapshot, manifest.submitted.PathSnapshot) {
			expectedPaths = append(expectedPaths, manifest.path)
		}
	}
	if len(expectedPaths) == 0 {
		return "", false, invalidSession(session.ID, "submission_snapshot_mismatch", "A changed submission has no changes in its frozen manifest.")
	}
	args := append([]string{"add", "--"}, task.paths...)
	if _, err := gitOutput(p.Root, args...); err != nil {
		return "", false, sessionInternal(session.ID, "stage task changes", err)
	}
	staged, err := gitBytes(p.Root, "diff", "--cached", "--name-only", "--no-renames", "-z", "--")
	if err != nil {
		return "", false, sessionInternal(session.ID, "inspect staged task paths", err)
	}
	if !samePaths(splitNullPaths(staged), expectedPaths) {
		return "", false, invalidSession(session.ID, "commit_path_mismatch", "Staging included paths outside the task's claims or missed a claimed path.")
	}
	for _, manifest := range task.manifest {
		stagedSnapshot, projectError := p.captureIndexPath(manifest.path)
		if projectError != nil {
			return "", false, projectError
		}
		if !snapshotsEqual(stagedSnapshot.PathSnapshot, manifest.submitted.PathSnapshot) {
			return "", false, invalidSession(session.ID, "commit_snapshot_mismatch", fmt.Sprintf("Staged path %s does not match its frozen submitted snapshot.", manifest.path))
		}
	}
	message := deterministicCommitMessage(task)
	if _, err := gitOutput(p.Root, "commit", "-m", message); err != nil {
		return "", false, invalidSession(session.ID, "git_commit_failed", fmt.Sprintf("Commit for task %s failed: %v", task.id, err))
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
	if message, err := gitOutput(p.Root, "log", "-1", "--format=%B", "HEAD"); err != nil || message != deterministicCommitMessage(task) {
		return "", false, invalidSession(session.ID, "commit_message_mismatch", "Task commit message differs from the deterministic Bandmaster message.")
	}
	if index, err := gitOutput(p.Root, "diff", "--cached", "--name-only"); err != nil || index != "" {
		return "", false, invalidSession(session.ID, "commit_index_mismatch", "Task commit left staged changes in the index.")
	}
	if dirty, err := gitOutput(p.Root, append([]string{"diff", "--name-only", "--"}, task.paths...)...); err != nil || dirty != "" {
		return "", false, invalidSession(session.ID, "commit_claim_dirty", "Task commit left claimed changes unstaged or dirty.")
	}
	committed, err := gitBytes(p.Root, "diff-tree", "--no-commit-id", "--name-only", "--no-renames", "-r", "-z", "HEAD")
	if err != nil {
		return "", false, sessionInternal(session.ID, "inspect task commit paths", err)
	}
	if !samePaths(splitNullPaths(committed), expectedPaths) {
		return "", false, invalidSession(session.ID, "commit_path_mismatch", "Task commit does not contain exactly its claimed paths.")
	}
	after, projectError := p.captureFinalizationPaths(task.paths)
	if projectError != nil {
		return "", false, projectError
	}
	return sha, !finalizationSnapshotsEqual(before, after), nil
}

func splitNullPaths(output []byte) []string {
	if len(output) == 0 {
		return nil
	}
	records := strings.Split(string(output), "\x00")
	if records[len(records)-1] == "" {
		records = records[:len(records)-1]
	}
	return records
}

func (p *Project) captureIndexPath(path string) (capturedSnapshot, *Error) {
	return p.captureGitPath("index", path, "--literal-pathspecs", "ls-files", "--stage", "-z", "--", path)
}

func (p *Project) captureGitPath(source, path string, args ...string) (capturedSnapshot, *Error) {
	record, err := gitBytes(p.Root, args...)
	if err != nil {
		return capturedSnapshot{}, internal("inspect Git "+source+" path", err)
	}
	if len(record) == 0 {
		return capturedSnapshot{Path: path, PathSnapshot: PathSnapshot{Presence: "absent", Type: "absent"}}, nil
	}
	records := splitNullPaths(record)
	if len(records) != 1 {
		return capturedSnapshot{}, internal("inspect Git "+source+" path", fmt.Errorf("expected one entry for %s, found %d", path, len(records)))
	}
	tab := strings.IndexByte(records[0], '\t')
	if tab < 0 || records[0][tab+1:] != path {
		return capturedSnapshot{}, internal("parse Git "+source+" path", fmt.Errorf("unexpected entry for %s", path))
	}
	fields := strings.Fields(records[0][:tab])
	if len(fields) < 3 {
		return capturedSnapshot{}, internal("parse Git "+source+" path", fmt.Errorf("unexpected metadata for %s", path))
	}
	mode, objectID := fields[0], fields[2]
	if source == "index" {
		objectID = fields[1]
	}
	content, err := gitBytes(p.Root, "cat-file", "blob", objectID)
	if err != nil {
		return capturedSnapshot{}, internal("read Git "+source+" path", err)
	}
	digest := sha256.Sum256(content)
	snapshot := capturedSnapshot{Path: path, content: content, PathSnapshot: PathSnapshot{Presence: "present", ContentHash: "sha256:" + hex.EncodeToString(digest[:])}}
	switch mode {
	case "100644":
		snapshot.Type = "regular_file"
	case "100755":
		snapshot.Type = "regular_file"
		snapshot.Executable = true
	case "120000":
		snapshot.Type = "symlink"
	default:
		return capturedSnapshot{}, invalid("unsupported_claim_path", fmt.Sprintf("Git %s path %s has unsupported mode %s.", source, path, mode))
	}
	return snapshot, nil
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

func deterministicCommitMessage(task finalizationTask) string {
	return fmt.Sprintf("Bandmaster task %s: %s\n\nIntent: %s\nBandmaster-Task-ID: %s\n", task.id, task.title, task.intent, task.id)
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
	if _, err := tx.Exec(`UPDATE tasks SET status = CASE WHEN (SELECT no_changes FROM task_submissions WHERE task_id = tasks.id) = 1 THEN 'no_op' ELSE 'committed' END, agent_identity = NULL, assignment_token = NULL, updated_at = ? WHERE id IN (SELECT task_id FROM batch_tasks WHERE batch_id = ?) AND status = 'submitted'`, now, batchID); err != nil {
		return sessionInternal(sessionID, "complete tasks", err)
	}
	if _, err := tx.Exec(`DELETE FROM task_diff_reviews WHERE task_id IN (SELECT task_id FROM batch_tasks WHERE batch_id = ?)`, batchID); err != nil {
		return sessionInternal(sessionID, "clear committed diff reviews", err)
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
	rows, err := tx.Query(`SELECT DISTINCT dependent.id FROM tasks dependent JOIN task_dependencies dependency ON dependency.task_id = dependent.id JOIN batch_tasks prerequisite_task ON prerequisite_task.task_id = dependency.prerequisite_id WHERE dependent.session_id = ? AND dependent.status = 'planned' AND prerequisite_task.batch_id = ? ORDER BY dependent.creation_order`, sessionID, batchID)
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
