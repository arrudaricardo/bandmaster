package project

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
)

const DebugContractVersion = "1"

type DebugOptions struct {
	SessionID        string
	HistoryLimit     int
	CompleteHistory  bool
	Unsafe           bool
	Version          string
	Executable       string
	CachedRepository *DebugRepository
	LastGitRevision  string
	ForceGit         bool
}

type DebugSnapshot struct {
	ContractVersion string                `json:"contract_version"`
	Collection      DebugCollection       `json:"collection"`
	Runtime         DebugRuntime          `json:"runtime"`
	Locations       DebugLocations        `json:"locations"`
	Repository      DebugRepository       `json:"repository"`
	State           DebugState            `json:"state"`
	Configuration   DebugConfiguration    `json:"configuration"`
	Session         *DebugSession         `json:"session,omitempty"`
	Tasks           []DebugTask           `json:"tasks"`
	Workers         []DebugWorker         `json:"workers"`
	Batches         []DebugBatch          `json:"batches"`
	Monitors        []DebugMonitor        `json:"monitors"`
	Integrity       []DebugIntegrity      `json:"integrity_violations"`
	Events          []DebugEvent          `json:"events"`
	Diagnostics     []DebugDiagnostic     `json:"diagnostics"`
	Revision        DebugRevisionBoundary `json:"revision"`
}

type DebugCollection struct {
	Status     string                 `json:"status"`
	Stable     bool                   `json:"stable"`
	BestEffort bool                   `json:"best_effort"`
	CapturedAt string                 `json:"captured_at"`
	Errors     []DebugCollectionError `json:"errors"`
}

type DebugCollectionError struct {
	Section   string `json:"section"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type DebugRuntime struct {
	BandmasterVersion string `json:"bandmaster_version"`
	Executable        string `json:"executable"`
	GoVersion         string `json:"go_version"`
	GOOS              string `json:"goos"`
	GOARCH            string `json:"goarch"`
	VCSRevision       string `json:"vcs_revision,omitempty"`
	VCSModified       *bool  `json:"vcs_modified,omitempty"`
}

type DebugLocations struct {
	ProjectRoot string `json:"project_root"`
	GitDir      string `json:"git_dir"`
	StatePath   string `json:"state_path"`
	ConfigPath  string `json:"config_path"`
}

type DebugRepository struct {
	Root         string   `json:"root"`
	Branch       string   `json:"branch,omitempty"`
	Head         string   `json:"head,omitempty"`
	ChangedPaths []string `json:"changed_paths"`
	IndexChanged bool     `json:"index_changed"`
	InspectionOK bool     `json:"inspection_ok"`
}

type DebugState struct {
	Initialization string `json:"initialization"`
	SchemaVersion  string `json:"schema_version,omitempty"`
	DatabaseSize   int64  `json:"database_size_bytes,omitempty"`
}

type DebugConfiguration struct {
	Status         string `json:"status"`
	Present        bool   `json:"present"`
	Digest         string `json:"digest,omitempty"`
	Approved       bool   `json:"approved"`
	ApprovedDigest string `json:"approved_digest,omitempty"`
}

type DebugSession struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	StartingBranch string `json:"starting_branch"`
	StartingCommit string `json:"starting_commit"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	Historical     bool   `json:"historical"`
}

type DebugLease struct {
	Status        string `json:"status"`
	DurationNanos int64  `json:"duration_nanos"`
	RenewedAt     string `json:"renewed_at"`
	ExpiresAt     string `json:"expires_at"`
}

type DebugClaim struct {
	Path                string `json:"path"`
	BaselinePresence    string `json:"baseline_presence"`
	BaselineType        string `json:"baseline_type"`
	BaselineHash        string `json:"baseline_hash,omitempty"`
	BaselineSize        int64  `json:"baseline_size_bytes"`
	BaselineExecutable  bool   `json:"baseline_executable"`
	SubmittedPresence   string `json:"submitted_presence,omitempty"`
	SubmittedType       string `json:"submitted_type,omitempty"`
	SubmittedHash       string `json:"submitted_hash,omitempty"`
	SubmittedSize       int64  `json:"submitted_size_bytes,omitempty"`
	SubmittedExecutable bool   `json:"submitted_executable,omitempty"`
	ClaimedAt           string `json:"claimed_at"`
}

