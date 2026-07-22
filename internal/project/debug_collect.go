package project

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

func collectTasks(tx *sql.Tx, options DebugOptions, snapshot *DebugSnapshot) {
	rows, err := tx.Query(`
		SELECT task.id, task.creation_order, task.title, task.status,
			COALESCE(task.worker_identity, ''), COALESCE(task.assignment_token, ''), task.core_frozen,
			COALESCE(member.batch_id, ''), task.created_at, task.updated_at,
			lease.status, lease.duration_nanos, lease.renewed_at, lease.expires_at
		FROM tasks task
		LEFT JOIN batch_members member ON member.task_id = task.id
		LEFT JOIN task_leases lease ON lease.task_id = task.id
		WHERE task.session_id = ? ORDER BY task.creation_order`, snapshot.Session.ID)
	if err != nil {
		snapshot.addCollectionError("tasks", "tasks_unavailable", err, false)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var task DebugTask
		var token string
		var frozen int
		var leaseStatus, renewedAt, expiresAt sql.NullString
		var duration sql.NullInt64
		if err := rows.Scan(&task.ID, &task.CreationOrder, &task.Title, &task.Status, &task.WorkerIdentity, &token, &frozen, &task.BatchID, &task.CreatedAt, &task.UpdatedAt, &leaseStatus, &duration, &renewedAt, &expiresAt); err != nil {
			snapshot.addCollectionError("tasks", "task_unavailable", err, false)
			return
		}
		task.CoreFrozen = frozen != 0
		task.AssignmentTokenPresent = token != ""
		task.AssignmentTokenHash = fingerprint(token)
		if options.Unsafe {
			task.AssignmentToken = token
		}
		task.Prerequisites = []string{}
		task.Claims = []DebugClaim{}
		if leaseStatus.Valid {
			task.Lease = &DebugLease{Status: leaseStatus.String, DurationNanos: duration.Int64, RenewedAt: renewedAt.String, ExpiresAt: expiresAt.String}
		}
		snapshot.Tasks = append(snapshot.Tasks, task)
	}
	if err := rows.Err(); err != nil {
		snapshot.addCollectionError("tasks", "tasks_unavailable", err, false)
		return
	}
	for index := range snapshot.Tasks {
		collectTaskRelationships(tx, &snapshot.Tasks[index], snapshot)
	}
}

func collectTaskRelationships(tx *sql.Tx, task *DebugTask, snapshot *DebugSnapshot) {
	rows, err := tx.Query(`SELECT prerequisite_id FROM task_dependencies WHERE task_id = ? ORDER BY dependency_order`, task.ID)
	if err != nil {
		snapshot.addCollectionError("dependencies", "dependencies_unavailable", err, false)
	} else {
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				snapshot.addCollectionError("dependencies", "dependency_unavailable", err, false)
				break
			}
			task.Prerequisites = append(task.Prerequisites, id)
		}
		if err := rows.Close(); err != nil {
			snapshot.addCollectionError("dependencies", "dependencies_unavailable", err, false)
		}
	}
	claimRows, err := tx.Query(`SELECT claim.path, claim.baseline_presence, claim.baseline_type, COALESCE(claim.baseline_content_hash, ''), length(claim.baseline_content), claim.baseline_executable, claim.claimed_at,
		snapshot.presence, snapshot.file_type, COALESCE(snapshot.content_hash, ''), length(snapshot.content), snapshot.executable
		FROM claims claim LEFT JOIN submitted_snapshots snapshot ON snapshot.task_id = claim.task_id AND snapshot.path = claim.path
		WHERE claim.task_id = ? ORDER BY claim.claim_order`, task.ID)
	if err != nil {
		snapshot.addCollectionError("claims", "claims_unavailable", err, false)
	} else {
		for claimRows.Next() {
			var claim DebugClaim
			var size, submittedSize sql.NullInt64
			var executable int
			var submittedPresence, submittedType, submittedHash sql.NullString
			var submittedExecutable sql.NullInt64
			if err := claimRows.Scan(&claim.Path, &claim.BaselinePresence, &claim.BaselineType, &claim.BaselineHash, &size, &executable, &claim.ClaimedAt, &submittedPresence, &submittedType, &submittedHash, &submittedSize, &submittedExecutable); err != nil {
				snapshot.addCollectionError("claims", "claim_unavailable", err, false)
				break
			}
			claim.BaselineSize = size.Int64
			claim.BaselineExecutable = executable != 0
			claim.SubmittedPresence = submittedPresence.String
			claim.SubmittedType = submittedType.String
			claim.SubmittedHash = submittedHash.String
			claim.SubmittedSize = submittedSize.Int64
			claim.SubmittedExecutable = submittedExecutable.Int64 != 0
			task.Claims = append(task.Claims, claim)
		}
		if err := claimRows.Close(); err != nil {
			snapshot.addCollectionError("claims", "claims_unavailable", err, false)
		}
	}
	var submission DebugSubmission
	var noChanges int
	err = tx.QueryRow(`SELECT outcome, no_changes, length(behavior_changed), length(key_decisions), length(validation_expectations), length(known_risks), submitted_at FROM task_submissions WHERE task_id = ?`, task.ID).Scan(
		&submission.Outcome, &noChanges, &submission.BehaviorChangedSize, &submission.KeyDecisionsSize, &submission.ValidationExpectationsSize, &submission.KnownRisksSize, &submission.SubmittedAt)
	if err == nil {
		submission.NoChanges = noChanges != 0
		task.Submission = &submission
	} else if !errors.Is(err, sql.ErrNoRows) {
		snapshot.addCollectionError("submissions", "submission_unavailable", err, false)
	}
}

