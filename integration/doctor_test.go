package integration_test

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type doctorResponse struct {
	SchemaVersion string `json:"schema_version"`
	Command       string `json:"command"`
	Success       bool   `json:"success"`
	Result        struct {
		Healthy  bool `json:"healthy"`
		Findings []struct {
			Code     string `json:"code"`
			Severity string `json:"severity"`
			Entities struct {
				SessionID string   `json:"session_id,omitempty"`
				BatchIDs  []string `json:"batch_ids"`
				TaskIDs   []string `json:"task_ids"`
			} `json:"entities"`
			Paths            []string        `json:"paths"`
			Evidence         json.RawMessage `json:"evidence"`
			SupportedActions []string        `json:"supported_actions"`
		} `json:"findings"`
	} `json:"result"`
}

func decodeDoctor(t *testing.T, result commandResult) doctorResponse {
	t.Helper()
	if result.exitCode != 0 || result.stdout == "" {
		t.Fatalf("doctor command failed: %+v", result)
	}
	var response doctorResponse
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("decode doctor response: %v\n%s", err, result.stdout)
	}
	return response
}

func doctorFinding(t *testing.T, response doctorResponse, code string) json.RawMessage {
	t.Helper()
	for _, finding := range response.Result.Findings {
		if finding.Code == code {
			if finding.Severity == "" || len(finding.Evidence) == 0 || finding.Entities.BatchIDs == nil || finding.Entities.TaskIDs == nil || finding.Paths == nil || len(finding.SupportedActions) == 0 {
				t.Fatalf("finding %s lacks stable structured fields: %+v", code, finding)
			}
			return finding.Evidence
		}
	}
	t.Fatalf("missing doctor finding %s: %+v", code, response.Result.Findings)
	return nil
}

type doctorStateSnapshot struct {
	SessionStatus string
	AuditCount    int
	MonitorStatus string
	MonitorBeat   string
	Head          string
	Index         string
	Worktree      string
}

