package integration_test

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

type debugLease struct {
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at"`
}

type debugTask struct {
	ID             string      `json:"id"`
	Status         string      `json:"status"`
	WorkerIdentity string      `json:"worker_identity"`
	Lease          *debugLease `json:"lease"`
}

type debugAffected struct {
	TaskIDs []string `json:"task_ids"`
	Workers []string `json:"worker_identities"`
	Paths   []string `json:"paths"`
}

type debugDiagnostic struct {
	Code             string        `json:"code"`
	Severity         string        `json:"severity"`
	SuggestedActions []string      `json:"suggested_actions"`
	Affected         debugAffected `json:"affected"`
}

type debugResponse struct {
	Success bool `json:"success"`
	Result  struct {
		Collection struct {
			Status     string `json:"status"`
			Stable     bool   `json:"stable"`
			BestEffort bool   `json:"best_effort"`
		} `json:"collection"`
		Repository struct {
			ChangedPaths []string `json:"changed_paths"`
			IndexChanged bool     `json:"index_changed"`
		} `json:"repository"`
		Tasks       []debugTask       `json:"tasks"`
		Diagnostics []debugDiagnostic `json:"diagnostics"`
		Revision    struct {
			DatabaseBefore int64  `json:"database_before"`
			DatabaseAfter  int64  `json:"database_after"`
			GitBefore      string `json:"git_before"`
			GitAfter       string `json:"git_after"`
		} `json:"revision"`
	} `json:"result"`
}

func runDebugJSON(t *testing.T, repo string, args ...string) debugResponse {
	t.Helper()
	commandArgs := []string{"debug"}
	commandArgs = append(commandArgs, args...)
	commandArgs = append(commandArgs, "--json")
	result := runBandmaster(t, repo, commandArgs...)
	return decodeDebugJSON(t, result)
}

func decodeDebugJSON(t *testing.T, result commandResult) debugResponse {
	t.Helper()
	if result.exitCode != 0 {
		t.Fatalf("debug exit=%d stdout=%s stderr=%s", result.exitCode, result.stdout, result.stderr)
	}
	var response debugResponse
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("decode debug response: %v\n%s", err, result.stdout)
	}
	if !response.Success {
		t.Fatalf("debug response was unsuccessful: %+v", response)
	}
	return response
}

func TestDebugRetriesTransientGitMutationAndReportsPersistentMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test Git wrapper requires a POSIX shell")
	}
	for _, test := range []struct {
		name           string
		persistent     bool
		wantStable     bool
		wantBestEffort bool
	}{
		{name: "transient", wantStable: true},
		{name: "persistent", persistent: true, wantBestEffort: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := approvedCleanRepository(t)
			wrapperDir := t.TempDir()
			counterPath := filepath.Join(wrapperDir, "status-count")
			markerPath := filepath.Join(wrapperDir, "mutation-recorded")
			mutationPath := filepath.Join(repo, "changing.txt")
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			wrapper := `#!/bin/sh
"$BANDMASTER_REAL_GIT" "$@"
result=$?
case " $* " in
  *" status "*)
    count=0
    if [ -f "$BANDMASTER_GIT_COUNT" ]; then count=$(sed -n '1p' "$BANDMASTER_GIT_COUNT"); fi
    count=$((count + 1))
    printf '%s\n' "$count" > "$BANDMASTER_GIT_COUNT"
    if [ "$BANDMASTER_MUTATION_MODE" = persistent ] || { [ "$count" -eq 2 ] && [ ! -f "$BANDMASTER_MUTATION_MARKER" ]; }; then
      printf '%s\n' "$count" > "$BANDMASTER_MUTATION_PATH"
      : > "$BANDMASTER_MUTATION_MARKER"
    fi
    ;;
esac
exit "$result"
`
			wrapperPath := filepath.Join(wrapperDir, "git")
			writeFile(t, wrapperPath, wrapper)
			if err := os.Chmod(wrapperPath, 0o755); err != nil {
				t.Fatal(err)
			}
			mode := "transient"
			if test.persistent {
				mode = "persistent"
			}
			result := runBandmasterWithEnvironment(t, repo, []string{
				"PATH=" + wrapperDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"BANDMASTER_REAL_GIT=" + realGit,
				"BANDMASTER_GIT_COUNT=" + counterPath,
				"BANDMASTER_MUTATION_MARKER=" + markerPath,
				"BANDMASTER_MUTATION_MODE=" + mode,
				"BANDMASTER_MUTATION_PATH=" + mutationPath,
			}, "debug", "--json")
			response := decodeDebugJSON(t, result)
			if response.Result.Collection.Stable != test.wantStable || response.Result.Collection.BestEffort != test.wantBestEffort {
				t.Fatalf("collection = %+v revision = %+v", response.Result.Collection, response.Result.Revision)
			}
			count, err := strconv.Atoi(strings.TrimSpace(readFile(t, counterPath)))
			if err != nil || count < 6 {
				t.Fatalf("debug did not exercise bounded retry: status count=%d err=%v", count, err)
			}
			if test.wantStable && (response.Result.Revision.GitBefore != response.Result.Revision.GitAfter) {
				t.Fatalf("stable retry retained unequal Git boundaries: %+v", response.Result.Revision)
			}
			if test.wantBestEffort && (response.Result.Revision.GitBefore == response.Result.Revision.GitAfter) {
				t.Fatalf("persistent mutation was not reflected in Git boundaries: %+v", response.Result.Revision)
			}
		})
	}
}