func collectBatches(tx *sql.Tx, snapshot *DebugSnapshot) {
	rows, err := tx.Query(`SELECT id, creation_order, base_branch, base_commit, status, created_at, updated_at FROM batches WHERE session_id = ? ORDER BY creation_order`, snapshot.Session.ID)
	if err != nil {
		snapshot.addCollectionError("batches", "batches_unavailable", err, false)
		return
	}
	for rows.Next() {
		var batch DebugBatch
		batch.MemberTaskIDs = []string{}
		batch.Manifest = []DebugManifestPath{}
		batch.Validation = []DebugValidationAttempt{}
		if err := rows.Scan(&batch.ID, &batch.CreationOrder, &batch.BaseBranch, &batch.BaseCommit, &batch.Status, &batch.CreatedAt, &batch.UpdatedAt); err != nil {
			snapshot.addCollectionError("batches", "batch_unavailable", err, false)
			break
		}
		snapshot.Batches = append(snapshot.Batches, batch)
	}
	if err := rows.Close(); err != nil {
		snapshot.addCollectionError("batches", "batches_unavailable", err, false)
	}
	for index := range snapshot.Batches {
		collectBatchRelationships(tx, &snapshot.Batches[index], snapshot)
	}
}

func collectBatchRelationships(tx *sql.Tx, batch *DebugBatch, snapshot *DebugSnapshot) {
	memberRows, err := tx.Query(`SELECT task_id FROM batch_members WHERE batch_id = ? ORDER BY membership_order`, batch.ID)
	if err != nil {
		snapshot.addCollectionError("batches", "batch_members_unavailable", err, false)
	} else {
		for memberRows.Next() {
			var id string
			if err := memberRows.Scan(&id); err != nil {
				snapshot.addCollectionError("batches", "batch_member_unavailable", err, false)
				break
			}
			batch.MemberTaskIDs = append(batch.MemberTaskIDs, id)
		}
		_ = memberRows.Close()
	}
	manifestRows, err := tx.Query(`SELECT task_id, path, COALESCE(baseline_content_hash, ''), COALESCE(submitted_content_hash, '') FROM frozen_batch_paths WHERE batch_id = ? ORDER BY membership_order, claim_order`, batch.ID)
	if err != nil {
		snapshot.addCollectionError("manifests", "manifest_unavailable", err, false)
	} else {
		for manifestRows.Next() {
			var path DebugManifestPath
			if err := manifestRows.Scan(&path.TaskID, &path.Path, &path.BaselineHash, &path.SubmittedHash); err != nil {
				snapshot.addCollectionError("manifests", "manifest_path_unavailable", err, false)
				break
			}
			batch.Manifest = append(batch.Manifest, path)
		}
		_ = manifestRows.Close()
	}
	attemptRows, err := tx.Query(`SELECT attempt, status, started_at, finished_at FROM batch_validation_attempts WHERE batch_id = ? ORDER BY attempt`, batch.ID)
	if err != nil {
		snapshot.addCollectionError("validation", "validation_unavailable", err, false)
		return
	}
	for attemptRows.Next() {
		var attempt DebugValidationAttempt
		var finished sql.NullString
		attempt.Commands = []DebugValidationCommand{}
		if err := attemptRows.Scan(&attempt.Attempt, &attempt.Status, &attempt.StartedAt, &finished); err != nil {
			snapshot.addCollectionError("validation", "validation_attempt_unavailable", err, false)
			break
		}
		attempt.FinishedAt = finished.String
		batch.Validation = append(batch.Validation, attempt)
	}
	_ = attemptRows.Close()
	for index := range batch.Validation {
		attempt := &batch.Validation[index]
		commandRows, err := tx.Query(`SELECT command_order, source, COALESCE(task_id, ''), name, status, exit_code, duration_nanos, length(stdout), length(stderr), started_at, finished_at FROM batch_validation_runs WHERE batch_id = ? AND attempt = ? ORDER BY command_order`, batch.ID, attempt.Attempt)
		if err != nil {
			snapshot.addCollectionError("validation", "validation_commands_unavailable", err, false)
			continue
		}
		for commandRows.Next() {
			var command DebugValidationCommand
			var exit sql.NullInt64
			var duration int64
			if err := commandRows.Scan(&command.Order, &command.Source, &command.TaskID, &command.Name, &command.Status, &exit, &duration, &command.StdoutSize, &command.StderrSize, &command.StartedAt, &command.FinishedAt); err != nil {
				snapshot.addCollectionError("validation", "validation_command_unavailable", err, false)
				break
			}
			command.ExitCode = nullableInt(exit)
			command.DurationMilliseconds = time.Duration(duration).Milliseconds()
			attempt.Commands = append(attempt.Commands, command)
		}
		_ = commandRows.Close()
	}
}

