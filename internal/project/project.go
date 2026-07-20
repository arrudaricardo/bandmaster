package project

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
	"gopkg.in/yaml.v3"
)

const generatedSkill = `---
name: bandmaster
description: Coordinate parallel Codex workers safely with Bandmaster.
---

# Bandmaster

Use the project-local 'bandmaster' CLI for orchestration. **The parent Codex agent is the sole orchestrator:** only it may create tasks, assign work, manage barriers, run validation, finalize batches, recover workers, or spawn workers. Workers must never spawn agents or run orchestration commands. Always append '--json' and make decisions from stable JSON fields, never human prose.

## Decide whether to orchestrate

First run 'bandmaster session inspect --json'. If it reports an active, paused, finalizing, or aborting session, report that interrupted or ongoing session to the user and offer to resume it; do not start workers or a replacement session automatically. Treat workers from a lost parent as quarantined until the parent-held worker handle proves termination. If no session is open, use Bandmaster only when at least **two** tasks are independently implementable, independently testable, and have disjoint expected write sets. Otherwise work normally without Bandmaster.

Before a new session, run 'bandmaster config status --json'. If the validation configuration is not approved, report its digest and ask the user to review '.bandmaster.yaml' and run 'bandmaster config approve <digest> --json'. Never approve configuration on the user's behalf. After approval and with a clean repository, run 'bandmaster session start --json'.

## Parent workflow

Create a durable plan with 'bandmaster task create ... --json', including title, intent, expected outcome, and prerequisite IDs. Start every currently ready, independent task; do not impose an artificial concurrency cap. Assign each with 'bandmaster task assign <task-id> --worker <stable-worker-id> --json', retain the returned assignment token privately, and give the worker only its task ID and token.

When a worker reports a claim conflict or becomes blocked, wait for the conflicting owner to release or finish, then use 'bandmaster task requeue <task-id> --json' and assign a fresh worker. Wait for all batch members to submit or be deliberately stopped before 'bandmaster batch freeze --json', then run 'bandmaster batch validate --json' and 'bandmaster batch commit --json'. For validation failures, diagnose the result and reopen only the original owning task with 'bandmaster task repair ... --diagnosis <text> --intended-repair <text> --terminated-worker <id> --termination-proof <proof> --json'; then assign its repair. Never transfer an owner's claim to another task.

If a worker handle is lost or a lease expires, keep its task quarantined. Replace it only with proof from the parent-held handle using 'task recover ... --terminated-worker ... --termination-proof ... --json', or after explicit user confirmation with 'task recover ... --user-confirmation <text> ... --json'. Never infer termination from a missing handle.

## Worker contract

A worker edits only its assigned task. It must use its token on every worker command, first claim its complete initial write set before writing ('bandmaster task claim <task-id> --token <token> --path <path> --json'), and heartbeat during long work ('bandmaster task heartbeat <task-id> --token <token> --json'). It may expand or release only its own claims. It must not run Git mutations ('git add', 'git commit', checkout, reset, stash, rebase, or branch operations), create tasks, spawn agents, or edit unclaimed paths.

Before stopping, the worker reviews its owned diff with 'bandmaster task diff <task-id> --token <token> --json', then submits a structured handoff using 'bandmaster task submit <task-id> --token <token> --behavior-changed <text> --key-decisions <text> --validation-expectations <text> --known-risks <text> --json'. It then stops editing and reports the handoff to the parent. If it cannot claim safely, it reports the blocked result and exits without writing.
`

type Error struct {
	Code      string
	Message   string
	Retryable bool
	ExitCode  int
	SessionID string
}

type Project struct {
	Root   string
	GitDir string
}

type InitResult struct {
	ConfigPath       string `json:"config_path"`
	SkillPath        string `json:"skill_path"`
	ValidationDigest string `json:"validation_digest"`
	Approved         bool   `json:"approved"`
}

type ConfigStatus struct {
	ValidationDigest string `json:"validation_digest"`
	Approved         bool   `json:"approved"`
}

type configuration struct {
	Version             int               `yaml:"version"`
	WorkerLeaseDuration string            `yaml:"worker_lease_duration"`
	Validation          validationSection `yaml:"validation"`
}

type validationSection struct {
	Commands []validationCommand `yaml:"commands"`
}

type validationCommand struct {
	Name             string            `yaml:"name"`
	Argv             []string          `yaml:"argv,omitempty"`
	Script           string            `yaml:"script,omitempty"`
	WorkingDirectory string            `yaml:"working_directory"`
	Timeout          string            `yaml:"timeout"`
	Environment      map[string]string `yaml:"environment,omitempty"`
}

