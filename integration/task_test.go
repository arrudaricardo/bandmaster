package integration_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

type taskResponse struct {
	SchemaVersion string `json:"schema_version"`
	Command       string `json:"command"`
	Success       bool   `json:"success"`
	SessionID     string `json:"session_id"`
	Result        struct {
		ID              string   `json:"id"`
		CreationOrder   int64    `json:"creation_order"`
		Title           string   `json:"title"`
		Intent          string   `json:"intent"`
		ExpectedOutcome string   `json:"expected_outcome"`
		Prerequisites   []string `json:"prerequisites"`
		Status          string   `json:"status"`
		AgentIdentity   string   `json:"agent_identity"`
		AssignmentToken string   `json:"assignment_token"`
		CoreFrozen      bool     `json:"core_frozen"`
		BatchID         string   `json:"batch_id"`
		Lease           *struct {
			Status    string `json:"status"`
			RenewedAt string `json:"renewed_at"`
			ExpiresAt string `json:"expires_at"`
		} `json:"lease"`
		Claims []struct {
			Path     string `json:"path"`
			Baseline struct {
				Presence    string `json:"presence"`
				Type        string `json:"type"`
				ContentHash string `json:"content_hash"`
				Executable  bool   `json:"executable"`
			} `json:"baseline"`
			SubmittedSnapshot *struct {
				Presence    string `json:"presence"`
				Type        string `json:"type"`
				ContentHash string `json:"content_hash"`
				Executable  bool   `json:"executable"`
			} `json:"submitted_snapshot"`
		} `json:"claims"`
		OwnershipEvidence []struct {
			Path      string `json:"path"`
			ClaimedAt string `json:"claimed_at"`
			Baseline  struct {
				Presence    string `json:"presence"`
				Type        string `json:"type"`
				ContentHash string `json:"content_hash"`
				Executable  bool   `json:"executable"`
			} `json:"baseline"`
			SubmittedSnapshot *struct {
				Presence    string `json:"presence"`
				Type        string `json:"type"`
				ContentHash string `json:"content_hash"`
				Executable  bool   `json:"executable"`
			} `json:"submitted_snapshot"`
		} `json:"ownership_evidence"`
		FocusedValidation []struct {
			Name             string            `json:"name"`
			Argv             []string          `json:"argv"`
			WorkingDirectory string            `json:"working_directory"`
			Timeout          string            `json:"timeout"`
			Environment      map[string]string `json:"environment"`
		} `json:"focused_validation"`
		Submission *struct {
			Outcome                string `json:"outcome"`
			NoChanges              bool   `json:"no_changes"`
			BehaviorChanged        string `json:"behavior_changed"`
			KeyDecisions           string `json:"key_decisions"`
			ValidationExpectations string `json:"validation_expectations"`
			KnownRisks             string `json:"known_risks"`
			SubmittedAt            string `json:"submitted_at"`
		} `json:"submission"`
		AuditHistory []struct {
			Sequence         int64  `json:"sequence"`
			Event            string `json:"event"`
			FromStatus       string `json:"from_status"`
			ToStatus         string `json:"to_status"`
			AgentIdentity    string `json:"agent_identity"`
			TerminationProof string `json:"termination_proof"`
			RecoveryMethod   string `json:"recovery_method"`
			UserConfirmation string `json:"user_confirmation"`
			ReplacementToken string `json:"replacement_assignment_token"`
			Diagnosis        string `json:"diagnosis"`
			IntendedRepair   string `json:"intended_repair"`
			Invalidated      *struct {
				Outcome                string `json:"outcome"`
				BehaviorChanged        string `json:"behavior_changed"`
				KeyDecisions           string `json:"key_decisions"`
				ValidationExpectations string `json:"validation_expectations"`
				KnownRisks             string `json:"known_risks"`
				SubmittedAt            string `json:"submitted_at"`
			} `json:"invalidated_submission"`
			RepairSnapshots []struct {
				Path     string `json:"path"`
				Snapshot struct {
					Presence    string `json:"presence"`
					Type        string `json:"type"`
					ContentHash string `json:"content_hash"`
					Executable  bool   `json:"executable"`
				} `json:"snapshot"`
				InvalidatedSubmitted *struct {
					Presence    string `json:"presence"`
					Type        string `json:"type"`
					ContentHash string `json:"content_hash"`
					Executable  bool   `json:"executable"`
				} `json:"invalidated_submitted_snapshot"`
			} `json:"repair_snapshots"`
			OccurredAt string `json:"occurred_at"`
		} `json:"audit_history"`
	} `json:"result"`
	Error struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

func TestAssignmentOnlySucceedsForReadyTasksAndFreezesTheAgentAttempt(t *testing.T) {
	repo := approvedCleanRepository(t)
	session := successfulSessionCommand(t, repo, "start")
	ready := successfulTaskCommand(t, repo, "create",
		"--title", "Independent work",
		"--intent", "Run immediately",
		"--expected-outcome", "Agent can begin",
	)
	planned := successfulTaskCommand(t, repo, "create",
		"--title", "Dependent work",
		"--intent", "Wait for the prerequisite",
		"--expected-outcome", "Agent sees prior accepted work",
		"--prerequisite", ready.Result.ID,
	)

	blocked := runBandmaster(t, repo, "task", "assign", planned.Result.ID, "--agent", "agent-dependent", "--json")
	if blocked.exitCode != 2 {
		t.Fatalf("dependent assignment exit code = %d, want 2; stdout = %s; stderr = %s", blocked.exitCode, blocked.stdout, blocked.stderr)
	}
	blockedResponse := decodeTaskResponse(t, blocked.stdout)
	if blockedResponse.Success || blockedResponse.SessionID != session.SessionID || blockedResponse.Error.Code != "task_not_ready" || !blockedResponse.Error.Retryable {
		t.Fatalf("unexpected blocked assignment: %+v", blockedResponse)
	}

	assigned := successfulTaskCommand(t, repo, "assign", ready.Result.ID, "--agent", "agent-parser")
	if assigned.SessionID != session.SessionID || assigned.Result.Status != "assigned" || assigned.Result.AgentIdentity != "agent-parser" || assigned.Result.AssignmentToken == "" {
		t.Fatalf("unexpected assignment: %+v", assigned)
	}
	retried := successfulTaskCommand(t, repo, "assign", ready.Result.ID, "--agent", "agent-parser")
	if retried.Result.AssignmentToken != assigned.Result.AssignmentToken {
		t.Fatalf("idempotent assignment replaced token: first=%q second=%q", assigned.Result.AssignmentToken, retried.Result.AssignmentToken)
	}

	inspected := successfulTaskCommand(t, repo, "inspect", ready.Result.ID)
	if inspected.Result.Title != "Independent work" || inspected.Result.Intent != "Run immediately" || inspected.Result.ExpectedOutcome != "Agent can begin" {
		t.Fatalf("assignment changed frozen planning fields: %+v", inspected.Result)
	}
	if len(inspected.Result.AuditHistory) != 2 || inspected.Result.AuditHistory[1].Event != "task_assigned" || inspected.Result.AuditHistory[1].FromStatus != "ready" || inspected.Result.AuditHistory[1].ToStatus != "assigned" || inspected.Result.AuditHistory[1].AgentIdentity != "agent-parser" {
		t.Fatalf("unexpected assignment audit history: %+v", inspected.Result.AuditHistory)
	}
}

func TestAssignmentQuarantinesActiveSessionConfigurationDrift(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create",
		"--title", "Configuration-bound lease",
		"--intent", "Use only an approved lease duration",
		"--expected-outcome", "Assignment waits for approval",
	)
	configPath := filepath.Join(repo, ".bandmaster.yaml")
	config := readFile(t, configPath)
	writeFile(t, configPath, strings.Replace(config, "agent_lease_duration: 5m", "agent_lease_duration: 10m", 1))

	unapproved := runBandmaster(t, repo, "task", "assign", task.Result.ID, "--agent", "agent-config", "--json")
	assertTaskError(t, unapproved, 4, "unclaimed_change", false)
	if inspected := successfulTaskCommand(t, repo, "inspect", task.Result.ID); inspected.Result.Status != "ready" || inspected.Result.Lease != nil {
		t.Fatalf("unapproved lease configuration changed assignment state: %+v", inspected.Result)
	}
	if session := successfulSessionCommand(t, repo, "inspect"); session.Result.Status != "paused" {
		t.Fatalf("configuration drift did not pause the session: %+v", session.Result)
	}
}

