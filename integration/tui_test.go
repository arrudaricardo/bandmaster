package integration_test

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

type synchronizedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *synchronizedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(value)
}
func (b *synchronizedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

type compiledDashboard struct {
	t        *testing.T
	command  *exec.Cmd
	terminal *os.File
	output   *synchronizedBuffer
	done     chan error
}

func startCompiledDashboard(t *testing.T, repo string, width, height uint16, args ...string) *compiledDashboard {
	return startCompiledDashboardWithEnvironment(t, repo, width, height, []string{"TERM=xterm-256color", "NO_COLOR=1"}, args...)
}

func startCompiledDashboardWithEnvironment(t *testing.T, repo string, width, height uint16, environment []string, args ...string) *compiledDashboard {
	t.Helper()
	command := exec.Command(bandmasterBinary, args...)
	command.Dir = repo
	command.Env = environmentWithOverrides(os.Environ(), environment)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Cols: width, Rows: height})
	if err != nil {
		t.Fatalf("start compiled dashboard: %v", err)
	}
	output := &synchronizedBuffer{}
	done := make(chan error, 1)
	go func() { _, _ = io.Copy(output, terminal); done <- command.Wait() }()
	dashboard := &compiledDashboard{t: t, command: command, terminal: terminal, output: output, done: done}
	t.Cleanup(func() { dashboard.close() })
	return dashboard
}

func (d *compiledDashboard) send(keys string) {
	d.t.Helper()
	if _, err := io.WriteString(d.terminal, keys); err != nil {
		d.t.Fatalf("send dashboard keys %q: %v", keys, err)
	}
}

func (d *compiledDashboard) resize(width, height uint16) {
	d.t.Helper()
	if err := pty.Setsize(d.terminal, &pty.Winsize{Cols: width, Rows: height}); err != nil {
		d.t.Fatalf("resize dashboard: %v", err)
	}
}

func (d *compiledDashboard) waitFor(text string) string {
	d.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		visible := visibleTerminalText(d.output.String())
		if strings.Contains(visible, text) {
			return visible
		}
		select {
		case err := <-d.done:
			d.t.Fatalf("dashboard exited before %q: %v\n%s", text, err, visible)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.t.Fatalf("dashboard did not render %q:\n%s", text, visibleTerminalText(d.output.String()))
	return ""
}

func (d *compiledDashboard) waitForAdditional(text string, previous int) string {
	d.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		visible := visibleTerminalText(d.output.String())
		if strings.Count(visible, text) > previous {
			return visible
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.t.Fatalf("dashboard did not render an additional %q:\n%s", text, visibleTerminalText(d.output.String()))
	return ""
}

func (d *compiledDashboard) quit() {
	d.t.Helper()
	d.send("q")
	select {
	case err := <-d.done:
		if err != nil {
			d.t.Fatalf("dashboard quit: %v\n%s", err, visibleTerminalText(d.output.String()))
		}
	case <-time.After(2 * time.Second):
		// A resize signal and terminal input can arrive in either order. Retry the
		// idempotent quit key once after Bubble Tea has processed the resize.
		d.send("q")
		select {
		case err := <-d.done:
			if err != nil {
				d.t.Fatalf("dashboard quit after resize: %v\n%s", err, visibleTerminalText(d.output.String()))
			}
		case <-time.After(3 * time.Second):
			d.t.Fatal("dashboard did not quit cleanly")
		}
	}
	_ = d.terminal.Close()
	d.terminal = nil
}

func (d *compiledDashboard) close() {
	if d.terminal == nil {
		return
	}
	_ = d.command.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.done:
	case <-time.After(time.Second):
		_ = d.command.Process.Kill()
	}
	_ = d.terminal.Close()
	d.terminal = nil
}

var ansiSequence = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\a]*(?:\a|\x1b\\)|[()][A-Z0-9])`)
var ansiStyleSequence = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func visibleTerminalText(output string) string {
	return strings.ReplaceAll(ansiSequence.ReplaceAllString(output, ""), "\r", "")
}

func initializeApprovedRepository(t *testing.T) string {
	t.Helper()
	repo := newGitRepository(t)
	commitRepository(t, repo)
	initialized := runBandmaster(t, repo, "init", "--json")
	if initialized.exitCode != 0 {
		t.Fatalf("initialize: %s", initialized.stderr)
	}
	var response struct {
		Result struct {
			ValidationDigest string `json:"validation_digest"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(initialized.stdout), &response); err != nil {
		t.Fatalf("decode init: %v", err)
	}
	approved := runBandmaster(t, repo, "config", "approve", response.Result.ValidationDigest, "--json")
	if approved.exitCode != 0 {
		t.Fatalf("approve: %s", approved.stderr)
	}
	return repo
}