func (p *Project) readConfiguration() (configuration, *Error) {
	content, err := os.ReadFile(filepath.Join(p.Root, ".bandmaster.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return configuration{}, invalid("configuration_not_initialized", "Bandmaster is not initialized. Run `bandmaster init` first.")
	}
	if err != nil {
		return configuration{}, internal("read project configuration", err)
	}
	if projectError := validateConfiguration(content, p.Root); projectError != nil {
		return configuration{}, projectError
	}
	return decodeConfiguration(content)
}

func (p *Project) readApprovedConfiguration(queryer rowQuerier) (configuration, string, *Error) {
	content, err := os.ReadFile(filepath.Join(p.Root, ".bandmaster.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return configuration{}, "", invalid("configuration_not_initialized", "Bandmaster is not initialized. Run `bandmaster init` first.")
	}
	if err != nil {
		return configuration{}, "", internal("read project configuration", err)
	}
	if projectError := validateConfiguration(content, p.Root); projectError != nil {
		return configuration{}, "", projectError
	}
	digestBytes := sha256.Sum256(content)
	digest := hex.EncodeToString(digestBytes[:])
	var approvedDigest string
	if err := queryer.QueryRow(`SELECT value FROM metadata WHERE key = 'approved_configuration_digest'`).Scan(&approvedDigest); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return configuration{}, "", internal("read configuration approval", err)
	}
	if approvedDigest != digest {
		return configuration{}, digest, invalid("configuration_not_approved", fmt.Sprintf("Validation configuration %s is not approved.", digest))
	}
	config, projectError := decodeConfiguration(content)
	return config, digest, projectError
}

func decodeConfiguration(content []byte) (configuration, *Error) {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	var config configuration
	if err := decoder.Decode(&config); err != nil {
		return configuration{}, invalid("invalid_configuration", fmt.Sprintf("Invalid .bandmaster.yaml: %v", err))
	}
	for index := range config.Validation.Commands {
		if config.Validation.Commands[index].WorkingDirectory == "" {
			config.Validation.Commands[index].WorkingDirectory = "."
		}
	}
	return config, nil
}

func Open(cwd string) (*Project, *Error) {
	bare, bareErr := gitOutput(cwd, "rev-parse", "--is-bare-repository")
	if bareErr == nil && bare == "true" {
		return nil, invalid("unsupported_bare_repository", "Bare Git repositories are not supported.")
	}
	root, err := gitOutput(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, invalid("not_git_repository", "Bandmaster must be run in a Git working tree.")
	}
	gitDir, err := gitOutput(cwd, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return nil, internal("resolve Git metadata directory", err)
	}
	commonDir, err := gitOutput(cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, internal("resolve common Git metadata directory", err)
	}
	gitDir = filepath.Clean(gitDir)
	commonDir = filepath.Clean(commonDir)
	if gitDir != commonDir {
		return nil, invalid("unsupported_linked_worktree", "Linked Git worktrees are not supported.")
	}
	worktrees, err := gitOutput(cwd, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, internal("inspect linked Git worktrees", err)
	}
	if strings.Count("\n"+worktrees, "\nworktree ") > 1 {
		return nil, invalid("unsupported_linked_worktree", "Repositories with linked Git worktrees are not supported.")
	}
	root = filepath.Clean(root)
	if gitDir != filepath.Join(root, ".git") {
		return nil, invalid("unsupported_repository_layout", "Repositories with external Git metadata directories are not supported.")
	}
	project := &Project{Root: root, GitDir: commonDir}
	if projectError := project.validateLayout(); projectError != nil {
		return nil, projectError
	}
	return project, nil
}

func (p *Project) validateLayout() *Error {
	if sparse, err := gitOutput(p.Root, "config", "--bool", "core.sparseCheckout"); err == nil && sparse == "true" {
		return invalid("unsupported_sparse_checkout", "Sparse checkouts are not supported.")
	}
	index, err := gitOutput(p.Root, "ls-files", "--stage")
	if err != nil {
		return internal("inspect Git index", err)
	}
	for _, line := range strings.Split(index, "\n") {
		if strings.HasPrefix(line, "160000 ") {
			return invalid("unsupported_submodule", "Git submodules are not supported.")
		}
	}

	parent := filepath.Dir(p.Root)
	if outerRoot, err := gitOutput(parent, "rev-parse", "--show-toplevel"); err == nil && filepath.Clean(outerRoot) != p.Root {
		return invalid("unsupported_nested_repository", "A Git repository nested inside another repository is not supported.")
	}

	errNestedRepository := errors.New("nested Git repository")
	err = filepath.WalkDir(p.Root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == p.GitDir {
			return filepath.SkipDir
		}
		if path != p.Root && entryAliasesGitMetadata(path, entry) {
			candidateRoot := filepath.Dir(path)
			nestedRoot, nestedErr := gitOutput(candidateRoot, "rev-parse", "--show-toplevel")
			if nestedErr == nil && filepath.Clean(nestedRoot) == candidateRoot {
				return errNestedRepository
			}
			if entry.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if errors.Is(err, errNestedRepository) {
		return invalid("unsupported_nested_repository", "Nested Git repositories are not supported.")
	}
	if err != nil {
		return internal("inspect repository layout", err)
	}
	return nil
}

func entryAliasesGitMetadata(entryPath string, entry os.DirEntry) bool {
	if entry.Name() == ".git" {
		return true
	}
	entryInfo, err := entry.Info()
	if err != nil {
		return false
	}
	metadataInfo, err := os.Lstat(filepath.Join(filepath.Dir(entryPath), ".git"))
	return err == nil && os.SameFile(entryInfo, metadataInfo)
}

func (p *Project) Initialize() (InitResult, *Error) {
	configPath := filepath.Join(p.Root, ".bandmaster.yaml")
	skillPath := filepath.Join(p.Root, ".agents", "skills", "bandmaster", "SKILL.md")
	if projectError := validateLocalPath(p.Root, ".bandmaster.yaml"); projectError != nil {
		return InitResult{}, projectError
	}
	if projectError := validateLocalPath(p.Root, filepath.Join(".agents", "skills", "bandmaster", "SKILL.md")); projectError != nil {
		return InitResult{}, projectError
	}
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		commands, projectError := detectValidation(p.Root)
		if projectError != nil {
			return InitResult{}, projectError
		}
		config, err := yaml.Marshal(configuration{Version: 1, WorkerLeaseDuration: "5m", Validation: validationSection{Commands: commands}})
		if err != nil {
			return InitResult{}, internal("encode project configuration", err)
		}
		if err := writeFile(configPath, config, 0o644); err != nil {
			return InitResult{}, internal("write project configuration", err)
		}
	} else if err != nil {
		return InitResult{}, internal("inspect project configuration", err)
	}

	status, projectError := p.ConfigStatus()
	if projectError != nil {
		return InitResult{}, projectError
	}
	if err := writeFile(skillPath, []byte(generatedSkill), 0o644); err != nil {
		return InitResult{}, internal("write generated Codex skill", err)
	}
	return InitResult{
		ConfigPath:       ".bandmaster.yaml",
		SkillPath:        ".agents/skills/bandmaster/SKILL.md",
		ValidationDigest: status.ValidationDigest,
		Approved:         status.Approved,
	}, nil
}

func detectValidation(root string) ([]validationCommand, *Error) {
	type manifest struct {
		path string
		kind string
	}
	var manifests []manifest
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == ".agents" || entry.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		switch entry.Name() {
		case "go.mod", "package.json", "Cargo.toml", "pyproject.toml":
			manifests = append(manifests, manifest{path: path, kind: entry.Name()})
		}
		return nil
	})
	if err != nil {
		return nil, internal("scan repository for validation commands", err)
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].path < manifests[j].path })

	var commands []validationCommand
	for _, manifest := range manifests {
		directory := filepath.Dir(manifest.path)
		workingDirectory, err := filepath.Rel(root, directory)
		if err != nil {
			return nil, internal("resolve validation working directory", err)
		}
		workingDirectory = filepath.ToSlash(workingDirectory)
		if workingDirectory == "" {
			workingDirectory = "."
		}
		suffix := ""
		if workingDirectory != "." {
			suffix = "-" + strings.NewReplacer("/", "-", "_", "-").Replace(workingDirectory)
		}

		switch manifest.kind {
		case "go.mod":
			commands = append(commands, validationCommand{Name: "go-test" + suffix, Argv: []string{"go", "test", "./..."}, WorkingDirectory: workingDirectory, Timeout: "10m"})
		case "Cargo.toml":
			commands = append(commands, validationCommand{Name: "cargo-test" + suffix, Argv: []string{"cargo", "test"}, WorkingDirectory: workingDirectory, Timeout: "10m"})
		case "pyproject.toml":
			content, err := os.ReadFile(manifest.path)
			if err != nil {
				return nil, internal("read pyproject.toml", err)
			}
			if bytes.Contains(bytes.ToLower(content), []byte("pytest")) {
				commands = append(commands, validationCommand{Name: "pytest" + suffix, Argv: []string{"python", "-m", "pytest"}, WorkingDirectory: workingDirectory, Timeout: "10m"})
			}
		case "package.json":
			content, err := os.ReadFile(manifest.path)
			if err != nil {
				return nil, internal("read package.json", err)
			}
			var packageFile struct {
				Scripts map[string]string `json:"scripts"`
			}
			if err := json.Unmarshal(content, &packageFile); err != nil {
				return nil, invalid("validation_detection_failed", fmt.Sprintf("Cannot detect validation from %s: %v", filepath.ToSlash(manifest.path), err))
			}
			runner := detectPackageRunner(root, directory)
			for _, script := range []string{"test", "typecheck", "lint", "check"} {
				if strings.TrimSpace(packageFile.Scripts[script]) == "" {
					continue
				}
				argv := []string{runner, "run", script}
				if script == "test" && runner != "bun" {
					argv = []string{runner, script}
				}
				commands = append(commands, validationCommand{Name: runner + "-" + script + suffix, Argv: argv, WorkingDirectory: workingDirectory, Timeout: "10m"})
			}
		}
	}
	sort.SliceStable(commands, func(i, j int) bool {
		if commands[i].WorkingDirectory != commands[j].WorkingDirectory {
			if commands[i].WorkingDirectory == "." {
				return true
			}
			if commands[j].WorkingDirectory == "." {
				return false
			}
			return commands[i].WorkingDirectory < commands[j].WorkingDirectory
		}
		return commands[i].Name < commands[j].Name
	})
	disambiguateCommandNames(commands)
	return commands, nil
}

