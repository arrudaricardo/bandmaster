package integration_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type sessionResponse struct {
	SchemaVersion string `json:"schema_version"`
	Command       string `json:"command"`
	Success       bool   `json:"success"`
	SessionID     string `json:"session_id"`
	Result        struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		StartingBranch string `json:"starting_branch"`
		StartingCommit string `json:"starting_commit"`
		AuditHistory   []struct {
			Sequence             int64           `json:"sequence"`
			Event                string          `json:"event"`
			FromStatus           string          `json:"from_status,omitempty"`
			ToStatus             string          `json:"to_status"`
			IntegrityViolationID int64           `json:"integrity_violation_id,omitempty"`
			IntegrityKind        string          `json:"integrity_kind,omitempty"`
			IntegrityPath        string          `json:"integrity_path,omitempty"`
			ObservedState        json.RawMessage `json:"observed_state,omitempty"`
			OccurredAt           string          `json:"occurred_at"`
		} `json:"audit_history"`
		Monitor *struct {
			Generation      int64  `json:"generation"`
			ProcessID       int    `json:"process_id"`
			ProcessIdentity string `json:"process_identity"`
			ProcessStartID  string `json:"process_start_identity"`
			Status          string `json:"status"`
			HeartbeatAt     string `json:"heartbeat_at"`
			LastFullScanAt  string `json:"last_full_scan_at"`
		} `json:"monitor"`
		IntegrityViolations []struct {
			ID                   int64           `json:"id"`
			Kind                 string          `json:"kind"`
			Path                 string          `json:"path,omitempty"`
			ObservedState        json.RawMessage `json:"observed_state"`
			DetectedAt           string          `json:"detected_at"`
			RecoveredAt          string          `json:"recovered_at,omitempty"`
			RecoveryConfirmation string          `json:"recovery_confirmation,omitempty"`
		} `json:"integrity_violations"`
	} `json:"result"`
	Error struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

type abortPlanResponse struct {
	SchemaVersion string `json:"schema_version"`
	Command       string `json:"command"`
	Success       bool   `json:"success"`
	SessionID     string `json:"session_id"`
	Result        struct {
		SessionStatus string `json:"session_status"`
		Tasks         []struct {
			TaskID        string `json:"task_id"`
			CurrentStatus string `json:"current_status"`
			TargetStatus  string `json:"target_status"`
		} `json:"affected_tasks"`
		ActiveClaims []struct {
			TaskID string `json:"task_id"`
			Path   string `json:"path"`
		} `json:"active_claims"`
		PreservedArtifacts []struct {
			Kind  string `json:"kind"`
			Count int    `json:"count"`
		} `json:"preserved_artifacts"`
		Batches []struct {
			BatchID string `json:"batch_id"`
			Status  string `json:"status"`
		} `json:"batches"`
		Journals []struct {
			BatchID string `json:"batch_id"`
			Step    string `json:"step"`
		} `json:"journals"`
		Files    []string `json:"files"`
		Blockers []struct {
			Code string `json:"code"`
		} `json:"blockers"`
	} `json:"result"`
}

func TestSessionStartPersistsRepositoryBaselineAndAuditHistory(t *testing.T) {
	repo := approvedCleanRepository(t)
	startingCommit := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))

	started := runBandmaster(t, repo, "session", "start", "--json")
	if started.exitCode != 0 {
		t.Fatalf("session start exit code = %d, stdout = %s, stderr = %s", started.exitCode, started.stdout, started.stderr)
	}
	startResponse := decodeSessionResponse(t, started.stdout)
	if !startResponse.Success || startResponse.Command != "session start" || startResponse.SchemaVersion != "1" {
		t.Fatalf("unexpected start response: %+v", startResponse)
	}
	if startResponse.SessionID == "" || startResponse.Result.ID != startResponse.SessionID {
		t.Fatalf("session identity was not returned consistently: %+v", startResponse)
	}
	if startResponse.Result.Status != "active" || startResponse.Result.StartingBranch != "main" || startResponse.Result.StartingCommit != startingCommit {
		t.Fatalf("unexpected starting session: %+v", startResponse.Result)
	}
	if startResponse.Result.Monitor == nil || startResponse.Result.Monitor.Generation != 1 || startResponse.Result.Monitor.ProcessID <= 0 || startResponse.Result.Monitor.ProcessIdentity == "" || startResponse.Result.Monitor.ProcessStartID == "" || startResponse.Result.Monitor.Status != "healthy" || startResponse.Result.Monitor.HeartbeatAt == "" || startResponse.Result.Monitor.LastFullScanAt == "" {
		t.Fatalf("session did not start a healthy persisted monitor: %+v", startResponse.Result.Monitor)
	}

	inspected := runBandmaster(t, repo, "session", "inspect", "--json")
	if inspected.exitCode != 0 {
		t.Fatalf("session inspect exit code = %d, stdout = %s, stderr = %s", inspected.exitCode, inspected.stdout, inspected.stderr)
	}
	inspectResponse := decodeSessionResponse(t, inspected.stdout)
	if inspectResponse.Result.ID != startResponse.Result.ID || inspectResponse.Result.Status != "active" {
		t.Fatalf("fresh invocation did not observe the persisted session: %+v", inspectResponse)
	}
	if len(inspectResponse.Result.AuditHistory) != 1 || inspectResponse.Result.AuditHistory[0].Event != "session_started" || inspectResponse.Result.AuditHistory[0].ToStatus != "active" || inspectResponse.Result.AuditHistory[0].OccurredAt == "" {
		t.Fatalf("unexpected start audit history: %+v", inspectResponse.Result.AuditHistory)
	}
}

