package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/alipan/credential"
)

func saveAliPanGrant(ctx context.Context, lease *grantLease, record aliPanGrant, replace bool) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return lease.save(ctx, data, replace)
}

func TestManagedAliPanWorkflow(t *testing.T) {
	if config := os.Getenv("MWEAVE_TEST_ALIPAN_REOPEN"); config != "" {
		http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("unexpected request from fresh process: %s", r.URL)
		})
		os.Exit(testApp.Run(context.Background(), []string{"--config", config, "ls", "/ali", "--json"}, nil, os.Stdout, os.Stderr, false))
	}
	if !managedStoreSupported {
		t.Skip("managed credential store is unavailable")
	}
	home := t.TempDir()
	t.Setenv("APPDATA", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	config := filepath.Join(home, "config.json")
	ctx := context.Background()
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const initial = "synthetic-old-refresh"
	const rotated = "synthetic-new-refresh"

	lease, err := acquireGrant(id, true)
	if err != nil {
		t.Fatal(err)
	}
	record := aliPanGrant{
		ID: id, Account: "fixture-user", Scopes: []string{"file:all:read"}, ClientID: "fixture-app",
		ClientSecret: "fixture-app-secret", Checkpoint: credential.Record{Token: credential.Token{Refresh: initial}},
	}
	if err := saveAliPanGrant(ctx, lease, record, false); err != nil {
		t.Fatal(err)
	}
	if err := lease.lock.Close(); err != nil {
		t.Fatal(err)
	}

	var refreshes, lists atomic.Int32
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		if r.URL.String() == "https://content.invalid/report" && r.Method == http.MethodGet {
			if r.Header.Get("Authorization") != "" {
				t.Error("AliPan credentials reached the content origin")
			}
			_, _ = io.WriteString(w, "x")
			return w.Result(), nil
		}
		if r.URL.Scheme != "https" || r.URL.Host != "openapi.alipan.com" || r.Method != http.MethodPost {
			return nil, fmt.Errorf("unexpected AliPan request: %s %s", r.Method, r.URL)
		}
		switch r.URL.Path {
		case "/oauth/access_token":
			n := refreshes.Add(1)
			checkpoint, err := readAliPanAuthorization(lease.path, id)
			if err != nil || !checkpoint.Pending {
				t.Errorf("provider called without a pending checkpoint: %v", err)
			}
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				return nil, err
			}
			if payload["client_id"] != record.ClientID || payload["client_secret"] != record.ClientSecret ||
				payload["grant_type"] != "refresh_token" || payload["refresh_token"] != checkpoint.Refresh {
				t.Error("token request is inconsistent with the checkpoint")
			}
			if n > 1 {
				return nil, fmt.Errorf("fixture lost the refresh response")
			}
			_, _ = io.WriteString(w, `{"token_type":"Bearer","access_token":"fixture-access","refresh_token":"synthetic-new-refresh","expires_in":3600}`)
		case "/adrive/v1.0/openFile/list":
			lists.Add(1)
			checkpoint, err := readAliPanAuthorization(lease.path, id)
			if err != nil || checkpoint.Pending || checkpoint.Refresh != rotated ||
				r.Header.Get("Authorization") != "Bearer fixture-access" {
				t.Error("list used a token without a confirmed checkpoint")
			}
			_, _ = io.WriteString(w, `{"items":[{"drive_id":"drive","parent_file_id":"root","file_id":"report","name":"report.txt","type":"file","size":1}]}`)
		case "/adrive/v1.0/openFile/getDownloadUrl":
			if r.Header.Get("Authorization") != "Bearer fixture-access" {
				t.Error("download authorization used the wrong token")
			}
			_, _ = io.WriteString(w, `{"url":"https://content.invalid/report","method":"GET"}`)
		default:
			return nil, fmt.Errorf("unexpected AliPan path: %s", r.URL.Path)
		}
		return w.Result(), nil
	})

	const settings = `{"at":"/ali","driver":"alipan","options":{"drive_id":"drive","root_id":"root"},"credential":{"kind":"managed","ref":"` + id + `"}}`
	invokeCLI(t, config, settings, false, 0, "mount", "add", "--from", "-", "--json")
	if got := invokeCLI(t, config, "", false, 0, "ls", "/ali", "--json"); !bytes.Contains(got.Result, []byte("report.txt")) {
		t.Fatalf("listing after reopen: %s", got.Result)
	}
	target := filepath.Join(home, "report.txt")
	invokeCLI(t, config, "", false, 0, "get", "/ali/report.txt", target, "--json")
	if data, err := os.ReadFile(target); err != nil || string(data) != "x" {
		t.Fatalf("download: %q, %v", data, err)
	}
	if refreshes.Load() != 1 || lists.Load() != 3 {
		t.Fatalf("refreshes=%d lists=%d", refreshes.Load(), lists.Load())
	}
	configData, err := os.ReadFile(config)
	if err != nil || bytes.Contains(configData, []byte(rotated)) || bytes.Contains(configData, []byte(record.ClientSecret)) {
		t.Fatal("mount configuration contains authorization material or is missing")
	}
	data, err := os.ReadFile(lease.path)
	if err != nil || ((runtime.GOOS == "windows") && bytes.Contains(data, []byte(rotated))) {
		t.Fatal("credential store is unavailable or contains unprotected authorization material")
	}

	saved, err := loadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	active, err := testApp.openFor(ctx, saved, core.Path("/ali"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeRuntime(active)
	freshProcess := func(code string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestManagedAliPanWorkflow$")
		cmd.Env = append(os.Environ(), "MWEAVE_TEST_ALIPAN_REOPEN="+config)
		out, err := cmd.Output()
		var result resultForTest
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || json.Unmarshal(out, &result) != nil ||
			result.Error == nil || result.Error.Code != code || bytes.Contains(out, []byte(rotated)) {
			t.Fatalf("fresh process: want %s, got %s (%v)", code, out, err)
		}
	}
	freshProcess("credential_in_use")
	// Static edits can complete while the protected record is owned by another runtime.
	const edit = `{"at":"/ali","driver":"alipan","options":{"drive_id":"drive","root_path":"/"}}`
	invokeCLI(t, config, edit, false, 0, "mount", "edit", "/ali", "--from", "-", "--json")
	if err := closeRuntime(active); err != nil {
		t.Fatal(err)
	}
	lease, err = acquireGrant(id, false)
	if err != nil {
		t.Fatal(err)
	}
	record.Checkpoint.Token.Refresh = rotated
	if err := errors.Join(saveAliPanGrant(ctx, lease, record, true), lease.lock.Close()); err != nil {
		t.Fatal(err)
	}
	got := invokeCLI(t, config, "", false, 1, "ls", "/ali", "--json")
	if got.Error == nil || got.Error.Code != "interaction_required" {
		t.Fatalf("uncertain refresh: %+v", got.Error)
	}
	freshProcess("interaction_required")
	if refreshes.Load() != 2 || lists.Load() != 3 {
		t.Fatalf("uncertain checkpoint replayed or reached listing: refreshes=%d lists=%d", refreshes.Load(), lists.Load())
	}
}