func TestReplanningAssignedWorkRequiresTerminationAndRevokesTheToken(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	first := successfulTaskCommand(t, repo, "create",
		"--title", "First plan",
		"--intent", "Original behavior",
		"--expected-outcome", "Original result",
	)
	dependent := successfulTaskCommand(t, repo, "create",
		"--title", "Dependent plan",
		"--intent", "Use the first result",
		"--expected-outcome", "Dependency remains valid",
		"--prerequisite", first.Result.ID,
	)
	assigned := successfulTaskCommand(t, repo, "assign", first.Result.ID, "--agent", "agent-original")

	missingProof := runBandmaster(t, repo,
		"task", "replan", first.Result.ID,
		"--title", "Revised plan",
		"--intent", "Revised behavior",
		"--expected-outcome", "Revised result",
		"--json",
	)
	assertTaskError(t, missingProof, 3, "agent_termination_required", false)
	wrongProof := runBandmaster(t, repo,
		"task", "replan", first.Result.ID,
		"--title", "Revised plan",
		"--intent", "Revised behavior",
		"--expected-outcome", "Revised result",
		"--terminated-agent", "agent-other",
		"--termination-proof", "codex-handle-agent-other-stopped",
		"--json",
	)
	assertTaskError(t, wrongProof, 3, "agent_termination_mismatch", false)
	missingEvidence := runBandmaster(t, repo,
		"task", "replan", first.Result.ID,
		"--title", "Revised plan",
		"--intent", "Revised behavior",
		"--expected-outcome", "Revised result",
		"--terminated-agent", "agent-original",
		"--json",
	)
	assertTaskError(t, missingEvidence, 3, "agent_termination_proof_required", false)

	replanned := successfulTaskCommand(t, repo, "replan", first.Result.ID,
		"--title", "Revised plan",
		"--intent", "Revised behavior",
		"--expected-outcome", "Revised result",
		"--terminated-agent", "agent-original",
		"--termination-proof", "codex-handle-agent-original-stopped",
	)
	if replanned.Result.Status != "ready" || replanned.Result.AgentIdentity != "" || replanned.Result.AssignmentToken != "" {
		t.Fatalf("replanned assignment retained agent authority: %+v", replanned.Result)
	}
	if replanned.Result.Title != "Revised plan" || replanned.Result.Intent != "Revised behavior" || replanned.Result.ExpectedOutcome != "Revised result" {
		t.Fatalf("replanned fields were not persisted: %+v", replanned.Result)
	}
	if len(replanned.Result.AuditHistory) != 3 || replanned.Result.AuditHistory[2].Event != "task_replanned" || replanned.Result.AuditHistory[2].FromStatus != "assigned" || replanned.Result.AuditHistory[2].ToStatus != "ready" || replanned.Result.AuditHistory[2].AgentIdentity != "agent-original" || replanned.Result.AuditHistory[2].TerminationProof != "codex-handle-agent-original-stopped" {
		t.Fatalf("unexpected replan audit history: %+v", replanned.Result.AuditHistory)
	}

	reassigned := successfulTaskCommand(t, repo, "assign", first.Result.ID, "--agent", "agent-replacement")
	if reassigned.Result.AssignmentToken == "" || reassigned.Result.AssignmentToken == assigned.Result.AssignmentToken {
		t.Fatalf("replacement assignment did not receive a new token: old=%q new=%q", assigned.Result.AssignmentToken, reassigned.Result.AssignmentToken)
	}

	cycle := runBandmaster(t, repo,
		"task", "replan", first.Result.ID,
		"--title", "Cyclic plan",
		"--intent", "Invalid dependency",
		"--expected-outcome", "Cycle is rejected",
		"--prerequisite", dependent.Result.ID,
		"--terminated-agent", "agent-replacement",
		"--termination-proof", "codex-handle-agent-replacement-stopped",
		"--json",
	)
	assertTaskError(t, cycle, 3, "task_dependency_cycle", false)
	unchanged := successfulTaskCommand(t, repo, "inspect", first.Result.ID)
	if unchanged.Result.Status != "assigned" || unchanged.Result.AssignmentToken != reassigned.Result.AssignmentToken || unchanged.Result.Title != "Revised plan" {
		t.Fatalf("failed cyclic replan changed durable state: %+v", unchanged.Result)
	}
}

