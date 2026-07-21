package integration_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var bandmasterBinary string

func TestMain(m *testing.M) {
	buildDir, err := os.MkdirTemp("", "bandmaster-test-bin-")
	if err != nil {
		panic(err)
	}
	name := "bandmaster"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bandmasterBinary = filepath.Join(buildDir, name)

	cmd := exec.Command("go", "build", "-o", bandmasterBinary, "../cmd/bandmaster")
	cmd.Dir = filepath.Join(".")
	if output, err := cmd.CombinedOutput(); err != nil {
		panic("build bandmaster: " + err.Error() + ": " + string(output))
	}

	exitCode := m.Run()
	_ = os.RemoveAll(buildDir)
	os.Exit(exitCode)
}

func TestInitGeneratesUnapprovedConfigAndCodexSkill(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, filepath.Join(repo, "package.json"), `{"scripts":{"test":"node --test"}}`)

	result := runBandmaster(t, repo, "init", "--json")
	if result.exitCode != 0 {
		t.Fatalf("init exit code = %d, stderr = %s", result.exitCode, result.stderr)
	}

	var response struct {
		SchemaVersion string `json:"schema_version"`
		Command       string `json:"command"`
		Success       bool   `json:"success"`
		Result        struct {
			ConfigPath       string `json:"config_path"`
			SkillPath        string `json:"skill_path"`
			ValidationDigest string `json:"validation_digest"`
			Approved         bool   `json:"approved"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("decode JSON response: %v\n%s", err, result.stdout)
	}
	if response.SchemaVersion != "1" || response.Command != "init" || !response.Success {
		t.Fatalf("unexpected response envelope: %+v", response)
	}
	if response.Result.ConfigPath != ".bandmaster.yaml" {
		t.Errorf("config path = %q", response.Result.ConfigPath)
	}
	if response.Result.SkillPath != ".agents/skills/bandmaster/SKILL.md" {
		t.Errorf("skill path = %q", response.Result.SkillPath)
	}
	if len(response.Result.ValidationDigest) != 64 {
		t.Errorf("validation digest = %q", response.Result.ValidationDigest)
	}
	if response.Result.Approved {
		t.Error("detected validation must start unapproved")
	}

	config := readFile(t, filepath.Join(repo, ".bandmaster.yaml"))
	for _, expected := range []string{"version: 1", "worker_lease_duration: 5m", "name: npm-test", "- npm", "- test"} {
		if !strings.Contains(config, expected) {
			t.Errorf("config does not contain %q:\n%s", expected, config)
		}
	}

	skill := readFile(t, filepath.Join(repo, ".agents", "skills", "bandmaster", "SKILL.md"))
	if !strings.Contains(skill, "name: bandmaster") || !strings.Contains(skill, "bandmaster") {
		t.Fatalf("unexpected generated skill:\n%s", skill)
	}
}

func TestTUIRejectsJSONModeWithStableError(t *testing.T) {
	repo := newGitRepository(t)
	result := runBandmaster(t, repo, "tui", "--json")
	if result.exitCode != 3 {
		t.Fatalf("tui JSON exit code = %d, want 3; stdout = %s", result.exitCode, result.stdout)
	}
	var response struct {
		SchemaVersion string `json:"schema_version"`
		Command       string `json:"command"`
		Success       bool   `json:"success"`
		Error         struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("decode TUI JSON rejection: %v\n%s", err, result.stdout)
	}
	if response.SchemaVersion != "1" || response.Command != "tui" || response.Success || response.Error.Code != "invalid_arguments" || response.Error.Retryable {
		t.Fatalf("unexpected TUI JSON rejection: %+v", response)
	}
}

func TestVersionProvidesHumanAndVersionedJSONOutput(t *testing.T) {
	directory := t.TempDir()
	human := runBandmaster(t, directory, "version")
	if human.exitCode != 0 || !strings.Contains(human.stdout, "bandmaster dev") {
		t.Fatalf("unexpected human version output: exit=%d stdout=%q stderr=%q", human.exitCode, human.stdout, human.stderr)
	}

	jsonResult := runBandmaster(t, directory, "version", "--json")
	if jsonResult.exitCode != 0 {
		t.Fatalf("version exit code = %d, stderr = %s", jsonResult.exitCode, jsonResult.stderr)
	}
	var response struct {
		SchemaVersion string `json:"schema_version"`
		Command       string `json:"command"`
		Success       bool   `json:"success"`
		Result        struct {
			Version                 string `json:"version"`
			JSONSchemaVersion       string `json:"json_schema_version"`
			JSONSchemaCompatibility string `json:"json_schema_compatibility"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(jsonResult.stdout), &response); err != nil {
		t.Fatalf("decode version response: %v\n%s", err, jsonResult.stdout)
	}
	if response.SchemaVersion != "1" || response.Command != "version" || !response.Success || response.Result.Version != "dev" || response.Result.JSONSchemaVersion != "1" || response.Result.JSONSchemaCompatibility == "" {
		t.Fatalf("unexpected version response: %+v", response)
	}
}