type DebugSubmission struct {
	Outcome                    string `json:"outcome"`
	NoChanges                  bool   `json:"no_changes"`
	SubmittedAt                string `json:"submitted_at"`
	BehaviorChangedSize        int64  `json:"behavior_changed_size_bytes"`
	KeyDecisionsSize           int64  `json:"key_decisions_size_bytes"`
	ValidationExpectationsSize int64  `json:"validation_expectations_size_bytes"`
	KnownRisksSize             int64  `json:"known_risks_size_bytes"`
}

type DebugTask struct {
	ID                     string           `json:"id"`
	CreationOrder          int64            `json:"creation_order"`
	Title                  string           `json:"title"`
	Status                 string           `json:"status"`
	WorkerIdentity         string           `json:"worker_identity,omitempty"`
	AssignmentTokenPresent bool             `json:"assignment_token_present"`
	AssignmentTokenHash    string           `json:"assignment_token_fingerprint,omitempty"`
	AssignmentToken        string           `json:"assignment_token,omitempty"`
	CoreFrozen             bool             `json:"core_frozen"`
	BatchID                string           `json:"batch_id,omitempty"`
	Prerequisites          []string         `json:"prerequisites"`
	Lease                  *DebugLease      `json:"lease,omitempty"`
	Claims                 []DebugClaim     `json:"claims"`
	Submission             *DebugSubmission `json:"submission,omitempty"`
	CreatedAt              string           `json:"created_at"`
	UpdatedAt              string           `json:"updated_at"`
}

type DebugWorker struct {
	WorkerIdentity string      `json:"worker_identity"`
	TaskIDs        []string    `json:"task_ids"`
	ActiveTaskID   string      `json:"active_task_id,omitempty"`
	Lease          *DebugLease `json:"lease,omitempty"`
	ClaimPaths     []string    `json:"claim_paths"`
	LastActivityAt string      `json:"last_activity_at,omitempty"`
	Diagnostics    []string    `json:"diagnostic_codes"`
}

type DebugBatch struct {
	ID            string                   `json:"id"`
	CreationOrder int64                    `json:"creation_order"`
	Status        string                   `json:"status"`
	BaseBranch    string                   `json:"base_branch"`
	BaseCommit    string                   `json:"base_commit"`
	MemberTaskIDs []string                 `json:"member_task_ids"`
	Manifest      []DebugManifestPath      `json:"manifest"`
	Validation    []DebugValidationAttempt `json:"validation"`
	CreatedAt     string                   `json:"created_at"`
	UpdatedAt     string                   `json:"updated_at"`
}

type DebugManifestPath struct {
	TaskID        string `json:"task_id"`
	Path          string `json:"path"`
	BaselineHash  string `json:"baseline_hash,omitempty"`
	SubmittedHash string `json:"submitted_hash,omitempty"`
}

type DebugValidationAttempt struct {
	Attempt    int64                    `json:"attempt"`
	Status     string                   `json:"status"`
	StartedAt  string                   `json:"started_at"`
	FinishedAt string                   `json:"finished_at,omitempty"`
	Commands   []DebugValidationCommand `json:"commands"`
}

type DebugValidationCommand struct {
	Order                int64  `json:"order"`
	Source               string `json:"source"`
	TaskID               string `json:"task_id,omitempty"`
	Name                 string `json:"name"`
	Status               string `json:"status"`
	ExitCode             *int   `json:"exit_code,omitempty"`
	DurationMilliseconds int64  `json:"duration_milliseconds"`
	StdoutSize           int64  `json:"stdout_size_bytes"`
	StderrSize           int64  `json:"stderr_size_bytes"`
	StartedAt            string `json:"started_at"`
	FinishedAt           string `json:"finished_at"`
}

type DebugMonitor struct {
	Generation      int64  `json:"generation"`
	ProcessID       *int64 `json:"process_id,omitempty"`
	ProcessIdentity string `json:"process_identity"`
	Status          string `json:"status"`
	StartedAt       string `json:"started_at"`
	HeartbeatAt     string `json:"heartbeat_at,omitempty"`
	LastFullScanAt  string `json:"last_full_scan_at,omitempty"`
}