func TestCancelingAssignedClaimlessWorkRequiresTerminationAndResolvedDependents(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	prerequisite := successfulTaskCommand(t, repo, "create",
		"--title", "Potentially obsolete work",
		"--intent", "Provide an intermediate capability",
		"--expected-outcome", "A dependent can use it",
	)
	dependent := successfulTaskCommand(t, repo, "create",
		"--title", "Consumer",
		"--intent", "Use the intermediate capability",
		"--expected-outcome", "Combined behavior works",
		"--prerequisite", prerequisite.Result.ID,
	)
	assigned := successfulTaskCommand(t, repo, "assign", prerequisite.Result.ID, "--agent", "agent-obsolete")

	withDependent := runBandmaster(t, repo, "task", "cancel", prerequisite.Result.ID, "--terminated-agent", "agent-obsolete", "--termination-proof", "codex-handle-agent-obsolete-stopped", "--json")
	assertTaskError(t, withDependent, 3, "task_has_dependents", false)
	stillAssigned := successfulTaskCommand(t, repo, "inspect", prerequisite.Result.ID)
	if stillAssigned.Result.AssignmentToken != assigned.Result.AssignmentToken || stillAssigned.Result.Status != "assigned" {
		t.Fatalf("rejected cancellation changed assignment: %+v", stillAssigned.Result)
	}

	successfulTaskCommand(t, repo, "replan", dependent.Result.ID,
		"--title", "Independent consumer",
		"--intent", "Proceed without the intermediate capability",
		"--expected-outcome", "Independent behavior works",
	)
	missingProof := runBandmaster(t, repo, "task", "cancel", prerequisite.Result.ID, "--json")
	assertTaskError(t, missingProof, 3, "agent_termination_required", false)
	wrongProof := runBandmaster(t, repo, "task", "cancel", prerequisite.Result.ID, "--terminated-agent", "agent-other", "--termination-proof", "codex-handle-agent-other-stopped", "--json")
	assertTaskError(t, wrongProof, 3, "agent_termination_mismatch", false)
	missingEvidence := runBandmaster(t, repo, "task", "cancel", prerequisite.Result.ID, "--terminated-agent", "agent-obsolete", "--json")
	assertTaskError(t, missingEvidence, 3, "agent_termination_proof_required", false)

	canceled := successfulTaskCommand(t, repo, "cancel", prerequisite.Result.ID, "--terminated-agent", "agent-obsolete", "--termination-proof", "codex-handle-agent-obsolete-stopped")
	if canceled.Result.Status != "canceled" || canceled.Result.AgentIdentity != "" || canceled.Result.AssignmentToken != "" {
		t.Fatalf("canceled task retained agent authority: %+v", canceled.Result)
	}
	if len(canceled.Result.AuditHistory) != 3 || canceled.Result.AuditHistory[2].Event != "task_canceled" || canceled.Result.AuditHistory[2].FromStatus != "assigned" || canceled.Result.AuditHistory[2].ToStatus != "canceled" || canceled.Result.AuditHistory[2].AgentIdentity != "agent-obsolete" || canceled.Result.AuditHistory[2].TerminationProof != "codex-handle-agent-obsolete-stopped" {
		t.Fatalf("unexpected cancellation audit history: %+v", canceled.Result.AuditHistory)
	}
	retried := successfulTaskCommand(t, repo, "cancel", prerequisite.Result.ID)
	if retried.Result.Status != "canceled" || len(retried.Result.AuditHistory) != 3 {
		t.Fatalf("repeated cancellation was not idempotent: %+v", retried.Result)
	}
}

