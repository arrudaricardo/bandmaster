package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/bandmaster-dev/bandmaster/internal/project"
	tea "github.com/charmbracelet/bubbletea"
)

func dashboardFixture() project.DebugSnapshot {
	return project.DebugSnapshot{
		State:         project.DebugState{Initialization: "ready"},
		Configuration: project.DebugConfiguration{Present: true, Approved: true},
		Session:       &project.DebugSession{ID: "session_1234567890", Status: "active", StartingBranch: "main", StartingCommit: "abcdef1234567890"},
		Tasks: []project.DebugTask{
			{ID: "task-parser", CreationOrder: 1, Title: "Build parser", Status: "editing", AgentIdentity: "agent-parser", Claims: []project.DebugClaim{{Path: "parser.go"}}, Lease: &project.DebugLease{Status: "active", ExpiresAt: "2030-01-01T00:00:00Z"}},
			{ID: "task-docs", CreationOrder: 2, Title: "Write docs", Status: "planned"},
		},
		Agents: []project.DebugAgent{{AgentIdentity: "agent-parser", ActiveTaskID: "task-parser", ClaimPaths: []string{"parser.go"}, Lease: &project.DebugLease{Status: "active", ExpiresAt: "2030-01-01T00:00:00Z"}}},
	}
}

func TestDashboardStartsWithTaskOperationsAndAuthoritativeDetails(t *testing.T) {
	m := model{debug: dashboardFixture(), width: 120, height: 30, noColor: true}
	m.reconcileSelection()
	view := m.View()
	for _, want := range []string{"read-only operations dashboard", "[Tasks]", "In progress", "Build parser", "Exact state: editing", "Agent: agent-parser", "Owned files: parser.go", "r refresh", "q quit"} {
		if !strings.Contains(view, want) {
			t.Errorf("dashboard view does not contain %q:\n%s", want, view)
		}
	}
}

