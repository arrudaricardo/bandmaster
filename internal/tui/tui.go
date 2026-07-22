// Package tui provides Bandmaster's read-only terminal operations dashboard.
package tui

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bandmaster-dev/bandmaster/internal/project"
	tea "github.com/charmbracelet/bubbletea"
)

const refreshInterval = 2 * time.Second

var tabNames = []string{"Tasks", "Agents", "Batches", "Diagnostics"}

type snapshotMsg struct {
	snapshot project.DebugSnapshot
	err      error
}

type tickMsg time.Time

type model struct {
	project       *project.Project
	options       project.DebugOptions
	debug         project.DebugSnapshot
	err           error
	width         int
	height        int
	tab           int
	selectedIDs   [4]string
	selected      [4]int
	scroll        [4]int
	detailScroll  [4]int
	filters       [4]string
	newlyUrgent   map[string]bool
	filtering     bool
	details       bool
	help          bool
	notice        string
	lastUpdatedAt time.Time
	noColor       bool
}

// Run starts the interactive, read-only Bandmaster status dashboard.
func Run(p *project.Project, input io.Reader, output io.Writer) error {
	return RunDebug(p, project.DebugOptions{}, input, output)
}

// RunDebug renders the normalized debug model as Bandmaster's canonical dashboard.
func RunDebug(p *project.Project, options project.DebugOptions, input io.Reader, output io.Writer) error {
	program := tea.NewProgram(model{project: p, options: options, noColor: os.Getenv("NO_COLOR") != ""}, tea.WithAltScreen(), tea.WithInput(input), tea.WithOutput(output))
	_, err := program.Run()
	return err
}

func (m model) Init() tea.Cmd { return loadSnapshot(m.project, m.options) }

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = message.Width, message.Height
	case tea.KeyMsg:
		key := message.String()
		if m.filtering {
			switch key {
			case "enter":
				m.filtering = false
				m.reconcileSelection()
			case "esc":
				m.filtering = false
				m.filters[m.tab] = ""
				m.reconcileSelection()
			case "backspace", "ctrl+h":
				if query := m.filters[m.tab]; query != "" {
					_, size := lastRune(query)
					m.filters[m.tab] = query[:len(query)-size]
				}
				m.reconcileSelection()
			default:
				if len(message.Runes) > 0 {
					m.filters[m.tab] += string(message.Runes)
					m.reconcileSelection()
				}
			}
			return m, nil
		}
		switch key {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r":
			return m, loadSnapshot(m.project, m.options)
		case "up", "k":
			m.moveSelection(-1)
		case "down", "j":
			m.moveSelection(1)
		case "left", "h":
			if !m.details && !m.help {
				m.tab = (m.tab + len(tabNames) - 1) % len(tabNames)
				m.reconcileSelection()
			}
		case "right", "l":
			if !m.details && !m.help {
				m.tab = (m.tab + 1) % len(tabNames)
				m.reconcileSelection()
			}
		case "enter":
			if len(m.activeItems()) > 0 {
				m.details = true
			}
		case "esc":
			if m.help {
				m.help = false
			} else if m.details {
				m.details = false
			} else if m.filters[m.tab] != "" {
				m.filters[m.tab] = ""
				m.reconcileSelection()
			}
		case "?":
			m.help = !m.help
		case "/":
			m.filtering = true
		case "[":
			m.jumpTaskDependency(false)
		case "]":
			m.jumpTaskDependency(true)
		case "pgup":
			m.moveDetailScroll(-1)
		case "pgdown":
			m.moveDetailScroll(1)
		}
	case snapshotMsg:
		previous := m.debug
		m.debug, m.err = message.snapshot, message.err
		m.lastUpdatedAt = time.Now()
		m.reconcileAfterRefresh(previous)
		return m, waitForRefresh()
	case tickMsg:
		return m, loadSnapshot(m.project, m.options)
	}
	return m, nil
}

func (m model) View() string {
	if m.width > 0 && (m.width < 44 || m.height < 14) {
		return m.frame("BANDMASTER\n\nTerminal too small. Bandmaster needs at least 44 columns × 14 rows.\n\nq quit")
	}
	body := m.renderDashboard()
	if m.height > 0 {
		lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
		if len(lines) > m.height {
			footer := ""
			for i := len(lines) - 1; i >= 0; i-- {
				if strings.TrimSpace(lines[i]) != "" {
					footer = lines[i]
					break
				}
			}
			lines = append(lines[:m.height-1], footer)
		} else if len(lines) < m.height {
			padding := make([]string, m.height-len(lines))
			lines = append(lines[:len(lines)-1], append(padding, lines[len(lines)-1])...)
		}
		body = strings.Join(lines, "\n")
	}
	return body
}

