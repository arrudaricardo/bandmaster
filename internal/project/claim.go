package project

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type PathSnapshot struct {
	Presence    string `json:"presence"`
	Type        string `json:"type"`
	ContentHash string `json:"content_hash,omitempty"`
	Executable  bool   `json:"executable"`
}

type Claim struct {
	Path              string        `json:"path"`
	Baseline          PathSnapshot  `json:"baseline"`
	SubmittedSnapshot *PathSnapshot `json:"submitted_snapshot,omitempty"`
}

type FocusedValidation struct {
	Name             string            `json:"name"`
	Argv             []string          `json:"argv,omitempty"`
	Script           string            `json:"script,omitempty"`
	WorkingDirectory string            `json:"working_directory"`
	Timeout          string            `json:"timeout"`
	Environment      map[string]string `json:"environment,omitempty"`
}

type ClaimRequest struct {
	AssignmentToken   string
	Paths             []string
	FocusedValidation []FocusedValidation
}

type PreflightResult struct {
	SessionID         string              `json:"-"`
	TaskID            string              `json:"task_id"`
	AssignmentValid   bool                `json:"assignment_valid"`
	RepositoryChanged bool                `json:"repository_changed"`
	Paths             []Claim             `json:"paths"`
	FocusedValidation []FocusedValidation `json:"focused_validation"`
}

type capturedSnapshot struct {
	Path string
	PathSnapshot
	content []byte
}

type pathSemantics struct {
	caseFold             bool
	unicodeNormalization bool
	probeDevice          uint64
}

type databaseQuerier interface {
	rowQuerier
	Query(query string, args ...any) (*sql.Rows, error)
}

func (p *Project) PreflightTask(id string, request ClaimRequest) (PreflightResult, *Error) {
	normalizeFocusedValidations(request.FocusedValidation)
	if projectError := p.validateClaimRequest(request); projectError != nil {
		return PreflightResult{}, projectError
	}
	if _, projectError := p.renewWorkerLease(id, request.AssignmentToken, "assigned"); projectError != nil {
		return PreflightResult{}, projectError
	}
	db, projectError := p.openState()
	if projectError != nil {
		return PreflightResult{}, projectError
	}
	defer db.Close()
	session, projectError := inspectActiveSession(db)
	if projectError != nil {
		return PreflightResult{}, projectError
	}
	if projectError := validateTaskAuthority(db, session.ID, id, request.AssignmentToken, "assigned"); projectError != nil {
		return PreflightResult{}, projectError
	}
	semantics, projectError := p.claimPathSemantics()
	if projectError != nil {
		projectError.SessionID = session.ID
		return PreflightResult{}, projectError
	}
	if projectError := validateClaimAliases(request.Paths, semantics); projectError != nil {
		projectError.SessionID = session.ID
		return PreflightResult{}, projectError
	}
	if projectError := p.validateClaimRepositoryState(db, session); projectError != nil {
		return PreflightResult{}, projectError
	}
	claims, projectError := p.captureClaims(request.Paths, semantics)
	if projectError != nil {
		projectError.SessionID = session.ID
		return PreflightResult{}, projectError
	}
	if projectError := validateClaimsAvailable(db, session.ID, request.Paths, semantics); projectError != nil {
		return PreflightResult{}, projectError
	}
	if projectError := p.validateClaimRepositoryState(db, session); projectError != nil {
		return PreflightResult{}, projectError
	}
	return PreflightResult{SessionID: session.ID, TaskID: id, AssignmentValid: true, Paths: publicClaims(claims), FocusedValidation: request.FocusedValidation}, nil
}

