package project

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type Task struct {
	SessionID         string              `json:"-"`
	ID                string              `json:"id"`
	CreationOrder     int64               `json:"creation_order"`
	Title             string              `json:"title"`
	Intent            string              `json:"intent"`
	ExpectedOutcome   string              `json:"expected_outcome"`
	Prerequisites     []string            `json:"prerequisites"`
	Status            string              `json:"status"`
	WorkerIdentity    string              `json:"worker_identity,omitempty"`
	AssignmentToken   string              `json:"assignment_token,omitempty"`
	CoreFrozen        bool                `json:"core_frozen"`
	BatchID           string              `json:"batch_id,omitempty"`
	CommitSHA         string              `json:"commit_sha,omitempty"`
	Lease             *WorkerLease        `json:"lease,omitempty"`
	Claims            []Claim             `json:"claims"`
	FocusedValidation []FocusedValidation `json:"focused_validation"`
	Submission        *Submission         `json:"submission,omitempty"`
	CreatedAt         string              `json:"created_at"`
	UpdatedAt         string              `json:"updated_at"`
	AuditHistory      []TaskAuditEvent    `json:"audit_history"`
}

type TaskAuditEvent struct {
	Sequence         int64            `json:"sequence"`
	Event            string           `json:"event"`
	FromStatus       string           `json:"from_status,omitempty"`
	ToStatus         string           `json:"to_status"`
	WorkerIdentity   string           `json:"worker_identity,omitempty"`
	TerminationProof string           `json:"termination_proof,omitempty"`
	RecoveryMethod   string           `json:"recovery_method,omitempty"`
	UserConfirmation string           `json:"user_confirmation,omitempty"`
	ReplacementToken string           `json:"replacement_assignment_token,omitempty"`
	Diagnosis        string           `json:"diagnosis,omitempty"`
	IntendedRepair   string           `json:"intended_repair,omitempty"`
	Invalidated      *Submission      `json:"invalidated_submission,omitempty"`
	RepairSnapshots  []RepairSnapshot `json:"repair_snapshots,omitempty"`
	OccurredAt       string           `json:"occurred_at"`
}

type RepairSnapshot struct {
	Path                 string        `json:"path"`
	Snapshot             PathSnapshot  `json:"snapshot"`
	InvalidatedSubmitted *PathSnapshot `json:"invalidated_submitted_snapshot,omitempty"`
}

type TaskPlan struct {
	Title           string
	Intent          string
	ExpectedOutcome string
	Prerequisites   []string
}

type TaskList struct {
	Tasks []Task `json:"tasks"`
}

func (p *Project) CreateTask(plan TaskPlan) (Task, *Error) {
	if projectError := validateTaskPlan(plan); projectError != nil {
		return Task{}, projectError
	}
	db, projectError := p.openState()
	if projectError != nil {
		return Task{}, projectError
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return Task{}, internal("begin task creation", err)
	}
	defer tx.Rollback()
	session, projectError := inspectActiveSession(tx)
	if projectError != nil {
		return Task{}, projectError
	}
	if projectError := validatePrerequisites(tx, session.ID, "", plan.Prerequisites); projectError != nil {
		projectError.SessionID = session.ID
		return Task{}, projectError
	}

	id, err := newTaskID()
	if err != nil {
		return Task{}, sessionInternal(session.ID, "generate task identity", err)
	}
	var creationOrder int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(creation_order), 0) + 1 FROM tasks WHERE session_id = ?`, session.ID).Scan(&creationOrder); err != nil {
		return Task{}, sessionInternal(session.ID, "choose task creation order", err)
	}
	status, projectError := taskReadiness(tx, plan.Prerequisites)
	if projectError != nil {
		projectError.SessionID = session.ID
		return Task{}, projectError
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO tasks(id, session_id, creation_order, title, intent, expected_outcome, status, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, session.ID, creationOrder, plan.Title, plan.Intent, plan.ExpectedOutcome, status, now, now); err != nil {
		return Task{}, sessionInternal(session.ID, "create task", err)
	}
	for index, prerequisite := range plan.Prerequisites {
		if _, err := tx.Exec(`INSERT INTO task_dependencies(task_id, prerequisite_id, dependency_order) VALUES(?, ?, ?)`, id, prerequisite, index+1); err != nil {
			return Task{}, sessionInternal(session.ID, "record task prerequisite", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO task_audit_events(session_id, task_id, event, to_status, occurred_at) VALUES(?, ?, 'task_created', ?, ?)`, session.ID, id, status, now); err != nil {
		return Task{}, sessionInternal(session.ID, "record task creation", err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, sessionInternal(session.ID, "commit task creation", err)
	}
	return inspectTask(db, session.ID, id)
}

func validateTaskPlan(plan TaskPlan) *Error {
	for name, value := range map[string]string{"title": plan.Title, "intent": plan.Intent, "expected outcome": plan.ExpectedOutcome} {
		if strings.TrimSpace(value) == "" {
			return invalid("invalid_task_plan", fmt.Sprintf("Task %s must not be empty.", name))
		}
	}
	seen := make(map[string]struct{}, len(plan.Prerequisites))
	for _, prerequisite := range plan.Prerequisites {
		if strings.TrimSpace(prerequisite) == "" {
			return invalid("invalid_task_plan", "Task prerequisite identities must not be empty.")
		}
		if _, exists := seen[prerequisite]; exists {
			return invalid("duplicate_task_prerequisite", fmt.Sprintf("Task prerequisite %s is duplicated.", prerequisite))
		}
		seen[prerequisite] = struct{}{}
	}
	return nil
}

func newTaskID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "task_" + hex.EncodeToString(value), nil
}

func newAssignmentToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "assignment_" + hex.EncodeToString(value), nil
}