func (m model) renderDashboard() string {
	if m.err != nil {
		return m.guided("Bandmaster is not initialized", "Bandmaster coordinates Agents working on durable Tasks in one repository.", "bandmaster init")
	}
	if screen := m.guidedState(); screen != "" {
		return screen
	}
	if m.help {
		return m.helpView()
	}

	var out strings.Builder
	out.WriteString(m.heading("BANDMASTER"))
	out.WriteString("  " + m.healthBanner() + "\n")
	out.WriteString("  " + m.renderTabs() + "\n")
	if m.notice != "" {
		out.WriteString("  ! " + m.notice + "\n")
	}
	if m.filtering || m.filters[m.tab] != "" {
		cursor := ""
		if m.filtering {
			cursor = "_"
		}
		out.WriteString(fmt.Sprintf("  Filter /%s%s  %d match(es)\n", m.filters[m.tab], cursor, len(m.activeItems())))
	}
	if m.details && m.width > 0 && m.width < 110 {
		out.WriteString(m.renderDetails(max(40, m.width-4)))
	} else if m.width >= 110 {
		list := strings.Split(strings.TrimSuffix(m.renderList(max(44, m.width/2-3)), "\n"), "\n")
		details := strings.Split(strings.TrimSuffix(m.renderDetails(max(44, m.width-m.width/2-5)), "\n"), "\n")
		rows := max(len(list), len(details))
		for i := 0; i < rows; i++ {
			left, right := "", ""
			if i < len(list) {
				left = list[i]
			}
			if i < len(details) {
				right = details[i]
			}
			out.WriteString(fmt.Sprintf("  %-*s │ %s\n", max(44, m.width/2-3), truncatePlain(left, max(44, m.width/2-3)), right))
		}
	} else {
		out.WriteString(m.renderList(max(40, m.width-4)))
	}
	out.WriteString(m.footer())
	return m.frame(out.String())
}

func (m model) guidedState() string {
	snapshot := m.debug
	if snapshot.State.Initialization == "uninitialized" || !snapshot.Configuration.Present {
		return m.guided("Initialize Bandmaster", "Create and review the tracked Agent configuration before orchestration begins.", "bandmaster init")
	}
	if !snapshot.Configuration.Approved {
		return m.guided("Configuration approval required", "Approval is a trust boundary: inspect .bandmaster.yaml before accepting its validation commands.", "bandmaster config status --json → bandmaster config approve <digest> --json")
	}
	if snapshot.Session == nil {
		return m.guided("Ready to coordinate", "The repository is initialized and no Session is active.", "bandmaster session start --json")
	}
	if hasQuarantine(snapshot) {
		return m.guided("Session quarantined", "Bandmaster stopped unsafe progress and preserved the evidence needed for recovery.", "bandmaster debug --json")
	}
	switch snapshot.Session.Status {
	case "paused":
		return m.guided("Session paused", "New assignments are stopped while durable Task and ownership evidence remains available.", "bandmaster session inspect --json")
	case "completed":
		return m.guided("Session completed", "All finalized work and audit evidence remain available for inspection.", "bandmaster session inspect --json")
	}
	return ""
}

func (m model) guided(title, explanation, command string) string {
	return m.frame(m.heading("BANDMASTER") + "\n  " + m.strong(title) + "\n\n  " + explanation + "\n\n  Next: " + command + m.footer())
}

func (m model) renderTabs() string {
	parts := make([]string, len(tabNames))
	for i, name := range tabNames {
		if i == m.tab {
			parts[i] = "[" + name + "]"
		} else {
			parts[i] = " " + name + " "
		}
	}
	return strings.Join(parts, "  ")
}

type displayItem struct {
	id, primary, secondary string
	urgent                 bool
	newlyUrgent            bool
}

