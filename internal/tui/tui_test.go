package tui

import (
	"strings"
	"testing"

	"github.com/bandmaster-dev/bandmaster/internal/project"
)

func TestDashboardViewSummarizesSessionAndTasks(t *testing.T) {
	m := model{snapshot: snapshot{
		session: &project.Session{ID: "session_1234567890", Status: "active", StartingBranch: "main", StartingCommit: "abcdef1234567890"},
		tasks: []project.Task{
			{Title: "Build parser", Status: "editing", WorkerIdentity: "worker-parser", Claims: []project.Claim{{Path: "parser.go"}}},
			{Title: "Write docs", Status: "planned"},
		},
	}}

	m.height = 30
	view := m.View()
	for _, want := range []string{"BANDMASTER", "live status dashboard", "Build parser", "worker-parser", "editing", "planned", "r refresh", "q quit"} {
		if !strings.Contains(view, want) {
			t.Errorf("dashboard view does not contain %q:\n%s", want, view)
		}
	}
}

func TestTaskCountsAndTruncationAreStable(t *testing.T) {
	counts := taskCounts([]project.Task{{Status: "ready"}, {Status: "ready"}, {Status: "blocked"}})
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
