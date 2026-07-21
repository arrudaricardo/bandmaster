package cli

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
