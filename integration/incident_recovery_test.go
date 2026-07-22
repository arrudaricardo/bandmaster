package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestProductionIncidentRecoversSafelyThroughPublicCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("incident proof requires Unix symlink semantics")
	}
	repo := repositoryWithValidation(t, `
  commands:
    - name: incident-shapes
      argv: ["/bin/sh", "-c", "test -f new-a.txt && test -f new-b.txt && test -f new-c.txt && test -f new-d.txt && test -f new-e.txt && test ! -e deleted.txt && test ! -e rename-source.txt && test -f rename-destination.txt && test -L linked"]
      timeout: 5s
`)
	writeFile(t, filepath.Join(repo, "deleted.txt"), "delete this\n")
	writeFile(t, filepath.Join(repo, "rename-source.txt"), "rename this\n")
	if err := os.Symlink("README.md", filepath.Join(repo, "linked")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "deleted.txt", "rename-source.txt", "linked")
	runGit(t, repo, "commit", "-m", "Add production incident fixtures")

	started := successfulSessionCommand(t, repo, "start")
	type agent struct {
		task       taskResponse
		assignment taskResponse
		paths      []string
	}
	agents := []agent{
		{
			task:  successfulTaskCommand(t, repo, "create", "--title", "Add project core", "--intent", "Create the new modules and modify owned content", "--expected-outcome", "Six exact path changes"),
			paths: []string{"owned.txt", "new-a.txt", "new-b.txt", "new-c.txt", "new-d.txt", "new-e.txt"},
		},
		{
			task:  successfulTaskCommand(t, repo, "create", "--title", "Remove and rename", "--intent", "Delete obsolete content and move a source", "--expected-outcome", "Deletion and rename are retained"),
			paths: []string{"deleted.txt", "rename-source.txt", "rename-destination.txt"},
		},
		{
			task:  successfulTaskCommand(t, repo, "create", "--title", "Retarget symlink", "--intent", "Update the linked entry", "--expected-outcome", "Symlink identity is retained"),
			paths: []string{"linked"},
		},
	}
	for index := range agents {
		agents[index].assignment = successfulTaskCommand(t, repo, "assign", agents[index].task.Result.ID, "--agent", "incident-agent-"+string(rune('a'+index)))
		for _, path := range agents[index].paths {
			successfulTaskCommand(t, repo, "claim", agents[index].task.Result.ID, "--token", agents[index].assignment.Result.AssignmentToken, "--path", path)
		}
	}

	writeFile(t, filepath.Join(repo, "owned.txt"), "incident modification\n")
	for index, path := range []string{"new-a.txt", "new-b.txt", "new-c.txt", "new-d.txt", "new-e.txt"} {
		writeFile(t, filepath.Join(repo, path), "new module "+string(rune('1'+index))+"\n")
	}
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
	for _, current := range agents {
		submitBatchTask(t, repo, current.task.Result.ID, current.assignment.Result.AssignmentToken)
	}

	frozen := successfulBatchCommand(t, repo, "freeze")
	if frozen.Result.Status != "frozen" || len(frozen.Result.Tasks) != 3 || len(frozen.Result.Manifest) != 10 {
		t.Fatalf("incident batch did not freeze every disjoint path: %+v", frozen.Result)
	}
	validated := successfulBatchCommand(t, repo, "validate")
	if validated.Result.Status != "finalizing" || len(validated.Result.Validation) == 0 || validated.Result.Validation[len(validated.Result.Validation)-1].Status != "passed" {
		t.Fatalf("official incident validation did not pass: %+v", validated.Result)
	}

	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	writeFile(t, hook, "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	failed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_FAIL_ROLLBACK_AT=after-normalize-index"}, "batch", "commit", "--json")
	if failed.exitCode != 4 {
		t.Fatalf("incident failure exit=%d stdout=%s stderr=%s", failed.exitCode, failed.stdout, failed.stderr)
	}
	var causal struct {
		Error struct {
			Code            string `json:"code"`
			InitiatingError struct {
				Code string `json:"code"`
			} `json:"initiating_error"`
			RollbackError struct {
				Operation string `json:"operation"`
			} `json:"rollback_error"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(failed.stdout), &causal); err != nil {
		t.Fatalf("decode causal failure: %v\n%s", err, failed.stdout)
	}
	if causal.Error.Code != "ambiguous_finalization_rollback" || causal.Error.InitiatingError.Code != "git_commit_failed" || causal.Error.RollbackError.Operation != "after-normalize-index" {
		t.Fatalf("incident response lost causal chain: %+v", causal.Error)
	}
	assertIncidentGitState(t, repo, started.Result.StartingCommit)

	beforeDoctorSession := successfulSessionCommand(t, repo, "inspect")
	beforeDoctorBatch := successfulBatchCommand(t, repo, "inspect")
	beforeDoctorTask := successfulTaskCommand(t, repo, "inspect", agents[0].task.Result.ID)
	beforeDoctorGit := runGit(t, repo, "status", "--porcelain=v1")
	diagnosis := decodeDoctor(t, runBandmaster(t, repo, "doctor", "--json"))
	journalEvidence := doctorFinding(t, diagnosis, "contradictory_finalization_journal")
	doctorFinding(t, diagnosis, "unresolved_integrity_violation")
	if len(journalEvidence) == 0 {
		t.Fatal("doctor omitted journal evidence")
	}
	if after := successfulSessionCommand(t, repo, "inspect"); !reflect.DeepEqual(after.Result, beforeDoctorSession.Result) {
		t.Fatalf("doctor mutated session state:\nbefore=%+v\nafter=%+v", beforeDoctorSession.Result, after.Result)
	}
	if after := successfulBatchCommand(t, repo, "inspect"); !reflect.DeepEqual(after.Result, beforeDoctorBatch.Result) {
		t.Fatalf("doctor mutated batch state:\nbefore=%+v\nafter=%+v", beforeDoctorBatch.Result, after.Result)
	}
	if after := successfulTaskCommand(t, repo, "inspect", agents[0].task.Result.ID); !reflect.DeepEqual(after.Result, beforeDoctorTask.Result) {
		t.Fatalf("doctor mutated task attribution:\nbefore=%+v\nafter=%+v", beforeDoctorTask.Result, after.Result)
	}
	if after := runGit(t, repo, "status", "--porcelain=v1"); after != beforeDoctorGit {
		t.Fatalf("doctor mutated Git state:\nbefore=%s\nafter=%s", beforeDoctorGit, after)
	}

	integrityRecovered := runBandmaster(t, repo, "integrity", "recover", "--confirmation", "rollback evidence inspected and repository invariants verified", "--json")
	if integrityRecovered.exitCode != 0 {
		t.Fatalf("integrity recovery failed: %+v", integrityRecovered)
	}
	restored := decodeSessionResponse(t, integrityRecovered.stdout)
	if restored.Result.Status != "finalizing" {
		t.Fatalf("integrity recovery did not restore finalizing session: %+v", restored.Result)
	}
	finalizationRecovered := runBandmaster(t, repo, "finalization", "recover", "--confirmation", "interrupted hook and monitor are stopped", "--json")
	if finalizationRecovered.exitCode != 0 {
		t.Fatalf("finalization recovery failed: %+v", finalizationRecovered)
	}
	recovery := decodeFinalizationRecovery(t, finalizationRecovered)
	if recovery.Result.Outcome != "rolled_back" || recovery.Result.After.SessionStatus != "active" || recovery.Result.After.BatchStatus != "repair_pending" {
		t.Fatalf("finalization recovery returned incompatible state: %+v", recovery.Result)
	}
	assertIncidentGitState(t, repo, started.Result.StartingCommit)

	preview := runBandmaster(t, repo, "session", "abort", "--dry-run", "--json")
	if preview.exitCode != 0 {
		t.Fatalf("abort preview failed: %+v", preview)
	}
	var abortPreview abortPlanResponse
	if err := json.Unmarshal([]byte(preview.stdout), &abortPreview); err != nil {
		t.Fatalf("decode abort preview: %v", err)
	}
	if len(abortPreview.Result.Blockers) != 1 || abortPreview.Result.Blockers[0].Code != "agent_termination_confirmation_required" || len(abortPreview.Result.ActiveClaims) != 10 || len(abortPreview.Result.PreservedArtifacts) == 0 {
		t.Fatalf("abort preview did not expose confirmation and preservation plan: %+v", abortPreview.Result)
	}
	abortedResult := runBandmaster(t, repo, "session", "abort", "--termination-confirmation", "all incident agent handles and hook process stopped", "--json")
	if abortedResult.exitCode != 0 {
		t.Fatalf("confirmed abort failed: %+v", abortedResult)
	}
	aborted := decodeSessionResponse(t, abortedResult.stdout)
	if aborted.Result.Status != "aborted" {
		t.Fatalf("incident session did not terminate: %+v", aborted.Result)
	}
	for _, current := range agents {
		inspected := successfulTaskCommand(t, repo, "inspect", current.task.Result.ID)
		if inspected.Result.Status != "quarantined" || len(inspected.Result.Claims) != 0 || len(inspected.Result.OwnershipEvidence) != len(current.paths) || inspected.Result.Submission == nil {
			t.Fatalf("abort lost immutable attribution for %s: %+v", current.task.Result.ID, inspected.Result)
		}
	}
	retainedBatch := successfulBatchCommand(t, repo, "inspect")
	if retainedBatch.Result.Status != "repair_pending" || len(retainedBatch.Result.Manifest) != 10 || len(retainedBatch.Result.AuditHistory) == 0 {
		t.Fatalf("abort lost batch evidence: %+v", retainedBatch.Result)
	}
	assertIncidentGitState(t, repo, started.Result.StartingCommit)
}

func assertIncidentGitState(t *testing.T, repo, wantHead string) {
	t.Helper()
	if head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD")); head != wantHead {
		t.Fatalf("incident HEAD=%s, want %s", head, wantHead)
	}
	if index := strings.TrimSpace(runGit(t, repo, "diff", "--cached", "--name-only")); index != "" {
		t.Fatalf("incident index not clean: %s", index)
	}
	if content := readFile(t, filepath.Join(repo, "owned.txt")); content != "incident modification\n" {
		t.Fatalf("modified content=%q", content)
	}
	for index, path := range []string{"new-a.txt", "new-b.txt", "new-c.txt", "new-d.txt", "new-e.txt"} {
		if content := readFile(t, filepath.Join(repo, path)); content != "new module "+string(rune('1'+index))+"\n" {
			t.Fatalf("new path %s content=%q", path, content)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "deleted.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted path restored unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "rename-source.txt")); !os.IsNotExist(err) {
		t.Fatalf("rename source restored unexpectedly: %v", err)
	}
	if content := readFile(t, filepath.Join(repo, "rename-destination.txt")); content != "rename this\n" {
		t.Fatalf("rename destination content=%q", content)
	}
	if target, err := os.Readlink(filepath.Join(repo, "linked")); err != nil || target != "owned.txt" {
		t.Fatalf("symlink target=%q err=%v", target, err)
	}
}
