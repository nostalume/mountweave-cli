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

type countedDriver struct {
	*local.Driver
	closed *int
}

func (d countedDriver) Close() error {
	*d.closed++
	return d.Driver.Close()
}

func TestSelectedHost(t *testing.T) {
	base := t.TempDir()
	config := filepath.Join(base, "settings.json")
	root := filepath.Join(base, "data")
	stage := filepath.Join(base, "staging")
	for _, dir := range []string{root, stage} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "report"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	app, err := host.New(source)
	if err != nil {
		t.Fatal(err)
	}
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
	effects := func(wantOpens, wantCloses int) {
		t.Helper()
		if opens != wantOpens || closes != wantCloses {
			t.Fatalf("opened %d and closed %d, want %d and %d", opens, closes, wantOpens, wantCloses)
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
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	record := func(at, origin string, broken bool) string {
		return fmt.Sprintf(`{"at":%q,"driver":"fixture","options":{"origin":%q,"broken":%t},"credential":{"kind":"env","ref":"TEST_MWEAVE_HOST_CREDENTIAL"}}`, at, origin, broken)
	}

	invoke("", true, 0, "mount", "add", "--help")
	run("", 0, "mount", "list")
	effects(0, 0)
	invoke("origin-one\nenv:TEST_MWEAVE_HOST_CREDENTIAL\ny\n", true, 0, "mount", "add", "/guided")
	effects(1, 1)
	run(record("/script", "origin-one", false), 0, "mount", "add", "--from", "-")
	effects(2, 2)
	before := exported()
	problem(run(record("/other", "another-recipient", false), 1, "mount", "add", "--from", "-"), "credential_scope_conflict")
	problem(run(`{"at":"/script","driver":"fixture","options":{"origin":"a","origin":"b"}}`, 1, "mount", "edit", "/script", "--from", "-"), "invalid_config")
	effects(2, 2)
	problem(run(record("/script", "origin-one", true), 1, "mount", "edit", "/script", "--from", "-"), "upstream_failure")
	effects(3, 3)
	if after := exported(); !bytes.Equal(before, after) || bytes.Contains(after, []byte("sealed-value")) {
		t.Fatal("failed edit changed the saved record or exposed its secret")
	}
	if got := invoke("", false, 0, "get", "/script/report", "-"); got != "original" {
		t.Fatalf("stdout download: %q", got)
	}
	effects(4, 4)
	if got := run("partial", 1, "put", "-", "/script/report", "--replace", "--size", "8"); !bytes.Contains(got.Result, []byte(`"effect":"unchanged"`)) {
		t.Fatalf("failed upload receipt: %s", got.Result)
	}
	raw, err := os.ReadFile(filepath.Join(root, "report"))
	if err != nil || string(raw) != "original" {
		t.Fatalf("failed upload replaced the old body: %q %v", raw, err)
	}
	in, writer := io.Pipe()
	defer writer.Close()
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	args := []string{"--config", config, "--timeout", "150ms", "put", "-", "/script/report", "--replace", "--json"}
	if code := app.Run(ctx, args, in, &out, io.Discard, false); code != 1 || !strings.Contains(out.String(), `"code":"timeout"`) {
		t.Fatalf("cancelled upload did not stop its blocked input: %d %s", code, &out)
	}
	effects(6, 6)
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
		if err != nil {
			t.Fatal(err)
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
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
		if _, err := io.WriteString(c.in, record); err != nil {
			t.Fatal(err)
		}
		if err := c.in.Close(); err != nil {
			t.Fatal(err)
		}
		if line, err := c.out.ReadString('\n'); err != nil || line != "read\n" {
			t.Fatalf("writer did not consume record: %q %v", line, err)
		}
	}
	finish := func(c child, code int, contains string) {
		t.Helper()
		result, err := io.ReadAll(c.out)
		if err != nil {
			t.Fatal(err)
		}
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
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if ok, err := filelock.Try(lock); !ok || err != nil {
		t.Fatalf("hold config commit lock: %v", err)
	}
	interrupted := start()
	send(interrupted, "/interrupted")
	if err := interrupted.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = interrupted.cmd.Wait()
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(app.Run(t.Context(), []string{"--config", path, "mount", "add", "--from", "-", "--json"}, &writerInput{}, os.Stdout, os.Stderr, false))
}
