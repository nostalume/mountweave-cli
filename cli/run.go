package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/alipan/credential"
	"golang.org/x/term"
)

// Main runs the process command line with terminal detection and signal cancellation.
func (app *App) Main() int {
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, term.IsTerminal(int(os.Stdin.Fd())))
}

type problem struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	NextAction string `json:"next_action,omitempty"`
}

func (p *problem) Error() string { return p.Message }
func usage(message string) error { return &problem{Code: "usage", Message: message} }

func publicError(err error) *problem {
	if err == nil {
		return nil
	}
	var p *problem
	var file *core.Error
	provider := websiteSignInProvider(err)
	switch {
	case errors.As(err, &p):
		return p
	case provider != "":
		return &problem{Code: "interaction_required", Message: "The " + provider + " website needs sign-in.", NextAction: "Use mount edit to sign in again."}
	case errors.Is(err, credential.ErrPersistence):
		return &problem{Code: "credential_unavailable", Message: "The authorization checkpoint could not be saved.", NextAction: "Check the protected credential store for this user."}
	case errors.Is(err, credential.ErrRefreshUncertain):
		return &problem{Code: "interaction_required", Message: "The authorization refresh outcome could not be confirmed.", NextAction: "Check the authorization with the provider before retrying."}
	case errors.Is(err, context.DeadlineExceeded):
		return &problem{Code: "timeout", Message: "The operation timed out."}
	case errors.Is(err, context.Canceled):
		return &problem{Code: "cancelled", Message: "The operation was cancelled."}
	case errors.As(err, &file):
		return &problem{Code: string(file.Code), Message: strings.ReplaceAll(string(file.Code), "_", " ") + "."}
	case errors.Is(err, os.ErrNotExist):
		return &problem{Code: "not_found", Message: "The requested file or directory does not exist."}
	case errors.Is(err, os.ErrPermission):
		return &problem{Code: "permission_denied", Message: "Access to a required file or directory was denied."}
	default:
		return &problem{Code: "io_error", Message: "The operation could not complete because of a local I/O error."}
	}
}

type response struct {
	Result any      `json:"result"`
	Error  *problem `json:"error"`
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
		return respond(a.json, out, diagnostics, nil, "", err)
	}
	if a.help {
		text := app.helpText(a, command)
		return respond(a.json, out, diagnostics, text, text, nil)
	}
	switch a.command {
	case "version":
		version := "devel"
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "(devel)" && info.Main.Version != "" {
			version = info.Main.Version
		}
		return respond(a.json, out, diagnostics, version, "mweave "+version+"\n", nil)
	}
	if a.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.timeout)
		defer cancel()
	}
	if a.config == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return respond(a.json, out, diagnostics, nil, "", &problem{Code: "invalid_config", Message: "The user configuration directory is unavailable.", NextAction: "Provide --config FILE."})
		}
		a.config = filepath.Join(dir, "mweave", "config.json")
	}
	setup := a.command == "mount add" || a.command == "mount edit" || a.command == "export add"
	if setup && a.from == "" && (a.json || !terminal) {
		return respond(a.json, out, diagnostics, nil, "", &problem{Code: "interaction_required", Message: "Setup requires a terminal or a configuration record.", NextAction: "Provide a record with --from FILE or --from -."})
	}
	saved, err := loadConfig(a.config)
	if err != nil {
		return respond(a.json, out, diagnostics, nil, "", err)
	}
	switch {
	case a.command == "serve":
		return app.runServe(ctx, saved, a.json, out, diagnostics)
	case a.command == "export add":
		result, line, err := addExport(ctx, saved, a, in, diagnostics)
		return respond(a.json, out, diagnostics, result, line, err)
	case strings.HasPrefix(a.command, "mount "):
		result, line, err := app.runMount(ctx, saved, a, in, diagnostics)
		return respond(a.json, out, diagnostics, result, line, err)
	default:
		return app.runFile(ctx, saved, a, in, out, diagnostics)
	}
}

func respond(machine bool, out, diagnostics io.Writer, result any, line string, err error) int {
	p := publicError(err)
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
		case mountReceipt:
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

func status(p *problem) int {
	if p == nil {
		return 0
	}
	if p.Code == "usage" {
		return 2
	}
	return 1
}

func printProblem(w io.Writer, p *problem) {
	fmt.Fprintf(w, "%s: %s\n", p.Code, p.Message)
	if p.NextAction != "" {
		fmt.Fprintln(w, p.NextAction)
	}
}

func outputFailure(w io.Writer) {
	fmt.Fprintln(w, "output_error: The result could not be written.")
}