func collectMonitors(tx *sql.Tx, snapshot *DebugSnapshot) {
	rows, err := tx.Query(`SELECT generation, process_id, process_identity, status, started_at, heartbeat_at, last_full_scan_at FROM session_monitors WHERE session_id = ? ORDER BY generation`, snapshot.Session.ID)
	if err != nil {
		snapshot.addCollectionError("monitors", "monitors_unavailable", err, false)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var monitor DebugMonitor
		var processID sql.NullInt64
		var heartbeat, scan sql.NullString
		if err := rows.Scan(&monitor.Generation, &processID, &monitor.ProcessIdentity, &monitor.Status, &monitor.StartedAt, &heartbeat, &scan); err != nil {
			snapshot.addCollectionError("monitors", "monitor_unavailable", err, false)
			return
		}
		if processID.Valid {
			value := processID.Int64
			monitor.ProcessID = &value
		}
		monitor.HeartbeatAt = heartbeat.String
		monitor.LastFullScanAt = scan.String
		snapshot.Monitors = append(snapshot.Monitors, monitor)
	}
}

func collectIntegrity(tx *sql.Tx, snapshot *DebugSnapshot) {
	rows, err := tx.Query(`SELECT id, kind, path, observed_state_json, detected_at, recovered_at FROM integrity_violations WHERE session_id = ? ORDER BY id`, snapshot.Session.ID)
	if err != nil {
		snapshot.addCollectionError("integrity", "integrity_unavailable", err, false)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var violation DebugIntegrity
		var evidence string
		var recovered sql.NullString
		if err := rows.Scan(&violation.ID, &violation.Kind, &violation.Path, &evidence, &violation.DetectedAt, &recovered); err != nil {
			snapshot.addCollectionError("integrity", "integrity_violation_unavailable", err, false)
			return
		}
		violation.EvidenceHash = fingerprint(evidence)
		violation.EvidenceSize = int64(len(evidence))
		violation.RecoveredAt = recovered.String
		snapshot.Integrity = append(snapshot.Integrity, violation)
	}
}

