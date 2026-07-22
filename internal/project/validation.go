package project

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const validationOutputLimit = 64 * 1024

var inheritedValidationEnvironment = []string{
	"CI",
	"HOME",
	"LANG",
	"LC_ALL",
	"PATH",
	"TEMP",
	"TMP",
	"TMPDIR",
	"TZ",
}

type BatchValidationAttempt struct {
	Attempt    int64                `json:"attempt"`
	Status     string               `json:"status"`
	StartedAt  string               `json:"started_at"`
	FinishedAt string               `json:"finished_at,omitempty"`
	Commands   []BatchValidationRun `json:"commands"`
}

type BatchValidationRun struct {
	Attempt                  int64             `json:"attempt"`
	CommandOrder             int64             `json:"command_order"`
	Source                   string            `json:"source"`
	TaskID                   string            `json:"task_id,omitempty"`
	Name                     string            `json:"name"`
	Argv                     []string          `json:"argv,omitempty"`
	Script                   string            `json:"script,omitempty"`
	ResolvedArgv             []string          `json:"resolved_argv"`
	WorkingDirectory         string            `json:"working_directory"`
	ResolvedWorkingDirectory string            `json:"resolved_working_directory"`
	Timeout                  string            `json:"timeout"`
	EnvironmentOverrides     map[string]string `json:"environment_overrides"`
	ResolvedEnvironment      map[string]string `json:"resolved_environment"`
	Status                   string            `json:"status"`
	ExitCode                 *int              `json:"exit_code"`
	DurationMilliseconds     int64             `json:"duration_milliseconds"`
	Stdout                   string            `json:"stdout"`
	Stderr                   string            `json:"stderr"`
	StdoutTruncated          bool              `json:"stdout_truncated"`
	StderrTruncated          bool              `json:"stderr_truncated"`
	StartedAt                string            `json:"started_at"`
	FinishedAt               string            `json:"finished_at"`
	durationNanos            int64
}

type officialValidationCommand struct {
	source string
	taskID string
	validationCommand
}

func (p *Project) ValidateBatch() (Batch, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return Batch{}, projectError
	}
	defer db.Close()

	session, projectError := inspectOpenSessionWithQueryer(db)
	if projectError != nil {
		return Batch{}, projectError
	}
	if session.Status != "finalizing" {
		return Batch{}, invalidSession(session.ID, "session_not_finalizing", "Official validation requires a frozen batch in a finalizing session.")
	}
	var batchID, batchStatus string
	if err := db.QueryRow(`SELECT id, status FROM batches WHERE session_id = ? ORDER BY creation_order DESC LIMIT 1`, session.ID).Scan(&batchID, &batchStatus); err != nil {
		return Batch{}, sessionInternal(session.ID, "read validation batch", err)
	}
	switch batchStatus {
	case "finalizing":
		return inspectBatch(db, batchID)
	case "validating":
		return Batch{}, blocked(session.ID, "validation_in_progress", fmt.Sprintf("Batch %s is already validating.", batchID))
	case "repair_pending":
		return Batch{}, invalidSession(session.ID, "validation_repair_required", fmt.Sprintf("Batch %s requires repair before validation can run again.", batchID))
	case "quarantined":
		return Batch{}, quarantined(session.ID, "integrity_recovery_required", fmt.Sprintf("Batch %s is quarantined and requires explicit integrity recovery.", batchID))
	case "frozen":
	default:
		return Batch{}, invalidSession(session.ID, "batch_not_frozen", fmt.Sprintf("Batch %s cannot validate from %s state.", batchID, batchStatus))
	}

	if observations, scanError := p.scanRepository(db, session); scanError != nil {
		return Batch{}, scanError
	} else if len(observations) != 0 {
		if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
			return Batch{}, projectError
		}
		return Batch{}, integrityError(session.ID, observations[0])
	}
	config, _, projectError := p.readApprovedConfiguration(db)
	if projectError != nil {
		projectError.SessionID = session.ID
		return Batch{}, projectError
	}
	commands, projectError := loadOfficialValidationCommands(db, session.ID, batchID, config)
	if projectError != nil {
		return Batch{}, projectError
	}

	attempt, projectError := beginValidationAttempt(db, session.ID, batchID)
	if projectError != nil {
		return Batch{}, projectError
	}
	for index, command := range commands {
		observations, scanError := p.scanRepository(db, session)
		if scanError != nil {
			return Batch{}, scanError
		}
		if len(observations) != 0 {
			_ = finishValidationAttempt(db, session.ID, batchID, attempt, "integrity_violation")
			if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
				return Batch{}, projectError
			}
			return Batch{}, integrityError(session.ID, observations[0])
		}

		run := p.runOfficialValidationCommand(attempt, int64(index+1), command)
		observations, scanError = p.scanRepository(db, session)
		if scanError != nil {
			return Batch{}, scanError
		}
		if len(observations) != 0 {
			run.Status = "integrity_violation"
		}
		if projectError := persistValidationRun(db, session.ID, batchID, run); projectError != nil {
			return Batch{}, projectError
		}
		if len(observations) != 0 {
			if projectError := finishValidationAttempt(db, session.ID, batchID, attempt, "integrity_violation"); projectError != nil {
				return Batch{}, projectError
			}
			if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
				return Batch{}, projectError
			}
			return Batch{}, integrityError(session.ID, observations[0])
		}
		if run.Status != "passed" {
			if projectError := failValidationAttempt(db, session.ID, batchID, attempt); projectError != nil {
				return Batch{}, projectError
			}
			if projectError := p.StartIntegrityMonitor(session.ID); projectError != nil {
				return Batch{}, projectError
			}
			return Batch{}, validationFailure(session.ID, run)
		}
	}
	if projectError := passValidationAttempt(db, session.ID, batchID, attempt); projectError != nil {
		return Batch{}, projectError
	}
	return inspectBatch(db, batchID)
}

