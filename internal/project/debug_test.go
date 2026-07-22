package project

import "testing"

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
