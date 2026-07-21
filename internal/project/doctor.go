package project

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type DoctorResult struct {
	Healthy  bool            `json:"healthy"`
	Findings []DoctorFinding `json:"findings"`
}

type DoctorFinding struct {
	Code             string         `json:"code"`
	Severity         string         `json:"severity"`
	Entities         DoctorEntities `json:"entities"`
	Paths            []string       `json:"paths"`
	Evidence         any            `json:"evidence"`
	SupportedActions []string       `json:"supported_actions"`
}

type DoctorEntities struct {
	SessionID string   `json:"session_id,omitempty"`
	BatchIDs  []string `json:"batch_ids"`
	TaskIDs   []string `json:"task_ids"`
}

type doctorSession struct {
	ID, Status string
}

func (p *Project) Doctor() (DoctorResult, *Error) {
	db, projectError := p.openStateReadOnly()
	if projectError != nil {
		return DoctorResult{}, projectError
	}
	defer db.Close()
	result := DoctorResult{Healthy: true, Findings: []DoctorFinding{}}

	session, found, projectError := latestDoctorSession(db)
	if projectError != nil {
		return DoctorResult{}, projectError
	}
	if found {
		if finding, incompatible, projectError := doctorCompatibilityFinding(db, session); projectError != nil {
			return DoctorResult{}, projectError
		} else if incompatible {
			result.Findings = append(result.Findings, finding)
		}
	}
	journalFindings, rollbackPaths, projectError := doctorJournalFindings(db)
	if projectError != nil {
		return DoctorResult{}, projectError
	}
	result.Findings = append(result.Findings, journalFindings...)
	indexFinding, hasIndexFinding, projectError := p.doctorIndexFinding(session, found, rollbackPaths)
	if projectError != nil {
		return DoctorResult{}, projectError
	}
	if hasIndexFinding {
		result.Findings = append(result.Findings, indexFinding)
	}
	integrityFindings, projectError := doctorIntegrityFindings(db)
	if projectError != nil {
		return DoctorResult{}, projectError
	}
	result.Findings = append(result.Findings, integrityFindings...)
	cleanupFindings, projectError := doctorCleanupFindings(db, session)
	if projectError != nil {
		return DoctorResult{}, projectError
	}
	result.Findings = append(result.Findings, cleanupFindings...)
	result.Healthy = len(result.Findings) == 0
	return result, nil
}

