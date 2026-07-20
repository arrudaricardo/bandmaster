package project

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	monitorHeartbeatInterval = 500 * time.Millisecond
	monitorFullScanInterval  = 250 * time.Millisecond
	monitorHealthyTimeout    = 2 * time.Second
)

var errProcessNotRunning = errors.New("process is not running")

type IntegrityMonitor struct {
	Generation      int64  `json:"generation"`
	ProcessID       int    `json:"process_id"`
	ProcessIdentity string `json:"process_identity"`
	ProcessStartID  string `json:"process_start_identity"`
	Status          string `json:"status"`
	StartedAt       string `json:"started_at"`
	HeartbeatAt     string `json:"heartbeat_at,omitempty"`
	LastFullScanAt  string `json:"last_full_scan_at,omitempty"`
}

type IntegrityViolation struct {
	ID                   int64           `json:"id"`
	Kind                 string          `json:"kind"`
	Path                 string          `json:"path,omitempty"`
	ObservedState        json.RawMessage `json:"observed_state"`
	DetectedAt           string          `json:"detected_at"`
	RecoveredAt          string          `json:"recovered_at,omitempty"`
	RecoveryConfirmation string          `json:"recovery_confirmation,omitempty"`
}

type integrityObservation struct {
	Kind          string
	Path          string
	ObservedState any
	TaskID        string
	BatchID       string
}

// PrepareMutation is the common integrity gate for public state-changing commands.
func (p *Project) PrepareMutation(command string) *Error {
	db, projectError := p.openState()
	if projectError != nil {
		return projectError
	}
	defer db.Close()
	session, projectError := inspectOpenSessionWithQueryer(db)
	if projectError != nil {
		if projectError.Code == "session_not_active" {
			return nil
		}
		return projectError
	}
	if session.Status == "paused" {
		var kind, violationPath string
		err := db.QueryRow(`SELECT kind, path FROM integrity_violations WHERE session_id = ? AND recovered_at IS NULL ORDER BY id LIMIT 1`, session.ID).Scan(&kind, &violationPath)
		if err == nil {
			return integrityError(session.ID, integrityObservation{Kind: kind, Path: violationPath})
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return sessionInternal(session.ID, "read unresolved integrity violation", err)
		}
		if command == "session pause" || command == "session finish" {
			return nil
		}
		return invalidSession(session.ID, "session_not_active", fmt.Sprintf("Session %s is paused and has no healthy integrity monitor.", session.ID))
	}
	if session.Status == "finalizing" {
		if command == "session finish" || command == "batch freeze" || command == "batch validate" || command == "batch commit" || command == "batch finalize" {
			return nil
		}
		return invalidSession(session.ID, "session_finalizing", fmt.Sprintf("Session %s is finalizing and cannot accept another mutation.", session.ID))
	}
	if session.Status != "active" {
		return nil
	}
	monitor, projectError := inspectLatestMonitor(db, session.ID)
	if projectError != nil && projectError.Code != "monitor_unhealthy" {
		return projectError
	}
	if projectError != nil || !monitorHealthy(monitor, time.Now().UTC()) {
		observation := integrityObservation{Kind: "monitor_unhealthy", Path: ".git/bandmaster/monitor", ObservedState: map[string]any{"monitor": monitor}}
		if projectError := p.persistIntegrityViolations(session, []integrityObservation{observation}); projectError != nil {
			return projectError
		}
		return quarantined(session.ID, "monitor_unhealthy", "The session integrity monitor is not healthy; the session was paused and current work was quarantined.")
	}
	observations, projectError := p.scanRepository(db, session)
	if projectError != nil {
		return projectError
	}
	if len(observations) == 0 {
		return nil
	}
	if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
		return projectError
	}
	return integrityError(session.ID, observations[0])
}

// monitorStopped proves that a finalization monitor was deliberately stopped and
// is no longer running. A stale "stopped" record with a live process is unsafe.
func monitorStopped(monitor IntegrityMonitor) bool {
	if monitor.Status != "stopped" {
		return false
	}
	if monitor.ProcessID <= 0 {
		return true
	}
	_, err := processStartIdentity(monitor.ProcessID)
	return errors.Is(err, errProcessNotRunning)
}

func monitorHealthy(monitor IntegrityMonitor, now time.Time) bool {
	if monitor.Status != "healthy" || monitor.ProcessID <= 0 || monitor.ProcessIdentity == "" || monitor.HeartbeatAt == "" || monitor.LastFullScanAt == "" {
		return false
	}
	heartbeat, err := time.Parse(time.RFC3339Nano, monitor.HeartbeatAt)
	if err != nil || now.Sub(heartbeat) > monitorHealthyTimeout {
		return false
	}
	startIdentity, err := processStartIdentity(monitor.ProcessID)
	return err == nil && monitor.ProcessStartID != "" && startIdentity == monitor.ProcessStartID
}

