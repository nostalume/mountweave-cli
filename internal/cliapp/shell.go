package cliapp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"slices"
	"strings"
	"time"

	core "github.com/nostalume/mountweave"
)

func (app *App) runShell(ctx context.Context, saved ConfigSnapshot, a arguments, in io.Reader, out, diagnostics io.Writer, interrupts <-chan os.Signal) (code int) {
	rt, err := newFileRuntime()
	if err != nil {
		return app.respond(false, out, diagnostics, nil, "", err)
	}
	defer func() {
		if err := CloseRuntime(rt.core); err != nil {
			fmt.Fprintf(diagnostics, "shutdown_failed: %s\n", AppProblem(app, err).Message)
			code = max(code, 1)
		}
	}()
	if interrupts == nil {
		// An embedded run can own a pipe instead of a process terminal.
		defer closeOnCancel(ctx, in)()
	}
	// A file command may close its output to interrupt a pipe write. The
	// terminal belongs to the session and cannot be closed by a command.
	output := &shellOutput{Writer: out}
	input := bufio.NewReader(in)
	directory := "/"
	changeDirectory := func(operand string) error {
		op, cancel := commandContext(ctx, interrupts, 0)
		defer cancel()
		if strings.HasPrefix(operand, "key:") {
			return core.Fail(core.UnsupportedOperation, nil)
		}
		target := core.Path(mountedPath(shellPath(directory, operand), true))
		if err := app.installFor(op, saved, rt, true, target); err != nil {
			return err
		}
		entry, err := rt.core.Stat(op, LocalActor, target)
		if err != nil {
			return app.fileError(saved, []core.Address{target}, err, "", "")
		}
		if entry.Kind != core.Directory {
			return core.Fail(core.NotDirectory, nil)
		}
		directory = target.Value()
		return nil
	}
	if len(a.operands) != 0 {
		if err := changeDirectory(a.operands[0]); err != nil {
			return app.respond(false, output, diagnostics, nil, "", err)
		}
	}
	for ctx.Err() == nil {
		prompt := "mweave:" + displayText(directory) + "> "
		if _, err := io.WriteString(diagnostics, prompt); err != nil {
			return 1
		}
		line, readErr := readShellLine(ctx, input, diagnostics, prompt, interrupts)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			if ctx.Err() != nil {
				return 0
			}
			return app.respond(false, output, diagnostics, nil, "", readErr)
		}
		words, err := shellWords(line)
		if err == nil && len(words) != 0 {
			switch words[0] {
			case "exit", "pwd", "help":
				if len(words) != 1 {
					err = usage(words[0] + " takes no operands.")
				} else if words[0] == "exit" {
					return 0
				} else if words[0] == "pwd" {
					fmt.Fprintln(output, displayText(directory))
				} else {
					var help strings.Builder
					help.WriteString("Use cd PATH, pwd, exit, or a file command. Add --help to a file command for usage.\n\n")
					printCommands(&help, fileCommands)
					io.WriteString(output, help.String())
				}
			case "cd":
				if len(words) != 2 || words[1] == "" {
					err = usage("Usage: cd DIRECTORY")
				} else {
					err = changeDirectory(words[1])
				}
			default:
				err = app.shellFile(ctx, interrupts, saved, rt, directory, words, output, diagnostics)
			}
		}
		if err != nil {
			app.respond(false, output, diagnostics, nil, "", err)
		}
		if output.err != nil {
			return 1
		}
		if readErr != nil {
			return 0
		}
	}
	return 0
}