type DebugIntegrity struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	Path         string `json:"path,omitempty"`
	EvidenceHash string `json:"evidence_hash"`
	EvidenceSize int64  `json:"evidence_size_bytes"`
	DetectedAt   string `json:"detected_at"`
	RecoveredAt  string `json:"recovered_at,omitempty"`
}

type DebugEvent struct {
	Sequence   int64  `json:"sequence"`
	Kind       string `json:"kind"`
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Event      string `json:"event"`
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status"`
	OccurredAt string `json:"occurred_at"`
}

type DebugDiagnostic struct {
	Code             string         `json:"code"`
	Severity         string         `json:"severity"`
	Affected         DebugAffected  `json:"affected"`
	Evidence         map[string]any `json:"evidence"`
	SuggestedActions []string       `json:"suggested_actions"`
}

type DebugAffected struct {
	SessionIDs []string `json:"session_ids"`
	BatchIDs   []string `json:"batch_ids"`
	TaskIDs    []string `json:"task_ids"`
	Workers    []string `json:"worker_identities"`
	Paths      []string `json:"paths"`
}

type DebugRevisionBoundary struct {
	DatabaseBefore int64  `json:"database_before,omitempty"`
	DatabaseAfter  int64  `json:"database_after,omitempty"`
	GitBefore      string `json:"git_before,omitempty"`
	GitAfter       string `json:"git_after,omitempty"`
}

func (p *Project) Debug(options DebugOptions) (DebugSnapshot, *Error) {
	if options.HistoryLimit == 0 {
		options.HistoryLimit = 50
	}
	if options.HistoryLimit < 0 {
		return DebugSnapshot{}, invalid("invalid_arguments", "--history-limit must be greater than zero.")
	}
	var last DebugSnapshot
	for attempt := 0; attempt < 2; attempt++ {
		snapshot := p.collectDebug(options)
		last = snapshot
		if options.SessionID != "" && snapshot.Session == nil && hasCollectionError(snapshot, "session_not_found") {
			return DebugSnapshot{}, invalid("session_not_found", fmt.Sprintf("Session %s does not exist.", options.SessionID))
		}
		if snapshot.Collection.Stable {
			return snapshot, nil
		}
	}
	last.Collection.BestEffort = true
	return last, nil
}

func (p *Project) collectDebug(options DebugOptions) DebugSnapshot {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	snapshot := DebugSnapshot{
		ContractVersion: DebugContractVersion,
		Collection:      DebugCollection{Status: "complete", Stable: true, CapturedAt: now, Errors: []DebugCollectionError{}},
		Runtime:         debugRuntime(options),
		Locations:       DebugLocations{ProjectRoot: p.Root, GitDir: p.GitDir, StatePath: filepath.Join(p.GitDir, "bandmaster", "state.db"), ConfigPath: filepath.Join(p.Root, ".bandmaster.yaml")},
		Repository:      DebugRepository{Root: p.Root, ChangedPaths: []string{}},
		State:           DebugState{Initialization: "uninitialized"},
		Tasks:           []DebugTask{}, Workers: []DebugWorker{}, Batches: []DebugBatch{}, Monitors: []DebugMonitor{}, Integrity: []DebugIntegrity{}, Events: []DebugEvent{}, Diagnostics: []DebugDiagnostic{},
	}
	snapshot.Revision.GitBefore = p.gitRevision()
	if options.CachedRepository != nil && !options.ForceGit && options.LastGitRevision == snapshot.Revision.GitBefore {
		snapshot.Repository = *options.CachedRepository
	} else {
		p.collectDebugRepository(&snapshot)
	}
	p.collectDebugConfiguration(&snapshot)
	info, err := os.Stat(snapshot.Locations.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		snapshot.Revision.GitAfter = p.gitRevision()
		snapshot.Collection.Stable = snapshot.Revision.GitBefore == snapshot.Revision.GitAfter
		return snapshot
	}
	if err != nil {
		snapshot.addCollectionError("state", "state_unavailable", err, false)
		return snapshot
	}
	snapshot.State.Initialization = "initialized"
	snapshot.State.DatabaseSize = info.Size()
	db, err := openDebugState(snapshot.Locations.StatePath)
	if err != nil {
		snapshot.addCollectionError("database", "state_unavailable", err, isBusyError(err))
		return snapshot
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		snapshot.addCollectionError("database", "state_unavailable", err, isBusyError(err))
		return snapshot
	}
	defer tx.Rollback()
	_ = tx.QueryRow(`PRAGMA data_version`).Scan(&snapshot.Revision.DatabaseBefore)
	p.collectDebugDatabase(tx, options, &snapshot)
	if err := tx.Commit(); err != nil {
		snapshot.addCollectionError("database", "collection_failed", err, isBusyError(err))
	}
	_ = db.QueryRow(`PRAGMA data_version`).Scan(&snapshot.Revision.DatabaseAfter)
	// Git is deliberately inspected immediately after the coherent database read.
	snapshot.Revision.GitAfter = p.gitRevision()
	if options.CachedRepository == nil || options.ForceGit || options.LastGitRevision != snapshot.Revision.GitAfter || snapshot.Revision.GitBefore != snapshot.Revision.GitAfter {
		snapshot.Repository = DebugRepository{Root: p.Root, ChangedPaths: []string{}}
		p.collectDebugRepository(&snapshot)
		snapshot.Revision.GitAfter = p.gitRevision()
	}
	snapshot.Collection.Stable = snapshot.Revision.DatabaseBefore == snapshot.Revision.DatabaseAfter && snapshot.Revision.GitBefore == snapshot.Revision.GitAfter
	p.deriveDiagnostics(&snapshot)
	return snapshot
}

