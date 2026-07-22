package integration_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

func TestIntegrityRecoveryRestoresFinalizingSessionAndIsIdempotent(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "recovered.txt"), "baseline\n")
	runGit(t, repo, "add", "recovered.txt")
	runGit(t, repo, "commit", "-m", "Add recovery fixture")
	started := successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Recover finalization", "--intent", "Restore a finalizing batch", "--expected-outcome", "Commit remains available")
	assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-recovery")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", "recovered.txt")
	writeFile(t, filepath.Join(repo, "recovered.txt"), "submitted\n")
	submitBatchTask(t, repo, task.Result.ID, assigned.Result.AssignmentToken)
	frozen := successfulBatchCommand(t, repo, "freeze")

	state, err := sql.Open("sqlite3", filepath.Join(repo, ".git", "bandmaster", "state.db"))
	if err != nil {
		t.Fatalf("open recovery fixture state: %v", err)
	}
	defer state.Close()
	tx, err := state.Begin()
	if err != nil {
		t.Fatalf("begin recovery fixture: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE sessions SET status = 'paused', updated_at = ? WHERE id = ?`, now, started.Result.ID); err != nil {
		t.Fatalf("pause corrupt fixture: %v", err)
	}
	if _, err := tx.Exec(`UPDATE batches SET status = 'quarantined', updated_at = ? WHERE id = ?`, now, frozen.Result.ID); err != nil {
		t.Fatalf("quarantine fixture batch: %v", err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET status = 'quarantined', updated_at = ? WHERE id = ?`, now, task.Result.ID); err != nil {
		t.Fatalf("quarantine fixture task: %v", err)
	}
	result, err := tx.Exec(`INSERT INTO integrity_violations(session_id, kind, path, observed_state_json, detected_at) VALUES(?, 'fixture_corruption', '', '{}', ?)`, started.Result.ID, now)
	if err != nil {
		t.Fatalf("record fixture violation: %v", err)
	}
	violationID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read fixture violation: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO integrity_quarantines(violation_id, batch_id, previous_status) VALUES(?, ?, 'finalizing')`, violationID, frozen.Result.ID); err != nil {
		t.Fatalf("record fixture batch quarantine: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO integrity_quarantines(violation_id, task_id, previous_status) VALUES(?, ?, 'submitted')`, violationID, task.Result.ID); err != nil {
		t.Fatalf("record fixture task quarantine: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit recovery fixture: %v", err)
	}

	recovered := successfulIntegrityRecovery(t, repo, "restored finalization fixture")
	if recovered.Result.Status != "finalizing" {
		t.Fatalf("recovered session status = %s, want finalizing", recovered.Result.Status)
	}
	if batch := successfulBatchCommand(t, repo, "inspect"); batch.Result.Status != "finalizing" {
		t.Fatalf("recovered batch status = %s, want finalizing", batch.Result.Status)
	}
	if restoredTask := successfulTaskCommand(t, repo, "inspect", task.Result.ID); restoredTask.Result.Status != "submitted" {
		t.Fatalf("recovered task status = %s, want submitted", restoredTask.Result.Status)
	}

	retried := successfulIntegrityRecovery(t, repo, "retry restored finalization fixture")
	if retried.Result.Status != "finalizing" || len(retried.Result.AuditHistory) != len(recovered.Result.AuditHistory) {
		t.Fatalf("recovery retry was not stable: first=%+v retry=%+v", recovered.Result, retried.Result)
	}
	commit := runBandmaster(t, repo, "batch", "commit", "--json")
	if commit.exitCode != 0 {
		response := decodeBatchResponse(t, commit.stdout)
		if response.Error.Code == "incompatible_session_batch_state" || response.Error.Code == "session_not_finalizing" || response.Error.Code == "batch_not_validated" {
			t.Fatalf("documented next command rejected recovered pair: %+v", response.Error)
		}
	}
}

func TestMutationRejectsIncompatibleSessionBatchPairWithoutDurableChanges(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "incompatible.txt"), "baseline\n")
	runGit(t, repo, "add", "incompatible.txt")
	runGit(t, repo, "commit", "-m", "Add incompatible state fixture")
	started := successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Existing task", "--intent", "Retain state", "--expected-outcome", "Rejected mutation is atomic")
	assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-incompatible")
	claimed := successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", "incompatible.txt")

	state, err := sql.Open("sqlite3", filepath.Join(repo, ".git", "bandmaster", "state.db"))
	if err != nil {
		t.Fatalf("open incompatible fixture state: %v", err)
	}
	if _, err := state.Exec(`UPDATE batches SET status = 'finalizing' WHERE id = ?`, claimed.Result.BatchID); err != nil {
		state.Close()
		t.Fatalf("create incompatible fixture: %v", err)
	}
	if err := state.Close(); err != nil {
		t.Fatalf("close incompatible fixture state: %v", err)
	}

	failed := runBandmaster(t, repo, "task", "create", "--title", "Must not exist", "--intent", "Reject corrupt pair", "--expected-outcome", "No durable mutation", "--json")
	assertTaskError(t, failed, 3, "incompatible_session_batch_state", false)
	after := successfulSessionCommand(t, repo, "inspect")
	if after.Result.Status != "active" || len(after.Result.AuditHistory) != len(started.Result.AuditHistory) {
		t.Fatalf("rejected mutation changed session state: before=%+v after=%+v", started.Result, after.Result)
	}
	if batch := successfulBatchCommand(t, repo, "inspect", claimed.Result.BatchID); batch.Result.Status != "finalizing" {
		t.Fatalf("rejected mutation changed batch state: %+v", batch.Result)
	}
	if existing := successfulTaskCommand(t, repo, "inspect", task.Result.ID); existing.Result.Status != "editing" {
		t.Fatalf("rejected mutation changed task state: %+v", existing.Result)
	}
}