func TestDebugRetriesTransientPublicDatabaseMutation(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	running := newBandmasterCommand(repo, "debug", "--json")
	running.command.Env = environmentWithOverrides(os.Environ(), []string{"BANDMASTER_TEST_DEBUG_DATABASE_READ_DELAY=1500ms"})
	startedAt := time.Now()
	if err := running.command.Start(); err != nil {
		t.Fatal(err)
	}
	// Wait until the compiled process has its read-only state connection open;
	// by the time lsof returns, the first coherent read has entered its delay.
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		running.command.Process.Kill()
		t.Skip("lsof is required to synchronize the compiled debug process")
	}
	statePath := filepath.Join(repo, ".git", "bandmaster", "state.db")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := exec.Command(lsof, "-a", "-p", strconv.Itoa(running.command.Process.Pid), "--", statePath).Run(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("debug did not open its read-only state connection")
		}
		time.Sleep(10 * time.Millisecond)
	}
	successfulTaskCommand(t, repo, "create",
		"--title", "Concurrent public mutation",
		"--intent", "Exercise debug retry",
		"--expected-outcome", "The second observation is stable",
	)
	successfulSessionCommand(t, repo, "pause")
	result := waitBandmasterCommand(t, running)
	response := decodeDebugJSON(t, result)
	if !response.Result.Collection.Stable || response.Result.Collection.BestEffort {
		t.Fatalf("transient public mutation did not yield a stable retry: collection=%+v revision=%+v", response.Result.Collection, response.Result.Revision)
	}
	if elapsed := time.Since(startedAt); elapsed < 2700*time.Millisecond {
		t.Fatalf("debug returned before the bounded retry: elapsed=%s collection=%+v revision=%+v", elapsed, response.Result.Collection, response.Result.Revision)
	}
}

func TestDebugIdleSnapshotsAreStable(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	successfulSessionCommand(t, repo, "finish")

	for attempt := 0; attempt < 2; attempt++ {
		response := runDebugJSON(t, repo)
		if response.Result.Collection.Status != "complete" || !response.Result.Collection.Stable || response.Result.Collection.BestEffort {
			t.Fatalf("idle snapshot %d collection = %+v revision = %+v", attempt+1, response.Result.Collection, response.Result.Revision)
		}
		if response.Result.Revision.DatabaseBefore != response.Result.Revision.DatabaseAfter || response.Result.Revision.GitBefore != response.Result.Revision.GitAfter {
			t.Fatalf("idle snapshot %d revision boundaries differ: %+v", attempt+1, response.Result.Revision)
		}
	}
	human := runBandmaster(t, repo, "debug")
	if human.exitCode != 0 || strings.Contains(human.stdout, "best effort") {
		t.Fatalf("idle human output reported degraded collection: exit=%d stdout=%q stderr=%q", human.exitCode, human.stdout, human.stderr)
	}
	watch := startDebugWatch(t, repo)
	initial := watch.read(t, 5*time.Second)
	snapshot, _ := initial["snapshot"].(map[string]any)
	collection, _ := snapshot["collection"].(map[string]any)
	if initial["type"] != "snapshot" || collection["stable"] != true || collection["best_effort"] != false {
		t.Fatalf("idle watch snapshot did not preserve corrected collection state: %#v", initial)
	}
	heartbeat := watch.read(t, 12*time.Second)
	heartbeatCollection, _ := heartbeat["collection"].(map[string]any)
	if heartbeat["type"] != "heartbeat" || heartbeatCollection["stable"] != true || heartbeatCollection["best_effort"] != false {
		t.Fatalf("idle watch heartbeat did not preserve corrected collection state: %#v", heartbeat)
	}
	watch.stop(t)
}