func collectEvents(tx *sql.Tx, options DebugOptions, snapshot *DebugSnapshot) {
	limit := " LIMIT ?"
	args := []any{snapshot.Session.ID, snapshot.Session.ID, snapshot.Session.ID}
	if options.CompleteHistory {
		limit = ""
	} else {
		args = append(args, options.HistoryLimit)
	}
	query := `SELECT sequence, kind, entity_type, entity_id, event, from_status, to_status, occurred_at FROM (
		SELECT sequence, 'session' kind, 'session' entity_type, session_id entity_id, event, COALESCE(from_status, '') from_status, to_status, occurred_at FROM audit_events WHERE session_id = ?
		UNION ALL SELECT sequence, 'task', 'task', task_id, event, COALESCE(from_status, ''), to_status, occurred_at FROM task_audit_events WHERE session_id = ?
		UNION ALL SELECT sequence, 'batch', 'batch', batch_id, event, COALESCE(from_status, ''), to_status, occurred_at FROM batch_audit_events WHERE session_id = ?
	) ORDER BY occurred_at DESC, kind, sequence DESC` + limit
	rows, err := tx.Query(query, args...)
	if err != nil {
		snapshot.addCollectionError("events", "events_unavailable", err, false)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var event DebugEvent
		if err := rows.Scan(&event.Sequence, &event.Kind, &event.EntityType, &event.EntityID, &event.Event, &event.FromStatus, &event.ToStatus, &event.OccurredAt); err != nil {
			snapshot.addCollectionError("events", "event_unavailable", err, false)
			return
		}
		snapshot.Events = append(snapshot.Events, event)
	}
}

func deriveWorkers(snapshot *DebugSnapshot) {
	workers := map[string]*DebugWorker{}
	for _, task := range snapshot.Tasks {
		if task.WorkerIdentity == "" {
			continue
		}
		worker := workers[task.WorkerIdentity]
		if worker == nil {
			worker = &DebugWorker{WorkerIdentity: task.WorkerIdentity, TaskIDs: []string{}, ClaimPaths: []string{}, Diagnostics: []string{}}
			workers[task.WorkerIdentity] = worker
		}
		worker.TaskIDs = append(worker.TaskIDs, task.ID)
		if taskCanCarryLiveWorkerOwnership(task.Status) {
			worker.ActiveTaskID = task.ID
			worker.Lease = task.Lease
		}
		for _, claim := range task.Claims {
			worker.ClaimPaths = append(worker.ClaimPaths, claim.Path)
		}
		if task.UpdatedAt > worker.LastActivityAt {
			worker.LastActivityAt = task.UpdatedAt
		}
	}
	for _, worker := range workers {
		worker.TaskIDs = sortedUnique(worker.TaskIDs)
		worker.ClaimPaths = sortedUnique(worker.ClaimPaths)
		snapshot.Workers = append(snapshot.Workers, *worker)
	}
	sort.Slice(snapshot.Workers, func(i, j int) bool { return snapshot.Workers[i].WorkerIdentity < snapshot.Workers[j].WorkerIdentity })
}