func debugRuntime(options DebugOptions) DebugRuntime {
	executable := options.Executable
	if executable == "" {
		executable, _ = os.Executable()
	}
	r := DebugRuntime{BandmasterVersion: options.Version, Executable: executable, GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	if r.BandmasterVersion == "" {
		r.BandmasterVersion = "dev"
	}
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				r.VCSRevision = setting.Value
			case "vcs.modified":
				value := setting.Value == "true"
				r.VCSModified = &value
			}
		}
	}
	return r
}

func openDebugState(path string) (*sql.DB, error) {
	stateURL := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(200)"
	db, err := sql.Open("sqlite3", stateURL)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func (p *Project) collectDebugRepository(snapshot *DebugSnapshot) {
	branch, branchErr := debugGitOutput(p.Root, "symbolic-ref", "--quiet", "--short", "HEAD")
	head, headErr := debugGitOutput(p.Root, "rev-parse", "--verify", "HEAD")
	status, statusErr := debugGitOutput(p.Root, "status", "--porcelain=v1")
	if branchErr == nil {
		snapshot.Repository.Branch = branch
	}
	if headErr == nil {
		snapshot.Repository.Head = head
	}
	if statusErr == nil {
		for _, line := range strings.Split(status, "\n") {
			if len(line) < 4 {
				continue
			}
			path := line[3:]
			if arrow := strings.LastIndex(path, " -> "); arrow >= 0 {
				path = path[arrow+4:]
			}
			snapshot.Repository.ChangedPaths = append(snapshot.Repository.ChangedPaths, path)
			if line[0] != ' ' && line[0] != '?' {
				snapshot.Repository.IndexChanged = true
			}
		}
		snapshot.Repository.InspectionOK = true
	} else if headErr != nil {
		// A newly initialized repository without a first commit is still useful.
		snapshot.Repository.InspectionOK = true
	}
	if statusErr != nil {
		snapshot.addCollectionError("git", "git_inspection_failed", statusErr, false)
	}
	_ = branchErr
	_ = headErr
}

func (p *Project) collectDebugConfiguration(snapshot *DebugSnapshot) {
	content, err := os.ReadFile(snapshot.Locations.ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		snapshot.Configuration.Status = "missing"
		return
	}
	if err != nil {
		snapshot.Configuration.Status = "unavailable"
		snapshot.addCollectionError("configuration", "configuration_unavailable", err, false)
		return
	}
	digest := sha256.Sum256(content)
	snapshot.Configuration.Present = true
	snapshot.Configuration.Digest = hex.EncodeToString(digest[:])
	if projectError := validateConfiguration(content, p.Root); projectError != nil {
		snapshot.Configuration.Status = "invalid"
		snapshot.addCollectionError("configuration", projectError.Code, errors.New(projectError.Message), false)
		return
	}
	snapshot.Configuration.Status = "valid"
}

func (p *Project) collectDebugDatabase(tx *sql.Tx, options DebugOptions, snapshot *DebugSnapshot) {
	var schemaVersion int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&schemaVersion); err != nil {
		snapshot.addCollectionError("schema", "schema_unavailable", err, false)
	} else {
		snapshot.State.SchemaVersion = strconv.Itoa(schemaVersion)
	}
	if snapshot.State.SchemaVersion == "0" {
		snapshot.State.SchemaVersion = "1"
	}
	if err := tx.QueryRow(`SELECT value FROM metadata WHERE key = 'approved_configuration_digest'`).Scan(&snapshot.Configuration.ApprovedDigest); err != nil && !errors.Is(err, sql.ErrNoRows) {
		snapshot.addCollectionError("configuration", "approval_unavailable", err, false)
	}
	snapshot.Configuration.Approved = snapshot.Configuration.Digest != "" && snapshot.Configuration.Digest == snapshot.Configuration.ApprovedDigest
	if !snapshot.Configuration.Approved && snapshot.Configuration.Present && snapshot.Configuration.Status == "valid" {
		snapshot.Configuration.Status = "unapproved"
	}
	if err := collectSession(tx, options.SessionID, snapshot); err != nil {
		code := "session_unavailable"
		if errors.Is(err, errDebugSessionNotFound) {
			code = "session_not_found"
		}
		snapshot.addCollectionError("session", code, err, false)
		return
	}
	if snapshot.Session == nil {
		return
	}
	collectTasks(tx, options, snapshot)
	collectBatches(tx, snapshot)
	collectMonitors(tx, snapshot)
	collectIntegrity(tx, snapshot)
	collectEvents(tx, options, snapshot)
	deriveWorkers(snapshot)
}

