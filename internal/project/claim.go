package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type Claim struct {
	Path              string        `json:"path"`
	Baseline          PathSnapshot  `json:"baseline"`
	SubmittedSnapshot *PathSnapshot `json:"submitted_snapshot,omitempty"`
}

type OwnershipEvidence struct {
	Path              string        `json:"path"`
	Baseline          PathSnapshot  `json:"baseline"`
	SubmittedSnapshot *PathSnapshot `json:"submitted_snapshot,omitempty"`
	ClaimedAt         string        `json:"claimed_at"`
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
			if _, err := tx.Exec(`INSERT OR IGNORE INTO task_path_ownership(session_id, batch_id, task_id, claim_order, path, baseline_presence, baseline_type, baseline_content_hash, baseline_executable, baseline_content, claimed_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, session.ID, batchID, id, nextOrder+index, claim.Path, claim.Presence, claim.Type, nullableString(claim.ContentHash), claim.Executable, nullableBytes(claim.content), now); err != nil {
				return Task{}, sessionInternal(session.ID, "record expanded path ownership evidence", err)
			}
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
		if _, err := tx.Exec(`INSERT OR IGNORE INTO task_path_ownership(session_id, batch_id, task_id, claim_order, path, baseline_presence, baseline_type, baseline_content_hash, baseline_executable, baseline_content, claimed_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, session.ID, batchID, id, index+1, claim.Path, claim.Presence, claim.Type, nullableString(claim.ContentHash), claim.Executable, nullableBytes(claim.content), now); err != nil {
			return Task{}, sessionInternal(session.ID, "record path ownership evidence", err)
		}
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
