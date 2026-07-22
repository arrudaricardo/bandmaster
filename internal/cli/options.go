package cli

import "fmt"

func parseInitOptions(args []string) (bool, error) {
	installDebugSkill := false
	for _, arg := range args {
		if arg != "--debug-skill" {
			return false, fmt.Errorf("unknown option %s", arg)
		}
		if installDebugSkill {
			return false, fmt.Errorf("option --debug-skill may be specified only once")
		}
		installDebugSkill = true
	}
	return installDebugSkill, nil
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

func parseAbortOptions(args []string) (terminationConfirmation string, dryRun bool, err error) {
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--dry-run":
			if dryRun {
				return "", false, fmt.Errorf("option --dry-run may be specified only once")
			}
			dryRun = true
		case "--termination-confirmation":
			if terminationConfirmation != "" {
				return "", false, fmt.Errorf("option --termination-confirmation may be specified only once")
			}
			if index+1 >= len(args) {
				return "", false, fmt.Errorf("option --termination-confirmation requires a value")
			}
			index++
			terminationConfirmation = args[index]
		default:
			return "", false, fmt.Errorf("unknown option %s", args[index])
		}
	}
	return terminationConfirmation, dryRun, nil
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