func TestCompiledDashboardControlsBothEntryPoints(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pseudo-terminals use Unix PTY semantics")
	}
	for _, entry := range [][]string{{"debug", "--watch"}, {"tui"}} {
		t.Run(strings.Join(entry, "_"), func(t *testing.T) {
			repo := initializeApprovedRepository(t)
			dashboard := startCompiledDashboard(t, repo, 100, 28, entry...)
			visible := dashboard.waitFor("live · updated 0s ago · q quit · r refresh")
			if ansiStyleSequence.MatchString(dashboard.output.String()) {
				t.Fatal("NO_COLOR dashboard emitted ANSI styling")
			}
			liveFrames := strings.Count(visible, "live · updated 0s ago · q quit · r refresh")
			dashboard.send("r")
			visible = dashboard.waitForAdditional("live · updated 0s ago · q quit · r refresh", liveFrames)
			if strings.Contains(visible, "refreshing") {
				t.Fatalf("dashboard exposed transient refresh state:\n%s", visible)
			}
			dashboard.resize(40, 10)
			dashboard.waitFor("Terminal too small")
			dashboard.resize(100, 28)
			dashboard.waitFor("Ready to coordinate")
			dashboard.quit()
		})
	}
}

func TestCompiledDashboardObservationIsReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pseudo-terminals use Unix PTY semantics")
	}
	repo := initializeApprovedRepository(t)
	before := captureRepositoryEvidence(t, repo)
	dashboard := startCompiledDashboard(t, repo, 100, 28, "tui")
	dashboard.waitFor("Ready to coordinate")
	dashboard.send("r")
	dashboard.resize(72, 20)
	dashboard.waitFor("Ready to coordinate")
	dashboard.quit()
	after := captureRepositoryEvidence(t, repo)
	if before != after {
		t.Fatalf("dashboard observation changed repository, Git, or orchestration evidence\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestCompiledDashboardNavigatesTasksTabsDetailsFilteringAndHelp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pseudo-terminals use Unix PTY semantics")
	}
	repo := approvedCleanRepository(t)
	if started := runBandmaster(t, repo, "session", "start", "--json"); started.exitCode != 0 {
		t.Fatalf("start Session: stdout=%s stderr=%s", started.stdout, started.stderr)
	}
	api := successfulTaskCommand(t, repo, "create", "--title", "Build API", "--intent", "Implement endpoint", "--expected-outcome", "Endpoint works")
	successfulTaskCommand(t, repo, "create", "--title", "Write docs", "--intent", "Document endpoint", "--expected-outcome", "Docs explain API", "--prerequisite", api.Result.ID)
	if assigned := runBandmaster(t, repo, "task", "assign", api.Result.ID, "--agent", "agent-tui", "--json"); assigned.exitCode != 0 {
		t.Fatalf("assign Agent: stdout=%s stderr=%s", assigned.stdout, assigned.stderr)
	}

	dashboard := startCompiledDashboard(t, repo, 120, 30, "debug", "--watch")
	visible := dashboard.waitFor("Exact state: assigned")
	detailFrames := strings.Count(visible, "Task Build API")
	successfulTaskCommand(t, repo, "create", "--title", "Auto refreshed Task", "--intent", "Appear without manual refresh", "--expected-outcome", "Selection remains stable")
	dashboard.waitFor("Auto refreshed Task")
	dashboard.resize(90, 24)
	dashboard.send("\r")
	dashboard.waitForAdditional("Task Build API", detailFrames)
	dashboard.send("j\r")
	dashboard.waitFor("Exact state: planned")
	assignedDetails := strings.Count(visibleTerminalText(dashboard.output.String()), "Exact state: assigned")
	dashboard.send("[")
	dashboard.waitForAdditional("Exact state: assigned", assignedDetails)
	dashboard.send("\x1b")
	time.Sleep(200 * time.Millisecond)
	dashboard.send("\x1b[C")
	dashboard.waitFor("[Agents]")
	dashboard.send("/")
	dashboard.waitFor("Filter /_")
	dashboard.send("agent-tui")
	dashboard.send("\r")
	dashboard.waitFor("1 match(es)")
	dashboard.send("?")
	dashboard.waitFor("The dashboard is strictly read-only")
	dashboard.send("?")
	dashboard.send("\x1b")
	time.Sleep(200 * time.Millisecond)
	dashboard.send("\x1b[C")
	dashboard.waitFor("[Batches]")
	dashboard.send("\x1b[C")
	dashboard.waitFor("[Diagnostics]")
	dashboard.send("\x1b[D")
	dashboard.send("\x1b[D")
	dashboard.waitFor("[Agents]")
	dashboard.resize(60, 18)
	dashboard.waitFor("agent-tui")
	dashboard.quit()
}

func TestCompiledDashboardGuidesApprovalAndSupportsColor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pseudo-terminals use Unix PTY semantics")
	}
	repo := newGitRepository(t)
	if initialized := runBandmaster(t, repo, "init", "--json"); initialized.exitCode != 0 {
		t.Fatalf("init: %s", initialized.stderr)
	}
	dashboard := startCompiledDashboardWithEnvironment(t, repo, 90, 24, []string{"TERM=xterm-256color", "NO_COLOR="}, "tui")
	dashboard.waitFor("Configuration approval required")
	dashboard.waitFor("bandmaster config approve <digest> --json")
	if !ansiStyleSequence.MatchString(dashboard.output.String()) {
		t.Fatal("color-enabled dashboard emitted no ANSI styling")
	}
	dashboard.quit()
}