func TestIntegrityRecoveryRestoresEveryInterruptedBatchPair(t *testing.T) {
	tests := []struct {
		previousBatch string
		wantSession   string
		wantBatch     string
	}{
		{previousBatch: "frozen", wantSession: "finalizing", wantBatch: "frozen"},
		{previousBatch: "validating", wantSession: "finalizing", wantBatch: "frozen"},
		{previousBatch: "finalizing", wantSession: "finalizing", wantBatch: "finalizing"},
		{previousBatch: "final_validating", wantSession: "finalizing", wantBatch: "finalizing"},
		{previousBatch: "repair_pending", wantSession: "active", wantBatch: "repair_pending"},
	}
	for _, test := range tests {
		t.Run(test.previousBatch, func(t *testing.T) {
			repo := approvedCleanRepository(t)
			started := successfulSessionCommand(t, repo, "start")
			batchID := "batch_recovery_" + test.previousBatch
			state, err := sql.Open("sqlite3", filepath.Join(repo, ".git", "bandmaster", "state.db"))
			if err != nil {
				t.Fatalf("open recovery state: %v", err)
			}
			tx, err := state.Begin()
			if err != nil {
				t.Fatalf("begin recovery state: %v", err)
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := tx.Exec(`INSERT INTO batches(id, session_id, creation_order, base_branch, base_commit, status, created_at, updated_at) VALUES(?, ?, 1, ?, ?, 'quarantined', ?, ?)`, batchID, started.Result.ID, started.Result.StartingBranch, started.Result.StartingCommit, now, now); err != nil {
				t.Fatalf("insert quarantined batch: %v", err)
			}
			if _, err := tx.Exec(`UPDATE sessions SET status = 'paused', updated_at = ? WHERE id = ?`, now, started.Result.ID); err != nil {
				t.Fatalf("pause recovery fixture: %v", err)
			}
			result, err := tx.Exec(`INSERT INTO integrity_violations(session_id, kind, path, observed_state_json, detected_at) VALUES(?, 'fixture_corruption', '', '{}', ?)`, started.Result.ID, now)
			if err != nil {
				t.Fatalf("insert recovery violation: %v", err)
			}
			violationID, err := result.LastInsertId()
			if err != nil {
				t.Fatalf("read recovery violation: %v", err)
			}
			if _, err := tx.Exec(`INSERT INTO integrity_quarantines(violation_id, batch_id, previous_status) VALUES(?, ?, ?)`, violationID, batchID, test.previousBatch); err != nil {
				t.Fatalf("insert batch quarantine: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit recovery fixture: %v", err)
			}
			if err := state.Close(); err != nil {
				t.Fatalf("close recovery state: %v", err)
			}

			recovered := successfulIntegrityRecovery(t, repo, "restore "+test.previousBatch+" fixture")
			if recovered.Result.Status != test.wantSession {
				t.Fatalf("recovered session = %s, want %s", recovered.Result.Status, test.wantSession)
			}
			batch := successfulBatchCommand(t, repo, "inspect", batchID)
			if batch.Result.Status != test.wantBatch {
				t.Fatalf("recovered batch = %s, want %s", batch.Result.Status, test.wantBatch)
			}
		})
	}
}

func TestMonitorDetectsUnclaimedDriftAndRequiresAuditedRecovery(t *testing.T) {
	repo := approvedCleanRepository(t)
	started := successfulSessionCommand(t, repo, "start")
	driftPath := filepath.Join(repo, "unexpected.txt")
	writeFile(t, driftPath, "outside ownership\n")

	paused := waitForSessionStatus(t, repo, "paused")
	if paused.Result.Monitor == nil || paused.Result.Monitor.Status != "stopped" {
		t.Fatalf("monitor did not stop after quarantining the session: %+v", paused.Result.Monitor)
	}
	violation := unresolvedViolation(t, paused, "unclaimed_change", "unexpected.txt")
	if violation.ObservedState == nil || violation.DetectedAt == "" {
		t.Fatalf("integrity evidence is incomplete: %+v", violation)
	}
	assertIntegrityAudit(t, paused, "unclaimed_change", "unexpected.txt")

	blocked := runBandmaster(t, repo, "task", "create", "--title", "Blocked", "--intent", "Must not mutate", "--expected-outcome", "Recovery first", "--json")
	assertTaskError(t, blocked, 4, "unclaimed_change", false)
	failedRecovery := runBandmaster(t, repo, "integrity", "recover", "--confirmation", "inspected unexpected.txt", "--json")
	assertIntegrityError(t, failedRecovery, 3, "integrity_not_restored")

	if err := os.Remove(driftPath); err != nil {
		t.Fatalf("restore repository: %v", err)
	}
	recovered := successfulIntegrityRecovery(t, repo, "removed the unowned file after inspection")
	if recovered.Result.Status != "paused" || recovered.Result.IntegrityViolations[0].RecoveredAt == "" || recovered.Result.IntegrityViolations[0].RecoveryConfirmation == "" {
		t.Fatalf("recovery was not explicitly audited: %+v", recovered.Result)
	}
	resumed := successfulSessionCommand(t, repo, "resume")
	if resumed.Result.Monitor == nil || resumed.Result.Monitor.Status != "healthy" || resumed.Result.Monitor.Generation != started.Result.Monitor.Generation+1 || resumed.Result.Monitor.ProcessIdentity == started.Result.Monitor.ProcessIdentity {
		t.Fatalf("resume did not start a fresh healthy monitor after a full scan: before=%+v after=%+v", started.Result.Monitor, resumed.Result.Monitor)
	}
}

func TestSubmittedPathDriftQuarantinesAndRestoresAffectedWork(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "submitted.txt"), "baseline\n")
	runGit(t, repo, "add", "submitted.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add submitted fixture")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Freeze work", "--intent", "Submit exact content", "--expected-outcome", "Later edits quarantine")
	assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-submitted-drift")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", "submitted.txt")
	writeFile(t, filepath.Join(repo, "submitted.txt"), "submitted content\n")
	if result := runBandmaster(t, repo, "task", "diff", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--json"); result.exitCode != 0 {
		t.Fatalf("review submitted fixture: %+v", result)
	}
	successfulTaskCommand(t, repo, "submit", task.Result.ID,
		"--token", assigned.Result.AssignmentToken,
		"--behavior-changed", "Changed submitted content",
		"--key-decisions", "Keep exact snapshot",
		"--validation-expectations", "Content remains frozen",
		"--known-risks", "None",
	)

	writeFile(t, filepath.Join(repo, "submitted.txt"), "edited after submission\n")
	paused := waitForSessionStatus(t, repo, "paused")
	unresolvedViolation(t, paused, "submitted_path_drift", "submitted.txt")
	quarantined := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if quarantined.Result.Status != "quarantined" || len(quarantined.Result.Claims) != 1 || quarantined.Result.Claims[0].SubmittedSnapshot == nil {
		t.Fatalf("submitted owner was not quarantined with retained evidence: %+v", quarantined.Result)
	}

	writeFile(t, filepath.Join(repo, "submitted.txt"), "submitted content\n")
	successfulIntegrityRecovery(t, repo, "restored the frozen submitted snapshot")
	restored := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if restored.Result.Status != "submitted" {
		t.Fatalf("integrity recovery did not restore the task's prior state: %+v", restored.Result)
	}
}

