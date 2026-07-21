package integration_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type finalizationRecoveryResponse struct {
	SchemaVersion string `json:"schema_version"`
	Command       string `json:"command"`
	Success       bool   `json:"success"`
	Result        struct {
		BatchID              string `json:"batch_id"`
		JournalStep          string `json:"journal_step"`
		Classification       string `json:"classification"`
		Action               string `json:"action"`
		Outcome              string `json:"outcome"`
		Idempotent           bool   `json:"idempotent"`
		OperatorConfirmation string `json:"operator_confirmation,omitempty"`
		Before               struct {
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
			PreBatchCommit string   `json:"pre_batch_commit"`
			ObservedHead   string   `json:"observed_head"`
			StagedPaths    []string `json:"staged_paths"`
			HookActivity   string   `json:"hook_activity"`
			MonitorStatus  string   `json:"monitor_status"`
			Reasons        []string `json:"reasons"`
		} `json:"evidence"`
	} `json:"result"`
}

func decodeFinalizationRecovery(t *testing.T, result commandResult) finalizationRecoveryResponse {
	t.Helper()
	var response finalizationRecoveryResponse
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("decode finalization recovery: %v\n%s", err, result.stdout)
	}
	return response
}

func TestCommitBatchCreatesOrderedTaskCommitsAndCompletesNoOps(t *testing.T) {
	repo := repositoryWithValidation(t, "\n  commands:\n    - name: final-check\n      argv: [\"/bin/sh\", \"-c\", \"test -f owned.txt\"]\n      timeout: 2s\n")
	started := successfulSessionCommand(t, repo, "start")
	changed := successfulTaskCommand(t, repo, "create", "--title", "Change owned", "--intent", "Make the fixture change", "--expected-outcome", "Owned content changes")
	noOp := successfulTaskCommand(t, repo, "create", "--title", "Inspect fixture", "--intent", "Leave the fixture unchanged", "--expected-outcome", "No content changes")
	dependent := successfulTaskCommand(t, repo, "create", "--title", "Use committed change", "--intent", "Start only after the changed task commits", "--expected-outcome", "Ready in the next batch", "--prerequisite", changed.Result.ID)
	changedAssignment := successfulTaskCommand(t, repo, "assign", changed.Result.ID, "--worker", "commit-changed")
	noOpAssignment := successfulTaskCommand(t, repo, "assign", noOp.Result.ID, "--worker", "commit-noop")
	successfulTaskCommand(t, repo, "claim", changed.Result.ID, "--token", changedAssignment.Result.AssignmentToken, "--path", "owned.txt")
	successfulTaskCommand(t, repo, "claim", noOp.Result.ID, "--token", noOpAssignment.Result.AssignmentToken, "--path", "README.md")
	writeFile(t, filepath.Join(repo, "owned.txt"), "committed content\n")
	submitBatchTask(t, repo, changed.Result.ID, changedAssignment.Result.AssignmentToken)
	submitBatchTask(t, repo, noOp.Result.ID, noOpAssignment.Result.AssignmentToken)
	frozen := successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	committed := successfulBatchCommand(t, repo, "commit")

	if committed.Result.Status != "committed" || len(committed.Result.Validation) != 2 || committed.Result.Validation[1].Status != "passed" {
		t.Fatalf("batch did not commit and run final validation: %+v", committed.Result)
	}
	if task := successfulTaskCommand(t, repo, "inspect", changed.Result.ID); task.Result.Status != "committed" || len(task.Result.Claims) != 0 || len(task.Result.OwnershipEvidence) != 1 || task.Result.OwnershipEvidence[0].SubmittedSnapshot == nil || task.Result.Submission == nil {
		t.Fatalf("changed task was not committed and released: %+v", task.Result)
	}
	if task := successfulTaskCommand(t, repo, "inspect", noOp.Result.ID); task.Result.Status != "no_op" || len(task.Result.Claims) != 0 || len(task.Result.OwnershipEvidence) != 1 || task.Result.OwnershipEvidence[0].SubmittedSnapshot == nil || task.Result.Submission == nil {
		t.Fatalf("no-op task was not completed and released: %+v", task.Result)
	}
	if task := successfulTaskCommand(t, repo, "inspect", dependent.Result.ID); task.Result.Status != "ready" || task.Result.BatchID != "" {
		t.Fatalf("dependent task was not released for a later batch: %+v", task.Result)
	}
	if status := strings.TrimSpace(runGit(t, repo, "status", "--porcelain")); status != "" {
		t.Fatalf("committed batch left worktree dirty: %s", status)
	}
	log := runGit(t, repo, "log", "--format=%s", started.Result.StartingCommit+"..HEAD")
	if lines := strings.Fields(strings.TrimSpace(log)); len(lines) < 2 || strings.Join(lines[:2], " ") != "Bandmaster task" {
		t.Fatalf("task commit message is not deterministic: %q", log)
	}
	if retried := successfulBatchCommand(t, repo, "commit"); retried.Result.ID != frozen.Result.ID || retried.Result.Status != "committed" || len(retried.Result.Manifest) != 2 {
		t.Fatalf("committed batch was not idempotent: %+v", retried.Result)
	}
}