// openStateReadOnly deliberately avoids openState: doctor must not create the
// state directory, switch journal modes, initialize tables, or run migrations.
func (p *Project) openStateReadOnly() (*sql.DB, *Error) {
	statePath := filepath.Join(p.GitDir, "bandmaster", "state.db")
	if projectError := validateLocalPath(p.GitDir, statePath); projectError != nil {
		return nil, projectError
	}
	if _, err := os.Stat(statePath); errors.Is(err, os.ErrNotExist) {
		return nil, invalid("state_not_initialized", "Bandmaster state is not initialized; run `bandmaster init` first.")
	} else if err != nil {
		return nil, internal("inspect runtime state", err)
	}
	stateURL := (&url.URL{Scheme: "file", Path: statePath}).String() + "?mode=ro"
	db, err := sql.Open("sqlite3", stateURL)
	if err != nil {
		return nil, internal("open runtime state read-only", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, internal("read runtime state", err)
	}
	return db, nil
}

func latestDoctorSession(db *sql.DB) (doctorSession, bool, *Error) {
	var session doctorSession
	err := db.QueryRow(`SELECT id, status FROM sessions ORDER BY created_at DESC LIMIT 1`).Scan(&session.ID, &session.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return session, false, nil
	}
	if err != nil {
		return session, false, internal("read doctor session", err)
	}
	return session, true, nil
}

func doctorCompatibilityFinding(db *sql.DB, session doctorSession) (DoctorFinding, bool, *Error) {
	batchStatus, projectError := latestBatchStatus(db, session.ID)
	if projectError != nil {
		return DoctorFinding{}, false, projectError
	}
	for _, transition := range sessionBatchTransitions {
		if transition.sessionStatus == session.Status && transition.batchStatus == batchStatus {
			return DoctorFinding{}, false, nil
		}
	}
	batchIDs := []string{}
	var batchID string
	if err := db.QueryRow(`SELECT id FROM batches WHERE session_id = ? ORDER BY creation_order DESC LIMIT 1`, session.ID).Scan(&batchID); err == nil {
		batchIDs = append(batchIDs, batchID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return DoctorFinding{}, false, sessionInternal(session.ID, "read incompatible batch identity", err)
	}
	return newDoctorFinding("incompatible_session_batch_state", "error", session.ID, batchIDs, nil, nil,
		map[string]any{"session_status": session.Status, "batch_status": displayBatchStatus(batchStatus)},
		"bandmaster integrity recover --confirmation <inspection> --json", "bandmaster session abort --dry-run --json"), true, nil
}

func doctorJournalFindings(db *sql.DB) ([]DoctorFinding, map[string]map[string]bool, *Error) {
	rows, err := db.Query(`SELECT journal.batch_id, journal.session_id, journal.step, journal.expected_branch, journal.pre_batch_commit,
		batch.id, batch.session_id, batch.status, session.id
		FROM finalization_journals journal
		LEFT JOIN batches batch ON batch.id = journal.batch_id
		LEFT JOIN sessions session ON session.id = journal.session_id
		ORDER BY journal.batch_id`)
	if err != nil {
		return nil, nil, internal("inspect finalization journals", err)
	}
	findings := []DoctorFinding{}
	type journalRecord struct {
		batchID, sessionID, step string
		supported                bool
	}
	records := []journalRecord{}
	for rows.Next() {
		var batchID, sessionID, step, branch, preCommit string
		var storedBatchID, batchSessionID, batchStatus, storedSessionID sql.NullString
		if err := rows.Scan(&batchID, &sessionID, &step, &branch, &preCommit, &storedBatchID, &batchSessionID, &batchStatus, &storedSessionID); err != nil {
			rows.Close()
			return nil, nil, internal("read finalization journal diagnosis", err)
		}
		evidence := map[string]any{"journal_step": step, "expected_branch": branch, "pre_batch_commit": preCommit, "batch_status": batchStatus.String}
		if !storedBatchID.Valid || !storedSessionID.Valid || batchSessionID.String != sessionID {
			findings = append(findings, newDoctorFinding("dangling_finalization_journal", "critical", sessionID, []string{batchID}, nil, []string{".git/bandmaster/state.db"}, evidence,
				"bandmaster doctor --json", "bandmaster session abort --dry-run --json"))
			continue
		}
		supported := (step == "prepared" || step == "committing" || step == "validating") && (batchStatus.String == "finalizing" || batchStatus.String == "final_validating")
		records = append(records, journalRecord{batchID: batchID, sessionID: sessionID, step: step, supported: supported})
		if batchStatus.String != "finalizing" && batchStatus.String != "final_validating" {
			findings = append(findings, newDoctorFinding("contradictory_finalization_journal", "error", sessionID, []string{batchID}, nil, []string{".git/bandmaster/state.db"}, evidence,
				"bandmaster finalization recover --json", "bandmaster batch abandon --reason <reason> --confirmation <inspection> --json"))
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, internal("read finalization journals", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, internal("close finalization journal diagnosis", err)
	}
	rollbackPaths := map[string]map[string]bool{}
	for _, record := range records {
		if !record.supported {
			continue
		}
		pathRows, err := db.Query(`SELECT path FROM frozen_batch_paths WHERE batch_id = ? ORDER BY path`, record.batchID)
		if err != nil {
			return nil, nil, sessionInternal(record.sessionID, "read journal-backed rollback paths", err)
		}
		paths := rollbackPaths[record.sessionID]
		if paths == nil {
			paths = map[string]bool{}
			rollbackPaths[record.sessionID] = paths
		}
		for pathRows.Next() {
			var path string
			if err := pathRows.Scan(&path); err != nil {
				pathRows.Close()
				return nil, nil, sessionInternal(record.sessionID, "read journal-backed rollback path", err)
			}
			paths[path] = true
		}
		if err := pathRows.Close(); err != nil {
			return nil, nil, sessionInternal(record.sessionID, "close journal-backed rollback paths", err)
		}
	}
	return findings, rollbackPaths, nil
}

func (p *Project) doctorIndexFinding(session doctorSession, sessionFound bool, rollbackPaths map[string]map[string]bool) (DoctorFinding, bool, *Error) {
	staged, err := gitOutput(p.Root, "diff", "--cached", "--name-only")
	if err != nil {
		return DoctorFinding{}, false, internal("inspect staged doctor paths", err)
	}
	if staged == "" {
		return DoctorFinding{}, false, nil
	}
	paths := strings.Split(staged, "\n")
	code, severity := "index_drift", "error"
	actions := []string{"bandmaster session inspect --json", "bandmaster session abort --dry-run --json"}
	journalCorrelated := sessionFound && len(rollbackPaths[session.ID]) != 0
	for _, path := range paths {
		journalCorrelated = journalCorrelated && rollbackPaths[session.ID][path]
	}
	if journalCorrelated {
		code, severity = "staged_rollback_residue", "critical"
		actions = []string{"bandmaster finalization recover --json", "bandmaster doctor --json"}
	}
	sessionID := ""
	if sessionFound {
		sessionID = session.ID
	}
	return newDoctorFinding(code, severity, sessionID, nil, nil, append([]string{".git/index"}, paths...), map[string]any{"staged_paths": paths, "journal_correlated": journalCorrelated}, actions...), true, nil
}

func doctorIntegrityFindings(db *sql.DB) ([]DoctorFinding, *Error) {
	rows, err := db.Query(`SELECT id, session_id, kind, path, observed_state_json, detected_at FROM integrity_violations WHERE recovered_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, internal("inspect unresolved integrity violations", err)
	}
	type violation struct {
		id                                  int64
		sessionID, kind, path, observed, at string
	}
	var violations []violation
	for rows.Next() {
		var current violation
		if err := rows.Scan(&current.id, &current.sessionID, &current.kind, &current.path, &current.observed, &current.at); err != nil {
			rows.Close()
			return nil, internal("read unresolved integrity violation", err)
		}
		violations = append(violations, current)
	}
	if err := rows.Close(); err != nil {
		return nil, internal("close unresolved integrity violations", err)
	}
	findings := []DoctorFinding{}
	for _, current := range violations {
		batchIDs, taskIDs, projectError := doctorQuarantinedEntities(db, current.id)
		if projectError != nil {
			return nil, projectError
		}
		var state any
		if err := json.Unmarshal([]byte(current.observed), &state); err != nil {
			state = current.observed
		}
		paths := []string{}
		if current.path != "" {
			paths = append(paths, current.path)
		}
		findings = append(findings, newDoctorFinding("unresolved_integrity_violation", "error", current.sessionID, batchIDs, taskIDs, paths,
			map[string]any{"violation_id": current.id, "kind": current.kind, "observed_state": state, "detected_at": current.at},
			"bandmaster integrity recover --confirmation <inspection> --json", "bandmaster session abort --dry-run --json"))
	}
	return findings, nil
}

func doctorQuarantinedEntities(db *sql.DB, violationID int64) ([]string, []string, *Error) {
	rows, err := db.Query(`SELECT COALESCE(batch_id, ''), COALESCE(task_id, '') FROM integrity_quarantines WHERE violation_id = ? ORDER BY batch_id, task_id`, violationID)
	if err != nil {
		return nil, nil, internal("inspect integrity quarantine entities", err)
	}
	defer rows.Close()
	batchIDs, taskIDs := []string{}, []string{}
	for rows.Next() {
		var batchID, taskID string
		if err := rows.Scan(&batchID, &taskID); err != nil {
			return nil, nil, internal("read integrity quarantine entity", err)
		}
		if batchID != "" {
			batchIDs = append(batchIDs, batchID)
		}
		if taskID != "" {
			taskIDs = append(taskIDs, taskID)
		}
	}
	return batchIDs, taskIDs, nil
}

func doctorCleanupFindings(db *sql.DB, session doctorSession) ([]DoctorFinding, *Error) {
	tables, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, internal("inspect cleanup relationships", err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			tables.Close()
			return nil, internal("read cleanup relationship table", err)
		}
		names = append(names, name)
	}
	if err := tables.Close(); err != nil {
		return nil, internal("close cleanup relationship tables", err)
	}
	findings := []DoctorFinding{}
	for _, name := range names {
		rows, err := db.Query(`SELECT "from", "to" FROM pragma_foreign_key_list(?) WHERE "table" = 'claims' ORDER BY id, seq`, name)
		if err != nil {
			return nil, internal("inspect cleanup foreign keys", err)
		}
		var relationships []map[string]string
		for rows.Next() {
			var from, to string
			if err := rows.Scan(&from, &to); err != nil {
				rows.Close()
				return nil, internal("read cleanup foreign key", err)
			}
			relationships = append(relationships, map[string]string{"from": from, "to": to})
		}
		rows.Close()
		if len(relationships) == 0 {
			continue
		}
		var count int
		if err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, quoteSQLiteIdentifier(name))).Scan(&count); err != nil {
			return nil, internal("count cleanup dependency rows", err)
		}
		if count == 0 {
			continue
		}
		findings = append(findings, newDoctorFinding("database_cleanup_blocker", "error", session.ID, nil, nil, []string{".git/bandmaster/state.db"},
			map[string]any{"dependent_table": name, "rows": count, "references": relationships},
			"bandmaster session abort --dry-run --json", "bandmaster doctor --json"))
	}
	violations, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return nil, internal("check database relationships", err)
	}
	defer violations.Close()
	for violations.Next() {
		var table, parent string
		var rowID, fkID any
		if err := violations.Scan(&table, &rowID, &parent, &fkID); err != nil {
			return nil, internal("read database relationship violation", err)
		}
		findings = append(findings, newDoctorFinding("database_cleanup_blocker", "critical", session.ID, nil, nil, []string{".git/bandmaster/state.db"},
			map[string]any{"table": table, "row_id": rowID, "parent_table": parent, "foreign_key_id": fkID}, "bandmaster doctor --json"))
	}
	return findings, nil
}

func newDoctorFinding(code, severity, sessionID string, batchIDs, taskIDs, paths []string, evidence any, actions ...string) DoctorFinding {
	if batchIDs == nil {
		batchIDs = []string{}
	}
	if taskIDs == nil {
		taskIDs = []string{}
	}
	if paths == nil {
		paths = []string{}
	}
	sort.Strings(batchIDs)
	sort.Strings(taskIDs)
	sort.Strings(paths)
	return DoctorFinding{Code: code, Severity: severity, Entities: DoctorEntities{SessionID: sessionID, BatchIDs: batchIDs, TaskIDs: taskIDs}, Paths: paths, Evidence: evidence, SupportedActions: actions}
}

func quoteSQLiteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
