package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfficialValidationRunsFocusedThenApprovedRepositoryCommands(t *testing.T) {
	repo := repositoryWithValidation(t, `
  commands:
    - name: repository-check
      script: |
        printf 'repository:%s:%s\n' "$REPOSITORY_VALUE" "$PWD"
      timeout: 2s
      environment:
        REPOSITORY_VALUE: approved
`)
	focused := focusedValidationJSON(t, map[string]any{
		"name":        "focused-check",
		"script":      `printf 'focused:%s:%s\n' "$FOCUSED_VALUE" "$PWD"`,
		"timeout":     "2s",
		"environment": map[string]string{"FOCUSED_VALUE": "agent"},
	})
	taskID, batchID, startingCommit := frozenValidationBatch(t, repo, focused)

	validated := successfulBatchCommand(t, repo, "validate")
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatalf("resolve repository path: %v", err)
	}
	if validated.Result.ID != batchID || validated.Result.Status != "finalizing" || len(validated.Result.Validation) != 1 {
		t.Fatalf("unexpected validated batch: %+v", validated.Result)
	}
	attempt := validated.Result.Validation[0]
	if attempt.Status != "passed" || attempt.StartedAt == "" || attempt.FinishedAt == "" || len(attempt.Commands) != 2 {
		t.Fatalf("validation attempt was not recorded: %+v", attempt)
	}
	focusedRun := attempt.Commands[0]
	if focusedRun.Source != "focused" || focusedRun.TaskID != taskID || focusedRun.Name != "focused-check" || focusedRun.CommandOrder != 1 || focusedRun.WorkingDirectory != "." || focusedRun.Status != "passed" || focusedRun.ExitCode == nil || *focusedRun.ExitCode != 0 {
		t.Fatalf("unexpected focused validation record: %+v", focusedRun)
	}
	if focusedRun.EnvironmentOverrides["FOCUSED_VALUE"] != "agent" || focusedRun.ResolvedEnvironment["FOCUSED_VALUE"] != "agent" || focusedRun.ResolvedEnvironment["GIT_CONFIG_GLOBAL"] != "" {
		t.Fatalf("focused validation environment was not minimal plus overrides: %+v", focusedRun.ResolvedEnvironment)
	}
	if len(focusedRun.ResolvedArgv) != 3 || focusedRun.ResolvedArgv[0] != "/bin/sh" || focusedRun.ResolvedWorkingDirectory != resolvedRepo || !strings.Contains(focusedRun.Stdout, "focused:agent:"+resolvedRepo) || focusedRun.StdoutTruncated || focusedRun.StderrTruncated {
		t.Fatalf("focused command details were not resolved and captured: %+v", focusedRun)
	}
	repositoryRun := attempt.Commands[1]
	if repositoryRun.Source != "repository" || repositoryRun.TaskID != "" || repositoryRun.Name != "repository-check" || repositoryRun.CommandOrder != 2 || repositoryRun.WorkingDirectory != "." || repositoryRun.ResolvedWorkingDirectory != resolvedRepo || repositoryRun.Status != "passed" || !strings.Contains(repositoryRun.Stdout, "repository:approved:"+resolvedRepo) {
		t.Fatalf("unexpected repository validation record: %+v", repositoryRun)
	}
	if head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD")); head != startingCommit {
		t.Fatalf("validation created a commit: HEAD=%s want=%s", head, startingCommit)
	}
	retried := successfulBatchCommand(t, repo, "validate")
	if len(retried.Result.Validation) != 1 || retried.Result.Validation[0].StartedAt != attempt.StartedAt {
		t.Fatalf("successful validation retry was not idempotent: %+v", retried.Result.Validation)
	}
	inspected := successfulBatchCommand(t, repo, "inspect", batchID)
	if len(inspected.Result.Validation) != 1 || len(inspected.Result.Validation[0].Commands) != 2 {
		t.Fatalf("fresh invocation did not inspect validation outcomes: %+v", inspected.Result.Validation)
	}
}