func (m model) activeItems() []displayItem {
	query := strings.ToLower(m.filters[m.tab])
	items := make([]displayItem, 0)
	add := func(item displayItem, searchable ...string) {
		if query == "" || strings.Contains(strings.ToLower(strings.Join(searchable, " ")), query) {
			items = append(items, item)
		}
	}
	switch m.tab {
	case 0:
		tasks := append([]project.DebugTask(nil), m.debug.Tasks...)
		sort.SliceStable(tasks, func(i, j int) bool {
			gi, gj := taskGroupRank(tasks[i].Status), taskGroupRank(tasks[j].Status)
			if gi != gj {
				return gi < gj
			}
			return tasks[i].CreationOrder < tasks[j].CreationOrder
		})
		for _, task := range tasks {
			paths := make([]string, 0, len(task.Claims))
			for _, claim := range task.Claims {
				paths = append(paths, claim.Path)
			}
			group := taskGroup(task.Status)
			add(displayItem{id: task.ID, primary: task.Title, secondary: group + " · " + task.Status, urgent: group == "Needs attention", newlyUrgent: m.newlyUrgent[task.ID]}, task.ID, task.Title, group, task.Status, task.AgentIdentity, task.BatchID, strings.Join(paths, " "))
		}
	case 1:
		for _, agent := range m.debug.Agents {
			lease := "no active lease"
			if agent.Lease != nil {
				lease = agent.Lease.Status + " lease"
			}
			add(displayItem{id: agent.AgentIdentity, primary: agent.AgentIdentity, secondary: lease + " · task " + shortID(agent.ActiveTaskID)}, agent.AgentIdentity, agent.ActiveTaskID, lease, strings.Join(agent.ClaimPaths, " "))
		}
	case 2:
		for _, batch := range m.debug.Batches {
			taskIDs := debugBatchTaskIDs(batch)
			paths := make([]string, 0, len(batch.Manifest))
			for _, path := range batch.Manifest {
				paths = append(paths, path.Path)
			}
			validation := make([]string, 0, len(batch.Validation))
			for _, attempt := range batch.Validation {
				validation = append(validation, attempt.Status)
			}
			add(displayItem{id: batch.ID, primary: shortID(batch.ID), secondary: fmt.Sprintf("%s · %d Batch Tasks · %d paths", batch.Status, len(batch.Tasks), len(batch.Manifest))}, batch.ID, batch.Status, strings.Join(taskIDs, " "), strings.Join(paths, " "), strings.Join(validation, " "))
		}
	case 3:
		occurrences := map[string]int{}
		for _, diagnostic := range sortedDiagnostics(m.debug.Diagnostics) {
			baseID := diagnosticStableID(diagnostic)
			occurrences[baseID]++
			id := diagnosticOccurrenceID(baseID, occurrences[baseID])
			title := diagnosticTitle(diagnostic.Code)
			affected := append(append(append([]string{}, diagnostic.Affected.TaskIDs...), diagnostic.Affected.Agents...), diagnostic.Affected.BatchIDs...)
			affected = append(affected, diagnostic.Affected.Paths...)
			add(displayItem{id: id, primary: title, secondary: diagnostic.Severity + " · " + diagnostic.Code, urgent: diagnostic.Severity == "error" || diagnostic.Severity == "critical"}, title, diagnostic.Code, diagnostic.Severity, strings.Join(affected, " "), fmt.Sprint(diagnostic.Evidence), strings.Join(diagnostic.SuggestedActions, " "))
		}
	}
	return items
}

func (m model) renderList(width int) string {
	items := m.activeItems()
	if len(items) == 0 {
		return "\n  No matching " + strings.ToLower(tabNames[m.tab]) + ".\n"
	}
	start := min(m.scroll[m.tab], len(items)-1)
	limit := len(items)
	if m.height > 0 {
		limit = min(limit, start+max(3, m.height-9))
	}
	var out strings.Builder
	group := ""
	for i := start; i < limit; i++ {
		item := items[i]
		if m.tab == 0 {
			current := strings.SplitN(item.secondary, " · ", 2)[0]
			if current != group {
				group = current
				out.WriteString("\n  " + m.strong(group) + "\n")
			}
		}
		marker := " "
		if item.id == m.selectedIDs[m.tab] {
			marker = ">"
		}
		urgent := " "
		if item.urgent {
			urgent = "!"
		}
		if item.newlyUrgent {
			item.secondary = "NEW · " + item.secondary
		}
		if m.width > 0 && m.width < 72 {
			out.WriteString(fmt.Sprintf("  %s%s %s\n", marker, urgent, truncatePlain(item.primary, width-6)))
			out.WriteString(fmt.Sprintf("     %s\n", truncatePlain(item.secondary, width-7)))
		} else {
			out.WriteString(fmt.Sprintf("  %s%s %-*s  %s\n", marker, urgent, max(16, width/2), truncatePlain(item.primary, max(16, width/2)), truncatePlain(item.secondary, max(12, width/2-8))))
		}
	}
	return out.String()
}

