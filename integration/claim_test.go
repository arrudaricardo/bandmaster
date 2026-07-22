package integration_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const focusedGoValidation = `{"name":"go-focused","argv":["go","test","./..."],"working_directory":".","timeout":"2m"}`

type preflightResponse struct {
	SchemaVersion string `json:"schema_version"`
	Command       string `json:"command"`
	Success       bool   `json:"success"`
	SessionID     string `json:"session_id"`
	Result        struct {
		TaskID            string `json:"task_id"`
		AssignmentValid   bool   `json:"assignment_valid"`
		RepositoryChanged bool   `json:"repository_changed"`
		Paths             []struct {
			Path     string `json:"path"`
			Baseline struct {
				Presence    string `json:"presence"`
				Type        string `json:"type"`
				ContentHash string `json:"content_hash"`
				Executable  bool   `json:"executable"`
			} `json:"baseline"`
		} `json:"paths"`
	} `json:"result"`
}

type diffResponse struct {
	SchemaVersion string `json:"schema_version"`
	Command       string `json:"command"`
	Success       bool   `json:"success"`
	SessionID     string `json:"session_id"`
	Result        struct {
		TaskID string `json:"task_id"`
		Paths  []struct {
			Path     string `json:"path"`
			Changed  bool   `json:"changed"`
			Patch    string `json:"patch"`
			Baseline struct {
				Presence    string `json:"presence"`
				Type        string `json:"type"`
				ContentHash string `json:"content_hash"`
				Executable  bool   `json:"executable"`
			} `json:"baseline"`
			Current struct {
				Presence    string `json:"presence"`
				Type        string `json:"type"`
				ContentHash string `json:"content_hash"`
				Executable  bool   `json:"executable"`
			} `json:"current"`
		} `json:"paths"`
	} `json:"result"`
}

func TestPreflightIsReadOnlyAndInitialClaimPersistsRegularFileBaselines(t *testing.T) {
	repo := approvedCleanRepository(t)
	existingContent := "#!/bin/sh\necho claimed\n"
	existingPath := filepath.Join(repo, "scripts", "claimed.sh")
	writeFile(t, existingPath, existingContent)
	if err := os.Chmod(existingPath, 0o755); err != nil {
		t.Fatalf("make fixture executable: %v", err)
	}
	runGit(t, repo, "add", "scripts/claimed.sh")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add claim fixtures")

	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create",
		"--title", "Claim regular files",
		"--intent", "Exercise ownership",
		"--expected-outcome", "Baselines persist",
	)
	assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-claim")
	args := []string{
		"task", "preflight", task.Result.ID,
		"--token", assigned.Result.AssignmentToken,
		"--path", "scripts/claimed.sh",
		"--path", "notes/new.txt",
		"--validation", focusedGoValidation,
		"--json",
	}
	preflightResult := runBandmaster(t, repo, args...)
	if preflightResult.exitCode != 0 {
		t.Fatalf("preflight exit code = %d, stdout = %s, stderr = %s", preflightResult.exitCode, preflightResult.stdout, preflightResult.stderr)
	}
	var preflight preflightResponse
	if err := json.Unmarshal([]byte(preflightResult.stdout), &preflight); err != nil {
		t.Fatalf("decode preflight: %v\n%s", err, preflightResult.stdout)
	}
	if !preflight.Success || preflight.Command != "task preflight" || preflight.Result.TaskID != task.Result.ID || !preflight.Result.AssignmentValid || preflight.Result.RepositoryChanged {
		t.Fatalf("unexpected preflight response: %+v", preflight)
	}
	if len(preflight.Result.Paths) != 2 {
		t.Fatalf("preflight path count = %d, want 2: %+v", len(preflight.Result.Paths), preflight.Result.Paths)
	}
	wantHash := sha256.Sum256([]byte(existingContent))
	if got := preflight.Result.Paths[0]; got.Path != "scripts/claimed.sh" || got.Baseline.Presence != "present" || got.Baseline.Type != "regular_file" || got.Baseline.ContentHash != "sha256:"+hex.EncodeToString(wantHash[:]) || !got.Baseline.Executable {
		t.Fatalf("unexpected existing-file baseline: %+v", got)
	}
	if got := preflight.Result.Paths[1]; got.Path != "notes/new.txt" || got.Baseline.Presence != "absent" || got.Baseline.Type != "absent" || got.Baseline.ContentHash != "" || got.Baseline.Executable {
		t.Fatalf("unexpected absent-file baseline: %+v", got)
	}

	stillAssigned := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if stillAssigned.Result.Status != "assigned" || stillAssigned.Result.CoreFrozen || stillAssigned.Result.BatchID != "" || len(stillAssigned.Result.Claims) != 0 || len(stillAssigned.Result.FocusedValidation) != 0 {
		t.Fatalf("read-only preflight changed durable task state: %+v", stillAssigned.Result)
	}

	claimArgs := append([]string{}, args...)
	claimArgs[1] = "claim"
	claimedResult := runBandmaster(t, repo, claimArgs...)
	if claimedResult.exitCode != 0 {
		t.Fatalf("claim exit code = %d, stdout = %s, stderr = %s", claimedResult.exitCode, claimedResult.stdout, claimedResult.stderr)
	}
	claimed := decodeTaskResponse(t, claimedResult.stdout)
	if !claimed.Success || claimed.Command != "task claim" || claimed.Result.Status != "editing" || !claimed.Result.CoreFrozen || claimed.Result.BatchID == "" {
		t.Fatalf("unexpected claim response: %+v", claimed)
	}
	if len(claimed.Result.Claims) != 2 || claimed.Result.Claims[0].Path != "scripts/claimed.sh" || claimed.Result.Claims[0].Baseline.ContentHash != "sha256:"+hex.EncodeToString(wantHash[:]) || claimed.Result.Claims[1].Path != "notes/new.txt" || claimed.Result.Claims[1].Baseline.Presence != "absent" {
		t.Fatalf("claims did not preserve preflight baselines: %+v", claimed.Result.Claims)
	}
	if len(claimed.Result.FocusedValidation) != 1 || claimed.Result.FocusedValidation[0].Name != "go-focused" || claimed.Result.FocusedValidation[0].WorkingDirectory != "." || claimed.Result.FocusedValidation[0].Timeout != "2m" {
		t.Fatalf("focused validation was not persisted: %+v", claimed.Result.FocusedValidation)
	}
	if inspected := successfulTaskCommand(t, repo, "inspect", task.Result.ID); inspected.Result.Status != "editing" || len(inspected.Result.Claims) != 2 || inspected.Result.BatchID != claimed.Result.BatchID {
		t.Fatalf("fresh invocation did not observe claimed state: %+v", inspected.Result)
	}
}

