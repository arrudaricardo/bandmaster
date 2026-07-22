package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bandmaster-dev/bandmaster/internal/project"
	"github.com/bandmaster-dev/bandmaster/internal/tui"
)

type debugCLIOptions struct {
	sessionID       string
	historyLimit    int
	completeHistory bool
	unsafe          bool
	watch           bool
	followLatest    bool
	interval        time.Duration
}

func parseDebugOptions(args []string) (debugCLIOptions, error) {
	options := debugCLIOptions{historyLimit: 50, interval: time.Second}
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--watch":
			options.watch = true
		case "--follow-latest":
			options.followLatest = true
		case "--complete-history":
			options.completeHistory = true
		case "--unsafe", "--unsafe-show-secrets":
			options.unsafe = true
		case "--session", "--history-limit", "--interval":
			name := args[index]
			if index+1 >= len(args) {
				return options, fmt.Errorf("%s requires a value", name)
			}
			index++
			value := args[index]
			switch name {
			case "--session":
				options.sessionID = value
			case "--history-limit":
				limit, err := strconv.Atoi(value)
				if err != nil || limit <= 0 {
					return options, fmt.Errorf("--history-limit must be a positive integer")
				}
				options.historyLimit = limit
			case "--interval":
				interval, err := time.ParseDuration(value)
				if err != nil {
					return options, fmt.Errorf("invalid --interval: %w", err)
				}
				if interval < 250*time.Millisecond {
					return options, fmt.Errorf("--interval must be at least 250ms")
				}
				options.interval = interval
			}
		default:
			return options, fmt.Errorf("unknown option %s", args[index])
		}
	}
	if options.followLatest && !options.watch {
		return options, fmt.Errorf("--follow-latest requires --watch")
	}
	return options, nil
}

func runDebug(currentProject *project.Project, options debugCLIOptions, jsonOutput bool, stdout, stderr io.Writer) int {
	executable, _ := os.Executable()
	projectOptions := project.DebugOptions{SessionID: options.sessionID, HistoryLimit: options.historyLimit, CompleteHistory: options.completeHistory, Unsafe: options.unsafe, Version: Version, Executable: executable}
	if options.watch {
		return runDebugWatch(currentProject, projectOptions, options, jsonOutput, stdout, stderr)
	}
	snapshot, projectError := currentProject.Debug(projectOptions)
	if projectError != nil {
		return writeProjectError(stdout, stderr, jsonOutput, "debug", projectError)
	}
	if jsonOutput {
		return writeJSON(stdout, envelope{SchemaVersion: "2", Command: "debug", Success: true, SessionID: snapshotSessionID(snapshot), Result: snapshot})
	}
	return writeDebugHuman(stdout, snapshot)
}

func snapshotSessionID(snapshot project.DebugSnapshot) string {
	if snapshot.Session == nil {
		return ""
	}
	return snapshot.Session.ID
}

func writeDebugHuman(output io.Writer, snapshot project.DebugSnapshot) int {
	var text strings.Builder
	text.WriteString("Bandmaster debug snapshot\n")
	text.WriteString(fmt.Sprintf("Collection: %s", snapshot.Collection.Status))
	if snapshot.Collection.BestEffort {
		text.WriteString(" (best effort)")
	}
	text.WriteString("\n")
	text.WriteString(fmt.Sprintf("Runtime: bandmaster %s · %s · %s/%s\n", snapshot.Runtime.BandmasterVersion, snapshot.Runtime.GoVersion, snapshot.Runtime.GOOS, snapshot.Runtime.GOARCH))
	text.WriteString(fmt.Sprintf("Executable: %s\nProject: %s\nState: %s", snapshot.Runtime.Executable, snapshot.Repository.Root, snapshot.State.Initialization))
	if snapshot.State.SchemaVersion != "" {
		text.WriteString(" (schema " + snapshot.State.SchemaVersion + ")")
	}
	text.WriteString("\n")
	if snapshot.Session != nil {
		history := ""
		if snapshot.Session.Historical {
			history = " (historical)"
		}
		text.WriteString(fmt.Sprintf("Session: %s [%s]%s\n", snapshot.Session.ID, snapshot.Session.Status, history))
		text.WriteString(fmt.Sprintf("Agents: %d · Tasks: %d · Batches: %d · Diagnostics: %d\n", len(snapshot.Agents), len(snapshot.Tasks), len(snapshot.Batches), len(snapshot.Diagnostics)))
		for _, diagnostic := range snapshot.Diagnostics {
			text.WriteString(fmt.Sprintf("- %s [%s]", diagnostic.Code, diagnostic.Severity))
			if len(diagnostic.SuggestedActions) > 0 {
				text.WriteString(" → " + diagnostic.SuggestedActions[0])
			}
			text.WriteString("\n")
		}
	} else {
		text.WriteString("Session: none\n")
	}
	for _, collectionError := range snapshot.Collection.Errors {
		text.WriteString(fmt.Sprintf("Collection error: %s: %s\n", collectionError.Section, collectionError.Message))
	}
	if _, err := io.WriteString(output, text.String()); err != nil {
		return exitInternal
	}
	return 0
}