func TestSessionFinishValidatesCleanCompletedWork(t *testing.T) {
	repo := repositoryWithValidation(t, "\n  commands:\n    - name: final-check\n      argv: [\"/bin/sh\", \"-c\", \"test -f README.md\"]\n      timeout: 2s\n")
	started := successfulSessionCommand(t, repo, "start")
	finished := successfulSessionCommand(t, repo, "finish")
	if finished.Result.Status != "completed" || finished.Result.ID != started.Result.ID {
		t.Fatalf("session was not completed: %+v", finished.Result)
	}
	if status := strings.TrimSpace(runGit(t, repo, "status", "--porcelain")); status != "" {
		t.Fatalf("completion left repository dirty: %s", status)
	}
}

func TestCommitBatchRollsBackHookFailureAndPreservesEdits(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	started := successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Change owned", "--intent", "Make the fixture change", "--expected-outcome", "Owned content changes")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "hook-worker")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "preserved content\n")
	submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
	successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeFile(t, hook, "#!/bin/sh\nprintf hook-change > hook-outside.txt\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	failed := runBandmaster(t, repo, "batch", "commit", "--json")
	if failed.exitCode == 0 || !strings.Contains(failed.stdout, "finalization") {
		t.Fatalf("hook failure was accepted: exit=%d stdout=%s", failed.exitCode, failed.stdout)
	}
	if head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD")); head != started.Result.StartingCommit {
		t.Fatalf("failed finalization changed HEAD: %s", head)
	}
	if content := readFile(t, filepath.Join(repo, "owned.txt")); content != "preserved content\n" {
		t.Fatalf("owned edit was not restored: %q", content)
	}
	if content := readFile(t, filepath.Join(repo, "hook-outside.txt")); content != "hook-change" {
		t.Fatalf("hook edit was not restored: %q", content)
	}
	if index := strings.TrimSpace(runGit(t, repo, "diff", "--cached", "--name-only")); index != "" {
		t.Fatalf("rollback left a staged index: %s", index)
	}
	if batch := successfulBatchCommand(t, repo, "inspect"); batch.Result.Status != "repair_pending" {
		t.Fatalf("ordinary hook failure did not enter repair pending: %+v", batch.Result)
	}
}