func TestOfficialValidationFailureIsBoundedAndRepairPending(t *testing.T) {
	repo := repositoryWithValidation(t, `
  commands:
    - name: noisy-failure
      script: |
        dd if=/dev/zero bs=70000 count=1 2>/dev/null | tr '\000' x
        dd if=/dev/zero bs=70000 count=1 2>/dev/null | tr '\000' y >&2
        exit 7
      timeout: 10s
`)
	taskID, batchID, startingCommit := frozenValidationBatch(t, repo, "")

	result := runBandmaster(t, repo, "batch", "validate", "--json")
	response := assertBatchError(t, result, 5, "validation_failed", false)
	if response.SessionID == "" {
		t.Fatal("validation failure omitted its session identity")
	}
	inspected := successfulBatchCommand(t, repo, "inspect", batchID)
	if inspected.Result.Status != "repair_pending" || len(inspected.Result.Validation) != 1 || inspected.Result.Validation[0].Status != "failed" || len(inspected.Result.Validation[0].Commands) != 1 {
		t.Fatalf("failed validation did not persist repair-pending state: %+v", inspected.Result)
	}
	run := inspected.Result.Validation[0].Commands[0]
	if run.Status != "failed" || run.ExitCode == nil || *run.ExitCode != 7 || len(run.Stdout) != 64*1024 || len(run.Stderr) != 64*1024 || !run.StdoutTruncated || !run.StderrTruncated {
		t.Fatalf("failed validation output was not bounded and recorded: status=%s exit=%v stdout=%d stderr=%d stdout_truncated=%t stderr_truncated=%t", run.Status, run.ExitCode, len(run.Stdout), len(run.Stderr), run.StdoutTruncated, run.StderrTruncated)
	}
	if session := successfulSessionCommand(t, repo, "inspect"); session.Result.Status != "active" || session.Result.Monitor == nil || session.Result.Monitor.Status != "healthy" {
		t.Fatalf("ordinary validation failure did not return to monitored active repair: %+v", session.Result)
	}
	if task := successfulTaskCommand(t, repo, "inspect", taskID); task.Result.Status != "submitted" || len(task.Result.Claims) != 1 {
		t.Fatalf("ordinary validation failure changed task ownership before repair selection: %+v", task.Result)
	}
	if head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD")); head != startingCommit {
		t.Fatalf("failed validation created a commit: HEAD=%s want=%s", head, startingCommit)
	}
}

func TestOfficialValidationEnforcesTimeout(t *testing.T) {
	repo := repositoryWithValidation(t, `
  commands:
    - name: timeout-check
      argv: ["/bin/sh", "-c", "sleep 5"]
      timeout: 100ms
`)
	_, batchID, _ := frozenValidationBatch(t, repo, "")

	assertBatchError(t, runBandmaster(t, repo, "batch", "validate", "--json"), 5, "validation_failed", false)
	inspected := successfulBatchCommand(t, repo, "inspect", batchID)
	run := inspected.Result.Validation[0].Commands[0]
	if run.Status != "timed_out" || run.ExitCode != nil || run.DurationMilliseconds < 50 || run.DurationMilliseconds > 2500 {
		t.Fatalf("timeout outcome was not recorded reproducibly: %+v", run)
	}
}

