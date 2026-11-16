package cli

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
	"slices"
	"strings"
	"testing"
	"time"

	core "github.com/nostalume/mountweave"
)

var testApp, _ = New()

type resultForTest struct {
	Result json.RawMessage
	Error  *problem
}

func invokeCLI(t *testing.T, configPath, input string, terminal bool, code int, args ...string) resultForTest {
	t.Helper()
	return invokeApp(t, testApp, configPath, input, terminal, code, args...)
}

func invokeApp(t *testing.T, app *App, configPath, input string, terminal bool, code int, args ...string) resultForTest {
	t.Helper()
	var out, diag bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got := app.Run(ctx, append([]string{"--config", configPath}, args...), strings.NewReader(input), &out, &diag, terminal); got != code {
		t.Fatalf("%v: exit=%d, output=%s, diagnostics=%s", args, got, &out, &diag)
	}
	var got resultForTest
	if slices.Contains(args, "--json") {
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("%v: %v: %s", args, err, &out)
		}
	}
	return got
}

func TestCommandHelp(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(config, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want []string
		hide []string
	}{
		{[]string{"help"}, []string{"Work with files", "Access over protocols", "Manage settings", "mount", "serve", "export"}, []string{"root_url", "Set up a mount."}},
		{[]string{"serve", "--help"}, []string{"Usage: mweave serve", "--json"}, []string{"--timeout", "--from"}},
		{[]string{"export", "--help"}, []string{"Usage: mweave export COMMAND", "add"}, []string{"root_url", "serve"}},
		{[]string{"mount", "--help"}, []string{"Usage: mweave mount COMMAND", "add", "list", "edit", "remove"}, []string{"root_url", "check", "stat"}},
		{[]string{"mount", "add", "--help"}, []string{"mweave mount add [PATH]", "mweave mount add --from FILE", "webdav", "pan123"}, []string{"--limit", "root_url", "max_response_bytes"}},
		{[]string{"ls", "--help"}, []string{"mweave ls ADDRESS", "--limit N"}, []string{"--from", "root_url"}},
		{[]string{"stat", "-h"}, []string{"mweave stat ADDRESS"}, []string{"--limit", "--from"}},
	} {
		got := invokeCLI(t, config, "", false, 0, append(tc.args, "--json")...)
		var text string
		if err := json.Unmarshal(got.Result, &text); err != nil || got.Error != nil {
			t.Fatalf("%v: %s, %v", tc.args, got.Result, got.Error)
		}
		for _, value := range tc.want {
			if !strings.Contains(text, value) {
				t.Errorf("%v: missing %q", tc.args, value)
			}
		}
		for _, value := range tc.hide {
			if strings.Contains(text, value) {
				t.Errorf("%v: unexpectedly showed %q", tc.args, value)
			}
		}
	}
	for _, args := range [][]string{
		{"mount", "check", "/files"},
		{"serve", "--timeout", "1s"},
		{"mount", "add", "/files", "--from", "-"},
		{"stat", "--limit", "1", "/files"},
		{"get", "/files/blob", "-"},
		{"put", "--size", "1", "file", "/files/blob"},
		{"cp", "-r", "--replace", "/files/a", "/files/b"},
		{"stat"},
	} {
		got := invokeCLI(t, config, "", false, 2, append(args, "--json")...)
		if got.Error == nil || got.Error.Code != "usage" {
			t.Fatalf("%v: %+v", args, got.Error)
		}
	}
}