func disambiguateCommandNames(commands []validationCommand) {
	counts := make(map[string]int, len(commands))
	for _, command := range commands {
		counts[command.Name]++
	}
	for index := range commands {
		if counts[commands[index].Name] < 2 {
			continue
		}
		identity := commands[index].Name + "\x00" + commands[index].WorkingDirectory + "\x00" + strings.Join(commands[index].Argv, "\x00")
		digest := sha256.Sum256([]byte(identity))
		commands[index].Name += "-" + hex.EncodeToString(digest[:4])
	}
}

func detectPackageRunner(root, directory string) string {
	for current := directory; ; current = filepath.Dir(current) {
		for _, candidate := range []struct {
			file   string
			runner string
		}{
			{file: "pnpm-lock.yaml", runner: "pnpm"},
			{file: "yarn.lock", runner: "yarn"},
			{file: "bun.lock", runner: "bun"},
			{file: "bun.lockb", runner: "bun"},
			{file: "package-lock.json", runner: "npm"},
		} {
			if _, err := os.Stat(filepath.Join(current, candidate.file)); err == nil {
				return candidate.runner
			}
		}
		if current == root || filepath.Dir(current) == current {
			break
		}
	}
	return "npm"
}

func (p *Project) ConfigStatus() (ConfigStatus, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return ConfigStatus{}, projectError
	}
	defer db.Close()
	return p.configStatus(db)
}

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func (p *Project) configStatus(queryer rowQuerier) (ConfigStatus, *Error) {
	digest, projectError := p.currentConfigDigest()
	if projectError != nil {
		return ConfigStatus{}, projectError
	}
	var approvedDigest string
	err := queryer.QueryRow(`SELECT value FROM metadata WHERE key = 'approved_configuration_digest'`).Scan(&approvedDigest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ConfigStatus{}, internal("read configuration approval", err)
	}
	return ConfigStatus{ValidationDigest: digest, Approved: err == nil && approvedDigest == digest}, nil
}