func TestSessionCannotFinishWithIncompleteTasks(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create",
		"--title", "Required work",
		"--intent", "Keep the session open",
		"--expected-outcome", "Completion waits for the task",
	)

	unfinished := runBandmaster(t, repo, "session", "finish", "--json")
	if unfinished.exitCode != 3 {
		t.Fatalf("finish exit code = %d, want 3; stdout = %s; stderr = %s", unfinished.exitCode, unfinished.stdout, unfinished.stderr)
	}
	finishResponse := decodeSessionResponse(t, unfinished.stdout)
	if finishResponse.Success || finishResponse.Error.Code != "session_tasks_incomplete" {
		t.Fatalf("unexpected unfinished-task response: %+v", finishResponse)
	}

	successfulTaskCommand(t, repo, "cancel", task.Result.ID)
	finished := successfulSessionCommand(t, repo, "finish")
	if finished.Result.Status != "completed" {
		t.Fatalf("session status = %q, want completed", finished.Result.Status)
	}
}

type taskListResponse struct {
	SchemaVersion string `json:"schema_version"`
	Command       string `json:"command"`
	Success       bool   `json:"success"`
	SessionID     string `json:"session_id"`
	Result        struct {
		Tasks []struct {
			ID              string   `json:"id"`
			CreationOrder   int64    `json:"creation_order"`
			Title           string   `json:"title"`
			Intent          string   `json:"intent"`
			ExpectedOutcome string   `json:"expected_outcome"`
			Prerequisites   []string `json:"prerequisites"`
			Status          string   `json:"status"`
		} `json:"tasks"`
	} `json:"result"`
}