func TestCommitBatchQuarantinesHookChangeOutsideClaims(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Change owned", "--intent", "Make the fixture change", "--expected-outcome", "Owned content changes")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "hook-worker")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "worker content\n")
	submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
	successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeFile(t, hook, "#!/bin/sh\nprintf outside > outside.txt\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	failed := runBandmaster(t, repo, "batch", "commit", "--json")
	if failed.exitCode != 4 || !strings.Contains(failed.stdout, "integrity") {
		t.Fatalf("outside hook change was not quarantined: exit=%d stdout=%s", failed.exitCode, failed.stdout)
	}
	if content := readFile(t, filepath.Join(repo, "outside.txt")); content != "outside" {
		t.Fatalf("outside hook edit was not restored: %q", content)
	}
	if session := successfulSessionCommand(t, repo, "inspect"); session.Result.Status != "paused" {
		t.Fatalf("integrity failure did not pause the session: %+v", session.Result)
	}
}

func TestFinalizationRecoverRollsBackKnownInterruptedStepsAndIsIdempotent(t *testing.T) {
	for _, step := range []string{"prepared", "committing", "validating"} {
		t.Run(step, func(t *testing.T) {
			repo := repositoryWithValidation(t, "")
			started := successfulSessionCommand(t, repo, "start")
			task := successfulTaskCommand(t, repo, "create", "--title", "Crash recovery", "--intent", "Recover a stopped finalization", "--expected-outcome", "Repairable changes")
			assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "crash-worker")
			successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
			writeFile(t, filepath.Join(repo, "owned.txt"), "recoverable content\n")
			submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
			successfulBatchCommand(t, repo, "freeze")
			successfulBatchCommand(t, repo, "validate")
			crashed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_CRASH_FINALIZATION_AT=" + step}, "batch", "commit", "--json")
			if crashed.exitCode != 97 {
				t.Fatalf("finalization did not crash at %s: %+v", step, crashed)
			}
			ambiguousRetry := runBandmaster(t, repo, "batch", "commit", "--json")
			if ambiguousRetry.exitCode != 3 || !strings.Contains(ambiguousRetry.stdout, "finalization_recovery_required") {
				t.Fatalf("%s interruption still allowed ambiguous batch commit recovery: %+v", step, ambiguousRetry)
			}
			missingConfirmation := runBandmaster(t, repo, "finalization", "recover", "--json")
			if missingConfirmation.exitCode != 3 || !strings.Contains(missingConfirmation.stdout, "finalization_recovery_confirmation_required") {
				t.Fatalf("%s recovery did not require operator confirmation: %+v", step, missingConfirmation)
			}
			recovered := runBandmaster(t, repo, "finalization", "recover", "--confirmation", "inspected interrupted test process", "--json")
			if recovered.exitCode != 0 {
				session := successfulSessionCommand(t, repo, "inspect")
				t.Fatalf("fresh process did not recover %s: %+v violations=%+v", step, recovered, session.Result.IntegrityViolations)
			}
			response := decodeFinalizationRecovery(t, recovered)
			if response.SchemaVersion != "1" || response.Command != "finalization recover" || !response.Success || response.Result.JournalStep != step || response.Result.Classification != "recognized" || response.Result.Action != "rollback" || response.Result.Outcome != "rolled_back" || !response.Result.Idempotent {
				t.Fatalf("unexpected %s recovery JSON: %+v", step, response)
			}
			if response.Result.Before.SessionStatus != "finalizing" || response.Result.Before.BatchStatus != "finalizing" || response.Result.After.SessionStatus != "active" || response.Result.After.BatchStatus != "repair_pending" || response.Result.Evidence.ExpectedBranch == "" || response.Result.Evidence.ExpectedBranch != response.Result.Evidence.ObservedBranch || response.Result.Evidence.PreBatchCommit == "" || response.Result.Evidence.ObservedHead == "" || response.Result.Evidence.HookActivity != "stopped" || response.Result.Evidence.MonitorStatus != "stopped" || len(response.Result.Evidence.StagedPaths) != 0 {
				t.Fatalf("incomplete %s recovery evidence: %+v", step, response.Result)
			}
			repeated := runBandmaster(t, repo, "finalization", "recover", "--json")
			if repeated.exitCode != 0 || repeated.stdout != recovered.stdout {
				t.Fatalf("%s recovery was not stable and idempotent:\nfirst=%s\nsecond=%s", step, recovered.stdout, repeated.stdout)
			}
			if head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD")); head != started.Result.StartingCommit {
				t.Fatalf("%s recovery retained provisional commit %s", step, head)
			}
			if content := readFile(t, filepath.Join(repo, "owned.txt")); content != "recoverable content\n" {
				t.Fatalf("%s recovery lost edits: %q", step, content)
			}
			if batch := successfulBatchCommand(t, repo, "inspect"); batch.Result.Status != "repair_pending" {
				t.Fatalf("%s recovery did not require repair: %+v", step, batch.Result)
			}
		})
	}
}

