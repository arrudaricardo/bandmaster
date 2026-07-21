package project

import (
	"database/sql"
	"errors"
)

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