func TestTasksPersistPlanningIdentityDependenciesAndReadiness(t *testing.T) {
	repo := approvedCleanRepository(t)
	session := successfulSessionCommand(t, repo, "start")

	first := successfulTaskCommand(t, repo, "create",
		"--title", "Build parser",
		"--intent", "Accept task plans",
		"--expected-outcome", "Valid plans persist",
	)
	if first.Result.ID == "" || first.Result.CreationOrder != 1 || first.Result.Status != "ready" {
		t.Fatalf("unexpected first task: %+v", first)
	}

	second := successfulTaskCommand(t, repo, "create",
		"--title", "Wire command",
		"--intent", "Expose planning",
		"--expected-outcome", "Agents can create tasks",
		"--prerequisite", first.Result.ID,
	)
	if second.Result.ID == "" || second.Result.ID == first.Result.ID || second.Result.CreationOrder != 2 || second.Result.Status != "planned" {
		t.Fatalf("unexpected dependent task: %+v", second)
	}

	listed := runBandmaster(t, repo, "task", "list", "--json")
	if listed.exitCode != 0 {
		t.Fatalf("task list exit code = %d, stdout = %s, stderr = %s", listed.exitCode, listed.stdout, listed.stderr)
	}
	var response taskListResponse
	if err := json.Unmarshal([]byte(listed.stdout), &response); err != nil {
		t.Fatalf("decode task list: %v\n%s", err, listed.stdout)
	}
	if !response.Success || response.SchemaVersion != "2" || response.Command != "task list" || response.SessionID != session.SessionID {
		t.Fatalf("unexpected task list envelope: %+v", response)
	}
	if len(response.Result.Tasks) != 2 {
		t.Fatalf("task count = %d, want 2: %+v", len(response.Result.Tasks), response.Result.Tasks)
	}
	if response.Result.Tasks[0].Title != "Build parser" || response.Result.Tasks[0].Intent != "Accept task plans" || response.Result.Tasks[0].ExpectedOutcome != "Valid plans persist" {
		t.Fatalf("first task planning fields changed: %+v", response.Result.Tasks[0])
	}
	if len(response.Result.Tasks[1].Prerequisites) != 1 || response.Result.Tasks[1].Prerequisites[0] != first.Result.ID {
		t.Fatalf("dependent prerequisites = %v, want [%s]", response.Result.Tasks[1].Prerequisites, first.Result.ID)
	}
}

func successfulTaskCommand(t *testing.T, repo, action string, args ...string) taskResponse {
	t.Helper()
	commandArgs := append([]string{"task", action}, args...)
	commandArgs = append(commandArgs, "--json")
	result := runBandmaster(t, repo, commandArgs...)
	if result.exitCode != 0 {
		t.Fatalf("task %s exit code = %d, stdout = %s, stderr = %s", action, result.exitCode, result.stdout, result.stderr)
	}
	response := decodeTaskResponse(t, result.stdout)
	if !response.Success || response.SchemaVersion != "2" || response.Command != "task "+action {
		t.Fatalf("unexpected task %s response: %+v", action, response)
	}
	return response
}

func decodeTaskResponse(t *testing.T, output string) taskResponse {
	t.Helper()
	var response taskResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("decode task response: %v\n%s", err, output)
	}
	return response
}

func assertTaskError(t *testing.T, result commandResult, wantExit int, wantCode string, wantRetryable bool) taskResponse {
	t.Helper()
	if result.exitCode != wantExit {
		t.Fatalf("task command exit code = %d, want %d; stdout = %s; stderr = %s", result.exitCode, wantExit, result.stdout, result.stderr)
	}
	response := decodeTaskResponse(t, result.stdout)
	if response.Success || response.SchemaVersion != "2" || response.Error.Code != wantCode || response.Error.Retryable != wantRetryable {
		t.Fatalf("task command error = %+v, want code %q retryable %t", response, wantCode, wantRetryable)
	}
	return response
}
