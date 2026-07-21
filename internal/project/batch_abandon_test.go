package project

import (
	"database/sql"
	"testing"
)

func TestMigrateBatchAbandonmentSchemaPreservesForeignKeyTargets(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:batch-abandon-migration?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE sessions(id TEXT PRIMARY KEY)`,
		`CREATE TABLE batches (
			id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(id),
			creation_order INTEGER NOT NULL, base_branch TEXT NOT NULL, base_commit TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('collecting', 'frozen', 'validating', 'repair_pending', 'repairing', 'finalizing', 'final_validating', 'committed', 'quarantined')),
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(session_id, creation_order))`,
		`CREATE TABLE members(batch_id TEXT NOT NULL REFERENCES batches(id))`,
		`INSERT INTO sessions VALUES('session')`,
		`INSERT INTO batches VALUES('batch', 'session', 1, 'main', 'abc', 'collecting', 'now', 'now')`,
		`INSERT INTO members VALUES('batch')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("prepare old schema with %q: %v", statement, err)
		}
	}
	if projectError := migrateBatchAbandonmentSchema(db); projectError != nil {
		t.Fatalf("migrate old batch schema: %+v", projectError)
	}
	if _, err := db.Exec(`UPDATE batches SET status = 'abandoned' WHERE id = 'batch'`); err != nil {
		t.Fatalf("migrated status rejects abandonment: %v", err)
	}
	var target string
	if err := db.QueryRow(`SELECT "table" FROM pragma_foreign_key_list('members') LIMIT 1`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target != "batches" {
		t.Fatalf("child foreign key target = %q, want batches", target)
	}
}