func TestClaimlessSessionCanPauseResumeAndFinish(t *testing.T) {
	repo := approvedCleanRepository(t)
	started := successfulSessionCommand(t, repo, "start")

	paused := successfulSessionCommand(t, repo, "pause")
	if paused.SessionID != started.SessionID || paused.Result.Status != "paused" {
		t.Fatalf("unexpected paused session: %+v", paused)
	}
	resumed := successfulSessionCommand(t, repo, "resume")
	if resumed.SessionID != started.SessionID || resumed.Result.Status != "active" {
		t.Fatalf("unexpected resumed session: %+v", resumed)
	}
	finished := successfulSessionCommand(t, repo, "finish")
	if finished.SessionID != started.SessionID || finished.Result.Status != "completed" {
		t.Fatalf("unexpected completed session: %+v", finished)
	}

	inspected := successfulSessionCommand(t, repo, "inspect")
	wantEvents := []string{"session_started", "session_paused", "session_resumed", "session_finalizing", "session_completed"}
	if len(inspected.Result.AuditHistory) != len(wantEvents) {
		t.Fatalf("audit event count = %d, want %d: %+v", len(inspected.Result.AuditHistory), len(wantEvents), inspected.Result.AuditHistory)
	}
	for index, wantEvent := range wantEvents {
		event := inspected.Result.AuditHistory[index]
		if event.Event != wantEvent || event.Sequence == 0 || event.OccurredAt == "" {
			t.Errorf("audit event %d = %+v, want %q with durable metadata", index, event, wantEvent)
		}
	}
	if inspected.Result.AuditHistory[1].FromStatus != "active" || inspected.Result.AuditHistory[1].ToStatus != "paused" || inspected.Result.AuditHistory[2].FromStatus != "paused" || inspected.Result.AuditHistory[2].ToStatus != "active" || inspected.Result.AuditHistory[3].FromStatus != "active" || inspected.Result.AuditHistory[3].ToStatus != "finalizing" || inspected.Result.AuditHistory[4].FromStatus != "finalizing" || inspected.Result.AuditHistory[4].ToStatus != "completed" {
		t.Fatalf("audit history does not describe transitions: %+v", inspected.Result.AuditHistory)
	}
}

func TestSessionAbortPreservesEditsAndRequiresTerminationConfirmation(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Abort work", "--intent", "Preserve edits", "--expected-outcome", "Quarantine ownership")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "abort-worker")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "preserved work\n")

	waiting := runBandmaster(t, repo, "session", "abort", "--json")
	if waiting.exitCode != 3 {
		t.Fatalf("abort without proof exit code = %d: %s", waiting.exitCode, waiting.stdout)
	}
	if response := decodeSessionResponse(t, waiting.stdout); response.Error.Code != "worker_termination_confirmation_required" {
		t.Fatalf("unexpected abort proof error: %+v", response)
	}
	if session := successfulSessionCommand(t, repo, "inspect"); session.Result.Status != "active" || session.Result.Monitor == nil || session.Result.Monitor.Status != "healthy" {
		t.Fatalf("abort without proof mutated session or monitor state: %+v", session.Result)
	}
	if taskState := successfulTaskCommand(t, repo, "inspect", task.Result.ID); taskState.Result.Status != "editing" || len(taskState.Result.Claims) != 1 {
		t.Fatalf("abort without proof mutated task ownership: %+v", taskState.Result)
	}

	abortResult := runBandmaster(t, repo, "session", "abort", "--termination-confirmation", "worker handle exited", "--json")
	if abortResult.exitCode != 0 {
		t.Fatalf("confirmed abort failed: %+v", abortResult)
	}
	aborted := decodeSessionResponse(t, abortResult.stdout)
	if aborted.Result.Status != "aborted" {
		t.Fatalf("unexpected aborted session: %+v", aborted.Result)
	}
	if content := readFile(t, filepath.Join(repo, "owned.txt")); content != "preserved work\n" {
		t.Fatalf("abort discarded edits: %q", content)
	}
	inspectedTask := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if inspectedTask.Result.Status != "quarantined" || len(inspectedTask.Result.Claims) != 0 {
		t.Fatalf("abort did not retain quarantined ownership history and clear claims: %+v", inspectedTask.Result)
	}
	if started := runBandmaster(t, repo, "session", "start", "--json"); started.exitCode != 3 || !strings.Contains(started.stdout, "working_tree_not_clean") {
		t.Fatalf("new session accepted preserved work: %+v", started)
	}
	runGit(t, repo, "clean", "-fd")
	if restarted := successfulSessionCommand(t, repo, "start"); restarted.Result.Status != "active" {
		t.Fatalf("cleaned repository did not admit a new session: %+v", restarted.Result)
	}
}