func TestMonitorDeathPausesMutationAndResumeReplacesIt(t *testing.T) {
	repo := approvedCleanRepository(t)
	started := successfulSessionCommand(t, repo, "start")
	if err := syscall.Kill(started.Result.Monitor.ProcessID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill integrity monitor: %v", err)
	}

	mutation := runBandmaster(t, repo, "task", "create", "--title", "Unsafe mutation", "--intent", "Require monitoring", "--expected-outcome", "No mutation", "--json")
	assertTaskError(t, mutation, 4, "monitor_unhealthy", false)
	paused := successfulSessionCommand(t, repo, "inspect")
	unresolvedViolation(t, paused, "monitor_unhealthy", ".git/bandmaster/monitor")

	successfulIntegrityRecovery(t, repo, "confirmed the old monitor process is stopped")
	resumed := successfulSessionCommand(t, repo, "resume")
	if resumed.Result.Monitor.Generation != 2 || resumed.Result.Monitor.ProcessIdentity == started.Result.Monitor.ProcessIdentity || resumed.Result.Monitor.Status != "healthy" {
		t.Fatalf("unhealthy monitor was not safely replaced: before=%+v after=%+v", started.Result.Monitor, resumed.Result.Monitor)
	}
}

func TestHungMonitorMustBeTerminatedBeforeReplacement(t *testing.T) {
	repo := approvedCleanRepository(t)
	started := successfulSessionCommand(t, repo, "start")
	pid := started.Result.Monitor.ProcessID
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatalf("stop integrity monitor: %v", err)
	}
	time.Sleep(2200 * time.Millisecond)

	mutation := runBandmaster(t, repo, "task", "create", "--title", "Unsafe mutation", "--intent", "Detect a hung monitor", "--expected-outcome", "No mutation", "--json")
	assertTaskError(t, mutation, 4, "monitor_unhealthy", false)
	unconfirmed := runBandmaster(t, repo, "integrity", "recover", "--confirmation", "monitor is stopped but not terminated", "--json")
	assertIntegrityError(t, unconfirmed, 3, "monitor_termination_unconfirmed")

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill stopped integrity monitor: %v", err)
	}
	successfulIntegrityRecovery(t, repo, "proved the stopped monitor process was terminated")
	resumed := successfulSessionCommand(t, repo, "resume")
	if resumed.Result.Monitor.Generation != 2 || resumed.Result.Monitor.ProcessIdentity == started.Result.Monitor.ProcessIdentity {
		t.Fatalf("replacement monitor identity was not fenced: before=%+v after=%+v", started.Result.Monitor, resumed.Result.Monitor)
	}
}

