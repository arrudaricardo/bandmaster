package integration_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestMonitorDetectsUnclaimedDriftAndRequiresAuditedRecovery(t *testing.T) {
	repo := approvedCleanRepository(t)
	started := successfulSessionCommand(t, repo, "start")
	driftPath := filepath.Join(repo, "unexpected.txt")
	writeFile(t, driftPath, "outside ownership\n")

	paused := waitForSessionStatus(t, repo, "paused")
	if paused.Result.Monitor == nil || paused.Result.Monitor.Status != "stopped" {
		t.Fatalf("monitor did not stop after quarantining the session: %+v", paused.Result.Monitor)
	}
	violation := unresolvedViolation(t, paused, "unclaimed_change", "unexpected.txt")
	if violation.ObservedState == nil || violation.DetectedAt == "" {
		t.Fatalf("integrity evidence is incomplete: %+v", violation)
	}
	assertIntegrityAudit(t, paused, "unclaimed_change", "unexpected.txt")

	blocked := runBandmaster(t, repo, "task", "create", "--title", "Blocked", "--intent", "Must not mutate", "--expected-outcome", "Recovery first", "--json")
	assertTaskError(t, blocked, 4, "unclaimed_change", false)
	failedRecovery := runBandmaster(t, repo, "integrity", "recover", "--confirmation", "inspected unexpected.txt", "--json")
	assertIntegrityError(t, failedRecovery, 3, "integrity_not_restored")

	if err := os.Remove(driftPath); err != nil {
		t.Fatalf("restore repository: %v", err)
	}
	recovered := successfulIntegrityRecovery(t, repo, "removed the unowned file after inspection")
	if recovered.Result.Status != "paused" || recovered.Result.IntegrityViolations[0].RecoveredAt == "" || recovered.Result.IntegrityViolations[0].RecoveryConfirmation == "" {
		t.Fatalf("recovery was not explicitly audited: %+v", recovered.Result)
	}
	resumed := successfulSessionCommand(t, repo, "resume")
	if resumed.Result.Monitor == nil || resumed.Result.Monitor.Status != "healthy" || resumed.Result.Monitor.Generation != started.Result.Monitor.Generation+1 || resumed.Result.Monitor.ProcessIdentity == started.Result.Monitor.ProcessIdentity {
		t.Fatalf("resume did not start a fresh healthy monitor after a full scan: before=%+v after=%+v", started.Result.Monitor, resumed.Result.Monitor)
	}
}

func TestSubmittedPathDriftQuarantinesAndRestoresAffectedWork(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "submitted.txt"), "baseline\n")
	runGit(t, repo, "add", "submitted.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add submitted fixture")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Freeze work", "--intent", "Submit exact content", "--expected-outcome", "Later edits quarantine")
	assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "worker-submitted-drift")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", "submitted.txt")
	writeFile(t, filepath.Join(repo, "submitted.txt"), "submitted content\n")
	if result := runBandmaster(t, repo, "task", "diff", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--json"); result.exitCode != 0 {
		t.Fatalf("review submitted fixture: %+v", result)
	}
	successfulTaskCommand(t, repo, "submit", task.Result.ID,
		"--token", assigned.Result.AssignmentToken,
		"--behavior-changed", "Changed submitted content",
		"--key-decisions", "Keep exact snapshot",
		"--validation-expectations", "Content remains frozen",
		"--known-risks", "None",
	)

	writeFile(t, filepath.Join(repo, "submitted.txt"), "edited after submission\n")
	paused := waitForSessionStatus(t, repo, "paused")
	unresolvedViolation(t, paused, "submitted_path_drift", "submitted.txt")
	quarantined := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if quarantined.Result.Status != "quarantined" || len(quarantined.Result.Claims) != 1 || quarantined.Result.Claims[0].SubmittedSnapshot == nil {
		t.Fatalf("submitted owner was not quarantined with retained evidence: %+v", quarantined.Result)
	}

	writeFile(t, filepath.Join(repo, "submitted.txt"), "submitted content\n")
	successfulIntegrityRecovery(t, repo, "restored the frozen submitted snapshot")
	restored := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if restored.Result.Status != "submitted" {
		t.Fatalf("integrity recovery did not restore the task's prior state: %+v", restored.Result)
	}
}