func TestDashboardPinsStableLiveStatusToBottomRow(t *testing.T) {
	m := model{debug: dashboardFixture(), width: 90, height: 24, lastUpdatedAt: time.Now(), noColor: true}
	m.reconcileSelection()

	view := m.View()
	lines := strings.Split(strings.TrimSuffix(view, "\n"), "\n")
	if got, want := lines[len(lines)-1], "  live · updated 0s ago · q quit · r refresh"; got != want {
		t.Fatalf("bottom row = %q, want %q:\n%s", got, want, view)
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	view = updated.(model).View()
	if strings.Contains(view, "refreshing") {
		t.Fatalf("manual refresh changed visible live status:\n%s", view)
	}
}

func TestTaskGroupingUsesOperationalUrgencyAndStableCreationOrder(t *testing.T) {
	m := model{debug: dashboardFixture()}
	m.debug.Tasks = append(m.debug.Tasks,
		project.DebugTask{ID: "task-blocked-later", CreationOrder: 4, Title: "Blocked later", Status: "blocked"},
		project.DebugTask{ID: "task-blocked-first", CreationOrder: 3, Title: "Blocked first", Status: "quarantined"},
		project.DebugTask{ID: "task-done", CreationOrder: 5, Title: "Done", Status: "committed"},
	)
	items := m.activeItems()
	got := []string{items[0].id, items[1].id, items[2].id, items[3].id, items[4].id}
	want := []string{"task-blocked-first", "task-blocked-later", "task-parser", "task-docs", "task-done"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("task order = %v, want %v", got, want)
	}
}

func TestSelectionFollowsStableTaskIDAcrossRefresh(t *testing.T) {
	m := model{debug: dashboardFixture()}
	m.reconcileSelection()
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(model)
	if m.selectedIDs[0] != "task-docs" {
		t.Fatalf("selected task = %q", m.selectedIDs[0])
	}
	next := dashboardFixture()
	next.Tasks = append([]project.DebugTask{{ID: "task-urgent", CreationOrder: 3, Title: "Urgent", Status: "blocked"}}, next.Tasks...)
	previous := m.debug
	m.debug = next
	m.reconcileAfterRefresh(previous)
	if m.selectedIDs[0] != "task-docs" {
		t.Fatalf("selection moved after reordering: %q", m.selectedIDs[0])
	}
	if m.newlyUrgent["task-urgent"] {
		t.Fatal("a newly created urgent Task should not be marked as a state transition")
	}
	moved := next
	moved.Tasks = append([]project.DebugTask(nil), next.Tasks...)
	moved.Tasks[1].Status = "blocked"
	m.debug = moved
	m.reconcileAfterRefresh(next)
	if m.selectedIDs[0] != "task-docs" || !m.newlyUrgent["task-parser"] {
		t.Fatalf("urgent transition stole focus or was not marked: selected=%q markers=%v", m.selectedIDs[0], m.newlyUrgent)
	}
}

func TestDisappearingSelectionFallsBackDeterministicallyWithNotice(t *testing.T) {
	m := model{debug: dashboardFixture()}
	m.reconcileSelection()
	m.moveSelection(1)
	previous := m.debug
	m.debug.Tasks = m.debug.Tasks[:1]
	m.reconcileAfterRefresh(previous)
	if m.selectedIDs[0] != "task-parser" || !strings.Contains(m.notice, "disappeared") {
		t.Fatalf("fallback selection=%q notice=%q", m.selectedIDs[0], m.notice)
	}
}

func TestTaskFilterMatchesAgentOwnedPathAndBatch(t *testing.T) {
	m := model{debug: dashboardFixture()}
	m.debug.Tasks[0].BatchID = "batch-alpha"
	for _, query := range []string{"AGENT-PARSER", "parser.go", "batch-alpha", "in progress"} {
		m.filters[0] = query
		items := m.activeItems()
		if len(items) != 1 || items[0].id != "task-parser" {
			t.Fatalf("filter %q returned %#v", query, items)
		}
	}
}

func TestTaskDetailsNavigateDependenciesByStableID(t *testing.T) {
	m := model{debug: dashboardFixture(), details: true}
	m.debug.Tasks[1].Prerequisites = []string{"task-parser"}
	m.selectedIDs[0] = "task-docs"
	m.jumpTaskDependency(false)
	if m.selectedIDs[0] != "task-parser" {
		t.Fatalf("prerequisite jump selected %q", m.selectedIDs[0])
	}
	m.jumpTaskDependency(true)
	if m.selectedIDs[0] != "task-docs" {
		t.Fatalf("unlock jump selected %q", m.selectedIDs[0])
	}
}

func TestDiagnosticsUseUniqueStableIdentityAndRankCriticalSeverity(t *testing.T) {
	m := model{debug: dashboardFixture(), tab: 3}
	m.debug.Diagnostics = []project.DebugDiagnostic{
		{Code: "dependency_wait", Severity: "critical", Affected: project.DebugAffected{TaskIDs: []string{"task-parser"}}, Evidence: map[string]any{"attempt": 1}},
		{Code: "dependency_wait", Severity: "critical", Affected: project.DebugAffected{TaskIDs: []string{"task-parser"}}, Evidence: map[string]any{"attempt": 2}},
	}
	items := m.activeItems()
	if len(items) != 2 || items[0].id == items[1].id {
		t.Fatalf("diagnostic identities = %#v", items)
	}
	if items[0].secondary != "critical · dependency_wait" || !strings.Contains(m.healthBanner(), "Error diagnostic") {
		t.Fatalf("critical diagnostic was not prioritized: %#v banner=%q", items, m.healthBanner())
	}
	selected := items[1].id
	m.selectedIDs[3] = selected
	m.debug.Diagnostics[1].Evidence = map[string]any{"attempt": 3, "changed": true}
	m.reconcileAfterRefresh(m.debug)
	if m.selectedIDs[3] != selected {
		t.Fatalf("mutable evidence changed diagnostic identity: got %q want %q", m.selectedIDs[3], selected)
	}
}

func TestLongDiagnosticCommandsRemainReachableByDetailScrolling(t *testing.T) {
	diagnostic := project.DebugDiagnostic{Code: "long_command", Severity: "error", Affected: project.DebugAffected{TaskIDs: []string{"task-parser"}}, Evidence: map[string]any{"path": strings.Repeat("deep/", 20)}, SuggestedActions: []string{"bandmaster task recover task-parser --termination-proof " + strings.Repeat("evidence-", 20) + " --json"}}
	m := model{debug: dashboardFixture(), tab: 3, details: true, width: 50, height: 14}
	m.debug.Diagnostics = []project.DebugDiagnostic{diagnostic}
	m.selectedIDs[3] = diagnosticOccurrenceID(diagnosticStableID(diagnostic), 1)
	first := m.renderDetails(46)
	if !strings.Contains(first, "↓ more") {
		t.Fatalf("long details do not advertise scrolling:\n%s", first)
	}
	later := first
	for i := 0; i < 5 && !strings.Contains(later, "--json"); i++ {
		m.moveDetailScroll(1)
		later = m.renderDetails(46)
	}
	if !strings.Contains(later, "--json") {
		t.Fatalf("supported command remains inaccessible after scrolling:\n%s", later)
	}
}

func TestNoColorAndMinimumSizeRemainReadable(t *testing.T) {
	m := model{debug: dashboardFixture(), width: 40, height: 10, noColor: true}
	view := m.View()
	if strings.Contains(view, "\x1b[") || !strings.Contains(view, "Terminal too small") || !strings.Contains(view, "44 columns × 14 rows") {
		t.Fatalf("unexpected minimum-size view:\n%s", view)
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