func TestLocalWorkflow(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "files")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < core.DefaultPage+2; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%03d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(home, "config.json")
	t.Chdir(home)
	invokeCLI(t, configPath, "\nfiles\ny\n", true, 0, "mount", "add", "/guided")
	if got := invokeCLI(t, configPath, "", false, 1, "mount", "add", "--json"); got.Error == nil || got.Error.Code != "interaction_required" {
		t.Fatal(got)
	}
	if got := invokeCLI(t, configPath, localRecord("/script"), false, 0, "mount", "add", "--from", "-", "--json"); !bytes.Contains(got.Result, []byte(`"effect":"applied"`)) {
		t.Fatal(string(got.Result))
	}

	// Saved paths use the real configuration directory across working directories.
	t.Chdir(t.TempDir())
	if got := invokeCLI(t, configPath, "", false, 0, "stat", "/script/file-000", "--json"); !bytes.Contains(got.Result, []byte(`"kind":"file"`)) {
		t.Fatal(string(got.Result))
	}
	got := invokeCLI(t, configPath, "", false, 0, "ls", "/script", "--limit", "101", "--json")
	var page struct {
		Items []core.Entry
		State string
	}
	if err := json.Unmarshal(got.Result, &page); err != nil || len(page.Items) != 101 || page.State != "limited" {
		t.Fatalf("listing: %s (%v)", got.Result, err)
	}
	if got := invokeCLI(t, configPath, "", false, 0, "ls", "/script", "--json"); !bytes.Contains(got.Result, []byte(`"state":"complete"`)) {
		t.Fatal(string(got.Result))
	}

	// Preserve a valid but unavailable mount and never instantiate it to list
	// saved definitions or browse an unrelated mount.
	previous, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	previous.value.Mounts = append(previous.value.Mounts, mountRecord{At: "/offline", Driver: "future", Options: json.RawMessage(`{"native":true}`)})
	if _, err := previous.save(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := invokeCLI(t, configPath, "", false, 0, "mount", "list", "--json"); !bytes.Contains(got.Result, []byte(`"driver":"future","driver_available":false`)) {
		t.Fatal(string(got.Result))
	}
	invokeCLI(t, configPath, "", false, 0, "stat", "/script", "--json")
	for _, malformed := range []string{
		`{"at":"/bad","at":"/other","driver":"local","options":{"root":"files"}}`,
		`{"At":"/bad","driver":"local","options":{"root":"files"}}`,
		`{"at":"/bad","driver":"local","options":{"root":"files","password":"fixture-secret"}}`,
	} {
		got := invokeCLI(t, configPath, malformed, false, 1, "mount", "add", "--from", "-", "--json")
		if got.Error == nil || got.Error.Code != "invalid_config" || strings.Contains(got.Error.Message, "fixture-secret") {
			t.Fatal(got)
		}
	}
}

func localRecord(at string) string {
	return fmt.Sprintf(`{"at":%q,"driver":"local","options":{"root":"files"}}`, at)
}

func TestConfigProcesses(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.json")
	if err := os.Mkdir(filepath.Join(home, "files"), 0700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := testApp.Run(context.Background(), []string{"--config", path, "mount", "add", "--from", "-"}, strings.NewReader(localRecord("/existing")), &out, &out, false); code != 0 {
		t.Fatal(out.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type writer struct {
		cmd    *exec.Cmd
		input  io.WriteCloser
		output *bufio.Reader
	}
	start := func() writer {
		t.Helper()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConfigWriterHelper$")
		cmd.Env = append(os.Environ(), "MWEAVE_TEST_CONFIG="+path)
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
		w := writer{cmd, input, bufio.NewReader(output)}
		if line, err := w.output.ReadString('\n'); err != nil || line != "ready\n" {
			t.Fatalf("writer did not load its snapshot: %q %v", line, err)
		}
		return w
	}
	send := func(w writer, at string) {
		t.Helper()
		if _, err := io.WriteString(w.input, localRecord(at)); err != nil {
			t.Fatal(err)
		}
		if err := w.input.Close(); err != nil {
			t.Fatal(err)
		}
		if line, err := w.output.ReadString('\n'); err != nil || line != "read\n" {
			t.Fatalf("writer did not consume its input: %q %v", line, err)
		}
	}
	finish := func(w writer, code int, text string) {
		t.Helper()
		body, err := io.ReadAll(w.output)
		if err != nil {
			t.Fatal(err)
		}
		_ = w.cmd.Wait()
		if w.cmd.ProcessState.ExitCode() != code || !bytes.Contains(body, []byte(text)) {
			t.Fatalf("exit=%d, output=%s", w.cmd.ProcessState.ExitCode(), body)
		}
	}

	first, second := start(), start()
	send(first, "/winner")
	finish(first, 0, `"effect":"applied"`)
	send(second, "/loser")
	finish(second, 1, `"code":"config_conflict"`)

	lock, err := os.OpenFile(filepath.Join(home, ".config.json.mweave.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if ok, err := tryConfigLock(lock); err != nil || !ok {
		t.Fatalf("hold commit lock: %v", err)
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
	saved, err := loadConfig(path)
	if err != nil || len(saved.value.Mounts) != 3 || saved.value.Mounts[1].At != "/winner" || saved.value.Mounts[2].At != "/recovered" {
		t.Fatalf("unexpected saved configuration: %+v %v", saved.value, err)
	}
}

type processInput struct {
	ready bool
}

func (r *processInput) Read(p []byte) (int, error) {
	if !r.ready {
		fmt.Fprintln(os.Stdout, "ready")
		r.ready = true
	}
	n, err := os.Stdin.Read(p)
	if err == io.EOF {
		fmt.Fprintln(os.Stdout, "read")
	}
	return n, err
}

func TestConfigWriterHelper(t *testing.T) {
	path := os.Getenv("MWEAVE_TEST_CONFIG")
	if path != "" {
		os.Exit(testApp.Run(context.Background(), []string{"--config", path, "mount", "add", "--from", "-", "--json"}, &processInput{}, os.Stdout, os.Stderr, false))
	}
}
