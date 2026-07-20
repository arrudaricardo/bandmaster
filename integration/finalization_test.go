package integration_test

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitBatchCreatesOrderedTaskCommitsAndCompletesNoOps(t *testing.T) {
	repo := repositoryWithValidation(t, "\n  commands:\n    - name: final-check\n      argv: [\"/bin/sh\", \"-c\", \"test -f owned.txt\"]\n      timeout: 2s\n")
	started := successfulSessionCommand(t, repo, "start")
	changed := successfulTaskCommand(t, repo, "create", "--title", "Change owned", "--intent", "Make the fixture change", "--expected-outcome", "Owned content changes")
	noOp := successfulTaskCommand(t, repo, "create", "--title", "Inspect fixture", "--intent", "Leave the fixture unchanged", "--expected-outcome", "No content changes")
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
	if task := successfulTaskCommand(t, repo, "inspect", changed.Result.ID); task.Result.Status != "committed" || len(task.Result.Claims) != 0 {
		t.Fatalf("changed task was not committed and released: %+v", task.Result)
	}
	if task := successfulTaskCommand(t, repo, "inspect", noOp.Result.ID); task.Result.Status != "no_op" || len(task.Result.Claims) != 0 {
		t.Fatalf("no-op task was not completed and released: %+v", task.Result)
	}
	if status := strings.TrimSpace(runGit(t, repo, "status", "--porcelain")); status != "" {
		t.Fatalf("committed batch left worktree dirty: %s", status)
	}
	log := runGit(t, repo, "log", "--format=%s", started.Result.StartingCommit+"..HEAD")
	if lines := strings.Fields(strings.TrimSpace(log)); len(lines) < 2 || strings.Join(lines[:2], " ") != "Bandmaster task" {
		t.Fatalf("task commit message is not deterministic: %q", log)
	}
	if retried := successfulBatchCommand(t, repo, "commit"); retried.Result.ID != frozen.Result.ID || retried.Result.Status != "committed" {
		t.Fatalf("committed batch was not idempotent: %+v", retried.Result)
	}
}