func TestInitialClaimRequiresTheOwningTokenAndGrantsNoPartialWriteSet(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "owned.txt"), "baseline\n")
	if err := os.Chmod(filepath.Join(repo, "owned.txt"), 0o654); err != nil {
		t.Fatalf("set non-Git-executable fixture mode: %v", err)
	}
	runGit(t, repo, "add", "owned.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add ownership fixture")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Atomic claim", "--intent", "Own all paths", "--expected-outcome", "No partial ownership")
	assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-atomic")

	stale := runBandmaster(t, repo, "task", "claim", task.Result.ID,
		"--token", "assignment_stale",
		"--path", "owned.txt",
		"--json",
	)
	assertTaskError(t, stale, 3, "invalid_assignment_token", false)
	afterStale := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if afterStale.Result.Status != "assigned" || afterStale.Result.CoreFrozen || len(afterStale.Result.Claims) != 0 {
		t.Fatalf("stale token changed durable task state: %+v", afterStale.Result)
	}

	partial := runBandmaster(t, repo, "task", "claim", task.Result.ID,
		"--token", assigned.Result.AssignmentToken,
		"--path", "owned.txt",
		"--path", ".agents",
		"--json",
	)
	assertTaskError(t, partial, 3, "unsupported_claim_path", false)
	afterPartial := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if afterPartial.Result.Status != "assigned" || afterPartial.Result.CoreFrozen || afterPartial.Result.BatchID != "" || len(afterPartial.Result.Claims) != 0 {
		t.Fatalf("failed multi-path claim granted partial ownership: %+v", afterPartial.Result)
	}

	claimed := successfulTaskCommand(t, repo, "claim", task.Result.ID,
		"--token", assigned.Result.AssignmentToken,
		"--path", "owned.txt",
		"--path", "generated",
	)
	if claimed.Result.Status != "editing" || len(claimed.Result.Claims) != 2 || claimed.Result.Claims[0].Path != "owned.txt" || claimed.Result.Claims[0].Baseline.Executable {
		t.Fatalf("valid owning token did not acquire the claim: %+v", claimed.Result)
	}

	contender := successfulTaskCommand(t, repo, "create", "--title", "Conflicting claim", "--intent", "Test path conflicts", "--expected-outcome", "No partial nested claim")
	contenderAssignment := successfulTaskCommand(t, repo, "assign", contender.Result.ID, "--agent", "agent-contender")
	conflict := runBandmaster(t, repo, "task", "claim", contender.Result.ID,
		"--token", contenderAssignment.Result.AssignmentToken,
		"--path", "free.txt",
		"--path", "generated/nested.txt",
		"--json",
	)
	assertTaskError(t, conflict, 2, "claim_unavailable", true)
	unchangedContender := successfulTaskCommand(t, repo, "inspect", contender.Result.ID)
	if unchangedContender.Result.Status != "blocked" || unchangedContender.Result.AgentIdentity != "" || unchangedContender.Result.AssignmentToken != "" || unchangedContender.Result.BatchID != "" || len(unchangedContender.Result.Claims) != 0 {
		t.Fatalf("conflicting write set granted partial claims: %+v", unchangedContender.Result)
	}
	lastEvent := unchangedContender.Result.AuditHistory[len(unchangedContender.Result.AuditHistory)-1]
	if lastEvent.Event != "task_blocked" || lastEvent.FromStatus != "assigned" || lastEvent.ToStatus != "blocked" {
		t.Fatalf("conflicting write set was not durably blocked: %+v", unchangedContender.Result.AuditHistory)
	}

	replan := runBandmaster(t, repo, "task", "replan", task.Result.ID,
		"--title", "Changed meaning",
		"--intent", "Should fail",
		"--expected-outcome", "Meaning remains frozen",
		"--json",
	)
	assertTaskError(t, replan, 3, "task_core_frozen", false)
}

func TestSeparateAgentProcessesAcquireDisjointClaimsConcurrently(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "left.txt"), "left baseline\n")
	writeFile(t, filepath.Join(repo, "right.txt"), "right baseline\n")
	runGit(t, repo, "add", "left.txt", "right.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add concurrent claim fixtures")
	successfulSessionCommand(t, repo, "start")

	left := successfulTaskCommand(t, repo, "create", "--title", "Edit left", "--intent", "Change the left file", "--expected-outcome", "Left is owned")
	right := successfulTaskCommand(t, repo, "create", "--title", "Edit right", "--intent", "Change the right file", "--expected-outcome", "Right is owned")
	leftAssignment := successfulTaskCommand(t, repo, "assign", left.Result.ID, "--agent", "agent-left")
	rightAssignment := successfulTaskCommand(t, repo, "assign", right.Result.ID, "--agent", "agent-right")

	leftCommand := newBandmasterCommand(repo, "task", "claim", left.Result.ID, "--token", leftAssignment.Result.AssignmentToken, "--path", "left.txt", "--json")
	rightCommand := newBandmasterCommand(repo, "task", "claim", right.Result.ID, "--token", rightAssignment.Result.AssignmentToken, "--path", "right.txt", "--json")
	if err := leftCommand.command.Start(); err != nil {
		t.Fatalf("start left agent: %v", err)
	}
	if err := rightCommand.command.Start(); err != nil {
		t.Fatalf("start right agent: %v", err)
	}
	leftResult := waitBandmasterCommand(t, leftCommand)
	rightResult := waitBandmasterCommand(t, rightCommand)
	if leftResult.exitCode != 0 || rightResult.exitCode != 0 {
		t.Fatalf("concurrent disjoint claims failed: left=%+v right=%+v", leftResult, rightResult)
	}

	leftTask := decodeTaskResponse(t, leftResult.stdout)
	rightTask := decodeTaskResponse(t, rightResult.stdout)
	if leftTask.Result.Status != "editing" || rightTask.Result.Status != "editing" || leftTask.Result.BatchID == "" || leftTask.Result.BatchID != rightTask.Result.BatchID {
		t.Fatalf("disjoint agents did not join one collecting batch: left=%+v right=%+v", leftTask.Result, rightTask.Result)
	}
	writeFile(t, filepath.Join(repo, "left.txt"), "left changed\n")
	writeFile(t, filepath.Join(repo, "right.txt"), "right changed\n")
	if diff := runBandmaster(t, repo, "task", "diff", left.Result.ID, "--token", leftAssignment.Result.AssignmentToken, "--json"); diff.exitCode != 0 {
		t.Fatalf("left agent could not inspect its concurrent edit: %+v", diff)
	}
	if diff := runBandmaster(t, repo, "task", "diff", right.Result.ID, "--token", rightAssignment.Result.AssignmentToken, "--json"); diff.exitCode != 0 {
		t.Fatalf("right agent could not inspect its concurrent edit: %+v", diff)
	}
}

func TestConcurrentOverlappingClaimsLeaveOneBlockedAgentClaimless(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "shared.txt"), "baseline\n")
	runGit(t, repo, "add", "shared.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add overlapping claim fixture")
	successfulSessionCommand(t, repo, "start")

	first := successfulTaskCommand(t, repo, "create", "--title", "First contender", "--intent", "Claim shared path", "--expected-outcome", "One agent owns it")
	second := successfulTaskCommand(t, repo, "create", "--title", "Second contender", "--intent", "Claim shared path", "--expected-outcome", "One agent blocks")
	firstAssignment := successfulTaskCommand(t, repo, "assign", first.Result.ID, "--agent", "agent-first-overlap")
	secondAssignment := successfulTaskCommand(t, repo, "assign", second.Result.ID, "--agent", "agent-second-overlap")

	firstCommand := newBandmasterCommand(repo, "task", "claim", first.Result.ID, "--token", firstAssignment.Result.AssignmentToken, "--path", "first-free.txt", "--path", "shared.txt", "--json")
	secondCommand := newBandmasterCommand(repo, "task", "claim", second.Result.ID, "--token", secondAssignment.Result.AssignmentToken, "--path", "second-free.txt", "--path", "shared.txt", "--json")
	if err := firstCommand.command.Start(); err != nil {
		t.Fatalf("start first contender: %v", err)
	}
	if err := secondCommand.command.Start(); err != nil {
		t.Fatalf("start second contender: %v", err)
	}
	results := []commandResult{waitBandmasterCommand(t, firstCommand), waitBandmasterCommand(t, secondCommand)}
	successCount := 0
	blockedCount := 0
	for _, result := range results {
		switch result.exitCode {
		case 0:
			successCount++
		case 2:
			blockedCount++
			assertTaskError(t, result, 2, "claim_unavailable", true)
		default:
			t.Fatalf("unexpected overlapping claim result: %+v", result)
		}
	}
	if successCount != 1 || blockedCount != 1 {
		t.Fatalf("overlapping claims produced %d successes and %d blocked outcomes", successCount, blockedCount)
	}
	firstState := successfulTaskCommand(t, repo, "inspect", first.Result.ID)
	secondState := successfulTaskCommand(t, repo, "inspect", second.Result.ID)
	states := []taskResponse{firstState, secondState}
	for _, state := range states {
		if state.Result.Status == "blocked" && (len(state.Result.Claims) != 0 || state.Result.BatchID != "" || state.Result.AssignmentToken != "") {
			t.Fatalf("blocked concurrent contender retained partial ownership: %+v", state.Result)
		}
		if state.Result.Status == "editing" && len(state.Result.Claims) != 2 {
			t.Fatalf("winning concurrent contender did not receive its complete write set: %+v", state.Result)
		}
	}
}

