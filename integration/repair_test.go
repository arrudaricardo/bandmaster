package integration_test

import (
	"path/filepath"
	"testing"
)

func TestOrdinaryAgentFailureRetainsCollectingOwnershipForReplacement(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "owned.txt"), "baseline\n")
	runGit(t, repo, "add", "owned.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add repair fixture")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Repair partial work", "--intent", "Keep original attribution", "--expected-outcome", "Replacement continues safely")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-failed")
	claimed := successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
	writeFile(t, filepath.Join(repo, "owned.txt"), "partial edit\n")

	repairPending := successfulTaskCommand(t, repo, "repair", task.Result.ID,
		"--terminated-agent", "agent-failed",
		"--termination-proof", "codex-handle-agent-failed-stopped",
		"--diagnosis", "agent exited before completing the implementation",
		"--intended-repair", "finish the owned file and validate it",
	)
	if repairPending.Result.Status != "repair_pending" || repairPending.Result.AssignmentToken != "" || repairPending.Result.AgentIdentity != "" || len(repairPending.Result.Claims) != 1 {
		t.Fatalf("ordinary agent failure did not retain inactive ownership: %+v", repairPending.Result)
	}
	if repairPending.Result.Claims[0].Baseline.ContentHash != claimed.Result.Claims[0].Baseline.ContentHash || readFile(t, filepath.Join(repo, "owned.txt")) != "partial edit\n" {
		t.Fatalf("ordinary agent failure changed the baseline or partial edit: before=%+v after=%+v", claimed.Result.Claims, repairPending.Result.Claims)
	}
	event := repairPending.Result.AuditHistory[len(repairPending.Result.AuditHistory)-1]
	if event.Event != "task_repair_requested" || event.FromStatus != "editing" || event.ToStatus != "repair_pending" || event.AgentIdentity != "agent-failed" || event.TerminationProof != "codex-handle-agent-failed-stopped" || event.Diagnosis == "" || event.IntendedRepair == "" || len(event.RepairSnapshots) != 1 || event.RepairSnapshots[0].Path != "owned.txt" {
		t.Fatalf("ordinary failure repair evidence was not audited: %+v", event)
	}
	batch := successfulBatchCommand(t, repo, "inspect", claimed.Result.BatchID)
	if batch.Result.Status != "collecting" || len(batch.Result.Tasks) != 1 || batch.Result.Tasks[0].TaskID != task.Result.ID {
		t.Fatalf("ordinary failure closed or changed the collecting batch: %+v", batch.Result)
	}
	assertTaskError(t, runBandmaster(t, repo, "task", "heartbeat", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--json"), 3, "invalid_assignment_token", false)

	replacement := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-replacement")
	if replacement.Result.Status != "editing" || replacement.Result.AssignmentToken == "" || replacement.Result.AssignmentToken == assignment.Result.AssignmentToken || replacement.Result.BatchID != claimed.Result.BatchID || len(replacement.Result.Claims) != 1 {
		t.Fatalf("replacement did not inherit collecting ownership with a fresh token: %+v", replacement.Result)
	}
	linked := replacement.Result.AuditHistory[len(replacement.Result.AuditHistory)-2]
	if linked.Event != "task_repair_requested" || linked.ReplacementToken != replacement.Result.AssignmentToken {
		t.Fatalf("replacement token was not linked to repair evidence: %+v", linked)
	}
	expanded := successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", replacement.Result.AssignmentToken, "--path", "additional.txt")
	if len(expanded.Result.Claims) != 2 || expanded.Result.Claims[0].Path != "owned.txt" || expanded.Result.Claims[1].Path != "additional.txt" {
		t.Fatalf("replacement could not atomically expand retained ownership: %+v", expanded.Result.Claims)
	}
	writeFile(t, filepath.Join(repo, "owned.txt"), "baseline\n")
	released := successfulTaskCommand(t, repo, "release", task.Result.ID, "--token", replacement.Result.AssignmentToken, "--path", "owned.txt")
	if len(released.Result.Claims) != 1 || released.Result.Claims[0].Path != "additional.txt" {
		t.Fatalf("historical repair snapshots prevented safe claim release: %+v", released.Result.Claims)
	}
}

func TestFrozenBatchRepairReopensOriginalOwnersAndRequiresResubmission(t *testing.T) {
	repo := repositoryWithValidation(t, `
  commands:
    - name: repaired-content
      script: |
        grep -qx 'left repaired' left.txt && grep -qx 'right repaired' right.txt
      timeout: 2s
`)
	writeFile(t, filepath.Join(repo, "left.txt"), "left baseline\n")
	writeFile(t, filepath.Join(repo, "right.txt"), "right baseline\n")
	runGit(t, repo, "add", "left.txt", "right.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add multi-owner repair fixtures")
	started := successfulSessionCommand(t, repo, "start")
	left := successfulTaskCommand(t, repo, "create", "--title", "Repair left", "--intent", "Own left", "--expected-outcome", "Left passes validation")
	right := successfulTaskCommand(t, repo, "create", "--title", "Repair right", "--intent", "Own right", "--expected-outcome", "Right passes validation")
	leftAssignment := successfulTaskCommand(t, repo, "assign", left.Result.ID, "--agent", "agent-left-original")
	rightAssignment := successfulTaskCommand(t, repo, "assign", right.Result.ID, "--agent", "agent-right-original")
	leftClaimed := successfulTaskCommand(t, repo, "claim", left.Result.ID, "--token", leftAssignment.Result.AssignmentToken, "--path", "left.txt")
	rightClaimed := successfulTaskCommand(t, repo, "claim", right.Result.ID, "--token", rightAssignment.Result.AssignmentToken, "--path", "right.txt")
	writeFile(t, filepath.Join(repo, "left.txt"), "left broken\n")
	writeFile(t, filepath.Join(repo, "right.txt"), "right broken\n")
	submitBatchTask(t, repo, left.Result.ID, leftAssignment.Result.AssignmentToken)
	submitBatchTask(t, repo, right.Result.ID, rightAssignment.Result.AssignmentToken)
	frozen := successfulBatchCommand(t, repo, "freeze")
	assertBatchError(t, runBandmaster(t, repo, "batch", "validate", "--json"), 5, "validation_failed", false)

	leftRepair := successfulTaskCommand(t, repo, "repair", left.Result.ID,
		"--terminated-agent", "agent-left-original", "--termination-proof", "left agent stopped after submission",
		"--diagnosis", "left content fails the combined check", "--intended-repair", "replace left content",
	)
	if leftRepair.Result.Status != "repair_pending" || leftRepair.Result.Submission != nil || leftRepair.Result.Claims[0].SubmittedSnapshot != nil || leftRepair.Result.Claims[0].Baseline.ContentHash != leftClaimed.Result.Claims[0].Baseline.ContentHash {
		t.Fatalf("left repair did not invalidate only stale submission state: %+v", leftRepair.Result)
	}
	leftRepairEvent := leftRepair.Result.AuditHistory[len(leftRepair.Result.AuditHistory)-1]
	if leftRepairEvent.Invalidated == nil || leftRepairEvent.Invalidated.BehaviorChanged == "" || len(leftRepairEvent.RepairSnapshots) != 1 || leftRepairEvent.RepairSnapshots[0].InvalidatedSubmitted == nil {
		t.Fatalf("left repair did not retain invalidated submission history: %+v", leftRepairEvent)
	}
	repairing := successfulBatchCommand(t, repo, "inspect", frozen.Result.ID)
	if repairing.Result.Status != "repairing" || repairing.Result.BaseCommit != started.Result.StartingCommit || len(repairing.Result.Tasks) != 2 || len(repairing.Result.Manifest) != 0 {
		t.Fatalf("frozen repair changed base/ordered Batch Tasks or retained a stale manifest: %+v", repairing.Result)
	}
	if repairing.Result.Tasks[0].TaskID != left.Result.ID || repairing.Result.Tasks[1].TaskID != right.Result.ID || repairing.Result.Tasks[1].Status != "submitted" {
		t.Fatalf("repair changed ownership or reopened an unselected owner: %+v", repairing.Result.Tasks)
	}

	unrelated := successfulTaskCommand(t, repo, "create", "--title", "Unrelated", "--intent", "Must wait", "--expected-outcome", "Cannot join repair")
	assertTaskError(t, runBandmaster(t, repo, "task", "assign", unrelated.Result.ID, "--agent", "agent-unrelated", "--json"), 2, "batch_repair_in_progress", true)
	if inspected := successfulTaskCommand(t, repo, "inspect", unrelated.Result.ID); inspected.Result.Status != "ready" || inspected.Result.AssignmentToken != "" || inspected.Result.BatchID != "" || len(inspected.Result.Claims) != 0 {
		t.Fatalf("unrelated task joined the closed repair batch: %+v", inspected.Result)
	}

	rightRepair := successfulTaskCommand(t, repo, "repair", right.Result.ID,
		"--user-confirmation", "I confirmed agent-right-original is no longer running",
		"--diagnosis", "right content also fails the combined check", "--intended-repair", "replace right content",
	)
	if rightRepair.Result.Status != "repair_pending" || rightRepair.Result.Claims[0].Path != "right.txt" || rightRepair.Result.Claims[0].Baseline.ContentHash != rightClaimed.Result.Claims[0].Baseline.ContentHash {
		t.Fatalf("second owner was not independently reopened with retained ownership: %+v", rightRepair.Result)
	}
	assertBatchError(t, runBandmaster(t, repo, "batch", "freeze", "--json"), 2, "batch_task_not_submitted", true)

	leftReplacement := successfulTaskCommand(t, repo, "assign", left.Result.ID, "--agent", "agent-left-repair")
	rightReplacement := successfulTaskCommand(t, repo, "assign", right.Result.ID, "--agent", "agent-right-repair")
	leftExpanded := successfulTaskCommand(t, repo, "claim", left.Result.ID, "--token", leftReplacement.Result.AssignmentToken, "--path", "left-extra.txt")
	if len(leftExpanded.Result.Claims) != 2 || leftExpanded.Result.Claims[0].Path != "left.txt" || leftExpanded.Result.Claims[1].Path != "left-extra.txt" {
		t.Fatalf("frozen-batch repair could not expand into an unowned path: %+v", leftExpanded.Result.Claims)
	}
	writeFile(t, filepath.Join(repo, "left.txt"), "left repaired\n")
	writeFile(t, filepath.Join(repo, "left-extra.txt"), "repair detail\n")
	writeFile(t, filepath.Join(repo, "right.txt"), "right repaired\n")
	submitBatchTask(t, repo, left.Result.ID, leftReplacement.Result.AssignmentToken)
	assertBatchError(t, runBandmaster(t, repo, "batch", "freeze", "--json"), 2, "active_agents", true)
	submitBatchTask(t, repo, right.Result.ID, rightReplacement.Result.AssignmentToken)

	refrozen := successfulBatchCommand(t, repo, "freeze")
	if refrozen.Result.ID != frozen.Result.ID || refrozen.Result.BaseCommit != frozen.Result.BaseCommit || len(refrozen.Result.Tasks) != 2 || len(refrozen.Result.Manifest) != 3 {
		t.Fatalf("repair barrier did not preserve and rebuild the original batch: %+v", refrozen.Result)
	}
	for _, task := range refrozen.Result.Tasks {
		if task.Status != "submitted" {
			t.Fatalf("repair barrier admitted an unsubmitted owner: %+v", refrozen.Result.Tasks)
		}
	}
	validated := successfulBatchCommand(t, repo, "validate")
	if validated.Result.ID != frozen.Result.ID || validated.Result.Status != "finalizing" || len(validated.Result.Validation) != 2 || validated.Result.Validation[1].Status != "passed" {
		t.Fatalf("repaired batch did not rerun official validation: %+v", validated.Result)
	}
}
