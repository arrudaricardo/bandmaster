package integration_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver"
)

func TestGoV2ContractUsesOnlyAgentVocabulary(t *testing.T) {
	repo := approvedCleanRepository(t)
	started := runBandmaster(t, repo, "session", "start", "--json")
	if started.exitCode != 0 {
		t.Fatalf("start Session: %s", started.stderr)
	}
	created := successfulTaskCommand(t, repo, "create", "--title", "Contract task", "--intent", "Expose Agent identity", "--expected-outcome", "Only v2 fields")
	assigned := runBandmaster(t, repo, "task", "assign", created.Result.ID, "--agent", "agent-contract", "--json")
	if assigned.exitCode != 0 {
		t.Fatalf("assign Agent: %s", assigned.stderr)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(assigned.stdout), &envelope); err != nil {
		t.Fatalf("decode assignment: %v", err)
	}
	if envelope["schema_version"] != "2" {
		t.Fatalf("schema version = %#v", envelope["schema_version"])
	}
	if !strings.Contains(assigned.stdout, `"agent_identity":"agent-contract"`) {
		t.Fatalf("Agent field absent: %s", assigned.stdout)
	}
	for _, retired := range []string{`"worker"`, `"workers"`, `"worker_identity"`, `"member"`, `"members"`, `"membership_order"`} {
		if strings.Contains(assigned.stdout, retired) {
			t.Fatalf("v2 assignment contains retired field %s: %s", retired, assigned.stdout)
		}
	}

	legacy := runBandmaster(t, repo, "task", "assign", created.Result.ID, "--worker", "legacy", "--json")
	if legacy.exitCode != 3 || !strings.Contains(legacy.stdout, "unknown option --worker") {
		t.Fatalf("retired flag did not fail normally: exit=%d stdout=%s stderr=%s", legacy.exitCode, legacy.stdout, legacy.stderr)
	}
}

func TestGoV2MigratesExistingStateAndKeepsPublicTaskOperationsWorking(t *testing.T) {
	repo := approvedCleanRepository(t)
	if started := runBandmaster(t, repo, "session", "start", "--json"); started.exitCode != 0 {
		t.Fatalf("start Session: %s", started.stdout)
	}
	created := successfulTaskCommand(t, repo, "create", "--title", "Migrated task", "--intent", "Preserve evidence", "--expected-outcome", "Inspection remains operational")
	if assigned := runBandmaster(t, repo, "task", "assign", created.Result.ID, "--agent", "agent-migrated", "--json"); assigned.exitCode != 0 {
		t.Fatalf("assign Agent: %s", assigned.stdout)
	}
	if paused := runBandmaster(t, repo, "session", "pause", "--json"); paused.exitCode != 0 {
		t.Fatalf("pause Session: %s", paused.stdout)
	}

	state, err := sql.Open("sqlite3", filepath.Join(repo, ".git", "bandmaster", "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	for _, statement := range []string{
		`PRAGMA foreign_keys = OFF`,
		`ALTER TABLE tasks RENAME COLUMN agent_identity TO worker_identity`,
		`ALTER TABLE task_audit_events RENAME COLUMN agent_identity TO worker_identity`,
		`ALTER TABLE batch_tasks RENAME TO batch_members`,
		`ALTER TABLE batch_members RENAME COLUMN task_order TO membership_order`,
		`ALTER TABLE frozen_batch_paths RENAME COLUMN task_order TO membership_order`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := state.Exec(statement); err != nil {
			_ = state.Close()
			t.Fatalf("prepare v1 state with %q: %v", statement, err)
		}
	}
	if err := state.Close(); err != nil {
		t.Fatalf("close v1 state: %v", err)
	}

	inspected := runBandmaster(t, repo, "task", "inspect", created.Result.ID, "--json")
	if inspected.exitCode != 0 || !strings.Contains(inspected.stdout, `"agent_identity":"agent-migrated"`) || strings.Contains(inspected.stdout, `"worker_identity"`) {
		t.Fatalf("migrated Task is not operational: exit=%d stdout=%s stderr=%s", inspected.exitCode, inspected.stdout, inspected.stderr)
	}
	state, err = sql.Open("sqlite3", filepath.Join(repo, ".git", "bandmaster", "state.db"))
	if err != nil {
		t.Fatalf("reopen migrated state: %v", err)
	}
	defer state.Close()
	var schemaVersion int
	if err := state.QueryRow(`PRAGMA user_version`).Scan(&schemaVersion); err != nil || schemaVersion != 2 {
		t.Fatalf("migrated user_version=%d err=%v", schemaVersion, err)
	}
	var legacyTables int
	if err := state.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='batch_members'`).Scan(&legacyTables); err != nil || legacyTables != 0 {
		t.Fatalf("legacy Batch Member table remains: count=%d err=%v", legacyTables, err)
	}
}

func TestGoV2ConfigurationRejectsTrackedV1WithRegenerationGuidance(t *testing.T) {
	repo := newGitRepository(t)
	commitRepository(t, repo)
	writeFile(t, filepath.Join(repo, ".bandmaster.yaml"), "version: 1\nworker_lease_duration: 5m\nvalidation:\n  commands: []\n")
	result := runBandmaster(t, repo, "config", "status", "--json")
	if result.exitCode != 3 || !strings.Contains(result.stdout, "retired Worker settings") || !strings.Contains(result.stdout, "Regenerate") || !strings.Contains(result.stdout, "approve the new digest") {
		t.Fatalf("v1 configuration guidance: exit=%d stdout=%s stderr=%s", result.exitCode, result.stdout, result.stderr)
	}
	content, err := os.ReadFile(filepath.Join(repo, ".bandmaster.yaml"))
	if err != nil {
		t.Fatalf("read old configuration: %v", err)
	}
	if !strings.Contains(string(content), "worker_lease_duration") {
		t.Fatalf("old tracked configuration was silently rewritten: %s", content)
	}
}

func TestGeneratedGuidanceDistinguishesOrchestratorAndExecutingAgents(t *testing.T) {
	repo := newGitRepository(t)
	result := runBandmaster(t, repo, "init", "--debug-skill", "--json")
	if result.exitCode != 0 {
		t.Fatalf("init: %s", result.stderr)
	}
	skill := readFile(t, filepath.Join(repo, ".agents", "skills", "bandmaster", "SKILL.md"))
	for _, expected := range []string{"The Orchestrator Agent is the sole orchestrator", "Agents must never spawn or orchestrate peers", "--agent <stable-agent-id>", "--terminated-agent <id>"} {
		if !strings.Contains(skill, expected) {
			t.Errorf("generated guidance does not contain %q:\n%s", expected, skill)
		}
	}
	for _, retired := range []string{"--worker", "--terminated-worker", "worker_lease_duration", "batch member"} {
		if strings.Contains(strings.ToLower(skill), retired) {
			t.Errorf("generated guidance contains retired vocabulary %q", retired)
		}
	}
	debugSkill := readFile(t, filepath.Join(repo, ".agents", "skills", "debug-bandmaster", "SKILL.md"))
	if !strings.Contains(debugSkill, "derived agents") || !strings.Contains(debugSkill, "do not invent a persisted Agent entity") {
		t.Errorf("generated debugging guidance does not explain derived Agents:\n%s", debugSkill)
	}
}
