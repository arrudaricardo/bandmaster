package integration_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestCommitBatchUsesFrozenManifestForMixedGitPathStates(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	writeFile(t, filepath.Join(repo, "deleted.txt"), "delete me\n")
	writeFile(t, filepath.Join(repo, "rename-source.txt"), "renamed content\n")
	writeFile(t, filepath.Join(repo, "executable.sh"), "#!/bin/sh\necho baseline\n")
	if err := os.Chmod(filepath.Join(repo, "executable.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("README.md", filepath.Join(repo, "linked")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "deleted.txt", "rename-source.txt", "executable.sh", "linked")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add manifest fixtures")

	started := successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Mix path states", "--intent", "Commit exact frozen manifest", "--expected-outcome", "Every Git path state is attributed")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "manifest-worker")
	paths := []string{"owned.txt", "created.txt", "deleted.txt", "rename-source.txt", "rename-destination.txt", "linked", "executable.sh"}
	for _, path := range paths {
		successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", path)
	}

	writeFile(t, filepath.Join(repo, "owned.txt"), "modified\n")
	writeFile(t, filepath.Join(repo, "created.txt"), "created\n")
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
	commitResult := runBandmaster(t, repo, "batch", "commit", "--json")
	if commitResult.exitCode != 0 {
		session := successfulSessionCommand(t, repo, "inspect")
		t.Fatalf("batch commit failed: exit=%d stdout=%s stderr=%s violations=%+v", commitResult.exitCode, commitResult.stdout, commitResult.stderr, session.Result.IntegrityViolations)
	}
	committed := decodeBatchResponse(t, commitResult.stdout)

	if committed.Result.Status != "committed" {
		t.Fatalf("batch status = %s, want committed", committed.Result.Status)
	}
	if count := strings.TrimSpace(runGit(t, repo, "rev-list", "--count", started.Result.StartingCommit+"..HEAD")); count != "1" {
		t.Fatalf("task produced %s commits, want exactly one", count)
	}
	wantPathList := append([]string(nil), paths...)
	sort.Strings(wantPathList)
	wantPaths := strings.Join(wantPathList, "\n")
	gotPaths := strings.TrimSpace(runGit(t, repo, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"))
	if gotPaths != wantPaths {
		t.Fatalf("committed paths:\n%s\nwant:\n%s", gotPaths, wantPaths)
	}
	if target := strings.TrimSpace(runGit(t, repo, "show", "HEAD:linked")); target != "owned.txt" {
		t.Fatalf("committed symlink target = %q", target)
	}
	if mode := strings.Fields(runGit(t, repo, "ls-tree", "HEAD", "executable.sh"))[0]; mode != "100755" {
		t.Fatalf("committed executable mode = %s", mode)
	}
	if status := strings.TrimSpace(runGit(t, repo, "status", "--porcelain")); status != "" {
		t.Fatalf("successful finalization left Git state dirty: %s", status)
	}
}

func TestCommitBatchRejectsPathDriftFromFrozenSubmittedSnapshot(t *testing.T) {
	repo := repositoryWithValidation(t, "")
	started := successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Freeze exact content", "--intent", "Reject post-validation drift", "--expected-outcome", "No commit is created")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "drift-worker")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "submitted\n")
	submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
	successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	writeFile(t, filepath.Join(repo, "owned.txt"), "drifted after validation\n")

	result := runBandmaster(t, repo, "batch", "commit", "--json")
	assertBatchError(t, result, 4, "submitted_path_drift", false)
	if head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD")); head != started.Result.StartingCommit {
		t.Fatalf("snapshot drift created commit %s, want %s", head, started.Result.StartingCommit)
	}
}