func TestSessionAbortWithoutWorkersCompletesImmediately(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	aborted := successfulSessionCommand(t, repo, "abort")
	if aborted.Result.Status != "aborted" {
		t.Fatalf("claimless abort = %+v", aborted.Result)
	}
}

func TestSessionAbortDryRunReturnsPlanWithoutMutation(t *testing.T) {
	repo := approvedCleanRepository(t)
	started := successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Preview abort", "--intent", "Inspect disposition", "--expected-outcome", "No mutation")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "worker-abort-preview")
	claimed := successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "preview.txt")
	beforeTask := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	beforeSession := successfulSessionCommand(t, repo, "inspect")

	previewResult := runBandmaster(t, repo, "session", "abort", "--dry-run", "--json")
	if previewResult.exitCode != 0 {
		t.Fatalf("abort dry-run failed: exit=%d stdout=%s stderr=%s", previewResult.exitCode, previewResult.stdout, previewResult.stderr)
	}
	var preview abortPlanResponse
	if err := json.Unmarshal([]byte(previewResult.stdout), &preview); err != nil {
		t.Fatalf("decode abort plan: %v\n%s", err, previewResult.stdout)
	}
	if !preview.Success || preview.Command != "session abort" || preview.SessionID != started.SessionID || preview.Result.SessionStatus != "active" {
		t.Fatalf("unexpected abort plan envelope: %+v", preview)
	}
	if len(preview.Result.Tasks) != 1 || preview.Result.Tasks[0].TaskID != task.Result.ID || preview.Result.Tasks[0].CurrentStatus != "editing" || preview.Result.Tasks[0].TargetStatus != "quarantined" || len(preview.Result.ActiveClaims) != 1 || preview.Result.ActiveClaims[0].Path != "preview.txt" || len(preview.Result.Batches) != 1 || len(preview.Result.Files) != 1 || preview.Result.Files[0] != "preview.txt" {
		t.Fatalf("abort plan omitted affected state: %+v", preview.Result)
	}
	if len(preview.Result.PreservedArtifacts) == 0 || len(preview.Result.Blockers) != 1 || preview.Result.Blockers[0].Code != "worker_termination_confirmation_required" {
		t.Fatalf("abort plan omitted preserved evidence or blockers: %+v", preview.Result)
	}

	afterSession := successfulSessionCommand(t, repo, "inspect")
	afterTask := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if afterSession.Result.Status != beforeSession.Result.Status || len(afterSession.Result.AuditHistory) != len(beforeSession.Result.AuditHistory) || afterSession.Result.Monitor == nil || beforeSession.Result.Monitor == nil || afterSession.Result.Monitor.ProcessIdentity != beforeSession.Result.Monitor.ProcessIdentity || afterSession.Result.Monitor.Status != "healthy" {
		t.Fatalf("dry-run mutated session or monitor: before=%+v after=%+v", beforeSession.Result, afterSession.Result)
	}
	if afterTask.Result.Status != beforeTask.Result.Status || len(afterTask.Result.Claims) != len(claimed.Result.Claims) || len(afterTask.Result.AuditHistory) != len(beforeTask.Result.AuditHistory) {
		t.Fatalf("dry-run mutated task state: before=%+v after=%+v", beforeTask.Result, afterTask.Result)
	}
}