func collectSession(tx *sql.Tx, selected string, snapshot *DebugSnapshot) error {
	query := `SELECT id, status, starting_branch, starting_commit, created_at, updated_at FROM sessions `
	args := []any{}
	if selected != "" {
		query += `WHERE id = ?`
		args = append(args, selected)
	} else {
		query += `ORDER BY CASE WHEN status IN ('active','paused','finalizing','aborting') THEN 0 ELSE 1 END, created_at DESC LIMIT 1`
	}
	var session DebugSession
	err := tx.QueryRow(query, args...).Scan(&session.ID, &session.Status, &session.StartingBranch, &session.StartingCommit, &session.CreatedAt, &session.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		if selected != "" {
			return fmt.Errorf("%w: %s", errDebugSessionNotFound, selected)
		}
		return nil
	}
	if err != nil {
		return err
	}
	session.Historical = session.Status == "completed" || session.Status == "aborted"
	snapshot.Session = &session
	return nil
}

var errDebugSessionNotFound = errors.New("requested session does not exist")

func hasCollectionError(snapshot DebugSnapshot, code string) bool {
	for _, collectionError := range snapshot.Collection.Errors {
		if collectionError.Code == code {
			return true
		}
	}
	return false
}

func (s *DebugSnapshot) addCollectionError(section, code string, err error, retryable bool) {
	s.Collection.Status = "partial"
	s.Collection.Stable = false
	s.Collection.Errors = append(s.Collection.Errors, DebugCollectionError{Section: section, Code: code, Message: err.Error(), Retryable: retryable})
}

func (p *Project) gitRevision() string {
	head, _ := debugGitOutput(p.Root, "rev-parse", "--verify", "HEAD")
	index := ""
	if info, err := os.Stat(filepath.Join(p.GitDir, "index")); err == nil {
		index = fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
	}
	return head + ":" + index
}

func debugGitOutput(dir string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"--no-optional-locks", "-C", dir}, args...)...)
	output, err := command.Output()
	value := strings.TrimSuffix(string(output), "\n")
	return strings.TrimSuffix(value, "\r"), err
}

func fingerprint(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:8])
}

func isBusyError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "busy") || strings.Contains(strings.ToLower(err.Error()), "locked")
}

func nullableInt(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	result := int(value.Int64)
	return &result
}
func sortedUnique(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}
