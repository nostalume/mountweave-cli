package cliapp

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

const (
	localFileOperand = "FILE|-"
	treePathOperand  = "PATH"
)

type commandSpec struct {
	name, summary    string
	operands         []string
	required         int
	options          []optionID
	exclusiveOperand optionID
	showSources      bool
	foreground       bool
	children         []commandSpec
}

var fileCommands = []commandSpec{
	{name: "ls", summary: "List directory contents. Defaults to the mounted root (/).", operands: []string{"ADDRESS"}, options: []optionID{limitOption}},
	{name: "stat", summary: "Show information about a file or directory.", operands: []string{"ADDRESS"}, required: 1},
	{name: "get", summary: "Download to a local file or stdout.", operands: []string{"ADDRESS", localFileOperand}, required: 2},
	{name: "put", summary: "Upload from a local file or stdin.", operands: []string{localFileOperand, "ADDRESS"}, required: 2, options: []optionID{replaceOption, sizeOption}},
	{name: "cp", summary: "Copy to an exact path or into a directory ending in /.", operands: []string{"SOURCE", "TARGET"}, required: 2, options: []optionID{recursiveOption, replaceOption}},
	{name: "mv", summary: "Move to an exact path or into a directory ending in /.", operands: []string{"SOURCE", "TARGET"}, required: 2},
	{name: "mkdir", summary: "Create a directory.", operands: []string{treePathOperand}, required: 1},
	{name: "trash", summary: "Move one file to the recycle bin.", operands: []string{"ADDRESS"}, required: 1},
	{name: "remove", summary: "Permanently delete one file.", operands: []string{"ADDRESS"}, required: 1},
	{name: "rmdir", summary: "Permanently delete an empty directory.", operands: []string{treePathOperand}, required: 1},
}

var commandSections = []struct {
	title    string
	commands []commandSpec
}{
	{"Work with files", append(slices.Clip(fileCommands), commandSpec{
		name: "shell", summary: "Run file commands in an interactive session.", operands: []string{treePathOperand}, foreground: true,
	})},
	{"Access over protocols", []commandSpec{
		{name: "serve", summary: "Serve configured exports in the foreground.", foreground: true},
	}},
	{"Manage settings", []commandSpec{
		{name: "mount", summary: "Manage saved mounts.", children: []commandSpec{
			{name: "add", summary: "Set up a mount.", operands: []string{"PATH"}, options: []optionID{fromOption}, exclusiveOperand: fromOption, showSources: true},
			{name: "list", summary: "List saved mounts."},
			{name: "edit", summary: "Edit a saved mount. Omit PATH to choose one interactively.", operands: []string{"PATH"}, options: []optionID{fromOption}},
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
	command                commandSpec
	config, from           string
	commandWords, operands []string
	help, json             bool
	limit                  int
	timeout                time.Duration
	replace, recursive     bool
	size                   *int64
}

func findCommand(words []string) (commandSpec, int) {
	for _, section := range commandSections {
		i := slices.IndexFunc(section.commands, func(c commandSpec) bool { return c.name == words[0] })
		if i < 0 {
			continue
		}
		command := section.commands[i]
		if len(words) == 1 || len(command.children) == 0 {
			return command, 1
		}
		if i := slices.IndexFunc(command.children, func(c commandSpec) bool { return c.name == words[1] }); i >= 0 {
			return command.children[i], 2
		}
	}
	return commandSpec{}, 0
}

func parse(args []string) (arguments, error) {
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
			return a, usage("Unknown option. Run mweave help.")
		}
		id := optionID(index)
		if seen[id] {
			return a, usage("An option was provided more than once.")
		}
		seen[id] = true
		takesValue := options[id].value != ""
		switch {
		case inline && !takesValue:
			return a, usage("This option does not take a value.")
		case !inline && takesValue:
			i++
			if i == len(args) || strings.HasPrefix(args[i], "--") {
				return a, usage("An option value is missing.")
			}
			value = args[i]
		}
		if takesValue && value == "" {
			return a, usage("An option value is empty.")
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
				return a, usage("--size requires a nonnegative byte count.")
			}
			a.size = &n
		case limitOption:
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return a, usage("--limit requires a positive integer.")
			}
			a.limit = n
		case timeoutOption:
			d, err := time.ParseDuration(value)
			if err != nil || d <= 0 {
				return a, usage("--timeout requires a positive duration, such as 10s.")
			}
			a.timeout = d
		}
	}

	if len(words) != 0 {
		if words[0] == "rm" {
			return a, usage("Use trash for recoverable file deletion, remove for permanent file deletion, or rmdir for an empty directory. Recursive deletion is unavailable in the CLI.")
		}
		command, depth := findCommand(words)
		if depth == 0 {
			return a, usage("Unknown command. Run mweave help.")
		}
		a.command, a.commandWords, a.operands = command, words[:depth], words[depth:]
	}
	command := a.command
	a.help = a.help || command.name == "" || command.name == "help" || len(command.children) != 0
	for id, supplied := range seen {
		if supplied && !slices.Contains(commonOptions, optionID(id)) && !slices.Contains(command.options, optionID(id)) {
			return a, usage("This option is unavailable for the selected command.")
		}
	}
	if command.exclusiveOperand != noOption && seen[command.exclusiveOperand] && len(a.operands) != 0 {
		return a, usage("Provide a mount path or a complete record through --from.")
	}
	if len(a.operands) > len(command.operands) || (!a.help && len(a.operands) < command.required) {
		return a, usage("Invalid operands. Run mweave " + strings.Join(a.commandWords, " ") + " --help.")
	}
	switch {
	case a.help:
	case command.name == "edit" && len(a.commandWords) == 2 && a.commandWords[0] == "mount" &&
		len(a.operands) == 0 && (a.json || a.from != ""):
		return a, usage("Provide a mount path when using --from or --json.")
	case command.name == "shell" && a.json:
		return a, usage("--json applies to individual file commands in shell.")
	case command.name == "get" && a.operands[1] == "-" && a.json:
		return a, usage("Use either stdout for the download or --json with a local destination.")
	case a.size != nil && a.operands[0] != "-":
		return a, usage("--size applies to put from stdin.")
	case a.recursive && a.replace:
		return a, usage("--replace applies to single-file copies.")
	case command.foreground && a.timeout != 0:
		return a, usage("--timeout applies to individual operations.")
	}
	return a, nil
}

func (app *App) helpText(a arguments) string {
	command := a.command
	var text strings.Builder
	if command.name == "" || command.name == "help" {
		text.WriteString("Usage: mweave [--config FILE] COMMAND\n")
		for _, section := range commandSections {
			fmt.Fprintf(&text, "\n%s\n", section.title)
			printCommands(&text, section.commands)
		}
		text.WriteString("\nRun mweave COMMAND --help for usage and options.\n")
		return text.String()
	}

	name := "mweave " + strings.Join(a.commandWords, " ")
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
			if command.foreground && (id == timeoutOption || command.name == "shell" && id == jsonOption) {
				continue
			}
			option := options[id]
			fmt.Fprintf(&text, "  %-20s %s\n", option.syntax(), option.summary)
		}
	}
	if command.showSources {
		text.WriteString("\nAvailable sources\n")
		_ = app.printSources(&text, app.sourceNames("", SourceConnection.available), false)
	}
	return text.String()
}

func printCommands(text *strings.Builder, commands []commandSpec) {
	for _, command := range commands {
		fmt.Fprintf(text, "  %-10s %s\n", command.name, command.summary)
	}
}
