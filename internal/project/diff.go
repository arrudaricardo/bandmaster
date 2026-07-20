package project

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type PathDiff struct {
	Path     string       `json:"path"`
	Baseline PathSnapshot `json:"baseline"`
	Current  PathSnapshot `json:"current"`
	Changed  bool         `json:"changed"`
	Patch    string       `json:"patch,omitempty"`
}

type TaskDiff struct {
	SessionID  string     `json:"-"`
	TaskID     string     `json:"task_id"`
	Paths      []PathDiff `json:"paths"`
	ReviewedAt string     `json:"reviewed_at"`
}

type storedClaim struct {
	Path     string
	Baseline capturedSnapshot
}

func (p *Project) DiffTask(id, assignmentToken string) (TaskDiff, *Error) {
	if strings.TrimSpace(assignmentToken) == "" {
		return TaskDiff{}, invalid("assignment_token_required", "The current assignment token is required.")
	}
	if _, projectError := p.renewWorkerLease(id, assignmentToken, "editing"); projectError != nil {
		return TaskDiff{}, projectError
	}
	db, projectError := p.openState()
	if projectError != nil {
		return TaskDiff{}, projectError
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return TaskDiff{}, internal("begin task diff review", err)
	}
	defer tx.Rollback()
	session, projectError := inspectActiveSession(tx)
	if projectError != nil {
		return TaskDiff{}, projectError
	}
	if projectError := validateTaskAuthority(tx, session.ID, id, assignmentToken, "editing"); projectError != nil {
		return TaskDiff{}, projectError
	}
	if projectError := p.validateClaimRepositoryState(tx, session); projectError != nil {
		return TaskDiff{}, projectError
	}
	claims, projectError := loadStoredClaims(tx, session.ID, id)
	if projectError != nil {
		return TaskDiff{}, projectError
	}
	reviewed := make([]capturedSnapshot, 0, len(claims))
	result := TaskDiff{SessionID: session.ID, TaskID: id, Paths: make([]PathDiff, 0, len(claims))}
	for _, claim := range claims {
		current, projectError := p.capturePath(claim.Path)
		if projectError != nil {
			projectError.SessionID = session.ID
			return TaskDiff{}, projectError
		}
		changed := !snapshotsEqual(claim.Baseline.PathSnapshot, current.PathSnapshot)
		current.Path = claim.Path
		reviewed = append(reviewed, current)
		pathDiff := PathDiff{Path: claim.Path, Baseline: claim.Baseline.PathSnapshot, Current: current.PathSnapshot, Changed: changed}
		if changed {
			patch, projectError := renderPathPatch(claim.Path, claim.Baseline, current)
			if projectError != nil {
				projectError.SessionID = session.ID
				return TaskDiff{}, projectError
			}
			pathDiff.Patch = patch
		}
		result.Paths = append(result.Paths, pathDiff)
	}
	if projectError := p.validateClaimRepositoryState(tx, session); projectError != nil {
		return TaskDiff{}, projectError
	}
	if _, err := tx.Exec(`DELETE FROM task_diff_reviews WHERE task_id = ?`, id); err != nil {
		return TaskDiff{}, sessionInternal(session.ID, "replace task diff review", err)
	}
	result.ReviewedAt = time.Now().UTC().Format(time.RFC3339Nano)
	for _, snapshot := range reviewed {
		if _, err := tx.Exec(`INSERT INTO task_diff_reviews(task_id, path, presence, file_type, content_hash, executable, reviewed_at) VALUES(?, ?, ?, ?, ?, ?, ?)`, id, snapshot.Path, snapshot.Presence, snapshot.Type, nullableString(snapshot.ContentHash), snapshot.Executable, result.ReviewedAt); err != nil {
			return TaskDiff{}, sessionInternal(session.ID, "record task diff review", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, from_status, to_status, occurred_at) VALUES(?, ?, 'task_diff_reviewed', 'editing', 'editing', ?)`, session.ID, id, result.ReviewedAt); err != nil {
		return TaskDiff{}, sessionInternal(session.ID, "audit task diff review", err)
	}
	if err := tx.Commit(); err != nil {
		return TaskDiff{}, sessionInternal(session.ID, "commit task diff review", err)
	}
	if projectError := p.PrepareMutation("post-task diff"); projectError != nil {
		return TaskDiff{}, projectError
	}
	return result, nil
}

func loadStoredClaims(queryer databaseQuerier, sessionID, taskID string) ([]storedClaim, *Error) {
	rows, err := queryer.Query(`SELECT path, baseline_presence, baseline_type, baseline_content_hash, baseline_executable, baseline_content FROM claims WHERE session_id = ? AND task_id = ? ORDER BY claim_order`, sessionID, taskID)
	if err != nil {
		return nil, sessionInternal(sessionID, "read claimed baselines", err)
	}
	defer rows.Close()
	var claims []storedClaim
	for rows.Next() {
		var claim storedClaim
		var contentHash sql.NullString
		var executable int
		var content []byte
		if err := rows.Scan(&claim.Path, &claim.Baseline.Presence, &claim.Baseline.Type, &contentHash, &executable, &content); err != nil {
			return nil, sessionInternal(sessionID, "read claimed baseline", err)
		}
		claim.Baseline.ContentHash = contentHash.String
		claim.Baseline.Executable = executable != 0
		claim.Baseline.content = content
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, sessionInternal(sessionID, "read claimed baselines", err)
	}
	if len(claims) == 0 {
		return nil, invalidSession(sessionID, "task_has_no_claims", fmt.Sprintf("Task %s has no path claims.", taskID))
	}
	return claims, nil
}

func snapshotsEqual(left, right PathSnapshot) bool {
	return left.Presence == right.Presence && left.Type == right.Type && left.ContentHash == right.ContentHash && left.Executable == right.Executable
}

func renderPathPatch(claimPath string, baseline, current capturedSnapshot) (string, *Error) {
	directory, err := os.MkdirTemp("", "bandmaster-diff-*")
	if err != nil {
		return "", internal("create task diff workspace", err)
	}
	defer os.RemoveAll(directory)
	before := filepath.Join("before", filepath.FromSlash(claimPath))
	after := filepath.Join("after", filepath.FromSlash(claimPath))
	beforeArg := before
	afterArg := after
	if baseline.Presence == "absent" {
		beforeArg = "/dev/null"
	} else if err := writeDiffFile(filepath.Join(directory, before), baseline); err != nil {
		return "", internal("write baseline diff file", err)
	}
	if current.Presence == "absent" {
		afterArg = "/dev/null"
	} else if err := writeDiffFile(filepath.Join(directory, after), current); err != nil {
		return "", internal("write current diff file", err)
	}
	command := exec.Command("git", "diff", "--no-index", "--binary", "--no-ext-diff", "--", beforeArg, afterArg)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
			return "", internal("render claimed path diff", fmt.Errorf("%w: %s", err, output))
		}
	}
	replacements := []struct{ old, new string }{
		{"a/before/", "a/"},
		{"b/before/", "b/"},
		{"a/after/", "a/"},
		{"b/after/", "b/"},
	}
	lines := strings.SplitAfter(string(output), "\n")
	for index, line := range lines {
		if !strings.HasPrefix(line, "diff --git ") && !strings.HasPrefix(line, "--- ") && !strings.HasPrefix(line, "+++ ") && !strings.HasPrefix(line, "Binary files ") {
			continue
		}
		for _, replacement := range replacements {
			line = strings.Replace(line, replacement.old, replacement.new, 1)
		}
		lines[index] = line
	}
	return strings.Join(lines, ""), nil
}

func writeDiffFile(filePath string, snapshot capturedSnapshot) error {
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return err
	}
	if snapshot.Type == "symlink" {
		return os.Symlink(string(snapshot.content), filePath)
	}
	mode := os.FileMode(0o644)
	if snapshot.Executable {
		mode = 0o755
	}
	return os.WriteFile(filePath, snapshot.content, mode)
}
