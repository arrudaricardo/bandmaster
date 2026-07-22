package integration_test

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

func TestSessionAbortDryRunDoesNotMigrateLegacyOwnershipSchema(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Preview legacy abort", "--intent", "Keep schema untouched", "--expected-outcome", "Dry-run is read-only")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "legacy-preview-agent")
	successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "legacy-preview.txt")
	successfulSessionCommand(t, repo, "pause")

	statePath := filepath.Join(repo, ".git", "bandmaster", "state.db")
	state, err := sql.Open("sqlite3", statePath)
	if err != nil {
		t.Fatalf("open legacy preview fixture: %v", err)
	}
	if _, err := state.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disable fixture foreign keys: %v", err)
	}
	if _, err := state.Exec(`DROP TABLE task_path_ownership`); err != nil {
		t.Fatalf("remove post-legacy ownership table: %v", err)
	}
	if _, err := state.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint legacy preview fixture: %v", err)
	}
	var schemaVersionBefore int
	if err := state.QueryRow(`PRAGMA schema_version`).Scan(&schemaVersionBefore); err != nil {
		t.Fatalf("read legacy schema version: %v", err)
	}
	if err := state.Close(); err != nil {
		t.Fatalf("close legacy preview fixture: %v", err)
	}
	bytesBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read legacy state bytes: %v", err)
	}
	statBefore, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat legacy state: %v", err)
	}

	preview := runBandmaster(t, repo, "session", "abort", "--dry-run", "--termination-confirmation", "legacy agent stopped", "--json")
	if preview.exitCode != 0 {
		t.Fatalf("preview legacy abort: exit=%d stdout=%s stderr=%s", preview.exitCode, preview.stdout, preview.stderr)
	}
	bytesAfter, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("reread legacy state bytes: %v", err)
	}
	statAfter, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("restat legacy state: %v", err)
	}
	if !bytes.Equal(bytesBefore, bytesAfter) || !statBefore.ModTime().Equal(statAfter.ModTime()) {
		t.Fatalf("abort preview changed legacy state file: bytes_equal=%t mtime_before=%s mtime_after=%s", bytes.Equal(bytesBefore, bytesAfter), statBefore.ModTime(), statAfter.ModTime())
	}
	state, err = sql.Open("sqlite3", statePath)
	if err != nil {
		t.Fatalf("reopen legacy preview fixture: %v", err)
	}
	defer state.Close()
	var ownershipTables, schemaVersionAfter int
	if err := state.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'task_path_ownership'`).Scan(&ownershipTables); err != nil {
		t.Fatalf("inspect ownership migration state: %v", err)
	}
	if err := state.QueryRow(`PRAGMA schema_version`).Scan(&schemaVersionAfter); err != nil {
		t.Fatalf("read schema version after preview: %v", err)
	}
	if ownershipTables != 0 || schemaVersionAfter != schemaVersionBefore {
		t.Fatalf("abort preview migrated legacy schema: ownership_tables=%d schema_before=%d schema_after=%d", ownershipTables, schemaVersionBefore, schemaVersionAfter)
	}
}

func TestExistingClaimBackedSnapshotsMigrateWithoutLosingEvidence(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "legacy.txt"), "before\n")
	runGit(t, repo, "add", "legacy.txt")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Add legacy fixture")
	successfulSessionCommand(t, repo, "start")
	task := successfulTaskCommand(t, repo, "create", "--title", "Migrate ownership", "--intent", "Retain legacy evidence", "--expected-outcome", "Claims become releasable")
	assignment := successfulTaskCommand(t, repo, "assign", task.Result.ID, "--agent", "agent-legacy-migration")
	claimed := successfulTaskCommand(t, repo, "claim", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--path", "legacy.txt")
	writeFile(t, filepath.Join(repo, "legacy.txt"), "after\n")
	if reviewed := runBandmaster(t, repo, "task", "diff", task.Result.ID, "--token", assignment.Result.AssignmentToken, "--json"); reviewed.exitCode != 0 {
		t.Fatalf("review legacy migration diff: exit=%d stdout=%s stderr=%s", reviewed.exitCode, reviewed.stdout, reviewed.stderr)
	}
	submitted := successfulTaskCommand(t, repo, "submit", task.Result.ID,
		"--token", assignment.Result.AssignmentToken,
		"--behavior-changed", "Legacy content changed",
		"--key-decisions", "Retain ownership attribution",
		"--validation-expectations", "Migration preserves snapshots",
		"--known-risks", "None",
	)
	successfulSessionCommand(t, repo, "pause")

	state, err := sql.Open("sqlite3", filepath.Join(repo, ".git", "bandmaster", "state.db"))
	if err != nil {
		t.Fatalf("open legacy state fixture: %v", err)
	}
	for _, statement := range []string{
		`PRAGMA foreign_keys = OFF`,
		`ALTER TABLE submitted_snapshots RENAME TO submitted_snapshots_evidence_backed`,
		`ALTER TABLE task_diff_reviews RENAME TO task_diff_reviews_evidence_backed`,
		`DROP TABLE task_path_ownership`,
		`CREATE TABLE submitted_snapshots (
			task_id TEXT NOT NULL REFERENCES tasks(id), path TEXT NOT NULL,
			presence TEXT NOT NULL, file_type TEXT NOT NULL, content_hash TEXT,
			executable INTEGER NOT NULL, content BLOB, PRIMARY KEY(task_id, path),
			FOREIGN KEY(task_id, path) REFERENCES claims(task_id, path))`,
		`INSERT INTO submitted_snapshots SELECT * FROM submitted_snapshots_evidence_backed`,
		`DROP TABLE submitted_snapshots_evidence_backed`,
		`CREATE TABLE task_diff_reviews (
			task_id TEXT NOT NULL REFERENCES tasks(id), path TEXT NOT NULL,
			presence TEXT NOT NULL, file_type TEXT NOT NULL, content_hash TEXT,
			executable INTEGER NOT NULL, reviewed_at TEXT NOT NULL, PRIMARY KEY(task_id, path),
			FOREIGN KEY(task_id, path) REFERENCES claims(task_id, path))`,
		`INSERT INTO task_diff_reviews SELECT * FROM task_diff_reviews_evidence_backed`,
		`DROP TABLE task_diff_reviews_evidence_backed`,
		`PRAGMA foreign_keys = ON`,
	} {
		if _, err := state.Exec(statement); err != nil {
			state.Close()
			t.Fatalf("prepare legacy state with %q: %v", statement, err)
		}
	}
	if err := state.Close(); err != nil {
		t.Fatalf("close legacy state fixture: %v", err)
	}

	migrated := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if len(migrated.Result.Claims) != 1 || len(migrated.Result.OwnershipEvidence) != 1 {
		t.Fatalf("migration lost active or immutable ownership: %+v", migrated.Result)
	}
	evidence := migrated.Result.OwnershipEvidence[0]
	if evidence.Path != "legacy.txt" || evidence.Baseline.ContentHash != claimed.Result.Claims[0].Baseline.ContentHash || evidence.SubmittedSnapshot == nil || evidence.SubmittedSnapshot.ContentHash != submitted.Result.Claims[0].SubmittedSnapshot.ContentHash || migrated.Result.Submission == nil {
		t.Fatalf("migration lost baseline, submission, or attribution: %+v", migrated.Result)
	}

	aborted := runBandmaster(t, repo, "session", "abort", "--termination-confirmation", "legacy agent stopped", "--json")
	if aborted.exitCode != 0 {
		t.Fatalf("abort migrated submitted task: exit=%d stdout=%s stderr=%s", aborted.exitCode, aborted.stdout, aborted.stderr)
	}
	afterAbort := successfulTaskCommand(t, repo, "inspect", task.Result.ID)
	if len(afterAbort.Result.Claims) != 0 || len(afterAbort.Result.OwnershipEvidence) != 1 || afterAbort.Result.OwnershipEvidence[0].SubmittedSnapshot == nil || afterAbort.Result.Submission == nil {
		t.Fatalf("migrated evidence did not survive claim release: %+v", afterAbort.Result)
	}
}
