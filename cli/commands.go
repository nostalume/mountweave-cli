package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

type optionID int

const (
	noOption optionID = iota
	helpOption
	configOption
	jsonOption
	timeoutOption
	limitOption
	fromOption
	replaceOption
	recursiveOption
	sizeOption
)

type optionSpec struct {
	name, value, summary string
}

var options = [...]optionSpec{
	helpOption:      {"--help", "", "Show help for this command."},
	configOption:    {"--config", "FILE", "Read settings from this file."},
	jsonOption:      {"--json", "", "Write a machine-readable result."},
	timeoutOption:   {"--timeout", "DURATION", "Cancel an operation after this duration."},
	limitOption:     {"--limit", "N", "Emit at most N entries."},
	fromOption:      {"--from", "FILE", "Read one mount record. Use - for stdin."},
	replaceOption:   {"--replace", "", "Publish the complete file, replacing an existing file if present."},
	recursiveOption: {"-r", "", "Operate on the directory tree."},
	sizeOption:      {"--size", "BYTES", "Declare the byte length of standard input."},
}

func (o optionSpec) syntax() string {
	if o.value == "" {
		return o.name
	}
	return o.name + " " + o.value
}

var commonOptions = []optionID{helpOption, configOption, jsonOption, timeoutOption}

type commandSpec struct {
	name, summary    string
	operands         []string
	required         int
	options          []optionID
	exclusiveOperand optionID
	showSources      bool
	children         []commandSpec
}

var commandSections = []struct {
	title    string
	commands []commandSpec
}{
	{"Work with files", []commandSpec{
		{name: "ls", summary: "List directory contents.", operands: []string{"ADDRESS"}, required: 1, options: []optionID{limitOption}},
		{name: "stat", summary: "Show information about a file or directory.", operands: []string{"ADDRESS"}, required: 1},
		{name: "get", summary: "Download to a local file or stdout.", operands: []string{"ADDRESS", "FILE|-"}, required: 2},
		{name: "put", summary: "Upload from a local file or stdin.", operands: []string{"FILE|-", "ADDRESS"}, required: 2, options: []optionID{replaceOption, sizeOption}},
		{name: "cp", summary: "Copy to an exact mounted address.", operands: []string{"SOURCE", "TARGET"}, required: 2, options: []optionID{recursiveOption, replaceOption}},
		{name: "mv", summary: "Move to an exact mounted address.", operands: []string{"SOURCE", "TARGET"}, required: 2},
		{name: "mkdir", summary: "Create a directory.", operands: []string{"PATH"}, required: 1},
		{name: "rm", summary: "Remove a file or an explicitly selected tree.", operands: []string{"ADDRESS"}, required: 1, options: []optionID{recursiveOption}},
		{name: "rmdir", summary: "Remove an empty directory.", operands: []string{"PATH"}, required: 1},
	}},
	{"Access over protocols", []commandSpec{
		{name: "serve", summary: "Serve configured exports in the foreground."},
	}},
	{"Manage settings", []commandSpec{
		{name: "mount", summary: "Manage saved mounts.", children: []commandSpec{
			{name: "add", summary: "Set up a mount.", operands: []string{"PATH"}, options: []optionID{fromOption}, exclusiveOperand: fromOption, showSources: true},
			{name: "list", summary: "List saved mounts."},
			{name: "edit", summary: "Edit a saved mount.", operands: []string{"PATH"}, required: 1, options: []optionID{fromOption}},
			{name: "remove", summary: "Remove a saved mount.", operands: []string{"PATH"}, required: 1},
		}},
		{name: "export", summary: "Manage saved protocol exports.", children: []commandSpec{
			{name: "add", summary: "Set up a WebDAV, JSON HTTP, SFTP, FTP, or S3 export.", options: []optionID{fromOption}},
		}},
	}},
	{"Other commands", []commandSpec{
		{name: "help", summary: "Show top-level help."},
		{name: "version", summary: "Show the build version."},
	}},
}

type arguments struct {
	command, config, from string
	operands              []string
	help, json            bool
	limit                 int
	timeout               time.Duration
	replace, recursive    bool
	size                  *int64
}

func findCommand(words []string) (commandSpec, int) {
	for _, section := range commandSections {
		for _, command := range section.commands {
			if command.name != words[0] {
				continue
			}
			if len(words) == 1 || len(command.children) == 0 {
				return command, 1
			}
			for _, child := range command.children {
				if child.name == words[1] {
					return child, 2
				}
			}
		}
	}
	return commandSpec{}, 0
}

