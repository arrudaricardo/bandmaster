package integration_test

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
