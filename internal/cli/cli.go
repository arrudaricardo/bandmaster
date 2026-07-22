package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/bandmaster-dev/bandmaster/internal/project"
	"github.com/bandmaster-dev/bandmaster/internal/tui"
)

const (
	exitInternal = 1
	exitInvalid  = 3
)

var Version = "dev"

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
		return writeError(stdout, stderr, jsonOutput, "unknown", "invalid_arguments", "usage: bandmaster version | doctor | debug [--json] [--watch] | tui | init [--debug-skill] | config status | config approve <digest> | session <start|inspect|pause|resume|finish|abort> [--dry-run] [--termination-confirmation <text>] | integrity recover --confirmation <text> | finalization recover [--confirmation <text>] | batch <freeze|validate|commit|inspect|abandon> [--reason <text> --confirmation <text>] | task <create|list|inspect|assign|replan|cancel|requeue|recover|repair|preflight|claim|release|heartbeat|diff|submit> [--json [--pretty]]", false, exitInvalid)
	}
	installDebugSkill := false
	if command == "init" {
		var optionError error
		installDebugSkill, optionError = parseInitOptions(args[1:])
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
	}
	var debugOptions debugCLIOptions
	if command == "debug" {
		var optionError error
		debugOptions, optionError = parseDebugOptions(args[1:])
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		if debugOptions.watch && prettyJSON {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", "--pretty is not supported with --watch.", false, exitInvalid)
		}
	}
	abortConfirmation := ""
	abortDryRun := false
	if command == "session abort" {
		var optionError error
		abortConfirmation, abortDryRun, optionError = parseAbortOptions(args[2:])
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
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
	if command == "debug" {
		return runDebug(currentProject, debugOptions, jsonOutput, stdout, stderr)
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
		executable, _ := os.Executable()
		if err := tui.RunDebug(currentProject, project.DebugOptions{Version: Version, Executable: executable}, os.Stdin, stdout); err != nil {
			return writeError(stdout, stderr, false, command, "tui_failed", fmt.Sprintf("Run interactive TUI: %v", err), false, exitInternal)
		}
		return 0
	}
	if mutatingCommand(command) && !abortDryRun {
		if projectError := currentProject.PrepareMutation(command); projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
	}

	switch command {
	case "doctor":
		result, projectError := currentProject.Doctor()
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		if jsonOutput {
			return writeJSON(stdout, envelope{SchemaVersion: "1", Command: command, Success: true, Result: result})
		}
		if result.Healthy {
			return writeHuman(stdout, "Bandmaster state is healthy.\n")
		}
		return writeHuman(stdout, "Bandmaster found %d recovery issue(s); rerun with --json for structured evidence.\n", len(result.Findings))
	case "init":
		result, projectError := currentProject.Initialize(project.InitOptions{InstallDebugSkill: installDebugSkill})
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		if jsonOutput {
			return writeJSON(stdout, envelope{SchemaVersion: "1", Command: command, Success: true, Result: result})
		}
		message := fmt.Sprintf("Initialized Bandmaster.\nValidation digest: %s\nApproved: %t\n", result.ValidationDigest, result.Approved)
		if result.DebugSkillPath != "" {
			message += fmt.Sprintf("Debug skill: %s\n", result.DebugSkillPath)
		}
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
		if abortDryRun {
			result, projectError := currentProject.PlanAbort(abortConfirmation)
			if projectError != nil {
				return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
			}
			if jsonOutput {
				return writeJSON(stdout, envelope{SchemaVersion: "1", Command: command, Success: true, SessionID: result.SessionID, Result: result})
			}
			return writeHuman(stdout, "Abort plan for session %s: %d task(s), %d active claim(s), %d blocker(s).\n", result.SessionID, len(result.AffectedTasks), len(result.ActiveClaims), len(result.Blockers))
		}
		result, projectError := currentProject.AbortSession(abortConfirmation)
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
	case "finalization recover":
		options, optionError := parseTaskOptions(args[2:], map[string]bool{"--confirmation": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		result, projectError := currentProject.RecoverFinalization(oneOption(options, "--confirmation"))
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		if jsonOutput {
			return writeJSON(stdout, envelope{SchemaVersion: "1", Command: command, Success: true, SessionID: result.SessionID, Result: result})
		}
		return writeHuman(stdout, "Finalization recovery for batch %s: %s (%s).\n", result.BatchID, result.Action, result.Outcome)
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
	case "batch abandon":
		options, optionError := parseTaskOptions(args[2:], map[string]bool{"--reason": true, "--confirmation": true})
		if optionError != nil {
			return writeError(stdout, stderr, jsonOutput, command, "invalid_arguments", optionError.Error(), false, exitInvalid)
		}
		result, projectError := currentProject.AbandonBatch(oneOption(options, "--reason"), oneOption(options, "--confirmation"))
		if projectError != nil {
			return writeProjectError(stdout, stderr, jsonOutput, command, projectError)
		}
		if jsonOutput {
			return writeJSON(stdout, envelope{SchemaVersion: "1", Command: command, Success: true, SessionID: result.SessionID, Result: result})
		}
		return writeHuman(stdout, "Batch %s is abandoned.\nNext action: %s.\n", result.BatchID, result.NextAction)
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