func TestSessionAbortCleanupFailureRollsBackAndRetryCompletes(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Retry abort", "--intent", "Roll back cleanup", "--expected-outcome", "Retry completes")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "worker-abort-retry")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "retry.txt")
	before := successfulTaskCommand(t, repo, "inspect", task.Result.ID)

	failed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_FAIL_ABORT_AT=after-claim-release"}, "session", "abort", "--termination-confirmation", "worker stopped", "--json")
	if failed.exitCode == 0 || !strings.Contains(failed.stdout, "abort_cleanup_failed") {
		t.Fatalf("injected abort cleanup did not fail safely: %+v", failed)
	}
	afterFailure := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if afterFailure.Result.Status != "editing" || len(afterFailure.Result.Claims) != 1 || len(afterFailure.Result.AuditHistory) != len(before.Result.AuditHistory) {
		t.Fatalf("failed abort committed partial cleanup: %+v", afterFailure.Result)
	}
	if session := successfulSessionCommand(t, repo, "inspect"); session.Result.Status != "active" || session.Result.Monitor == nil || session.Result.Monitor.Status != "stopped" || len(session.Result.AuditHistory) != 1 {
		t.Fatalf("failed abort left a non-retryable durable state: %+v", session.Result)
	}

	retried := runBandmaster(t, repo, "session", "abort", "--termination-confirmation", "worker stopped", "--json")
	if retried.exitCode != 0 {
		t.Fatalf("abort retry failed: exit=%d stdout=%s stderr=%s", retried.exitCode, retried.stdout, retried.stderr)
	}
	if session := decodeSessionResponse(t, retried.stdout); session.Result.Status != "aborted" {
		t.Fatalf("abort retry did not complete: %+v", session.Result)
	}
}

func TestSessionAbortRequiresFinalizationReconciliation(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Frozen abort", "--intent", "Require reconciliation", "--expected-outcome", "Abort remains fail closed")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "worker-frozen-abort")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "frozen abort\n")
	submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
	successfulBatchCommand(t, repo, "freeze")

	previewResult := runBandmaster(t, repo, "session", "abort", "--dry-run", "--termination-confirmation", "worker stopped", "--json")
	if previewResult.exitCode != 0 {
		t.Fatalf("frozen abort preview failed: %+v", previewResult)
	}
	var preview abortPlanResponse
	if err := json.Unmarshal([]byte(previewResult.stdout), &preview); err != nil {
		t.Fatalf("decode frozen abort plan: %v", err)
	}
	if len(preview.Result.Blockers) != 1 || preview.Result.Blockers[0].Code != "finalization_recovery_required" || len(preview.Result.Batches) != 1 || preview.Result.Batches[0].Status != "frozen" {
		t.Fatalf("frozen abort plan did not require reconciliation: %+v", preview.Result)
	}
	blocked := runBandmaster(t, repo, "session", "abort", "--termination-confirmation", "worker stopped", "--json")
	if blocked.exitCode != 3 || !strings.Contains(blocked.stdout, "finalization_recovery_required") {
		t.Fatalf("unreconciled finalizing work was aborted: %+v", blocked)
	}
	if batch := successfulBatchCommand(t, repo, "inspect"); batch.Result.Status != "frozen" || len(batch.Result.Manifest) != 1 {
		t.Fatalf("blocked abort mutated frozen evidence: %+v", batch.Result)
	}
}

func TestSessionAbortPreservesQuarantinedFailureEvidence(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "quarantined.txt"), "baseline\n")
	runGit(t, repo, "add", "quarantined.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add quarantine fixture")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Abort quarantine", "--intent", "Preserve failure evidence", "--expected-outcome", "Abort remains auditable")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "worker-quarantined-abort")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "quarantined.txt")
	writeFile(t, filepath.Join(repo, "quarantined.txt"), "submitted\n")
	if reviewed := runBandmaster(t, repo, "task", "diff", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--json"); reviewed.exitCode != 0 {
		t.Fatalf("review quarantine fixture: %+v", reviewed)
	}
	successfulTaskCommand(t, repo, "submit", task.Result.ID,
		"--token", assignment.Result.AssignmentToken,
		"--behavior-changed", "Submitted content",
		"--key-decisions", "Preserve exact bytes",
		"--validation-expectations", "Snapshot stays fixed",
		"--known-risks", "None",
	)
	writeFile(t, filepath.Join(repo, "quarantined.txt"), "drifted\n")
	paused := waitForSessionStatus(t, repo, "paused")
	unresolvedViolation(t, paused, "submitted_path_drift", "quarantined.txt")

	aborted := runBandmaster(t, repo, "session", "abort", "--termination-confirmation", "worker stopped", "--json")
	if aborted.exitCode != 0 {
		t.Fatalf("abort quarantined work: exit=%d stdout=%s stderr=%s", aborted.exitCode, aborted.stdout, aborted.stderr)
	}
	inspectedSession := successfulSessionCommand(t, repo, "inspect")
	inspectedTask := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if inspectedSession.Result.Status != "aborted" || len(inspectedSession.Result.IntegrityViolations) == 0 || inspectedSession.Result.IntegrityViolations[0].Kind != "submitted_path_drift" {
		t.Fatalf("abort lost quarantined failure evidence: %+v", inspectedSession.Result)
	}
	if inspectedTask.Result.Status != "quarantined" || len(inspectedTask.Result.Claims) != 0 || len(inspectedTask.Result.OwnershipEvidence) != 1 || inspectedTask.Result.OwnershipEvidence[0].SubmittedSnapshot == nil || inspectedTask.Result.Submission == nil {
		t.Fatalf("abort lost quarantined ownership evidence: %+v", inspectedTask.Result)
	}
}