func (m model) renderDetails(width int) string {
	id := m.selectedIDs[m.tab]
	if id == "" {
		return "\n  Select an item to inspect details.\n"
	}
	var lines []string
	switch m.tab {
	case 0:
		for _, task := range m.debug.Tasks {
			if task.ID != id {
				continue
			}
			agent := task.AgentIdentity
			if agent == "" {
				agent = "unassigned"
			}
			lease, expiry := "none", "n/a"
			if task.Lease != nil {
				lease, expiry = task.Lease.Status, task.Lease.ExpiresAt
			}
			paths := make([]string, 0, len(task.Claims))
			for _, claim := range task.Claims {
				paths = append(paths, claim.Path)
			}
			lines = []string{"Task " + task.Title, "ID: " + task.ID, "Exact state: " + task.Status, "Agent: " + agent, "Lease: " + lease, "Assignment expires: " + expiry, "Batch: " + valueOr(task.BatchID, "none"), "Owned files: " + valueOr(strings.Join(paths, ", "), "none"), "Blocked by: " + valueOr(strings.Join(task.Prerequisites, ", "), "none"), "Unlocks: " + valueOr(strings.Join(m.unlocks(task.ID), ", "), "none"), "Latest diagnostic: " + valueOr(m.latestTaskDiagnostic(task.ID), "none")}
			break
		}
	case 1:
		for _, agent := range m.debug.Agents {
			if agent.AgentIdentity == id {
				lease, expiry := "none", "n/a"
				if agent.Lease != nil {
					lease, expiry = agent.Lease.Status, agent.Lease.ExpiresAt
				}
				lines = []string{"Derived Agent " + agent.AgentIdentity, "Active Task: " + valueOr(agent.ActiveTaskID, "none"), "Lease: " + lease, "Assignment expires: " + expiry, "Owned files: " + valueOr(strings.Join(agent.ClaimPaths, ", "), "none"), "Latest activity: " + valueOr(agent.LastActivityAt, "unknown"), "Agents are derived from authoritative Task, Lease, Claim, and activity evidence."}
				break
			}
		}
	case 2:
		for _, batch := range m.debug.Batches {
			if batch.ID == id {
				lines = []string{"Batch " + batch.ID, "Exact state: " + batch.Status, "Ordered Batch Tasks: " + valueOr(strings.Join(debugBatchTaskIDs(batch), ", "), "none"), "Path count: " + strconv.Itoa(len(batch.Manifest)), "Validation attempts: " + strconv.Itoa(len(batch.Validation)), "Base: " + batch.BaseBranch + " @ " + shortID(batch.BaseCommit)}
				break
			}
		}
	case 3:
		occurrences := map[string]int{}
		for _, diagnostic := range m.debug.Diagnostics {
			baseID := diagnosticStableID(diagnostic)
			occurrences[baseID]++
			if diagnosticOccurrenceID(baseID, occurrences[baseID]) == id {
				action := "none"
				if len(diagnostic.SuggestedActions) > 0 {
					action = diagnostic.SuggestedActions[0]
				}
				lines = []string{diagnosticTitle(diagnostic.Code), "Code: " + diagnostic.Code, "Severity: " + diagnostic.Severity, "Impact: orchestration may require attention before safe progress.", "Evidence: " + fmt.Sprint(diagnostic.Evidence), "Next command: " + action, "Commands are displayed only and are never executed by the dashboard."}
				break
			}
		}
	}
	wrapped := make([]string, 0, len(lines))
	for _, line := range lines {
		wrapped = append(wrapped, wrapText(line, max(8, width))...)
	}
	start := min(m.detailScroll[m.tab], max(0, len(wrapped)-1))
	limit := len(wrapped)
	if m.height > 0 {
		limit = min(limit, start+max(3, m.height-8))
	}
	var out strings.Builder
	out.WriteString("\n  " + m.strong("Details") + "\n")
	for _, line := range wrapped[start:limit] {
		out.WriteString("  " + line + "\n")
	}
	if limit < len(wrapped) {
		out.WriteString("  ↓ more — PgDn\n")
	}
	if start > 0 {
		out.WriteString("  ↑ more — PgUp\n")
	}
	return out.String()
}

func (m model) helpView() string {
	return m.frame(m.heading("BANDMASTER HELP") + "\n  ↑/↓ or j/k  move selection\n  ←/→ or h/l  switch tabs\n  Enter       open details\n  Esc         close details or clear filter\n  /           filter the active tab\n  ?           toggle help\n  r           refresh\n  q           quit\n\n  The dashboard is strictly read-only. Suggested commands are never executed." + m.footer())
}

