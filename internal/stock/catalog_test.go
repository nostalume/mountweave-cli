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

	localsource "github.com/nostalume/mountweave-cli/cli/source/local"
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
		{"baidupan", "", `{"root_path":"/Archive"}`, "https://pan.baidu.com", ""},
		{"baidupan", "open", `{"root_path":"/Archive"}`, "https://pan.baidu.com", ""},
		{"pan115", "", `{"root_id":"42"}`, "Authorization https://proapi.115.com", ""},
		{"pan123", "", `{"root_id":"42"}`, "Authorization https://open-api.123pan.com", "Folder ID 42"},
		{"pan123", "website", `{"root":"/"}`, "https://yun.123pan.cn/", ""},
		{"pan123", "webdav", `{"root_url":"https://example.test/webdav/","username":"reader"}`, "webdav https example.test 443", "https://example.test/webdav/"},
		{"pikpak", "", `{"device_id":"0123456789abcdef0123456789abcdef"}`, "Authorization https://api-drive.mypikpak.com", ""},
		{"alipan-web", "", `{"account":"fixture","drive_id":"resource","root_id":"root"}`, "https://www.alipan.com/drive", ""},
		{"githubrepo", "", `{"repository":"example/tree","ref":"main"}`, "Authorization https://api.github.com", ""},
		{"atomgit", "", `{"repository":"example/tree","ref":"main"}`, "Authorization https://api.atomgit.com", ""},
		{"guangyapan", "", `{"client_id":"fixture"}`, "Authorization https://openapi.guangyapan.com", ""},
		{"guangyapan", "open", `{"client_id":"fixture"}`, "Authorization https://openapi.guangyapan.com", ""},
	}
	for _, tc := range cases {
		src, err := cliapp.PrepareMountSource(testApp, cliapp.MountRecord{At: "/files", Driver: tc.id, Connection: tc.connection, Options: json.RawMessage(tc.options)}, t.TempDir())
		if err != nil || src.Audience.Origin != tc.origin || src.Location.Location != tc.location {
			t.Errorf("%s/%s: origin %q, location %q, error %v", tc.id, tc.connection, src.Audience.Origin, src.Location.Location, err)
		}
	}
	for _, tc := range []struct {
		id, origin string
		route      *cliapp.Route
	}{
		{"baidupan", "https://pan.baidu.com/disk/main", baiduWebsiteRoute()},
		{"pan115", "https://f.115.com/", pan115WebsiteRoute()},
		{"mediafire", "https://app.mediafire.com/", mediaFireWebsiteRoute()},
		{"guangyapan", "https://www.guangyapan.com/#/home/all", guangyaWebsiteRoute()},
		{"tianyipan", "https://cloud.189.cn/web/login.html", tianyiWebsiteRoute()},
	} {
		website, err := cliapp.PrepareMountSource(testApp, cliapp.MountRecord{
			At: "/files", Driver: tc.id, Connection: "website", Options: json.RawMessage(`{"root":"/Archive"}`),
		}, t.TempDir())
		if tc.route == nil {
			if err == nil || cliapp.AppProblem(testApp, err).Code != "mount_unavailable" {
				t.Fatalf("%s website unavailable build: %v", tc.id, err)
			}
		} else if err != nil || website.OpenManaged == nil || website.Audience.Origin != tc.origin {
			t.Fatalf("%s website route: audience %+v, open configured %t (%v)", tc.id, website.Audience, website.OpenManaged != nil, err)
		}
	}
	r2, cos := s3Presets["r2"], s3Presets["cos"]
	if r2.Region != "auto" || r2.Endpoint != "" || !cos.ListV1 || cos.Region != "ap-guangzhou" {
		t.Fatalf("S3 defaults: R2 %+v, COS %+v", r2, cos)
	}
	named, err := prepareS3(s3Options{Bucket: "files", Region: "local", Endpoint: "http://127.0.0.1:9000"}, "")
	if err != nil || named.Mode != cliapp.SDKNamedProfile || named.Audience.Origin != "http://127.0.0.1:9000" {
		t.Fatalf("custom S3 profile: %+v (%v)", named.Audience, err)
	}
	const edit = `{"at":"/nas","driver":"sftp","options":{"address":"127.0.0.1","username":"fixture","known_hosts":"missing_hosts","identity_file":"key"}`
	const ref = `,"credential":{"kind":"env","ref":"TEST_SFTP_KEY"}`
	cfg := filepath.Join(t.TempDir(), "config.json")
	before := `{"version":1,"mounts":[` + edit + ref + `}]}`
	writeFixture(t, cfg, []byte(before))
	result := invokeCLI(t, cfg, edit+`,"credential":null}`, false, 1, "mount", "edit", "/nas", "--from", "-", "--json")
	if after, err := os.ReadFile(cfg); err != nil || string(after) != before || result.Error == nil || result.Error.Code != "host_key_unavailable" {
		t.Fatalf("clearing an SSH passphrase must verify host trust and preserve the old record on failure: %+v (%v)", result.Error, err)
	}
}

func TestNumberedMountChoices(t *testing.T) {
	local, _ := cliapp.ConnectionFor(localsource.Source(), "")
	first, second := local, local
	first.Method, second.Method = "first", "second"
	app, err := cliapp.NewApp(nil,
		cliapp.SingleSource("fixture-alpha", "Local folders", *local.Route),
		cliapp.ServiceSource("fixture-bravo", "Personal archive", first, second),
		cliapp.SingleSource("fixture-charlie", "Shared archive", *local.Route),
	)
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.json")
	input := strings.NewReader("?\narchive\n0\n1\n9\n2\n" + t.TempDir() + "\ny\n")
	var diagnostics bytes.Buffer
	args := []string{"--config", cfg, "mount", "add", "/archive"}
	if code := app.Run(t.Context(), args, input, &bytes.Buffer{}, &diagnostics, true); code != 0 {
		t.Fatalf("exit %d: %s", code, &diagnostics)
	}
	saved, err := cliapp.LoadConfig(cfg)
	if err != nil || len(saved.Value.Mounts) != 1 ||
		saved.Value.Mounts[0].Driver != "fixture-bravo" || saved.Value.Mounts[0].Connection != "second" {
		t.Fatalf("numbered mount: %+v (%v)", saved.Value.Mounts, err)
	}
	for _, want := range []string{"1. fixture-alpha", "1. fixture-bravo", "2. second"} {
		if !strings.Contains(diagnostics.String(), want) {
			t.Fatalf("missing %q in %s", want, &diagnostics)
		}
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
