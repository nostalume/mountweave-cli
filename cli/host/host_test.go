package host_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/cli/host"
	localsource "github.com/nostalume/mountweave-cli/cli/source/local"
	"github.com/nostalume/mountweave-cli/internal/filelock"
	"github.com/nostalume/mountweave/drivers/local"
)

type receipt struct {
	Result json.RawMessage
	Error  *struct{ Code string }
}

func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type countedDriver struct {
	*local.Driver
	closed *int
}

func (d countedDriver) Close() error {
	*d.closed++
	return d.Driver.Close()
}

func TestSelectedHost(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	config := filepath.Join(base, "settings.json")
	root, stage := t.TempDir(), t.TempDir()
	require(t, os.WriteFile(filepath.Join(root, "report"), []byte("original"), 0600))
	t.Setenv("TEST_MWEAVE_HOST_CREDENTIAL", "sealed-value")
	type options struct {
		Origin string `json:"origin"`
		Broken bool   `json:"broken,omitempty"`
	}
	var opens, closes int
	source, err := host.NewSource("fixture", "Sealed fixture", func(o *options, ask host.Prompt) (err error) {
		o.Origin, err = ask("Origin", o.Origin)
		return err
	}, func(o options, configDir string) (host.PreparedSource, error) {
		if o.Origin == "" || configDir != base {
			return host.PreparedSource{}, fmt.Errorf("invalid options or config directory: %v, %q", o, configDir)
		}
		return host.PreparedSource{CredentialOrigin: o.Origin, Open: func(_ context.Context, secret string) (core.Driver, error) {
			opens++
			if secret != "sealed-value" {
				return nil, fmt.Errorf("unexpected credential")
			}
			d, err := local.New(root, local.Options{StagingDirectory: stage, MaxStagingBytes: 1 << 20})
			if err != nil {
				return nil, err
			}
			var openErr error
			if o.Broken {
				openErr = core.Fail(core.UpstreamFailure, nil)
			}
			return countedDriver{d, &closes}, openErr
		}}, nil
	})
	require(t, err)
	app, err := host.New(source)
	require(t, err)
	invoke := func(input string, terminal bool, want int, args ...string) string {
		t.Helper()
		var out, diag bytes.Buffer
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		got := app.Run(ctx, append([]string{"--config", config}, args...), strings.NewReader(input), &out, &diag, terminal)
		if got != want {
			t.Fatalf("%v: exit=%d, want %d: %s %s", args, got, want, &out, &diag)
		}
		return out.String()
	}
	run := func(input string, want int, args ...string) receipt {
		t.Helper()
		raw := invoke(input, false, want, append(args, "--json")...)
		var result receipt
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatalf("command receipt: %q (%v)", raw, err)
		}
		return result
	}
	effects := func(want int) {
		t.Helper()
		if opens != want || closes != want {
			t.Fatalf("opened %d and closed %d, want %d each", opens, closes, want)
		}
	}
	problem := func(result receipt, want string) {
		t.Helper()
		if result.Error == nil || result.Error.Code != want {
			t.Fatalf("expected %s: %+v %s", want, result.Error, result.Result)
		}
	}
	exported := func() []byte {
		t.Helper()
		raw, err := os.ReadFile(config)
		require(t, err)
		return raw
	}
	record := func(at, origin string, broken bool) string {
		return fmt.Sprintf(`{"at":%q,"driver":"fixture","options":{"origin":%q,"broken":%t},"credential":{"kind":"env","ref":"TEST_MWEAVE_HOST_CREDENTIAL"}}`, at, origin, broken)
	}

	invoke("", true, 0, "mount", "add", "--help")
	run("", 0, "mount", "list")
	effects(0)
	invoke("origin-one\nenv:TEST_MWEAVE_HOST_CREDENTIAL\ny\n", true, 0, "mount", "add", "guided")
	effects(1)
	run(record("/script", "origin-one", false), 0, "mount", "add", "--from", "-")
	if got := invoke("", false, 0, "ls"); got != "guided/\nscript/\n" {
		t.Fatalf("mount root listing: %q", got)
	}
	if got := run("", 0, "ls", "/", "--limit", "1"); !bytes.Contains(got.Result, []byte("\"state\":\"limited\"")) {
		t.Fatalf("root listing limit: %s", got.Result)
	}
	if got := invoke("", false, 0, "mount", "list"); !strings.Contains(got, "/script  ·  fixture\n") {
		t.Fatalf("mount list: %q", got)
	}
	problem(run("", 2, "rm", "-r", "script/../guided"), "usage")
	problem(run("", 1, "remove", "script/report/"), "invalid_path")
	effects(2)
	before := exported()
	problem(run(record("/other", "another-recipient", false), 1, "mount", "add", "--from", "-"), "credential_scope_conflict")
	problem(run(`{"at":"/script","driver":"fixture","options":{"origin":"a","origin":"b"}}`, 1, "mount", "edit", "/script", "--from", "-"), "invalid_config")
	effects(2)
	problem(run(record("/script", "origin-one", true), 1, "mount", "edit", "script/", "--from", "-"), "upstream_failure")
	effects(3)
	if after := exported(); !bytes.Equal(before, after) || bytes.Contains(after, []byte("sealed-value")) {
		t.Fatal("failed edit changed the saved record or exposed its secret")
	}
	if got := invoke("", false, 0, "get", "script\\report", "-"); got != "original" {
		t.Fatalf("stdout download: %q", got)
	}
	effects(4)
	if got := run("partial", 1, "put", "-", "/script/report", "--replace", "--size", "8"); !bytes.Contains(got.Result, []byte(`"effect":"unchanged"`)) {
		t.Fatalf("failed upload receipt: %s", got.Result)
	}
	raw, err := os.ReadFile(filepath.Join(root, "report"))
	if err != nil || string(raw) != "original" {
		t.Fatalf("failed upload replaced the old body: %q %v", raw, err)
	}
	for _, command := range [][]string{{"put", "-", "/script/report", "--replace", "--json"}, {"ls", "/script", "--json"}} {
		t.Run(command[0]+" blocked stream", func(t *testing.T) {
			reader, writer := io.Pipe()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			defer context.AfterFunc(ctx, func() { _ = writer.Close() })()
			defer writer.Close()
			var result bytes.Buffer
			var in io.Reader = reader
			var out io.Writer = &result
			if command[0] == "ls" {
				in, out = nil, writer
			}
			args := append([]string{"--config", config, "--timeout", "150ms"}, command...)
			if code := app.Run(ctx, args, in, out, io.Discard, false); code != 1 || ctx.Err() != nil ||
				command[0] == "put" && !strings.Contains(result.String(), `"code":"timeout"`) {
				t.Fatalf("blocked stream did not stop at command timeout: %d %s", code, &result)
			}
		})
	}
	effects(7)
	require(t, os.Mkdir(filepath.Join(root, "目录"), 0700))
	require(t, os.WriteFile(filepath.Join(root, "Plan 2026.txt"), nil, 0600))
	if got := invoke("", false, 0, "ls", "script/"); !strings.Contains(got, "目录/\n") ||
		!strings.Contains(got, "Plan 2026.txt\n") || strings.Contains(got, "\t") {
		t.Fatalf("directory listing: %q", got)
	}
	if got := invoke("", false, 0, "stat", "script/Plan 2026.txt"); !strings.Contains(got, "Path      /script/Plan 2026.txt\nType      file\nSize      0 bytes\n") {
		t.Fatalf("file details: %q", got)
	}
	if got := invoke("", false, 0, "stat", "/"); !strings.Contains(got, "Path      /\nType      directory\n") {
		t.Fatalf("virtual root details: %q", got)
	}
	effects(9)

	for range 2 {
		invoke("", false, 0, "stat", "/script/report")
	}
	effects(11)
	copyPath := filepath.Join(base, "download copy.txt")
	session := "ls\ncd script\nls --limit 1\nstat 'Plan 2026.txt'\n" +
		"get \"Plan 2026.txt\" \"" + copyPath + "\"\nstat 'unterminated\npwd\n" +
		"remove report/\nstat report\ncd ..\npwd\nexit\nstat /guided/report\n"
	got := invoke(session, true, 0, "shell")
	for _, part := range []string{"script/\n", "/script\n", "/\n", "Path      /script/report\n", "Path      /script/Plan 2026.txt\n"} {
		if !strings.Contains(got, part) {
			t.Fatalf("shell missing %q: %q", part, got)
		}
	}
	if strings.Contains(got, "Path      /guided/report\n") {
		t.Fatalf("shell ran a command after exit: %q", got)
	}
	if _, err := os.Stat(copyPath); err != nil {
		t.Fatalf("quoted local download path was changed: %v", err)
	}
	effects(12)

	if got := invoke("pwd\nexit\n", true, 0, "shell", "script"); got != "/script\n" {
		t.Fatalf("shell starting path: %q", got)
	}
	effects(13)

	problem(run("", 1, "trash", "/script/report"), "unsupported_operation")
	if raw, err := os.ReadFile(filepath.Join(root, "report")); err != nil || string(raw) != "original" {
		t.Fatalf("unsupported trash changed the original: %q %v", raw, err)
	}
	run("", 0, "remove", "/script/Plan 2026.txt")
	if _, err := os.Stat(filepath.Join(root, "Plan 2026.txt")); !os.IsNotExist(err) {
		t.Fatalf("permanent remove did not delete the disposable file: %v", err)
	}
	run("", 0, "rmdir", "/script/目录")
	if _, err := os.Stat(filepath.Join(root, "目录")); !os.IsNotExist(err) {
		t.Fatalf("rmdir did not delete the empty directory: %v", err)
	}
	effects(16)

	folder := filepath.Join(root, "目录")
	require(t, os.Mkdir(folder, 0700))
	require(t, os.WriteFile(filepath.Join(folder, "report"), []byte("occupant"), 0600))
	for i, command := range [][]string{
		{"cp", "script/report", "script/目录/"},
		{"put", filepath.Join(root, "report"), "script/目录/"},
	} {
		result := run("", 0, command...)
		name := fmt.Sprintf("/script/目录/report (%d)", i+2)
		var placement struct{ Destination string }
		if err := json.Unmarshal(result.Result, &placement); err != nil || placement.Destination != name {
			t.Fatalf("wrong destination from %s: %s, %v", command[0], result.Result, err)
		}
	}
	problem(run("", 2, "put", "-", "script/目录/"), "usage")
	problem(run("", 1, "mv", "script/report", "script/missing/"), "not_found")
	got = invoke("mv report 目录/\nexit\n", true, 0, "shell", "script")
	if !strings.Contains(got, "/script/目录/report (4)") {
		t.Fatalf("shell did not show the placed file: %q", got)
	}
	for i, want := range []string{"occupant", "original", "original", "original"} {
		name := "report"
		if i > 0 {
			name = fmt.Sprintf("report (%d)", i+1)
		}
		body, err := os.ReadFile(filepath.Join(folder, name))
		if err != nil || string(body) != want {
			t.Fatalf("destination %q has %q, want %q: %v", name, body, want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "report")); !os.IsNotExist(err) {
		t.Fatalf("moved file is still at its source: %v", err)
	}
	effects(20)

	require(t, os.Mkdir(filepath.Join(root, "tree"), 0700))
	require(t, os.Mkdir(filepath.Join(folder, "tree"), 0700))
	require(t, os.WriteFile(filepath.Join(root, "tree", "entry"), []byte("child"), 0600))
	result := run("", 0, "cp", "-r", "script/tree", "script/目录/")
	if !bytes.Contains(result.Result, []byte(`"destination":"/script/目录/tree (2)"`)) {
		t.Fatalf("recursive copy did not report its exact root: %s", result.Result)
	}
	body, err := os.ReadFile(filepath.Join(folder, "tree (2)", "entry"))
	if err != nil || string(body) != "child" {
		t.Fatalf("recursive copy did not place its subtree: %q %v", body, err)
	}
}