func TestFailedManualPauseRequiresRecoveryBeforeMonitorReplacement(t *testing.T) {
	repo := approvedCleanRepository(t)
	started := successfulSessionCommand(t, repo, "start")
	pid := started.Result.Monitor.ProcessID
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatalf("stop integrity monitor: %v", err)
	}

	paused := runBandmaster(t, repo, "session", "pause", "--json")
	if paused.exitCode != 4 {
		t.Fatalf("pause with hung monitor exit = %d, want 4; stdout=%s stderr=%s", paused.exitCode, paused.stdout, paused.stderr)
	}
	pausedResponse := decodeSessionResponse(t, paused.stdout)
	if pausedResponse.Error.Code != "monitor_unhealthy" {
		t.Fatalf("pause error = %+v, want monitor_unhealthy", pausedResponse)
	}
	resume := runBandmaster(t, repo, "session", "resume", "--json")
	assertIntegrityError(t, resume, 3, "integrity_recovery_required")

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill stopped integrity monitor: %v", err)
	}
	successfulIntegrityRecovery(t, repo, "terminated the monitor that could not stop during pause")
	resumed := successfulSessionCommand(t, repo, "resume")
	if resumed.Result.Monitor.Generation != 2 || resumed.Result.Monitor.ProcessIdentity == started.Result.Monitor.ProcessIdentity {
		t.Fatalf("manual pause recovery did not fence replacement: before=%+v after=%+v", started.Result.Monitor, resumed.Result.Monitor)
	}
}

