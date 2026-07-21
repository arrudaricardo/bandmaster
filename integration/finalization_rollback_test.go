package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCommitBatchRollbackPreservesMixedPathStatesWithCleanIndex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink and executable-mode assertions require Unix Git semantics")
	}
	repo := repositoryWithValidation(t, "")
	writeFile(t, filepath.Join(repo, "deleted.txt"), "delete me\n")
	writeFile(t, filepath.Join(repo, "rename-source.txt"), "rename me\n")
	writeFile(t, filepath.Join(repo, "executable.sh"), "#!/bin/sh\necho baseline\n")
	if err := os.Symlink("README.md", filepath.Join(repo, "linked")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "deleted.txt", "rename-source.txt", "executable.sh", "linked")
	runGit(t, repo, "commit", "-m", "Add rollback fixtures")

	started := successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Preserve mixed edits", "--intent", "Exercise transactional rollback", "--expected-outcome", "Every edit survives unstaged")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "rollback-worker")
	paths := []string{"owned.txt", "created.txt", "deleted.txt", "rename-source.txt", "rename-destination.txt", "linked", "executable.sh", "hook-edit.txt"}
	for _, path := range paths {
		successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", path)
	}

	writeFile(t, filepath.Join(repo, "owned.txt"), "modified\n")
	writeFile(t, filepath.Join(repo, "created.txt"), "created\n")
	writeFile(t, filepath.Join(repo, "hook-edit.txt"), "submitted hook path\n")
	if err := os.Remove(filepath.Join(repo, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repo, "rename-source.txt"), filepath.Join(repo, "rename-destination.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("owned.txt", filepath.Join(repo, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repo, "executable.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
	successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeFile(t, hook, "#!/bin/sh\nprintf 'hook edit\\n' > hook-edit.txt\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}

	failed := runBandmaster(t, repo, "batch", "commit", "--json")
	if failed.exitCode != 3 || !strings.Contains(failed.stdout, "finalization_failed") {
		t.Fatalf("hook failure was not rolled back: exit=%d stdout=%s stderr=%s", failed.exitCode, failed.stdout, failed.stderr)
	}
	if branch := strings.TrimSpace(runGit(t, repo, "branch", "--show-current")); branch != started.Result.StartingBranch {
		t.Fatalf("rollback branch = %q, want %q", branch, started.Result.StartingBranch)
	}
	if head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD")); head != started.Result.StartingCommit {
		t.Fatalf("rollback HEAD = %q, want %q", head, started.Result.StartingCommit)
	}
	if index := strings.TrimSpace(runGit(t, repo, "diff", "--cached", "--name-only")); index != "" {
		t.Fatalf("rollback left staged paths: %s", index)
	}
	if content := readFile(t, filepath.Join(repo, "owned.txt")); content != "modified\n" {
		t.Fatalf("modified content = %q", content)
	}
	if content := readFile(t, filepath.Join(repo, "created.txt")); content != "created\n" {
		t.Fatalf("created content = %q", content)
	}
	if _, err := os.Stat(filepath.Join(repo, "deleted.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted path was not preserved as deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "rename-source.txt")); !os.IsNotExist(err) {
		t.Fatalf("rename source was not preserved as absent: %v", err)
	}
	if content := readFile(t, filepath.Join(repo, "rename-destination.txt")); content != "rename me\n" {
		t.Fatalf("rename destination content = %q", content)
	}
	if target, err := os.Readlink(filepath.Join(repo, "linked")); err != nil || target != "owned.txt" {
		t.Fatalf("symlink target = %q, err=%v", target, err)
	}
	info, err := os.Stat(filepath.Join(repo, "executable.sh"))
	if err != nil {
		t.Fatalf("inspect executable mode: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("executable mode was not preserved: mode=%v", info.Mode())
	}
	if content := readFile(t, filepath.Join(repo, "hook-edit.txt")); content != "hook edit\n" {
		t.Fatalf("hook edit content = %q", content)
	}
}

func TestCommitBatchRollbackFailureReportsInitiatingAndRecoveryErrors(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Expose rollback cause", "--intent", "Retain the complete failure chain", "--expected-outcome", "Structured causal errors")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "rollback-failure-worker")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "submitted\n")
	submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
	successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeFile(t, hook, "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}

	failed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_FAIL_ROLLBACK_AT=normalize-index"}, "batch", "commit", "--json")
	if failed.exitCode != 4 {
		t.Fatalf("rollback failure exit=%d stdout=%s stderr=%s", failed.exitCode, failed.stdout, failed.stderr)
	}
	var response struct {
		Error struct {
			Code            string `json:"code"`
			InitiatingError struct {
				Code string `json:"code"`
			} `json:"initiating_error"`
			RollbackError struct {
				Operation string `json:"operation"`
				Message   string `json:"message"`
			} `json:"rollback_error"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(failed.stdout), &response); err != nil {
		t.Fatalf("decode rollback error JSON: %v\n%s", err, failed.stdout)
	}
	if response.Error.Code != "ambiguous_finalization_rollback" || response.Error.InitiatingError.Code != "git_commit_failed" || response.Error.RollbackError.Operation != "normalize-index" || response.Error.RollbackError.Message == "" {
		t.Fatalf("incomplete rollback error chain: %+v", response.Error)
	}

	inspected := successfulSessionCommand(t, repo, "inspect")
	if len(inspected.Result.IntegrityViolations) == 0 {
		t.Fatal("rollback failure did not persist integrity evidence")
	}
	var evidence struct {
		InitiatingError struct {
			Code string `json:"code"`
		} `json:"initiating_error"`
		RollbackError struct {
			Operation string `json:"operation"`
		} `json:"rollback_error"`
	}
	if err := json.Unmarshal(inspected.Result.IntegrityViolations[0].ObservedState, &evidence); err != nil {
		t.Fatalf("decode rollback integrity evidence: %v", err)
	}
	if evidence.InitiatingError.Code != "git_commit_failed" || evidence.RollbackError.Operation != "normalize-index" {
		t.Fatalf("integrity evidence lost causal chain: %+v", evidence)
	}
}
