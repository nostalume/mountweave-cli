package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/local"
)

func TestFileWorkflow(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"a", "b", "staging"} {
		if err := os.Mkdir(filepath.Join(home, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(home, "config.json")
	contents := `{"version":1,"mounts":[{"at":"/a","driver":"local","options":{"root":"a","staging_directory":"staging","max_staging_bytes":1048576}},{"at":"/b","driver":"local","options":{"root":"b","staging_directory":"staging","max_staging_bytes":1048576}}]}`
	if err := os.WriteFile(cfg, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	runJSON := func(input string, exit int, args ...string) resultForTest {
		t.Helper()
		return invokeApp(t, app, cfg, input, false, exit, append(args, "--json")...)
	}
	effect := func(result resultForTest, want string) {
		t.Helper()
		if !bytes.Contains(result.Result, []byte(`"effect":"`+want+`"`)) {
			t.Fatalf("effect: %s, %+v", result.Result, result.Error)
		}
	}
	file := func(name, want string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(name)))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v", name, data, err)
		}
	}

	effect(runJSON("", 0, "mkdir", "/a/dir"), "applied")
	effect(runJSON("old", 0, "put", "-", "/a/dir/item", "--size", "3"), "applied")
	effect(runJSON("new", 1, "put", "-", "/a/dir/item"), "unchanged")
	effect(runJSON("bad", 1, "put", "-", "/a/dir/item", "--replace", "--size", "4"), "unchanged")
	file("a/dir/item", "old")
	effect(runJSON("new", 0, "put", "-", "/a/dir/item", "--replace"), "applied")

	dest := filepath.Join(home, "download")
	effect(runJSON("", 0, "get", "/a/dir/item", dest), "applied")
	effect(runJSON("", 1, "get", "/a/dir/item", dest), "unchanged")
	file("download", "new")
	var body, diagnostics bytes.Buffer
	if code := testApp.Run(context.Background(), []string{"--config", cfg, "get", "/a/dir/item", "-"}, strings.NewReader(""), &body, &diagnostics, false); code != 0 || body.String() != "new" || diagnostics.Len() != 0 {
		t.Fatalf("stdout body = %q, exit=%d, diagnostics=%s", &body, code, &diagnostics)
	}

	diagnostics.Reset()
	if code := testApp.Run(context.Background(), []string{"--config", cfg, "get", "/a/dir/item", "-"}, strings.NewReader(""), closedOutput{}, &diagnostics, false); code != 1 || !strings.Contains(diagnostics.String(), "output_error") {
		t.Fatalf("closed download: %d %s", code, &diagnostics)
	}

	for _, upload := range []struct {
		exit int
		line string
	}{{0, "Uploaded \"/b/single\".\n"}, {1, "Destination unchanged: \"/b/single\".\n"}} {
		var output, diagnostics bytes.Buffer
		code := app.Run(t.Context(), []string{"--config", cfg, "put", dest, "/b/single"}, nil, &output, &diagnostics, false)
		if code != upload.exit || output.String() != upload.line {
			t.Fatalf("upload returned %d: %s %s", code, &output, &diagnostics)
		}
	}
	effect(runJSON("", 0, "cp", "--replace", "/a/dir/item", "/b/single"), "applied")
	if got := runJSON("", 0, "cp", "-r", "/a/dir", "/b/copy"); !bytes.Contains(got.Result, []byte(`"relative_path":"/item"`)) {
		t.Fatal(string(got.Result))
	}
	if got := runJSON("", 0, "mv", "/b/copy", "/b/moved"); !bytes.Contains(got.Result, []byte(`"source_effect":"applied","target_effect":"applied"`)) {
		t.Fatal(string(got.Result))
	}
	file("b/moved/item", "new")
	effect(runJSON("", 1, "rm", "/b/moved"), "unchanged")
	effect(runJSON("", 1, "rmdir", "/b/moved"), "unchanged")
	effect(runJSON("", 0, "rm", "-r", "/b/moved"), "applied")
	effect(runJSON("", 0, "mkdir", "/b/empty"), "applied")
	effect(runJSON("", 0, "rmdir", "/b/empty"), "applied")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUploadPipeHelper$")
	child.Env = append(os.Environ(), "MWEAVE_TEST_UPLOAD_CONFIG="+cfg)
	writer, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	output, err := child.CombinedOutput()
	if child.ProcessState == nil || child.ProcessState.ExitCode() != 1 || !bytes.Contains(output, []byte(`"code":"timeout"`)) {
		t.Fatalf("cancelled upload from inherited pipe: %s, %v", output, err)
	}
	file("a/dir/item", "new")

	racePath := filepath.Join(home, "competing-download")
	var beforeRead func() error = func() error { return os.WriteFile(racePath, []byte("competitor"), 0600) }
	saved, err := loadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	saved.value.Mounts = append(saved.value.Mounts, mountRecord{At: "/guarded", Driver: "guard-fixture", Options: json.RawMessage(`{}`)})
	if _, err := saved.save(context.Background()); err != nil {
		t.Fatal(err)
	}
	app.routes["guard-fixture"] = sourceRoute("Guarded Local", nil, func(struct{}, string) (mountSource, error) {
		return mountSource{open: func(context.Context, string) (core.Driver, error) {
			d, err := local.New(filepath.Join(home, "a"))
			if err != nil {
				return nil, err
			}
			return &guardedFileSource{Driver: d, beforeRead: beforeRead}, nil
		}}, nil
	})
	if got := runJSON("", 1, "get", "/guarded/dir/item", racePath); got.Error == nil || got.Error.Code != string(core.AlreadyExists) {
		t.Fatalf("download race: %+v", got.Error)
	}
	file("competing-download", "competitor")
	beforeRead = nil
	if stages, err := filepath.Glob(filepath.Join(home, ".mweave-download-*")); err != nil || len(stages) != 0 {
		t.Fatalf("temporary downloads: %v, %v", stages, err)
	}

	got := runJSON("", 1, "mv", "/guarded/dir/item", "/b/partially-moved")
	if got.Error == nil || got.Error.Code != string(core.PreconditionFailed) || !bytes.Contains(got.Result, []byte(`"source_effect":"unchanged","target_effect":"applied"`)) {
		t.Fatalf("partial move: %s, %+v", got.Result, got.Error)
	}
	file("a/dir/item", "new")
	file("b/partially-moved", "new")
	diagnostics.Reset()
	if code := testApp.Run(context.Background(), []string{"--config", cfg, "rm", "/b/partially-moved", "--json"}, strings.NewReader(""), closedOutput{}, &diagnostics, false); code != 1 ||
		!strings.Contains(diagnostics.String(), "Effect: applied") || !strings.Contains(diagnostics.String(), "output_error") {
		t.Fatalf("lost receipt: %d %s", code, &diagnostics)
	}
}

