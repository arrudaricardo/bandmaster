// Package tui provides Bandmaster's read-only terminal status dashboard.
package tui

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/bandmaster-dev/bandmaster/internal/project"
	tea "github.com/charmbracelet/bubbletea"
)

const refreshInterval = 2 * time.Second

type snapshotMsg struct {
	snapshot project.DebugSnapshot
	err      error
}

type tickMsg time.Time

type model struct {
	project    *project.Project
	options    project.DebugOptions
	debug      project.DebugSnapshot
	err        error
	refreshing bool
	height     int
}

// Run starts the interactive, read-only Bandmaster status dashboard.
func Run(p *project.Project, input io.Reader, output io.Writer) error {
	return RunDebug(p, project.DebugOptions{}, input, output)
}

// RunDebug renders the normalized debug model as Bandmaster's canonical dashboard.
func RunDebug(p *project.Project, options project.DebugOptions, input io.Reader, output io.Writer) error {
	program := tea.NewProgram(model{project: p, options: options, refreshing: true}, tea.WithAltScreen(), tea.WithInput(input), tea.WithOutput(output))
	_, err := program.Run()
	return err
}

func (m model) Init() tea.Cmd {
	return loadSnapshot(m.project, m.options)
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.height = message.Height
	case tea.KeyMsg:
		switch message.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "r":
			m.refreshing = true
			return m, loadSnapshot(m.project, m.options)
		}
	case snapshotMsg:
		m.debug = message.snapshot
		m.err = message.err
		m.refreshing = false
		return m, waitForRefresh()
	case tickMsg:
		m.refreshing = true
		return m, loadSnapshot(m.project, m.options)
	}
	return m, nil
}

func (m model) View() string {
	body := renderDashboard(m)
	padding := max(0, m.height-strings.Count(body, "\n")-2)
	return body + strings.Repeat("\n", padding) + footer(m.refreshing)
}