func TestOfficialValidationMutationQuarantinesBatch(t *testing.T) {
	tests := []struct {
		name             string
		script           string
		wantKind         string
		wantPath         string
		restoreDirectory bool
	}{
		{name: "submitted path", script: "printf 'validation mutation\\n' > owned.txt", wantKind: "submitted_path_drift", wantPath: "owned.txt"},
		{name: "unsupported submitted type", script: "rm owned.txt && mkdir owned.txt", wantKind: "submitted_path_drift", wantPath: "owned.txt", restoreDirectory: true},
		{name: "unclaimed path", script: "printf 'unexpected\\n' > validation-output.txt", wantKind: "unclaimed_change", wantPath: "validation-output.txt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := repositoryWithValidation(t, "\n  commands:\n    - name: mutating-check\n      script: |\n        "+test.script+"\n      timeout: 2s\n")
			_, batchID, _ := frozenValidationBatch(t, repo, "")

			assertBatchError(t, runBandmaster(t, repo, "batch", "validate", "--json"), 4, test.wantKind, false)
			inspected := successfulBatchCommand(t, repo, "inspect", batchID)
			if inspected.Result.Status != "quarantined" || len(inspected.Result.Validation) != 1 || inspected.Result.Validation[0].Status != "integrity_violation" || inspected.Result.Validation[0].Commands[0].Status != "integrity_violation" {
				t.Fatalf("validation mutation did not quarantine its recorded attempt: %+v", inspected.Result)
			}
			session := successfulSessionCommand(t, repo, "inspect")
			if session.Result.Status != "paused" || len(session.Result.IntegrityViolations) == 0 {
				t.Fatalf("validation mutation did not pause with integrity evidence: %+v", session.Result)
			}
			violation := session.Result.IntegrityViolations[len(session.Result.IntegrityViolations)-1]
			if violation.Kind != test.wantKind || violation.Path != test.wantPath || violation.DetectedAt == "" {
				t.Fatalf("unexpected validation integrity evidence: %+v", violation)
			}
			if test.restoreDirectory {
				if err := os.Remove(filepath.Join(repo, "owned.txt")); err != nil {
					t.Fatalf("remove unsupported submitted type: %v", err)
				}
				writeFile(t, filepath.Join(repo, "owned.txt"), "submitted\n")
			} else if test.wantPath == "owned.txt" {
				writeFile(t, filepath.Join(repo, "owned.txt"), "submitted\n")
			} else if err := os.Remove(filepath.Join(repo, test.wantPath)); err != nil {
				t.Fatalf("remove validation mutation: %v", err)
			}
			recovered := runBandmaster(t, repo, "integrity", "recover", "--confirmation", "restored the frozen submitted snapshots after validation mutation", "--json")
			if recovered.exitCode != 0 {
				t.Fatalf("recover validation integrity: %+v", recovered)
			}
			if session := successfulSessionCommand(t, repo, "inspect"); session.Result.Status != "finalizing" {
				t.Fatalf("recovered validation batch was stranded outside finalization: %+v", session.Result)
			}
			if batch := successfulBatchCommand(t, repo, "inspect", batchID); batch.Result.Status != "frozen" {
				t.Fatalf("recovered validation batch was not made retryable: %+v", batch.Result)
			}
		})
	}
}

func repositoryWithValidation(t *testing.T, validationYAML string) string {
	t.Helper()
	repo := newGitRepository(t)
	writeFile(t, filepath.Join(repo, "README.md"), "# Validation fixture\n")
	writeFile(t, filepath.Join(repo, "owned.txt"), "baseline\n")
	writeFile(t, filepath.Join(repo, ".bandmaster.yaml"), "version: 2\nagent_lease_duration: 5m\nvalidation:"+validationYAML)
	initialized := runBandmaster(t, repo, "init", "--json")
	if initialized.exitCode != 0 {
		t.Fatalf("init validation repository: %+v", initialized)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Initialize validation fixture")
	approved := runBandmaster(t, repo, "config", "approve", responseDigest(t, initialized.stdout), "--json")
	if approved.exitCode != 0 {
		t.Fatalf("approve validation repository: %+v", approved)
	}
	return repo
}

func frozenValidationBatch(t *testing.T, repo, focusedValidation string) (string, string, string) {
	t.Helper()
	started := successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Validate work", "--intent", "Exercise official validation", "--expected-outcome", "Validation is recorded")
	assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-validation")
	claimArgs := []string{"claim", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", "owned.txt"}
	if focusedValidation != "" {
		claimArgs = append(claimArgs, "--validation", focusedValidation)
	}
	claimed := successfulTaskCommand(t, repo, claimArgs[0], claimArgs[1:]...)
	writeFile(t, filepath.Join(repo, "owned.txt"), "submitted\n")
	submitBatchTask(t, repo, task.Result.ID, assigned.Result.AssignmentToken)
	frozen := successfulBatchCommand(t, repo, "freeze")
	if frozen.Result.ID != claimed.Result.BatchID || frozen.Result.Status != "frozen" {
		t.Fatalf("unexpected frozen validation batch: %+v", frozen.Result)
	}
	return task.Result.ID, frozen.Result.ID, started.Result.StartingCommit
}

func focusedValidationJSON(t *testing.T, value map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode focused validation: %v", err)
	}
	return string(encoded)
}
