package project

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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

func newMonitorIdentity() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "monitor_" + hex.EncodeToString(value), nil
}