func renderDashboard(m model) string {
	var content strings.Builder
	content.WriteString("\n  \033[1;36mBANDMASTER\033[0m  \033[2m· live status dashboard\033[0m\n")
	content.WriteString("  \033[2m━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\033[0m\n\n")
	if m.err != nil {
		content.WriteString(fmt.Sprintf("  \033[1;31mUnable to load status\033[0m\n  %s\n\n", m.err))
		content.WriteString("  Run \033[1mbandmaster init\033[0m in a supported Git repository, then press \033[1mr\033[0m.\n")
		return content.String()
	}
	debugSnapshot := m.debug
	if debugSnapshot.Session == nil {
		content.WriteString("  \033[1;33mNo Bandmaster session has been recorded.\033[0m\n\n")
		content.WriteString(fmt.Sprintf("  State: %s · Collection: %s\n", debugSnapshot.State.Initialization, debugSnapshot.Collection.Status))
		content.WriteString("  Start when your repository is clean:\n  \033[1mbandmaster session start\033[0m\n")
		return content.String()
	}

	session := debugSnapshot.Session
	content.WriteString(fmt.Sprintf("  Session  %s  %s\n", statusBadge(session.Status), dim(shortID(session.ID))))
	content.WriteString(fmt.Sprintf("  Runtime  \033[1mbandmaster %s\033[0m  %s · %s/%s\n", debugSnapshot.Runtime.BandmasterVersion, debugSnapshot.Runtime.GoVersion, debugSnapshot.Runtime.GOOS, debugSnapshot.Runtime.GOARCH))
	content.WriteString(fmt.Sprintf("  Branch   \033[1m%s\033[0m  %s", debugSnapshot.Repository.Branch, dim(shortID(debugSnapshot.Repository.Head))))
	if len(debugSnapshot.Repository.ChangedPaths) > 0 {
		content.WriteString(fmt.Sprintf("  · %d changed path(s)", len(debugSnapshot.Repository.ChangedPaths)))
	}
	content.WriteString("\n")
	content.WriteString(fmt.Sprintf("  State    %s · config %s · collection %s\n", debugSnapshot.State.Initialization, debugSnapshot.Configuration.Status, debugSnapshot.Collection.Status))
	if len(debugSnapshot.Monitors) > 0 {
		monitor := debugSnapshot.Monitors[len(debugSnapshot.Monitors)-1]
		content.WriteString(fmt.Sprintf("  Monitor  %s  %s\n", statusBadge(monitor.Status), dim(monitor.HeartbeatAt)))
	}
	if len(debugSnapshot.Integrity) > 0 {
		content.WriteString(fmt.Sprintf("  \033[1;31m%d integrity violation record(s)\033[0m\n", len(debugSnapshot.Integrity)))
	}

	content.WriteString("\n  \033[1mWork overview\033[0m\n")
	content.WriteString("  ────────────────────────────────────────────────────────────────\n")
	counts := taskCounts(debugSnapshot.Tasks)
	if len(counts) == 0 {
		content.WriteString("  No tasks planned yet.\n")
	} else {
		for _, status := range sortedKeys(counts) {
			content.WriteString(fmt.Sprintf("  %-18s %d\n", statusBadge(status), counts[status]))
		}
	}

	if len(debugSnapshot.Tasks) > 0 {
		content.WriteString("\n  \033[1mTasks\033[0m\n")
		content.WriteString("  \033[2mSTATUS              TASK                                      OWNER / CLAIMS\033[0m\n")
		content.WriteString("  ─────────────────────────────────────────────────────────────────────────\n")
		for index, task := range debugSnapshot.Tasks {
			if index == 12 {
				content.WriteString(fmt.Sprintf("  \033[2m… %d more task(s); use debug --json for the complete record\033[0m\n", len(debugSnapshot.Tasks)-index))
				break
			}
			owner := task.WorkerIdentity
			if owner == "" {
				owner = "unassigned"
			}
			content.WriteString(fmt.Sprintf("  %-25s %-41s %s · %d claim(s)\n", task.Status, truncate(task.Title, 39), owner, len(task.Claims)))
		}
	}
	if len(debugSnapshot.Workers) > 0 {
		content.WriteString("\n  \033[1mWorkers, leases, and claims\033[0m\n")
		for _, worker := range debugSnapshot.Workers {
			lease := "no active lease"
			if worker.Lease != nil {
				lease = fmt.Sprintf("lease %s until %s", worker.Lease.Status, worker.Lease.ExpiresAt)
			}
			content.WriteString(fmt.Sprintf("  \033[1m%s\033[0m  %s · task %s\n", worker.WorkerIdentity, lease, shortID(worker.ActiveTaskID)))
			for _, claimPath := range worker.ClaimPaths {
				content.WriteString(fmt.Sprintf("    claim  %s\n", claimPath))
			}
		}
	}
	if len(debugSnapshot.Batches) > 0 {
		content.WriteString("\n  \033[1mBatches\033[0m\n")
		for _, batch := range debugSnapshot.Batches {
			content.WriteString(fmt.Sprintf("  %s  %s · %d member(s) · %d path(s)\n", statusBadge(batch.Status), shortID(batch.ID), len(batch.MemberTaskIDs), len(batch.Manifest)))
		}
	}
	if len(debugSnapshot.Diagnostics) > 0 {
		content.WriteString("\n  \033[1mActionable diagnostics\033[0m\n")
		for index, diagnostic := range debugSnapshot.Diagnostics {
			if index == 8 {
				break
			}
			action := "inspect debug --json"
			if len(diagnostic.SuggestedActions) > 0 {
				action = diagnostic.SuggestedActions[0]
			}
			content.WriteString(fmt.Sprintf("  %-24s %-8s %s\n", diagnostic.Code, diagnostic.Severity, action))
		}
	}
	return content.String()
}

func loadSnapshot(p *project.Project, options project.DebugOptions) tea.Cmd {
	return func() tea.Msg {
		snapshot, projectError := p.Debug(options)
		if projectError != nil {
			return snapshotMsg{err: fmt.Errorf("%s", projectError.Message)}
		}
		return snapshotMsg{snapshot: snapshot}
	}
}

func waitForRefresh() tea.Cmd {
	return tea.Tick(refreshInterval, func(now time.Time) tea.Msg {
		return tickMsg(now)
	})
}

func taskCounts(tasks []project.DebugTask) map[string]int {
	counts := make(map[string]int)
	for _, task := range tasks {
		counts[task.Status]++
	}
	return counts
}

func sortedKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func statusBadge(status string) string {
	color := "36"
	switch status {
	case "active", "ready", "editing", "submitted", "committed", "completed", "healthy", "passed":
		color = "32"
	case "blocked", "repair_pending", "paused", "aborting", "stopped":
		color = "33"
	case "quarantined", "failed", "unhealthy":
		color = "31"
	case "planned", "assigned", "finalizing":
		color = "35"
	}
	return fmt.Sprintf("\033[1;%sm%-16s\033[0m", color, status)
}

func footer(refreshing bool) string {
	state := "live"
	if refreshing {
		state = "refreshing"
	}
	return fmt.Sprintf("\n  \033[2m%s · refreshes every 2s · r refresh · q quit\033[0m\n", state)
}

func shortID(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func truncate(value string, length int) string {
	if len(value) <= length {
		return value
	}
	return value[:length-1] + "…"
}

func dim(value string) string { return "\033[2m" + value + "\033[0m" }