func TestConfigContenders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	type child struct {
		cmd *exec.Cmd
		in  io.WriteCloser
		out *bufio.Reader
	}
	start := func() child {
		t.Helper()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHostWriterProcess$")
		cmd.Env = append(os.Environ(), "MWEAVE_TEST_HOST_CONFIG="+path)
		cmd.Stderr = os.Stderr
		in, err := cmd.StdinPipe()
		require(t, err)
		out, err := cmd.StdoutPipe()
		require(t, err)
		require(t, cmd.Start())
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})
		c := child{cmd, in, bufio.NewReader(out)}
		if ready, err := c.out.ReadString('\n'); err != nil || ready != "ready\n" {
			t.Fatalf("writer did not load snapshot: %q %v", ready, err)
		}
		return c
	}
	send := func(c child, at string) {
		t.Helper()
		record := fmt.Sprintf(`{"at":%q,"driver":"local","options":{"root":"."}}`, at)
		_, err := io.WriteString(c.in, record)
		require(t, err)
		require(t, c.in.Close())
		if line, err := c.out.ReadString('\n'); err != nil || line != "read\n" {
			t.Fatalf("writer did not consume record: %q %v", line, err)
		}
	}
	finish := func(c child, code int, contains string) {
		t.Helper()
		result, err := io.ReadAll(c.out)
		require(t, err)
		err = c.cmd.Wait()
		if c.cmd.ProcessState == nil || c.cmd.ProcessState.ExitCode() != code || !bytes.Contains(result, []byte(contains)) {
			t.Fatalf("writer exit: %s, %v", result, err)
		}
	}
	winner, stale := start(), start()
	send(winner, "/winner")
	finish(winner, 0, `"effect":"applied"`)
	send(stale, "/stale")
	finish(stale, 1, `"code":"config_conflict"`)

	lock, err := os.OpenFile(filepath.Join(dir, ".settings.json.mweave.lock"), os.O_RDWR, 0600)
	require(t, err)
	defer lock.Close()
	if ok, err := filelock.Try(lock); !ok || err != nil {
		t.Fatalf("hold config commit lock: %v", err)
	}
	interrupted := start()
	send(interrupted, "/interrupted")
	require(t, interrupted.cmd.Process.Kill())
	_ = interrupted.cmd.Wait()
	require(t, lock.Close())
	next := start()
	send(next, "/recovered")
	finish(next, 0, `"effect":"applied"`)
	saved, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(saved, []byte("/winner")) || !bytes.Contains(saved, []byte("/recovered")) ||
		bytes.Contains(saved, []byte("/stale")) || bytes.Contains(saved, []byte("/interrupted")) {
		t.Fatalf("competing writes left %q (%v)", saved, err)
	}
}

type writerInput struct {
	begun bool
}

func (r *writerInput) Read(p []byte) (int, error) {
	if !r.begun {
		fmt.Fprintln(os.Stdout, "ready")
		r.begun = true
	}
	n, err := os.Stdin.Read(p)
	if err == io.EOF {
		fmt.Fprintln(os.Stdout, "read")
	}
	return n, err
}

func TestHostWriterProcess(t *testing.T) {
	path := os.Getenv("MWEAVE_TEST_HOST_CONFIG")
	if path == "" {
		return
	}
	app, err := host.New(localsource.Source())
	require(t, err)
	os.Exit(app.Run(t.Context(), []string{"--config", path, "mount", "add", "--from", "-", "--json"}, &writerInput{}, os.Stdout, os.Stderr, false))
}