func (m model) footer() string {
	age := "0s"
	if !m.lastUpdatedAt.IsZero() {
		age = humanAge(time.Since(m.lastUpdatedAt))
	}
	return fmt.Sprintf("\n  live · updated %s ago · q quit · r refresh\n", age)
}

func (m *model) moveSelection(delta int) {
	items := m.activeItems()
	if len(items) == 0 {
		return
	}
	index := m.selected[m.tab] + delta
	if index < 0 {
		index = 0
	}
	if index >= len(items) {
		index = len(items) - 1
	}
	m.selected[m.tab], m.selectedIDs[m.tab] = index, items[index].id
	m.detailScroll[m.tab] = 0
	if m.height > 0 {
		visible := max(3, m.height-9)
		if index < m.scroll[m.tab] {
			m.scroll[m.tab] = index
		}
		if index >= m.scroll[m.tab]+visible {
			m.scroll[m.tab] = index - visible + 1
		}
	}
}

func (m *model) moveDetailScroll(delta int) {
	if delta < 0 {
		m.detailScroll[m.tab] = max(0, m.detailScroll[m.tab]-max(1, m.height-10))
		return
	}
	m.detailScroll[m.tab] += max(1, m.height-10)
}

func (m *model) jumpTaskDependency(unlocks bool) {
	if m.tab != 0 || !m.details {
		return
	}
	var targets []string
	if unlocks {
		targets = m.unlocks(m.selectedIDs[0])
	} else {
		for _, task := range m.debug.Tasks {
			if task.ID == m.selectedIDs[0] {
				targets = task.Prerequisites
				break
			}
		}
	}
	if len(targets) == 0 {
		return
	}
	for index, item := range m.activeItems() {
		if item.id == targets[0] {
			m.selected[0], m.selectedIDs[0] = index, item.id
			return
		}
	}
}

func (m *model) reconcileSelection() {
	items := m.activeItems()
	if len(items) == 0 {
		m.selectedIDs[m.tab], m.selected[m.tab], m.scroll[m.tab] = "", 0, 0
		return
	}
	for index, item := range items {
		if item.id == m.selectedIDs[m.tab] {
			m.selected[m.tab] = index
			return
		}
	}
	index := min(m.selected[m.tab], len(items)-1)
	m.selected[m.tab], m.selectedIDs[m.tab] = index, items[index].id
}

func (m *model) reconcileAfterRefresh(previous project.DebugSnapshot) {
	previousStatus := make(map[string]string, len(previous.Tasks))
	for _, task := range previous.Tasks {
		previousStatus[task.ID] = task.Status
	}
	m.newlyUrgent = make(map[string]bool)
	for _, task := range m.debug.Tasks {
		oldStatus, existed := previousStatus[task.ID]
		if existed && taskGroup(oldStatus) != "Needs attention" && taskGroup(task.Status) == "Needs attention" {
			m.newlyUrgent[task.ID] = true
		}
	}
	for tab := range tabNames {
		oldTab := m.tab
		m.tab = tab
		oldID := m.selectedIDs[tab]
		m.reconcileSelection()
		if oldID != "" && oldID != m.selectedIDs[tab] {
			m.notice = "The selected item disappeared; the nearest remaining item is selected."
		}
		m.tab = oldTab
	}
}

func (m model) unlocks(taskID string) []string {
	var ids []string
	for _, task := range m.debug.Tasks {
		for _, prerequisite := range task.Prerequisites {
			if prerequisite == taskID {
				ids = append(ids, task.ID)
			}
		}
	}
	return ids
}
func (m model) latestTaskDiagnostic(taskID string) string {
	for i := len(m.debug.Diagnostics) - 1; i >= 0; i-- {
		for _, id := range m.debug.Diagnostics[i].Affected.TaskIDs {
			if id == taskID {
				return m.debug.Diagnostics[i].Code
			}
		}
	}
	return ""
}