func TestUploadPipeHelper(t *testing.T) {
	if cfg := os.Getenv("MWEAVE_TEST_UPLOAD_CONFIG"); cfg != "" {
		os.Exit(testApp.Run(context.Background(), []string{"--config", cfg, "--timeout", "100ms", "put", "-", "/a/dir/item", "--replace", "--json"}, os.Stdin, os.Stdout, os.Stderr, false))
	}
}

type guardedFileSource struct {
	*local.Driver
	beforeRead func() error
}

func (*guardedFileSource) MoveFileValidator(core.NativeAddress, core.Evidence) (core.Validator, error) {
	return core.Validator{Domain: "fixture", Tag: `"fixture"`}, nil
}

func (*guardedFileSource) Remove(context.Context, core.NativeAddress, ...core.RemoveOptions) (core.Change, error) {
	return core.Change{Effect: core.Unchanged}, core.Fail(core.PreconditionFailed, nil)
}

func (d *guardedFileSource) Open(ctx context.Context, addr core.NativeAddress, opts core.OpenOptions) (core.Opened, error) {
	content, err := d.Driver.Open(ctx, addr, opts)
	if err == nil && content.Body != nil && d.beforeRead != nil {
		content.Body = &firstRead{ReadCloser: content.Body, hook: d.beforeRead}
	}
	return content, err
}

type firstRead struct {
	io.ReadCloser
	hook func() error
}

func (r *firstRead) Read(p []byte) (int, error) {
	if r.hook != nil {
		err := r.hook()
		r.hook = nil
		if err != nil {
			return 0, err
		}
	}
	return r.ReadCloser.Read(p)
}

type closedOutput struct{}

func (closedOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
