package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/bandmaster-dev/bandmaster/internal/project"
)

type envelope struct {
	SchemaVersion string        `json:"schema_version"`
	Command       string        `json:"command"`
	Success       bool          `json:"success"`
	SessionID     string        `json:"session_id,omitempty"`
	Result        any           `json:"result,omitempty"`
	Error         *errorPayload `json:"error,omitempty"`
}

type errorPayload struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type versionResult struct {
	Version                 string `json:"version"`
	JSONSchemaVersion       string `json:"json_schema_version"`
	JSONSchemaCompatibility string `json:"json_schema_compatibility"`
}

type prettyJSONOutput struct{ io.Writer }

func approvalGuidance(digest string) string {
	return fmt.Sprintf("Review .bandmaster.yaml, then run `bandmaster config approve %s`.\n", digest)
}

func writeTask(output io.Writer, jsonOutput bool, command string, task project.Task) int {
	if jsonOutput {
		return writeJSON(output, envelope{SchemaVersion: "1", Command: command, Success: true, SessionID: task.SessionID, Result: task})
	}
	prerequisites := "none"
	if len(task.Prerequisites) > 0 {
		prerequisites = strings.Join(task.Prerequisites, ", ")
	}
	return writeHuman(output, "Task %s is %s.\nTitle: %s\nPrerequisites: %s\n", task.ID, task.Status, task.Title, prerequisites)
}

func writeSession(output io.Writer, jsonOutput bool, command string, session project.Session) int {
	if jsonOutput {
		return writeJSON(output, envelope{SchemaVersion: "1", Command: command, Success: true, SessionID: session.ID, Result: session})
	}
	message := fmt.Sprintf("Session %s is %s.\nStarting branch: %s\nStarting commit: %s\n", session.ID, session.Status, session.StartingBranch, session.StartingCommit)
	if command == "session inspect" {
		message += "Audit history:\n"
		for _, event := range session.AuditHistory {
			message += fmt.Sprintf("- %s %s: %s -> %s\n", event.OccurredAt, event.Event, event.FromStatus, event.ToStatus)
		}
	}
	return writeHuman(output, "%s", message)
}

func writeBatch(output io.Writer, jsonOutput bool, command string, batch project.Batch) int {
	if jsonOutput {
		return writeJSON(output, envelope{SchemaVersion: "1", Command: command, Success: true, SessionID: batch.SessionID, Result: batch})
	}
	return writeHuman(output, "Batch %s is %s.\nMembers: %d\nManifest paths: %d\n", batch.ID, batch.Status, len(batch.Members), len(batch.Manifest))
}

func writeProjectError(stdout, stderr io.Writer, jsonOutput bool, command string, err *project.Error) int {
	return writeErrorWithSession(stdout, stderr, jsonOutput, command, err.SessionID, err.Code, err.Message, err.Retryable, err.ExitCode)
}

func writeError(stdout, stderr io.Writer, jsonOutput bool, command, code, message string, retryable bool, exitCode int) int {
	return writeErrorWithSession(stdout, stderr, jsonOutput, command, "", code, message, retryable, exitCode)
}

func writeErrorWithSession(stdout, stderr io.Writer, jsonOutput bool, command, sessionID, code, message string, retryable bool, exitCode int) int {
	if jsonOutput {
		returnCode := writeJSON(stdout, envelope{
			SchemaVersion: "1",
			Command:       command,
			Success:       false,
			SessionID:     sessionID,
			Error:         &errorPayload{Code: code, Message: message, Retryable: retryable},
		})
		if returnCode != 0 {
			return returnCode
		}
		return exitCode
	}
	fmt.Fprintln(stderr, message)
	return exitCode
}

func writeJSON(output io.Writer, value envelope) int {
	encoder := json.NewEncoder(output)
	if _, ok := output.(prettyJSONOutput); ok {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(value); err != nil {
		return exitInternal
	}
	return 0
}

func writeHuman(output io.Writer, format string, args ...any) int {
	if _, err := fmt.Fprintf(output, format, args...); err != nil {
		return exitInternal
	}
	return 0
}