func (p *Project) deriveDiagnostics(snapshot *DebugSnapshot) {
	if snapshot.Session == nil {
		return
	}
	sessionID := []string{snapshot.Session.ID}
	addDiagnostic := func(code, severity string, affected DebugAffected, evidence map[string]any, actions ...string) {
		if affected.SessionIDs == nil {
			affected.SessionIDs = sessionID
		}
		if affected.BatchIDs == nil {
			affected.BatchIDs = []string{}
		}
		if affected.TaskIDs == nil {
			affected.TaskIDs = []string{}
		}
		if affected.Workers == nil {
			affected.Workers = []string{}
		}
		if affected.Paths == nil {
			affected.Paths = []string{}
		}
		snapshot.Diagnostics = append(snapshot.Diagnostics, DebugDiagnostic{Code: code, Severity: severity, Affected: affected, Evidence: evidence, SuggestedActions: actions})
	}
	now := time.Now().UTC()
	if snapshot.Configuration.Present && !snapshot.Configuration.Approved {
		addDiagnostic("configuration_drift", "warning", DebugAffected{}, map[string]any{"status": snapshot.Configuration.Status, "digest": snapshot.Configuration.Digest}, "bandmaster config status --json", "bandmaster config approve <digest> --json")
	}
	if snapshot.Repository.IndexChanged {
		addDiagnostic("git_index_drift", "error", DebugAffected{Paths: snapshot.Repository.ChangedPaths}, map[string]any{"index_changed": true}, "bandmaster doctor --json", "bandmaster session abort --dry-run --json")
	}
	if snapshot.Repository.Branch != "" && snapshot.Repository.Branch != snapshot.Session.StartingBranch {
		addDiagnostic("git_branch_drift", "critical", DebugAffected{}, map[string]any{"expected": snapshot.Session.StartingBranch, "observed": snapshot.Repository.Branch}, "bandmaster doctor --json", "bandmaster integrity recover --confirmation <inspection> --json")
	}
	for _, violation := range snapshot.Integrity {
		if violation.RecoveredAt == "" {
			addDiagnostic("integrity_violation", "error", DebugAffected{Paths: []string{violation.Path}}, map[string]any{"id": violation.ID, "kind": violation.Kind, "evidence_hash": violation.EvidenceHash}, "bandmaster integrity recover --confirmation <inspection> --json", "bandmaster session abort --dry-run --json")
		}
	}
	for _, monitor := range snapshot.Monitors {
		if monitor.Status == "unhealthy" {
			addDiagnostic("monitor_unhealthy", "error", DebugAffected{}, map[string]any{"generation": monitor.Generation, "heartbeat_at": monitor.HeartbeatAt}, "bandmaster doctor --json")
		}
		if monitor.Status == "healthy" && monitor.HeartbeatAt != "" {
			if heartbeat, err := time.Parse(time.RFC3339Nano, monitor.HeartbeatAt); err == nil && now.Sub(heartbeat) > 2*time.Second {
				addDiagnostic("monitor_stale", "error", DebugAffected{}, map[string]any{"generation": monitor.Generation, "heartbeat_at": monitor.HeartbeatAt}, "bandmaster doctor --json")
			}
		}
	}
	owned := map[string]bool{}
	for _, task := range snapshot.Tasks {
		for _, claim := range task.Claims {
			owned[claim.Path] = true
		}
	}
	for _, path := range snapshot.Repository.ChangedPaths {
		if !owned[path] {
			addDiagnostic("unowned_worktree_drift", "error", DebugAffected{Paths: []string{path}}, map[string]any{"path": path}, "bandmaster doctor --json", "bandmaster session abort --dry-run --json")
		}
	}
	statusByTask := map[string]string{}
	for _, task := range snapshot.Tasks {
		statusByTask[task.ID] = task.Status
	}
	for _, task := range snapshot.Tasks {
		liveWorkerOwnership := taskCanCarryLiveWorkerOwnership(task.Status)
		if liveWorkerOwnership && task.WorkerIdentity != "" && task.Lease != nil {
			expires, err := time.Parse(time.RFC3339Nano, task.Lease.ExpiresAt)
			if err == nil && now.After(expires) && task.Lease.Status == "active" {
				addDiagnostic("lease_expired", "error", DebugAffected{TaskIDs: []string{task.ID}, Workers: []string{task.WorkerIdentity}}, map[string]any{"expires_at": task.Lease.ExpiresAt}, "bandmaster task recover "+task.ID+" --terminated-worker "+task.WorkerIdentity+" --termination-proof <proof> --json", "bandmaster task inspect "+task.ID+" --json")
			} else if err == nil && task.AssignmentTokenPresent && task.Lease.Status == "active" && expires.Sub(now) <= time.Duration(task.Lease.DurationNanos)/4 {
				addDiagnostic("lease_expiring", "warning", DebugAffected{TaskIDs: []string{task.ID}, Workers: []string{task.WorkerIdentity}}, map[string]any{"expires_at": task.Lease.ExpiresAt, "remaining_milliseconds": expires.Sub(now).Milliseconds()}, "bandmaster task heartbeat "+task.ID+" --token <assignment-token> --json")
			}
		}
		if task.Status == "blocked" {
			addDiagnostic("task_blocked", "warning", DebugAffected{TaskIDs: []string{task.ID}}, map[string]any{"prerequisites": task.Prerequisites}, "bandmaster task requeue "+task.ID+" --json")
		}
		if task.Status == "submitted" {
			for _, claim := range task.Claims {
				if claim.SubmittedPresence == "" {
					continue
				}
				current, projectError := p.capturePath(claim.Path)
				if projectError != nil {
					snapshot.addCollectionError("filesystem", "submitted_path_unavailable", errors.New(projectError.Message), false)
					continue
				}
				if current.Presence != claim.SubmittedPresence || current.Type != claim.SubmittedType || current.ContentHash != claim.SubmittedHash || current.Executable != claim.SubmittedExecutable {
					addDiagnostic("submitted_path_drift", "error", DebugAffected{TaskIDs: []string{task.ID}, Paths: []string{claim.Path}}, map[string]any{"submitted_presence": claim.SubmittedPresence, "submitted_type": claim.SubmittedType, "submitted_hash": claim.SubmittedHash, "observed_presence": current.Presence, "observed_type": current.Type, "observed_hash": current.ContentHash}, "bandmaster doctor --json", "bandmaster task repair "+task.ID+" --json")
				}
			}
		}
		for _, prerequisite := range task.Prerequisites {
			if statusByTask[prerequisite] != "committed" && statusByTask[prerequisite] != "no_op" {
				addDiagnostic("dependency_wait", "info", DebugAffected{TaskIDs: []string{task.ID, prerequisite}}, map[string]any{"task_id": task.ID, "prerequisite_id": prerequisite, "prerequisite_status": statusByTask[prerequisite]}, "bandmaster task inspect "+prerequisite+" --json")
			}
		}
		if taskCanCarryLiveWorkerOwnership(task.Status) && (task.WorkerIdentity == "" || task.Lease == nil) {
			addDiagnostic("worker_invariant_failure", "critical", DebugAffected{TaskIDs: []string{task.ID}, Workers: []string{task.WorkerIdentity}}, map[string]any{"status": task.Status, "worker_present": task.WorkerIdentity != "", "lease_present": task.Lease != nil}, "bandmaster doctor --json")
		}
	}
	for _, event := range snapshot.Events {
		if event.Kind == "task" && event.Event == "task_blocked" {
			addDiagnostic("claim_contention", "warning", DebugAffected{TaskIDs: []string{event.EntityID}}, map[string]any{"event_sequence": event.Sequence, "occurred_at": event.OccurredAt}, "bandmaster task requeue "+event.EntityID+" --json")
		}
	}
	for index := range snapshot.Workers {
		for _, diagnostic := range snapshot.Diagnostics {
			for _, worker := range diagnostic.Affected.Workers {
				if worker == snapshot.Workers[index].WorkerIdentity {
					snapshot.Workers[index].Diagnostics = append(snapshot.Workers[index].Diagnostics, diagnostic.Code)
				}
			}
		}
		snapshot.Workers[index].Diagnostics = sortedUnique(snapshot.Workers[index].Diagnostics)
	}
	_ = p
}