func (p *Project) Approve(expectedDigest string) (ConfigStatus, *Error) {
	digest, projectError := p.currentConfigDigest()
	if projectError != nil {
		return ConfigStatus{}, projectError
	}
	if expectedDigest != digest {
		return ConfigStatus{}, invalid("configuration_digest_mismatch", fmt.Sprintf("Configuration digest is %s, not %s. Review the current configuration before approving it.", digest, expectedDigest))
	}
	db, projectError := p.openState()
	if projectError != nil {
		return ConfigStatus{}, projectError
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO metadata(key, value) VALUES('approved_configuration_digest', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, digest); err != nil {
		return ConfigStatus{}, internal("store configuration approval", err)
	}
	return ConfigStatus{ValidationDigest: digest, Approved: true}, nil
}

func (p *Project) currentConfigDigest() (string, *Error) {
	content, err := os.ReadFile(filepath.Join(p.Root, ".bandmaster.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return "", invalid("configuration_not_initialized", "Bandmaster is not initialized. Run `bandmaster init` first.")
	}
	if err != nil {
		return "", internal("read project configuration", err)
	}
	if projectError := validateConfiguration(content, p.Root); projectError != nil {
		return "", projectError
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:]), nil
}

func (p *Project) workerLeaseConfiguration() (time.Duration, string, *Error) {
	content, err := os.ReadFile(filepath.Join(p.Root, ".bandmaster.yaml"))
	if err != nil {
		return 0, "", internal("read worker lease configuration", err)
	}
	if projectError := validateConfiguration(content, p.Root); projectError != nil {
		return 0, "", projectError
	}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	var config configuration
	if err := decoder.Decode(&config); err != nil {
		return 0, "", invalid("invalid_configuration", fmt.Sprintf("Invalid .bandmaster.yaml: %v", err))
	}
	duration, err := time.ParseDuration(config.WorkerLeaseDuration)
	if err != nil || duration <= 0 {
		return 0, "", invalid("invalid_configuration", "worker_lease_duration must be a positive Go duration such as 5m.")
	}
	digest := sha256.Sum256(content)
	return duration, hex.EncodeToString(digest[:]), nil
}

func validateConfiguration(content []byte, root string) *Error {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	var config configuration
	if err := decoder.Decode(&config); err != nil {
		return invalid("invalid_configuration", fmt.Sprintf("Invalid .bandmaster.yaml: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return invalid("invalid_configuration", ".bandmaster.yaml must contain exactly one YAML document.")
		}
		return invalid("invalid_configuration", fmt.Sprintf("Invalid .bandmaster.yaml: %v", err))
	}
	if config.Version != 1 {
		return invalid("unsupported_configuration_version", fmt.Sprintf("Unsupported Bandmaster configuration version %d.", config.Version))
	}
	if duration, err := time.ParseDuration(config.WorkerLeaseDuration); err != nil || duration <= 0 {
		return invalid("invalid_configuration", "worker_lease_duration must be a positive Go duration such as 5m.")
	}
	names := make(map[string]struct{}, len(config.Validation.Commands))
	for index, command := range config.Validation.Commands {
		if command.Name == "" {
			return invalid("invalid_configuration", fmt.Sprintf("Validation command %d has no name.", index+1))
		}
		if _, exists := names[command.Name]; exists {
			return invalid("invalid_configuration", fmt.Sprintf("Validation command name %q is duplicated.", command.Name))
		}
		names[command.Name] = struct{}{}
		if (len(command.Argv) == 0) == (strings.TrimSpace(command.Script) == "") {
			return invalid("invalid_configuration", fmt.Sprintf("Validation command %q must define exactly one of argv or script.", command.Name))
		}
		for _, argument := range command.Argv {
			if argument == "" {
				return invalid("invalid_configuration", fmt.Sprintf("Validation command %q contains an empty argument.", command.Name))
			}
		}
		if filepath.IsAbs(command.WorkingDirectory) {
			return invalid("invalid_configuration", fmt.Sprintf("Validation command %q has an invalid working directory.", command.Name))
		}
		workingDirectory := command.WorkingDirectory
		if workingDirectory == "" {
			workingDirectory = "."
		}
		workingDirectoryPath := filepath.FromSlash(workingDirectory)
		if filepath.ToSlash(filepath.Clean(workingDirectoryPath)) != workingDirectory {
			return invalid("invalid_configuration", fmt.Sprintf("Validation command %q working directory is not canonical.", command.Name))
		}
		workDir := filepath.Clean(filepath.Join(root, workingDirectoryPath))
		relative, err := filepath.Rel(root, workDir)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return invalid("invalid_configuration", fmt.Sprintf("Validation command %q working directory escapes the repository.", command.Name))
		}
		info, err := os.Stat(workDir)
		if err != nil || !info.IsDir() {
			return invalid("invalid_configuration", fmt.Sprintf("Validation command %q working directory does not exist.", command.Name))
		}
		resolvedRoot, rootErr := filepath.EvalSymlinks(root)
		resolvedWorkDir, workDirErr := filepath.EvalSymlinks(workDir)
		resolvedRelative, relativeErr := filepath.Rel(resolvedRoot, resolvedWorkDir)
		if rootErr != nil || workDirErr != nil || relativeErr != nil || resolvedRelative == ".." || strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator)) {
			return invalid("invalid_configuration", fmt.Sprintf("Validation command %q working directory resolves outside the repository.", command.Name))
		}
		if duration, err := time.ParseDuration(command.Timeout); err != nil || duration <= 0 {
			return invalid("invalid_configuration", fmt.Sprintf("Validation command %q has an invalid timeout.", command.Name))
		}
		for name := range command.Environment {
			if strings.TrimSpace(name) == "" || strings.Contains(name, "=") {
				return invalid("invalid_configuration", fmt.Sprintf("Validation command %q has an invalid environment name.", command.Name))
			}
		}
	}
	return nil
}

