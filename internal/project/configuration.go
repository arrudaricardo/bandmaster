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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const generatedSkill = `---
name: bandmaster
description: Coordinate parallel Codex workers safely with Bandmaster.
---

# Bandmaster

Use the project-local 'bandmaster' CLI for orchestration. **The parent Codex agent is the sole orchestrator:** only it may create tasks, assign work, manage barriers, run validation, finalize batches, recover workers, or spawn workers. Workers must never spawn agents or run orchestration commands. Always append '--json' and make decisions from stable JSON fields, never human prose.

## Decide whether to orchestrate

First run 'bandmaster session inspect --json'. If it reports an active, paused, finalizing, or aborting session, run the strictly read-only 'bandmaster doctor --json', report its stable findings and supported actions, and offer to resume the interrupted or ongoing session; do not start workers or a replacement session automatically. Doctor diagnoses but never repairs state. Treat workers from a lost parent as quarantined until the parent-held worker handle proves termination. If no session is open, prefer Bandmaster for any independently implementable and independently testable task, including a single task, whenever delegation benefits from isolated ownership, durable audit history, or long-running work. Work normally without Bandmaster only for a truly trivial change where that lifecycle would add more cost than safety. For two or more tasks, require disjoint expected write sets before assigning workers concurrently.

Before a new session, run 'bandmaster config status --json'. If the validation configuration is not approved, report its digest and ask the user to review '.bandmaster.yaml' and run 'bandmaster config approve <digest> --json'. Never approve configuration on the user's behalf. If configuration inspection returns 'invalid_configuration', do not repeatedly run 'init': an existing configuration is validated rather than replaced. Report the exact error and ask the user to correct the configuration. In particular, an older configuration missing 'worker_lease_duration' needs an explicit positive duration such as 'worker_lease_duration: 5m'; that edit creates a new digest requiring user review and approval. After approval and with a clean repository, run 'bandmaster session start --json'.

## Parent workflow

Create a durable plan with 'bandmaster task create ... --json', including title, intent, expected outcome, and prerequisite IDs. Start every currently ready, independent task; do not impose an artificial concurrency cap. Assign each with 'bandmaster task assign <task-id> --worker <stable-worker-id> --json', retain the returned assignment token privately, and give the worker only its task ID and token.

When a worker reports a claim conflict or becomes blocked, wait for the conflicting owner to release or finish, then use 'bandmaster task requeue <task-id> --json' and assign a fresh worker. Wait for all batch members to submit or be deliberately stopped before 'bandmaster batch freeze --json'. Official repository-wide validation starts only after every batch member has stopped editing and the batch is frozen: run 'bandmaster batch validate --json', then 'bandmaster batch commit --json'. After provisional commits, preserve the existing final validation stage performed by batch commit. If finalization is interrupted, use 'bandmaster finalization recover --json' to classify it; provide '--confirmation <inspection>' only when the structured result requires operator confirmation for rollback. Do not recover by guessing or repeatedly issuing 'batch commit'. To stop a recognized nonterminal batch while preserving its edits and evidence, use 'bandmaster batch abandon --reason <reason> --confirmation <stopped-process-evidence> --json'; inspect the preserved work or abort the paused session next. For validation failures, diagnose the result and reopen only the original owning task with 'bandmaster task repair ... --diagnosis <text> --intended-repair <text> --terminated-worker <id> --termination-proof <proof> --json'; then assign its repair. Never transfer an owner's claim to another task.

If a worker handle is lost or a lease expires, keep its task quarantined. Replace it only with proof from the parent-held handle using 'task recover ... --terminated-worker ... --termination-proof ... --json', or after explicit user confirmation with 'task recover ... --user-confirmation <text> ... --json'. Never infer termination from a missing handle.

## Worker contract

A worker edits only its assigned task. It must use its token on every worker command, first claim its complete initial write set and declare focused validations before writing ('bandmaster task claim <task-id> --token <token> --path <path> --validation <json> --json'), and heartbeat during long work ('bandmaster task heartbeat <task-id> --token <token> --json'). Workers run only focused checks scoped to their owned behavior while peers share the mutable working tree. A worker-observed repository-wide failure during concurrent package movement is diagnostic only, not an official batch result. It may expand or release only its own claims. It must not run Git mutations ('git add', 'git commit', checkout, reset, stash, rebase, or branch operations), create tasks, spawn agents, or edit unclaimed paths.

Before stopping, the worker reviews its owned diff with 'bandmaster task diff <task-id> --token <token> --json', then submits a structured handoff using 'bandmaster task submit <task-id> --token <token> --behavior-changed <text> --key-decisions <text> --validation-expectations <text> --known-risks <text> --json'. It then stops editing and reports the handoff to the parent. Do not keep editing after submission or race the frozen barrier. If it cannot claim safely, it reports the blocked result and exits without writing.
`