func TestConfigApprovalAppliesOnlyToTheExactCurrentDigest(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/project\n\ngo 1.22\n")

	initResult := runBandmaster(t, repo, "init", "--json")
	if initResult.exitCode != 0 {
		t.Fatalf("init exit code = %d, stderr = %s", initResult.exitCode, initResult.stderr)
	}
	var initialized struct {
		Result struct {
			ValidationDigest string `json:"validation_digest"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(initResult.stdout), &initialized); err != nil {
		t.Fatalf("decode init response: %v", err)
	}

	approvedResult := runBandmaster(t, repo, "config", "approve", initialized.Result.ValidationDigest, "--json")
	if approvedResult.exitCode != 0 {
		t.Fatalf("approve exit code = %d, stderr = %s", approvedResult.exitCode, approvedResult.stderr)
	}
	assertApprovalStatus(t, repo, true, initialized.Result.ValidationDigest)

	configPath := filepath.Join(repo, ".bandmaster.yaml")
	config := readFile(t, configPath)
	writeFile(t, configPath, strings.Replace(config, "timeout: 10m", "timeout: 5m", 1))

	assertApprovalStatus(t, repo, false, "")
	staleApproval := runBandmaster(t, repo, "config", "approve", initialized.Result.ValidationDigest, "--json")
	if staleApproval.exitCode != 3 {
		t.Fatalf("stale approval exit code = %d, want 3; stdout = %s; stderr = %s", staleApproval.exitCode, staleApproval.stdout, staleApproval.stderr)
	}
	var failure struct {
		Success bool `json:"success"`
		Error   struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(staleApproval.stdout), &failure); err != nil {
		t.Fatalf("decode stale approval response: %v\n%s", err, staleApproval.stdout)
	}
	if failure.Success || failure.Error.Code != "configuration_digest_mismatch" || failure.Error.Retryable || !strings.Contains(failure.Error.Message, "bandmaster config approve") {
		t.Fatalf("unexpected stale approval error: %+v", failure)
	}
}

func TestApprovalIsLocalToTheCloneAndRuntimeState(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/project\n\ngo 1.24\n")
	initResult := runBandmaster(t, repo, "init", "--json")
	if initResult.exitCode != 0 {
		t.Fatalf("init exit code = %d, stderr = %s", initResult.exitCode, initResult.stderr)
	}
	digest := responseDigest(t, initResult.stdout)
	approved := runBandmaster(t, repo, "config", "approve", digest, "--json")
	if approved.exitCode != 0 {
		t.Fatalf("approve exit code = %d, stderr = %s", approved.exitCode, approved.stderr)
	}

	if err := os.RemoveAll(filepath.Join(repo, ".git", "bandmaster")); err != nil {
		t.Fatalf("remove runtime state: %v", err)
	}
	assertApprovalStatus(t, repo, false, digest)
	approved = runBandmaster(t, repo, "config", "approve", digest, "--json")
	if approved.exitCode != 0 {
		t.Fatalf("reapprove exit code = %d, stderr = %s", approved.exitCode, approved.stderr)
	}

	runGit(t, repo, "add", ".bandmaster.yaml", ".agents/skills/bandmaster/SKILL.md", "go.mod")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Initialize Bandmaster")
	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, filepath.Dir(clone), "clone", repo, clone)
	assertApprovalStatus(t, clone, false, digest)
}

func responseDigest(t *testing.T, output string) string {
	t.Helper()
	var response struct {
		Result struct {
			ValidationDigest string `json:"validation_digest"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("decode response digest: %v\n%s", err, output)
	}
	return response.Result.ValidationDigest
}

func TestInitDetectsValidationAcrossAMonorepo(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/project\n\ngo 1.24\n")
	writeFile(t, filepath.Join(repo, "pnpm-lock.yaml"), "lockfileVersion: '9.0'\n")
	writeFile(t, filepath.Join(repo, "apps", "web", "package.json"), `{"scripts":{"test":"vitest run","typecheck":"tsc --noEmit","dev":"vite"}}`)

	result := runBandmaster(t, repo, "init", "--json")
	if result.exitCode != 0 {
		t.Fatalf("init exit code = %d, stderr = %s", result.exitCode, result.stderr)
	}
	config := readFile(t, filepath.Join(repo, ".bandmaster.yaml"))
	for _, expected := range []string{
		"name: go-test",
		"- go",
		"- ./...",
		"name: pnpm-test-apps-web",
		"name: pnpm-typecheck-apps-web",
		"working_directory: apps/web",
	} {
		if !strings.Contains(config, expected) {
			t.Errorf("config does not contain %q:\n%s", expected, config)
		}
	}
	if strings.Contains(config, "dev") {
		t.Fatalf("development script was detected as validation:\n%s", config)
	}
}

func TestInitDisambiguatesCollidingMonorepoCommandNames(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, filepath.Join(repo, "a_b", "go.mod"), "module example.com/underscore\n\ngo 1.24\n")
	writeFile(t, filepath.Join(repo, "a-b", "go.mod"), "module example.com/hyphen\n\ngo 1.24\n")

	result := runBandmaster(t, repo, "init", "--json")
	if result.exitCode != 0 {
		t.Fatalf("init exit code = %d, stderr = %s, stdout = %s", result.exitCode, result.stderr, result.stdout)
	}
	config := readFile(t, filepath.Join(repo, ".bandmaster.yaml"))
	if strings.Count(config, "name: go-test-a-b-") != 2 {
		t.Fatalf("colliding commands were not disambiguated:\n%s", config)
	}
	assertApprovalStatus(t, repo, false, responseDigest(t, result.stdout))
}

