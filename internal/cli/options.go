package cli

import "fmt"

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