type debugStreamRecord struct {
	SchemaVersion string                         `json:"schema_version"`
	StreamVersion string                         `json:"stream_version"`
	Sequence      int64                          `json:"sequence"`
	Type          string                         `json:"type"`
	CapturedAt    string                         `json:"captured_at"`
	Snapshot      *project.DebugSnapshot         `json:"snapshot,omitempty"`
	Change        *project.DebugChange           `json:"change,omitempty"`
	Revision      *project.DebugRevisionBoundary `json:"revision,omitempty"`
	Collection    *project.DebugCollection       `json:"collection,omitempty"`
}

func writeStreamRecord(output io.Writer, record debugStreamRecord) error {
	return json.NewEncoder(output).Encode(record)
}

func runDebugDashboard(currentProject *project.Project, options project.DebugOptions, stdout, stderr io.Writer) int {
	if err := tui.RunDebug(currentProject, options, os.Stdin, stdout); err != nil {
		return writeError(stdout, stderr, false, "debug", "debug_dashboard_failed", fmt.Sprintf("Run debug dashboard: %v", err), false, exitInternal)
	}
	return 0
}

func runDebugWatch(currentProject *project.Project, projectOptions project.DebugOptions, options debugCLIOptions, jsonOutput bool, stdout, stderr io.Writer) int {
	if !jsonOutput {
		return runDebugDashboard(currentProject, projectOptions, stdout, stderr)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	current, projectError := currentProject.Debug(projectOptions)
	if projectError != nil {
		return writeProjectError(stdout, stderr, true, "debug", projectError)
	}
	if !options.followLatest && projectOptions.SessionID == "" && current.Session != nil {
		projectOptions.SessionID = current.Session.ID
	}
	sequence := int64(1)
	now := time.Now().UTC()
	if err := writeStreamRecord(stdout, debugStreamRecord{SchemaVersion: "2", StreamVersion: "2", Sequence: sequence, Type: "snapshot", CapturedAt: now.Format(time.RFC3339Nano), Snapshot: &current}); err != nil {
		return exitInternal
	}
	wasPartial := current.Collection.Status == "partial"
	lastGitInspection := time.Now()
	ticker := time.NewTicker(options.interval)
	heartbeat := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case captured := <-ticker.C:
			cachedRepository := current.Repository
			projectOptions.CachedRepository = &cachedRepository
			projectOptions.LastGitRevision = current.Revision.GitAfter
			projectOptions.ForceGit = captured.Sub(lastGitInspection) >= 2*time.Second
			next, projectError := currentProject.Debug(projectOptions)
			if projectError != nil {
				sequence++
				collection := project.DebugCollection{Status: "partial", Stable: false, CapturedAt: captured.UTC().Format(time.RFC3339Nano), Errors: []project.DebugCollectionError{{Section: "snapshot", Code: projectError.Code, Message: projectError.Message, Retryable: projectError.Retryable}}}
				if err := writeStreamRecord(stdout, debugStreamRecord{SchemaVersion: "2", StreamVersion: "2", Sequence: sequence, Type: "collection_error", CapturedAt: collection.CapturedAt, Collection: &collection}); err != nil {
					return exitInternal
				}
				wasPartial = true
				continue
			}
			if projectOptions.ForceGit || next.Revision.GitAfter != current.Revision.GitAfter {
				lastGitInspection = captured
			}
			if next.Collection.Status == "partial" {
				sequence++
				if err := writeStreamRecord(stdout, debugStreamRecord{SchemaVersion: "2", StreamVersion: "2", Sequence: sequence, Type: "collection_error", CapturedAt: captured.UTC().Format(time.RFC3339Nano), Collection: &next.Collection, Revision: &next.Revision}); err != nil {
					return exitInternal
				}
			} else if wasPartial {
				sequence++
				if err := writeStreamRecord(stdout, debugStreamRecord{SchemaVersion: "2", StreamVersion: "2", Sequence: sequence, Type: "recovered", CapturedAt: captured.UTC().Format(time.RFC3339Nano), Collection: &next.Collection, Revision: &next.Revision}); err != nil {
					return exitInternal
				}
			}
			wasPartial = next.Collection.Status == "partial"
			for _, change := range project.DebugChanges(current, next) {
				sequence++
				change := change
				if err := writeStreamRecord(stdout, debugStreamRecord{SchemaVersion: "2", StreamVersion: "2", Sequence: sequence, Type: "change", CapturedAt: captured.UTC().Format(time.RFC3339Nano), Change: &change, Revision: &next.Revision}); err != nil {
					return exitInternal
				}
			}
			current = next
		case captured := <-heartbeat.C:
			sequence++
			if err := writeStreamRecord(stdout, debugStreamRecord{SchemaVersion: "2", StreamVersion: "2", Sequence: sequence, Type: "heartbeat", CapturedAt: captured.UTC().Format(time.RFC3339Nano), Revision: &current.Revision, Collection: &current.Collection}); err != nil {
				return exitInternal
			}
		}
	}
}
