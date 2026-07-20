package integration_test

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
)

type batchResponse struct {
	SchemaVersion string `json:"schema_version"`
	Command       string `json:"command"`
	Success       bool   `json:"success"`
	SessionID     string `json:"session_id"`
	Result        struct {
		ID            string `json:"id"`
		CreationOrder int64  `json:"creation_order"`
		BaseBranch    string `json:"base_branch"`
		BaseCommit    string `json:"base_commit"`
		Status        string `json:"status"`
		FrozenAt      string `json:"frozen_at"`
		Members       []struct {
			TaskID          string `json:"task_id"`
			MembershipOrder int64  `json:"membership_order"`
			TaskOrder       int64  `json:"task_creation_order"`
			Status          string `json:"status"`
			Outcome         string `json:"submission_outcome"`
		} `json:"members"`
		Manifest []struct {
			TaskID          string `json:"task_id"`
			MembershipOrder int64  `json:"membership_order"`
			ClaimOrder      int64  `json:"claim_order"`
			Path            string `json:"path"`
			Baseline        struct {
				Presence    string `json:"presence"`
				Type        string `json:"type"`
				ContentHash string `json:"content_hash"`
				Executable  bool   `json:"executable"`
			} `json:"baseline"`
			Submitted struct {
				Presence    string `json:"presence"`
				Type        string `json:"type"`
				ContentHash string `json:"content_hash"`
				Executable  bool   `json:"executable"`
			} `json:"submitted"`
		} `json:"manifest"`
		Validation []struct {
			Attempt    int64  `json:"attempt"`
			Status     string `json:"status"`
			StartedAt  string `json:"started_at"`
			FinishedAt string `json:"finished_at"`
			Commands   []struct {
				Attempt                  int64             `json:"attempt"`
				CommandOrder             int64             `json:"command_order"`
				Source                   string            `json:"source"`
				TaskID                   string            `json:"task_id"`
				Name                     string            `json:"name"`
				Argv                     []string          `json:"argv"`
				Script                   string            `json:"script"`
				ResolvedArgv             []string          `json:"resolved_argv"`
				WorkingDirectory         string            `json:"working_directory"`
				ResolvedWorkingDirectory string            `json:"resolved_working_directory"`
				Timeout                  string            `json:"timeout"`
				EnvironmentOverrides     map[string]string `json:"environment_overrides"`
				ResolvedEnvironment      map[string]string `json:"resolved_environment"`
				Status                   string            `json:"status"`
				ExitCode                 *int              `json:"exit_code"`
				DurationMilliseconds     int64             `json:"duration_milliseconds"`
				Stdout                   string            `json:"stdout"`
				Stderr                   string            `json:"stderr"`
				StdoutTruncated          bool              `json:"stdout_truncated"`
				StderrTruncated          bool              `json:"stderr_truncated"`
				StartedAt                string            `json:"started_at"`
				FinishedAt               string            `json:"finished_at"`
			} `json:"commands"`
		} `json:"validation"`
		AuditHistory []struct {
			Sequence   int64  `json:"sequence"`
			Event      string `json:"event"`
			FromStatus string `json:"from_status"`
			ToStatus   string `json:"to_status"`
			OccurredAt string `json:"occurred_at"`
		} `json:"audit_history"`
	} `json:"result"`
	Error struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

func TestBatchFreezePersistsOrderedAttributableMembership(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "left.txt"), "left baseline\n")
	writeFile(t, filepath.Join(repo, "right.txt"), "right baseline\n")
	runGit(t, repo, "add", "left.txt", "right.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add batch fixtures")
	started := successfulSessionCommand(t, repo, "start")

	left := successfulTaskCommand(t, repo, "create", "--title", "Edit left", "--intent", "Change left behavior", "--expected-outcome", "Left is updated")
	right := successfulTaskCommand(t, repo, "create", "--title", "Inspect right", "--intent", "Keep right behavior", "--expected-outcome", "Right remains unchanged")
	dependent := successfulTaskCommand(t, repo, "create", "--title", "Use left", "--intent", "Wait for accepted left work", "--expected-outcome", "Starts in a later batch", "--prerequisite", left.Result.ID)
	leftAssignment := successfulTaskCommand(t, repo, "assign", left.Result.ID, "--worker", "worker-batch-left")
	rightAssignment := successfulTaskCommand(t, repo, "assign", right.Result.ID, "--worker", "worker-batch-right")

	preflight := runBandmaster(t, repo, "task", "preflight", left.Result.ID, "--token", leftAssignment.Result.AssignmentToken, "--path", "left.txt", "--json")
	if preflight.exitCode != 0 {
		t.Fatalf("preflight failed: %+v", preflight)
	}
	if inspected := successfulTaskCommand(t, repo, "inspect", left.Result.ID); inspected.Result.BatchID != "" {
		t.Fatalf("preflight joined a batch before initial claim: %+v", inspected.Result)
	}

	leftClaimed := successfulTaskCommand(t, repo, "claim", left.Result.ID, "--token", leftAssignment.Result.AssignmentToken, "--path", "left.txt")
	rightClaimed := successfulTaskCommand(t, repo, "claim", right.Result.ID, "--token", rightAssignment.Result.AssignmentToken, "--path", "right.txt")
	if leftClaimed.Result.BatchID == "" || rightClaimed.Result.BatchID != leftClaimed.Result.BatchID {
		t.Fatalf("independent initial claims did not join one collecting batch: left=%+v right=%+v", leftClaimed.Result, rightClaimed.Result)
	}

	contender := successfulTaskCommand(t, repo, "create", "--title", "Contend for left", "--intent", "Prove blocked work stays out", "--expected-outcome", "No batch membership")
	contenderAssignment := successfulTaskCommand(t, repo, "assign", contender.Result.ID, "--worker", "worker-batch-contender")
	contention := runBandmaster(t, repo, "task", "claim", contender.Result.ID, "--token", contenderAssignment.Result.AssignmentToken, "--path", "left.txt", "--json")
	assertTaskError(t, contention, 2, "claim_unavailable", true)
	if blockedTask := successfulTaskCommand(t, repo, "inspect", contender.Result.ID); blockedTask.Result.BatchID != "" || len(blockedTask.Result.Claims) != 0 {
		t.Fatalf("blocked task joined the collecting batch: %+v", blockedTask.Result)
	}

	assertBatchError(t, runBandmaster(t, repo, "batch", "freeze", "--json"), 2, "active_workers", true)
	writeFile(t, filepath.Join(repo, "left.txt"), "left submitted\n")
	submitBatchTask(t, repo, left.Result.ID, leftAssignment.Result.AssignmentToken)
	assertBatchError(t, runBandmaster(t, repo, "batch", "freeze", "--json"), 2, "active_workers", true)
	submitBatchTask(t, repo, right.Result.ID, rightAssignment.Result.AssignmentToken)

	dependentAssignment := runBandmaster(t, repo, "task", "assign", dependent.Result.ID, "--worker", "worker-dependent-too-early", "--json")
	assertTaskError(t, dependentAssignment, 2, "task_not_ready", true)
	if pending := successfulTaskCommand(t, repo, "inspect", dependent.Result.ID); pending.Result.Status != "planned" || pending.Result.BatchID != "" {
		t.Fatalf("dependent work entered its prerequisite batch: %+v", pending.Result)
	}

	const freezeProcesses = 4
	startFreeze := make(chan struct{})
	freezeResults := make(chan concurrentCommandResult, freezeProcesses)
	var freezeWorkers sync.WaitGroup
	for range freezeProcesses {
		freezeWorkers.Add(1)
		go func() {
			defer freezeWorkers.Done()
			<-startFreeze
			freezeResults <- runBandmasterConcurrently(repo, "batch", "freeze", "--json")
		}()
	}
	close(startFreeze)
	freezeWorkers.Wait()
	close(freezeResults)
	var frozen batchResponse
	for result := range freezeResults {
		if result.err != nil || result.exitCode != 0 {
			t.Fatalf("concurrent batch freeze failed: exit=%d err=%v stdout=%s stderr=%s", result.exitCode, result.err, result.stdout, result.stderr)
		}
		response := decodeBatchResponse(t, result.stdout)
		if frozen.Result.ID == "" {
			frozen = response
		}
		if response.Result.ID != leftClaimed.Result.BatchID || response.Result.Status != "frozen" {
			t.Fatalf("unexpected concurrent batch freeze response: %+v", response)
		}
	}
	if frozen.SessionID != started.SessionID || frozen.Result.ID != leftClaimed.Result.BatchID || frozen.Result.Status != "frozen" || frozen.Result.FrozenAt == "" {
		t.Fatalf("unexpected frozen batch: %+v", frozen)
	}
	if frozen.Result.BaseBranch != "main" || frozen.Result.BaseCommit != started.Result.StartingCommit || frozen.Result.CreationOrder != 1 {
		t.Fatalf("frozen batch lost its base: %+v", frozen.Result)
	}
	if len(frozen.Result.Members) != 2 || frozen.Result.Members[0].TaskID != left.Result.ID || frozen.Result.Members[0].MembershipOrder != 1 || frozen.Result.Members[1].TaskID != right.Result.ID || frozen.Result.Members[1].MembershipOrder != 2 {
		t.Fatalf("membership was not frozen in claim order: %+v", frozen.Result.Members)
	}
	if frozen.Result.Members[0].Outcome != "pending_changes" || frozen.Result.Members[1].Outcome != "pending_no_op" {
		t.Fatalf("submission outcomes were not retained: %+v", frozen.Result.Members)
	}
	if len(frozen.Result.Manifest) != 2 || frozen.Result.Manifest[0].TaskID != left.Result.ID || frozen.Result.Manifest[0].Path != "left.txt" || frozen.Result.Manifest[0].Baseline.ContentHash == frozen.Result.Manifest[0].Submitted.ContentHash || frozen.Result.Manifest[1].TaskID != right.Result.ID || frozen.Result.Manifest[1].Path != "right.txt" || frozen.Result.Manifest[1].Baseline.ContentHash != frozen.Result.Manifest[1].Submitted.ContentHash {
		t.Fatalf("frozen path manifest is not attributable and complete: %+v", frozen.Result.Manifest)
	}
	if len(frozen.Result.AuditHistory) != 1 || frozen.Result.AuditHistory[0].Event != "batch_frozen" || frozen.Result.AuditHistory[0].FromStatus != "collecting" || frozen.Result.AuditHistory[0].ToStatus != "frozen" || frozen.Result.AuditHistory[0].OccurredAt == "" {
		t.Fatalf("batch freeze was not audited: %+v", frozen.Result.AuditHistory)
	}

	inspected := successfulBatchCommand(t, repo, "inspect", frozen.Result.ID)
	if inspected.Result.ID != frozen.Result.ID || inspected.Result.FrozenAt != frozen.Result.FrozenAt || len(inspected.Result.Members) != 2 || len(inspected.Result.Manifest) != 2 {
		t.Fatalf("fresh invocation did not observe the frozen batch: %+v", inspected.Result)
	}
	retried := successfulBatchCommand(t, repo, "freeze")
	if retried.Result.ID != frozen.Result.ID || retried.Result.FrozenAt != frozen.Result.FrozenAt || len(retried.Result.AuditHistory) != 1 {
		t.Fatalf("repeated freeze was not idempotent: %+v", retried.Result)
	}
	if session := successfulSessionCommand(t, repo, "inspect"); session.Result.Status != "finalizing" || session.Result.Monitor == nil || session.Result.Monitor.Status != "stopped" || session.Result.AuditHistory[len(session.Result.AuditHistory)-1].Event != "batch_frozen" {
		t.Fatalf("batch freeze did not enter controlled finalization: %+v", session.Result)
	}
	if blockedTask := successfulTaskCommand(t, repo, "inspect", contender.Result.ID); blockedTask.Result.BatchID != "" {
		t.Fatalf("late blocked task altered frozen membership: %+v", blockedTask.Result)
	}
	assertSessionError(t, repo, "finish", "batch_finalization_in_progress")
	writeFile(t, filepath.Join(repo, "left.txt"), "changed after the frozen barrier\n")
	assertBatchError(t, runBandmaster(t, repo, "batch", "freeze", "--json"), 4, "submitted_path_drift", false)
	if session := successfulSessionCommand(t, repo, "inspect"); session.Result.Status != "paused" {
		t.Fatalf("post-freeze drift did not quarantine finalization: %+v", session.Result)
	}
}