func loadOfficialValidationCommands(db *sql.DB, sessionID, batchID string, config configuration) ([]officialValidationCommand, *Error) {
	rows, err := db.Query(`
		SELECT focused.name, focused.argv_json, focused.script, focused.working_directory, focused.timeout, focused.environment_json, task.task_id
		FROM batch_tasks task
		JOIN focused_validations focused ON focused.task_id = task.task_id
		WHERE task.batch_id = ?
		ORDER BY task.task_order, focused.validation_order`, batchID)
	if err != nil {
		return nil, sessionInternal(sessionID, "read focused validation plan", err)
	}
	var commands []officialValidationCommand
	for rows.Next() {
		var command officialValidationCommand
		var argvJSON, script sql.NullString
		var environmentJSON string
		command.source = "focused"
		if err := rows.Scan(&command.Name, &argvJSON, &script, &command.WorkingDirectory, &command.Timeout, &environmentJSON, &command.taskID); err != nil {
			rows.Close()
			return nil, sessionInternal(sessionID, "read focused validation command", err)
		}
		command.Script = script.String
		if argvJSON.Valid && json.Unmarshal([]byte(argvJSON.String), &command.Argv) != nil {
			rows.Close()
			return nil, sessionInternal(sessionID, "decode focused validation arguments", errors.New("invalid stored argument JSON"))
		}
		if err := json.Unmarshal([]byte(environmentJSON), &command.Environment); err != nil {
			rows.Close()
			return nil, sessionInternal(sessionID, "decode focused validation environment", err)
		}
		commands = append(commands, command)
	}
	if err := rows.Close(); err != nil {
		return nil, sessionInternal(sessionID, "close focused validation plan", err)
	}
	if err := rows.Err(); err != nil {
		return nil, sessionInternal(sessionID, "read focused validation plan", err)
	}
	for _, configured := range config.Validation.Commands {
		commands = append(commands, officialValidationCommand{source: "repository", validationCommand: configured})
	}
	return commands, nil
}

