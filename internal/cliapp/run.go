package cliapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"unicode"

	core "github.com/nostalume/mountweave"
	"golang.org/x/term"
)

// Main runs the process command line with terminal detection and signal cancellation.
func (app *App) Main() int {
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, term.IsTerminal(int(os.Stdin.Fd())))
}

// Run executes one invocation using the supplied streams and cancellation context.
func (app *App) Run(ctx context.Context, argv []string, in io.Reader, out, diagnostics io.Writer, terminal bool) int {
	a, command, err := parse(argv)
	if err != nil {
		// Option parsing can fail before it sees the requested output mode.
		for _, arg := range argv {
			if arg == "--" {
				break
			}
			a.json = a.json || arg == "--json"
		}
		return app.respond(a.json, out, diagnostics, nil, "", err)
	}
	if a.help {
		text := app.helpText(a, command)
		return app.respond(a.json, out, diagnostics, text, text, nil)
	}
	if a.command == "version" {
		version := "devel"
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "(devel)" && info.Main.Version != "" {
			version = info.Main.Version
		}
		return app.respond(a.json, out, diagnostics, version, "mweave "+version+"\n", nil)
	}
	if a.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.timeout)
		defer cancel()
	}
	if a.config == "" {
		a.config, err = DefaultConfigPath()
		if err != nil {
			return app.respond(a.json, out, diagnostics, nil, "", &Problem{Code: "invalid_config", Message: "The user configuration directory is unavailable.", NextAction: "Provide --config FILE."})
		}
	}
	if (a.json || !terminal) && a.from == "" && slices.Contains(command.options, fromOption) {
		return app.respond(a.json, out, diagnostics, nil, "", &Problem{Code: "interaction_required", Message: "Setup requires a terminal or a configuration record.", NextAction: "Provide a record with --from FILE or --from -."})
	}
	saved, err := LoadConfig(a.config)
	if err != nil {
		return app.respond(a.json, out, diagnostics, nil, "", err)
	}
	switch {
	case a.command == "serve":
		return app.runServe(ctx, saved, a.json, out, diagnostics)
	case a.command == "export add":
		result, line, err := addExport(ctx, saved, a, in, diagnostics)
		return app.respond(a.json, out, diagnostics, result, line, err)
	case strings.HasPrefix(a.command, "mount "):
		result, line, err := app.runMount(ctx, saved, a, in, diagnostics)
		return app.respond(a.json, out, diagnostics, result, line, err)
	default:
		return app.runFile(ctx, saved, a, in, out, diagnostics)
	}
}

func readRecord(ctx context.Context, name string, in io.Reader) (_ []byte, err error) {
	if name != "-" {
		return os.ReadFile(name)
	}
	if file, ok := in.(*os.File); ok {
		pipe, release, setupErr := pollablePipe(file)
		if setupErr != nil {
			return nil, setupErr
		}
		if release != nil {
			defer func() { err = errors.Join(err, release()) }()
		}
		in = pipe
	}
	if closer, ok := in.(io.Closer); ok {
		defer closeOnCancel(ctx, closer)()
	}
	return io.ReadAll(in)
}

type setupField struct {
	label string
	value *string
}

func Field(label string, value *string) setupField { return setupField{label, value} }

func AskFields(ask Prompt, fields ...setupField) error {
	for _, field := range fields {
		value, err := ask(field.label, *field.value)
		if err != nil {
			return err
		}
		*field.value = value
	}
	return nil
}

func promptInput(in io.Reader, out io.Writer) Prompt {
	input := bufio.NewReader(in)
	return func(label, current string) (string, error) {
		prompt := label
		if current != "" {
			prompt += fmt.Sprintf(" [%q]", current)
		}
		if _, err := fmt.Fprint(out, prompt+": "); err != nil {
			return "", err
		}
		line, err := input.ReadString('\n')
		if err != nil && !(err == io.EOF && line != "") {
			return "", err
		}
		value := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if value == "" {
			value = current
		}
		return value, nil
	}
}

func PromptReference(raw string) (*CredentialRef, error) {
	kind, name, ok := strings.Cut(raw, ":")
	if !ok || name == "" {
		return nil, InvalidConfig("Use a credential reference such as env:NAME, file:PATH, or profile:NAME as requested.")
	}
	if kind == "file" {
		var err error
		name, err = filepath.Abs(name)
		if err != nil {
			return nil, err
		}
	}
	return &CredentialRef{Kind: kind, Ref: name}, nil
}

func AskCredentialReference(ask Prompt, label string, prior *CredentialRef,
	parse func(string) (*CredentialRef, error), check func(*CredentialRef) error) (*CredentialRef, error) {
	current := ""
	if prior != nil {
		current = "keep"
	}
	prompt := label
	for {
		raw, err := ask(prompt, current)
		if err != nil {
			return nil, err
		}
		next := prior
		if raw != "keep" || prior == nil {
			next, err = parse(raw)
		}
		if err == nil {
			err = check(next)
		}
		if err == nil {
			return next, nil
		}
		prompt = AppProblem(nil, err).Message + "\n" + label
	}
}