func snapshotDoctorState(t *testing.T, repo string) doctorStateSnapshot {
	t.Helper()
	db := openDoctorState(t, repo)
	defer db.Close()
	var snapshot doctorStateSnapshot
	if err := db.QueryRow(`SELECT status FROM sessions ORDER BY created_at DESC LIMIT 1`).Scan(&snapshot.SessionStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&snapshot.AuditCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status, COALESCE(heartbeat_at, '') FROM session_monitors ORDER BY generation DESC LIMIT 1`).Scan(&snapshot.MonitorStatus, &snapshot.MonitorBeat); err != nil {
		t.Fatal(err)
	}
	snapshot.Head = strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	snapshot.Index = runGit(t, repo, "diff", "--cached", "--binary")
	snapshot.Worktree = runGit(t, repo, "status", "--porcelain=v1")
	return snapshot
}

func openDoctorState(t *testing.T, repo string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(repo, ".git", "bandmaster", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestDoctorHealthyIsStrictlyReadOnly(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	successfulSessionCommand(t, repo, "pause")
	before := snapshotDoctorState(t, repo)

	result := runBandmaster(t, repo, "doctor", "--json")
	if result.exitCode != 0 {
		t.Fatalf("doctor failed: %+v", result)
	}
	response := decodeDoctor(t, result)
	if response.SchemaVersion != "1" || response.Command != "doctor" || !response.Success || !response.Result.Healthy || len(response.Result.Findings) != 0 {
		t.Fatalf("unexpected healthy doctor result: %+v", response)
	}
	after := snapshotDoctorState(t, repo)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("doctor mutated repository state:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestDoctorReportsStateJournalIntegrityAndCleanupFindings(t *testing.T) {
	t.Run("incompatible pair", func(t *testing.T) {
		repo := approvedCleanRepository(t)
		started := successfulSessionCommand(t, repo, "start")
		successfulSessionCommand(t, repo, "pause")
		db := openDoctorState(t, repo)
		if _, err := db.Exec(`UPDATE sessions SET status = 'finalizing' WHERE id = ?`, started.Result.ID); err != nil {
			t.Fatal(err)
		}
		db.Close()
		response := decodeDoctor(t, runBandmaster(t, repo, "doctor", "--json"))
		if response.Result.Healthy {
			t.Fatal("incompatible state reported healthy")
		}
		doctorFinding(t, response, "incompatible_session_batch_state")
	})

	t.Run("contradictory and dangling journals", func(t *testing.T) {
		repo := repositoryWithValidation(t, "")
		started := successfulSessionCommand(t, repo, "start")
		task := successfulTaskCommand(t, repo, "create", "--title", "Journal doctor", "--intent", "Create journal evidence", "--expected-outcome", "Diagnosed interruption")
		assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "doctor-journal")
		successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
		writeFile(t, filepath.Join(repo, "owned.txt"), "journal\n")
		submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
		batch := successfulBatchCommand(t, repo, "freeze")
		successfulBatchCommand(t, repo, "validate")
		crashed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_CRASH_FINALIZATION_AT=prepared"}, "batch", "commit", "--json")
		if crashed.exitCode != 97 {
			t.Fatalf("did not create journal: %+v", crashed)
		}
		db := openDoctorState(t, repo)
		if _, err := db.Exec(`UPDATE batches SET status = 'repair_pending' WHERE id = ?`, batch.Result.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE sessions SET status = 'active' WHERE id = ?`, started.Result.ID); err != nil {
			t.Fatal(err)
		}
		db.Close()
		response := decodeDoctor(t, runBandmaster(t, repo, "doctor", "--json"))
		doctorFinding(t, response, "contradictory_finalization_journal")

		db = openDoctorState(t, repo)
		if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`DELETE FROM batches WHERE id = ?`, batch.Result.ID); err != nil {
			t.Fatal(err)
		}
		db.Close()
		response = decodeDoctor(t, runBandmaster(t, repo, "doctor", "--json"))
		doctorFinding(t, response, "dangling_finalization_journal")
	})

	t.Run("index classifications and unresolved integrity", func(t *testing.T) {
		repo := approvedCleanRepository(t)
		successfulSessionCommand(t, repo, "start")
		writeFile(t, filepath.Join(repo, "generic.txt"), "generic\n")
		runGit(t, repo, "add", "generic.txt")
		generic := decodeDoctor(t, runBandmaster(t, repo, "doctor", "--json"))
		doctorFinding(t, generic, "index_drift")
		failed := runBandmaster(t, repo, "session", "finish", "--json")
		if failed.exitCode != 4 {
			t.Fatalf("did not persist integrity violation: %+v", failed)
		}
		violated := decodeDoctor(t, runBandmaster(t, repo, "doctor", "--json"))
		doctorFinding(t, violated, "unresolved_integrity_violation")
	})

	t.Run("staged rollback residue", func(t *testing.T) {
		repo := repositoryWithValidation(t, "")
		successfulSessionCommand(t, repo, "start")
		task := successfulTaskCommand(t, repo, "create", "--title", "Residue", "--intent", "Diagnose staged finalization", "--expected-outcome", "Specific finding")
		assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "doctor-residue")
		successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
		writeFile(t, filepath.Join(repo, "owned.txt"), "residue\n")
		submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
		successfulBatchCommand(t, repo, "freeze")
		successfulBatchCommand(t, repo, "validate")
		crashed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_CRASH_FINALIZATION_AT=prepared"}, "batch", "commit", "--json")
		if crashed.exitCode != 97 {
			t.Fatalf("did not create journal: %+v", crashed)
		}
		runGit(t, repo, "add", "owned.txt")
		response := decodeDoctor(t, runBandmaster(t, repo, "doctor", "--json"))
		doctorFinding(t, response, "staged_rollback_residue")
		for _, finding := range response.Result.Findings {
			if finding.Code == "index_drift" {
				t.Fatal("journal-backed residue was also reported as generic index drift")
			}
		}
	})

	t.Run("unrelated staged path is index drift despite journal", func(t *testing.T) {
		repo := repositoryWithValidation(t, "")
		successfulSessionCommand(t, repo, "start")
		task := successfulTaskCommand(t, repo, "create", "--title", "Unrelated residue", "--intent", "Correlate journal paths", "--expected-outcome", "Generic drift remains generic")
		assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "doctor-unrelated")
		successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "owned.txt")
		writeFile(t, filepath.Join(repo, "owned.txt"), "submitted\n")
		submitBatchTask(t, repo, task.Result.ID, assignment.Result.AssignmentToken)
		successfulBatchCommand(t, repo, "freeze")
		successfulBatchCommand(t, repo, "validate")
		if crashed := runBandmasterWithEnvironment(t, repo, []string{"BANDMASTER_TEST_CRASH_FINALIZATION_AT=prepared"}, "batch", "commit", "--json"); crashed.exitCode != 97 {
			t.Fatalf("did not create journal: %+v", crashed)
		}
		writeFile(t, filepath.Join(repo, "unrelated.txt"), "staged\n")
		runGit(t, repo, "add", "unrelated.txt")
		response := decodeDoctor(t, runBandmaster(t, repo, "doctor", "--json"))
		doctorFinding(t, response, "index_drift")
		for _, finding := range response.Result.Findings {
			if finding.Code == "staged_rollback_residue" {
				t.Fatal("unrelated staged path was classified as journal-backed residue")
			}
		}
	})

	t.Run("database cleanup blocker", func(t *testing.T) {
		repo := approvedCleanRepository(t)
		successfulSessionCommand(t, repo, "start")
		task := successfulTaskCommand(t, repo, "create", "--title", "Legacy blocker", "--intent", "Expose dependency", "--expected-outcome", "Doctor action")
		assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--worker", "doctor-db")
		successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "blocker.txt")
		db := openDoctorState(t, repo)
		if _, err := db.Exec(`CREATE TABLE legacy_claim_dependency(task_id TEXT, path TEXT, FOREIGN KEY(task_id, path) REFERENCES claims(task_id, path))`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO legacy_claim_dependency VALUES(?, 'blocker.txt')`, task.Result.ID); err != nil {
			t.Fatal(err)
		}
		db.Close()
		response := decodeDoctor(t, runBandmaster(t, repo, "doctor", "--json"))
		doctorFinding(t, response, "database_cleanup_blocker")
	})
}