func TestBatchBarrierQuarantinesIndexDrift(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*testing.T, string)
		wantCode string
	}{
		{
			name: "index",
			mutate: func(t *testing.T, repo string) {
				writeFile(t, filepath.Join(repo, "staged.txt"), "outside index change\n")
				runGit(t, repo, "add", "staged.txt")
			},
			wantCode: "index_drift",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := approvedCleanRepository(t)
			writeFile(t, filepath.Join(repo, "owned.txt"), "baseline\n")
			runGit(t, repo, "add", "owned.txt")
			runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add barrier drift fixture")
			successfulSessionCommand(t, repo, "start")
			task := successfulTaskCommand(t, repo, "create", "--title", "Barrier drift", "--intent", "Freeze exact state", "--expected-outcome", "Unsafe drift is quarantined")
			assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "worker-barrier-drift")
			successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", "owned.txt")
			writeFile(t, filepath.Join(repo, "owned.txt"), "submitted\n")
			submitBatchTask(t, repo, task.Result.ID, assigned.Result.AssignmentToken)
			test.mutate(t, repo)

			result := runBandmaster(t, repo, "batch", "freeze", "--json")
			assertBatchError(t, result, 4, test.wantCode, false)
			if session := successfulSessionCommand(t, repo, "inspect"); session.Result.Status != "paused" {
				t.Fatalf("barrier drift did not pause the session: %+v", session.Result)
			}
		})
	}
}

