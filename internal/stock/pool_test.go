//go:build mweave_pool

package stock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
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
	var opens, live atomic.Int32
	source, err := cliapp.NewSource("fixture", "Closed fixture", nil, func(o options, dir string) (cliapp.PreparedSource, error) {
		return cliapp.PreparedSource{CredentialOrigin: o.Root, OptionalCredential: true, Open: func(context.Context, string) (core.Driver, error) {
			root := o.Root
			if root == "fail" {
				root = "parity"
			}
			d, err := local.New(filepath.Join(dir, root))
			if err != nil {
				return nil, err
			}
			opens.Add(1)
			live.Add(1)
			tracked := &trackedLocal{d, &live}
			if o.Root == "fail" {
				return tracked, core.Fail(core.UpstreamFailure, nil)
			}
			return tracked, nil
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(source)
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(input string, code int, args ...string) resultForTest {
		t.Helper()
		got := invokeApp(t, app, cfg, input, false, code, append(args, "--json")...)
		if live.Load() != 0 {
			t.Fatalf("%v: leaked %d Pool members", args, live.Load())
		}
		return got
	}
	save := func(opts cliapp.PoolOptions, code int, command ...string) resultForTest {
		t.Helper()
		raw, err := json.Marshal(opts)
		if err != nil {
			t.Fatal(err)
		}
		return invoke(fmt.Sprintf(`{"at":"/pool","driver":%q,"options":%s}`, cliapp.PoolSourceID, raw), code, append(command, "--from", "-")...)
	}
	invoke(`{"at":"/files","driver":"local","options":{"root":"ordinary"}}`, 0, "mount", "add", "--from", "-")
	opts := cliapp.PoolOptions{MetadataPath: "pool.db", DataShards: 1, ParityShards: 1, ShardBytes: 64, MaxBufferedBytes: 128,
		Members: []cliapp.PoolMember{
			{ID: "a", Identity: "primary", FailureDomain: "a", Driver: "fixture", Options: json.RawMessage(`{"root":"primary"}`)},
			{ID: "b", Identity: "parity", FailureDomain: "b", Driver: "fixture", Options: json.RawMessage(`{"root":"fail"}`)},
		}}
	t.Setenv("MWEAVE_POOL_SHARED", "fixture-password")
	shared := &cliapp.CredentialRef{Kind: "env", Ref: "MWEAVE_POOL_SHARED"}
	opts.Members[0].Credential, opts.Members[1].Credential = shared, shared
	got := save(opts, 1, "mount", "add")
	if got.Error == nil || opens.Load() != 0 {
		t.Fatalf("shared credential reached independent recipients: %+v", got.Error)
	}
	opts.Members[0].Credential, opts.Members[1].Credential = nil, nil
	got = save(opts, 1, "mount", "add")
	if got.Error == nil || got.Error.Code != string(core.UpstreamFailure) || !strings.Contains(got.Error.Message, `"b"`) {
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

	t.Setenv("MWEAVE_TEST_POOL_EXPORT", "fixture")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	invoke(fmt.Sprintf(`{"protocol":"http","listen":%q,"root":"/pool","auth":{"credential":{"kind":"env","ref":"MWEAVE_TEST_POOL_EXPORT"}}}`, addr),
		0, "export", "add", "--from", "-")
	runtime, err := cliapp.PrepareExports(app, t.Context(), authSnapshot(t, cfg), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Shutdown(false, time.Second) })
	if live.Load() != 1 {
		t.Fatalf("started export with %d live Pool members", live.Load())
	}
	var locked, diagLocked bytes.Buffer
	if code := app.Run(t.Context(), []string{"--config", cfg, "stat", "/pool/item", "--json"}, nil, &locked, &diagLocked, false); code != 1 ||
		!strings.Contains(locked.String(), `"code":"pool_in_use"`) || live.Load() != 1 {
		t.Fatalf("competing open: %d, %s, live=%d", code, &locked, live.Load())
	}
	if err := runtime.Shutdown(false, time.Second); err != nil {
		t.Fatal(err)
	}
	invoke("", 0, "stat", "/pool/item")
	metadata := filepath.Join(dir, opts.MetadataPath)
	if err := os.Rename(metadata, metadata+".backup"); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, metadata, nil)
	authProblem(t, invoke("", 1, "stat", "/pool/item"), "pool_metadata_missing")
	if info, err := os.Stat(metadata); err != nil || info.Size() != 0 {
		t.Fatalf("activation rewrote blank metadata: %v, %v", info, err)
	}
}

type trackedLocal struct {
	*local.Driver
	live *atomic.Int32
}

func (d *trackedLocal) Close() error {
	d.live.Add(-1)
	return d.Driver.Close()
}