func TestDebugCorrelatesClaimsWithFilesInsideNewDirectories(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	created := successfulTaskCommand(t, repo, "create",
		"--title", "Create generated files",
		"--intent", "Exercise exact ownership diagnostics",
		"--expected-outcome", "Only unclaimed files are reported",
	)
	assigned := successfulTaskCommand(t, repo, "assign", created.Result.ID, "--worker", "worker-generated")
	successfulTaskCommand(t, repo, "claim", created.Result.ID,
		"--token", assigned.Result.AssignmentToken,
		"--path", "generated/one.txt",
		"--path", "generated/two.txt",
	)
	writeFile(t, filepath.Join(repo, "generated", "one.txt"), "one\n")
	writeFile(t, filepath.Join(repo, "generated", "two.txt"), "two\n")

	owned := runDebugJSON(t, repo)
	if got, want := strings.Join(owned.Result.Repository.ChangedPaths, ","), "generated/one.txt,generated/two.txt"; got != want {
		t.Fatalf("changed paths = %q, want %q", got, want)
	}
	if diagnostics := debugDiagnosticsByCode(owned, "unowned_worktree_drift"); len(diagnostics) != 0 {
		t.Fatalf("fully claimed directory reported unowned drift: %+v", diagnostics)
	}

	writeFile(t, filepath.Join(repo, "generated", "unclaimed.txt"), "unclaimed\n")
	mixed := runDebugJSON(t, repo)
	diagnostics := debugDiagnosticsByCode(mixed, "unowned_worktree_drift")
	if len(diagnostics) != 1 || strings.Join(diagnostics[0].Affected.Paths, ",") != "generated/unclaimed.txt" {
		t.Fatalf("mixed directory diagnostics = %+v", diagnostics)
	}
}

func TestDebugPreservesTrackedGitStatusPaths(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "modified.txt"), "before\n")
	writeFile(t, filepath.Join(repo, "deleted.txt"), "delete me\n")
	writeFile(t, filepath.Join(repo, "rename-source.txt"), "rename me\n")
	runGit(t, repo, "add", "modified.txt", "deleted.txt", "rename-source.txt")
	runGit(t, repo, "commit", "-m", "Add debug Git fixtures")

	writeFile(t, filepath.Join(repo, "modified.txt"), "after\n")
	if err := os.Remove(filepath.Join(repo, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "mv", "rename-source.txt", "rename-destination.txt")
	writeFile(t, filepath.Join(repo, "untracked", "new.txt"), "new\n")

	response := runDebugJSON(t, repo)
	paths := append([]string(nil), response.Result.Repository.ChangedPaths...)
	sort.Strings(paths)
	if got, want := strings.Join(paths, ","), "deleted.txt,modified.txt,rename-destination.txt,untracked/new.txt"; got != want {
		t.Fatalf("tracked/untracked changed paths = %q, want %q", got, want)
	}
	if !response.Result.Repository.IndexChanged {
		t.Fatal("staged rename did not set index_changed")
	}
}