func submitBatchTask(t *testing.T, repo, taskID, token string) taskResponse {
	t.Helper()
	if result := runBandmaster(t, repo, "task", "diff", taskID, "--token", token, "--json"); result.exitCode != 0 {
		t.Fatalf("task diff failed: %+v", result)
	}
	return successfulTaskCommand(t, repo, "submit", taskID,
		"--token", token,
		"--behavior-changed", "Implemented the assigned batch work",
		"--key-decisions", "Kept ownership exact",
		"--validation-expectations", "Official batch validation should pass",
		"--known-risks", "None",
	)
}

func successfulBatchCommand(t *testing.T, repo, action string, args ...string) batchResponse {
	t.Helper()
	commandArgs := append([]string{"batch", action}, args...)
	commandArgs = append(commandArgs, "--json")
	result := runBandmaster(t, repo, commandArgs...)
	if result.exitCode != 0 {
		t.Fatalf("batch %s exit code = %d, stdout = %s, stderr = %s", action, result.exitCode, result.stdout, result.stderr)
	}
	response := decodeBatchResponse(t, result.stdout)
	if !response.Success || response.Command != "batch "+action {
		t.Fatalf("unexpected batch %s response: %+v", action, response)
	}
	return response
}

func assertBatchError(t *testing.T, result commandResult, wantExit int, wantCode string, wantRetryable bool) batchResponse {
	t.Helper()
	if result.exitCode != wantExit {
		t.Fatalf("batch command exit code = %d, want %d; stdout = %s; stderr = %s", result.exitCode, wantExit, result.stdout, result.stderr)
	}
	response := decodeBatchResponse(t, result.stdout)
	if response.Success || response.Error.Code != wantCode || response.Error.Retryable != wantRetryable {
		t.Fatalf("batch command error = %+v, want code %q retryable %t", response, wantCode, wantRetryable)
	}
	return response
}

func decodeBatchResponse(t *testing.T, output string) batchResponse {
	t.Helper()
	var response batchResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("decode batch response: %v\n%s", err, output)
	}
	return response
}