const generatedDebugSkill = `---
name: debug-bandmaster
description: Diagnose and explain Bandmaster runtime behavior from sanitized structured evidence. Use when asked to debug, diagnose, inspect, troubleshoot, or explain Bandmaster sessions, workers, tasks, claims, leases, batches, monitors, integrity, configuration, Git state, or orchestration failures.
---

# Debug Bandmaster

Begin with the installed executable's sanitized evidence:

1. Run 'bandmaster debug --json'. Do not initialize, repair, resume, sweep, or mutate state to obtain evidence.
2. Check 'runtime.bandmaster_version', 'runtime.executable', Go/build identity, repository location, state schema, collection status, and revision stability. Report partial or best-effort evidence honestly.
3. Interpret stable diagnostic codes, affected identities, evidence, and suggested supported CLI actions. Correlate derived workers with authoritative tasks, leases, and claims; do not invent a persisted Agent entity.
4. Correlate runtime evidence with source and public-interface tests. Never request or expose assignment tokens, environment values, stored content, raw SQLite rows, arbitrary blobs, or a database dump. Use the unsafe option only with explicit authorization and only when the secret itself is necessary.

For a changing reproduction, run 'bandmaster debug --watch --json' and consume the initial snapshot, semantic change records, collection errors/recovery, and heartbeats as NDJSON. Keep the selected session pinned unless the investigation explicitly needs '--follow-latest'.

If the request is diagnosis-only, stop after explaining the evidence and likely source path. Do not edit code. If the user authorizes a fix, implement and test it, build a fresh executable with 'go build -o <temporary-path>/bandmaster ./cmd/bandmaster', reproduce using that exact executable, and verify with a fresh '<temporary-path>/bandmaster debug --json'. Never treat an old installed binary's snapshot as proof of the fix.
`

const generatedDebugSkillUI = `interface:
  display_name: "Debug Bandmaster"
  short_description: "Diagnose Bandmaster runtime behavior"
  default_prompt: "Use $debug-bandmaster to diagnose this Bandmaster runtime issue."
`

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

func (p *Project) Initialize(options InitOptions) (InitResult, *Error) {
	configPath := filepath.Join(p.Root, ".bandmaster.yaml")
	skillPath := filepath.Join(p.Root, ".agents", "skills", "bandmaster", "SKILL.md")
	debugSkillPath := filepath.Join(p.Root, ".agents", "skills", "debug-bandmaster", "SKILL.md")
	debugSkillUIPath := filepath.Join(p.Root, ".agents", "skills", "debug-bandmaster", "agents", "openai.yaml")
	if projectError := validateLocalPath(p.Root, ".bandmaster.yaml"); projectError != nil {
		return InitResult{}, projectError
	}
	if projectError := validateLocalPath(p.Root, filepath.Join(".agents", "skills", "bandmaster", "SKILL.md")); projectError != nil {
		return InitResult{}, projectError
	}
	if options.InstallDebugSkill {
		for _, path := range []string{filepath.Join(".agents", "skills", "debug-bandmaster", "SKILL.md"), filepath.Join(".agents", "skills", "debug-bandmaster", "agents", "openai.yaml")} {
			if projectError := validateLocalPath(p.Root, path); projectError != nil {
				return InitResult{}, projectError
			}
		}
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
	debugSkillResultPath := ""
	if options.InstallDebugSkill {
		if err := writeFile(debugSkillPath, []byte(generatedDebugSkill), 0o644); err != nil {
			return InitResult{}, internal("write generated Bandmaster debugging skill", err)
		}
		if err := writeFile(debugSkillUIPath, []byte(generatedDebugSkillUI), 0o644); err != nil {
			return InitResult{}, internal("write generated Bandmaster debugging skill metadata", err)
		}
		debugSkillResultPath = ".agents/skills/debug-bandmaster/SKILL.md"
	}
	return InitResult{
		ConfigPath:       ".bandmaster.yaml",
		SkillPath:        ".agents/skills/bandmaster/SKILL.md",
		DebugSkillPath:   debugSkillResultPath,
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
		return ConfigStatus{}, invalid("configuration_digest_mismatch", fmt.Sprintf("Configuration digest is %s, not %s. Review the current configuration, then run `bandmaster config approve %s --json`.", digest, expectedDigest, digest))
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