func parse(args []string) (arguments, commandSpec, error) {
	var a arguments
	var words []string
	var seen [len(options)]bool
	parseOptions := true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if parseOptions && arg == "--" {
			parseOptions = false
			continue
		}
		if !parseOptions || !strings.HasPrefix(arg, "-") || arg == "-" {
			words = append(words, arg)
			continue
		}
		name, value, inline := strings.Cut(arg, "=")
		if name == "-h" {
			name = "--help"
		}
		index := slices.IndexFunc(options[:], func(o optionSpec) bool { return o.name == name })
		if index <= 0 {
			return a, commandSpec{}, usage("Unknown option. Run mweave help.")
		}
		id := optionID(index)
		if seen[id] {
			return a, commandSpec{}, usage("An option was provided more than once.")
		}
		seen[id] = true
		if options[id].value == "" {
			if inline {
				return a, commandSpec{}, usage("This option does not take a value.")
			}
		} else if !inline {
			i++
			if i == len(args) || strings.HasPrefix(args[i], "--") {
				return a, commandSpec{}, usage("An option value is missing.")
			}
			value = args[i]
		}
		if options[id].value != "" && value == "" {
			return a, commandSpec{}, usage("An option value is empty.")
		}
		switch id {
		case helpOption:
			a.help = true
		case jsonOption:
			a.json = true
		case configOption:
			a.config = value
		case fromOption:
			a.from = value
		case replaceOption:
			a.replace = true
		case recursiveOption:
			a.recursive = true
		case sizeOption:
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				return a, commandSpec{}, usage("--size requires a nonnegative byte count.")
			}
			a.size = &n
		case limitOption:
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return a, commandSpec{}, usage("--limit requires a positive integer.")
			}
			a.limit = n
		case timeoutOption:
			d, err := time.ParseDuration(value)
			if err != nil || d <= 0 {
				return a, commandSpec{}, usage("--timeout requires a positive duration, such as 10s.")
			}
			a.timeout = d
		}
	}

	var command commandSpec
	if len(words) == 0 {
		a.help = true
	} else {
		var depth int
		command, depth = findCommand(words)
		if depth == 0 {
			return a, command, usage("Unknown command. Run mweave help.")
		}
		a.command = strings.Join(words[:depth], " ")
		a.operands = words[depth:]
		a.help = a.help || a.command == "help" || len(command.children) != 0
	}
	for id, supplied := range seen {
		if supplied && !slices.Contains(commonOptions, optionID(id)) && !slices.Contains(command.options, optionID(id)) {
			return a, command, usage("This option is unavailable for the selected command.")
		}
	}
	if command.exclusiveOperand != noOption && seen[command.exclusiveOperand] && len(a.operands) != 0 {
		return a, command, usage("Provide a mount path or a complete record through --from.")
	}
	if len(a.operands) > len(command.operands) || (!a.help && len(a.operands) < command.required) {
		return a, command, usage("Invalid operands. Run mweave " + a.command + " --help.")
	}
	if !a.help {
		switch {
		case a.command == "get" && a.operands[1] == "-" && a.json:
			return a, command, usage("Use either stdout for the download or --json with a local destination.")
		case a.size != nil && a.operands[0] != "-":
			return a, command, usage("--size applies to put from stdin.")
		case a.recursive && a.replace:
			return a, command, usage("--replace applies to single-file copies.")
		case a.command == "serve" && a.timeout != 0:
			return a, command, usage("--timeout applies to one-shot commands. Stop serve with an interrupt.")
		}
	}
	return a, command, nil
}

func (app *App) helpText(a arguments, command commandSpec) string {
	var text strings.Builder
	if a.command == "" || a.command == "help" {
		text.WriteString("Usage: mweave [--config FILE] COMMAND\n")
		for _, section := range commandSections {
			fmt.Fprintf(&text, "\n%s\n", section.title)
			printCommands(&text, section.commands)
		}
		text.WriteString("\nRun mweave COMMAND --help for usage and options.\n")
		return text.String()
	}

	name := "mweave " + a.command
	if len(command.children) != 0 {
		fmt.Fprintf(&text, "Usage: %s COMMAND\n\n%s\n\n", name, command.summary)
		printCommands(&text, command.children)
		fmt.Fprintf(&text, "\nRun %s COMMAND --help for usage and options.\n", name)
		return text.String()
	}

	fmt.Fprintf(&text, "Usage: %s", name)
	for i, operand := range command.operands {
		if i < command.required {
			fmt.Fprintf(&text, " %s", operand)
		} else {
			fmt.Fprintf(&text, " [%s]", operand)
		}
	}
	for _, id := range command.options {
		if id == command.exclusiveOperand {
			fmt.Fprintf(&text, "\n       %s %s", name, options[id].syntax())
		} else {
			fmt.Fprintf(&text, " [%s]", options[id].syntax())
		}
	}
	fmt.Fprintf(&text, "\n\n%s\n\nOptions\n", command.summary)
	for _, ids := range [][]optionID{command.options, commonOptions} {
		for _, id := range ids {
			if a.command == "serve" && id == timeoutOption {
				continue
			}
			option := options[id]
			fmt.Fprintf(&text, "  %-20s %s\n", option.syntax(), option.summary)
		}
	}
	if command.showSources {
		text.WriteString("\nAvailable sources\n")
		_ = app.printSources(&text, app.sourceNames(""))
	}
	return text.String()
}

func printCommands(text *strings.Builder, commands []commandSpec) {
	for _, command := range commands {
		fmt.Fprintf(text, "  %-10s %s\n", command.name, command.summary)
	}
}