func TestFinalizationRecoverDoesNotReplayAnOlderRecoveryForANewInterruption(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Recover twice", "--intent", "Recover distinct finalization attempts", "--expected-outcome", "Each interruption is recovered")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "first-worker")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "first attempt\n")
	submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
	successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	if crashed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_CRASH_FINALIZATION_AT=prepared"}, "batch", "commit", "--json"); crashed.exitCode != 97 {
		t.Fatalf("first finalization did not crash: %+v", crashed)
	}
	first := runBandmaster(t, repo, "finalization", "recover", "--confirmation", "inspected first interruption", "--json")
	if first.exitCode != 0 || decodeFinalizationRecovery(t, first).Result.JournalStep != "prepared" {
		t.Fatalf("first recovery failed: %+v", first)
	}

	successfulTaskCommand(t, repo, "repair", task.Result.ID,
		"--user-confirmation", "the first worker is stopped",
		"--diagnosis", "the first finalization was interrupted", "--intended-repair", "resubmit the preserved change",
	)
	replacement := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "second-worker")
	writeFile(t, filepath.Join(repo, "owned.txt"), "second attempt\n")
	submitBatchTask(t, repo, task.Result.ID, replacement.Result.AssignmentToken)
	successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	if crashed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_CRASH_FINALIZATION_AT=committing"}, "batch", "commit", "--json"); crashed.exitCode != 97 {
		t.Fatalf("second finalization did not crash: %+v", crashed)
	}
	second := runBandmaster(t, repo, "finalization", "recover", "--confirmation", "inspected second interruption", "--json")
	if second.exitCode != 0 {
		t.Fatalf("second recovery failed: %+v", second)
	}
	response := decodeFinalizationRecovery(t, second)
	if response.Result.JournalStep != "committing" || response.Result.OperatorConfirmation != "inspected second interruption" {
		t.Fatalf("second interruption replayed the older recovery: %+v", response.Result)
	}
}

func TestFinalizationRecoveryAuditFailureLeavesJournalRetryable(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Retry recovery audit", "--intent", "Keep recovery durable", "--expected-outcome", "Retry completes the same recovery")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "audit-worker")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "retryable\n")
	submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
	successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	if crashed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_CRASH_FINALIZATION_AT=prepared"}, "batch", "commit", "--json"); crashed.exitCode != 97 {
		t.Fatalf("finalization did not crash: %+v", crashed)
	}
	failed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_FAIL_ROLLBACK_AT=before-recovery-commit"}, "finalization", "recover", "--confirmation", "inspected retryable interruption", "--json")
	if failed.exitCode == 0 {
		t.Fatalf("injected recovery audit failure succeeded: %+v", failed)
	}
	if batch := successfulBatchCommand(t, repo, "inspect"); batch.Result.Status != "finalizing" {
		t.Fatalf("failed recovery consumed durable state: %+v", batch.Result)
	}
	retried := runBandmaster(t, repo, "finalization", "recover", "--confirmation", "inspected retryable interruption", "--json")
	if retried.exitCode != 0 || decodeFinalizationRecovery(t, retried).Result.Outcome != "rolled_back" {
		t.Fatalf("recovery was not retryable after audit failure: %+v", retried)
	}
}

func runBandmasterWithEnvironment(t *testing.T, dir string, environment []string, args ...string) commandResult {
	t.Helper()
	cmd := exec.Command(bandmasterBinary, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), environment...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return result
	}
	if exitError, ok := err.(*exec.ExitError); ok {
		result.exitCode = exitError.ExitCode()
		return result
	}
	t.Fatalf("run bandmaster with environment: %v", err)
	return result
}

