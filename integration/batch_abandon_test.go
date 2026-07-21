package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type batchAbandonResponse struct {
	SchemaVersion string `json:"schema_version"`
	Command       string `json:"command"`
	Success       bool   `json:"success"`
	SessionID     string `json:"session_id"`
	Result        struct {
		BatchID        string `json:"batch_id"`
		Reason         string `json:"reason"`
		Confirmation   string `json:"confirmation"`
		Outcome        string `json:"outcome"`
		NextAction     string `json:"next_action"`
		Idempotent     bool   `json:"idempotent"`
		ReleasedClaims []struct {
			TaskID string `json:"task_id"`
			Path   string `json:"path"`
		} `json:"released_claims"`
		Before struct {
			SessionStatus string `json:"session_status"`
			BatchStatus   string `json:"batch_status"`
		} `json:"before"`
		After struct {
			SessionStatus string `json:"session_status"`
			BatchStatus   string `json:"batch_status"`
		} `json:"after"`
		Evidence struct {
			ExpectedBranch string   `json:"expected_branch"`
			ObservedBranch string   `json:"observed_branch"`
			ExpectedHead   string   `json:"expected_head"`
			ObservedHead   string   `json:"observed_head"`
			StagedPaths    []string `json:"staged_paths"`
			JournalStep    string   `json:"journal_step,omitempty"`
		} `json:"evidence"`
		FinalizationRecovery *struct {
			Action  string `json:"action"`
			Outcome string `json:"outcome"`
		} `json:"finalization_recovery,omitempty"`
	} `json:"result"`
}

func TestBatchAbandonDoesNotReplayAnEarlierBatchForTheLatestBatch(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	successfulSessionCommand(t, repo, "start")
	firstTask := successfulTaskCommand(t, repo, "create", "--title", "First batch", "--intent", "Abandon first work", "--expected-outcome", "First batch is abandoned")
	firstAssignment := successfulTaskCommand(t, repo, "assign", firstTask.Result.ID, "--worker", "first-abandon-worker")
	firstClaim := successfulTaskCommand(t, repo, "claim", firstTask.Result.ID, "--token", firstAssignment.Result.AssignmentToken, "--path", "first-abandoned.txt")
	first := runBandmaster(t, repo, "batch", "abandon", "--reason", "first approach superseded", "--confirmation", "first worker stopped", "--json")
	if first.exitCode != 0 {
		t.Fatalf("abandon first batch: %+v", first)
	}
	firstResponse := decodeBatchAbandon(t, first)
	if firstResponse.Result.BatchID != firstClaim.Result.BatchID {
		t.Fatalf("first abandonment targeted %s, want %s", firstResponse.Result.BatchID, firstClaim.Result.BatchID)
	}
	if err := os.Remove(filepath.Join(repo, "first-abandoned.txt")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clean first abandoned edit: %v", err)
	}
	successfulSessionCommand(t, repo, "resume")

	secondTask := successfulTaskCommand(t, repo, "create", "--title", "Second batch", "--intent", "Abandon later work", "--expected-outcome", "Second batch gets its own event")
	secondAssignment := successfulTaskCommand(t, repo, "assign", secondTask.Result.ID, "--worker", "second-abandon-worker")
	secondClaim := successfulTaskCommand(t, repo, "claim", secondTask.Result.ID, "--token", secondAssignment.Result.AssignmentToken, "--path", "second-abandoned.txt")
	if secondClaim.Result.BatchID == firstClaim.Result.BatchID {
		t.Fatalf("later work reused abandoned batch %s", secondClaim.Result.BatchID)
	}
	second := runBandmaster(t, repo, "batch", "abandon", "--reason", "second approach superseded", "--confirmation", "second worker stopped", "--json")
	if second.exitCode != 0 {
		t.Fatalf("abandon second batch: %+v", second)
	}
	secondResponse := decodeBatchAbandon(t, second)
	if secondResponse.Result.BatchID != secondClaim.Result.BatchID || secondResponse.Result.Reason != "second approach superseded" || second.stdout == first.stdout {
		t.Fatalf("later abandonment replayed stale result: first=%+v second=%+v", firstResponse.Result, secondResponse.Result)
	}
}

func decodeBatchAbandon(t *testing.T, result commandResult) batchAbandonResponse {
	t.Helper()
	var response batchAbandonResponse
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("decode batch abandonment: %v\n%s", err, result.stdout)
	}
	return response
}

