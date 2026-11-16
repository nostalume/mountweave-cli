//go:build mweave_pool

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/local"
)

func TestPoolMountWorkflow(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"ordinary", "primary", "parity"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(dir, "config.json")
	type options struct {
		Root string `json:"root"`
	}
	var live atomic.Int32
	source, err := NewSource("fixture", "Closed fixture", nil, func(o options, dir string) (PreparedSource, error) {
		return PreparedSource{Open: func(context.Context, string) (core.Driver, error) {
			root := o.Root
			if root == "fail" {
				root = "parity"
			}
			d, err := local.New(filepath.Join(dir, root))
			if err != nil {
				return nil, err
			}
			live.Add(1)
			var tracked core.Driver = &trackedLocal{d, &live}
			if o.Root == "fail" {
				return tracked, core.Fail(core.UpstreamFailure, nil)
			}
			return tracked, nil
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(source, source); err == nil {
		t.Fatal("duplicate source registration accepted")
	}
	app, err := New(source)
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(input string, code int, args ...string) resultForTest {
		t.Helper()
		got := invokeApp(t, app, cfg, input, false, code, append(args, "--json")...)
		if live.Load() != 0 {
			t.Fatalf("%v: leaked %d Drivers", args, live.Load())
		}
		return got
	}
	save := func(opts poolOptions, code int, command ...string) resultForTest {
		t.Helper()
		raw, err := json.Marshal(opts)
		if err != nil {
			t.Fatal(err)
		}
		record, err := json.Marshal(mountRecord{At: "/pool", Driver: "pool", Options: raw})
		if err != nil {
			t.Fatal(err)
		}
		return invoke(string(record), code, append(command, "--from", "-")...)
	}
	invoke(`{"at":"/files","driver":"local","options":{"root":"ordinary"}}`, 0, "mount", "add", "--from", "-")
	opts := poolOptions{MetadataPath: "pool.db", DataShards: 1, ParityShards: 1, ShardBytes: 64, MaxBufferedBytes: 128,
		Members: []poolMember{
			{ID: "a", Identity: "primary", FailureDomain: "a", Driver: "fixture", Options: json.RawMessage(`{"root":"primary"}`)},
			{ID: "b", Identity: "parity", FailureDomain: "b", Driver: "fixture", Options: json.RawMessage(`{"root":"fail"}`)},
		}}
	if got := save(opts, 1, "mount", "add"); got.Error == nil || got.Error.Code != string(core.UpstreamFailure) || !strings.Contains(got.Error.Message, `"b"`) {
		t.Fatalf("failed member: %+v", got.Error)
	}
	opts.Members[1].Driver = "local"
	opts.Members[1].Options = json.RawMessage(`{"root":"ordinary"}`)
	if got := save(opts, 1, "mount", "add"); got.Error == nil || got.Error.Code != "invalid_config" {
		t.Fatalf("overlapping roots: %+v", got.Error)
	}
	opts.Members[1].Options = json.RawMessage(`{"root":"parity"}`)
	save(opts, 0, "mount", "add")
	invoke("striped payload", 0, "put", "-", "/files/item")
	invoke("", 0, "cp", "/files/item", "/pool/item")
	opts.Members[1].Offline = true
	save(opts, 0, "mount", "edit", "/pool")
	var out, diag bytes.Buffer
	if exit := app.Run(t.Context(), []string{"--config", cfg, "get", "/pool/item", "-"}, nil, &out, &diag, false); exit != 0 || out.String() != "striped payload" || live.Load() != 0 {
		t.Fatalf("degraded read: exit=%d, body=%q, diagnostics=%s, live=%d", exit, out.String(), &diag, live.Load())
	}
	if got := invoke("next", 1, "put", "-", "/pool/next"); got.Error == nil || got.Error.Code != string(core.CapacityExceeded) ||
		!bytes.Contains(got.Result, []byte(`"effect":"unchanged"`)) {
		t.Fatalf("degraded write: %s, %+v", got.Result, got.Error)
	}
	invoke("next", 0, "put", "-", "/files/next")

	t.Setenv("MWEAVE_TEST_POOL_EXPORT", "fixture")
	addr := exportAddresses(t, 1)[0]
	invoke(fmt.Sprintf(`{"protocol":"http","listen":%q,"root":"/pool","auth":{"credential":{"kind":"env","ref":"MWEAVE_TEST_POOL_EXPORT"}}}`, addr),
		0, "export", "add", "--from", "-")
	saved, err := loadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := app.prepareExports(t.Context(), saved, nil)
	if err != nil {
		_ = runtime.shutdown(false, time.Second)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.shutdown(false, time.Second) })
	if live.Load() != 1 {
		t.Fatalf("started export with %d live members", live.Load())
	}
	var locked, diagLocked bytes.Buffer
	if code := app.Run(t.Context(), []string{"--config", cfg, "stat", "/pool/item", "--json"}, nil, &locked, &diagLocked, false); code != 1 ||
		!strings.Contains(locked.String(), `"code":"pool_in_use"`) || live.Load() != 1 {
		t.Fatalf("competing open: %d, %s, live=%d", code, &locked, live.Load())
	}
	if err := runtime.shutdown(false, time.Second); err != nil {
		t.Fatal(err)
	}
	invoke("", 0, "stat", "/pool/item")
}

type trackedLocal struct {
	*local.Driver
	live *atomic.Int32
}

func (d *trackedLocal) Close() error {
	d.live.Add(-1)
	return d.Driver.Close()
}