func TestInitPreservesProjectConfigAndOverwritesGeneratedSkill(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/project\n\ngo 1.24\n")
	first := runBandmaster(t, repo, "init", "--json")
	if first.exitCode != 0 {
		t.Fatalf("first init exit code = %d, stderr = %s", first.exitCode, first.stderr)
	}

	configPath := filepath.Join(repo, ".bandmaster.yaml")
	configured := strings.Replace(readFile(t, configPath), "timeout: 10m", "timeout: 5m", 1)
	writeFile(t, configPath, configured)
	skillPath := filepath.Join(repo, ".agents", "skills", "bandmaster", "SKILL.md")
	writeFile(t, skillPath, "local edits that must be replaced\n")

	second := runBandmaster(t, repo, "init", "--json")
	if second.exitCode != 0 {
		t.Fatalf("second init exit code = %d, stderr = %s", second.exitCode, second.stderr)
	}
	if got := readFile(t, configPath); got != configured {
		t.Fatalf("init replaced existing project configuration:\n%s", got)
	}
	skill := readFile(t, skillPath)
	for _, expected := range []string{
		"name: bandmaster",
		"at least **two** tasks are independently implementable",
		"parent Codex agent is the sole orchestrator",
		"do not start workers or a replacement session automatically",
		"bandmaster task claim <task-id> --token <token>",
		"bandmaster task heartbeat <task-id> --token <token>",
		"It must not run Git mutations",
		"bandmaster task submit <task-id> --token <token>",
		"bandmaster task requeue <task-id> --json",
		"bandmaster task repair",
		"Never infer termination from a missing handle",
	} {
		if !strings.Contains(skill, expected) {
			t.Errorf("generated skill does not contain %q:\n%s", expected, skill)
		}
	}
	if strings.Contains(skill, "local edits") {
		t.Fatalf("generated skill retained local edits:\n%s", skill)
	}
}