func beginValidationAttempt(db *sql.DB, sessionID, batchID string) (int64, *Error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, sessionInternal(sessionID, "begin official validation", err)
	}
	defer tx.Rollback()
	var sessionStatus, batchStatus string
	if err := tx.QueryRow(`SELECT session.status, batch.status FROM sessions session JOIN batches batch ON batch.session_id = session.id WHERE session.id = ? AND batch.id = ?`, sessionID, batchID).Scan(&sessionStatus, &batchStatus); err != nil {
		return 0, sessionInternal(sessionID, "verify official validation state", err)
	}
	if sessionStatus != "finalizing" || batchStatus != "frozen" {
		return 0, invalidSession(sessionID, "batch_not_frozen", fmt.Sprintf("Batch %s is %s and cannot begin official validation.", batchID, batchStatus))
	}
	var attempt int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(attempt), 0) + 1 FROM batch_validation_attempts WHERE batch_id = ?`, batchID).Scan(&attempt); err != nil {
		return 0, sessionInternal(sessionID, "choose validation attempt", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO batch_validation_attempts(batch_id, attempt, status, started_at) VALUES(?, ?, 'running', ?)`, batchID, attempt, now); err != nil {
		return 0, sessionInternal(sessionID, "record validation attempt", err)
	}
	if _, err := tx.Exec(`UPDATE batches SET status = 'validating', updated_at = ? WHERE id = ? AND status = 'frozen'`, now, batchID); err != nil {
		return 0, sessionInternal(sessionID, "start batch validation", err)
	}
	if _, err := tx.Exec(`INSERT INTO batch_audit_events(session_id, batch_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'validation_started', 'frozen', 'validating', ?)`, sessionID, batchID, now); err != nil {
		return 0, sessionInternal(sessionID, "audit validation start", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, sessionInternal(sessionID, "commit validation start", err)
	}
	return attempt, nil
}

func finishValidationAttempt(db *sql.DB, sessionID, batchID string, attempt int64, status string) *Error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE batch_validation_attempts SET status = ?, finished_at = ? WHERE batch_id = ? AND attempt = ? AND status = 'running'`, status, now, batchID, attempt); err != nil {
		return sessionInternal(sessionID, "finish validation attempt", err)
	}
	return nil
}

func failValidationAttempt(db *sql.DB, sessionID, batchID string, attempt int64) *Error {
	tx, err := db.Begin()
	if err != nil {
		return sessionInternal(sessionID, "begin validation failure", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE batch_validation_attempts SET status = 'failed', finished_at = ? WHERE batch_id = ? AND attempt = ? AND status = 'running'`, now, batchID, attempt); err != nil {
		return sessionInternal(sessionID, "record validation failure", err)
	}
	if _, err := tx.Exec(`UPDATE batches SET status = 'repair_pending', updated_at = ? WHERE id = ? AND status = 'validating'`, now, batchID); err != nil {
		return sessionInternal(sessionID, "mark batch repair pending", err)
	}
	if _, err := tx.Exec(`UPDATE sessions SET status = 'active', updated_at = ? WHERE id = ? AND status = 'finalizing'`, now, sessionID); err != nil {
		return sessionInternal(sessionID, "resume session for validation repair", err)
	}
	if _, err := tx.Exec(`INSERT INTO batch_audit_events(session_id, batch_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'validation_failed', 'validating', 'repair_pending', ?)`, sessionID, batchID, now); err != nil {
		return sessionInternal(sessionID, "audit validation failure", err)
	}
	if _, err := tx.Exec(`INSERT INTO audit_events(session_id, event, from_status, to_status, occurred_at) VALUES(?, 'validation_failed', 'finalizing', 'active', ?)`, sessionID, now); err != nil {
		return sessionInternal(sessionID, "audit validation repair", err)
	}
	if err := tx.Commit(); err != nil {
		return sessionInternal(sessionID, "commit validation failure", err)
	}
	return nil
}