func TestDebugLeaseDiagnosticsOnlyDescribeLiveWorkerOwnership(t *testing.T) {
	repo := approvedCleanRepositoryWithLeaseDuration(t, "2s")
	successfulSessionCommand(t, repo, "start")
	live := successfulTaskCommand(t, repo, "create",
		"--title", "Live leased work",
		"--intent", "Keep active ownership actionable",
		"--expected-outcome", "Lease timing is diagnosed",
	)
	live = successfulTaskCommand(t, repo, "assign", live.Result.ID, "--worker", "worker-live")
	terminal := successfulTaskCommand(t, repo, "create",
		"--title", "Canceled leased work",
		"--intent", "Preserve historical lease evidence",
		"--expected-outcome", "Terminal history is not actionable",
	)
	terminal = successfulTaskCommand(t, repo, "assign", terminal.Result.ID, "--worker", "worker-terminal")
	terminal = successfulTaskCommand(t, repo, "cancel", terminal.Result.ID,
		"--terminated-worker", "worker-terminal",
		"--termination-proof", "codex-handle-worker-terminal-stopped",
	)

	liveExpiry, err := time.Parse(time.RFC3339Nano, live.Result.Lease.ExpiresAt)
	if err != nil {
		t.Fatalf("parse live lease expiry: %v", err)
	}
	if wait := time.Until(liveExpiry.Add(-350 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	expiring := runDebugJSON(t, repo)
	expiringDiagnostics := debugDiagnosticsByCode(expiring, "lease_expiring")
	if len(expiringDiagnostics) != 1 || strings.Join(expiringDiagnostics[0].Affected.TaskIDs, ",") != live.Result.ID || strings.Join(expiringDiagnostics[0].Affected.Workers, ",") != "worker-live" {
		t.Fatalf("expiring diagnostics = %+v", expiringDiagnostics)
	}
	if len(expiringDiagnostics[0].SuggestedActions) == 0 || !strings.Contains(expiringDiagnostics[0].SuggestedActions[0], "task heartbeat "+live.Result.ID) {
		t.Fatalf("expiring suggestion is unusable: %+v", expiringDiagnostics[0].SuggestedActions)
	}

	if wait := time.Until(liveExpiry.Add(150 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	expired := runDebugJSON(t, repo)
	expiredDiagnostics := debugDiagnosticsByCode(expired, "lease_expired")
	if len(expiredDiagnostics) != 1 || strings.Join(expiredDiagnostics[0].Affected.TaskIDs, ",") != live.Result.ID || strings.Join(expiredDiagnostics[0].Affected.Workers, ",") != "worker-live" || expiredDiagnostics[0].Severity != "error" {
		t.Fatalf("expired diagnostics = %+v", expiredDiagnostics)
	}
	if len(expiredDiagnostics[0].SuggestedActions) == 0 || !strings.Contains(expiredDiagnostics[0].SuggestedActions[0], "--terminated-worker worker-live") {
		t.Fatalf("expired recovery suggestion is unusable: %+v", expiredDiagnostics[0].SuggestedActions)
	}
	terminalSnapshot := debugTaskByID(t, expired, terminal.Result.ID)
	if terminalSnapshot.Status != "canceled" || terminalSnapshot.Lease == nil || terminalSnapshot.Lease.ExpiresAt == "" {
		t.Fatalf("terminal lease history was not preserved: %+v", terminalSnapshot)
	}
	for _, diagnostic := range expired.Result.Diagnostics {
		for _, action := range diagnostic.SuggestedActions {
			if strings.Contains(action, "--terminated-worker  ") {
				t.Fatalf("suggested action contains an empty worker identity: %q", action)
			}
		}
	}
}

func TestDebugCompiledCLIKeepsTerminalHistoricalLeasesNonActionable(t *testing.T) {
	repo := approvedCleanRepository(t)
	writeFile(t, filepath.Join(repo, "changed-terminal.txt"), "before\n")
	writeFile(t, filepath.Join(repo, "unchanged-terminal.txt"), "stable\n")
	runGit(t, repo, "add", "changed-terminal.txt", "unchanged-terminal.txt")
	runGit(t, repo, "commit", "-m", "Add terminal lease fixtures")
	successfulSessionCommand(t, repo, "start")
	changed := successfulTaskCommand(t, repo, "create", "--title", "Committed terminal task", "--intent", "Create a committed task", "--expected-outcome", "Historical lease stays evidence-only")
	noOp := successfulTaskCommand(t, repo, "create", "--title", "No-op terminal task", "--intent", "Create a no-op task", "--expected-outcome", "Historical lease stays evidence-only")
	changedAssignment := successfulTaskCommand(t, repo, "assign", changed.Result.ID, "--worker", "worker-committed")
	noOpAssignment := successfulTaskCommand(t, repo, "assign", noOp.Result.ID, "--worker", "worker-no-op")
	successfulTaskCommand(t, repo, "claim", changed.Result.ID, "--token", changedAssignment.Result.AssignmentToken, "--path", "changed-terminal.txt")
	successfulTaskCommand(t, repo, "claim", noOp.Result.ID, "--token", noOpAssignment.Result.AssignmentToken, "--path", "unchanged-terminal.txt")
	writeFile(t, filepath.Join(repo, "changed-terminal.txt"), "after\n")
	submitBatchTask(t, repo, changed.Result.ID, changedAssignment.Result.AssignmentToken)
	submitBatchTask(t, repo, noOp.Result.ID, noOpAssignment.Result.AssignmentToken)
	successfulBatchCommand(t, repo, "freeze")
	successfulBatchCommand(t, repo, "validate")
	successfulBatchCommand(t, repo, "commit")
	if status := successfulTaskCommand(t, repo, "inspect", changed.Result.ID).Result.Status; status != "committed" {
		t.Fatalf("changed task status = %q, want committed", status)
	}
	if status := successfulTaskCommand(t, repo, "inspect", noOp.Result.ID).Result.Status; status != "no_op" {
		t.Fatalf("unchanged task status = %q, want no_op", status)
	}

	// Supported current workflows release terminal leases. Recreate only the
	// legacy historical-lease condition; all assertions stay at the freshly
	// compiled CLI's public JSON boundary.
	state, err := sql.Open("sqlite3", filepath.Join(repo, ".git", "bandmaster", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := "2000-01-01T00:00:00Z"
	if _, err := state.Exec(`UPDATE task_leases SET status = 'active', renewed_at = ?, expires_at = ? WHERE task_id IN (?, ?)`, expiresAt, expiresAt, changed.Result.ID, noOp.Result.ID); err != nil {
		state.Close()
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}

	response := runDebugJSON(t, repo)
	for _, id := range []string{changed.Result.ID, noOp.Result.ID} {
		task := debugTaskByID(t, response, id)
		if task.Lease == nil || task.Lease.Status != "active" || task.Lease.ExpiresAt != expiresAt {
			t.Fatalf("terminal task %s lost historical lease evidence: %+v", id, task)
		}
		for _, code := range []string{"lease_expired", "lease_expiring"} {
			for _, diagnostic := range debugDiagnosticsByCode(response, code) {
				if strings.Join(diagnostic.Affected.TaskIDs, ",") == id {
					t.Fatalf("terminal task %s received %s: %+v", id, code, diagnostic)
				}
			}
		}
	}
}

func debugTaskByID(t *testing.T, response debugResponse, id string) debugTask {
	t.Helper()
	for _, task := range response.Result.Tasks {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("debug snapshot omitted task %s", id)
	return debugTask{}
}

func debugDiagnosticsByCode(response debugResponse, code string) []debugDiagnostic {
	var diagnostics []debugDiagnostic
	for _, diagnostic := range response.Result.Diagnostics {
		if diagnostic.Code == code {
			diagnostics = append(diagnostics, diagnostic)
		}
	}
	return diagnostics
}

func TestDebugUninitializedRepositoryIsUsefulAndReadOnly(t *testing.T) {
	repo := newGitRepository(t)
	before, err := os.ReadDir(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}

	result := runBandmaster(t, repo, "debug", "--json")
	if result.exitCode != 0 {
		t.Fatalf("debug exit=%d stdout=%s stderr=%s", result.exitCode, result.stdout, result.stderr)
	}
	var response struct {
		SchemaVersion string `json:"schema_version"`
		Command       string `json:"command"`
		Success       bool   `json:"success"`
		Result        struct {
			ContractVersion string `json:"contract_version"`
			Collection      struct {
				Status string `json:"status"`
			} `json:"collection"`
			Runtime struct {
				BandmasterVersion string `json:"bandmaster_version"`
				Executable        string `json:"executable"`
				GoVersion         string `json:"go_version"`
			} `json:"runtime"`
			State struct {
				Initialization string `json:"initialization"`
			} `json:"state"`
			Repository struct {
				Root string `json:"root"`
			} `json:"repository"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("decode debug response: %v\n%s", err, result.stdout)
	}
	if response.SchemaVersion != "1" || response.Command != "debug" || !response.Success || response.Result.ContractVersion != "1" {
		t.Fatalf("unexpected response: %+v", response)
	}
	if response.Result.Collection.Status != "complete" || response.Result.State.Initialization != "uninitialized" {
		t.Fatalf("unexpected collection state: %+v", response.Result)
	}
	resolvedRepo, _ := filepath.EvalSymlinks(repo)
	if response.Result.Runtime.BandmasterVersion == "" || response.Result.Runtime.Executable == "" || response.Result.Runtime.GoVersion == "" || response.Result.Repository.Root != resolvedRepo {
		t.Fatalf("missing runtime/repository evidence: %+v", response.Result)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "bandmaster")); !os.IsNotExist(err) {
		t.Fatalf("debug created Bandmaster state: %v", err)
	}
	after, err := os.ReadDir(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("debug changed Git metadata entries: before=%d after=%d", len(before), len(after))
	}

	human := runBandmaster(t, repo, "debug")
	if human.exitCode != 0 || !strings.Contains(human.stdout, "uninitialized") || !strings.Contains(human.stdout, "Bandmaster debug snapshot") {
		t.Fatalf("unexpected human snapshot: exit=%d stdout=%q stderr=%q", human.exitCode, human.stdout, human.stderr)
	}
}

func TestDebugJSONWatchEmitsSemanticChangesAndStopsCleanly(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	command := exec.Command(bandmasterBinary, "debug", "--watch", "--json", "--interval", "250ms")
	command.Dir = repo
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	})
	lines := make(chan string, 16)
	scanErrors := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		scanErrors <- scanner.Err()
	}()
	readRecord := func(timeout time.Duration) map[string]any {
		t.Helper()
		select {
		case line := <-lines:
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("invalid NDJSON record: %v\n%s", err, line)
			}
			return record
		case err := <-scanErrors:
			t.Fatalf("watch ended early: %v", err)
		case <-time.After(timeout):
			t.Fatal("timed out waiting for watch record")
		}
		return nil
	}
	initial := readRecord(5 * time.Second)
	if initial["type"] != "snapshot" || initial["sequence"] != float64(1) || initial["snapshot"] == nil {
		t.Fatalf("unexpected initial record: %#v", initial)
	}
	created := runBandmaster(t, repo, "task", "create", "--title", "Watch me", "--intent", "Emit a change", "--expected-outcome", "A semantic record", "--json")
	if created.exitCode != 0 {
		t.Fatalf("create task: %s %s", created.stdout, created.stderr)
	}
	deadline := time.Now().Add(5 * time.Second)
	found := false
	lastSequence := initial["sequence"].(float64)
	for time.Now().Before(deadline) {
		record := readRecord(time.Until(deadline))
		if sequence := record["sequence"].(float64); sequence <= lastSequence {
			t.Fatalf("non-monotonic stream sequence: previous=%v record=%#v", lastSequence, record)
		} else {
			lastSequence = sequence
		}
		if record["type"] == "change" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("watch did not emit a semantic change")
	}
	heartbeatDeadline := time.Now().Add(12 * time.Second)
	for {
		record := readRecord(time.Until(heartbeatDeadline))
		if sequence := record["sequence"].(float64); sequence <= lastSequence {
			t.Fatalf("non-monotonic heartbeat sequence: previous=%v record=%#v", lastSequence, record)
		} else {
			lastSequence = sequence
		}
		if record["type"] == "heartbeat" {
			if record["revision"] == nil || record["collection"] == nil {
				t.Fatalf("heartbeat lacks health evidence: %#v", record)
			}
			break
		}
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("watch did not stop successfully: %v", err)
	}
}

func TestDebugJSONWatchReportsTransientFailureAndRecovery(t *testing.T) {
	repo := approvedCleanRepository(t)
	successfulSessionCommand(t, repo, "start")
	successfulSessionCommand(t, repo, "pause")
	watch := startDebugWatch(t, repo)
	if record := watch.read(t, 5*time.Second); record["type"] != "snapshot" {
		t.Fatalf("initial record = %#v", record)
	}

	statePath := filepath.Join(repo, ".git", "bandmaster", "state.db")
	backupPath := filepath.Join(repo, ".git", "bandmaster", "state.debug-backup")
	if err := os.Rename(statePath, backupPath); err != nil {
		t.Fatal(err)
	}
	writeFile(t, statePath, "transient malformed state\n")
	for {
		record := watch.read(t, 5*time.Second)
		if record["type"] == "collection_error" {
			break
		}
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backupPath, statePath); err != nil {
		t.Fatal(err)
	}
	for {
		record := watch.read(t, 5*time.Second)
		if record["type"] == "recovered" {
			break
		}
	}
	watch.stop(t)
}

func TestDebugWatchPinsSessionUnlessFollowingLatest(t *testing.T) {
	repo := approvedCleanRepository(t)
	first := successfulSessionCommand(t, repo, "start")
	pinned := startDebugWatch(t, repo)
	_ = pinned.read(t, 5*time.Second)
	successfulSessionCommand(t, repo, "finish")
	second := successfulSessionCommand(t, repo, "start")
	pinDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(pinDeadline) {
		record, ok := pinned.tryRead(time.Until(pinDeadline))
		if !ok {
			break
		}
		if debugStreamSessionID(record) == second.Result.ID {
			t.Fatalf("pinned watch adopted new session: %#v", record)
		}
	}
	pinned.stop(t)

	following := startDebugWatch(t, repo, "--follow-latest")
	_ = following.read(t, 5*time.Second)
	successfulSessionCommand(t, repo, "finish")
	third := successfulSessionCommand(t, repo, "start")
	deadline := time.Now().Add(5 * time.Second)
	for {
		record := following.read(t, time.Until(deadline))
		if debugStreamSessionID(record) == third.Result.ID {
			break
		}
	}
	following.stop(t)
	if first.Result.ID == second.Result.ID || second.Result.ID == third.Result.ID {
		t.Fatal("test sessions were not distinct")
	}
}

type debugWatchProcess struct {
	command *exec.Cmd
	lines   chan string
	errors  chan error
}

func startDebugWatch(t *testing.T, repo string, extra ...string) *debugWatchProcess {
	t.Helper()
	args := []string{"debug", "--watch", "--json", "--interval", "250ms"}
	args = append(args, extra...)
	command := exec.Command(bandmasterBinary, args...)
	command.Dir = repo
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	watch := &debugWatchProcess{command: command, lines: make(chan string, 32), errors: make(chan error, 1)}
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			watch.lines <- scanner.Text()
		}
		watch.errors <- scanner.Err()
	}()
	t.Cleanup(func() {
		if watch.command.Process != nil {
			_ = watch.command.Process.Kill()
		}
	})
	return watch
}

func (watch *debugWatchProcess) read(t *testing.T, timeout time.Duration) map[string]any {
	t.Helper()
	select {
	case line := <-watch.lines:
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid NDJSON: %v\n%s", err, line)
		}
		return record
	case err := <-watch.errors:
		t.Fatalf("watch ended early: %v", err)
	case <-time.After(timeout):
		t.Fatal("timed out waiting for watch record")
	}
	return nil
}

func (watch *debugWatchProcess) tryRead(timeout time.Duration) (map[string]any, bool) {
	select {
	case line := <-watch.lines:
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			return nil, false
		}
		return record, true
	case <-time.After(timeout):
		return nil, false
	}
}

func (watch *debugWatchProcess) stop(t *testing.T) {
	t.Helper()
	if err := watch.command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := watch.command.Wait(); err != nil {
		t.Fatalf("watch stop: %v", err)
	}
}

func debugStreamSessionID(record map[string]any) string {
	change, _ := record["change"].(map[string]any)
	after, _ := change["after"].(map[string]any)
	id, _ := after["id"].(string)
	return id
}

func TestDebugRejectsInvalidArgumentsWithoutChangingState(t *testing.T) {
	repo := newGitRepository(t)
	result := runBandmaster(t, repo, "debug", "--session")
	if result.exitCode != 3 || !strings.Contains(result.stderr, "--session requires a value") {
		t.Fatalf("unexpected invalid argument result: exit=%d stdout=%q stderr=%q", result.exitCode, result.stdout, result.stderr)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "bandmaster")); !os.IsNotExist(err) {
		t.Fatalf("invalid debug invocation created state: %v", err)
	}
}

func TestDebugNormalizesRelationshipsAndRedactsAuthority(t *testing.T) {
	repo := approvedCleanRepository(t)
	session := successfulSessionCommand(t, repo, "start")
	created := runBandmaster(t, repo, "task", "create", "--title", "Sensitive work", "--intent", "Keep authority private", "--expected-outcome", "A safe snapshot", "--json")
	if created.exitCode != 0 {
		t.Fatalf("create task: %s %s", created.stdout, created.stderr)
	}
	var task taskResponse
	if err := json.Unmarshal([]byte(created.stdout), &task); err != nil {
		t.Fatal(err)
	}
	assigned := runBandmaster(t, repo, "task", "assign", task.Result.ID, "--worker", "worker-debug", "--json")
	if assigned.exitCode != 0 {
		t.Fatalf("assign task: %s %s", assigned.stdout, assigned.stderr)
	}
	if err := json.Unmarshal([]byte(assigned.stdout), &task); err != nil {
		t.Fatal(err)
	}
	token := task.Result.AssignmentToken

	result := runBandmaster(t, repo, "debug", "--session", session.Result.ID, "--json")
	if result.exitCode != 0 {
		t.Fatalf("debug exit=%d stdout=%s stderr=%s", result.exitCode, result.stdout, result.stderr)
	}
	if strings.Contains(result.stdout, token) {
		t.Fatal("debug exposed an assignment token")
	}
	var response struct {
		Result struct {
			Session *struct {
				ID         string `json:"id"`
				Historical bool   `json:"historical"`
			} `json:"session"`
			Tasks []struct {
				ID                         string `json:"id"`
				AssignmentTokenPresent     bool   `json:"assignment_token_present"`
				AssignmentTokenFingerprint string `json:"assignment_token_fingerprint"`
				AssignmentToken            string `json:"assignment_token"`
			} `json:"tasks"`
			Workers []struct {
				WorkerIdentity string `json:"worker_identity"`
				ActiveTaskID   string `json:"active_task_id"`
			} `json:"workers"`
			State struct {
				SchemaVersion string `json:"schema_version"`
			} `json:"state"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("decode: %v\n%s", err, result.stdout)
	}
	if response.Result.Session == nil || response.Result.Session.ID != session.Result.ID || response.Result.Session.Historical || response.Result.State.SchemaVersion == "" {
		t.Fatalf("bad session/state: %+v", response.Result)
	}
	if len(response.Result.Tasks) != 1 || !response.Result.Tasks[0].AssignmentTokenPresent || response.Result.Tasks[0].AssignmentTokenFingerprint == "" || response.Result.Tasks[0].AssignmentToken != "" {
		t.Fatalf("bad redacted task: %+v", response.Result.Tasks)
	}
	if len(response.Result.Workers) != 1 || response.Result.Workers[0].WorkerIdentity != "worker-debug" || response.Result.Workers[0].ActiveTaskID != task.Result.ID {
		t.Fatalf("bad worker view: %+v", response.Result.Workers)
	}

	unsafe := runBandmaster(t, repo, "debug", "--session", session.Result.ID, "--unsafe", "--json")
	if unsafe.exitCode != 0 || !strings.Contains(unsafe.stdout, token) {
		t.Fatalf("authorized debug did not reveal the assignment token: exit=%d stdout=%s stderr=%s", unsafe.exitCode, unsafe.stdout, unsafe.stderr)
	}
}

func TestInitDoesNotInstallDedicatedDebuggingSkillByDefault(t *testing.T) {
	repo := newGitRepository(t)
	result := runBandmaster(t, repo, "init", "--json")
	if result.exitCode != 0 {
		t.Fatalf("init: %s %s", result.stdout, result.stderr)
	}
	path := filepath.Join(repo, ".agents", "skills", "debug-bandmaster", "SKILL.md")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("default init installed debug skill: %v", err)
	}
	var response struct {
		Result struct {
			DebugSkillPath string `json:"debug_skill_path"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil || response.Result.DebugSkillPath != "" {
		t.Fatalf("default init reported debug skill: %+v err=%v", response, err)
	}
}

func TestInitDebugSkillFlagInstallsDedicatedSkillDeterministically(t *testing.T) {
	repo := newGitRepository(t)
	first := runBandmaster(t, repo, "init", "--debug-skill", "--json")
	if first.exitCode != 0 {
		t.Fatalf("init: %s %s", first.stdout, first.stderr)
	}
	path := filepath.Join(repo, ".agents", "skills", "debug-bandmaster", "SKILL.md")
	metadataPath := filepath.Join(repo, ".agents", "skills", "debug-bandmaster", "agents", "openai.yaml")
	skill := readFile(t, path)
	metadata := readFile(t, metadataPath)
	for _, expected := range []string{"name: debug-bandmaster", "debug, diagnose, inspect, troubleshoot, or explain", "bandmaster debug --json", "bandmaster debug --watch --json", "diagnosis-only", "go build", "fresh"} {
		if !strings.Contains(skill, expected) {
			t.Errorf("debug skill missing %q:\n%s", expected, skill)
		}
	}
	for _, expected := range []string{"display_name: \"Debug Bandmaster\"", "$debug-bandmaster"} {
		if !strings.Contains(metadata, expected) {
			t.Errorf("metadata missing %q:\n%s", expected, metadata)
		}
	}
	orchestration := readFile(t, filepath.Join(repo, ".agents", "skills", "bandmaster", "SKILL.md"))
	if strings.Contains(orchestration, "# Debug Bandmaster") || strings.Contains(orchestration, "diagnosis-only") {
		t.Fatal("debugging workflow leaked into orchestration skill")
	}
	second := runBandmaster(t, repo, "init", "--debug-skill", "--json")
	if second.exitCode != 0 || readFile(t, path) != skill || readFile(t, metadataPath) != metadata {
		t.Fatal("init did not update the debugging skill deterministically")
	}
	writeFile(t, path, "custom debug skill\n")
	withoutFlag := runBandmaster(t, repo, "init", "--json")
	if withoutFlag.exitCode != 0 || readFile(t, path) != "custom debug skill\n" {
		t.Fatal("default init modified an existing debug skill")
	}
}

func TestInitRejectsInvalidDebugSkillOptions(t *testing.T) {
	for _, args := range [][]string{{"init", "--unknown", "--json"}, {"init", "--debug-skill", "--debug-skill", "--json"}} {
		repo := newGitRepository(t)
		result := runBandmaster(t, repo, args...)
		if result.exitCode != 3 {
			t.Fatalf("%v exit=%d stdout=%s stderr=%s", args, result.exitCode, result.stdout, result.stderr)
		}
		if _, err := os.Stat(filepath.Join(repo, ".git", "bandmaster")); !os.IsNotExist(err) {
			t.Fatalf("%v created state: %v", args, err)
		}
	}
}

func TestDebugReturnsPartialEvidenceForMalformedState(t *testing.T) {
	repo := newGitRepository(t)
	statePath := filepath.Join(repo, ".git", "bandmaster", "state.db")
	writeFile(t, statePath, "not a sqlite database\n")
	before := readFile(t, statePath)
	result := runBandmaster(t, repo, "debug", "--json")
	if result.exitCode != 0 {
		t.Fatalf("debug malformed state exit=%d stdout=%s stderr=%s", result.exitCode, result.stdout, result.stderr)
	}
	var response struct {
		Result struct {
			Collection struct {
				Status string `json:"status"`
				Errors []struct {
					Section string `json:"section"`
				} `json:"errors"`
			} `json:"collection"`
			Runtime struct {
				GoVersion string `json:"go_version"`
			} `json:"runtime"`
			Repository struct {
				Root string `json:"root"`
			} `json:"repository"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("decode: %v\n%s", err, result.stdout)
	}
	if response.Result.Collection.Status != "partial" || len(response.Result.Collection.Errors) == 0 || response.Result.Runtime.GoVersion == "" || response.Result.Repository.Root == "" {
		t.Fatalf("missing partial evidence: %+v", response.Result)
	}
	if after := readFile(t, statePath); after != before {
		t.Fatal("debug changed malformed state")
	}
}

func TestDebugReturnsPartialEvidenceForMalformedStateWithSessionSelection(t *testing.T) {
	repo := newGitRepository(t)
	statePath := filepath.Join(repo, ".git", "bandmaster", "state.db")
	writeFile(t, statePath, "not a sqlite database\n")
	result := runBandmaster(t, repo, "debug", "--session", "session_expected", "--json")
	if result.exitCode != 0 {
		t.Fatalf("selected degraded debug exit=%d stdout=%s stderr=%s", result.exitCode, result.stdout, result.stderr)
	}
	var response struct {
		Result struct {
			Collection struct {
				Status string `json:"status"`
			} `json:"collection"`
			Runtime struct {
				GoVersion string `json:"go_version"`
			} `json:"runtime"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil || response.Result.Collection.Status != "partial" || response.Result.Runtime.GoVersion == "" {
		t.Fatalf("missing selected partial evidence: %+v err=%v", response, err)
	}
}

func TestDebugMissingSelectedSessionIsNonzero(t *testing.T) {
	repo := approvedCleanRepository(t)
	result := runBandmaster(t, repo, "debug", "--session", "session_missing", "--json")
	if result.exitCode != 3 {
		t.Fatalf("missing session exit=%d stdout=%s stderr=%s", result.exitCode, result.stdout, result.stderr)
	}
	var response struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil || response.Error.Code != "session_not_found" {
		t.Fatalf("unexpected missing-session error: %+v err=%v", response, err)
	}
}