func TestConfigStatusRejectsInvalidVersionedConfiguration(t *testing.T) {
	repo := newGitRepository(t)
	writeFile(t, filepath.Join(repo, ".bandmaster.yaml"), "version: 2\nvalidation:\n  commands: []\n")

	result := runBandmaster(t, repo, "config", "status", "--json")
	if result.exitCode != 3 {
		t.Fatalf("exit code = %d, want 3; stdout = %s; stderr = %s", result.exitCode, result.stdout, result.stderr)
	}
	var response struct {
		Success bool `json:"success"`
		Error   struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("decode invalid configuration response: %v\n%s", err, result.stdout)
	}
	if response.Success || response.Error.Code != "unsupported_configuration_version" || response.Error.Retryable {
		t.Fatalf("unexpected invalid configuration response: %+v", response)
	}
}

func TestConfigStatusRejectsAmbiguousOrEscapingConfiguration(t *testing.T) {
	t.Run("trailing YAML document", func(t *testing.T) {
		repo := newGitRepository(t)
		writeFile(t, filepath.Join(repo, ".bandmaster.yaml"), "version: 1\nvalidation:\n  commands: []\n---\nversion: 1\nvalidation:\n  commands: []\n")
		assertConfigError(t, repo, "invalid_configuration")
	})

	t.Run("symlinked working directory outside repository", func(t *testing.T) {
		repo := newGitRepository(t)
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(repo, "outside")); err != nil {
			t.Fatalf("create working-directory symlink: %v", err)
		}
		writeFile(t, filepath.Join(repo, ".bandmaster.yaml"), "version: 1\nvalidation:\n  commands:\n    - name: escaped\n      argv: [go, test, ./...]\n      working_directory: outside\n      timeout: 10m\n")
		assertConfigError(t, repo, "invalid_configuration")
	})
}

func assertConfigError(t *testing.T, repo, wantCode string) {
	t.Helper()
	result := runBandmaster(t, repo, "config", "status", "--json")
	if result.exitCode != 3 {
		t.Fatalf("exit code = %d, want 3; stdout = %s; stderr = %s", result.exitCode, result.stdout, result.stderr)
	}
	var response struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("decode configuration error: %v\n%s", err, result.stdout)
	}
	if response.Error.Code != wantCode {
		t.Fatalf("configuration error code = %q, want %q", response.Error.Code, wantCode)
	}
}

