package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bandmaster-dev/bandmaster/internal/project"
	"github.com/bandmaster-dev/bandmaster/internal/tui"
)

const (
	exitInternal = 1
	exitInvalid  = 3
)

var Version = "dev"

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

func Run(args []string, stdout, stderr io.Writer) int {
	jsonOutput, prettyJSON, args := extractJSONFlags(args)
	command := commandName(args)
	if prettyJSON && !jsonOutput {
		return writeError(stdout, stderr, false, command, "invalid_arguments", "--pretty requires --json.", false, exitInvalid)
	}
	if prettyJSON {
		stdout = prettyJSONOutput{Writer: stdout}
	}
	if command == "" {
		return writeError(stdout, stderr, jsonOutput, "unknown", "invalid_arguments", "usage: bandmaster version | tui | init | config status | config approve <digest> | session <start|inspect|pause|resume|finish|abort> [--termination-confirmation <text>] | integrity recover --confirmation <text> | batch <freeze|validate|commit|inspect> [batch-id] | task <create|list|inspect|assign|replan|cancel|requeue|recover|repair|preflight|claim|release|heartbeat|diff|submit> [--json [--pretty]]", false, exitInvalid)
	}
	if command == "version" {
		result := versionResult{
			Version:                 Version,
			JSONSchemaVersion:       "1",
			JSONSchemaCompatibility: "Schema 1 fields will not be removed or change meaning; additive fields may be introduced.",
		}
		if jsonOutput {
			return writeJSON(stdout, envelope{SchemaVersion: "1", Command: command, Success: true, Result: result})
		}
		return writeHuman(stdout, "bandmaster %s\n", Version)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return writeError(stdout, stderr, jsonOutput, command, "internal_error", fmt.Sprintf("determine current directory: %v", err), false, exitInternal)
	}
	currentProject, projectError := project.Open(cwd)
	if projectError != nil {
		return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
	}
	if command == "monitor run" {
		if projectError := currentProject.RunIntegrityMonitor(args[2], args[3]); projectError != nil {
			return projectError.ExitCode
		}
		return 0
	}
	if command == "tui" {
		if jsonOutput {
			return writeError(stdout, stderr, true, command, "invalid_arguments", "The interactive TUI does not support --json.", false, exitInvalid)
		}
		if err := tui.Run(currentProject, os.Stdin, stdout); err != nil {
			return writeError(stdout, stderr, false, command, "tui_failed", fmt.Sprintf("Run interactive TUI: %v", err), false, exitInternal)
		}
		return 0
	}
	if mutatingCommand(command) {
		if projectError := currentProject.PrepareMutation(command); projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
	}

	switch command {
	case "init":
		result, projectError := currentProject.Initialize()
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		if jsonOutput {
			return writeJSON(stdout, envelope{SchemaVersion: "1", Command: command, Success: true, Result: result})
		}
		message := fmt.Sprintf("Initialized Bandmaster.\nValidation digest: %s\nApproved: %t\n", result.ValidationDigest, result.Approved)
		if !result.Approved {
			message += approvalGuidance(result.ValidationDigest)
		}
		return writeHuman(stdout, "%s", message)
	case "config status":
		result, projectError := currentProject.ConfigStatus()
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		if jsonOutput {
			return writeJSON(stdout, envelope{SchemaVersion: "1", Command: command, Success: true, Result: result})
		}
		message := fmt.Sprintf("Validation digest: %s\nApproved: %t\n", result.ValidationDigest, result.Approved)
		if !result.Approved {
			message += approvalGuidance(result.ValidationDigest)
		}
		return writeHuman(stdout, "%s", message)
	case "config approve":
		result, projectError := currentProject.Approve(args[2])
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		if jsonOutput {
			return writeJSON(stdout, envelope{SchemaVersion: "1", Command: command, Success: true, Result: result})
		}
		return writeHuman(stdout, "Approved validation configuration %s.\n", result.ValidationDigest)
	case "session start":
		result, projectError := currentProject.StartSession()
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeSession(stdout, jsonOutput, command, result)
	case "session inspect":
		result, projectError := currentProject.InspectSession()
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeSession(stdout, jsonOutput, command, result)
	case "session pause", "session resume", "session finish":
		result, projectError := currentProject.TransitionSession(args[1])
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeSession(stdout, jsonOutput, command, result)
	case "session abort":
		options, optionError := parseTaskOptions(args[2:], map[string]bool{"--termination-confirmation": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		result, projectError := currentProject.AbortSession(oneOption(options, "--termination-confirmation"))
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeSession(stdout, jsonOutput, command, result)
	case "integrity recover":
		options, optionError := parseTaskOptions(args[2:], map[string]bool{"--confirmation": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		result, projectError := currentProject.RecoverIntegrity(oneOption(options, "--confirmation"))
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeSession(stdout, jsonOutput, command, result)
	case "batch freeze":
		result, projectError := currentProject.FreezeBatch()
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeBatch(stdout, jsonOutput, command, result)
	case "batch validate":
		result, projectError := currentProject.ValidateBatch()
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeBatch(stdout, jsonOutput, command, result)
	case "batch commit", "batch finalize":
		result, projectError := currentProject.CommitBatch()
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeBatch(stdout, jsonOutput, command, result)
	case "batch inspect":
		batchID := ""
		if len(args) == 3 {
			batchID = args[2]
		}
		result, projectError := currentProject.InspectBatch(batchID)
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeBatch(stdout, jsonOutput, command, result)
	case "task create":
		options, optionError := parseTaskOptions(args[2:], map[string]bool{"--title": true, "--intent": true, "--expected-outcome": true, "--prerequisite": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		result, projectError := currentProject.CreateTask(project.TaskPlan{
			Title:           oneOption(options, "--title"),
			Intent:          oneOption(options, "--intent"),
			ExpectedOutcome: oneOption(options, "--expected-outcome"),
			Prerequisites:   options["--prerequisite"],
		})
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeTask(stdout, jsonOutput, command, result)
	case "task list":
		sessionID, result, projectError := currentProject.ListTasks()
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		if jsonOutput {
			return writeJSON(stdout, envelope{SchemaVersion: "1", Command: command, Success: true, SessionID: sessionID, Result: result})
		}
		if len(result.Tasks) == 0 {
			return writeHuman(stdout, "No tasks in session %s.\n", sessionID)
		}
		for _, task := range result.Tasks {
			if code := writeHuman(stdout, "%d. %s [%s] %s\n", task.CreationOrder, task.ID, task.Status, task.Title); code != 0 {
				return code
			}
		}
		return 0
	case "task inspect":
		result, projectError := currentProject.InspectTask(args[2])
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeTask(stdout, jsonOutput, command, result)
	case "task assign":
		options, optionError := parseTaskOptions(args[3:], map[string]bool{"--worker": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		result, projectError := currentProject.AssignTask(args[2], oneOption(options, "--worker"))
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeTask(stdout, jsonOutput, command, result)
	case "task replan":
		options, optionError := parseTaskOptions(args[3:], map[string]bool{"--title": true, "--intent": true, "--expected-outcome": true, "--prerequisite": true, "--terminated-worker": true, "--termination-proof": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		result, projectError := currentProject.ReplanTask(args[2], project.TaskPlan{
			Title:           oneOption(options, "--title"),
			Intent:          oneOption(options, "--intent"),
			ExpectedOutcome: oneOption(options, "--expected-outcome"),
			Prerequisites:   options["--prerequisite"],
		}, oneOption(options, "--terminated-worker"), oneOption(options, "--termination-proof"))
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeTask(stdout, jsonOutput, command, result)
	case "task cancel":
		options, optionError := parseTaskOptions(args[3:], map[string]bool{"--terminated-worker": true, "--termination-proof": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		result, projectError := currentProject.CancelTask(args[2], oneOption(options, "--terminated-worker"), oneOption(options, "--termination-proof"))
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeTask(stdout, jsonOutput, command, result)
	case "task requeue":
		result, projectError := currentProject.RequeueTask(args[2])
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeTask(stdout, jsonOutput, command, result)
	case "task recover":
		options, optionError := parseTaskOptions(args[3:], map[string]bool{"--terminated-worker": true, "--termination-proof": true, "--user-confirmation": true, "--diagnosis": true, "--intended-repair": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		result, projectError := currentProject.RecoverTask(args[2], project.RepairRequest{
			TerminatedWorker: oneOption(options, "--terminated-worker"),
			TerminationProof: oneOption(options, "--termination-proof"),
			UserConfirmation: oneOption(options, "--user-confirmation"),
			Diagnosis:        oneOption(options, "--diagnosis"),
			IntendedRepair:   oneOption(options, "--intended-repair"),
		})
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeTask(stdout, jsonOutput, command, result)
	case "task repair":
		options, optionError := parseTaskOptions(args[3:], map[string]bool{"--terminated-worker": true, "--termination-proof": true, "--user-confirmation": true, "--diagnosis": true, "--intended-repair": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		result, projectError := currentProject.RepairTask(args[2], project.RepairRequest{
			TerminatedWorker: oneOption(options, "--terminated-worker"),
			TerminationProof: oneOption(options, "--termination-proof"),
			UserConfirmation: oneOption(options, "--user-confirmation"),
			Diagnosis:        oneOption(options, "--diagnosis"),
			IntendedRepair:   oneOption(options, "--intended-repair"),
		})
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeTask(stdout, jsonOutput, command, result)
	case "task preflight", "task claim":
		options, optionError := parseTaskOptions(args[3:], map[string]bool{"--token": true, "--path": true, "--validation": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		validations := make([]project.FocusedValidation, 0, len(options["--validation"]))
		for _, encoded := range options["--validation"] {
			var validation project.FocusedValidation
			if err := json.Unmarshal([]byte(encoded), &validation); err != nil {
				return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", fmt.Sprintf("decode --validation JSON: %v", err), false, exitInvalid)
			}
			validations = append(validations, validation)
		}
		request := project.ClaimRequest{AssignmentToken: oneOption(options, "--token"), Paths: options["--path"], FocusedValidation: validations}
		if command == "task preflight" {
			result, projectError := currentProject.PreflightTask(args[2], request)
			if projectError != nil {
				return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
			}
			if jsonOutput {
				return writeJSON(stdout, envelope{SchemaVersion: "1", Command: command, Success: true, SessionID: result.SessionID, Result: result})
			}
			return writeHuman(stdout, "Preflight passed for task %s with %d path(s).\n", result.TaskID, len(result.Paths))
		}
		result, projectError := currentProject.ClaimTask(args[2], request)
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeTask(stdout, jsonOutput, command, result)
	case "task release":
		options, optionError := parseTaskOptions(args[3:], map[string]bool{"--token": true, "--path": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		result, projectError := currentProject.ReleaseTaskClaims(args[2], oneOption(options, "--token"), options["--path"])
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeTask(stdout, jsonOutput, command, result)
	case "task heartbeat":
		options, optionError := parseTaskOptions(args[3:], map[string]bool{"--token": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		result, projectError := currentProject.HeartbeatTask(args[2], oneOption(options, "--token"))
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeTask(stdout, jsonOutput, command, result)
	case "task diff":
		options, optionError := parseTaskOptions(args[3:], map[string]bool{"--token": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		result, projectError := currentProject.DiffTask(args[2], oneOption(options, "--token"))
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		if jsonOutput {
			return writeJSON(stdout, envelope{SchemaVersion: "1", Command: command, Success: true, SessionID: result.SessionID, Result: result})
		}
		for _, path := range result.Paths {
			if path.Patch != "" {
				if code := writeHuman(stdout, "%s", path.Patch); code != 0 {
					return code
				}
			}
		}
		return 0
	case "task submit":
		options, optionError := parseTaskOptions(args[3:], map[string]bool{"--token": true, "--behavior-changed": true, "--key-decisions": true, "--validation-expectations": true, "--known-risks": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		result, projectError := currentProject.SubmitTask(args[2], oneOption(options, "--token"), project.SubmissionHandoff{
			BehaviorChanged:        oneOption(options, "--behavior-changed"),
			KeyDecisions:           oneOption(options, "--key-decisions"),
			ValidationExpectations: oneOption(options, "--validation-expectations"),
			KnownRisks:             oneOption(options, "--known-risks"),
		})
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		return writeTask(stdout, jsonOutput, command, result)
	default:
		return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", "invalid command arguments", false, exitInvalid)
	}
}

func approvalGuidance(digest string) string {
	return fmt.Sprintf("Review .bandmaster.yaml, then run `bandmaster config approve %s`.\n", digest)
}

func commandName(args []string) string {
	switch {
	case len(args) == 1 && args[0] == "version":
		return "version"
	case len(args) == 1 && args[0] == "init":
		return "init"
	case len(args) == 1 && args[0] == "tui":
		return "tui"
	case len(args) == 2 && args[0] == "config" && args[1] == "status":
		return "config status"
	case len(args) == 3 && args[0] == "config" && args[1] == "approve":
		return "config approve"
	case len(args) == 2 && args[0] == "session" && (args[1] == "start" || args[1] == "inspect" || args[1] == "pause" || args[1] == "resume" || args[1] == "finish"):
		return "session " + args[1]
	case len(args) >= 2 && args[0] == "session" && args[1] == "abort":
		return "session " + args[1]
	case len(args) >= 2 && args[0] == "integrity" && args[1] == "recover":
		return "integrity recover"
	case len(args) == 2 && args[0] == "batch" && args[1] == "freeze":
		return "batch freeze"
	case len(args) == 2 && args[0] == "batch" && args[1] == "validate":
		return "batch validate"
	case len(args) == 2 && args[0] == "batch" && (args[1] == "commit" || args[1] == "finalize"):
		return "batch " + args[1]
	case (len(args) == 2 || len(args) == 3) && args[0] == "batch" && args[1] == "inspect":
		return "batch inspect"
	case len(args) == 4 && args[0] == "monitor" && args[1] == "run":
		return "monitor run"
	case len(args) >= 2 && args[0] == "task" && args[1] == "create":
		return "task create"
	case len(args) == 2 && args[0] == "task" && args[1] == "list":
		return "task list"
	case len(args) == 3 && args[0] == "task" && args[1] == "inspect":
		return "task inspect"
	case len(args) >= 3 && args[0] == "task" && args[1] == "assign":
		return "task assign"
	case len(args) >= 3 && args[0] == "task" && args[1] == "replan":
		return "task replan"
	case len(args) >= 3 && args[0] == "task" && args[1] == "cancel":
		return "task cancel"
	case len(args) == 3 && args[0] == "task" && args[1] == "requeue":
		return "task requeue"
	case len(args) >= 3 && args[0] == "task" && args[1] == "recover":
		return "task recover"
	case len(args) >= 3 && args[0] == "task" && args[1] == "repair":
		return "task repair"
	case len(args) >= 3 && args[0] == "task" && (args[1] == "preflight" || args[1] == "claim" || args[1] == "release" || args[1] == "heartbeat" || args[1] == "diff" || args[1] == "submit"):
		return "task " + args[1]
	default:
		return ""
	}
}

func mutatingCommand(command string) bool {
	switch command {
	case "init", "config approve", "session pause", "session finish", "session abort", "batch freeze", "batch validate", "batch commit", "batch finalize",
		"task create", "task assign", "task replan", "task cancel", "task requeue", "task recover", "task repair",
		"task preflight", "task claim", "task release", "task heartbeat", "task diff", "task submit":
		return true
	default:
		return false
	}
}

func parseTaskOptions(args []string, allowed map[string]bool) (map[string][]string, error) {
	options := make(map[string][]string)
	for index := 0; index < len(args); index += 2 {
		name := args[index]
		if !allowed[name] {
			return nil, fmt.Errorf("unknown option %s", name)
		}
		if index+1 >= len(args) {
			return nil, fmt.Errorf("option %s requires a value", name)
		}
		if name != "--prerequisite" && name != "--path" && name != "--validation" && len(options[name]) != 0 {
			return nil, fmt.Errorf("option %s may be specified only once", name)
		}
		options[name] = append(options[name], args[index+1])
	}
	return options, nil
}

func oneOption(options map[string][]string, name string) string {
	if len(options[name]) == 0 {
		return ""
	}
	return options[name][0]
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

func extractJSONFlags(args []string) (jsonOutput, prettyJSON bool, filtered []string) {
	filtered = make([]string, 0, len(args))
	for _, arg := range args {
		switch arg {
		case "--json":
			jsonOutput = true
		case "--pretty":
			prettyJSON = true
		default:
			filtered = append(filtered, arg)
		}
	}
	return jsonOutput, prettyJSON, filtered
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