func TestClaimExpansionReleaseAndBlockedTaskRequeuePreserveOwnership(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "owned.txt"), "baseline\n")
	writeFile(t, filepath.Join(repo, "busy.txt"), "busy baseline\n")
	runGit(t, repo, "add", "owned.txt", "busy.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add expansion fixtures")
	successfulSessionCommand(t, repo, "start")

	owner := successfulTaskCommand(t, repo, "create", "--title", "Own paths", "--intent", "Exercise expansion", "--expected-outcome", "Claims stay atomic")
	ownerAssignment := successfulTaskCommand(t, repo, "assign", owner.Result.ID, "--agent", "agent-owner")
	successfulTaskCommand(t, repo, "claim", owner.Result.ID, "--token", ownerAssignment.Result.AssignmentToken, "--path", "owned.txt")
	expanded := successfulTaskCommand(t, repo, "claim", owner.Result.ID,
		"--token", ownerAssignment.Result.AssignmentToken,
		"--path", "unused.txt",
		"--path", "expanded.txt",
	)
	if expanded.Result.Status != "editing" || len(expanded.Result.Claims) != 3 || expanded.Result.Claims[1].Path != "unused.txt" || expanded.Result.Claims[2].Path != "expanded.txt" {
		t.Fatalf("editing task did not atomically expand its write set: %+v", expanded.Result)
	}

	busyOwner := successfulTaskCommand(t, repo, "create", "--title", "Own busy path", "--intent", "Create contention", "--expected-outcome", "Busy path is unavailable")
	busyAssignment := successfulTaskCommand(t, repo, "assign", busyOwner.Result.ID, "--agent", "agent-busy")
	successfulTaskCommand(t, repo, "claim", busyOwner.Result.ID, "--token", busyAssignment.Result.AssignmentToken, "--path", "busy.txt")

	failedExpansion := runBandmaster(t, repo, "task", "claim", owner.Result.ID,
		"--token", ownerAssignment.Result.AssignmentToken,
		"--path", "free.txt",
		"--path", "busy.txt",
		"--json",
	)
	assertTaskError(t, failedExpansion, 2, "claim_unavailable", true)
	afterFailedExpansion := successfulTaskCommand(t, repo, "inspect", owner.Result.ID)
	if afterFailedExpansion.Result.Status != "editing" || len(afterFailedExpansion.Result.Claims) != 3 {
		t.Fatalf("failed expansion changed existing ownership: %+v", afterFailedExpansion.Result)
	}

	writeFile(t, filepath.Join(repo, "owned.txt"), "changed\n")
	changedRelease := runBandmaster(t, repo, "task", "release", owner.Result.ID, "--token", ownerAssignment.Result.AssignmentToken, "--path", "owned.txt", "--json")
	assertTaskError(t, changedRelease, 3, "claim_changed", false)
	if afterChangedRelease := successfulTaskCommand(t, repo, "inspect", owner.Result.ID); len(afterChangedRelease.Result.Claims) != 3 {
		t.Fatalf("changed claim was released: %+v", afterChangedRelease.Result.Claims)
	}

	released := successfulTaskCommand(t, repo, "release", owner.Result.ID, "--token", ownerAssignment.Result.AssignmentToken, "--path", "unused.txt")
	if released.Result.Status != "editing" || len(released.Result.Claims) != 2 || released.Result.Claims[0].Path != "owned.txt" || released.Result.Claims[1].Path != "expanded.txt" {
		t.Fatalf("unchanged claim was not released independently: %+v", released.Result)
	}
	if len(released.Result.OwnershipEvidence) != 3 || released.Result.OwnershipEvidence[1].Path != "unused.txt" {
		t.Fatalf("claim release discarded immutable ownership evidence: %+v", released.Result.OwnershipEvidence)
	}

	blockedTask := successfulTaskCommand(t, repo, "create", "--title", "Wait for busy path", "--intent", "Retry after release", "--expected-outcome", "Requeue succeeds")
	blockedAssignment := successfulTaskCommand(t, repo, "assign", blockedTask.Result.ID, "--agent", "agent-blocked")
	blockedClaim := runBandmaster(t, repo, "task", "claim", blockedTask.Result.ID,
		"--token", blockedAssignment.Result.AssignmentToken,
		"--path", "available-after-requeue.txt",
		"--path", "busy.txt",
		"--json",
	)
	assertTaskError(t, blockedClaim, 2, "claim_unavailable", true)
	successfulTaskCommand(t, repo, "release", busyOwner.Result.ID, "--token", busyAssignment.Result.AssignmentToken, "--path", "busy.txt")
	requeued := successfulTaskCommand(t, repo, "requeue", blockedTask.Result.ID)
	if requeued.Result.Status != "ready" || requeued.Result.AssignmentToken != "" || len(requeued.Result.Claims) != 0 {
		t.Fatalf("blocked task was not returned claimless to ready: %+v", requeued.Result)
	}
	reassigned := successfulTaskCommand(t, repo, "assign", blockedTask.Result.ID, "--agent", "agent-requeued")
	if reassigned.Result.AssignmentToken == "" || reassigned.Result.AssignmentToken == blockedAssignment.Result.AssignmentToken {
		t.Fatalf("requeued task did not receive a fresh token: old=%q new=%q", blockedAssignment.Result.AssignmentToken, reassigned.Result.AssignmentToken)
	}
	retried := successfulTaskCommand(t, repo, "claim", blockedTask.Result.ID,
		"--token", reassigned.Result.AssignmentToken,
		"--path", "available-after-requeue.txt",
		"--path", "busy.txt",
	)
	if retried.Result.Status != "editing" || len(retried.Result.Claims) != 2 {
		t.Fatalf("requeued task could not acquire released write set: %+v", retried.Result)
	}
}

func TestAgentActionsRenewLeasesAndExplicitHeartbeatSupportsLongEdits(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "leased.txt"), "baseline\n")
	runGit(t, repo, "add", "leased.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add lease fixture")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Lease work", "--intent", "Keep ownership active", "--expected-outcome", "Every action renews")
	assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-lease")
	if assigned.Result.Lease == nil || assigned.Result.Lease.Status != "active" || assigned.Result.Lease.RenewedAt == "" || assigned.Result.Lease.ExpiresAt == "" {
		t.Fatalf("assignment did not create an active lease: %+v", assigned.Result.Lease)
	}

	time.Sleep(10 * time.Millisecond)
	preflight := runBandmaster(t, repo, "task", "preflight", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", "leased.txt", "--json")
	if preflight.exitCode != 0 {
		t.Fatalf("preflight failed: %+v", preflight)
	}
	afterPreflight := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if afterPreflight.Result.Lease == nil || afterPreflight.Result.Lease.RenewedAt <= assigned.Result.Lease.RenewedAt {
		t.Fatalf("preflight did not renew the agent lease: before=%+v after=%+v", assigned.Result.Lease, afterPreflight.Result.Lease)
	}

	time.Sleep(10 * time.Millisecond)
	claimed := successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", "leased.txt")
	if claimed.Result.Lease == nil || claimed.Result.Lease.RenewedAt <= afterPreflight.Result.Lease.RenewedAt {
		t.Fatalf("claim did not renew the agent lease: before=%+v after=%+v", afterPreflight.Result.Lease, claimed.Result.Lease)
	}

	time.Sleep(10 * time.Millisecond)
	heartbeat := successfulTaskCommand(t, repo, "heartbeat", task.Result.ID, "--token", assigned.Result.AssignmentToken)
	if heartbeat.Result.Lease == nil || heartbeat.Result.Lease.Status != "active" || heartbeat.Result.Lease.RenewedAt <= claimed.Result.Lease.RenewedAt {
		t.Fatalf("explicit heartbeat did not renew the agent lease: before=%+v after=%+v", claimed.Result.Lease, heartbeat.Result.Lease)
	}

	writeFile(t, filepath.Join(repo, "leased.txt"), "changed\n")
	time.Sleep(10 * time.Millisecond)
	if diff := runBandmaster(t, repo, "task", "diff", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--json"); diff.exitCode != 0 {
		t.Fatalf("diff failed: %+v", diff)
	}
	afterDiff := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if afterDiff.Result.Lease == nil || afterDiff.Result.Lease.RenewedAt <= heartbeat.Result.Lease.RenewedAt {
		t.Fatalf("diff did not renew the agent lease: before=%+v after=%+v", heartbeat.Result.Lease, afterDiff.Result.Lease)
	}
}