func TestInitRejectsUnsupportedRepositoryLayoutsWithTypedErrors(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*testing.T) string
		wantCode string
	}{
		{
			name:     "not a repository",
			setup:    func(t *testing.T) string { return t.TempDir() },
			wantCode: "not_git_repository",
		},
		{
			name: "bare repository",
			setup: func(t *testing.T) string {
				parent := t.TempDir()
				repo := filepath.Join(parent, "bare.git")
				runGit(t, parent, "init", "--bare", "-b", "main", repo)
				return repo
			},
			wantCode: "unsupported_bare_repository",
		},
		{
			name: "linked worktree",
			setup: func(t *testing.T) string {
				repo := newGitRepository(t)
				commitRepository(t, repo)
				worktree := filepath.Join(t.TempDir(), "linked")
				runGit(t, repo, "worktree", "add", "-b", "linked", worktree)
				return worktree
			},
			wantCode: "unsupported_linked_worktree",
		},
		{
			name: "main checkout with linked worktree",
			setup: func(t *testing.T) string {
				repo := newGitRepository(t)
				commitRepository(t, repo)
				worktree := filepath.Join(t.TempDir(), "linked")
				runGit(t, repo, "worktree", "add", "-b", "linked", worktree)
				return repo
			},
			wantCode: "unsupported_linked_worktree",
		},
		{
			name: "sparse checkout",
			setup: func(t *testing.T) string {
				repo := newGitRepository(t)
				commitRepository(t, repo)
				runGit(t, repo, "sparse-checkout", "init", "--cone")
				return repo
			},
			wantCode: "unsupported_sparse_checkout",
		},
		{
			name: "submodule gitlink",
			setup: func(t *testing.T) string {
				repo := newGitRepository(t)
				commitRepository(t, repo)
				sha := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
				runGit(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+sha+",vendor/dependency")
				return repo
			},
			wantCode: "unsupported_submodule",
		},
		{
			name: "nested repository",
			setup: func(t *testing.T) string {
				repo := newGitRepository(t)
				nested := filepath.Join(repo, "vendor", "dependency")
				if err := os.MkdirAll(nested, 0o755); err != nil {
					t.Fatal(err)
				}
				runGit(t, nested, "init", "-b", "main")
				return repo
			},
			wantCode: "unsupported_nested_repository",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := test.setup(t)
			result := runBandmaster(t, directory, "init", "--json")
			if result.exitCode != 3 {
				t.Fatalf("exit code = %d, want 3; stdout = %s; stderr = %s", result.exitCode, result.stdout, result.stderr)
			}
			var response struct {
				SchemaVersion string `json:"schema_version"`
				Success       bool   `json:"success"`
				Error         struct {
					Code      string `json:"code"`
					Message   string `json:"message"`
					Retryable bool   `json:"retryable"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
				t.Fatalf("decode error response: %v\n%s", err, result.stdout)
			}
			if response.SchemaVersion != "1" || response.Success || response.Error.Code != test.wantCode || response.Error.Message == "" || response.Error.Retryable {
				t.Fatalf("unexpected error response: %+v", response)
			}
		})
	}
}

func TestInitRejectsProjectArtifactSymlinkEscape(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string, string)
	}{
		{
			name: "skill parent",
			setup: func(t *testing.T, repo, outside string) {
				if err := os.Symlink(outside, filepath.Join(repo, ".agents")); err != nil {
					t.Fatalf("create skill parent symlink: %v", err)
				}
			},
		},
		{
			name: "configuration file",
			setup: func(t *testing.T, repo, outside string) {
				outsideConfig := filepath.Join(outside, "config.yaml")
				writeFile(t, outsideConfig, "version: 1\nvalidation:\n  commands: []\n")
				if err := os.Symlink(outsideConfig, filepath.Join(repo, ".bandmaster.yaml")); err != nil {
					t.Fatalf("create configuration symlink: %v", err)
				}
			},
		},
		{
			name: "runtime state parent",
			setup: func(t *testing.T, repo, outside string) {
				if err := os.Symlink(outside, filepath.Join(repo, ".git", "bandmaster")); err != nil {
					t.Fatalf("create runtime state symlink: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := newGitRepository(t)
			outside := t.TempDir()
			test.setup(t, repo, outside)
			result := runBandmaster(t, repo, "init", "--json")
			if result.exitCode != 3 {
				t.Fatalf("exit code = %d, want 3; stdout = %s; stderr = %s", result.exitCode, result.stdout, result.stderr)
			}
			var response struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
				t.Fatalf("decode symlink error: %v\n%s", err, result.stdout)
			}
			if response.Error.Code != "unsafe_project_path" {
				t.Fatalf("error code = %q, want unsafe_project_path", response.Error.Code)
			}
			if _, err := os.Stat(filepath.Join(outside, "skills", "bandmaster", "SKILL.md")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("skill escaped repository, stat error = %v", err)
			}
		})
	}
}

func TestInitSupportsWhitespaceInRootAndOrdinaryDotGitFile(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repository ?# ")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatalf("create repository: %v", err)
	}
	runGit(t, repo, "init", "-b", "main")
	writeFile(t, filepath.Join(repo, "docs", ".git"), "ordinary documentation fixture\n")

	result := runBandmaster(t, repo, "init", "--json")
	if result.exitCode != 0 {
		t.Fatalf("init exit code = %d, stdout = %s, stderr = %s", result.exitCode, result.stdout, result.stderr)
	}
}

func TestInitRejectsFilesystemAliasesOfNestedGitMetadata(t *testing.T) {
	repo := newGitRepository(t)
	nested := filepath.Join(repo, "vendor", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("create nested repository: %v", err)
	}
	runGit(t, nested, "init", "-b", "main")
	metadata := filepath.Join(nested, ".git")
	temporary := filepath.Join(nested, ".git-temporary")
	if err := os.Rename(metadata, temporary); err != nil {
		t.Fatalf("temporarily rename nested metadata: %v", err)
	}
	if err := os.Rename(temporary, filepath.Join(nested, ".GIT")); err != nil {
		t.Fatalf("rename nested metadata with alternate spelling: %v", err)
	}

	probe := exec.Command("git", "rev-parse", "--show-toplevel")
	probe.Dir = nested
	_, aliasErr := probe.Output()
	result := runBandmaster(t, repo, "init", "--json")
	if aliasErr == nil {
		if result.exitCode != 3 {
			t.Fatalf("nested metadata alias exit code = %d, want 3; stdout = %s; stderr = %s", result.exitCode, result.stdout, result.stderr)
		}
		var response struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
			t.Fatalf("decode nested metadata alias error: %v\n%s", err, result.stdout)
		}
		if response.Error.Code != "unsupported_nested_repository" {
			t.Fatalf("nested metadata alias error = %q, want unsupported_nested_repository", response.Error.Code)
		}
		return
	}
	if result.exitCode != 0 {
		t.Fatalf("distinct .GIT directory should not be treated as Git metadata: exit=%d stdout=%s stderr=%s", result.exitCode, result.stdout, result.stderr)
	}
}

func assertApprovalStatus(t *testing.T, repo string, wantApproved bool, wantDigest string) {
	t.Helper()
	result := runBandmaster(t, repo, "config", "status", "--json")
	if result.exitCode != 0 {
		t.Fatalf("config status exit code = %d, stderr = %s", result.exitCode, result.stderr)
	}
	var response struct {
		Success bool `json:"success"`
		Result  struct {
			ValidationDigest string `json:"validation_digest"`
			Approved         bool   `json:"approved"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
		t.Fatalf("decode config status response: %v\n%s", err, result.stdout)
	}
	if !response.Success || response.Result.Approved != wantApproved {
		t.Fatalf("approval status = %+v, want approved %t", response, wantApproved)
	}
	if wantDigest != "" && response.Result.ValidationDigest != wantDigest {
		t.Fatalf("validation digest = %q, want %q", response.Result.ValidationDigest, wantDigest)
	}
}

type commandResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func runBandmaster(t *testing.T, dir string, args ...string) commandResult {
	t.Helper()
	cmd := exec.Command(bandmasterBinary, args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("run bandmaster: %v", err)
		}
		exitCode = exitError.ExitCode()
	}
	return commandResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String()}
}

func newGitRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = repo
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	t.Cleanup(func() {
		// Stop any session monitor before TempDir removes its SQLite state.
		pause := exec.Command(bandmasterBinary, "session", "pause", "--json")
		pause.Dir = repo
		_ = pause.Run()
	})
	return repo
}

func commitRepository(t *testing.T, repo string) {
	t.Helper()
	writeFile(t, filepath.Join(repo, "README.md"), "# Test repository\n")
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "-c", "user.name=Bandmaster Tests", "-c", "user.email=bandmaster@example.invalid", "commit", "-m", "Initial commit")
}

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = directory
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