func TestIgnoredUntrackedPathsStayOutsideOwnershipWhileTrackedPathsRemainCovered(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "cache/\ntracked-ignored.txt\n")
	writeFile(t, filepath.Join(repo, "tracked-ignored.txt"), "tracked baseline\n")
	runGit(t, repo, "add", "-f", ".gitignore", "tracked-ignored.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add ignore fixtures")
	successfulSessionCommand(t, repo, "start")
	writeFile(t, filepath.Join(repo, "cache", "artifact.txt"), "ignored\n")

	task := successfulTaskCommand(t, repo, "create", "--title", "Ignore policy", "--intent", "Keep ignored output unowned", "--expected-outcome", "Ignored claims fail")
	assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-ignore")
	ignoredClaim := runBandmaster(t, repo, "task", "claim", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", "cache/artifact.txt", "--json")
	assertTaskError(t, ignoredClaim, 3, "ignored_untracked_path", false)

	writeFile(t, filepath.Join(repo, "tracked-ignored.txt"), "tracked drift\n")
	paused := waitForSessionStatus(t, repo, "paused")
	unresolvedViolation(t, paused, "unclaimed_change", "tracked-ignored.txt")
}

func TestAuthoritativeScansDetectGitControlStateDrift(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*testing.T, string)
		wantKinds map[string]string
	}{
		{
			name: "branch",
			mutate: func(t *testing.T, repo string) {
				runGit(t, repo, "checkout", "-b", "outside-session")
			},
			wantKinds: map[string]string{"branch_drift": ".git/HEAD"},
		},
		{
			name: "head and base",
			mutate: func(t *testing.T, repo string) {
				runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "--allow-empty", "-m", "Outside session")
			},
			wantKinds: map[string]string{"head_drift": ".git/HEAD", "base_drift": "refs/heads/main"},
		},
		{
			name: "index",
			mutate: func(t *testing.T, repo string) {
				writeFile(t, filepath.Join(repo, "staged.txt"), "staged outside Bandmaster\n")
				runGit(t, repo, "add", "staged.txt")
			},
			wantKinds: map[string]string{"index_drift": ".git/index"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := approvedCleanRepository(t)
			successfulSessionCommand(t, repo, "start")
			test.mutate(t, repo)
			paused := waitForSessionStatus(t, repo, "paused")
			for kind, path := range test.wantKinds {
				unresolvedViolation(t, paused, kind, path)
				assertIntegrityAudit(t, paused, kind, path)
			}
		})
	}
}

func assertIntegrityAudit(t *testing.T, response sessionResponse, kind, path string) {
	t.Helper()
	for _, event := range response.Result.AuditHistory {
		if event.Event == "integrity_violation_observed" && event.IntegrityViolationID != 0 && event.IntegrityKind == kind && event.IntegrityPath == path && len(event.ObservedState) != 0 && event.OccurredAt != "" {
			return
		}
	}
	t.Fatalf("missing append-only integrity audit for %s at %s: %+v", kind, path, response.Result.AuditHistory)
}

type integrityViolationView struct {
	ObservedState []byte
	DetectedAt    string
}

func unresolvedViolation(t *testing.T, response sessionResponse, kind, path string) integrityViolationView {
	t.Helper()
	for _, violation := range response.Result.IntegrityViolations {
		if violation.Kind == kind && violation.Path == path && violation.RecoveredAt == "" {
			return integrityViolationView{ObservedState: violation.ObservedState, DetectedAt: violation.DetectedAt}
		}
	}
	t.Fatalf("missing unresolved %s violation for %q: %+v", kind, path, response.Result.IntegrityViolations)
	return integrityViolationView{}
}

func waitForSessionStatus(t *testing.T, repo, status string) sessionResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response := successfulSessionCommand(t, repo, "inspect")
		if response.Result.Status == status {
			return response
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("session did not reach %s", status)
	return sessionResponse{}
}

func successfulIntegrityRecovery(t *testing.T, repo, confirmation string) sessionResponse {
	t.Helper()
	result := runBandmaster(t, repo, "integrity", "recover", "--confirmation", confirmation, "--json")
	if result.exitCode != 0 {
		t.Fatalf("integrity recovery exit code = %d, stdout = %s, stderr = %s", result.exitCode, result.stdout, result.stderr)
	}
	return decodeSessionResponse(t, result.stdout)
}

func assertIntegrityError(t *testing.T, result commandResult, exitCode int, code string) {
	t.Helper()
	if result.exitCode != exitCode {
		t.Fatalf("integrity command exit code = %d, want %d; stdout = %s; stderr = %s", result.exitCode, exitCode, result.stdout, result.stderr)
	}
	response := decodeSessionResponse(t, result.stdout)
	if response.Success || response.Error.Code != code {
		t.Fatalf("integrity error = %+v, want %s", response, code)
	}
}