func TestExpiredLeaseQuarantinesClaimsUntilAuditedRecoveryAndReplacement(t *testing.T) {
	repo := approvedCleanRepositoryWithLeaseDuration(t, "5s")
	writeFile(t, filepath.Join(repo, "quarantined.txt"), "baseline\n")
	runGit(t, repo, "add", "quarantined.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add quarantine fixture")
	successfulSessionCommand(t, repo, "start")
	owner := successfulTaskCommand(t, repo, "create", "--title", "Quarantine work", "--intent", "Retain abandoned ownership", "--expected-outcome", "Replacement inherits claims")
	assignment := successfulTaskCommand(t, repo, "assign", owner.Result.ID, "--agent", "agent-expiring")
	claimed := successfulTaskCommand(t, repo, "claim", owner.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "quarantined.txt")
	writeFile(t, filepath.Join(repo, "quarantined.txt"), "partial edit\n")

	expiresAt, err := time.Parse(time.RFC3339Nano, claimed.Result.Lease.ExpiresAt)
	if err != nil {
		t.Fatalf("parse lease expiry: %v", err)
	}
	if wait := time.Until(expiresAt) + 150*time.Millisecond; wait > 0 {
		time.Sleep(wait)
	}
	expiredAssignment := runBandmaster(t, repo, "task", "assign", owner.Result.ID, "--agent", "agent-expiring", "--json")
	assertTaskError(t, expiredAssignment, 4, "lease_expired", false)
	quarantined := successfulTaskCommand(t, repo, "inspect", owner.Result.ID)
	if quarantined.Result.Status != "quarantined" || quarantined.Result.Lease == nil || quarantined.Result.Lease.Status != "expired" || len(quarantined.Result.Claims) != 1 || quarantined.Result.Claims[0].Baseline.Presence != "present" {
		t.Fatalf("expiry did not quarantine retained ownership: %+v", quarantined.Result)
	}

	contender := successfulTaskCommand(t, repo, "create", "--title", "Contend after expiry", "--intent", "Prove quarantine remains exclusive", "--expected-outcome", "Claim stays unavailable")
	contenderAssignment := successfulTaskCommand(t, repo, "assign", contender.Result.ID, "--agent", "agent-contender-after-expiry")
	contention := runBandmaster(t, repo, "task", "claim", contender.Result.ID, "--token", contenderAssignment.Result.AssignmentToken, "--path", "quarantined.txt", "--json")
	assertTaskError(t, contention, 2, "claim_unavailable", true)

	missingEvidence := runBandmaster(t, repo, "task", "recover", owner.Result.ID, "--json")
	assertTaskError(t, missingEvidence, 3, "agent_termination_required", false)
	wrongAgent := runBandmaster(t, repo, "task", "recover", owner.Result.ID, "--terminated-agent", "agent-other", "--termination-proof", "stopped", "--json")
	assertTaskError(t, wrongAgent, 3, "agent_termination_mismatch", false)
	recovered := successfulTaskCommand(t, repo, "recover", owner.Result.ID,
		"--user-confirmation", "I confirmed agent-expiring is no longer running",
		"--diagnosis", "the agent lease expired with partial edits",
		"--intended-repair", "continue the retained partial edit",
	)
	if recovered.Result.Status != "repair_pending" || recovered.Result.AssignmentToken != "" || len(recovered.Result.Claims) != 1 {
		t.Fatalf("audited recovery did not retain claims without agent authority: %+v", recovered.Result)
	}
	recoveryEvent := recovered.Result.AuditHistory[len(recovered.Result.AuditHistory)-1]
	if recoveryEvent.Event != "task_recovered" || recoveryEvent.RecoveryMethod != "user_confirmation" || recoveryEvent.UserConfirmation != "I confirmed agent-expiring is no longer running" || recoveryEvent.Diagnosis == "" || recoveryEvent.IntendedRepair == "" || len(recoveryEvent.RepairSnapshots) != 1 {
		t.Fatalf("user recovery confirmation was not audited: %+v", recoveryEvent)
	}

	replacement := successfulTaskCommand(t, repo, "assign", owner.Result.ID, "--agent", "agent-replacement")
	if replacement.Result.Status != "editing" || replacement.Result.AssignmentToken == "" || replacement.Result.AssignmentToken == assignment.Result.AssignmentToken || len(replacement.Result.Claims) != 1 || replacement.Result.Claims[0].Baseline.ContentHash != claimed.Result.Claims[0].Baseline.ContentHash {
		t.Fatalf("replacement did not inherit retained ownership with a new token: old=%+v new=%+v", assignment.Result, replacement.Result)
	}
	if content := readFile(t, filepath.Join(repo, "quarantined.txt")); content != "partial edit\n" {
		t.Fatalf("replacement recovery lost partial edits: %q", content)
	}
	linkedRecovery := replacement.Result.AuditHistory[len(replacement.Result.AuditHistory)-2]
	if linkedRecovery.Event != "task_recovered" || linkedRecovery.ReplacementToken != replacement.Result.AssignmentToken {
		t.Fatalf("recovery audit was not linked to the replacement token: event=%+v replacement=%+v", linkedRecovery, replacement.Result)
	}
	stale := runBandmaster(t, repo, "task", "heartbeat", owner.Result.ID, "--token", assignment.Result.AssignmentToken, "--json")
	assertTaskError(t, stale, 3, "invalid_assignment_token", false)

	replacementExpiry, err := time.Parse(time.RFC3339Nano, replacement.Result.Lease.ExpiresAt)
	if err != nil {
		t.Fatalf("parse replacement lease expiry: %v", err)
	}
	if wait := time.Until(replacementExpiry) + 150*time.Millisecond; wait > 0 {
		time.Sleep(wait)
	}
	expiredReplacement := runBandmaster(t, repo, "task", "heartbeat", owner.Result.ID, "--token", replacement.Result.AssignmentToken, "--json")
	assertTaskError(t, expiredReplacement, 4, "lease_expired", false)
	handleRecovered := successfulTaskCommand(t, repo, "recover", owner.Result.ID,
		"--terminated-agent", "agent-replacement",
		"--termination-proof", "codex-handle-agent-replacement-stopped",
		"--diagnosis", "the replacement lease expired before completion",
		"--intended-repair", "finish the same retained owned path",
	)
	handleEvent := handleRecovered.Result.AuditHistory[len(handleRecovered.Result.AuditHistory)-1]
	if handleRecovered.Result.Status != "repair_pending" || handleEvent.RecoveryMethod != "agent_handle" || handleEvent.TerminationProof != "codex-handle-agent-replacement-stopped" {
		t.Fatalf("agent-handle recovery was not audited: task=%+v event=%+v", handleRecovered.Result, handleEvent)
	}
	secondReplacement := successfulTaskCommand(t, repo, "assign", owner.Result.ID, "--agent", "agent-second-replacement")
	if secondReplacement.Result.AssignmentToken == "" || secondReplacement.Result.AssignmentToken == replacement.Result.AssignmentToken || len(secondReplacement.Result.Claims) != 1 {
		t.Fatalf("agent-handle recovery did not create a safe replacement: %+v", secondReplacement.Result)
	}
}

func approvedCleanRepositoryWithLeaseDuration(t *testing.T, duration string) string {
	t.Helper()
	repo := newGitRepository(t)
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/project\n\ngo 1.24\n")
	initialized := runBandmaster(t, repo, "init", "--json")
	if initialized.exitCode != 0 {
		t.Fatalf("init exit code = %d, stdout = %s, stderr = %s", initialized.exitCode, initialized.stdout, initialized.stderr)
	}
	configPath := filepath.Join(repo, ".bandmaster.yaml")
	config := readFile(t, configPath)
	writeFile(t, configPath, strings.Replace(config, "agent_lease_duration: 5m", "agent_lease_duration: "+duration, 1))
	runGit(t, repo, "add", ".")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Initialize project")
	status := runBandmaster(t, repo, "config", "status", "--json")
	approved := runBandmaster(t, repo, "config", "approve", responseDigest(t, status.stdout), "--json")
	if approved.exitCode != 0 {
		t.Fatalf("approve exit code = %d, stdout = %s, stderr = %s", approved.exitCode, approved.stdout, approved.stderr)
	}
	return repo
}

type runningBandmasterCommand struct {
	command *exec.Cmd
	stdout  bytes.Buffer
	stderr  bytes.Buffer
}

