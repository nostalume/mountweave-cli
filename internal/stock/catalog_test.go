package stock

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nostalume/mountweave-cli/internal/cliapp"
)

var testApp, _ = New()

type resultForTest struct {
	Result json.RawMessage
	Error  *cliapp.Problem
}

func invokeCLI(t *testing.T, configPath, input string, terminal bool, code int, args ...string) resultForTest {
	t.Helper()
	return invokeApp(t, testApp, configPath, input, terminal, code, args...)
}

func invokeApp(t *testing.T, app *cliapp.App, configPath, input string, terminal bool, code int, args ...string) resultForTest {
	t.Helper()
	var out, diag bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
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

func writeFixture(t *testing.T, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(name, content, 0600); err != nil {
		t.Fatal(err)
	}
}

func isolatedSettingsHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("LOCALAPPDATA", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	return home
}

type sealedTransport func(*http.Request) (*http.Response, error)

func (f sealedTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDefaultConnections(t *testing.T) {
	cases := []struct {
		id, connection, options, origin, location string
	}{
		{"pan123", "", `{"root_id":"42"}`, "Authorization https://open-api.123pan.com", "Folder ID 42"},
		{"pan123", "website", `{"root":"/"}`, "https://yun.123pan.cn/", ""},
		{"pan123", "webdav", `{"root_url":"https://example.test/webdav/","username":"reader"}`, "webdav https example.test 443", "https://example.test/webdav/"},
		{"pikpak", "", `{"device_id":"0123456789abcdef0123456789abcdef"}`, "Authorization https://api-drive.mypikpak.com", ""},
		{"alipan-web", "", `{"account":"fixture","drive_id":"resource","root_id":"root"}`, "https://www.alipan.com/drive", ""},
		{"githubrepo", "", `{"repository":"example/tree","ref":"main"}`, "Authorization https://api.github.com", ""},
		{"atomgit", "", `{"repository":"example/tree","ref":"main"}`, "Authorization https://api.atomgit.com", ""},
	}
	for _, tc := range cases {
		src, err := cliapp.PrepareMountSource(testApp, cliapp.MountRecord{At: "/files", Driver: tc.id, Connection: tc.connection, Options: json.RawMessage(tc.options)}, t.TempDir())
		if err != nil || src.Audience.Origin != tc.origin || src.Location.Location != tc.location {
			t.Errorf("%s/%s: origin %q, location %q, error %v", tc.id, tc.connection, src.Audience.Origin, src.Location.Location, err)
		}
	}
	if r := s3Presets["r2"]; r.Region != "auto" || r.Endpoint != "" {
		t.Fatalf("R2 defaults: %+v", r)
	}
	if r := s3Presets["cos"]; !r.ListV1 || r.Region != "ap-guangzhou" {
		t.Fatalf("COS defaults: %+v", r)
	}
	named, err := prepareS3(s3Options{Bucket: "files", Region: "local", Endpoint: "http://127.0.0.1:9000"}, "")
	if err != nil || named.Mode != cliapp.SDKNamedProfile || named.Audience.Origin != "http://127.0.0.1:9000" {
		t.Fatalf("custom S3 profile: %+v (%v)", named.Audience, err)
	}
	sftp, err := prepareSFTP(sftpOptions{Address: "127.0.0.1", Username: "fixture", KnownHosts: "missing_hosts"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = sftp.Open(t.Context(), "sealed-password")
	if problem, ok := err.(*cliapp.Problem); !ok || problem.Code != "host_key_unavailable" {
		t.Fatalf("SFTP opened without loading host trust: %v", err)
	}
}

func TestCredentialRecipientBeforeOpen(t *testing.T) {
	cfg := filepath.Join(isolatedSettingsHome(t), "config.json")
	t.Setenv("MWEAVE_TEST_SHARED", "sealed-password")
	writeFixture(t, cfg, []byte(`{"version":1,"mounts":[{"at":"/old","driver":"webdav","options":{"root_url":"https://old.invalid/files/","username":"reader"},"credential":{"kind":"env","ref":"MWEAVE_TEST_SHARED"}}]}`))
	var requests int
	old := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = old })
	http.DefaultClient = &http.Client{Transport: sealedTransport(func(_ *http.Request) (*http.Response, error) {
		requests++
		return nil, context.Canceled
	})}
	const record = `{"at":"/new","driver":"pan123","connection":"webdav","options":{"root_url":"https://other.invalid/","username":"reader"},"credential":{"kind":"env","ref":"MWEAVE_TEST_SHARED"}}`
	result := invokeCLI(t, cfg, record, false, 1, "mount", "add", "--from", "-", "--json")
	saved, err := cliapp.LoadConfig(cfg)
	if result.Error == nil || result.Error.Code != "credential_scope_conflict" || requests != 0 || err != nil || len(saved.Value.Mounts) != 1 || saved.Value.Mounts[0].At != "/old" {
		t.Fatalf("credential escaped admission: %+v, requests=%d, saved=%+v, error=%v", result.Error, requests, saved.Value.Mounts, err)
	}
}