func taskCanCarryLiveWorkerOwnership(status string) bool {
	return status == "assigned" || status == "editing"
}

// DebugSemanticState produces a stable, secret-free representation used by the watcher.
func DebugSemanticState(snapshot DebugSnapshot) ([]byte, error) {
	value := struct {
		Session     *DebugSession     `json:"session"`
		Tasks       []DebugTask       `json:"tasks"`
		Workers     []DebugWorker     `json:"workers"`
		Batches     []DebugBatch      `json:"batches"`
		Monitors    map[string]any    `json:"monitors"`
		Integrity   []DebugIntegrity  `json:"integrity"`
		Diagnostics []DebugDiagnostic `json:"diagnostics"`
	}{snapshot.Session, snapshot.Tasks, snapshot.Workers, snapshot.Batches, monitorMap(snapshot.Monitors), snapshot.Integrity, snapshot.Diagnostics}
	return json.Marshal(value)
}

func DebugChanges(previous, current DebugSnapshot) []DebugChange {
	before, _ := DebugSemanticState(previous)
	after, _ := DebugSemanticState(current)
	if string(before) == string(after) {
		return []DebugChange{}
	}
	changes := []DebugChange{}
	if fmt.Sprint(previous.Session) != fmt.Sprint(current.Session) {
		changes = append(changes, DebugChange{Kind: "session_changed", EntityType: "session", Before: previous.Session, After: current.Session})
	}
	changes = appendEntityChanges(changes, "task", taskMap(previous.Tasks), taskMap(current.Tasks))
	changes = appendEntityChanges(changes, "worker", workerMap(previous.Workers), workerMap(current.Workers))
	changes = appendEntityChanges(changes, "batch", batchMap(previous.Batches), batchMap(current.Batches))
	changes = appendEntityChanges(changes, "monitor", monitorMap(previous.Monitors), monitorMap(current.Monitors))
	changes = appendEntityChanges(changes, "integrity_violation", integrityMap(previous.Integrity), integrityMap(current.Integrity))
	if fmt.Sprint(previous.Diagnostics) != fmt.Sprint(current.Diagnostics) {
		changes = append(changes, DebugChange{Kind: "diagnostics_changed", EntityType: "snapshot", Before: previous.Diagnostics, After: current.Diagnostics})
	}
	return changes
}