func (app *App) shellFile(ctx context.Context, interrupts <-chan os.Signal, saved ConfigSnapshot, rt *fileRuntime, directory string, words []string, out, diagnostics io.Writer) error {
	cmd, err := parse(words)
	if err != nil {
		return err
	}
	if cmd.config != "" || len(cmd.commandWords) != 1 ||
		!slices.ContainsFunc(fileCommands, func(spec commandSpec) bool { return spec.name == cmd.command.name }) {
		return usage("Shell accepts file commands with its current configuration.")
	}
	if cmd.help {
		_, err := io.WriteString(out, app.helpText(cmd))
		return err
	}
	if cmd.command.name == "ls" && len(cmd.operands) == 0 {
		cmd.operands = []string{directory}
	}
	for i, operand := range cmd.operands {
		if cmd.command.operands[i] == localFileOperand {
			if operand == "-" {
				return usage("Use a local file path for transfers in shell.")
			}
		} else if !strings.HasPrefix(operand, "key:") {
			cmd.operands[i] = shellPath(directory, operand)
		}
	}
	op, cancel := commandContext(ctx, interrupts, cmd.timeout)
	defer cancel()
	app.runFile(op, saved, cmd, nil, out, diagnostics, rt)
	return nil
}

func shellPath(directory, operand string) string {
	if operand == "" {
		return ""
	}
	operand = strings.ReplaceAll(operand, "\\", "/")
	if !strings.HasPrefix(operand, "/") {
		operand = strings.TrimSuffix(directory, "/") + "/" + operand
	}
	if strings.Contains(operand, "//") {
		return operand
	}
	target := path.Clean(operand)
	if target != "/" && strings.HasSuffix(operand, "/") {
		target += "/"
	}
	return target
}

func shellWords(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	var quote byte
	started := false
	for i := range len(line) {
		c := line[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				word.WriteByte(c)
			}
		case c == '"' || c == '\'':
			started, quote = true, c
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		default:
			started = true
			word.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, usage("Close the quoted argument on the same line.")
	}
	if started {
		words = append(words, word.String())
	}
	return words, nil
}

func commandContext(parent context.Context, interrupts <-chan os.Signal, timeout time.Duration) (context.Context, func()) {
	if interrupts == nil && timeout == 0 {
		return parent, func() {}
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
	if interrupts == nil {
		return ctx, cancel
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-interrupts:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		cancel()
		<-done
	}
}

func readShellLine(ctx context.Context, input *bufio.Reader, diagnostics io.Writer, prompt string, interrupts <-chan os.Signal) (string, error) {
	if interrupts == nil {
		return input.ReadString('\n')
	}
	// Only the process terminal needs asynchronous reads. At shutdown, Main
	// can leave an idle kernel read to the terminating process.
	type reply struct {
		line string
		err  error
	}
	ready := make(chan reply, 1)
	read := func() {
		go func() {
			line, err := input.ReadString('\n')
			ready <- reply{line, err}
		}()
	}
	read()
	redraw := func() error {
		_, err := io.WriteString(diagnostics, "\n"+prompt)
		return err
	}
	var interruptedAt time.Time
	for {
		select {
		case result := <-ready:
			if runtime.GOOS != "windows" || result.line != "" || !errors.Is(result.err, io.EOF) {
				return result.line, result.err
			}
			// A cooked Windows console may return an empty EOF while the
			// same Ctrl+C reaches os/signal separately.
			if time.Since(interruptedAt) > consoleInterruptWindow {
				if !waitConsoleInterrupt(ctx, interrupts) {
					return result.line, result.err
				}
				if err := redraw(); err != nil {
					return "", err
				}
			}
			interruptedAt = time.Time{}
			read()
		case <-interrupts:
			interruptedAt = time.Now()
			if err := redraw(); err != nil {
				return "", err
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

const consoleInterruptWindow = 100 * time.Millisecond

func waitConsoleInterrupt(ctx context.Context, interrupts <-chan os.Signal) bool {
	timer := time.NewTimer(consoleInterruptWindow)
	defer timer.Stop()
	select {
	case <-interrupts:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

type shellOutput struct {
	io.Writer
	err error
}

func (out *shellOutput) Write(data []byte) (int, error) {
	if out.err != nil {
		return 0, out.err
	}
	n, err := out.Writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	out.err = err
	return n, err
}