func newBandmasterCommand(repo string, args ...string) *runningBandmasterCommand {
	running := &runningBandmasterCommand{command: exec.Command(bandmasterBinary, args...)}
	running.command.Dir = repo
	running.command.Stdout = &running.stdout
	running.command.Stderr = &running.stderr
	return running
}

func waitBandmasterCommand(t *testing.T, running *runningBandmasterCommand) commandResult {
	t.Helper()
	err := running.command.Wait()
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("wait for bandmaster: %v", err)
		}
		exitCode = exitError.ExitCode()
	}
	return commandResult{exitCode: exitCode, stdout: running.stdout.String(), stderr: running.stderr.String()}
}

func TestDiffReviewsEveryClaimFromBaselineAndRejectsUnownedChanges(t *testing.T) {
	repo := approvedCleanRepository(t)
	existingPath := filepath.Join(repo, "cmd", "tool.sh")
	writeFile(t, existingPath, "#!/bin/sh\necho before\n")
	unicodePath := filepath.Join(repo, "docs", "café.txt")
	writeFile(t, unicodePath, "before unicode\n")
	if err := os.Chmod(existingPath, 0o755); err != nil {
		t.Fatalf("make baseline executable: %v", err)
	}
	runGit(t, repo, "add", "cmd/tool.sh", "docs/café.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add diff fixture")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Review diff", "--intent", "Inspect owned changes", "--expected-outcome", "Complete path review")
	assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-diff")
	successfulTaskCommand(t, repo, "claim", task.Result.ID,
		"--token", assigned.Result.AssignmentToken,
		"--path", "cmd/tool.sh",
		"--path", "docs/new.txt",
		"--path", "docs/café.txt",
	)

	writeFile(t, existingPath, "#!/bin/sh\necho after\na/after/cmd/tool.sh\n")
	if err := os.Chmod(existingPath, 0o644); err != nil {
		t.Fatalf("remove executable bit: %v", err)
	}
	writeFile(t, filepath.Join(repo, "docs", "new.txt"), "new documentation\n")
	writeFile(t, unicodePath, "after unicode\n")
	writeFile(t, filepath.Join(repo, "outside.txt"), "not owned\n")

	unowned := runBandmaster(t, repo, "task", "diff", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--json")
	assertTaskError(t, unowned, 4, "unclaimed_change", false)
	if err := os.Remove(filepath.Join(repo, "outside.txt")); err != nil {
		t.Fatalf("remove unowned fixture: %v", err)
	}
	successfulIntegrityRecovery(t, repo, "removed the unclaimed diff fixture")
	successfulSessionCommand(t, repo, "resume")

	result := runBandmaster(t, repo, "task", "diff", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--json")
	if result.exitCode != 0 {
		t.Fatalf("task diff exit code = %d, stdout = %s, stderr = %s", result.exitCode, result.stdout, result.stderr)
	}
	var response diffResponse
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("decode task diff: %v\n%s", err, result.stdout)
	}
	if !response.Success || response.Command != "task diff" || response.Result.TaskID != task.Result.ID || len(response.Result.Paths) != 3 {
		t.Fatalf("unexpected task diff: %+v", response)
	}
	existing := response.Result.Paths[0]
	if existing.Path != "cmd/tool.sh" || !existing.Changed || !existing.Baseline.Executable || existing.Current.Executable || existing.Baseline.ContentHash == existing.Current.ContentHash || !containsAll(existing.Patch, "echo before", "echo after", "+a/after/cmd/tool.sh", "old mode 100755", "new mode 100644") {
		t.Fatalf("existing path diff is incomplete: %+v", existing)
	}
	created := response.Result.Paths[1]
	if created.Path != "docs/new.txt" || !created.Changed || created.Baseline.Presence != "absent" || created.Current.Presence != "present" || created.Current.ContentHash == "" || !containsAll(created.Patch, "new file mode", "new documentation") {
		t.Fatalf("created path diff is incomplete: %+v", created)
	}
	unicode := response.Result.Paths[2]
	if unicode.Path != "docs/café.txt" || !unicode.Changed || !containsAll(unicode.Patch, "before unicode", "after unicode") || strings.Contains(unicode.Patch, "before/docs") || strings.Contains(unicode.Patch, "after/docs") {
		t.Fatalf("quoted UTF-8 path retained temporary diff prefixes: %+v", unicode)
	}
}

func TestSymlinkClaimsSnapshotTargetsAndFreezeSubmittedChanges(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "convert-to-link"), "regular before\n")
	if err := os.Symlink("targets/before.txt", filepath.Join(repo, "current-link")); err != nil {
		t.Fatalf("create tracked symlink: %v", err)
	}
	runGit(t, repo, "add", "convert-to-link", "current-link")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add symlink fixture")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Update symlinks", "--intent", "Track exact link targets", "--expected-outcome", "Symlink snapshots are frozen")
	assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-symlink")

	claimed := successfulTaskCommand(t, repo, "claim", task.Result.ID,
		"--token", assigned.Result.AssignmentToken,
		"--path", "current-link",
		"--path", "new-link",
		"--path", "convert-to-link",
	)
	wantBaselineHash := sha256.Sum256([]byte("targets/before.txt"))
	if got := claimed.Result.Claims[0].Baseline; got.Presence != "present" || got.Type != "symlink" || got.ContentHash != "sha256:"+hex.EncodeToString(wantBaselineHash[:]) || got.Executable {
		t.Fatalf("unexpected symlink baseline: %+v", got)
	}

	if err := os.Remove(filepath.Join(repo, "current-link")); err != nil {
		t.Fatalf("remove baseline symlink: %v", err)
	}
	if err := os.Symlink("targets/after.txt", filepath.Join(repo, "current-link")); err != nil {
		t.Fatalf("replace tracked symlink: %v", err)
	}
	if err := os.Symlink("targets/new.txt", filepath.Join(repo, "new-link")); err != nil {
		t.Fatalf("create new symlink: %v", err)
	}
	if err := os.Remove(filepath.Join(repo, "convert-to-link")); err != nil {
		t.Fatalf("remove regular file before type change: %v", err)
	}
	if err := os.Symlink("targets/converted.txt", filepath.Join(repo, "convert-to-link")); err != nil {
		t.Fatalf("replace regular file with symlink: %v", err)
	}

	result := runBandmaster(t, repo, "task", "diff", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--json")
	if result.exitCode != 0 {
		t.Fatalf("task diff exit code = %d, stdout = %s, stderr = %s", result.exitCode, result.stdout, result.stderr)
	}
	var response diffResponse
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("decode task diff: %v\n%s", err, result.stdout)
	}
	if len(response.Result.Paths) != 3 {
		t.Fatalf("symlink diff path count = %d, want 3: %+v", len(response.Result.Paths), response.Result.Paths)
	}
	updated := response.Result.Paths[0]
	if updated.Current.Type != "symlink" || updated.Current.Executable || !containsAll(updated.Patch, "120000", "targets/before.txt", "targets/after.txt") {
		t.Fatalf("updated symlink diff is incomplete: %+v", updated)
	}
	created := response.Result.Paths[1]
	if created.Baseline.Presence != "absent" || created.Current.Type != "symlink" || !containsAll(created.Patch, "new file mode 120000", "targets/new.txt") {
		t.Fatalf("created symlink diff is incomplete: %+v", created)
	}
	converted := response.Result.Paths[2]
	if converted.Baseline.Type != "regular_file" || converted.Current.Type != "symlink" || !containsAll(converted.Patch, "deleted file mode 100644", "new file mode 120000", "regular before", "targets/converted.txt") {
		t.Fatalf("regular-to-symlink diff is incomplete: %+v", converted)
	}

	submitted := successfulTaskCommand(t, repo, "submit", task.Result.ID,
		"--token", assigned.Result.AssignmentToken,
		"--behavior-changed", "Updated link destinations",
		"--key-decisions", "Preserved links as links",
		"--validation-expectations", "Symlink targets are exact",
		"--known-risks", "None",
	)
	for _, claim := range submitted.Result.Claims {
		if claim.SubmittedSnapshot == nil || claim.SubmittedSnapshot.Type != "symlink" || claim.SubmittedSnapshot.ContentHash == "" || claim.SubmittedSnapshot.Executable {
			t.Fatalf("submitted symlink snapshot was not frozen: %+v", claim)
		}
	}
}