func TestSessionAbortCompletesAfterFinalizationRecovery(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Recover then abort", "--intent", "Reconcile finalization first", "--expected-outcome", "Abort repair-pending work")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "worker-recovered-abort")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "recovered abort\n")
	submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
	successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	crashed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_CRASH_FINALIZATION_AT=prepared"}, "batch", "commit", "--json")
	if crashed.exitCode != 97 {
		t.Fatalf("did not create interrupted finalization: %+v", crashed)
	}
	previewResult := runBandmaster(t, repo, "session", "abort", "--dry-run", "--termination-confirmation", "worker stopped", "--json")
	if previewResult.exitCode != 0 {
		t.Fatalf("preview interrupted finalization abort: %+v", previewResult)
	}
	var preview abortPlanResponse
	if err := json.Unmarshal([]byte(previewResult.stdout), &preview); err != nil {
		t.Fatalf("decode interrupted abort plan: %v", err)
	}
	if len(preview.Result.Journals) != 1 || preview.Result.Journals[0].Step != "prepared" || len(preview.Result.Blockers) != 1 || preview.Result.Blockers[0].Code != "finalization_recovery_required" {
		t.Fatalf("interrupted abort plan omitted journal blocker: %+v", preview.Result)
	}
	recovered := runBandmaster(t, repo, "finalization", "recover", "--confirmation", "inspected interrupted process", "--json")
	if recovered.exitCode != 0 {
		t.Fatalf("recover finalization before abort: %+v", recovered)
	}
	if batch := successfulBatchCommand(t, repo, "inspect"); batch.Result.Status != "repair_pending" || len(batch.Result.Manifest) != 1 {
		t.Fatalf("finalization recovery did not retain repair evidence: %+v", batch.Result)
	}
	aborted := runBandmaster(t, repo, "session", "abort", "--termination-confirmation", "worker stopped", "--json")
	if aborted.exitCode != 0 || decodeSessionResponse(t, aborted.stdout).Result.Status != "aborted" {
		t.Fatalf("abort after finalization recovery failed: %+v", aborted)
	}
	if inspected := successfulTaskCommand(t, repo, "inspect", task.Result.ID); len(inspected.Result.Claims) != 0 || len(inspected.Result.OwnershipEvidence) != 1 || inspected.Result.OwnershipEvidence[0].SubmittedSnapshot == nil {
		t.Fatalf("reconciled abort lost ownership evidence: %+v", inspected.Result)
	}
}