func (p *Project) ClaimTask(id string, request ClaimRequest) (Task, *Error) {
	normalizeFocusedValidations(request.FocusedValidation)
	if projectError := p.validateClaimRequest(request); projectError != nil {
		return Task{}, projectError
	}
	if _, projectError := p.renewWorkerLease(id, request.AssignmentToken, "assigned", "editing"); projectError != nil {
		return Task{}, projectError
	}
	db, projectError := p.openState()
	if projectError != nil {
		return Task{}, projectError
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return Task{}, internal("begin initial claim", err)
	}
	defer tx.Rollback()
	session, projectError := inspectActiveSession(tx)
	if projectError != nil {
		return Task{}, projectError
	}
	if projectError := sweepExpiredLeases(tx, session.ID, time.Now().UTC()); projectError != nil {
		return Task{}, projectError
	}
	semantics, projectError := p.claimPathSemantics()
	if projectError != nil {
		projectError.SessionID = session.ID
		return Task{}, projectError
	}
	if projectError := validateClaimAliases(request.Paths, semantics); projectError != nil {
		projectError.SessionID = session.ID
		return Task{}, projectError
	}
	if projectError := validateTaskAuthority(tx, session.ID, id, request.AssignmentToken, "assigned", "editing"); projectError != nil {
		return Task{}, projectError
	}
	var taskStatus string
	if err := tx.QueryRow(`SELECT status FROM tasks WHERE session_id = ? AND id = ?`, session.ID, id).Scan(&taskStatus); err != nil {
		return Task{}, sessionInternal(session.ID, "read task claim state", err)
	}
	if taskStatus == "editing" && len(request.FocusedValidation) != 0 {
		return Task{}, invalidSession(session.ID, "focused_validation_initial_only", "Focused validation commands must be declared with the initial write set.")
	}
	if taskStatus == "assigned" {
		prerequisites, projectError := taskPrerequisites(tx, session.ID, id)
		if projectError != nil {
			return Task{}, projectError
		}
		if readiness, projectError := taskReadiness(tx, prerequisites); projectError != nil {
			return Task{}, projectError
		} else if readiness != "ready" {
			return Task{}, blocked(session.ID, "task_not_ready", fmt.Sprintf("Task %s no longer has successful prerequisites.", id))
		}
	}
	if projectError := p.validateClaimRepositoryState(tx, session); projectError != nil {
		return Task{}, projectError
	}
	claims, projectError := p.captureClaims(request.Paths, semantics)
	if projectError != nil {
		projectError.SessionID = session.ID
		return Task{}, projectError
	}
	if projectError := p.validateClaimRepositoryState(tx, session); projectError != nil {
		return Task{}, projectError
	}
	if projectError := validateClaimsAvailable(tx, session.ID, request.Paths, semantics); projectError != nil {
		if taskStatus == "editing" {
			return Task{}, projectError
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.Exec(`UPDATE tasks SET status = 'blocked', worker_identity = NULL, assignment_token = NULL, updated_at = ? WHERE id = ? AND status = 'assigned'`, now, id); err != nil {
			return Task{}, sessionInternal(session.ID, "block task after claim contention", err)
		}
		if _, err := tx.Exec(`UPDATE task_leases SET status = 'closed' WHERE task_id = ?`, id); err != nil {
			return Task{}, sessionInternal(session.ID, "close blocked worker lease", err)
		}
		if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'task_blocked', 'assigned', 'blocked', ?)`, session.ID, id, now); err != nil {
			return Task{}, sessionInternal(session.ID, "record blocked task", err)
		}
		if err := tx.Commit(); err != nil {
			return Task{}, sessionInternal(session.ID, "commit blocked task", err)
		}
		return Task{}, projectError
	}
	if taskStatus == "editing" {
		if projectError := validateTaskBatchStatus(tx, session.ID, id, "collecting", "repairing"); projectError != nil {
			return Task{}, projectError
		}
		var batchID string
		var nextOrder int
		if err := tx.QueryRow(`SELECT batch_id FROM batch_members WHERE task_id = ?`, id).Scan(&batchID); err != nil {
			return Task{}, sessionInternal(session.ID, "read task batch for claim expansion", err)
		}
		if err := tx.QueryRow(`SELECT COALESCE(MAX(claim_order), 0) + 1 FROM claims WHERE task_id = ?`, id).Scan(&nextOrder); err != nil {
			return Task{}, sessionInternal(session.ID, "choose expanded claim order", err)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		for index, claim := range claims {
			if _, err := tx.Exec(`INSERT INTO claims(session_id, batch_id, task_id, claim_order, path, baseline_presence, baseline_type, baseline_content_hash, baseline_executable, baseline_content, claimed_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, session.ID, batchID, id, nextOrder+index, claim.Path, claim.Presence, claim.Type, nullableString(claim.ContentHash), claim.Executable, nullableBytes(claim.content), now); err != nil {
				return Task{}, sessionInternal(session.ID, "record expanded path claim", err)
			}
		}
		if _, err := tx.Exec(`UPDATE tasks SET updated_at = ? WHERE id = ? AND status = 'editing'`, now, id); err != nil {
			return Task{}, sessionInternal(session.ID, "update expanded task", err)
		}
		if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'claims_expanded', 'editing', 'editing', ?)`, session.ID, id, now); err != nil {
			return Task{}, sessionInternal(session.ID, "record claim expansion", err)
		}
		if err := tx.Commit(); err != nil {
			return Task{}, sessionInternal(session.ID, "commit claim expansion", err)
		}
		if projectError := p.PrepareMutation("post-claim expansion"); projectError != nil {
			return Task{}, projectError
		}
		return inspectTask(db, session.ID, id)
	}
	batchID, projectError := ensureCollectingBatch(tx, session)
	if projectError != nil {
		if projectError.Code == "batch_repair_in_progress" {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := tx.Exec(`UPDATE tasks SET status = 'blocked', worker_identity = NULL, assignment_token = NULL, updated_at = ? WHERE id = ? AND status = 'assigned'`, now, id); err != nil {
				return Task{}, sessionInternal(session.ID, "block unrelated task during batch repair", err)
			}
			if _, err := tx.Exec(`UPDATE task_leases SET status = 'closed' WHERE task_id = ?`, id); err != nil {
				return Task{}, sessionInternal(session.ID, "close unrelated worker lease during batch repair", err)
			}
			if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'task_blocked', 'assigned', 'blocked', ?)`, session.ID, id, now); err != nil {
				return Task{}, sessionInternal(session.ID, "record unrelated task blocked during batch repair", err)
			}
			if err := tx.Commit(); err != nil {
				return Task{}, sessionInternal(session.ID, "commit unrelated task blocked during batch repair", err)
			}
		}
		return Task{}, projectError
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var membershipOrder int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(membership_order), 0) + 1 FROM batch_members WHERE batch_id = ?`, batchID).Scan(&membershipOrder); err != nil {
		return Task{}, sessionInternal(session.ID, "choose batch membership order", err)
	}
	if _, err := tx.Exec(`INSERT INTO batch_members(batch_id, task_id, membership_order) VALUES(?, ?, ?)`, batchID, id, membershipOrder); err != nil {
		return Task{}, sessionInternal(session.ID, "join collecting batch", err)
	}
	for index, claim := range claims {
		if _, err := tx.Exec(`INSERT INTO claims(session_id, batch_id, task_id, claim_order, path, baseline_presence, baseline_type, baseline_content_hash, baseline_executable, baseline_content, claimed_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, session.ID, batchID, id, index+1, claim.Path, claim.Presence, claim.Type, nullableString(claim.ContentHash), claim.Executable, nullableBytes(claim.content), now); err != nil {
			return Task{}, sessionInternal(session.ID, "record path claim", err)
		}
	}
	for index, validation := range request.FocusedValidation {
		argvJSON, err := json.Marshal(validation.Argv)
		if err != nil {
			return Task{}, sessionInternal(session.ID, "encode focused validation arguments", err)
		}
		environmentJSON, err := json.Marshal(validation.Environment)
		if err != nil {
			return Task{}, sessionInternal(session.ID, "encode focused validation environment", err)
		}
		if _, err := tx.Exec(`INSERT INTO focused_validations(task_id, validation_order, name, argv_json, script, working_directory, timeout, environment_json) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, id, index+1, validation.Name, nullableJSON(validation.Argv, argvJSON), nullableString(validation.Script), validation.WorkingDirectory, validation.Timeout, environmentJSON); err != nil {
			return Task{}, sessionInternal(session.ID, "record focused validation", err)
		}
	}
	if _, err := tx.Exec(`UPDATE tasks SET status = 'editing', core_frozen = 1, updated_at = ? WHERE id = ? AND status = 'assigned'`, now, id); err != nil {
		return Task{}, sessionInternal(session.ID, "start task editing", err)
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'claims_acquired', 'assigned', 'editing', ?)`, session.ID, id, now); err != nil {
		return Task{}, sessionInternal(session.ID, "record initial claims", err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, sessionInternal(session.ID, "commit initial claims", err)
	}
	if projectError := p.PrepareMutation("post-initial claim"); projectError != nil {
		return Task{}, projectError
	}
	return inspectTask(db, session.ID, id)
}

func (p *Project) validateClaimRequest(request ClaimRequest) *Error {
	if strings.TrimSpace(request.AssignmentToken) == "" {
		return invalid("assignment_token_required", "The current assignment token is required.")
	}
	if len(request.Paths) == 0 {
		return invalid("claim_paths_required", "At least one exact file path is required.")
	}
	seenPaths := make(map[string]struct{}, len(request.Paths))
	for _, claimPath := range request.Paths {
		if projectError := validateClaimPath(claimPath); projectError != nil {
			return projectError
		}
		if _, exists := seenPaths[claimPath]; exists {
			return invalid("duplicate_claim_path", fmt.Sprintf("Claim path %s is duplicated.", claimPath))
		}
		for existing := range seenPaths {
			if strings.HasPrefix(claimPath, existing+"/") || strings.HasPrefix(existing, claimPath+"/") {
				return invalid("conflicting_claim_paths", fmt.Sprintf("Claim paths %s and %s cannot be acquired together because one is nested beneath the other.", existing, claimPath))
			}
		}
		seenPaths[claimPath] = struct{}{}
	}
	seenNames := make(map[string]struct{}, len(request.FocusedValidation))
	for _, validation := range request.FocusedValidation {
		if projectError := p.validateFocusedValidation(validation); projectError != nil {
			return projectError
		}
		if _, exists := seenNames[validation.Name]; exists {
			return invalid("duplicate_focused_validation", fmt.Sprintf("Focused validation name %q is duplicated.", validation.Name))
		}
		seenNames[validation.Name] = struct{}{}
	}
	return nil
}

func validateClaimPath(claimPath string) *Error {
	if !utf8.ValidString(claimPath) || claimPath == "" || filepath.IsAbs(claimPath) || strings.Contains(claimPath, `\`) || path.Clean(claimPath) != claimPath {
		return invalid("invalid_claim_path", fmt.Sprintf("Claim path %q must be a canonical UTF-8 repository-relative path using slash separators.", claimPath))
	}
	parts := strings.Split(claimPath, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return invalid("invalid_claim_path", fmt.Sprintf("Claim path %q contains an invalid segment.", claimPath))
		}
	}
	if parts[0] == ".git" {
		return invalid("invalid_claim_path", "Git metadata paths cannot be claimed.")
	}
	return nil
}

func (p *Project) validateFocusedValidation(validation FocusedValidation) *Error {
	if strings.TrimSpace(validation.Name) == "" || (len(validation.Argv) == 0) == (strings.TrimSpace(validation.Script) == "") {
		return invalid("invalid_focused_validation", "Each focused validation requires a name and exactly one of argv or script.")
	}
	for _, argument := range validation.Argv {
		if argument == "" {
			return invalid("invalid_focused_validation", fmt.Sprintf("Focused validation %q contains an empty argument.", validation.Name))
		}
	}
	if filepath.IsAbs(validation.WorkingDirectory) || path.Clean(validation.WorkingDirectory) != validation.WorkingDirectory || strings.Contains(validation.WorkingDirectory, `\`) {
		return invalid("invalid_focused_validation", fmt.Sprintf("Focused validation %q has an invalid working directory.", validation.Name))
	}
	workDir := filepath.Join(p.Root, filepath.FromSlash(validation.WorkingDirectory))
	resolvedRoot, rootErr := filepath.EvalSymlinks(p.Root)
	resolvedWorkDir, workDirErr := filepath.EvalSymlinks(workDir)
	relative, relativeErr := filepath.Rel(resolvedRoot, resolvedWorkDir)
	info, statErr := os.Stat(workDir)
	if rootErr != nil || workDirErr != nil || relativeErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || statErr != nil || !info.IsDir() {
		return invalid("invalid_focused_validation", fmt.Sprintf("Focused validation %q working directory must be an existing repository directory.", validation.Name))
	}
	if duration, err := time.ParseDuration(validation.Timeout); err != nil || duration <= 0 {
		return invalid("invalid_focused_validation", fmt.Sprintf("Focused validation %q has an invalid timeout.", validation.Name))
	}
	for name := range validation.Environment {
		if strings.TrimSpace(name) == "" || strings.Contains(name, "=") {
			return invalid("invalid_focused_validation", fmt.Sprintf("Focused validation %q has an invalid environment name.", validation.Name))
		}
	}
	return nil
}

func normalizeFocusedValidations(validations []FocusedValidation) {
	for index := range validations {
		if validations[index].WorkingDirectory == "" {
			validations[index].WorkingDirectory = "."
		}
	}
}

func validateTaskAuthority(queryer rowQuerier, sessionID, taskID, token string, requiredStatuses ...string) *Error {
	var status string
	var currentToken sql.NullString
	if err := queryer.QueryRow(`SELECT status, assignment_token FROM tasks WHERE session_id = ? AND id = ?`, sessionID, taskID).Scan(&status, &currentToken); errors.Is(err, sql.ErrNoRows) {
		return invalidSession(sessionID, "task_not_found", fmt.Sprintf("Task %s does not exist in the active session.", taskID))
	} else if err != nil {
		return sessionInternal(sessionID, "validate task authority", err)
	}
	if currentToken.String == "" || token != currentToken.String {
		return invalidSession(sessionID, "invalid_assignment_token", fmt.Sprintf("The assignment token for task %s is missing, stale, or belongs to another task.", taskID))
	}
	for _, requiredStatus := range requiredStatuses {
		if status == requiredStatus {
			return nil
		}
	}
	return invalidSession(sessionID, "invalid_task_state", fmt.Sprintf("Task %s must be %s, not %s.", taskID, strings.Join(requiredStatuses, " or "), status))
}

func (p *Project) ReleaseTaskClaims(id, assignmentToken string, paths []string) (Task, *Error) {
	if strings.TrimSpace(assignmentToken) == "" {
		return Task{}, invalid("assignment_token_required", "The current assignment token is required.")
	}
	if len(paths) == 0 {
		return Task{}, invalid("claim_paths_required", "At least one exact file path is required.")
	}
	seen := make(map[string]struct{}, len(paths))
	for _, claimPath := range paths {
		if projectError := validateClaimPath(claimPath); projectError != nil {
			return Task{}, projectError
		}
		if _, exists := seen[claimPath]; exists {
			return Task{}, invalid("duplicate_claim_path", fmt.Sprintf("Claim path %s is duplicated.", claimPath))
		}
		seen[claimPath] = struct{}{}
	}
	if _, projectError := p.renewWorkerLease(id, assignmentToken, "editing"); projectError != nil {
		return Task{}, projectError
	}
	db, projectError := p.openState()
	if projectError != nil {
		return Task{}, projectError
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return Task{}, internal("begin claim release", err)
	}
	defer tx.Rollback()
	session, projectError := inspectActiveSession(tx)
	if projectError != nil {
		return Task{}, projectError
	}
	if projectError := validateTaskAuthority(tx, session.ID, id, assignmentToken, "editing"); projectError != nil {
		return Task{}, projectError
	}
	if projectError := validateTaskBatchStatus(tx, session.ID, id, "collecting"); projectError != nil {
		return Task{}, projectError
	}
	if projectError := p.validateClaimRepositoryState(tx, session); projectError != nil {
		return Task{}, projectError
	}
	claims, projectError := loadStoredClaims(tx, session.ID, id)
	if projectError != nil {
		return Task{}, projectError
	}
	baselines := make(map[string]PathSnapshot, len(claims))
	for _, claim := range claims {
		baselines[claim.Path] = claim.Baseline.PathSnapshot
	}
	for _, claimPath := range paths {
		baseline, exists := baselines[claimPath]
		if !exists {
			return Task{}, invalidSession(session.ID, "claim_not_owned", fmt.Sprintf("Task %s does not own path %s.", id, claimPath))
		}
		current, projectError := p.capturePath(claimPath)
		if projectError != nil {
			projectError.SessionID = session.ID
			return Task{}, projectError
		}
		if !snapshotsEqual(baseline, current.PathSnapshot) {
			return Task{}, invalidSession(session.ID, "claim_changed", fmt.Sprintf("Claimed path %s cannot be released because it no longer matches its baseline.", claimPath))
		}
	}
	for _, claimPath := range paths {
		if _, err := tx.Exec(`DELETE FROM task_diff_reviews WHERE task_id = ? AND path = ?`, id, claimPath); err != nil {
			return Task{}, sessionInternal(session.ID, "delete released path review", err)
		}
		if _, err := tx.Exec(`DELETE FROM claims WHERE session_id = ? AND task_id = ? AND path = ?`, session.ID, id, claimPath); err != nil {
			return Task{}, sessionInternal(session.ID, "release path claim", err)
		}
	}
	for _, claimPath := range paths {
		current, projectError := p.capturePath(claimPath)
		if projectError != nil {
			projectError.SessionID = session.ID
			return Task{}, projectError
		}
		if !snapshotsEqual(baselines[claimPath], current.PathSnapshot) {
			return Task{}, invalidSession(session.ID, "claim_changed", fmt.Sprintf("Claimed path %s changed while its ownership was being released.", claimPath))
		}
	}
	if projectError := p.validateClaimRepositoryState(tx, session); projectError != nil {
		return Task{}, projectError
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`UPDATE tasks SET updated_at = ? WHERE id = ? AND status = 'editing'`, now, id); err != nil {
		return Task{}, sessionInternal(session.ID, "update released task", err)
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'claims_released', 'editing', 'editing', ?)`, session.ID, id, now); err != nil {
		return Task{}, sessionInternal(session.ID, "record claim release", err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, sessionInternal(session.ID, "commit claim release", err)
	}
	if projectError := p.PrepareMutation("post-claim release"); projectError != nil {
		return Task{}, projectError
	}
	return inspectTask(db, session.ID, id)
}

func validateClaimsAvailable(queryer databaseQuerier, sessionID string, paths []string, semantics pathSemantics) *Error {
	rows, err := queryer.Query(`SELECT path, task_id FROM claims WHERE session_id = ?`, sessionID)
	if err != nil {
		return sessionInternal(sessionID, "inspect claim availability", err)
	}
	defer rows.Close()
	type ownedPath struct{ path, taskID string }
	var owned []ownedPath
	for rows.Next() {
		var current ownedPath
		if err := rows.Scan(&current.path, &current.taskID); err != nil {
			return sessionInternal(sessionID, "read claim availability", err)
		}
		owned = append(owned, current)
	}
	if err := rows.Err(); err != nil {
		return sessionInternal(sessionID, "inspect claim availability", err)
	}
	for _, claimPath := range paths {
		for _, current := range owned {
			if claimPathsConflict(claimPath, current.path, semantics) {
				return blocked(sessionID, "claim_unavailable", fmt.Sprintf("Path %s conflicts with path %s claimed by task %s; no paths were claimed.", claimPath, current.path, current.taskID))
			}
		}
	}
	return nil
}

func (p *Project) captureClaims(paths []string, semantics pathSemantics) ([]capturedSnapshot, *Error) {
	indexPaths, projectError := p.gitIndexPaths()
	if projectError != nil {
		return nil, projectError
	}
	claims := make([]capturedSnapshot, 0, len(paths))
	for _, claimPath := range paths {
		if projectError := p.validateClaimPathSpelling(claimPath, indexPaths, semantics); projectError != nil {
			return nil, projectError
		}
		if projectError := p.rejectIgnoredUntrackedPath(claimPath, indexPaths); projectError != nil {
			return nil, projectError
		}
		snapshot, projectError := p.capturePath(claimPath)
		if projectError != nil {
			return nil, projectError
		}
		claims = append(claims, capturedSnapshot{Path: claimPath, PathSnapshot: snapshot.PathSnapshot, content: snapshot.content})
	}
	return claims, nil
}

func (p *Project) rejectIgnoredUntrackedPath(claimPath string, indexPaths []string) *Error {
	for _, indexPath := range indexPaths {
		if indexPath == claimPath {
			return nil
		}
	}
	ignored, err := gitQuiet(p.Root, "check-ignore", "--quiet", "--no-index", "--", claimPath)
	if err != nil {
		return internal("inspect untracked path ignore policy", err)
	}
	if ignored {
		return invalid("ignored_untracked_path", fmt.Sprintf("Ignored untracked path %s stays outside Bandmaster ownership and rollback guarantees.", claimPath))
	}
	return nil
}

func (p *Project) claimPathSemantics() (pathSemantics, *Error) {
	probeDevice, err := filesystemDevice(p.GitDir)
	if err != nil {
		return pathSemantics{}, internal("inspect filesystem probe device", err)
	}
	suffixBytes := make([]byte, 8)
	if _, err := rand.Read(suffixBytes); err != nil {
		return pathSemantics{}, internal("generate filesystem probe identity", err)
	}
	suffix := hex.EncodeToString(suffixBytes)
	caseFold, err := filesystemNamesAlias(p.GitDir, ".bandmaster-case-"+suffix+"-a", ".bandmaster-case-"+suffix+"-A")
	if err != nil {
		return pathSemantics{}, internal("detect filesystem case folding", err)
	}
	normalizes, err := filesystemNamesAlias(p.GitDir, ".bandmaster-unicode-"+suffix+"-\u00e9", ".bandmaster-unicode-"+suffix+"-e\u0301")
	if err != nil {
		return pathSemantics{}, internal("detect filesystem Unicode normalization", err)
	}
	return pathSemantics{caseFold: caseFold, unicodeNormalization: normalizes, probeDevice: probeDevice}, nil
}

func filesystemDevice(filePath string) (uint64, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("filesystem device identity is unavailable")
	}
	return uint64(stat.Dev), nil
}

func filesystemNamesAlias(directory, storedName, alternateName string) (bool, error) {
	storedPath := filepath.Join(directory, storedName)
	file, err := os.OpenFile(storedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(storedPath)
		return false, err
	}
	defer os.Remove(storedPath)
	storedInfo, err := os.Lstat(storedPath)
	if err != nil {
		return false, err
	}
	alternateInfo, err := os.Lstat(filepath.Join(directory, alternateName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(storedInfo, alternateInfo), nil
}

func validateClaimAliases(paths []string, semantics pathSemantics) *Error {
	for index, claimPath := range paths {
		for _, part := range strings.Split(claimPath, "/") {
			if semantics.componentIdentity(part) == semantics.componentIdentity(".git") {
				return invalid("invalid_claim_path", "Git metadata paths cannot be claimed.")
			}
		}
		for _, existing := range paths[:index] {
			if claimPathsConflict(claimPath, existing, semantics) {
				return invalid("alias_claim_path", fmt.Sprintf("Claim paths %s and %s alias or nest under the worktree filesystem's path rules.", existing, claimPath))
			}
		}
	}
	return nil
}

func claimPathsConflict(left, right string, semantics pathSemantics) bool {
	leftParts := strings.Split(left, "/")
	rightParts := strings.Split(right, "/")
	common := len(leftParts)
	if len(rightParts) < common {
		common = len(rightParts)
	}
	for index := 0; index < common; index++ {
		if semantics.componentIdentity(leftParts[index]) != semantics.componentIdentity(rightParts[index]) {
			return false
		}
	}
	return true
}

func (s pathSemantics) componentIdentity(value string) string {
	if s.unicodeNormalization {
		value = norm.NFC.String(value)
	}
	if s.caseFold {
		value = cases.Fold().String(value)
	}
	return value
}

func (p *Project) gitIndexPaths() ([]string, *Error) {
	output, err := gitBytes(p.Root, "ls-files", "-z")
	if err != nil {
		return nil, internal("inspect Git index paths", err)
	}
	if len(output) == 0 {
		return nil, nil
	}
	if output[len(output)-1] != 0 {
		return nil, internal("parse Git index paths", errors.New("missing NUL terminator"))
	}
	records := strings.Split(string(output[:len(output)-1]), "\x00")
	return records, nil
}

func (p *Project) validateClaimPathSpelling(claimPath string, indexPaths []string, semantics pathSemantics) *Error {
	identityMatches := make([]string, 0, 1)
	for _, indexPath := range indexPaths {
		if claimPathsConflict(claimPath, indexPath, semantics) && len(strings.Split(claimPath, "/")) == len(strings.Split(indexPath, "/")) {
			identityMatches = append(identityMatches, indexPath)
		}
	}
	if len(identityMatches) > 1 {
		return invalid("ambiguous_claim_path", fmt.Sprintf("Claim path %s aliases multiple paths in the Git index.", claimPath))
	}
	if len(identityMatches) == 1 && identityMatches[0] != claimPath {
		return invalid("noncanonical_claim_path", fmt.Sprintf("Claim path %s must use Git-index spelling %s.", claimPath, identityMatches[0]))
	}

	current := p.Root
	parts := strings.Split(claimPath, "/")
	for index, part := range parts {
		entries, err := os.ReadDir(current)
		if err != nil {
			return internal("inspect claim path spelling", err)
		}
		exact := false
		for _, entry := range entries {
			if entry.Name() == part {
				exact = true
				break
			}
		}
		next := filepath.Join(current, filepath.FromSlash(part))
		info, statErr := os.Lstat(next)
		if errors.Is(statErr, os.ErrNotExist) {
			device, err := filesystemDevice(current)
			if err != nil {
				return internal("inspect absent claim destination filesystem", err)
			}
			if device != semantics.probeDevice {
				return invalid("ambiguous_claim_path", fmt.Sprintf("Absent claim path %s is on a filesystem whose alias behavior cannot be resolved safely.", claimPath))
			}
			localSemantics, err := directoryPathSemantics(current, semantics)
			if err != nil {
				return internal("inspect absent claim destination path semantics", err)
			}
			if localSemantics.caseFold != semantics.caseFold || localSemantics.unicodeNormalization != semantics.unicodeNormalization {
				return invalid("ambiguous_claim_path", fmt.Sprintf("Absent claim path %s is beneath a directory with different alias behavior and cannot be resolved safely.", claimPath))
			}
			return nil
		}
		if statErr != nil {
			return internal("inspect claim path spelling", statErr)
		}
		if existingPathAliasesGitMetadata(current, next, info) {
			return invalid("invalid_claim_path", "Git metadata paths cannot be claimed.")
		}
		if !exact {
			prefix := strings.Join(parts[:index+1], "/")
			if !indexHasExactPrefix(indexPaths, prefix) {
				return invalid("noncanonical_claim_path", fmt.Sprintf("Claim path %s does not use the existing directory-entry spelling for %s.", claimPath, prefix))
			}
		}
		if index < len(parts)-1 && info.Mode()&os.ModeSymlink != 0 {
			return invalid("unsupported_claim_path", fmt.Sprintf("Claim path %s traverses a parent symlink.", claimPath))
		}
		current = next
	}
	return nil
}

func existingPathAliasesGitMetadata(parent, candidate string, candidateInfo os.FileInfo) bool {
	metadataInfo, err := os.Lstat(filepath.Join(parent, ".git"))
	return err == nil && os.SameFile(candidateInfo, metadataInfo) && filepath.Base(candidate) != ".git"
}

func indexHasExactPrefix(indexPaths []string, prefix string) bool {
	for _, indexPath := range indexPaths {
		if indexPath == prefix || strings.HasPrefix(indexPath, prefix+"/") {
			return true
		}
	}
	return false
}

func (p *Project) capturePath(claimPath string) (capturedSnapshot, *Error) {
	current := p.Root
	parts := strings.Split(claimPath, "/")
	for index, part := range parts {
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return capturedSnapshot{PathSnapshot: PathSnapshot{Presence: "absent", Type: "absent"}}, nil
		}
		if err != nil {
			return capturedSnapshot{}, internal("inspect claim path", err)
		}
		if index < len(parts)-1 {
			if info.Mode()&os.ModeSymlink != 0 {
				return capturedSnapshot{}, invalid("unsupported_claim_path", fmt.Sprintf("Claim path %s traverses a parent symlink.", claimPath))
			}
			if !info.IsDir() {
				return capturedSnapshot{}, invalid("unsupported_claim_path", fmt.Sprintf("Claim path %s has a parent that is not a directory.", claimPath))
			}
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(current)
			if err != nil {
				return capturedSnapshot{}, internal("read claimed symlink", err)
			}
			content := []byte(target)
			digest := sha256.Sum256(content)
			return capturedSnapshot{PathSnapshot: PathSnapshot{Presence: "present", Type: "symlink", ContentHash: "sha256:" + hex.EncodeToString(digest[:])}, content: content}, nil
		}
		if !info.Mode().IsRegular() {
			return capturedSnapshot{}, invalid("unsupported_claim_path", fmt.Sprintf("Claim path %s is not a regular file or symlink.", claimPath))
		}
		content, err := os.ReadFile(current)
		if err != nil {
			return capturedSnapshot{}, internal("read claimed file", err)
		}
		digest := sha256.Sum256(content)
		executable, projectError := p.gitVisibleExecutable(claimPath, info.Mode().Perm()&0o100 != 0)
		if projectError != nil {
			return capturedSnapshot{}, projectError
		}
		return capturedSnapshot{PathSnapshot: PathSnapshot{Presence: "present", Type: "regular_file", ContentHash: "sha256:" + hex.EncodeToString(digest[:]), Executable: executable}, content: content}, nil
	}
	return capturedSnapshot{}, invalid("invalid_claim_path", "Claim path must not be empty.")
}

func (p *Project) gitVisibleExecutable(claimPath string, filesystemExecutable bool) (bool, *Error) {
	fileMode, err := gitOutput(p.Root, "config", "--bool", "core.fileMode")
	if err != nil || fileMode != "false" {
		return filesystemExecutable, nil
	}
	output, err := gitBytes(p.Root, "--literal-pathspecs", "ls-files", "--stage", "-z", "--", claimPath)
	if err != nil {
		return false, internal("inspect tracked executable mode", err)
	}
	if len(output) == 0 {
		return filesystemExecutable, nil
	}
	recordEnd := bytes.IndexByte(output, 0)
	if recordEnd < 0 {
		return false, internal("parse tracked executable mode", errors.New("missing NUL terminator"))
	}
	record := output[:recordEnd]
	tab := bytes.IndexByte(record, '\t')
	if tab < 0 || string(record[tab+1:]) != claimPath || tab < 6 {
		return false, internal("parse tracked executable mode", errors.New("unexpected index record"))
	}
	return string(record[:6]) == "100755", nil
}

func publicClaims(snapshots []capturedSnapshot) []Claim {
	claims := make([]Claim, 0, len(snapshots))
	for _, snapshot := range snapshots {
		claims = append(claims, Claim{Path: snapshot.Path, Baseline: snapshot.PathSnapshot})
	}
	return claims
}

func (p *Project) validateClaimRepositoryState(queryer databaseQuerier, session Session) *Error {
	branch, err := gitOutput(p.Root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch != session.StartingBranch {
		return invalidSession(session.ID, "branch_drift", "The current branch does not match the active session base.")
	}
	commit, err := gitOutput(p.Root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || commit != session.StartingCommit {
		return invalidSession(session.ID, "head_drift", "The current commit does not match the active session base.")
	}
	if clean, err := gitQuiet(p.Root, "diff", "--cached", "--quiet", "--exit-code"); err != nil {
		return sessionInternal(session.ID, "inspect Git index", err)
	} else if !clean {
		return invalidSession(session.ID, "index_drift", "The Git index changed during the active session.")
	}
	changed, projectError := p.changedPaths()
	if projectError != nil {
		projectError.SessionID = session.ID
		return projectError
	}
	for _, changedPath := range changed {
		var owner string
		err := queryer.QueryRow(`SELECT task_id FROM claims WHERE session_id = ? AND path = ?`, session.ID, changedPath).Scan(&owner)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return sessionInternal(session.ID, "inspect changed path ownership", err)
		}
		return invalidSession(session.ID, "unclaimed_change", fmt.Sprintf("Changed path %s has no owning task.", changedPath))
	}
	return nil
}

func (p *Project) changedPaths() ([]string, *Error) {
	output, err := gitBytes(p.Root, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return nil, internal("inspect changed paths", err)
	}
	var changed []string
	for len(output) > 0 {
		end := strings.IndexByte(string(output), 0)
		if end < 0 {
			return nil, internal("parse changed paths", errors.New("missing NUL terminator"))
		}
		record := string(output[:end])
		output = output[end+1:]
		if len(record) < 4 {
			return nil, internal("parse changed paths", errors.New("short status record"))
		}
		changed = append(changed, record[3:])
	}
	return changed, nil
}

func ensureCollectingBatch(tx *sql.Tx, session Session) (string, *Error) {
	var repairBatchID string
	err := tx.QueryRow(`SELECT id FROM batches WHERE session_id = ? AND status IN ('repair_pending', 'repairing') ORDER BY creation_order LIMIT 1`, session.ID).Scan(&repairBatchID)
	if err == nil {
		return "", blocked(session.ID, "batch_repair_in_progress", fmt.Sprintf("Batch %s must complete repair before unrelated work can join a new batch.", repairBatchID))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", sessionInternal(session.ID, "inspect pending batch repair", err)
	}
	var id, baseBranch, baseCommit string
	err = tx.QueryRow(`SELECT id, base_branch, base_commit FROM batches WHERE session_id = ? AND status = 'collecting'`, session.ID).Scan(&id, &baseBranch, &baseCommit)
	if err == nil {
		if baseBranch != session.StartingBranch || baseCommit != session.StartingCommit {
			return "", invalidSession(session.ID, "batch_base_drift", "The collecting batch base no longer matches the active session.")
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", sessionInternal(session.ID, "inspect collecting batch", err)
	}
	id, err = newBatchID()
	if err != nil {
		return "", sessionInternal(session.ID, "generate batch identity", err)
	}
	var creationOrder int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(creation_order), 0) + 1 FROM batches WHERE session_id = ?`, session.ID).Scan(&creationOrder); err != nil {
		return "", sessionInternal(session.ID, "choose batch creation order", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO batches(id, session_id, creation_order, base_branch, base_commit, status, created_at, updated_at) VALUES(?, ?, ?, ?, ?, 'collecting', ?, ?)`, id, session.ID, creationOrder, session.StartingBranch, session.StartingCommit, now, now); err != nil {
		return "", sessionInternal(session.ID, "create collecting batch", err)
	}
	return id, nil
}

func newBatchID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "batch_" + hex.EncodeToString(value), nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableBytes(value []byte) any {
	if value == nil {
		return nil
	}
	return value
}

func nullableJSON(value []string, encoded []byte) any {
	if value == nil {
		return nil
	}
	return encoded
}