func passValidationAttempt(db *sql.DB, sessionID, batchID string, attempt int64) *Error {
	tx, err := db.Begin()
	if err != nil {
		return sessionInternal(sessionID, "begin validation success", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE batch_validation_attempts SET status = 'passed', finished_at = ? WHERE batch_id = ? AND attempt = ? AND status = 'running'`, now, batchID, attempt); err != nil {
		return sessionInternal(sessionID, "record validation success", err)
	}
	if _, err := tx.Exec(`UPDATE batches SET status = 'finalizing', updated_at = ? WHERE id = ? AND status = 'validating'`, now, batchID); err != nil {
		return sessionInternal(sessionID, "advance validated batch", err)
	}
	if _, err := tx.Exec(`INSERT INTO batch_audit_events(session_id, batch_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'validation_passed', 'validating', 'finalizing', ?)`, sessionID, batchID, now); err != nil {
		return sessionInternal(sessionID, "audit validation success", err)
	}
	if err := tx.Commit(); err != nil {
		return sessionInternal(sessionID, "commit validation success", err)
	}
	return nil
}

func persistValidationRun(db *sql.DB, sessionID, batchID string, run BatchValidationRun) *Error {
	argvJSON, err := json.Marshal(run.Argv)
	if err != nil {
		return sessionInternal(sessionID, "encode validation arguments", err)
	}
	resolvedArgvJSON, err := json.Marshal(run.ResolvedArgv)
	if err != nil {
		return sessionInternal(sessionID, "encode resolved validation arguments", err)
	}
	overridesJSON, err := json.Marshal(run.EnvironmentOverrides)
	if err != nil {
		return sessionInternal(sessionID, "encode validation environment overrides", err)
	}
	environmentJSON, err := json.Marshal(run.ResolvedEnvironment)
	if err != nil {
		return sessionInternal(sessionID, "encode resolved validation environment", err)
	}
	var exitCode any
	if run.ExitCode != nil {
		exitCode = *run.ExitCode
	}
	_, err = db.Exec(`
		INSERT INTO batch_validation_runs(
			batch_id, attempt, command_order, source, task_id, name, argv_json, script, resolved_argv_json,
			working_directory, resolved_working_directory, timeout, environment_overrides_json,
			resolved_environment_json, status, exit_code, duration_nanos, stdout, stderr,
			stdout_truncated, stderr_truncated, started_at, finished_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		batchID, run.Attempt, run.CommandOrder, run.Source, nullableString(run.TaskID), run.Name,
		nullableJSON(run.Argv, argvJSON), nullableString(run.Script), resolvedArgvJSON, run.WorkingDirectory,
		run.ResolvedWorkingDirectory, run.Timeout, overridesJSON, environmentJSON, run.Status, exitCode,
		run.durationNanos, run.Stdout, run.Stderr, run.StdoutTruncated,
		run.StderrTruncated, run.StartedAt, run.FinishedAt)
	if err != nil {
		return sessionInternal(sessionID, "record validation command", err)
	}
	return nil
}

func (p *Project) runOfficialValidationCommand(attempt, order int64, command officialValidationCommand) BatchValidationRun {
	started := time.Now().UTC()
	environment := minimalValidationEnvironment(command.Environment)
	resolvedWorkDir, workDirError := p.resolveValidationWorkingDirectory(command.WorkingDirectory)
	run := BatchValidationRun{
		Attempt:                  attempt,
		CommandOrder:             order,
		Source:                   command.source,
		TaskID:                   command.taskID,
		Name:                     command.Name,
		Argv:                     command.Argv,
		Script:                   command.Script,
		WorkingDirectory:         command.WorkingDirectory,
		ResolvedWorkingDirectory: resolvedWorkDir,
		Timeout:                  command.Timeout,
		EnvironmentOverrides:     nonNilMap(command.Environment),
		ResolvedEnvironment:      environment,
		StartedAt:                started.Format(time.RFC3339Nano),
	}
	if workDirError != nil {
		run.Status = "start_failed"
		run.Stderr, run.StderrTruncated = boundedValidationText(workDirError.Error())
		finishValidationRun(&run, started)
		return run
	}
	resolvedArgv, err := resolveValidationArgv(command, environment, resolvedWorkDir)
	if err != nil {
		run.Status = "start_failed"
		run.Stderr, run.StderrTruncated = boundedValidationText(err.Error())
		finishValidationRun(&run, started)
		return run
	}
	run.ResolvedArgv = resolvedArgv
	timeout, _ := time.ParseDuration(command.Timeout)
	cmd := &exec.Cmd{
		Path:        resolvedArgv[0],
		Args:        resolvedArgv,
		Dir:         resolvedWorkDir,
		Env:         environmentList(environment),
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
	}
	stdout := &boundedValidationBuffer{limit: validationOutputLimit}
	stderr := &boundedValidationBuffer{limit: validationOutputLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		run.Status = "start_failed"
		run.Stderr, run.StderrTruncated = boundedValidationText(err.Error())
		finishValidationRun(&run, started)
		return run
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	var commandErr error
	select {
	case commandErr = <-done:
		if !timer.Stop() {
			<-timer.C
		}
	case <-timer.C:
		run.Status = "timed_out"
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		commandErr = <-done
	}
	if run.Status != "timed_out" {
		if commandErr == nil {
			exitCode := 0
			run.ExitCode = &exitCode
			run.Status = "passed"
		} else {
			run.Status = "failed"
			var exitError *exec.ExitError
			if errors.As(commandErr, &exitError) && exitError.Exited() {
				exitCode := exitError.ExitCode()
				run.ExitCode = &exitCode
			}
		}
	}
	run.Stdout, run.StdoutTruncated = stdout.result()
	run.Stderr, run.StderrTruncated = stderr.result()
	finishValidationRun(&run, started)
	return run
}

func (p *Project) resolveValidationWorkingDirectory(relative string) (string, error) {
	root, err := filepath.EvalSymlinks(p.Root)
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	workDir, err := filepath.EvalSymlinks(filepath.Join(p.Root, filepath.FromSlash(relative)))
	if err != nil {
		return "", fmt.Errorf("resolve validation working directory %s: %w", relative, err)
	}
	contained, err := filepath.Rel(root, workDir)
	if err != nil || contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("validation working directory %s resolves outside the repository", relative)
	}
	info, err := os.Stat(workDir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("validation working directory %s is not an existing repository directory", relative)
	}
	return workDir, nil
}

func finishValidationRun(run *BatchValidationRun, started time.Time) {
	finished := time.Now().UTC()
	run.durationNanos = finished.Sub(started).Nanoseconds()
	run.DurationMilliseconds = time.Duration(run.durationNanos).Milliseconds()
	run.FinishedAt = finished.Format(time.RFC3339Nano)
}

func resolveValidationArgv(command officialValidationCommand, environment map[string]string, workDir string) ([]string, error) {
	if command.Script != "" {
		if _, err := os.Stat("/bin/sh"); err != nil {
			return nil, fmt.Errorf("resolve shell for validation command %s: %w", command.Name, err)
		}
		return []string{"/bin/sh", "-c", command.Script}, nil
	}
	executable, err := lookPathInEnvironment(command.Argv[0], environment["PATH"], workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve validation command %s: %w", command.Name, err)
	}
	resolved := append([]string{executable}, command.Argv[1:]...)
	return resolved, nil
}

func lookPathInEnvironment(name, pathValue, workDir string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		candidate := name
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(workDir, candidate)
		}
		if executableFile(candidate) {
			return filepath.Clean(candidate), nil
		}
		return "", fmt.Errorf("executable %q was not found", name)
	}
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" {
			directory = workDir
		} else if !filepath.IsAbs(directory) {
			directory = filepath.Join(workDir, directory)
		}
		candidate := filepath.Join(directory, name)
		if executableFile(candidate) {
			return filepath.Clean(candidate), nil
		}
	}
	return "", fmt.Errorf("executable %q was not found in PATH", name)
}

func executableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func minimalValidationEnvironment(overrides map[string]string) map[string]string {
	environment := make(map[string]string, len(inheritedValidationEnvironment)+len(overrides))
	for _, name := range inheritedValidationEnvironment {
		if value, exists := os.LookupEnv(name); exists {
			environment[name] = value
		}
	}
	if environment["PATH"] == "" {
		environment["PATH"] = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	for name, value := range overrides {
		environment[name] = value
	}
	return environment
}

func environmentList(environment map[string]string) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+environment[name])
	}
	return result
}

func nonNilMap(value map[string]string) map[string]string {
	if value == nil {
		return map[string]string{}
	}
	return value
}

type boundedValidationBuffer struct {
	mu        sync.Mutex
	value     []byte
	limit     int
	truncated bool
}

func (b *boundedValidationBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(value)
	remaining := b.limit - len(b.value)
	if remaining > 0 {
		if len(value) > remaining {
			b.value = append(b.value, value[:remaining]...)
		} else {
			b.value = append(b.value, value...)
		}
	}
	if len(value) > remaining {
		b.truncated = true
	}
	return written, nil
}

func (b *boundedValidationBuffer) result() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.value), b.truncated
}

func boundedValidationText(value string) (string, bool) {
	if len(value) <= validationOutputLimit {
		return value, false
	}
	return value[:validationOutputLimit], true
}

func validationFailure(sessionID string, run BatchValidationRun) *Error {
	detail := fmt.Sprintf("Official validation command %s %s", run.Name, strings.ReplaceAll(run.Status, "_", " "))
	if run.ExitCode != nil {
		detail += fmt.Sprintf(" with exit code %d", *run.ExitCode)
	}
	return &Error{Code: "validation_failed", Message: detail + "; the batch requires repair and no commit was created.", ExitCode: 5, SessionID: sessionID}
}