func TestSessionAbortReleasesSubmittedClaimsAndPreservesOwnershipEvidence(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "owned.txt"), "before\n")
	runGit(t, repo, "add", "owned.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add owned fixture")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Submitted abort", "--intent", "Preserve evidence", "--expected-outcome", "Release only active locking")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "worker-submitted-abort")
	claimed := successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "after\n")
	if reviewed := runBandmaster(t, repo, "task", "diff", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--json"); reviewed.exitCode != 0 {
		t.Fatalf("review submitted abort diff: exit=%d stdout=%s stderr=%s", reviewed.exitCode, reviewed.stdout, reviewed.stderr)
	}
	submitted := successfulTaskCommand(t, repo, "submit", task.Result.ID,
		"--token", assignment.Result.AssignmentToken,
		"--behavior-changed", "Owned content changed",
		"--key-decisions", "Preserve the submitted bytes",
		"--validation-expectations", "Focused validation passed",
		"--known-risks", "None",
	)
	if submitted.Result.Status != "submitted" || submitted.Result.Submission == nil {
		t.Fatalf("task was not submitted: %+v", submitted.Result)
	}

	aborted := runBandmaster(t, repo, "session", "abort", "--termination-confirmation", "worker handle exited", "--json")
	if aborted.exitCode != 0 {
		t.Fatalf("abort submitted task: exit=%d stdout=%s stderr=%s", aborted.exitCode, aborted.stdout, aborted.stderr)
	}
	inspected := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if inspected.Result.Status != "quarantined" || len(inspected.Result.Claims) != 0 {
		t.Fatalf("abort retained active locking: %+v", inspected.Result)
	}
	if inspected.Result.Submission == nil || inspected.Result.Submission.SubmittedAt != submitted.Result.Submission.SubmittedAt {
		t.Fatalf("abort lost structured submission: %+v", inspected.Result.Submission)
	}
	if len(inspected.Result.OwnershipEvidence) != 1 {
		t.Fatalf("ownership evidence count = %d, want 1: %+v", len(inspected.Result.OwnershipEvidence), inspected.Result.OwnershipEvidence)
	}
	evidence := inspected.Result.OwnershipEvidence[0]
	if evidence.Path != "owned.txt" || evidence.ClaimedAt == "" || evidence.Baseline.ContentHash != claimed.Result.Claims[0].Baseline.ContentHash || evidence.SubmittedSnapshot == nil || evidence.SubmittedSnapshot.ContentHash != submitted.Result.Claims[0].SubmittedSnapshot.ContentHash {
		t.Fatalf("abort did not preserve attributable path evidence: %+v", evidence)
	}
}

func TestIllegalSessionTransitionDoesNotChangeDurableState(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	successfulSessionCommand(t, repo, "pause")

	illegal := runBandmaster(t, repo, "session", "finish", "--json")
	if illegal.exitCode != 3 {
		t.Fatalf("illegal finish exit code = %d, want 3; stdout = %s; stderr = %s", illegal.exitCode, illegal.stdout, illegal.stderr)
	}
	response := decodeSessionResponse(t, illegal.stdout)
	if response.Success || response.Error.Code != "invalid_session_transition" || response.Error.Retryable {
		t.Fatalf("unexpected illegal transition response: %+v", response)
	}

	inspected := successfulSessionCommand(t, repo, "inspect")
	if inspected.Result.Status != "paused" || len(inspected.Result.AuditHistory) != 2 {
		t.Fatalf("illegal transition changed durable state: %+v", inspected)
	}
}

func TestSessionTransitionsAreIdempotentWithoutDuplicateAuditEvents(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	successfulSessionCommand(t, repo, "pause")
	pausedAgain := successfulSessionCommand(t, repo, "pause")
	if pausedAgain.Result.Status != "paused" || len(pausedAgain.Result.AuditHistory) != 2 {
		t.Fatalf("repeated pause was not idempotent: %+v", pausedAgain)
	}

	successfulSessionCommand(t, repo, "resume")
	resumedAgain := successfulSessionCommand(t, repo, "resume")
	if resumedAgain.Result.Status != "active" || len(resumedAgain.Result.AuditHistory) != 3 {
		t.Fatalf("repeated resume was not idempotent: %+v", resumedAgain)
	}

	successfulSessionCommand(t, repo, "finish")
	finishedAgain := successfulSessionCommand(t, repo, "finish")
	if finishedAgain.Result.Status != "completed" || len(finishedAgain.Result.AuditHistory) != 5 {
		t.Fatalf("repeated finish was not idempotent: %+v", finishedAgain)
	}
}

func TestConcurrentDuplicateSessionTransitionIsIdempotent(t *testing.T) {
	repo := approvedCleanRepository(t)
	started := successfulSessionCommand(t, repo, "start")

	const processCount = 8
	start := make(chan struct{})
	results := make(chan concurrentCommandResult, processCount)
	var workers sync.WaitGroup
	for range processCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- runBandmasterConcurrently(repo, "session", "pause", "--json")
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	for result := range results {
		if result.err != nil || result.exitCode != 0 {
			t.Errorf("concurrent pause failed: exit=%d err=%v stdout=%s stderr=%s", result.exitCode, result.err, result.stdout, result.stderr)
			continue
		}
		response := decodeSessionResponse(t, result.stdout)
		if response.SessionID != started.SessionID || response.Result.Status != "paused" {
			t.Errorf("unexpected concurrent pause response: %+v", response)
		}
	}
	inspected := successfulSessionCommand(t, repo, "inspect")
	if inspected.Result.Status != "paused" || len(inspected.Result.AuditHistory) != 2 {
		t.Fatalf("concurrent pause was not applied exactly once: %+v", inspected)
	}
}