func TestMonitorDeathPausesMutationAndResumeReplacesIt(t *testing.T) {
	repo := approvedCleanRepository(t)
	started := successfulSessionCommand(t, repo, "start")
	if err := syscall.Kill(started.Result.Monitor.ProcessID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill integrity monitor: %v", err)
	}

	mutation := runBandmaster(t, repo, "task", "create", "--title", "Unsafe mutation", "--intent", "Require monitoring", "--expected-outcome", "No mutation", "--json")
	assertTaskError(t, mutation, 4, "monitor_unhealthy", false)
	paused := successfulSessionCommand(t, repo, "inspect")
	unresolvedViolation(t, paused, "monitor_unhealthy", ".git/bandmaster/monitor")

	successfulIntegrityRecovery(t, repo, "confirmed the old monitor process is stopped")
	resumed := successfulSessionCommand(t, repo, "resume")
	if resumed.Result.Monitor.Generation != 2 || resumed.Result.Monitor.ProcessIdentity == started.Result.Monitor.ProcessIdentity || resumed.Result.Monitor.Status != "healthy" {
		t.Fatalf("unhealthy monitor was not safely replaced: before=%+v after=%+v", started.Result.Monitor, resumed.Result.Monitor)
	}
}

func TestHungMonitorMustBeTerminatedBeforeReplacement(t *testing.T) {
	repo := approvedCleanRepository(t)
	started := successfulSessionCommand(t, repo, "start")
	pid := started.Result.Monitor.ProcessID
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatalf("stop integrity monitor: %v", err)
	}
	time.Sleep(2200 * time.Millisecond)

	mutation := runBandmaster(t, repo, "task", "create", "--title", "Unsafe mutation", "--intent", "Detect a hung monitor", "--expected-outcome", "No mutation", "--json")
	assertTaskError(t, mutation, 4, "monitor_unhealthy", false)
	unconfirmed := runBandmaster(t, repo, "integrity", "recover", "--confirmation", "monitor is stopped but not terminated", "--json")
	assertIntegrityError(t, unconfirmed, 3, "monitor_termination_unconfirmed")

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill stopped integrity monitor: %v", err)
	}
	successfulIntegrityRecovery(t, repo, "proved the stopped monitor process was terminated")
	resumed := successfulSessionCommand(t, repo, "resume")
	if resumed.Result.Monitor.Generation != 2 || resumed.Result.Monitor.ProcessIdentity == started.Result.Monitor.ProcessIdentity {
		t.Fatalf("replacement monitor identity was not fenced: before=%+v after=%+v", started.Result.Monitor, resumed.Result.Monitor)
	}
}

func TestFailedManualPauseRequiresRecoveryBeforeMonitorReplacement(t *testing.T) {
	repo := approvedCleanRepository(t)
	started := successfulSessionCommand(t, repo, "start")
	pid := started.Result.Monitor.ProcessID
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatalf("stop integrity monitor: %v", err)
	}

	paused := runBandmaster(t, repo, "session", "pause", "--json")
	if paused.exitCode != 4 {
		t.Fatalf("pause with hung monitor exit = %d, want 4; stdout=%s stderr=%s", paused.exitCode, paused.stdout, paused.stderr)
	}
	pausedResponse := decodeSessionResponse(t, paused.stdout)
	if pausedResponse.Error.Code != "monitor_unhealthy" {
		t.Fatalf("pause error = %+v, want monitor_unhealthy", pausedResponse)
	}
	resume := runBandmaster(t, repo, "session", "resume", "--json")
	assertIntegrityError(t, resume, 3, "integrity_recovery_required")

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill stopped integrity monitor: %v", err)
	}
	successfulIntegrityRecovery(t, repo, "terminated the monitor that could not stop during pause")
	resumed := successfulSessionCommand(t, repo, "resume")
	if resumed.Result.Monitor.Generation != 2 || resumed.Result.Monitor.ProcessIdentity == started.Result.Monitor.ProcessIdentity {
		t.Fatalf("manual pause recovery did not fence replacement: before=%+v after=%+v", started.Result.Monitor, resumed.Result.Monitor)
	}
}