func TestCommitBatchQuarantinesExternalGitStateAfterInterruption(t *testing.T) {
	mutations := map[string]func(*testing.T, string){
		"index": func(t *testing.T, repo string) {
			writeFile(t, filepath.Join(repo, "external.txt"), "staged\n")
			runGit(t, repo, "add", "external.txt")
		},
		"branch": func(t *testing.T, repo string) { runGit(t, repo, "checkout", "-b", "external") },
		"head": func(t *testing.T, repo string) {
			writeFile(t, filepath.Join(repo, "external.txt"), "committed\n")
			runGit(t, repo, "add", "external.txt")
			runGit(t, repo, "-c", "user.name=Tests", "-c", "user.email=tests@example.invalid", "commit", "-m", "external activity")
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			repo := repositoryWithValidation(t, "")
			successfulSessionCommand(t, repo, "start")
			task := successfulTaskCommand(t, repo, "create", "--title", "Interrupted", "--intent", "Leave a journal", "--expected-outcome", "Quarantine unknown state")
			assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "external-worker")
			successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
			writeFile(t, filepath.Join(repo, "owned.txt"), "worker content\n")
			submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
			successfulBatchCommand(t, repo, "freeze")
			successfulBatchCommand(t, repo, "validate")
			crashed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_CRASH_FINALIZATION_AT=prepared"}, "batch", "commit", "--json")
			if crashed.exitCode != 97 {
				t.Fatalf("did not create interrupted state: %+v", crashed)
			}
			mutate(t, repo)
			failed := runBandmaster(t, repo, "finalization", "recover", "--json")
			if failed.exitCode != 0 {
				t.Fatalf("%s state was accepted: %+v", name, failed)
			}
			response := decodeFinalizationRecovery(t, failed)
			if response.Result.Classification != "unknown" || response.Result.Action != "quarantine" || response.Result.Outcome != "quarantined" || response.Result.Evidence.ObservedBranch == "" || response.Result.Evidence.ObservedHead == "" {
				t.Fatalf("%s quarantine lacked structured evidence: %+v", name, response)
			}
			if session := successfulSessionCommand(t, repo, "inspect"); session.Result.Status != "paused" {
				t.Fatalf("%s state did not pause session: %+v", name, session.Result)
			}
		})
	}
}

func TestCommitBatchQuarantinesUnknownStateAfterInterruptedFinalization(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Change owned", "--intent", "Make the fixture change", "--expected-outcome", "Owned content changes")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "crash-worker")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "worker content\n")
	submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
	successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	// The hook kills Bandmaster (the parent of git), leaving its durable journal
	// behind while git may finish the commit without recording its SHA.
	writeFile(t, hook, "#!/bin/sh\nkill -9 $(ps -o ppid= -p \"$PPID\" | tr -d ' ')\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = runBandmaster(t, repo, "batch", "commit", "--json")
	failed := runBandmaster(t, repo, "finalization", "recover", "--json")
	if failed.exitCode != 0 {
		t.Fatalf("unknown interrupted finalization state was not quarantined: exit=%d stdout=%s", failed.exitCode, failed.stdout)
	}
	recovery := decodeFinalizationRecovery(t, failed)
	if recovery.Result.Classification != "unknown" || recovery.Result.Action != "quarantine" || recovery.Result.Outcome != "quarantined" || len(recovery.Result.Evidence.Reasons) == 0 {
		t.Fatalf("unknown interrupted finalization did not report quarantine evidence: %+v", recovery.Result)
	}
	if session := successfulSessionCommand(t, repo, "inspect"); session.Result.Status != "paused" {
		t.Fatalf("unknown interrupted finalization did not pause the session: %+v", session.Result)
	}
}

func TestCommitBatchIncludesAndAuditsStagedClaimHookChange(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Change owned", "--intent", "Make the fixture change", "--expected-outcome", "Owned content changes")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "hook-worker")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "worker content\n")
	submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
	successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeFile(t, hook, "#!/bin/sh\nprintf hook-content > owned.txt\ngit add owned.txt\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	successfulBatchCommand(t, repo, "commit")
	if content := strings.TrimSpace(runGit(t, repo, "show", "HEAD:owned.txt")); content != "hook-content" {
		t.Fatalf("staged hook content was not committed: %q", content)
	}
	inspected := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	found := false
	for _, event := range inspected.Result.AuditHistory {
		found = found || event.Event == "hook_change_committed"
	}
	if !found {
		t.Fatalf("committed hook change was not audited: %+v", inspected.Result.AuditHistory)
	}
}