func TestClaimsCoverCreationDeletionRenameAndExecutableChanges(t *testing.T) {
	t.Run("all changed paths are claimed", func(t *testing.T) {
		repo := approvedCleanRepository(t)
		writeFile(t, filepath.Join(repo, "delete.txt"), "delete me\n")
		writeFile(t, filepath.Join(repo, "rename-source.txt"), "move me\n")
		writeFile(t, filepath.Join(repo, "tool.sh"), "#!/bin/sh\n")
		runGit(t, repo, "add", "delete.txt", "rename-source.txt", "tool.sh")
		runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add path transition fixtures")
		successfulSessionCommand(t, repo, "start")
		task := successfulTaskCommand(t, repo, "create", "--title", "Change paths", "--intent", "Exercise Git path transitions", "--expected-outcome", "Every path is attributable")
		assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-path-transitions")
		successfulTaskCommand(t, repo, "claim", task.Result.ID,
			"--token", assigned.Result.AssignmentToken,
			"--path", "created.txt",
			"--path", "delete.txt",
			"--path", "rename-source.txt",
			"--path", "rename-destination.txt",
			"--path", "tool.sh",
		)

		writeFile(t, filepath.Join(repo, "created.txt"), "created\n")
		if err := os.Remove(filepath.Join(repo, "delete.txt")); err != nil {
			t.Fatalf("delete claimed file: %v", err)
		}
		if err := os.Rename(filepath.Join(repo, "rename-source.txt"), filepath.Join(repo, "rename-destination.txt")); err != nil {
			t.Fatalf("rename claimed file: %v", err)
		}
		if err := os.Chmod(filepath.Join(repo, "tool.sh"), 0o755); err != nil {
			t.Fatalf("make claimed file executable: %v", err)
		}

		result := runBandmaster(t, repo, "task", "diff", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--json")
		if result.exitCode != 0 {
			t.Fatalf("task diff exit code = %d, stdout = %s, stderr = %s", result.exitCode, result.stdout, result.stderr)
		}
		var response diffResponse
		if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
			t.Fatalf("decode task diff: %v\n%s", err, result.stdout)
		}
		if len(response.Result.Paths) != 5 {
			t.Fatalf("path transition count = %d, want 5: %+v", len(response.Result.Paths), response.Result.Paths)
		}
		if got := response.Result.Paths[0]; got.Baseline.Presence != "absent" || got.Current.Presence != "present" || !containsAll(got.Patch, "new file mode", "created") {
			t.Fatalf("creation diff is incomplete: %+v", got)
		}
		if got := response.Result.Paths[1]; got.Baseline.Presence != "present" || got.Current.Presence != "absent" || !containsAll(got.Patch, "deleted file mode", "delete me") {
			t.Fatalf("deletion diff is incomplete: %+v", got)
		}
		if got := response.Result.Paths[2]; got.Current.Presence != "absent" || !strings.Contains(got.Patch, "deleted file mode") {
			t.Fatalf("rename source diff is incomplete: %+v", got)
		}
		if got := response.Result.Paths[3]; got.Baseline.Presence != "absent" || got.Current.Presence != "present" || !strings.Contains(got.Patch, "new file mode") {
			t.Fatalf("rename destination diff is incomplete: %+v", got)
		}
		if got := response.Result.Paths[4]; !got.Current.Executable || !containsAll(got.Patch, "old mode 100644", "new mode 100755") {
			t.Fatalf("executable-bit diff is incomplete: %+v", got)
		}
	})

	t.Run("rename destination is unclaimed", func(t *testing.T) {
		repo := approvedCleanRepository(t)
		writeFile(t, filepath.Join(repo, "source.txt"), "move me\n")
		runGit(t, repo, "add", "source.txt")
		runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add rename fixture")
		successfulSessionCommand(t, repo, "start")
		task := successfulTaskCommand(t, repo, "create", "--title", "Incomplete rename", "--intent", "Prove both claims are required", "--expected-outcome", "Unowned destination is rejected")
		assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-incomplete-rename")
		successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", "source.txt")
		if err := os.Rename(filepath.Join(repo, "source.txt"), filepath.Join(repo, "destination.txt")); err != nil {
			t.Fatalf("rename fixture: %v", err)
		}

		result := runBandmaster(t, repo, "task", "diff", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--json")
		assertTaskError(t, result, 4, "unclaimed_change", false)
	})

	t.Run("core fileMode disables tracked mode drift", func(t *testing.T) {
		repo := approvedCleanRepository(t)
		trackedPath := filepath.Join(repo, "tracked-tool.sh")
		writeFile(t, trackedPath, "#!/bin/sh\n")
		if err := os.Chmod(trackedPath, 0o755); err != nil {
			t.Fatalf("make tracked fixture executable: %v", err)
		}
		runGit(t, repo, "add", "tracked-tool.sh")
		runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add fileMode fixture")
		runGit(t, repo, "config", "core.fileMode", "false")
		successfulSessionCommand(t, repo, "start")
		task := successfulTaskCommand(t, repo, "create", "--title", "Respect Git fileMode", "--intent", "Snapshot Git-visible modes", "--expected-outcome", "Ignored filesystem mode drift is unchanged")
		assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-filemode")
		claimed := successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", "tracked-tool.sh")
		if !claimed.Result.Claims[0].Baseline.Executable {
			t.Fatalf("tracked executable index mode was not captured: %+v", claimed.Result.Claims[0])
		}
		if err := os.Chmod(trackedPath, 0o644); err != nil {
			t.Fatalf("remove filesystem executable mode: %v", err)
		}

		result := runBandmaster(t, repo, "task", "diff", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--json")
		if result.exitCode != 0 {
			t.Fatalf("task diff exit code = %d, stdout = %s, stderr = %s", result.exitCode, result.stdout, result.stderr)
		}
		var response diffResponse
		if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
			t.Fatalf("decode task diff: %v\n%s", err, result.stdout)
		}
		if got := response.Result.Paths[0]; got.Changed || !got.Current.Executable || got.Patch != "" {
			t.Fatalf("core.fileMode=false exposed non-Git-visible mode drift: %+v", got)
		}
	})
}

func TestClaimPathsHonorGitSpellingAndFilesystemAliases(t *testing.T) {
	tests := []struct {
		name        string
		tracked     string
		alternate   string
		absentLeft  string
		absentRight string
	}{
		{name: "case folding", tracked: "CaseProbe.txt", alternate: "caseprobe.txt", absentLeft: "Future.txt", absentRight: "future.txt"},
		{name: "Unicode normalization", tracked: "caf\u00e9.txt", alternate: "cafe\u0301.txt", absentLeft: "r\u00e9sum\u00e9.txt", absentRight: "re\u0301sume\u0301.txt"},
	}
	for _, test := range tests {
		t.Run(test.name+" tracked spelling", func(t *testing.T) {
			repo := approvedCleanRepository(t)
			writeFile(t, filepath.Join(repo, test.tracked), "tracked\n")
			runGit(t, repo, "add", test.tracked)
			runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add spelling fixture")
			aliases := sameFilesystemObject(filepath.Join(repo, test.tracked), filepath.Join(repo, test.alternate))
			successfulSessionCommand(t, repo, "start")
			task := successfulTaskCommand(t, repo, "create", "--title", "Claim alternate spelling", "--intent", "Use canonical path identity", "--expected-outcome", "Aliases are rejected")
			assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-spelling")

			result := runBandmaster(t, repo, "task", "claim", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", test.alternate, "--json")
			if aliases {
				assertTaskError(t, result, 3, "noncanonical_claim_path", false)
				return
			}
			if result.exitCode != 0 {
				t.Fatalf("distinct spelling should be a safe absent destination: exit=%d stdout=%s stderr=%s", result.exitCode, result.stdout, result.stderr)
			}
			claimed := decodeTaskResponse(t, result.stdout)
			if len(claimed.Result.Claims) != 1 || claimed.Result.Claims[0].Baseline.Presence != "absent" {
				t.Fatalf("distinct alternate spelling was not captured as absent: %+v", claimed.Result.Claims)
			}
		})

		t.Run(test.name+" absent destinations", func(t *testing.T) {
			repo := approvedCleanRepository(t)
			writeFile(t, filepath.Join(repo, test.tracked), "probe\n")
			runGit(t, repo, "add", test.tracked)
			runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add filesystem probe")
			aliases := sameFilesystemObject(filepath.Join(repo, test.tracked), filepath.Join(repo, test.alternate))
			successfulSessionCommand(t, repo, "start")
			task := successfulTaskCommand(t, repo, "create", "--title", "Claim absent spellings", "--intent", "Resolve destination identity", "--expected-outcome", "Only distinct destinations are claimable")
			assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-absent-alias")

			result := runBandmaster(t, repo, "task", "claim", task.Result.ID,
				"--token", assigned.Result.AssignmentToken,
				"--path", test.absentLeft,
				"--path", test.absentRight,
				"--json",
			)
			if aliases {
				assertTaskError(t, result, 3, "alias_claim_path", false)
				return
			}
			if result.exitCode != 0 {
				t.Fatalf("distinct absent destinations should be claimable: exit=%d stdout=%s stderr=%s", result.exitCode, result.stdout, result.stderr)
			}
			claimed := decodeTaskResponse(t, result.stdout)
			if len(claimed.Result.Claims) != 2 || claimed.Result.Claims[0].Baseline.Presence != "absent" || claimed.Result.Claims[1].Baseline.Presence != "absent" {
				t.Fatalf("distinct absent destinations were not claimed: %+v", claimed.Result.Claims)
			}
		})
	}

	t.Run("persisted absent claims", func(t *testing.T) {
		repo := approvedCleanRepository(t)
		writeFile(t, filepath.Join(repo, "CaseProbe.txt"), "case probe\n")
		writeFile(t, filepath.Join(repo, "caf\u00e9.txt"), "normalization probe\n")
		runGit(t, repo, "add", "CaseProbe.txt", "caf\u00e9.txt")
		runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add alias probes")
		caseAliases := sameFilesystemObject(filepath.Join(repo, "CaseProbe.txt"), filepath.Join(repo, "caseprobe.txt"))
		unicodeAliases := sameFilesystemObject(filepath.Join(repo, "caf\u00e9.txt"), filepath.Join(repo, "cafe\u0301.txt"))
		successfulSessionCommand(t, repo, "start")

		pairs := []struct {
			name    string
			left    string
			right   string
			aliases bool
		}{
			{name: "case", left: "Owned.txt", right: "owned.txt", aliases: caseAliases},
			{name: "normalization", left: "r\u00e9sum\u00e9.md", right: "re\u0301sume\u0301.md", aliases: unicodeAliases},
		}
		for _, pair := range pairs {
			owner := successfulTaskCommand(t, repo, "create", "--title", "Own "+pair.name+" path", "--intent", "Reserve an absent destination", "--expected-outcome", "Alias remains unavailable")
			ownerAssignment := successfulTaskCommand(t, repo, "assign", owner.Result.ID, "--agent", "agent-owner-"+pair.name)
			successfulTaskCommand(t, repo, "claim", owner.Result.ID, "--token", ownerAssignment.Result.AssignmentToken, "--path", pair.left)
			contender := successfulTaskCommand(t, repo, "create", "--title", "Contend for "+pair.name+" path", "--intent", "Test persisted alias ownership", "--expected-outcome", "Alias contention is atomic")
			contenderAssignment := successfulTaskCommand(t, repo, "assign", contender.Result.ID, "--agent", "agent-contender-"+pair.name)
			result := runBandmaster(t, repo, "task", "claim", contender.Result.ID, "--token", contenderAssignment.Result.AssignmentToken, "--path", pair.right, "--json")
			if pair.aliases {
				assertTaskError(t, result, 2, "claim_unavailable", true)
				continue
			}
			if result.exitCode != 0 {
				t.Fatalf("distinct persisted path %q should be claimable: exit=%d stdout=%s stderr=%s", pair.right, result.exitCode, result.stdout, result.stderr)
			}
		}
	})
}

func TestClaimPathsRejectTraversalDirectoriesRepositoriesAndUnsupportedTypes(t *testing.T) {
	t.Run("lexical traversal and Git metadata", func(t *testing.T) {
		repo := approvedCleanRepository(t)
		successfulSessionCommand(t, repo, "start")
		task := successfulTaskCommand(t, repo, "create", "--title", "Reject invalid paths", "--intent", "Keep claims in the worktree", "--expected-outcome", "Invalid paths are rejected")
		assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-invalid-paths")
		for _, claimPath := range []string{"../outside.txt", "/absolute.txt", "nested//file.txt", "nested/./file.txt", `nested\file.txt`, ".git/config", "new/.git/config"} {
			result := runBandmaster(t, repo, "task", "preflight", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", claimPath, "--json")
			assertTaskError(t, result, 3, "invalid_claim_path", false)
		}
	})

	t.Run("filesystem alias of Git metadata", func(t *testing.T) {
		repo := approvedCleanRepository(t)
		aliasesGitMetadata := sameFilesystemObject(filepath.Join(repo, ".git"), filepath.Join(repo, ".GIT"))
		successfulSessionCommand(t, repo, "start")
		task := successfulTaskCommand(t, repo, "create", "--title", "Reject Git alias", "--intent", "Keep metadata outside claims", "--expected-outcome", "Only real metadata aliases are rejected")
		assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-git-alias")
		result := runBandmaster(t, repo, "task", "claim", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", ".GIT/config", "--json")
		if aliasesGitMetadata {
			assertTaskError(t, result, 3, "invalid_claim_path", false)
			return
		}
		if result.exitCode != 0 {
			t.Fatalf("distinct .GIT destination should be claimable: exit=%d stdout=%s stderr=%s", result.exitCode, result.stdout, result.stderr)
		}
	})

	t.Run("directory and parent symlink", func(t *testing.T) {
		repo := approvedCleanRepository(t)
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(repo, "escape")); err != nil {
			t.Fatalf("create parent symlink fixture: %v", err)
		}
		runGit(t, repo, "add", "escape")
		runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add path safety fixture")
		successfulSessionCommand(t, repo, "start")
		task := successfulTaskCommand(t, repo, "create", "--title", "Reject unsafe objects", "--intent", "Avoid traversal", "--expected-outcome", "Unsafe objects are rejected")
		assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-unsafe-objects")

		for _, claimPath := range []string{".agents", "escape/outside.txt"} {
			result := runBandmaster(t, repo, "task", "preflight", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", claimPath, "--json")
			assertTaskError(t, result, 3, "unsupported_claim_path", false)
		}
		if _, err := os.Stat(filepath.Join(outside, "outside.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("claim traversal touched outside path: %v", err)
		}
	})

	t.Run("nested repository", func(t *testing.T) {
		repo := approvedCleanRepository(t)
		successfulSessionCommand(t, repo, "start")
		nested := filepath.Join(repo, "vendor", "nested")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("create nested repository parent: %v", err)
		}
		runGit(t, nested, "init", "-b", "main")
		result := runBandmaster(t, repo, "session", "inspect", "--json")
		assertTaskError(t, result, 3, "unsupported_nested_repository", false)
	})

	t.Run("unsupported file type after claim", func(t *testing.T) {
		repo := approvedCleanRepository(t)
		successfulSessionCommand(t, repo, "start")
		task := successfulTaskCommand(t, repo, "create", "--title", "Reject named pipe", "--intent", "Keep snapshots complete", "--expected-outcome", "Unsupported types are rejected")
		assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-named-pipe")
		successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", "events.pipe")
		if err := syscall.Mkfifo(filepath.Join(repo, "events.pipe"), 0o600); err != nil {
			t.Fatalf("create named pipe: %v", err)
		}

		result := runBandmaster(t, repo, "task", "diff", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--json")
		assertTaskError(t, result, 3, "unsupported_claim_path", false)
	})
}

func sameFilesystemObject(left, right string) bool {
	leftInfo, leftErr := os.Lstat(left)
	rightInfo, rightErr := os.Lstat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func TestSubmissionFreezesStructuredChangedAndPendingNoOpSnapshots(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "changed.txt"), "before\n")
	writeFile(t, filepath.Join(repo, "unchanged.txt"), "stable\n")
	runGit(t, repo, "add", "changed.txt", "unchanged.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add submission fixtures")
	successfulSessionCommand(t, repo, "start")
	startingHead := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))

	changedTask := successfulTaskCommand(t, repo, "create", "--title", "Changed submission", "--intent", "Freeze edits", "--expected-outcome", "Changed snapshot persists")
	changedAssignment := successfulTaskCommand(t, repo, "assign", changedTask.Result.ID, "--agent", "agent-submit-changed")
	successfulTaskCommand(t, repo, "claim", changedTask.Result.ID, "--token", changedAssignment.Result.AssignmentToken, "--path", "changed.txt")
	writeFile(t, filepath.Join(repo, "changed.txt"), "after\n")
	writeFile(t, filepath.Join(repo, "unclaimed.txt"), "outside ownership\n")
	unclaimed := runBandmaster(t, repo, "task", "submit", changedTask.Result.ID,
		"--token", changedAssignment.Result.AssignmentToken,
		"--behavior-changed", "Changed output",
		"--key-decisions", "Kept the public format",
		"--validation-expectations", "Focused and repository tests pass",
		"--known-risks", "None",
		"--json",
	)
	assertTaskError(t, unclaimed, 4, "unclaimed_change", false)
	if err := os.Remove(filepath.Join(repo, "unclaimed.txt")); err != nil {
		t.Fatalf("remove unclaimed submission fixture: %v", err)
	}
	successfulIntegrityRecovery(t, repo, "removed the unclaimed submission fixture")
	successfulSessionCommand(t, repo, "resume")

	wrongToken := runBandmaster(t, repo, "task", "submit", changedTask.Result.ID,
		"--token", "assignment_wrong",
		"--behavior-changed", "Changed output",
		"--key-decisions", "Kept the public format",
		"--validation-expectations", "Focused and repository tests pass",
		"--known-risks", "None",
		"--json",
	)
	assertTaskError(t, wrongToken, 3, "invalid_assignment_token", false)
	beforeSubmit := successfulTaskCommand(t, repo, "inspect", changedTask.Result.ID)
	if beforeSubmit.Result.Status != "editing" || beforeSubmit.Result.Submission != nil || beforeSubmit.Result.Claims[0].SubmittedSnapshot != nil {
		t.Fatalf("rejected submission froze state: %+v", beforeSubmit.Result)
	}
	withoutReview := runBandmaster(t, repo, "task", "submit", changedTask.Result.ID,
		"--token", changedAssignment.Result.AssignmentToken,
		"--behavior-changed", "Changed output",
		"--key-decisions", "Kept the public format",
		"--validation-expectations", "Focused and repository tests pass",
		"--known-risks", "None",
		"--json",
	)
	assertTaskError(t, withoutReview, 3, "diff_review_required", false)
	if reviewed := runBandmaster(t, repo, "task", "diff", changedTask.Result.ID, "--token", changedAssignment.Result.AssignmentToken, "--json"); reviewed.exitCode != 0 {
		t.Fatalf("review changed diff: exit=%d stdout=%s stderr=%s", reviewed.exitCode, reviewed.stdout, reviewed.stderr)
	}
	writeFile(t, filepath.Join(repo, "changed.txt"), "after review changed\n")
	staleReview := runBandmaster(t, repo, "task", "submit", changedTask.Result.ID,
		"--token", changedAssignment.Result.AssignmentToken,
		"--behavior-changed", "Changed output",
		"--key-decisions", "Kept the public format",
		"--validation-expectations", "Focused and repository tests pass",
		"--known-risks", "None",
		"--json",
	)
	assertTaskError(t, staleReview, 3, "diff_review_stale", false)
	if reviewed := runBandmaster(t, repo, "task", "diff", changedTask.Result.ID, "--token", changedAssignment.Result.AssignmentToken, "--json"); reviewed.exitCode != 0 {
		t.Fatalf("refresh changed diff review: exit=%d stdout=%s stderr=%s", reviewed.exitCode, reviewed.stdout, reviewed.stderr)
	}

	changed := successfulTaskCommand(t, repo, "submit", changedTask.Result.ID,
		"--token", changedAssignment.Result.AssignmentToken,
		"--behavior-changed", "Changed output",
		"--key-decisions", "Kept the public format",
		"--validation-expectations", "Focused and repository tests pass",
		"--known-risks", "None",
	)
	if changed.Result.Status != "submitted" || changed.Result.Submission == nil || changed.Result.Submission.NoChanges || changed.Result.Submission.Outcome != "pending_changes" || changed.Result.Submission.SubmittedAt == "" {
		t.Fatalf("unexpected changed submission: %+v", changed.Result)
	}
	if changed.Result.Submission.BehaviorChanged != "Changed output" || changed.Result.Submission.KeyDecisions != "Kept the public format" || changed.Result.Submission.ValidationExpectations != "Focused and repository tests pass" || changed.Result.Submission.KnownRisks != "None" {
		t.Fatalf("structured handoff was not preserved: %+v", changed.Result.Submission)
	}
	if len(changed.Result.Claims) != 1 || changed.Result.Claims[0].SubmittedSnapshot == nil || changed.Result.Claims[0].SubmittedSnapshot.ContentHash == changed.Result.Claims[0].Baseline.ContentHash {
		t.Fatalf("changed current snapshot was not frozen: %+v", changed.Result.Claims)
	}

	noOpRepo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(noOpRepo, "unchanged.txt"), "stable\n")
	runGit(t, noOpRepo, "add", "unchanged.txt")
	runGit(t, noOpRepo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add no-op fixture")
	successfulSessionCommand(t, noOpRepo, "start")
	noOpHead := strings.TrimSpace(runGit(t, noOpRepo, "rev-parse", "HEAD"))
	noOpTask := successfulTaskCommand(t, noOpRepo, "create", "--title", "No-op submission", "--intent", "Record no change", "--expected-outcome", "No empty commit")
	noOpAssignment := successfulTaskCommand(t, noOpRepo, "assign", noOpTask.Result.ID, "--agent", "agent-submit-noop")
	successfulTaskCommand(t, noOpRepo, "claim", noOpTask.Result.ID, "--token", noOpAssignment.Result.AssignmentToken, "--path", "unchanged.txt")
	if reviewed := runBandmaster(t, noOpRepo, "task", "diff", noOpTask.Result.ID, "--token", noOpAssignment.Result.AssignmentToken, "--json"); reviewed.exitCode != 0 {
		t.Fatalf("review no-op diff: exit=%d stdout=%s stderr=%s", reviewed.exitCode, reviewed.stdout, reviewed.stderr)
	}
	noOp := successfulTaskCommand(t, noOpRepo, "submit", noOpTask.Result.ID,
		"--token", noOpAssignment.Result.AssignmentToken,
		"--behavior-changed", "No behavior changed",
		"--key-decisions", "Existing content was already correct",
		"--validation-expectations", "Validation should remain green",
		"--known-risks", "None",
	)
	if noOp.Result.Status != "submitted" || noOp.Result.Submission == nil || !noOp.Result.Submission.NoChanges || noOp.Result.Submission.Outcome != "pending_no_op" {
		t.Fatalf("unchanged submission was not recorded as pending no-op: %+v", noOp.Result)
	}
	if len(noOp.Result.Claims) != 1 || noOp.Result.Claims[0].SubmittedSnapshot == nil || noOp.Result.Claims[0].SubmittedSnapshot.ContentHash != noOp.Result.Claims[0].Baseline.ContentHash {
		t.Fatalf("no-op submitted snapshot does not match baseline: %+v", noOp.Result.Claims)
	}
	if head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD")); head != startingHead {
		t.Fatalf("submission created a commit: got %s want %s", head, startingHead)
	}
	if head := strings.TrimSpace(runGit(t, noOpRepo, "rev-parse", "HEAD")); head != noOpHead {
		t.Fatalf("no-op submission created a commit: got %s want %s", head, noOpHead)
	}
	inspected := successfulTaskCommand(t, noOpRepo, "inspect", noOpTask.Result.ID)
	if inspected.Result.Submission == nil || inspected.Result.Submission.SubmittedAt != noOp.Result.Submission.SubmittedAt || len(inspected.Result.AuditHistory) != 5 || inspected.Result.AuditHistory[4].Event != "task_submitted" {
		t.Fatalf("fresh invocation did not observe durable submission and audit: %+v", inspected.Result)
	}
}

func containsAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}
