package tui

import (
	"strings"
	"testing"

	"github.com/bandmaster-dev/bandmaster/internal/project"
)

func TestDashboardViewSummarizesSessionAndTasks(t *testing.T) {
	m := model{debug: project.DebugSnapshot{
		Session: &project.DebugSession{ID: "session_1234567890", Status: "active", StartingBranch: "main", StartingCommit: "abcdef1234567890"},
		Tasks: []project.DebugTask{
			{Title: "Build parser", Status: "editing", WorkerIdentity: "worker-parser", Claims: []project.DebugClaim{{Path: "parser.go"}}},
			{Title: "Write docs", Status: "planned"},
		},
		Workers: []project.DebugWorker{{WorkerIdentity: "worker-parser", ActiveTaskID: "task_parser", ClaimPaths: []string{"parser.go"}, Lease: &project.DebugLease{Status: "active", ExpiresAt: "2030-01-01T00:00:00Z"}}},
	}}

	m.height = 30
	view := m.View()
	for _, want := range []string{"BANDMASTER", "live status dashboard", "Build parser", "worker-parser", "editing", "planned", "lease active", "parser.go", "r refresh", "q quit"} {
		if !strings.Contains(view, want) {
			t.Errorf("dashboard view does not contain %q:\n%s", want, view)
		}
	}
}

func TestTaskCountsAndTruncationAreStable(t *testing.T) {
	counts := taskCounts([]project.DebugTask{{Status: "ready"}, {Status: "ready"}, {Status: "blocked"}})
	if counts["ready"] != 2 || counts["blocked"] != 1 {
		t.Fatalf("task counts = %#v", counts)
	}
	if got := sortedKeys(counts); len(got) != 2 || got[0] != "blocked" || got[1] != "ready" {
		t.Fatalf("sorted keys = %#v", got)
	}
	if got := truncate("abcdefgh", 5); got != "abcd…" {
		t.Fatalf("truncated title = %q", got)
	}
}