func TestBatchAbandonPreservesCollectingWorkAndOwnershipAndIsIdempotent(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Abandon edit", "--intent", "Preserve unfinished work", "--expected-outcome", "Audited abandonment")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "abandon-worker")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "abandoned.txt")
	writeFile(t, filepath.Join(repo, "abandoned.txt"), "preserved abandonment\n")

	missing := runBandmaster(t, repo, "batch", "abandon", "--json")
	if missing.exitCode != 3 || !strings.Contains(missing.stdout, "batch_abandonment_reason_required") {
		t.Fatalf("abandonment did not require reason and confirmation: %+v", missing)
	}
	abandoned := runBandmaster(t, repo, "batch", "abandon", "--reason", "superseded approach", "--confirmation", "worker handle stopped", "--json")
	if abandoned.exitCode != 0 {
		t.Fatalf("collecting abandonment failed: %+v", abandoned)
	}
	response := decodeBatchAbandon(t, abandoned)
	if response.SchemaVersion != "1" || response.Command != "batch abandon" || !response.Success || response.Result.Outcome != "abandoned" || !response.Result.Idempotent || response.Result.Before.SessionStatus != "active" || response.Result.Before.BatchStatus != "collecting" || response.Result.After.SessionStatus != "paused" || response.Result.After.BatchStatus != "abandoned" || response.Result.NextAction != "session abort or inspect preserved edits" || len(response.Result.ReleasedClaims) != 1 {
		t.Fatalf("unexpected collecting abandonment: %+v", response)
	}
	if content := readFile(t, filepath.Join(repo, "abandoned.txt")); content != "preserved abandonment\n" {
		t.Fatalf("abandonment lost edits: %q", content)
	}
	inspectedTask := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if inspectedTask.Result.Status != "canceled" || len(inspectedTask.Result.Claims) != 0 || len(inspectedTask.Result.OwnershipEvidence) != 1 {
		t.Fatalf("abandonment lost ownership evidence or active claim persisted: %+v", inspectedTask.Result)
	}
	repeated := runBandmaster(t, repo, "batch", "abandon", "--json")
	if repeated.exitCode != 0 || repeated.stdout != abandoned.stdout {
		t.Fatalf("abandonment was not byte-stable on retry:\nfirst=%s\nsecond=%s", abandoned.stdout, repeated.stdout)
	}
}

func TestBatchAbandonReconcilesInterruptedFinalizationBeforeReleasingClaims(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	started := successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Abandon finalization", "--intent", "Recover provisional state", "--expected-outcome", "Preserved work")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "finalization-abandon-worker")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "abandoned finalization\n")
	submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
	frozen := successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	crashed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_CRASH_FINALIZATION_AT=validating"}, "batch", "commit", "--json")
	if crashed.exitCode != 97 {
		t.Fatalf("did not create provisional finalization: %+v", crashed)
	}

	abandoned := runBandmaster(t, repo, "batch", "abandon", "--reason", "operator chose not to retry", "--confirmation", "confirmed interrupted process stopped", "--json")
	if abandoned.exitCode != 0 {
		t.Fatalf("finalization abandonment failed: %+v", abandoned)
	}
	response := decodeBatchAbandon(t, abandoned)
	if response.Result.BatchID != frozen.Result.ID || response.Result.FinalizationRecovery == nil || response.Result.FinalizationRecovery.Action != "rollback" || response.Result.FinalizationRecovery.Outcome != "rolled_back" || response.Result.Evidence.JournalStep != "validating" {
		t.Fatalf("abandonment did not retain finalization recovery evidence: %+v", response.Result)
	}
	if head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD")); head != started.Result.StartingCommit {
		t.Fatalf("abandonment retained provisional HEAD: %s", head)
	}
	if index := strings.TrimSpace(runGit(t, repo, "diff", "--cached", "--name-only")); index != "" {
		t.Fatalf("abandonment retained staged residue: %s", index)
	}
	if content := readFile(t, filepath.Join(repo, "owned.txt")); content != "abandoned finalization\n" {
		t.Fatalf("abandonment lost submitted edit: %q", content)
	}
	batch := successfulBatchCommand(t, repo, "inspect")
	if batch.Result.Status != "abandoned" || len(batch.Result.Manifest) != 1 {
		t.Fatalf("abandonment lost frozen evidence: %+v", batch.Result)
	}
}

func TestBatchAbandonFailsClosedOnAmbiguousFinalizationGitState(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Ambiguous abandon", "--intent", "Refuse unsafe cleanup", "--expected-outcome", "Quarantine")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "ambiguous-abandon-worker")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "submitted\n")
	submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
	successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	crashed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_CRASH_FINALIZATION_AT=prepared"}, "batch", "commit", "--json")
	if crashed.exitCode != 97 {
		t.Fatalf("did not create interrupted finalization: %+v", crashed)
	}
	writeFile(t, filepath.Join(repo, "external.txt"), "ambiguous staged state\n")
	runGit(t, repo, "add", "external.txt")

	refused := runBandmaster(t, repo, "batch", "abandon", "--reason", "cannot continue", "--confirmation", "process stopped", "--json")
	if refused.exitCode != 4 || !strings.Contains(refused.stdout, "batch_abandonment_quarantined") {
		t.Fatalf("ambiguous abandonment did not fail closed: %+v", refused)
	}
	if batch := successfulBatchCommand(t, repo, "inspect"); batch.Result.Status != "quarantined" {
		t.Fatalf("ambiguous batch was not left quarantined: %+v", batch.Result)
	}
}