func (p *Project) openState() (*sql.DB, *Error) {
	if projectError := validateLocalPath(p.GitDir, filepath.Join("bandmaster", "state.db")); projectError != nil {
		return nil, projectError
	}
	stateDir := filepath.Join(p.GitDir, "bandmaster")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, internal("create runtime state directory", err)
	}
	stateURL := (&url.URL{Scheme: "file", Path: filepath.Join(stateDir, "state.db")}).String() + "?_txlock=immediate"
	db, err := sql.Open("sqlite3", stateURL)
	if err != nil {
		return nil, internal("open runtime state", err)
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			status TEXT NOT NULL CHECK (status IN ('active', 'paused', 'finalizing', 'completed', 'aborting', 'aborted')),
			starting_branch TEXT NOT NULL,
			starting_commit TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS one_open_session ON sessions((1)) WHERE status IN ('active', 'paused', 'finalizing', 'aborting')`,
		`CREATE TABLE IF NOT EXISTS audit_events (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			event TEXT NOT NULL,
			from_status TEXT,
			to_status TEXT NOT NULL,
			occurred_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS session_monitors (
			session_id TEXT NOT NULL REFERENCES sessions(id),
			generation INTEGER NOT NULL,
			process_id INTEGER,
			process_identity TEXT NOT NULL,
			process_start_identity TEXT,
			status TEXT NOT NULL CHECK (status IN ('starting', 'healthy', 'unhealthy', 'stopped')),
			started_at TEXT NOT NULL,
			heartbeat_at TEXT,
			last_full_scan_at TEXT,
			PRIMARY KEY(session_id, generation),
			UNIQUE(process_identity)
		)`,
		`CREATE TABLE IF NOT EXISTS integrity_violations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			kind TEXT NOT NULL,
			path TEXT NOT NULL DEFAULT '',
			observed_state_json TEXT NOT NULL,
			detected_at TEXT NOT NULL,
			recovered_at TEXT,
			recovery_confirmation TEXT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS one_unresolved_integrity_violation ON integrity_violations(session_id, kind, path) WHERE recovered_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS integrity_audit_events (
			audit_sequence INTEGER PRIMARY KEY REFERENCES audit_events(sequence),
			violation_id INTEGER NOT NULL UNIQUE REFERENCES integrity_violations(id),
			kind TEXT NOT NULL,
			path TEXT NOT NULL,
			observed_state_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS session_completion_checks (
			session_id TEXT PRIMARY KEY REFERENCES sessions(id),
			full_scan_at TEXT NOT NULL,
			monitor_stopped_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS session_abort_events (
			session_id TEXT PRIMARY KEY REFERENCES sessions(id),
			termination_confirmation TEXT NOT NULL,
			occurred_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS integrity_quarantines (
			violation_id INTEGER NOT NULL REFERENCES integrity_violations(id),
			task_id TEXT REFERENCES tasks(id),
			batch_id TEXT REFERENCES batches(id),
			previous_status TEXT NOT NULL,
			CHECK ((task_id IS NULL) != (batch_id IS NULL)),
			UNIQUE(violation_id, task_id),
			UNIQUE(violation_id, batch_id)
		)`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			creation_order INTEGER NOT NULL,
			title TEXT NOT NULL,
			intent TEXT NOT NULL,
			expected_outcome TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('planned', 'ready', 'assigned', 'editing', 'blocked', 'submitted', 'repair_pending', 'quarantined', 'committed', 'no_op', 'canceled')),
			worker_identity TEXT,
			assignment_token TEXT,
			core_frozen INTEGER NOT NULL DEFAULT 0 CHECK (core_frozen IN (0, 1)),
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(session_id, creation_order)
		)`,
		`CREATE TABLE IF NOT EXISTS task_dependencies (
			task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			prerequisite_id TEXT NOT NULL REFERENCES tasks(id),
			dependency_order INTEGER NOT NULL,
			PRIMARY KEY(task_id, prerequisite_id),
			UNIQUE(task_id, dependency_order)
		)`,
		`CREATE TABLE IF NOT EXISTS task_audit_events (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			event TEXT NOT NULL,
			from_status TEXT,
			to_status TEXT NOT NULL,
			worker_identity TEXT,
			termination_proof TEXT,
			occurred_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_recovery_events (
			task_audit_sequence INTEGER PRIMARY KEY REFERENCES task_audit_events(sequence),
			recovery_method TEXT NOT NULL CHECK (recovery_method IN ('worker_handle', 'user_confirmation')),
			user_confirmation TEXT,
			replacement_assignment_token TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS task_leases (
			task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
			status TEXT NOT NULL CHECK (status IN ('active', 'expired', 'closed')),
			duration_nanos INTEGER NOT NULL CHECK (duration_nanos > 0),
			renewed_at TEXT NOT NULL,
			expires_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS batches (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			creation_order INTEGER NOT NULL,
			base_branch TEXT NOT NULL,
			base_commit TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('collecting', 'frozen', 'validating', 'repair_pending', 'repairing', 'finalizing', 'final_validating', 'committed', 'quarantined')),
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(session_id, creation_order)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS one_collecting_batch ON batches(session_id) WHERE status = 'collecting'`,
		`CREATE TABLE IF NOT EXISTS batch_members (
			batch_id TEXT NOT NULL REFERENCES batches(id),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			membership_order INTEGER NOT NULL,
			PRIMARY KEY(batch_id, task_id),
			UNIQUE(batch_id, membership_order),
			UNIQUE(task_id)
		)`,
		`CREATE TABLE IF NOT EXISTS batch_freezes (
			batch_id TEXT PRIMARY KEY REFERENCES batches(id),
			frozen_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS batch_audit_events (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			batch_id TEXT NOT NULL REFERENCES batches(id),
			event TEXT NOT NULL,
			from_status TEXT,
			to_status TEXT NOT NULL,
			occurred_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS claims (
			session_id TEXT NOT NULL REFERENCES sessions(id),
			batch_id TEXT NOT NULL REFERENCES batches(id),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			claim_order INTEGER NOT NULL,
			path TEXT NOT NULL,
			baseline_presence TEXT NOT NULL CHECK (baseline_presence IN ('absent', 'present')),
			baseline_type TEXT NOT NULL CHECK (baseline_type IN ('absent', 'regular_file', 'symlink')),
			baseline_content_hash TEXT,
			baseline_executable INTEGER NOT NULL CHECK (baseline_executable IN (0, 1)),
			baseline_content BLOB,
			claimed_at TEXT NOT NULL,
			PRIMARY KEY(session_id, path),
			UNIQUE(task_id, path),
			UNIQUE(task_id, claim_order)
		)`,
		`CREATE TABLE IF NOT EXISTS focused_validations (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			validation_order INTEGER NOT NULL,
			name TEXT NOT NULL,
			argv_json TEXT,
			script TEXT,
			working_directory TEXT NOT NULL,
			timeout TEXT NOT NULL,
			environment_json TEXT NOT NULL,
			PRIMARY KEY(task_id, validation_order),
			UNIQUE(task_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS task_submissions (
			task_id TEXT PRIMARY KEY REFERENCES tasks(id),
			outcome TEXT NOT NULL CHECK (outcome IN ('pending_changes', 'pending_no_op')),
			no_changes INTEGER NOT NULL CHECK (no_changes IN (0, 1)),
			behavior_changed TEXT NOT NULL,
			key_decisions TEXT NOT NULL,
			validation_expectations TEXT NOT NULL,
			known_risks TEXT NOT NULL,
			submitted_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS submitted_snapshots (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			path TEXT NOT NULL,
			presence TEXT NOT NULL CHECK (presence IN ('absent', 'present')),
			file_type TEXT NOT NULL CHECK (file_type IN ('absent', 'regular_file', 'symlink')),
			content_hash TEXT,
			executable INTEGER NOT NULL CHECK (executable IN (0, 1)),
			content BLOB,
			PRIMARY KEY(task_id, path),
			FOREIGN KEY(task_id, path) REFERENCES claims(task_id, path)
		)`,
		`CREATE TABLE IF NOT EXISTS frozen_batch_paths (
			batch_id TEXT NOT NULL REFERENCES batches(id),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			membership_order INTEGER NOT NULL,
			claim_order INTEGER NOT NULL,
			path TEXT NOT NULL,
			baseline_presence TEXT NOT NULL CHECK (baseline_presence IN ('absent', 'present')),
			baseline_type TEXT NOT NULL CHECK (baseline_type IN ('absent', 'regular_file', 'symlink')),
			baseline_content_hash TEXT,
			baseline_executable INTEGER NOT NULL CHECK (baseline_executable IN (0, 1)),
			baseline_content BLOB,
			submitted_presence TEXT NOT NULL CHECK (submitted_presence IN ('absent', 'present')),
			submitted_type TEXT NOT NULL CHECK (submitted_type IN ('absent', 'regular_file', 'symlink')),
			submitted_content_hash TEXT,
			submitted_executable INTEGER NOT NULL CHECK (submitted_executable IN (0, 1)),
			submitted_content BLOB,
			PRIMARY KEY(batch_id, path),
			UNIQUE(batch_id, task_id, claim_order)
		)`,
		`CREATE TABLE IF NOT EXISTS batch_validation_attempts (
			batch_id TEXT NOT NULL REFERENCES batches(id),
			attempt INTEGER NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('running', 'passed', 'failed', 'integrity_violation')),
			started_at TEXT NOT NULL,
			finished_at TEXT,
			PRIMARY KEY(batch_id, attempt)
		)`,
		`CREATE TABLE IF NOT EXISTS batch_validation_runs (
			batch_id TEXT NOT NULL REFERENCES batches(id),
			attempt INTEGER NOT NULL,
			command_order INTEGER NOT NULL,
			source TEXT NOT NULL CHECK (source IN ('focused', 'repository')),
			task_id TEXT REFERENCES tasks(id),
			name TEXT NOT NULL,
			argv_json TEXT,
			script TEXT,
			resolved_argv_json TEXT NOT NULL,
			working_directory TEXT NOT NULL,
			resolved_working_directory TEXT NOT NULL,
			timeout TEXT NOT NULL,
			environment_overrides_json TEXT NOT NULL,
			resolved_environment_json TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('passed', 'failed', 'timed_out', 'start_failed', 'integrity_violation')),
			exit_code INTEGER,
			duration_nanos INTEGER NOT NULL,
			stdout TEXT NOT NULL,
			stderr TEXT NOT NULL,
			stdout_truncated INTEGER NOT NULL CHECK (stdout_truncated IN (0, 1)),
			stderr_truncated INTEGER NOT NULL CHECK (stderr_truncated IN (0, 1)),
			started_at TEXT NOT NULL,
			finished_at TEXT NOT NULL,
			PRIMARY KEY(batch_id, attempt, command_order),
			FOREIGN KEY(batch_id, attempt) REFERENCES batch_validation_attempts(batch_id, attempt)
		)`,
		`CREATE TABLE IF NOT EXISTS finalization_journals (
			batch_id TEXT PRIMARY KEY REFERENCES batches(id),
			session_id TEXT NOT NULL REFERENCES sessions(id),
			expected_branch TEXT NOT NULL,
			pre_batch_commit TEXT NOT NULL,
			commit_plan_json TEXT NOT NULL,
			step TEXT NOT NULL CHECK (step IN ('prepared', 'committing', 'validating')),
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_commits (
			batch_id TEXT NOT NULL REFERENCES batches(id),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			commit_sha TEXT NOT NULL,
			committed_at TEXT NOT NULL,
			PRIMARY KEY(batch_id, task_id)
		)`,
		`CREATE TABLE IF NOT EXISTS task_diff_reviews (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			path TEXT NOT NULL,
			presence TEXT NOT NULL CHECK (presence IN ('absent', 'present')),
			file_type TEXT NOT NULL CHECK (file_type IN ('absent', 'regular_file', 'symlink')),
			content_hash TEXT,
			executable INTEGER NOT NULL CHECK (executable IN (0, 1)),
			reviewed_at TEXT NOT NULL,
			PRIMARY KEY(task_id, path),
			FOREIGN KEY(task_id, path) REFERENCES claims(task_id, path)
		)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, internal("initialize runtime state", err)
		}
	}
	if projectError := migrateIntegrityMonitorSchema(db); projectError != nil {
		db.Close()
		return nil, projectError
	}
	if projectError := migratePathSnapshotSchema(db); projectError != nil {
		db.Close()
		return nil, projectError
	}
	if projectError := initializeRepairSchema(db); projectError != nil {
		db.Close()
		return nil, projectError
	}
	return db, nil
}

const repairEventsTableSQL = `CREATE TABLE IF NOT EXISTS task_repair_events (
			task_audit_sequence INTEGER PRIMARY KEY REFERENCES task_audit_events(sequence),
			diagnosis TEXT NOT NULL,
			intended_repair TEXT NOT NULL,
			recovery_method TEXT NOT NULL CHECK (recovery_method IN ('worker_handle', 'user_confirmation')),
			user_confirmation TEXT,
			replacement_assignment_token TEXT,
			invalidated_submission_json TEXT
		)`

const repairSnapshotsTableSQL = `CREATE TABLE IF NOT EXISTS task_repair_snapshots (
			task_audit_sequence INTEGER NOT NULL REFERENCES task_repair_events(task_audit_sequence),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			path TEXT NOT NULL,
			presence TEXT NOT NULL CHECK (presence IN ('absent', 'present')),
			file_type TEXT NOT NULL CHECK (file_type IN ('absent', 'regular_file', 'symlink')),
			content_hash TEXT,
			executable INTEGER NOT NULL CHECK (executable IN (0, 1)),
			content BLOB,
			invalidated_presence TEXT CHECK (invalidated_presence IN ('absent', 'present')),
			invalidated_type TEXT CHECK (invalidated_type IN ('absent', 'regular_file', 'symlink')),
			invalidated_content_hash TEXT,
			invalidated_executable INTEGER CHECK (invalidated_executable IN (0, 1)),
			invalidated_content BLOB,
			PRIMARY KEY(task_audit_sequence, path)
		)`

func initializeRepairSchema(db *sql.DB) *Error {
	if _, err := db.Exec(repairEventsTableSQL); err != nil {
		return internal("initialize task repair events", err)
	}
	found, projectError := schemaColumnExists(db, "task_repair_events", "invalidated_submission_json")
	if projectError != nil {
		return projectError
	}
	if !found {
		if _, err := db.Exec(`ALTER TABLE task_repair_events ADD COLUMN invalidated_submission_json TEXT`); err != nil {
			found, inspectError := schemaColumnExists(db, "task_repair_events", "invalidated_submission_json")
			if inspectError != nil || !found {
				return internal("migrate task repair events", err)
			}
		}
	}
	if _, err := db.Exec(repairSnapshotsTableSQL); err != nil {
		return internal("initialize task repair snapshots", err)
	}
	return migrateRepairSnapshotsSchema(db)
}

func migrateRepairSnapshotsSchema(db *sql.DB) *Error {
	var tableSQL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'task_repair_snapshots'`).Scan(&tableSQL); err != nil {
		return internal("inspect task repair snapshot schema", err)
	}
	if strings.Contains(tableSQL, "invalidated_presence") && !strings.Contains(tableSQL, "REFERENCES claims") {
		return nil
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return internal("disable foreign keys for task repair snapshot migration", err)
	}
	foreignKeysDisabled := true
	defer func() {
		if foreignKeysDisabled {
			_, _ = db.Exec(`PRAGMA foreign_keys = ON`)
		}
	}()
	tx, err := db.Begin()
	if err != nil {
		return internal("begin task repair snapshot migration", err)
	}
	defer tx.Rollback()
	var lockedTableSQL string
	if err := tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'task_repair_snapshots'`).Scan(&lockedTableSQL); err != nil {
		return internal("recheck task repair snapshot schema", err)
	}
	if strings.Contains(lockedTableSQL, "invalidated_presence") && !strings.Contains(lockedTableSQL, "REFERENCES claims") {
		if err := tx.Rollback(); err != nil {
			return internal("close completed task repair snapshot migration", err)
		}
		if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
			return internal("restore foreign keys after concurrent task repair snapshot migration", err)
		}
		foreignKeysDisabled = false
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE task_repair_snapshots RENAME TO task_repair_snapshots_legacy`); err != nil {
		return internal("rename legacy task repair snapshots", err)
	}
	if _, err := tx.Exec(repairSnapshotsTableSQL); err != nil {
		return internal("create migrated task repair snapshots", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO task_repair_snapshots(
			task_audit_sequence, task_id, path, presence, file_type, content_hash, executable, content
		)
		SELECT task_audit_sequence, task_id, path, presence, file_type, content_hash, executable, content
		FROM task_repair_snapshots_legacy`); err != nil {
		return internal("copy legacy task repair snapshots", err)
	}
	if _, err := tx.Exec(`DROP TABLE task_repair_snapshots_legacy`); err != nil {
		return internal("drop legacy task repair snapshots", err)
	}
	if err := tx.Commit(); err != nil {
		return internal("commit task repair snapshot migration", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return internal("restore foreign keys after task repair snapshot migration", err)
	}
	foreignKeysDisabled = false
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return internal("verify task repair snapshot migration", err)
	}
	defer rows.Close()
	if rows.Next() {
		return internal("verify task repair snapshot migration", errors.New("foreign key violation after migration"))
	}
	if err := rows.Err(); err != nil {
		return internal("verify task repair snapshot migration", err)
	}
	return nil
}

func schemaColumnExists(db *sql.DB, table, column string) (bool, *Error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, internal("inspect runtime state schema", err)
	}
	found := false
	for rows.Next() {
		var sequence, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&sequence, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return false, internal("read runtime state schema", err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return false, internal("close runtime state schema", err)
	}
	if err := rows.Err(); err != nil {
		return false, internal("inspect runtime state schema", err)
	}
	return found, nil
}

func migrateIntegrityMonitorSchema(db *sql.DB) *Error {
	found, projectError := integrityMonitorStartIdentityColumnExists(db)
	if projectError != nil || found {
		return projectError
	}
	if _, err := db.Exec(`ALTER TABLE session_monitors ADD COLUMN process_start_identity TEXT`); err != nil {
		// Another process may have completed the additive migration while this
		// connection waited for SQLite's schema lock.
		found, inspectError := integrityMonitorStartIdentityColumnExists(db)
		if inspectError == nil && found {
			return nil
		}
		return internal("migrate integrity monitor schema", err)
	}
	return nil
}

func integrityMonitorStartIdentityColumnExists(db *sql.DB) (bool, *Error) {
	rows, err := db.Query(`PRAGMA table_info(session_monitors)`)
	if err != nil {
		return false, internal("inspect integrity monitor schema", err)
	}
	found := false
	for rows.Next() {
		var sequence, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&sequence, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return false, internal("read integrity monitor schema", err)
		}
		if name == "process_start_identity" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, internal("inspect integrity monitor schema", err)
	}
	if err := rows.Close(); err != nil {
		return false, internal("close integrity monitor schema", err)
	}
	return found, nil
}

func migratePathSnapshotSchema(db *sql.DB) *Error {
	var claimsSQL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'claims'`).Scan(&claimsSQL); err != nil {
		return internal("inspect path snapshot schema", err)
	}
	if strings.Contains(claimsSQL, "'symlink'") {
		return nil
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return internal("disable foreign keys for path snapshot migration", err)
	}
	foreignKeysDisabled := true
	defer func() {
		if foreignKeysDisabled {
			_, _ = db.Exec(`PRAGMA foreign_keys = ON`)
		}
	}()
	tx, err := db.Begin()
	if err != nil {
		return internal("begin path snapshot migration", err)
	}
	defer tx.Rollback()
	statements := []string{
		`ALTER TABLE submitted_snapshots RENAME TO submitted_snapshots_regular_only`,
		`ALTER TABLE task_diff_reviews RENAME TO task_diff_reviews_regular_only`,
		`CREATE TABLE claims_with_symlinks (
			session_id TEXT NOT NULL REFERENCES sessions(id),
			batch_id TEXT NOT NULL REFERENCES batches(id),
			task_id TEXT NOT NULL REFERENCES tasks(id),
			claim_order INTEGER NOT NULL,
			path TEXT NOT NULL,
			baseline_presence TEXT NOT NULL CHECK (baseline_presence IN ('absent', 'present')),
			baseline_type TEXT NOT NULL CHECK (baseline_type IN ('absent', 'regular_file', 'symlink')),
			baseline_content_hash TEXT,
			baseline_executable INTEGER NOT NULL CHECK (baseline_executable IN (0, 1)),
			baseline_content BLOB,
			claimed_at TEXT NOT NULL,
			PRIMARY KEY(session_id, path),
			UNIQUE(task_id, path),
			UNIQUE(task_id, claim_order)
		)`,
		`INSERT INTO claims_with_symlinks SELECT * FROM claims`,
		`DROP TABLE claims`,
		`ALTER TABLE claims_with_symlinks RENAME TO claims`,
		`CREATE TABLE submitted_snapshots (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			path TEXT NOT NULL,
			presence TEXT NOT NULL CHECK (presence IN ('absent', 'present')),
			file_type TEXT NOT NULL CHECK (file_type IN ('absent', 'regular_file', 'symlink')),
			content_hash TEXT,
			executable INTEGER NOT NULL CHECK (executable IN (0, 1)),
			content BLOB,
			PRIMARY KEY(task_id, path),
			FOREIGN KEY(task_id, path) REFERENCES claims(task_id, path)
		)`,
		`INSERT INTO submitted_snapshots SELECT * FROM submitted_snapshots_regular_only`,
		`DROP TABLE submitted_snapshots_regular_only`,
		`CREATE TABLE task_diff_reviews (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			path TEXT NOT NULL,
			presence TEXT NOT NULL CHECK (presence IN ('absent', 'present')),
			file_type TEXT NOT NULL CHECK (file_type IN ('absent', 'regular_file', 'symlink')),
			content_hash TEXT,
			executable INTEGER NOT NULL CHECK (executable IN (0, 1)),
			reviewed_at TEXT NOT NULL,
			PRIMARY KEY(task_id, path),
			FOREIGN KEY(task_id, path) REFERENCES claims(task_id, path)
		)`,
		`INSERT INTO task_diff_reviews SELECT * FROM task_diff_reviews_regular_only`,
		`DROP TABLE task_diff_reviews_regular_only`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return internal("migrate path snapshot schema", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return internal("commit path snapshot migration", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return internal("restore foreign keys after path snapshot migration", err)
	}
	foreignKeysDisabled = false
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return internal("verify path snapshot migration", err)
	}
	defer rows.Close()
	if rows.Next() {
		return internal("verify path snapshot migration", errors.New("foreign key violation after migration"))
	}
	if err := rows.Err(); err != nil {
		return internal("verify path snapshot migration", err)
	}
	return nil
}

func writeFile(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".bandmaster-write-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func validateLocalPath(root, relative string) *Error {
	current := root
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return internal("inspect project artifact path", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return invalid("unsafe_project_path", fmt.Sprintf("Project artifact path %q traverses a symlink.", filepath.ToSlash(relative)))
		}
		if index < len(parts)-1 && !info.IsDir() {
			return invalid("unsafe_project_path", fmt.Sprintf("Project artifact parent %q is not a directory.", filepath.ToSlash(current)))
		}
		if index == len(parts)-1 && !info.Mode().IsRegular() {
			return invalid("unsafe_project_path", fmt.Sprintf("Project artifact path %q is not a regular file.", filepath.ToSlash(relative)))
		}
	}
	return nil
}

func gitOutput(dir string, args ...string) (string, error) {
	output, err := gitBytes(dir, args...)
	value := strings.TrimSuffix(string(output), "\n")
	return strings.TrimSuffix(value, "\r"), err
}

func gitBytes(dir string, args ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	return command.Output()
}

func invalid(code, message string) *Error {
	return &Error{Code: code, Message: message, ExitCode: 3}
}

func blocked(sessionID, code, message string) *Error {
	return &Error{Code: code, Message: message, Retryable: true, ExitCode: 2, SessionID: sessionID}
}

func quarantined(sessionID, code, message string) *Error {
	return &Error{Code: code, Message: message, ExitCode: 4, SessionID: sessionID}
}

func internal(action string, err error) *Error {
	return &Error{Code: "internal_error", Message: fmt.Sprintf("%s: %v", action, err), ExitCode: 1}
}
