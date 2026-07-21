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
	if session := successfulSessionCommand(t, repo, "inspect"); session.Result.Status != "aborting" {
		t.Fatalf("abort did not enter aborting: %+v", session.Result)
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

	const processCount = 64
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