func AskChoice(ask Prompt, label, current string, choices ...string) (string, error) {
	prompt := label
	for {
		answer, err := ask(prompt, current)
		if err != nil {
			return "", err
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if slices.Contains(choices, answer) {
			return answer, nil
		}
		prompt = "Invalid choice. Try again.\n" + label
	}
}

func Confirm(ask Prompt, label string) error {
	answer, err := AskChoice(ask, label+" [y/N]", "", "y", "yes", "n", "no", "")
	if err == nil && answer != "y" && answer != "yes" {
		err = context.Canceled
	}
	return err
}

func userSettingsDir() (string, error) {
	if runtime.GOOS != "windows" {
		return os.UserConfigDir()
	}
	dir := os.Getenv("LOCALAPPDATA")
	if !filepath.IsAbs(dir) {
		return "", errors.New("%LOCALAPPDATA% is missing or is not an absolute path")
	}
	return dir, nil
}

func DefaultConfigPath() (string, error) {
	dir, err := userSettingsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mweave", "config.json"), nil
}

type Problem struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	NextAction string `json:"next_action,omitempty"`
	Reconnect  bool   `json:"-"`
}

func (p *Problem) Error() string { return p.Message }
func usage(message string) error { return &Problem{Code: "usage", Message: message} }

func AppProblem(app *App, err error) *Problem {
	if err == nil {
		return nil
	}
	var p *Problem
	var file *core.Error
	errors.As(err, &p)
	if app != nil && app.projectProblem != nil && (p == nil || p.Reconnect) {
		if projected := app.projectProblem(err); projected != nil {
			return projected
		}
	}
	switch {
	case p != nil:
		return p
	case errors.Is(err, context.DeadlineExceeded):
		return &Problem{Code: "timeout", Message: "The operation timed out."}
	case errors.Is(err, context.Canceled):
		return &Problem{Code: "cancelled", Message: "The operation was cancelled."}
	case errors.As(err, &file):
		return &Problem{Code: string(file.Code), Message: strings.ReplaceAll(string(file.Code), "_", " ") + "."}
	case errors.Is(err, os.ErrNotExist):
		return &Problem{Code: "not_found", Message: "The requested file or directory does not exist."}
	case errors.Is(err, os.ErrPermission):
		return &Problem{Code: "permission_denied", Message: "Access to a required file or directory was denied."}
	default:
		return &Problem{Code: "io_error", Message: "The operation could not complete because of a local I/O error."}
	}
}

type response struct {
	Result any      `json:"result"`
	Error  *Problem `json:"error"`
}

func MountEditAction(configPath, mountAt string) string {
	if strings.ContainsFunc(configPath, unicode.IsControl) || strings.ContainsFunc(mountAt, unicode.IsControl) {
		return fmt.Sprintf("Use mweave mount edit for mount %q with configuration file %q.", mountAt, configPath)
	}
	action := "mweave mount edit " + shellArgument(mountAt)
	if defaultPath, _ := DefaultConfigPath(); configPath != defaultPath {
		action += " --config " + shellArgument(configPath)
	}
	return action
}

func shellArgument(value string) string {
	if runtime.GOOS == "windows" {
		// PowerShell recognizes typographic apostrophes as quote delimiters too.
		return "'" + strings.NewReplacer("'", "''", "‘", "‘‘", "’", "’’", "‚", "‚‚", "‛", "‛‛").Replace(value) + "'"
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func (app *App) respond(machine bool, out, diagnostics io.Writer, result any, line string, err error) int {
	p := AppProblem(app, err)
	var writeErr error
	if machine {
		writeErr = json.NewEncoder(out).Encode(response{Result: result, Error: p})
	} else {
		if line != "" {
			_, writeErr = io.WriteString(out, line)
		} else if receipt, ok := result.(fileReceipt); ok {
			writeErr = receipt.write(out, true)
		}
		if p != nil {
			printProblem(diagnostics, p)
		}
	}
	if writeErr != nil {
		outputFailure(diagnostics)
		switch receipt := result.(type) {
		case MountReceipt:
			fmt.Fprintf(diagnostics, "Configuration effect for %q: %s.\n", receipt.At, receipt.Effect)
		case exportReceipt:
			fmt.Fprintf(diagnostics, "Configuration effect for %q: %s.\n", receipt.Listen, receipt.Effect)
		case fileReceipt:
			_ = receipt.write(diagnostics, false)
		}
		return 1
	}
	return status(p)
}

func status(p *Problem) int {
	if p == nil {
		return 0
	}
	if p.Code == "usage" {
		return 2
	}
	return 1
}

func printProblem(w io.Writer, p *Problem) {
	fmt.Fprintf(w, "%s: %s\n", p.Code, p.Message)
	if p.NextAction != "" {
		fmt.Fprintln(w, p.NextAction)
	}
}

func outputFailure(w io.Writer) {
	fmt.Fprintln(w, "output_error: The result could not be written.")
}