func TestCompiledDashboardGuidesLifecycleStates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pseudo-terminals use Unix PTY semantics")
	}
	t.Run("uninitialized", func(t *testing.T) {
		repo := newGitRepository(t)
		dashboard := startCompiledDashboard(t, repo, 90, 24, "tui")
		dashboard.waitFor("Initialize Bandmaster")
		dashboard.waitFor("bandmaster init")
		dashboard.quit()
	})
	t.Run("paused", func(t *testing.T) {
		repo := approvedCleanRepository(t)
		successfulSessionCommand(t, repo, "start")
		successfulSessionCommand(t, repo, "pause")
		dashboard := startCompiledDashboard(t, repo, 90, 24, "tui")
		dashboard.waitFor("Session paused")
		dashboard.quit()
	})
	t.Run("completed", func(t *testing.T) {
		repo := approvedCleanRepository(t)
		successfulSessionCommand(t, repo, "start")
		successfulSessionCommand(t, repo, "finish")
		dashboard := startCompiledDashboard(t, repo, 90, 24, "tui")
		dashboard.waitFor("Session completed")
		dashboard.quit()
	})
	t.Run("quarantined", func(t *testing.T) {
		repo := repositoryWithValidation(t, "\n  commands:\n    - name: mutating-check\n      script: |\n        printf 'validation mutation\\n' > owned.txt\n      timeout: 2s\n")
		frozenValidationBatch(t, repo, "")
		assertBatchError(t, runBandmaster(t, repo, "batch", "validate", "--json"), 4, "submitted_path_drift", false)
		dashboard := startCompiledDashboard(t, repo, 90, 24, "tui")
		dashboard.waitFor("Session quarantined")
		dashboard.waitFor("bandmaster debug --json")
		dashboard.quit()
	})
}

func TestCompiledDashboardPreservesSelectionAcrossUrgencyAndDisappearance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pseudo-terminals use Unix PTY semantics")
	}
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	urgent := successfulTaskCommand(t, repo, "create", "--title", "Becomes urgent", "--intent", "Exercise live urgency", "--expected-outcome", "Marked without stealing focus")
	steady := successfulTaskCommand(t, repo, "create", "--title", "Steady selection", "--intent", "Remain selected", "--expected-outcome", "Selection survives refresh")
	dashboard := startCompiledDashboard(t, repo, 100, 28, "tui")
	dashboard.waitFor("Steady selection")
	dashboard.send("j")
	mutateDashboardState(t, repo, `UPDATE tasks SET status = 'blocked' WHERE id = ?`, urgent.Result.ID)
	dashboard.waitFor("NEW")
	dashboard.send("\r")
	dashboard.waitFor("Task Steady selection")
	dashboard.send("\x1b")
	mutateDashboardState(t, repo, `DELETE FROM task_audit_events WHERE task_id = ?`, steady.Result.ID)
	mutateDashboardState(t, repo, `DELETE FROM tasks WHERE id = ?`, steady.Result.ID)
	dashboard.waitFor("The selected item disappeared")
	dashboard.quit()
}

func TestCompiledDashboardShowsBatchDetailsAndFiltering(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pseudo-terminals use Unix PTY semantics")
	}
	repo := repositoryWithValidation(t, "")
	_, batchID, _ := frozenValidationBatch(t, repo, "")
	dashboard := startCompiledDashboard(t, repo, 100, 28, "tui")
	dashboard.waitFor("Ready for batch")
	dashboard.send("l")
	dashboard.waitFor("[Agents]")
	dashboard.send("l")
	dashboard.waitFor("[Batches]")
	dashboard.send("/")
	dashboard.waitFor("Filter /_")
	dashboard.send(batchID)
	dashboard.waitFor("Filter /" + batchID)
	dashboard.send("\r")
	dashboard.waitFor("1 match(es)")
	dashboard.send("\r")
	dashboard.waitFor("Ordered Batch Tasks:")
	dashboard.quit()
}

func mutateDashboardState(t *testing.T, repo, statement string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(repo, ".git", "bandmaster", "state.db"))
	if err != nil {
		t.Fatalf("open dashboard fixture state: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(statement, args...); err != nil {
		t.Fatalf("mutate dashboard fixture state: %v", err)
	}
}

func captureRepositoryEvidence(t *testing.T, repo string) string {
	t.Helper()
	var evidence strings.Builder
	evidence.WriteString(runGit(t, repo, "status", "--porcelain=v1", "--untracked-files=all"))
	for _, relative := range []string{".bandmaster.yaml", filepath.Join(".git", "bandmaster", "state.db")} {
		content, err := os.ReadFile(filepath.Join(repo, relative))
		if err != nil {
			t.Fatalf("read evidence %s: %v", relative, err)
		}
		digest := sha256.Sum256(content)
		fmt.Fprintf(&evidence, "%s:%x\n", filepath.ToSlash(relative), digest)
	}
	return evidence.String()
}