func processStartIdentity(pid int) (string, error) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return "", err
	}
	if signalErr := process.Signal(syscall.Signal(0)); errors.Is(signalErr, os.ErrProcessDone) || errors.Is(signalErr, syscall.ESRCH) {
		return "", errProcessNotRunning
	} else if signalErr != nil {
		return "", signalErr
	}
	output, err := exec.Command("ps", "-ww", "-o", "lstart=", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return "", errProcessNotRunning
		}
		return "", err
	}
	identity := strings.TrimSpace(string(output))
	if identity == "" {
		return "", errProcessNotRunning
	}
	return identity, nil
}

func integrityError(sessionID string, observation integrityObservation) *Error {
	message := fmt.Sprintf("Repository integrity violation %s paused the session and quarantined affected work.", observation.Kind)
	if observation.Path != "" {
		message = fmt.Sprintf("Repository integrity violation %s at %s paused the session and quarantined affected work.", observation.Kind, observation.Path)
	}
	return quarantined(sessionID, observation.Kind, message)
}

func (p *Project) scanRepository(queryer databaseQuerier, session Session) ([]integrityObservation, *Error) {
	var observations []integrityObservation
	branch, branchErr := gitOutput(p.Root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if branchErr != nil || branch != session.StartingBranch {
		observations = append(observations, integrityObservation{Kind: "branch_drift", Path: ".git/HEAD", ObservedState: map[string]any{"expected": session.StartingBranch, "observed": branch, "attached": branchErr == nil}})
	}
	head, headErr := gitOutput(p.Root, "rev-parse", "--verify", "HEAD^{commit}")
	if headErr != nil || head != session.StartingCommit {
		observations = append(observations, integrityObservation{Kind: "head_drift", Path: ".git/HEAD", ObservedState: map[string]any{"expected": session.StartingCommit, "observed": head}})
	}
	base, baseErr := gitOutput(p.Root, "rev-parse", "--verify", "refs/heads/"+session.StartingBranch+"^{commit}")
	if baseErr != nil || base != session.StartingCommit {
		observations = append(observations, integrityObservation{Kind: "base_drift", Path: "refs/heads/" + session.StartingBranch, ObservedState: map[string]any{"branch": session.StartingBranch, "expected": session.StartingCommit, "observed": base}})
	}
	if clean, err := gitQuiet(p.Root, "diff", "--cached", "--quiet", "--exit-code"); err != nil {
		return nil, sessionInternal(session.ID, "inspect Git index", err)
	} else if !clean {
		observations = append(observations, integrityObservation{Kind: "index_drift", Path: ".git/index", ObservedState: map[string]any{"clean": false}})
	}

	changed, projectError := p.changedPaths()
	if projectError != nil {
		projectError.SessionID = session.ID
		return nil, projectError
	}
	for _, changedPath := range changed {
		var taskID, taskStatus, batchID string
		err := queryer.QueryRow(`
			SELECT claim.task_id, task.status, claim.batch_id
			FROM claims claim JOIN tasks task ON task.id = claim.task_id
			WHERE claim.session_id = ? AND claim.path = ?`, session.ID, changedPath).Scan(&taskID, &taskStatus, &batchID)
		if errors.Is(err, sql.ErrNoRows) {
			observations = append(observations, integrityObservation{Kind: "unclaimed_change", Path: changedPath, ObservedState: map[string]any{"git_visible": true, "owned": false}})
			continue
		}
		if err != nil {
			return nil, sessionInternal(session.ID, "inspect changed path ownership", err)
		}
		if taskStatus != "submitted" && taskStatus != "quarantined" {
			continue
		}
		observation, found, projectError := p.submittedPathObservation(queryer, session.ID, taskID, batchID, changedPath)
		if projectError != nil {
			return nil, projectError
		}
		if found {
			observations = append(observations, observation)
		}
	}

	rows, err := queryer.Query(`
		SELECT snapshot.task_id, claim.batch_id, snapshot.path, snapshot.presence, snapshot.file_type,
			snapshot.content_hash, snapshot.executable
		FROM submitted_snapshots snapshot
		JOIN claims claim ON claim.task_id = snapshot.task_id AND claim.path = snapshot.path
		WHERE claim.session_id = ?`, session.ID)
	if err != nil {
		return nil, sessionInternal(session.ID, "inspect submitted snapshots", err)
	}
	defer rows.Close()
	seen := make(map[string]struct{})
	for _, observation := range observations {
		if observation.Kind == "submitted_path_drift" {
			seen[observation.TaskID+"\x00"+observation.Path] = struct{}{}
		}
	}
	for rows.Next() {
		var taskID, batchID, snapshotPath string
		var expected PathSnapshot
		var contentHash sql.NullString
		var executable int
		if err := rows.Scan(&taskID, &batchID, &snapshotPath, &expected.Presence, &expected.Type, &contentHash, &executable); err != nil {
			return nil, sessionInternal(session.ID, "read submitted snapshot", err)
		}
		if _, exists := seen[taskID+"\x00"+snapshotPath]; exists {
			continue
		}
		expected.ContentHash = contentHash.String
		expected.Executable = executable != 0
		current, projectError := p.capturePath(snapshotPath)
		if projectError != nil {
			if projectError.Code == "unsupported_claim_path" {
				observations = append(observations, integrityObservation{Kind: "submitted_path_drift", Path: snapshotPath, TaskID: taskID, BatchID: batchID, ObservedState: map[string]any{"expected": expected, "observed_error": projectError.Message}})
				continue
			}
			return nil, projectError
		}
		if !snapshotsEqual(expected, current.PathSnapshot) {
			observations = append(observations, integrityObservation{Kind: "submitted_path_drift", Path: snapshotPath, TaskID: taskID, BatchID: batchID, ObservedState: map[string]any{"expected": expected, "observed": current.PathSnapshot}})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, sessionInternal(session.ID, "inspect submitted snapshots", err)
	}
	return observations, nil
}

func (p *Project) submittedPathObservation(queryer rowQuerier, sessionID, taskID, batchID, snapshotPath string) (integrityObservation, bool, *Error) {
	var expected PathSnapshot
	var contentHash sql.NullString
	var executable int
	err := queryer.QueryRow(`SELECT presence, file_type, content_hash, executable FROM submitted_snapshots WHERE task_id = ? AND path = ?`, taskID, snapshotPath).Scan(&expected.Presence, &expected.Type, &contentHash, &executable)
	if errors.Is(err, sql.ErrNoRows) {
		return integrityObservation{}, false, nil
	}
	if err != nil {
		return integrityObservation{}, false, sessionInternal(sessionID, "read submitted snapshot", err)
	}
	expected.ContentHash = contentHash.String
	expected.Executable = executable != 0
	current, projectError := p.capturePath(snapshotPath)
	if projectError != nil {
		if projectError.Code == "unsupported_claim_path" {
			return integrityObservation{Kind: "submitted_path_drift", Path: snapshotPath, TaskID: taskID, BatchID: batchID, ObservedState: map[string]any{"expected": expected, "observed_error": projectError.Message}}, true, nil
		}
		return integrityObservation{}, false, projectError
	}
	if snapshotsEqual(expected, current.PathSnapshot) {
		return integrityObservation{}, false, nil
	}
	return integrityObservation{Kind: "submitted_path_drift", Path: snapshotPath, TaskID: taskID, BatchID: batchID, ObservedState: map[string]any{"expected": expected, "observed": current.PathSnapshot}}, true, nil
}

func (p *Project) persistIntegrityViolations(session Session, observations []integrityObservation) *Error {
	db, projectError := p.openState()
	if projectError != nil {
		return projectError
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return sessionInternal(session.ID, "begin integrity quarantine", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, observation := range observations {
		encoded, err := json.Marshal(observation.ObservedState)
		if err != nil {
			return sessionInternal(session.ID, "encode integrity observation", err)
		}
		result, err := tx.Exec(`INSERT OR IGNORE INTO integrity_violations(session_id, kind, path, observed_state_json, detected_at) VALUES(?, ?, ?, ?, ?)`, session.ID, observation.Kind, observation.Path, encoded, now)
		if err != nil {
			return sessionInternal(session.ID, "record integrity violation", err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return sessionInternal(session.ID, "confirm integrity violation", err)
		}
		if inserted == 0 {
			continue
		}
		violationID, err := result.LastInsertId()
		if err != nil {
			return sessionInternal(session.ID, "read integrity violation identity", err)
		}
		auditResult, err := tx.Exec(`INSERT INTO audit_events(session_id, event, to_status, occurred_at) VALUES(?, 'integrity_violation_observed', 'paused', ?)`, session.ID, now)
		if err != nil {
			return sessionInternal(session.ID, "append integrity violation audit", err)
		}
		auditSequence, err := auditResult.LastInsertId()
		if err != nil {
			return sessionInternal(session.ID, "read integrity audit identity", err)
		}
		if _, err := tx.Exec(`INSERT INTO integrity_audit_events(audit_sequence, violation_id, kind, path, observed_state_json) VALUES(?, ?, ?, ?, ?)`, auditSequence, violationID, observation.Kind, observation.Path, encoded); err != nil {
			return sessionInternal(session.ID, "record integrity audit evidence", err)
		}
		if observation.TaskID != "" {
			if projectError := quarantineTaskForIntegrity(tx, session.ID, violationID, observation.TaskID, now); projectError != nil {
				return projectError
			}
		}
		if observation.BatchID != "" {
			if projectError := quarantineBatchForIntegrity(tx, session.ID, violationID, observation.BatchID, now); projectError != nil {
				return projectError
			}
		}
		if observation.TaskID == "" && observation.BatchID == "" {
			if projectError := quarantineCurrentBatches(tx, session.ID, violationID, now); projectError != nil {
				return projectError
			}
		}
	}
	var currentStatus string
	if err := tx.QueryRow(`SELECT status FROM sessions WHERE id = ?`, session.ID).Scan(&currentStatus); err != nil {
		return sessionInternal(session.ID, "read integrity session state", err)
	}
	if currentStatus == "active" || currentStatus == "finalizing" || currentStatus == "completed" {
		if _, err := tx.Exec(`UPDATE sessions SET status = 'paused', updated_at = ? WHERE id = ? AND status = ?`, now, session.ID, currentStatus); err != nil {
			return sessionInternal(session.ID, "pause session for integrity violation", err)
		}
		if _, err := tx.Exec(`INSERT INTO audit_events(session_id, event, from_status, to_status, occurred_at) VALUES(?, 'integrity_violation', ?, 'paused', ?)`, session.ID, currentStatus, now); err != nil {
			return sessionInternal(session.ID, "audit integrity pause", err)
		}
	}
	if _, err := tx.Exec(`UPDATE session_monitors SET status = 'unhealthy' WHERE session_id = ? AND generation = (SELECT MAX(generation) FROM session_monitors WHERE session_id = ?)`, session.ID, session.ID); err != nil {
		return sessionInternal(session.ID, "mark integrity monitor unhealthy", err)
	}
	if err := tx.Commit(); err != nil {
		return sessionInternal(session.ID, "commit integrity quarantine", err)
	}
	return nil
}

func quarantineTaskForIntegrity(tx *sql.Tx, sessionID string, violationID int64, taskID, now string) *Error {
	var status, worker string
	if err := tx.QueryRow(`SELECT status, COALESCE(worker_identity, '') FROM tasks WHERE id = ?`, taskID).Scan(&status, &worker); err != nil {
		return sessionInternal(sessionID, "read affected task", err)
	}
	if status == "quarantined" || status == "committed" || status == "no_op" || status == "canceled" {
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO integrity_quarantines(violation_id, task_id, previous_status) VALUES(?, ?, ?)`, violationID, taskID, status); err != nil {
		return sessionInternal(sessionID, "record task integrity quarantine", err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET status = 'quarantined', updated_at = ? WHERE id = ?`, now, taskID); err != nil {
		return sessionInternal(sessionID, "quarantine affected task", err)
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, worker_identity, occurred_at) VALUES(?, ?, 'integrity_violation', ?, 'quarantined', ?, ?)`, sessionID, taskID, status, nullableString(worker), now); err != nil {
		return sessionInternal(sessionID, "audit task integrity quarantine", err)
	}
	return nil
}

func quarantineBatchForIntegrity(tx *sql.Tx, sessionID string, violationID int64, batchID, now string) *Error {
	var status string
	if err := tx.QueryRow(`SELECT status FROM batches WHERE id = ?`, batchID).Scan(&status); err != nil {
		return sessionInternal(sessionID, "read affected batch", err)
	}
	if status == "quarantined" || status == "committed" {
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO integrity_quarantines(violation_id, batch_id, previous_status) VALUES(?, ?, ?)`, violationID, batchID, status); err != nil {
		return sessionInternal(sessionID, "record batch integrity quarantine", err)
	}
	if _, err := tx.Exec(`UPDATE batches SET status = 'quarantined', updated_at = ? WHERE id = ?`, now, batchID); err != nil {
		return sessionInternal(sessionID, "quarantine affected batch", err)
	}
	return nil
}

func quarantineCurrentBatches(tx *sql.Tx, sessionID string, violationID int64, now string) *Error {
	rows, err := tx.Query(`SELECT id FROM batches WHERE session_id = ? AND status != 'committed'`, sessionID)
	if err != nil {
		return sessionInternal(sessionID, "inspect affected batches", err)
	}
	var batchIDs []string
	for rows.Next() {
		var batchID string
		if err := rows.Scan(&batchID); err != nil {
			rows.Close()
			return sessionInternal(sessionID, "read affected batch", err)
		}
		batchIDs = append(batchIDs, batchID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return sessionInternal(sessionID, "inspect affected batches", err)
	}
	if err := rows.Close(); err != nil {
		return sessionInternal(sessionID, "close affected batch scan", err)
	}
	for _, batchID := range batchIDs {
		taskRows, err := tx.Query(`SELECT task_id FROM batch_members WHERE batch_id = ?`, batchID)
		if err != nil {
			return sessionInternal(sessionID, "inspect affected batch tasks", err)
		}
		var taskIDs []string
		for taskRows.Next() {
			var taskID string
			if err := taskRows.Scan(&taskID); err != nil {
				taskRows.Close()
				return sessionInternal(sessionID, "read affected batch task", err)
			}
			taskIDs = append(taskIDs, taskID)
		}
		if err := taskRows.Err(); err != nil {
			taskRows.Close()
			return sessionInternal(sessionID, "inspect affected batch tasks", err)
		}
		if err := taskRows.Close(); err != nil {
			return sessionInternal(sessionID, "close affected batch task scan", err)
		}
		for _, taskID := range taskIDs {
			if projectError := quarantineTaskForIntegrity(tx, sessionID, violationID, taskID, now); projectError != nil {
				return projectError
			}
		}
		if projectError := quarantineBatchForIntegrity(tx, sessionID, violationID, batchID, now); projectError != nil {
			return projectError
		}
	}
	return nil
}

func (p *Project) StartIntegrityMonitor(sessionID string) *Error {
	db, projectError := p.openState()
	if projectError != nil {
		return projectError
	}
	identity, err := newMonitorIdentity()
	if err != nil {
		db.Close()
		return sessionInternal(sessionID, "generate monitor identity", err)
	}
	var generation int64
	if err := db.QueryRow(`SELECT COALESCE(MAX(generation), 0) + 1 FROM session_monitors WHERE session_id = ?`, sessionID).Scan(&generation); err != nil {
		db.Close()
		return sessionInternal(sessionID, "choose monitor generation", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO session_monitors(session_id, generation, process_identity, status, started_at) VALUES(?, ?, ?, 'starting', ?)`, sessionID, generation, identity, now); err != nil {
		db.Close()
		return sessionInternal(sessionID, "record monitor start", err)
	}
	db.Close()

	executable, err := os.Executable()
	if err != nil {
		return p.failMonitorStart(sessionID, fmt.Errorf("resolve executable: %w", err))
	}
	command := exec.Command(executable, "monitor", "run", sessionID, identity)
	command.Dir = p.Root
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return p.failMonitorStart(sessionID, err)
	}
	_ = command.Process.Release()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		db, projectError := p.openState()
		if projectError != nil {
			return projectError
		}
		monitor, inspectError := inspectLatestMonitor(db, sessionID)
		db.Close()
		if inspectError == nil && monitor.Generation == generation && monitor.Status == "healthy" && monitor.LastFullScanAt != "" {
			return nil
		}
		if inspectError == nil && (monitor.Status == "unhealthy" || monitor.Status == "stopped") {
			detail := "The integrity monitor could not establish a healthy full-scan baseline."
			db, openError := p.openState()
			if openError == nil {
				var observed string
				if err := db.QueryRow(`SELECT observed_state_json FROM integrity_violations WHERE session_id = ? AND kind = 'monitor_unhealthy' ORDER BY id DESC LIMIT 1`, sessionID).Scan(&observed); err == nil {
					detail += " Observed: " + observed
				}
				db.Close()
			}
			return quarantined(sessionID, "monitor_start_failed", detail)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return p.failMonitorStart(sessionID, errors.New("monitor health check timed out"))
}

func (p *Project) StopIntegrityMonitor(sessionID string) *Error {
	db, projectError := p.openState()
	if projectError != nil {
		return projectError
	}
	monitor, projectError := inspectLatestMonitor(db, sessionID)
	if projectError != nil {
		db.Close()
		return projectError
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE session_monitors SET status = 'stopped', heartbeat_at = ? WHERE session_id = ? AND generation = ?`, now, sessionID, monitor.Generation); err != nil {
		db.Close()
		return sessionInternal(sessionID, "stop integrity monitor", err)
	}
	db.Close()
	if monitor.ProcessID <= 0 {
		return nil
	}
	startIdentity, err := processStartIdentity(monitor.ProcessID)
	if errors.Is(err, errProcessNotRunning) {
		return nil
	}
	if err != nil {
		return invalidSession(sessionID, "monitor_liveness_unknown", fmt.Sprintf("Cannot prove whether the previous integrity monitor stopped: %v", err))
	}
	if monitor.ProcessStartID == "" {
		return invalidSession(sessionID, "monitor_liveness_unknown", "The running monitor predates persisted process-start identity; terminate it explicitly before replacement.")
	}
	if startIdentity != monitor.ProcessStartID {
		return nil
	}
	process, err := os.FindProcess(monitor.ProcessID)
	if err != nil {
		return nil
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return sessionInternal(sessionID, "terminate integrity monitor", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		currentIdentity, err := processStartIdentity(monitor.ProcessID)
		if errors.Is(err, errProcessNotRunning) || (err == nil && currentIdentity != monitor.ProcessStartID) {
			return nil
		}
		if err != nil {
			return invalidSession(sessionID, "monitor_liveness_unknown", fmt.Sprintf("Cannot prove whether the previous integrity monitor stopped: %v", err))
		}
		time.Sleep(10 * time.Millisecond)
	}
	return invalidSession(sessionID, "monitor_termination_unconfirmed", "The previous integrity monitor process is still running and cannot be safely replaced.")
}

func (p *Project) failMonitorStart(sessionID string, cause error) *Error {
	db, projectError := p.openState()
	if projectError != nil {
		return projectError
	}
	session, inspectError := inspectOpenSessionWithQueryer(db)
	db.Close()
	if inspectError != nil {
		return inspectError
	}
	if projectError := p.persistIntegrityViolations(session, []integrityObservation{{Kind: "monitor_unhealthy", Path: ".git/bandmaster/monitor", ObservedState: map[string]any{"error": cause.Error()}}}); projectError != nil {
		return projectError
	}
	return quarantined(sessionID, "monitor_start_failed", fmt.Sprintf("Start integrity monitor: %v", cause))
}

func (p *Project) RunIntegrityMonitor(sessionID, identity string) *Error {
	db, projectError := p.openState()
	if projectError != nil {
		return projectError
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	startIdentity, err := processStartIdentity(os.Getpid())
	if err != nil {
		db.Close()
		return sessionInternal(sessionID, "read monitor process identity", err)
	}
	result, err := db.Exec(`UPDATE session_monitors SET process_id = ?, process_start_identity = ?, heartbeat_at = ? WHERE session_id = ? AND process_identity = ? AND status = 'starting'`, os.Getpid(), startIdentity, now, sessionID, identity)
	if err != nil {
		db.Close()
		return sessionInternal(sessionID, "claim monitor identity", err)
	}
	if updated, _ := result.RowsAffected(); updated != 1 {
		db.Close()
		return invalidSession(sessionID, "invalid_monitor_identity", "The monitor process identity is stale or invalid.")
	}
	db.Close()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return p.failRunningMonitor(sessionID, identity, err)
	}
	defer watcher.Close()
	if err := addRepositoryWatches(watcher, p.Root, p.GitDir); err != nil {
		return p.failRunningMonitor(sessionID, identity, err)
	}
	if projectError := p.monitorScan(sessionID, identity); projectError != nil {
		return projectError
	}
	fullScanTicker := time.NewTicker(monitorFullScanInterval)
	defer fullScanTicker.Stop()
	heartbeatStop := make(chan struct{})
	heartbeatResult := make(chan *Error, 1)
	defer close(heartbeatStop)
	go func() {
		ticker := time.NewTicker(monitorHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				active, projectError := p.updateMonitorHeartbeat(sessionID, identity)
				if projectError != nil || !active {
					heartbeatResult <- projectError
					return
				}
			case <-heartbeatStop:
				return
			}
		}
	}()
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() && !pathWithin(event.Name, p.GitDir) {
					_ = addRepositoryWatches(watcher, event.Name, p.GitDir)
				}
			}
			if projectError := p.monitorScan(sessionID, identity); projectError != nil {
				return projectError
			}
		case <-fullScanTicker.C:
			if projectError := p.monitorScan(sessionID, identity); projectError != nil {
				return projectError
			}
		case projectError := <-heartbeatResult:
			return projectError
		case <-watcher.Errors:
			// Notification loss is advisory; the periodic full scan remains authoritative.
		}
	}
}

func (p *Project) monitorScan(sessionID, identity string) *Error {
	db, projectError := p.openState()
	if projectError != nil {
		return projectError
	}
	session, projectError := inspectOpenSessionWithQueryer(db)
	if projectError != nil || session.ID != sessionID || session.Status != "active" {
		_, _ = db.Exec(`UPDATE session_monitors SET status = 'stopped', heartbeat_at = ? WHERE session_id = ? AND process_identity = ?`, time.Now().UTC().Format(time.RFC3339Nano), sessionID, identity)
		db.Close()
		return invalidSession(sessionID, "monitor_session_stopped", "The monitored session is no longer active.")
	}
	if current, projectError := monitorIdentityIsCurrent(db, sessionID, identity); projectError != nil {
		db.Close()
		return projectError
	} else if !current {
		_, _ = db.Exec(`UPDATE session_monitors SET status = 'stopped' WHERE session_id = ? AND process_identity = ?`, sessionID, identity)
		db.Close()
		return invalidSession(sessionID, "stale_monitor_generation", "A newer integrity monitor generation owns this session.")
	}
	observations, scanError := p.scanRepository(db, session)
	db.Close()
	if scanError != nil {
		db, openError := p.openState()
		if openError != nil {
			return openError
		}
		current, identityError := monitorIdentityIsCurrent(db, sessionID, identity)
		db.Close()
		if identityError != nil {
			return identityError
		}
		if !current {
			return invalidSession(sessionID, "stale_monitor_generation", "A newer integrity monitor generation owns this session.")
		}
		return p.failRunningMonitor(sessionID, identity, errors.New(scanError.Message))
	}
	if len(observations) != 0 {
		db, openError := p.openState()
		if openError != nil {
			return openError
		}
		current, identityError := monitorIdentityIsCurrent(db, sessionID, identity)
		db.Close()
		if identityError != nil {
			return identityError
		}
		if !current {
			return invalidSession(sessionID, "stale_monitor_generation", "A newer integrity monitor generation owns this session.")
		}
		if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
			return projectError
		}
		if db, openError := p.openState(); openError == nil {
			_, _ = db.Exec(`UPDATE session_monitors SET status = 'stopped', heartbeat_at = ? WHERE session_id = ? AND process_identity = ?`, time.Now().UTC().Format(time.RFC3339Nano), sessionID, identity)
			db.Close()
		}
		return integrityError(sessionID, observations[0])
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	db, projectError = p.openState()
	if projectError != nil {
		return projectError
	}
	defer db.Close()
	if current, projectError := monitorIdentityIsCurrent(db, sessionID, identity); projectError != nil {
		return projectError
	} else if !current {
		return invalidSession(sessionID, "stale_monitor_generation", "A newer integrity monitor generation owns this session.")
	}
	result, err := db.Exec(`UPDATE session_monitors SET status = 'healthy', heartbeat_at = ?, last_full_scan_at = ? WHERE session_id = ? AND process_identity = ? AND status IN ('starting', 'healthy')`, now, now, sessionID, identity)
	if err != nil {
		return sessionInternal(sessionID, "record monitor full scan", err)
	}
	if updated, err := result.RowsAffected(); err != nil || updated != 1 {
		if err == nil {
			err = fmt.Errorf("updated %d monitor rows", updated)
		}
		return sessionInternal(sessionID, "confirm monitor full scan", err)
	}
	return nil
}

func (p *Project) updateMonitorHeartbeat(sessionID, identity string) (bool, *Error) {
	db, projectError := p.openState()
	if projectError != nil {
		return false, projectError
	}
	defer db.Close()
	var status string
	if err := db.QueryRow(`SELECT status FROM sessions WHERE id = ?`, sessionID).Scan(&status); err != nil {
		return false, sessionInternal(sessionID, "read monitored session", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if status != "active" {
		_, _ = db.Exec(`UPDATE session_monitors SET status = 'stopped', heartbeat_at = ? WHERE session_id = ? AND process_identity = ?`, now, sessionID, identity)
		return false, nil
	}
	if current, projectError := monitorIdentityIsCurrent(db, sessionID, identity); projectError != nil {
		return false, projectError
	} else if !current {
		_, _ = db.Exec(`UPDATE session_monitors SET status = 'stopped' WHERE session_id = ? AND process_identity = ?`, sessionID, identity)
		return false, nil
	}
	result, err := db.Exec(`UPDATE session_monitors SET heartbeat_at = ? WHERE session_id = ? AND process_identity = ? AND status = 'healthy'`, now, sessionID, identity)
	if err != nil {
		return false, sessionInternal(sessionID, "renew monitor heartbeat", err)
	}
	if updated, err := result.RowsAffected(); err != nil || updated != 1 {
		if err == nil {
			err = fmt.Errorf("updated %d monitor rows", updated)
		}
		return false, sessionInternal(sessionID, "confirm monitor heartbeat", err)
	}
	return true, nil
}

func monitorIdentityIsCurrent(queryer rowQuerier, sessionID, identity string) (bool, *Error) {
	var currentIdentity, status string
	if err := queryer.QueryRow(`SELECT process_identity, status FROM session_monitors WHERE session_id = ? ORDER BY generation DESC LIMIT 1`, sessionID).Scan(&currentIdentity, &status); err != nil {
		return false, sessionInternal(sessionID, "verify monitor generation", err)
	}
	return currentIdentity == identity && (status == "starting" || status == "healthy"), nil
}

func (p *Project) failRunningMonitor(sessionID, identity string, cause error) *Error {
	db, projectError := p.openState()
	if projectError != nil {
		return projectError
	}
	current, identityError := monitorIdentityIsCurrent(db, sessionID, identity)
	if identityError != nil {
		db.Close()
		return identityError
	}
	if !current {
		db.Close()
		return invalidSession(sessionID, "stale_monitor_generation", "A newer integrity monitor generation owns this session.")
	}
	_, _ = db.Exec(`UPDATE session_monitors SET status = 'unhealthy', heartbeat_at = ? WHERE session_id = ? AND process_identity = ?`, time.Now().UTC().Format(time.RFC3339Nano), sessionID, identity)
	session, inspectError := inspectOpenSessionWithQueryer(db)
	db.Close()
	if inspectError == nil && session.Status == "active" {
		_ = p.persistIntegrityViolations(session, []integrityObservation{{Kind: "monitor_unhealthy", Path: ".git/bandmaster/monitor", ObservedState: map[string]any{"error": cause.Error()}}})
	}
	return quarantined(sessionID, "monitor_unhealthy", cause.Error())
}

func addRepositoryWatches(watcher *fsnotify.Watcher, root, gitDir string) error {
	return filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			return nil
		}
		if pathWithin(current, gitDir) {
			return filepath.SkipDir
		}
		if err := watcher.Add(current); err != nil {
			// Some kqueue implementations reject a directory containing a dangling
			// symlink. Notifications are advisory, so the full-scan ticker remains
			// authoritative when a directory cannot be watched.
			return nil
		}
		return nil
	})
}

func pathWithin(candidate, parent string) bool {
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func inspectLatestMonitor(queryer rowQuerier, sessionID string) (IntegrityMonitor, *Error) {
	var monitor IntegrityMonitor
	var processID sql.NullInt64
	var processStartID, heartbeat, fullScan sql.NullString
	err := queryer.QueryRow(`SELECT generation, process_id, process_identity, process_start_identity, status, started_at, heartbeat_at, last_full_scan_at FROM session_monitors WHERE session_id = ? ORDER BY generation DESC LIMIT 1`, sessionID).Scan(
		&monitor.Generation, &processID, &monitor.ProcessIdentity, &processStartID, &monitor.Status, &monitor.StartedAt, &heartbeat, &fullScan,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return IntegrityMonitor{}, invalidSession(sessionID, "monitor_unhealthy", "The active session has no integrity monitor.")
	}
	if err != nil {
		return IntegrityMonitor{}, sessionInternal(sessionID, "read integrity monitor", err)
	}
	monitor.ProcessID = int(processID.Int64)
	monitor.ProcessStartID = processStartID.String
	monitor.HeartbeatAt = heartbeat.String
	monitor.LastFullScanAt = fullScan.String
	return monitor, nil
}

func (p *Project) RecoverIntegrity(confirmation string) (Session, *Error) {
	if strings.TrimSpace(confirmation) == "" {
		return Session{}, invalid("recovery_confirmation_required", "Integrity recovery requires an explicit confirmation describing the inspection and restoration performed.")
	}
	db, projectError := p.openState()
	if projectError != nil {
		return Session{}, projectError
	}
	defer db.Close()
	session, projectError := p.inspectOpenSession(db)
	if projectError != nil {
		return Session{}, projectError
	}
	if session.Status != "paused" {
		return Session{}, invalidSession(session.ID, "integrity_recovery_requires_paused_session", "Integrity recovery requires a paused session.")
	}
	var unresolved int
	if err := db.QueryRow(`SELECT COUNT(*) FROM integrity_violations WHERE session_id = ? AND recovered_at IS NULL`, session.ID).Scan(&unresolved); err != nil {
		return Session{}, sessionInternal(session.ID, "inspect unresolved integrity violations", err)
	}
	if unresolved == 0 {
		return Session{}, invalidSession(session.ID, "no_integrity_violation", "The paused session has no unresolved integrity violation.")
	}
	observations, projectError := p.scanRepository(db, session)
	if projectError != nil {
		return Session{}, projectError
	}
	if len(observations) != 0 {
		if projectError := p.persistIntegrityViolations(session, observations); projectError != nil {
			return Session{}, projectError
		}
		return Session{}, invalidSession(session.ID, "integrity_not_restored", "Repository integrity is still inconsistent; restore every observed violation before recovery.")
	}
	if projectError := p.StopIntegrityMonitor(session.ID); projectError != nil {
		return Session{}, projectError
	}

	tx, err := db.Begin()
	if err != nil {
		return Session{}, sessionInternal(session.ID, "begin integrity recovery", err)
	}
	defer tx.Rollback()
	type quarantine struct {
		violationID   int64
		taskID        sql.NullString
		batchID       sql.NullString
		previousState string
	}
	rows, err := tx.Query(`
		SELECT quarantine.violation_id, quarantine.task_id, quarantine.batch_id, quarantine.previous_status
		FROM integrity_quarantines quarantine
		JOIN integrity_violations violation ON violation.id = quarantine.violation_id
		WHERE violation.session_id = ? AND violation.recovered_at IS NULL
		ORDER BY quarantine.violation_id`, session.ID)
	if err != nil {
		return Session{}, sessionInternal(session.ID, "read integrity quarantines", err)
	}
	var quarantines []quarantine
	for rows.Next() {
		var current quarantine
		if err := rows.Scan(&current.violationID, &current.taskID, &current.batchID, &current.previousState); err != nil {
			rows.Close()
			return Session{}, sessionInternal(session.ID, "read integrity quarantine", err)
		}
		quarantines = append(quarantines, current)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Session{}, sessionInternal(session.ID, "read integrity quarantines", err)
	}
	if err := rows.Close(); err != nil {
		return Session{}, sessionInternal(session.ID, "close integrity quarantines", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	restoreFinalizing := false
	for _, current := range quarantines {
		if current.taskID.Valid {
			if _, err := tx.Exec(`UPDATE tasks SET status = ?, updated_at = ? WHERE id = ? AND status = 'quarantined'`, current.previousState, now, current.taskID.String); err != nil {
				return Session{}, sessionInternal(session.ID, "restore task after integrity recovery", err)
			}
			if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'integrity_recovered', 'quarantined', ?, ?)`, session.ID, current.taskID.String, current.previousState, now); err != nil {
				return Session{}, sessionInternal(session.ID, "audit task integrity recovery", err)
			}
		}
		if current.batchID.Valid {
			restoredStatus := current.previousState
			if restoredStatus == "validating" {
				restoredStatus = "frozen"
			}
			if restoredStatus == "frozen" {
				restoreFinalizing = true
			}
			if _, err := tx.Exec(`UPDATE batches SET status = ?, updated_at = ? WHERE id = ? AND status = 'quarantined'`, restoredStatus, now, current.batchID.String); err != nil {
				return Session{}, sessionInternal(session.ID, "restore batch after integrity recovery", err)
			}
			if _, err := tx.Exec(`INSERT INTO batch_audit_events(session_id, batch_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'integrity_recovered', 'quarantined', ?, ?)`, session.ID, current.batchID.String, restoredStatus, now); err != nil {
				return Session{}, sessionInternal(session.ID, "audit batch integrity recovery", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE integrity_violations SET recovered_at = ?, recovery_confirmation = ? WHERE session_id = ? AND recovered_at IS NULL`, now, confirmation, session.ID); err != nil {
		return Session{}, sessionInternal(session.ID, "resolve integrity violations", err)
	}
	restoredSessionStatus := "paused"
	if restoreFinalizing {
		restoredSessionStatus = "finalizing"
		if _, err := tx.Exec(`UPDATE sessions SET status = 'finalizing', updated_at = ? WHERE id = ? AND status = 'paused'`, now, session.ID); err != nil {
			return Session{}, sessionInternal(session.ID, "restore batch finalization after integrity recovery", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO audit_events(session_id, event, from_status, to_status, occurred_at) VALUES(?, 'integrity_recovered', 'paused', ?, ?)`, session.ID, restoredSessionStatus, now); err != nil {
		return Session{}, sessionInternal(session.ID, "audit integrity recovery", err)
	}
	if err := tx.Commit(); err != nil {
		return Session{}, sessionInternal(session.ID, "commit integrity recovery", err)
	}
	return p.inspectSession(db, session.ID)
}

func newMonitorIdentity() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "monitor_" + hex.EncodeToString(value), nil
}