func TestConcurrentDuplicateSessionFinishCompletesChecksOnce(t *testing.T) {
	repo := approvedCleanRepository(t)
	started := successfulSessionCommand(t, repo, "start")

	const processCount = 4
	start := make(chan struct{})
	results := make(chan concurrentCommandResult, processCount)
	var workers sync.WaitGroup
	for range processCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- runBandmasterConcurrently(repo, "session", "finish", "--json")
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	for result := range results {
		if result.err != nil || result.exitCode != 0 {
			t.Errorf("concurrent finish failed: exit=%d err=%v stdout=%s stderr=%s", result.exitCode, result.err, result.stdout, result.stderr)
			continue
		}
		response := decodeSessionResponse(t, result.stdout)
		if response.SessionID != started.SessionID || response.Result.Status != "completed" {
			t.Errorf("unexpected concurrent finish response: %+v", response)
		}
	}
	inspected := successfulSessionCommand(t, repo, "inspect")
	if inspected.Result.Status != "completed" || len(inspected.Result.AuditHistory) != 3 || inspected.Result.AuditHistory[1].Event != "session_finalizing" || inspected.Result.AuditHistory[2].Event != "session_completed" {
		t.Fatalf("concurrent finish was not finalized exactly once: %+v", inspected.Result)
	}
}

type concurrentCommandResult struct {
	commandResult
	err error
}

func runBandmasterConcurrently(dir string, args ...string) concurrentCommandResult {
	cmd := exec.Command(bandmasterBinary, args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return concurrentCommandResult{commandResult: commandResult{exitCode: -1, stdout: stdout.String(), stderr: stderr.String()}, err: err}
		}
		exitCode = exitError.ExitCode()
	}
	return concurrentCommandResult{commandResult: commandResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String()}}
}

func TestSessionStartEnforcesConfigurationAndCleanGitPreconditions(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*testing.T) string
		wantCode string
	}{
		{
			name: "not initialized",
			setup: func(t *testing.T) string {
				repo := newGitRepository(t)
				commitRepository(t, repo)
				return repo
			},
			wantCode: "configuration_not_initialized",
		},
		{
			name: "configuration not approved",
			setup: func(t *testing.T) string {
				repo := newGitRepository(t)
				writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/project\n\ngo 1.24\n")
				initialized := runBandmaster(t, repo, "init", "--json")
				if initialized.exitCode != 0 {
					t.Fatalf("init: %s", initialized.stderr)
				}
				runGit(t, repo, "add", ".")
				runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Initialize project")
				return repo
			},
			wantCode: "configuration_not_approved",
		},
		{
			name: "repository has no commits",
			setup: func(t *testing.T) string {
				repo := newGitRepository(t)
				initialized := runBandmaster(t, repo, "init", "--json")
				if initialized.exitCode != 0 {
					t.Fatalf("init: %s", initialized.stderr)
				}
				approved := runBandmaster(t, repo, "config", "approve", responseDigest(t, initialized.stdout), "--json")
				if approved.exitCode != 0 {
					t.Fatalf("approve: %s", approved.stderr)
				}
				return repo
			},
			wantCode: "repository_has_no_commits",
		},
		{
			name: "detached head",
			setup: func(t *testing.T) string {
				repo := approvedCleanRepository(t)
				runGit(t, repo, "checkout", "--detach")
				return repo
			},
			wantCode: "detached_head",
		},
		{
			name: "dirty index",
			setup: func(t *testing.T) string {
				repo := approvedCleanRepository(t)
				writeFile(t, filepath.Join(repo, "staged.txt"), "staged\n")
				runGit(t, repo, "add", "staged.txt")
				return repo
			},
			wantCode: "index_not_clean",
		},
		{
			name: "dirty working tree",
			setup: func(t *testing.T) string {
				repo := approvedCleanRepository(t)
				writeFile(t, filepath.Join(repo, "untracked.txt"), "untracked\n")
				return repo
			},
			wantCode: "working_tree_not_clean",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := test.setup(t)
			assertSessionError(t, repo, "start", test.wantCode)
		})
	}
}

func TestSecondSessionIsRejectedBeforeReevaluatingRepositoryState(t *testing.T) {
	repo := approvedCleanRepository(t)
	started := successfulSessionCommand(t, repo, "start")
	writeFile(t, filepath.Join(repo, "worker-edit.txt"), "in progress\n")

	response := assertSessionError(t, repo, "start", "session_already_active")
	if response.SessionID != started.SessionID {
		t.Fatalf("active session error session_id = %q, want %q", response.SessionID, started.SessionID)
	}
}