type DebugChange struct {
	Kind       string `json:"kind"`
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id,omitempty"`
	Before     any    `json:"before,omitempty"`
	After      any    `json:"after,omitempty"`
}

func appendEntityChanges(changes []DebugChange, entity string, before, after map[string]any) []DebugChange {
	ids := map[string]bool{}
	for id := range before {
		ids[id] = true
	}
	for id := range after {
		ids[id] = true
	}
	ordered := []string{}
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	for _, id := range ordered {
		if fmt.Sprint(before[id]) != fmt.Sprint(after[id]) {
			changes = append(changes, DebugChange{Kind: entity + "_changed", EntityType: entity, EntityID: id, Before: before[id], After: after[id]})
		}
	}
	return changes
}
func taskMap(values []DebugTask) map[string]any {
	return indexDebugEntities(values, func(value DebugTask) string { return value.ID })
}
func workerMap(values []DebugWorker) map[string]any {
	return indexDebugEntities(values, func(value DebugWorker) string { return value.WorkerIdentity })
}
func batchMap(values []DebugBatch) map[string]any {
	return indexDebugEntities(values, func(value DebugBatch) string { return value.ID })
}
func monitorMap(values []DebugMonitor) map[string]any {
	type semanticMonitor struct {
		Generation int64  `json:"generation"`
		Status     string `json:"status"`
	}
	semantic := make([]semanticMonitor, 0, len(values))
	for _, value := range values {
		semantic = append(semantic, semanticMonitor{Generation: value.Generation, Status: value.Status})
	}
	return indexDebugEntities(semantic, func(value semanticMonitor) string { return fmt.Sprint(value.Generation) })
}
func integrityMap(values []DebugIntegrity) map[string]any {
	return indexDebugEntities(values, func(value DebugIntegrity) string { return fmt.Sprint(value.ID) })
}
func indexDebugEntities[T any](values []T, identity func(T) string) map[string]any {
	result := map[string]any{}
	for _, value := range values {
		result[identity(value)] = value
	}
	return result
}
