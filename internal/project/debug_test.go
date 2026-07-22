package project

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestDebugDatabaseRevisionDetectsCommittedChange(t *testing.T) {
	root := t.TempDir()
	project := &Project{Root: root, GitDir: root}
	writer, projectError := project.openState()
	if projectError != nil {
		t.Fatal(projectError.Message)
	}
	reader, err := openDebugState(filepath.Join(root, "bandmaster", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx := context.Background()
	conn, err := reader.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	collectRevision := func() int64 {
		t.Helper()
		snapshot := newDebugDatabaseSnapshot(DebugSnapshot{Configuration: DebugConfiguration{Status: "valid", Present: true, Digest: "approved"}})
		tx, err := beginDebugRead(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		project.collectDebugDatabase(tx, DebugOptions{}, &snapshot)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return debugDatabaseRevision(snapshot)
	}
	before := collectRevision()
	if _, err := writer.Exec(`INSERT INTO metadata(key, value) VALUES('approved_configuration_digest', 'approved')`); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	after := collectRevision()
	if before == after {
		t.Fatalf("committed change was not observed: before=%d after=%d", before, after)
	}
}

func TestDebugLeaseDiagnosticsIgnoreTerminalHistoricalLeases(t *testing.T) {
	expiredLease := &DebugLease{
		Status:        "active",
		DurationNanos: int64(5 * time.Minute),
		RenewedAt:     "1999-12-31T23:55:00Z",
		ExpiresAt:     "2000-01-01T00:00:00Z",
	}
	snapshot := DebugSnapshot{
		Session: &DebugSession{ID: "session-debug"},
		Tasks: []DebugTask{
			{ID: "task-live", Status: "assigned", WorkerIdentity: "worker-live", AssignmentTokenPresent: true, Lease: expiredLease, Prerequisites: []string{}, Claims: []DebugClaim{}},
			{ID: "task-canceled", Status: "canceled", Lease: expiredLease, Prerequisites: []string{}, Claims: []DebugClaim{}},
			{ID: "task-committed", Status: "committed", Lease: expiredLease, Prerequisites: []string{}, Claims: []DebugClaim{}},
			{ID: "task-no-op", Status: "no_op", Lease: expiredLease, Prerequisites: []string{}, Claims: []DebugClaim{}},
		},
		Diagnostics: []DebugDiagnostic{},
	}
	project := &Project{Root: t.TempDir()}
	project.deriveDiagnostics(&snapshot)

	var expiryDiagnostics []DebugDiagnostic
	for _, diagnostic := range snapshot.Diagnostics {
		if diagnostic.Code == "lease_expired" || diagnostic.Code == "lease_expiring" {
			expiryDiagnostics = append(expiryDiagnostics, diagnostic)
		}
	}
	if len(expiryDiagnostics) != 1 || len(expiryDiagnostics[0].Affected.TaskIDs) != 1 || expiryDiagnostics[0].Affected.TaskIDs[0] != "task-live" {
		t.Fatalf("lease timing diagnostics = %+v", expiryDiagnostics)
	}
	for _, task := range snapshot.Tasks {
		if task.Lease != expiredLease {
			t.Fatalf("historical lease evidence changed for %s: %+v", task.ID, task.Lease)
		}
	}
}

func TestDebugChangesReportsMonitorAndIntegritySemantics(t *testing.T) {
	before := DebugSnapshot{Monitors: []DebugMonitor{{Generation: 1, Status: "healthy", HeartbeatAt: "one"}}}
	heartbeatOnly := DebugSnapshot{Monitors: []DebugMonitor{{Generation: 1, Status: "healthy", HeartbeatAt: "two"}}}
	if changes := DebugChanges(before, heartbeatOnly); len(changes) != 0 {
		t.Fatalf("heartbeat produced semantic changes: %+v", changes)
	}

	unhealthy := DebugSnapshot{Monitors: []DebugMonitor{{Generation: 1, Status: "unhealthy", HeartbeatAt: "three"}}}
	changes := DebugChanges(before, unhealthy)
	if len(changes) != 1 || changes[0].Kind != "monitor_changed" || changes[0].EntityID != "1" {
		t.Fatalf("monitor change = %+v", changes)
	}

	withViolation := unhealthy
	withViolation.Integrity = []DebugIntegrity{{ID: 42, Kind: "unowned_path"}}
	changes = DebugChanges(unhealthy, withViolation)
	if len(changes) != 1 || changes[0].Kind != "integrity_violation_changed" || changes[0].EntityID != "42" {
		t.Fatalf("integrity change = %+v", changes)
	}
}