func TestSessionFinishRequiresCleanUnchangedRepository(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	dirtyPath := filepath.Join(repo, "unexpected.txt")
	writeFile(t, dirtyPath, "unexpected\n")
	runGit(t, repo, "add", "unexpected.txt")

	result := runBandmaster(t, repo, "session", "finish", "--json")
	if result.exitCode != 4 {
		t.Fatalf("session finish exit code = %d, want 4; stdout = %s; stderr = %s", result.exitCode, result.stdout, result.stderr)
	}
	response := decodeSessionResponse(t, result.stdout)
	if response.Error.Code != "index_drift" {
		t.Fatalf("session finish error = %+v, want index_drift", response)
	}
	runGit(t, repo, "reset", "--", "unexpected.txt")
	inspected := successfulSessionCommand(t, repo, "inspect")
	if inspected.Result.Status != "paused" || len(inspected.Result.IntegrityViolations) == 0 {
		t.Fatalf("integrity failure was not durably quarantined: %+v", inspected)
	}
	if err := os.Remove(dirtyPath); err != nil {
		t.Fatalf("clean repository: %v", err)
	}
	successfulIntegrityRecovery(t, repo, "removed the externally staged path")
	successfulSessionCommand(t, repo, "resume")
	finished := successfulSessionCommand(t, repo, "finish")
	if finished.Result.Status != "completed" {
		t.Fatalf("finish status = %q, want completed", finished.Result.Status)
	}
}

func TestSessionResumeRequiresCurrentConfigurationApproval(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	successfulSessionCommand(t, repo, "pause")
	configPath := filepath.Join(repo, ".bandmaster.yaml")
	config := readFile(t, configPath)
	writeFile(t, configPath, strings.Replace(config, "timeout: 10m", "timeout: 5m", 1))

	assertSessionError(t, repo, "resume", "configuration_not_approved")
	inspected := successfulSessionCommand(t, repo, "inspect")
	if inspected.Result.Status != "paused" || len(inspected.Result.AuditHistory) != 2 {
		t.Fatalf("failed resume changed durable state: %+v", inspected)
	}
}

func approvedCleanRepository(t *testing.T) string {
	t.Helper()
	repo := newGitRepository(t)
	writeFile(t, repo+"/go.mod", "module example.com/project\n\ngo 1.24\n")
	// init detects `go test ./...`; it needs a real package to validate successfully.
	writeFile(t, repo+"/project.go", "package project\n")
	initialized := runBandmaster(t, repo, "init", "--json")
	if initialized.exitCode != 0 {
		t.Fatalf("init exit code = %d, stdout = %s, stderr = %s", initialized.exitCode, initialized.stdout, initialized.stderr)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Initialize project")
	approved := runBandmaster(t, repo, "config", "approve", responseDigest(t, initialized.stdout), "--json")
	if approved.exitCode != 0 {
		t.Fatalf("approve exit code = %d, stdout = %s, stderr = %s", approved.exitCode, approved.stdout, approved.stderr)
	}
	return repo
}

func decodeSessionResponse(t *testing.T, output string) sessionResponse {
	t.Helper()
	var response sessionResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("decode session response: %v\n%s", err, output)
	}
	return response
}

func successfulSessionCommand(t *testing.T, repo, action string) sessionResponse {
	t.Helper()
	result := runBandmaster(t, repo, "session", action, "--json")
	if result.exitCode != 0 {
		t.Fatalf("session %s exit code = %d, stdout = %s, stderr = %s", action, result.exitCode, result.stdout, result.stderr)
	}
	response := decodeSessionResponse(t, result.stdout)
	if !response.Success || response.SchemaVersion != "1" || response.Command != "session "+action {
		t.Fatalf("unexpected session %s response: %+v", action, response)
	}
	return response
}

func assertSessionError(t *testing.T, repo, action, wantCode string) sessionResponse {
	t.Helper()
	result := runBandmaster(t, repo, "session", action, "--json")
	if result.exitCode != 3 {
		t.Fatalf("session %s exit code = %d, want 3; stdout = %s; stderr = %s", action, result.exitCode, result.stdout, result.stderr)
	}
	response := decodeSessionResponse(t, result.stdout)
	if response.Success || response.SchemaVersion != "1" || response.Error.Code != wantCode || response.Error.Retryable {
		t.Fatalf("session %s error = %+v, want non-retryable %q", action, response, wantCode)
	}
	return response
}