func TestIgnoredUntrackedPathsStayOutsideOwnershipWhileTrackedPathsRemainCovered(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "cache/\ntracked-ignored.txt\n")
	writeFile(t, filepath.Join(repo, "tracked-ignored.txt"), "tracked baseline\n")
	runGit(t, repo, "add", "-f", ".gitignore", "tracked-ignored.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add ignore fixtures")
	successfulSessionCommand(t, repo, "start")
	writeFile(t, filepath.Join(repo, "cache", "artifact.txt"), "ignored\n")

	task := successfulTaskCommand(t, repo, "create", "--title", "Ignore policy", "--intent", "Keep ignored output unowned", "--expected-outcome", "Ignored claims fail")
	assigned := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "worker-ignore")
	ignoredClaim := runBandmaster(t, repo, "task", "claim", task.Result.ID, "--token", assigned.Result.AssignmentToken, "--path", "cache/artifact.txt", "--json")
	assertTaskError(t, ignoredClaim, 3, "ignored_untracked_path", false)

	writeFile(t, filepath.Join(repo, "tracked-ignored.txt"), "tracked drift\n")
	paused := waitForSessionStatus(t, repo, "paused")
	unresolvedViolation(t, paused, "unclaimed_change", "tracked-ignored.txt")
}

func TestAuthoritativeScansDetectGitControlStateDrift(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*testing.T, string)
		wantKinds map[string]string
	}{
		{
			name: "branch",
			mutate: func(t *testing.T, repo string) {
				runGit(t, repo, "checkout", "-b", "outside-session")
			},
			wantKinds: map[string]string{"branch_drift": ".git/HEAD"},
		},
		{
			name: "head and base",
			mutate: func(t *testing.T, repo string) {
				runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "--allow-empty", "-m", "Outside session")
			},
			wantKinds: map[string]string{"head_drift": ".git/HEAD", "base_drift": "refs/heads/main"},
		},
		{
			name: "index",
			mutate: func(t *testing.T, repo string) {
				writeFile(t, filepath.Join(repo, "staged.txt"), "staged outside Bandmaster\n")
				runGit(t, repo, "add", "staged.txt")
			},
			wantKinds: map[string]string{"index_drift": ".git/index"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := approvedCleanRepository(t)
			successfulSessionCommand(t, repo, "start")
			test.mutate(t, repo)
			paused := waitForSessionStatus(t, repo, "paused")
			for kind, path := range test.wantKinds {
				unresolvedViolation(t, paused, kind, path)
				assertIntegrityAudit(t, paused, kind, path)
			}
		})
	}
}

func assertIntegrityAudit(t *testing.T, response sessionResponse, kind, path string) {
	t.Helper()
	for _, event := range response.Result.AuditHistory {
		if event.Event == "integrity_violation_observed" && event.IntegrityViolationID != 0 && event.IntegrityKind == kind && event.IntegrityPath == path && len(event.ObservedState) != 0 && event.OccurredAt != "" {
			return
		}
	}
	t.Fatalf("missing append-only integrity audit for %s at %s: %+v", kind, path, response.Result.AuditHistory)
}

type integrityViolationView struct {
	ObservedState []byte
	DetectedAt    string
}

func unresolvedViolation(t *testing.T, response sessionResponse, kind, path string) integrityViolationView {
	t.Helper()
	for _, violation := range response.Result.IntegrityViolations {
		if violation.Kind == kind && violation.Path == path && violation.RecoveredAt == "" {
			return integrityViolationView{ObservedState: violation.ObservedState, DetectedAt: violation.DetectedAt}
		}
	}
	t.Fatalf("missing unresolved %s violation for %q: %+v", kind, path, response.Result.IntegrityViolations)
	return integrityViolationView{}
}

func waitForSessionStatus(t *testing.T, repo, status string) sessionResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response := successfulSessionCommand(t, repo, "inspect")
		if response.Result.Status == status {
			return response
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("session did not reach %s", status)
	return sessionResponse{}
}

func successfulIntegrityRecovery(t *testing.T, repo, confirmation string) sessionResponse {
	t.Helper()
	result := runBandmaster(t, repo, "integrity", "recover", "--confirmation", confirmation, "--json")
	if result.exitCode != 0 {
		t.Fatalf("integrity recovery exit code = %d, stdout = %s, stderr = %s", result.exitCode, result.stdout, result.stderr)
	}
	return decodeSessionResponse(t, result.stdout)
}

func assertIntegrityError(t *testing.T, result commandResult, exitCode int, code string) {
	t.Helper()
	if result.exitCode != exitCode {
		t.Fatalf("integrity command exit code = %d, want %d; stdout = %s; stderr = %s", result.exitCode, exitCode, result.stdout, result.stderr)
	}
	response := decodeSessionResponse(t, result.stdout)
	if response.Success || response.Error.Code != code {
		t.Fatalf("integrity error = %+v, want %s", response, code)
	}
}
