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

type snapshot struct {
	session *project.Session
	tasks   []project.Task
}

type snapshotMsg struct {
	snapshot snapshot
	err      error
}

type tickMsg time.Time

type model struct {
	project    *project.Project
	snapshot   snapshot
	err        error
	refreshing bool
	height     int
}

// Run starts the interactive, read-only Bandmaster status dashboard.
func Run(p *project.Project, input io.Reader, output io.Writer) error {
	program := tea.NewProgram(model{project: p, refreshing: true}, tea.WithAltScreen(), tea.WithInput(input), tea.WithOutput(output))
	_, err := program.Run()
	return err
}

func (m model) Init() tea.Cmd {
	return loadSnapshot(m.project)
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
			return m, loadSnapshot(m.project)
		}
	case snapshotMsg:
		m.snapshot = message.snapshot
		m.err = message.err
		m.refreshing = false
		return m, waitForRefresh()
	case tickMsg:
		m.refreshing = true
		return m, loadSnapshot(m.project)
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
	if m.snapshot.session == nil {
		content.WriteString("  \033[1;33mNo Bandmaster session has been recorded.\033[0m\n\n")
		content.WriteString("  Start when your repository is clean:\n  \033[1mbandmaster session start\033[0m\n")
		return content.String()
	}

	session := m.snapshot.session
	content.WriteString(fmt.Sprintf("  Session  %s  %s\n", statusBadge(session.Status), dim(shortID(session.ID))))
	content.WriteString(fmt.Sprintf("  Branch   \033[1m%s\033[0m  %s\n", session.StartingBranch, dim(shortID(session.StartingCommit))))
	if session.Monitor != nil {
		content.WriteString(fmt.Sprintf("  Monitor  %s  %s\n", statusBadge(session.Monitor.Status), dim(session.Monitor.HeartbeatAt)))
	}
	if len(session.IntegrityViolations) > 0 {
		content.WriteString(fmt.Sprintf("  \033[1;31m%d unresolved integrity violation(s)\033[0m\n", len(session.IntegrityViolations)))
	}

	content.WriteString("\n  \033[1mWork overview\033[0m\n")
	content.WriteString("  ────────────────────────────────────────────────────────────────\n")
	counts := taskCounts(m.snapshot.tasks)
	if len(counts) == 0 {
		content.WriteString("  No tasks planned yet.\n")
	} else {
		for _, status := range sortedKeys(counts) {
			content.WriteString(fmt.Sprintf("  %-18s %d\n", statusBadge(status), counts[status]))
		}
	}

	if len(m.snapshot.tasks) > 0 {
		content.WriteString("\n  \033[1mTasks\033[0m\n")
		content.WriteString("  \033[2mSTATUS              TASK                                      OWNER / CLAIMS\033[0m\n")
		content.WriteString("  ─────────────────────────────────────────────────────────────────────────\n")
		for index, task := range m.snapshot.tasks {
			if index == 12 {
				content.WriteString(fmt.Sprintf("  \033[2m… %d more task(s); use task list --json for the complete record\033[0m\n", len(m.snapshot.tasks)-index))
				break
			}
			owner := task.WorkerIdentity
			if owner == "" {
				owner = "unassigned"
			}
			content.WriteString(fmt.Sprintf("  %-25s %-41s %s · %d claim(s)\n", task.Status, truncate(task.Title, 39), owner, len(task.Claims)))
		}
	}
	return content.String()
}

func loadSnapshot(p *project.Project) tea.Cmd {
	return func() tea.Msg {
		session, projectError := p.InspectSession()
		if projectError != nil {
			if projectError.Code == "session_not_found" {
				return snapshotMsg{}
			}
			return snapshotMsg{err: fmt.Errorf("%s", projectError.Message)}
		}
		_, tasks, projectError := p.ListTasks()
		if projectError != nil {
			return snapshotMsg{err: fmt.Errorf("%s", projectError.Message)}
		}
		return snapshotMsg{snapshot: snapshot{session: &session, tasks: tasks.Tasks}}
	}
}

func waitForRefresh() tea.Cmd {
	return tea.Tick(refreshInterval, func(now time.Time) tea.Msg {
		return tickMsg(now)
	})
}

func taskCounts(tasks []project.Task) map[string]int {
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
