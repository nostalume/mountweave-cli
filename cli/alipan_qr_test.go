package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nostalume/mountweave/drivers/alipan"
)

func TestAliPanScanWorkflow(t *testing.T) {
	if !managedStoreSupported {
		t.Skip("managed credential store is unavailable")
	}
	home := t.TempDir()
	t.Setenv("APPDATA", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("MWEAVE_ALIPAN_APP_ID", "approved-app")
	config := filepath.Join(home, "config.json")
	var scans, drives, refreshes atomic.Int32
	var challenge string
	noRefresh := false
	transport, launcher := http.DefaultTransport, launchOAuthBrowser
	t.Cleanup(func() { http.DefaultTransport, launchOAuthBrowser = transport, launcher })
	var presented []string
	launchOAuthBrowser = func(_ context.Context, name string) error {
		f, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		image, err := png.DecodeConfig(f)
		if err != nil || image.Width != 384 || image.Height != 384 {
			t.Fatalf("unreadable scan image: %+v, %v", image, err)
		}
		presented = append(presented, name)
		if len(presented) == 2 {
			return errors.New("image viewer unavailable")
		}
		return nil
	}
	http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "openapi.alipan.com" {
			return nil, fmt.Errorf("unexpected AliPan origin")
		}
		w := httptest.NewRecorder()
		var reply string
		switch r.Method + " " + r.URL.Path {
		case "POST /oauth/authorize/qrcode":
			scans.Add(1)
			var fields struct {
				App       string   `json:"client_id"`
				Scopes    []string `json:"scopes"`
				Challenge string   `json:"code_challenge"`
				Method    string   `json:"code_challenge_method"`
			}
			if json.NewDecoder(r.Body).Decode(&fields) != nil || fields.App != "approved-app" ||
				fields.Method != "S256" || !strings.Contains(strings.Join(fields.Scopes, " "), aliPanWriteScope) {
				t.Error("invalid QR request")
			}
			challenge = fields.Challenge
			reply = `{"qrCodeUrl":"https://www.alipan.com/authorize?fixture=1","sid":"fixture-session"}`
		case "GET /oauth/qrcode/fixture-session/status":
			reply = `{"status":"LoginSuccess","authCode":"fixture-code"}`
		case "POST /oauth/access_token":
			var fields map[string]string
			if json.NewDecoder(r.Body).Decode(&fields) != nil || fields["client_id"] != "approved-app" {
				t.Error("invalid token request")
			}
			if _, present := fields["client_secret"]; present {
				t.Error("QR authorization sent a client secret")
			}
			if fields["grant_type"] == "refresh_token" {
				refreshes.Add(1)
				if fields["refresh_token"] != "scan-refresh" {
					t.Error("wrong refresh token")
				}
				reply = `{"token_type":"Bearer","access_token":"scan-renewed","refresh_token":"scan-rotated","expires_in":3600}`
				break
			}
			hash := sha256.Sum256([]byte(fields["code_verifier"]))
			if fields["grant_type"] != "authorization_code" || fields["code"] != "fixture-code" || challenge != base64.RawURLEncoding.EncodeToString(hash[:]) {
				t.Error("authorization code did not use the matching verifier")
			}
			reply = `{"token_type":"Bearer","access_token":"scan-access","refresh_token":"scan-refresh","expires_in":3600}`
			if noRefresh {
				reply = `{"token_type":"Bearer","access_token":"scan-access","expires_in":3600}`
			}
		case "GET /oauth/users/scopes":
			if r.Header.Get("Authorization") != "Bearer scan-access" {
				t.Error("scopes did not receive the new access token")
			}
			reply = `{"id":"fixture-account","scopes":[{"scope":"user:base"},{"scope":"file:all:read"},{"scope":"file:all:write"}]}`
		case "POST /adrive/v1.0/user/getDriveInfo":
			drives.Add(1)
			reply = `{"user_id":"fixture-account","name":"Fixture","default_drive_id":"other-drive","resource_drive_id":"resource-drive","backup_drive_id":"backup-drive"}`
		case "POST /adrive/v1.0/openFile/list":
			var fields map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&fields) != nil || string(fields["drive_id"]) != `"resource-drive"` {
				t.Error("mount did not use the selected resource drive")
			}
			reply = `{"items":[]}`
		default:
			return nil, fmt.Errorf("unexpected AliPan request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, reply)
		return w.Result(), nil
	})

	invokeCLI(t, config, "alipan\nopen\nread_wirte\nread_write\n\nresourcee\n\n\ny\n", true, 0, "mount", "add", "/ali")
	saved, err := loadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	mount := saved.value.Mounts[0]
	var opts alipan.Options
	if err := json.Unmarshal(mount.Options, &opts); err != nil || mount.Credential.Kind != managedCredential || opts.DriveID != "resource-drive" ||
		opts.MaxBufferedBytes != aliPanPartSize || strings.Contains(string(mount.Options), "scan-refresh") {
		t.Fatalf("unexpected persisted mount: %+v", mount)
	}
	lease, err := acquireGrant(mount.Credential.Ref, false)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := readAliPanAuthorization(lease.path, mount.Credential.Ref)
	if err != nil {
		t.Fatal(err)
	}
	grant.Access = ""
	if err := errors.Join(grant.save(context.Background(), lease, true), lease.lock.Close()); err != nil {
		t.Fatal(err)
	}
	invokeCLI(t, config, "alipan\nopen\n\n\n\n\ny\n", true, 0, "mount", "add", "/ali-read")
	saved, err = loadConfig(config)
	if err != nil || len(saved.value.Mounts) != 2 || saved.value.Mounts[1].Credential.Ref != grant.ID ||
		strings.Contains(string(saved.value.Mounts[1].Options), "part_size") || scans.Load() != 1 || drives.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("reuse did not reread the selected account exactly once: scans=%d drives=%d refresh=%d err=%v", scans.Load(), drives.Load(), refreshes.Load(), err)
	}
	noRefresh = true
	var out, diagnostics bytes.Buffer
	input := strings.NewReader("alipan\nopen\nread_write\nauthorize\n")
	args := []string{"--config", config, "mount", "add", "/access-only"}
	if code := testApp.Run(t.Context(), args, input, &out, &diagnostics, true); code != 1 ||
		!strings.Contains(diagnostics.String(), "Scan the QR code") || !strings.ContainsAny(diagnostics.String(), "█▀▄") {
		t.Fatalf("scan without an image viewer returned %d: %s %s", code, &out, &diagnostics)
	}
	if len(presented) != 2 {
		t.Fatalf("new authorizations opened %d QR images", len(presented))
	}
	for _, name := range presented {
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temporary scan image still exists: %v", err)
		}
	}
	saved, err = loadConfig(config)
	if err != nil || len(saved.value.Mounts) != 2 {
		t.Fatal("short-lived authorization changed the configuration", err)
	}
}