func taskGroup(status string) string {
	switch status {
	case "blocked", "repair_pending", "quarantined":
		return "Needs attention"
	case "assigned", "editing":
		return "In progress"
	case "submitted":
		return "Ready for batch"
	case "planned", "ready":
		return "Waiting"
	default:
		return "Finished"
	}
}
func taskGroupRank(status string) int {
	switch taskGroup(status) {
	case "Needs attention":
		return 0
	case "In progress":
		return 1
	case "Ready for batch":
		return 2
	case "Waiting":
		return 3
	default:
		return 4
	}
}
func sortedDiagnostics(input []project.DebugDiagnostic) []project.DebugDiagnostic {
	result := append([]project.DebugDiagnostic(nil), input...)
	sort.SliceStable(result, func(i, j int) bool { return diagnosticRank(result[i]) < diagnosticRank(result[j]) })
	return result
}
func diagnosticRank(d project.DebugDiagnostic) int {
	if strings.Contains(strings.ToLower(d.Code), "integrity") {
		return 0
	}
	if d.Severity == "critical" {
		return 1
	}
	if d.Severity == "error" {
		return 2
	}
	if strings.Contains(strings.ToLower(d.Code), "blocked") {
		return 3
	}
	return 4
}
func diagnosticTitle(code string) string {
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(code))
	for i := range words {
		words[i] = strings.ToUpper(words[i][:1]) + words[i][1:]
	}
	return strings.Join(words, " ")
}
func hasQuarantine(snapshot project.DebugSnapshot) bool {
	if len(snapshot.Integrity) > 0 {
		return true
	}
	for _, task := range snapshot.Tasks {
		if task.Status == "quarantined" {
			return true
		}
	}
	for _, batch := range snapshot.Batches {
		if batch.Status == "quarantined" {
			return true
		}
	}
	return false
}
func (m model) healthBanner() string {
	if len(m.debug.Integrity) > 0 {
		return "!! Integrity violation — progress is quarantined"
	}
	for _, d := range m.debug.Diagnostics {
		if d.Severity == "critical" || d.Severity == "error" {
			return "! Error diagnostic — inspect Diagnostics"
		}
	}
	for _, task := range m.debug.Tasks {
		if task.Status == "blocked" {
			return "! Blocked work — inspect Tasks"
		}
	}
	return "✓ Session healthy — ordinary progress"
}
func (m model) heading(title string) string {
	return "\n  " + m.strong(title) + "  · read-only operations dashboard\n  " + strings.Repeat("─", max(20, min(80, m.width-4))) + "\n"
}
func (m model) strong(value string) string {
	if m.noColor {
		return value
	}
	return "\033[1;36m" + value + "\033[0m"
}
func (m model) frame(value string) string {
	if m.noColor {
		return stripANSI(value)
	}
	return value
}
func stripANSI(value string) string {
	var out strings.Builder
	skipping := false
	for _, r := range value {
		if r == '\x1b' {
			skipping = true
			continue
		}
		if skipping {
			if r == 'm' {
				skipping = false
			}
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}
func humanAge(age time.Duration) string {
	if age < time.Second {
		return "0s"
	}
	if age < time.Minute {
		return fmt.Sprintf("%ds", int(age.Seconds()))
	}
	return fmt.Sprintf("%dm", int(age.Minutes()))
}
func lastRune(value string) (rune, int) {
	return utf8.DecodeLastRuneInString(value)
}
func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
func debugBatchTaskIDs(batch project.DebugBatch) []string {
	ids := make([]string, 0, len(batch.Tasks))
	for _, task := range batch.Tasks {
		ids = append(ids, task.TaskID)
	}
	return ids
}
func shortID(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}
func truncatePlain(value string, length int) string {
	if length <= 1 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= length {
		return value
	}
	return string(runes[:length-1]) + "…"
}

func wrapText(value string, width int) []string {
	runes := []rune(value)
	if len(runes) == 0 {
		return []string{""}
	}
	var lines []string
	for len(runes) > width {
		lines = append(lines, string(runes[:width]))
		runes = runes[width:]
	}
	return append(lines, string(runes))
}

func diagnosticStableID(diagnostic project.DebugDiagnostic) string {
	return diagnostic.Code + "|" + strings.Join(diagnostic.Affected.SessionIDs, ",") + "|" + strings.Join(diagnostic.Affected.BatchIDs, ",") + "|" + strings.Join(diagnostic.Affected.TaskIDs, ",") + "|" + strings.Join(diagnostic.Affected.Agents, ",") + "|" + strings.Join(diagnostic.Affected.Paths, ",")
}

func diagnosticOccurrenceID(baseID string, occurrence int) string {
	return baseID + "#" + strconv.Itoa(occurrence)
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
	return tea.Tick(refreshInterval, func(now time.Time) tea.Msg { return tickMsg(now) })
}

// Retained for focused deterministic presentation tests.
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
func truncate(value string, length int) string { return truncatePlain(value, length) }
