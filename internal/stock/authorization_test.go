package stock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostalume/mountweave-cli/internal/cliapp"
)

func managedFixture(t *testing.T) string {
	t.Helper()
	if !cliapp.ManagedStoreSupported {
		t.Skip("managed credential storage is unavailable")
	}
	return filepath.Join(isolatedSettingsHome(t), "config.json")
}

func useSealedTransport(t *testing.T, transport sealedTransport) {
	t.Helper()
	original := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original })
}

func authReply(r *http.Request, status int, body string) (*http.Response, error) {
	return &http.Response{Request: r, StatusCode: status, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
}

func unexpectedAuthRequest(r *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("request outside authorization fixture: %s %s%s", r.Method, r.URL.Host, r.URL.Path)
}

func authSnapshot(t *testing.T, config string) cliapp.ConfigSnapshot {
	t.Helper()
	saved, err := cliapp.LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func authRef(t *testing.T, saved cliapp.ConfigSnapshot, at string) string {
	t.Helper()
	for _, mount := range saved.Value.Mounts {
		if mount.At == at && mount.Credential != nil && mount.Credential.Kind == cliapp.ManagedCredential {
			return mount.Credential.Ref
		}
	}
	t.Fatalf("%s has no saved managed credential", at)
	return ""
}

func readAuthGrant(t *testing.T, provider interface {
	LoadGrant(string) (cliapp.OAuthGrant, error)
}, id string) cliapp.OAuthGrant {
	t.Helper()
	grant, err := provider.LoadGrant(id)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func expireAuthGrant(t *testing.T, grant cliapp.OAuthGrant) {
	t.Helper()
	lease, err := cliapp.AcquireGrant(grant.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	grant.Expires = time.Now().Add(-time.Hour)
	_, err = grant.Save(t.Context(), lease, true)
	if err := errors.Join(err, lease.Lock.Close()); err != nil {
		t.Fatal(err)
	}
}

func authProblem(t *testing.T, got resultForTest, code string) {
	t.Helper()
	if got.Error == nil || got.Error.Code != code {
		t.Fatalf("wanted %s, got %+v", code, got.Error)
	}
}

func TestAliPanScanAndRotation(t *testing.T) {
	if path := os.Getenv("MWEAVE_TEST_ALIPAN_REOPEN"); path != "" {
		http.DefaultTransport = sealedTransport(func(*http.Request) (*http.Response, error) {
			panic("an uncertain checkpoint must reopen without contacting the provider")
		})
		os.Exit(testApp.Run(t.Context(), []string{"--config", path, "ls", "/ali", "--json"}, nil, os.Stdout, os.Stderr, false))
	}
	config := managedFixture(t)
	t.Setenv("MWEAVE_ALIPAN_APP_ID", "fixture-app")
	original := cliapp.LaunchOAuthBrowser
	t.Cleanup(func() { cliapp.LaunchOAuthBrowser = original })
	var image, challenge, id string
	cliapp.LaunchOAuthBrowser = func(_ context.Context, path string) error {
		image = path
		_, err := os.Stat(path)
		return err
	}
	var refreshes atomic.Int32
	useSealedTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "openapi.alipan.com" {
			return unexpectedAuthRequest(r)
		}
		reply := func(body string) (*http.Response, error) { return authReply(r, 200, body) }
		switch r.Method + " " + r.URL.Path {
		case "POST /oauth/authorize/qrcode":
			var fields struct {
				App       string   `json:"client_id"`
				Scopes    []string `json:"scopes"`
				Challenge string   `json:"code_challenge"`
			}
			if json.NewDecoder(r.Body).Decode(&fields) != nil || fields.App != "fixture-app" ||
				!strings.Contains(strings.Join(fields.Scopes, " "), aliPanWriteScope) {
				t.Error("scan did not use the selected app and write scope")
			}
			challenge = fields.Challenge
			return reply(`{"qrCodeUrl":"https://www.alipan.com/authorize?fixture=1","sid":"fixture-session"}`)
		case "GET /oauth/qrcode/fixture-session/status":
			return reply(`{"status":"LoginSuccess","authCode":"fixture-code"}`)
		case "POST /oauth/access_token":
			var fields map[string]string
			if json.NewDecoder(r.Body).Decode(&fields) != nil || fields["client_id"] != "fixture-app" || fields["client_secret"] != "" {
				t.Error("invalid AliPan token request")
			}
			if fields["grant_type"] == "authorization_code" {
				hash := sha256.Sum256([]byte(fields["code_verifier"]))
				if fields["code"] != "fixture-code" || base64.RawURLEncoding.EncodeToString(hash[:]) != challenge {
					t.Error("authorization code has a mismatched PKCE verifier")
				}
				return reply(`{"token_type":"Bearer","access_token":"scan-access","refresh_token":"scan-refresh","expires_in":3600}`)
			}
			n := refreshes.Add(1)
			checkpoint, err := aliPanAuthorization.LoadGrant(id)
			if err != nil || !checkpoint.Pending || fields["refresh_token"] != checkpoint.Refresh {
				t.Errorf("refresh lacked a durable pending checkpoint: %v", err)
			}
			if n == 2 {
				return nil, io.ErrUnexpectedEOF
			}
			return reply(`{"token_type":"Bearer","access_token":"renewed-access","refresh_token":"rotated-refresh","expires_in":3600}`)
		case "GET /oauth/users/scopes":
			return reply(`{"id":"fixture-account","scopes":[{"scope":"user:base"},{"scope":"file:all:read"},{"scope":"file:all:write"}]}`)
		case "POST /adrive/v1.0/user/getDriveInfo":
			return reply(`{"user_id":"fixture-account","name":"Fixture","default_drive_id":"other-drive","resource_drive_id":"resource-drive","backup_drive_id":"backup-drive"}`)
		case "POST /adrive/v1.0/openFile/list":
			var fields struct {
				Drive string `json:"drive_id"`
			}
			if json.NewDecoder(r.Body).Decode(&fields) != nil || fields.Drive != "resource-drive" {
				t.Error("list did not use the selected resource drive")
			}
			if r.Header.Get("Authorization") == "Bearer renewed-access" {
				checkpoint, err := aliPanAuthorization.LoadGrant(id)
				if err != nil || checkpoint.Pending || checkpoint.Refresh != "rotated-refresh" {
					t.Error("list used renewed credentials before confirmation")
				}
			}
			return reply(`{"items":[]}`)
		}
		return unexpectedAuthRequest(r)
	})
	invokeCLI(t, config, "alipan\nopen\nread_write\n\nresource\n\ny\n", true, 0, "mount", "add", "/ali")
	saved := authSnapshot(t, config)
	id = authRef(t, saved, "/ali")
	if _, err := os.Stat(image); image == "" || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scan did not close its image: %q %v", image, err)
	}
	grant := readAuthGrant(t, &aliPanAuthorization, id)
	var options struct {
		Drive string `json:"drive_id"`
		Part  int64  `json:"part_size"`
	}
	if json.Unmarshal(saved.Value.Mounts[0].Options, &options) != nil || options.Drive != "resource-drive" || options.Part != aliPanPartSize ||
		!grant.Covers([]string{aliPanWriteScope}) || grant.Refresh != "scan-refresh" {
		t.Fatalf("scan did not persist the selected drive and scope: %+v", options)
	}
	expireAuthGrant(t, grant)
	invokeCLI(t, config, "", false, 0, "ls", "/ali", "--json")
	expireAuthGrant(t, readAuthGrant(t, &aliPanAuthorization, id))
	for range 2 {
		authProblem(t, invokeCLI(t, config, "", false, 1, "ls", "/ali", "--json"), "interaction_required")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAliPanScanAndRotation$")
	cmd.Env = append(os.Environ(), "MWEAVE_TEST_ALIPAN_REOPEN="+config)
	data, err := cmd.Output()
	var exit *exec.ExitError
	var got resultForTest
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || json.Unmarshal(data, &got) != nil {
		t.Fatalf("restart returned %s (%v)", data, err)
	}
	authProblem(t, got, "interaction_required")
	if refreshes.Load() != 2 || authSnapshot(t, config).Digest != saved.Digest {
		t.Fatal("an uncertain refresh replayed or changed the mount")
	}
}

func Test115ScanAndRecovery(t *testing.T) {
	config := managedFixture(t)
	t.Setenv("MWEAVE_115_APP_ID", "fixture-app")
	original := cliapp.LaunchOAuthBrowser
	t.Cleanup(func() { cliapp.LaunchOAuthBrowser = original })
	cliapp.LaunchOAuthBrowser = func(context.Context, string) error { return errors.New("no image viewer") }
	var challenge, id string
	var scans, refreshes int
	var wrong bool
	useSealedTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" {
			return unexpectedAuthRequest(r)
		}
		auth := r.Header.Get("Authorization")
		reply := func(body string) (*http.Response, error) { return authReply(r, 200, body) }
		route := r.URL.Host + r.URL.Path
		if r.URL.Host == "passportapi.115.com" {
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Method != http.MethodPost || auth != "" {
				t.Error("grant request crossed into the control credential boundary")
			}
		}
		switch route {
		case "passportapi.115.com/open/authDeviceCode":
			scans++
			challenge = r.PostForm.Get("code_challenge")
			if r.PostForm.Get("client_id") != "fixture-app" || r.PostForm.Get("code_challenge_method") != "sha256" || len(challenge) != 43 {
				t.Error("incorrect 115 PKCE admission")
			}
			return reply(`{"state":1,"code":0,"data":{"uid":"sealed-device","time":12,"sign":"sealed-sign","qrcode":"https://115.invalid/fixture"}}`)
		case "qrcodeapi.115.com/get/status/":
			return reply(`{"state":1,"code":0,"data":{"status":2}}`)
		case "passportapi.115.com/open/deviceCodeToToken":
			hash := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if r.PostForm.Get("uid") != "sealed-device" || base64.RawURLEncoding.EncodeToString(hash[:]) != challenge {
				t.Error("exchange did not use the corresponding challenge")
			}
			return reply(`{"state":1,"code":0,"data":{"access_token":"sealed-access","refresh_token":"sealed-refresh","expires_in":7200}}`)
		case "passportapi.115.com/open/refreshToken":
			refreshes++
			grant, err := pan115Authorization.LoadGrant(id)
			if err != nil || !grant.Pending || r.PostForm.Get("refresh_token") != grant.Refresh {
				t.Error("115 refreshed before writing the pending checkpoint")
			}
			return nil, io.ErrUnexpectedEOF
		case "proapi.115.com/open/user/info":
			if auth != "Bearer sealed-access" {
				t.Error("identity request did not use the issued access token")
			}
			user := int64(9007199254740993)
			if wrong {
				user++
			}
			return reply(fmt.Sprintf(`{"state":true,"code":0,"data":{"user_id":%d,"user_name":"Fixture"}}`, user))
		case "proapi.115.com/open/ufile/files":
			if auth != "Bearer sealed-access" || r.URL.Query().Get("cid") != "0" {
				t.Error("list did not use the bound token and root")
			}
			return reply(`{"state":true,"cid":0,"count":1,"offset":0,"limit":1,"data":[{"fid":4,"pid":0,"fn":"file.txt","fc":"1","fs":6,"pc":"fixture-pick"}]}`)
		}
		return unexpectedAuthRequest(r)
	})
	const add = "115\n\n\n\ny\n"
	invokeCLI(t, config, add, true, 0, "mount", "add", "/115")
	invokeCLI(t, config, add, true, 0, "mount", "add", "/shared")
	saved := authSnapshot(t, config)
	id = authRef(t, saved, "/115")
	grant := readAuthGrant(t, &pan115Authorization, id)
	if grant.Account != "9007199254740993" || authRef(t, saved, "/shared") != id || scans != 1 {
		t.Fatal("115 did not preserve the exact account identity and reuse its grant")
	}
	expireAuthGrant(t, grant)
	authProblem(t, invokeCLI(t, config, "", false, 1, "ls", "/115", "--json"), "interaction_required")
	wrong = true
	invokeCLI(t, config, "reauthorize\n", true, 1, "mount", "edit", "/115")
	if current := readAuthGrant(t, &pan115Authorization, id); !current.Pending || current.Account != grant.Account {
		t.Fatal("a different account replaced the pending grant")
	}
	wrong = false
	invokeCLI(t, config, "reauthorize\n", true, 0, "mount", "edit", "/115")
	got := invokeCLI(t, config, "", false, 0, "ls", "/shared", "--json")
	if refreshes != 1 || authSnapshot(t, config).Digest != saved.Digest || !bytes.Contains(got.Result, []byte("file.txt")) {
		t.Fatal("same-account recovery did not restore the shared mount")
	}
}

func Test123ApplicationReissue(t *testing.T) {
	config := managedFixture(t)
	t.Setenv("MWEAVE_123_FIXTURE_SECRET", "fixture-secret")
	t.Setenv("MWEAVE_123_FIXTURE_NEW_SECRET", "fixture-new")
	const id = "9007199254740993"
	var issued int
	var wrong bool
	useSealedTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "open-api.123pan.com" {
			return unexpectedAuthRequest(r)
		}
		if r.Header.Get("Platform") != "open_platform" {
			t.Error("missing 123 API platform")
		}
		auth := r.Header.Get("Authorization")
		if r.URL.Path != "/api/v1/access_token" && auth != fmt.Sprintf("Bearer token-%d", issued) {
			t.Error("request did not use the current 123 application grant")
		}
		reply := func(data string) (*http.Response, error) { return authReply(r, 200, `{"code":0,"data":`+data+"}") }
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/access_token":
			want := "fixture-secret"
			if issued != 0 {
				want = "fixture-new"
			}
			var app map[string]string
			if json.NewDecoder(r.Body).Decode(&app) != nil || app["clientID"] != "fixture-app" ||
				app["clientSecret"] != want || auth != "" {
				t.Error("wrong 123 application issuance credential")
			}
			issued++
			return reply(fmt.Sprintf(`{"accessToken":"token-%d","expiredAt":%q}`, issued, time.Now().Add(time.Hour).Format(time.RFC3339)))
		case "GET /api/v1/user/info":
			uid := id
			if wrong {
				uid = "9007199254740994"
			}
			return reply(`{"uid":` + uid + `,"nickname":"Fixture"}`)
		case "GET /api/v1/file/detail":
			if r.URL.Query().Get("fileID") != id {
				t.Error("the folder ID was rounded")
			}
			return reply(`{"fileID":` + id + `,"parentFileID":0,"filename":"Archive","type":1,"size":0,"trashed":0}`)
		case "GET /api/v2/file/list":
			if r.URL.Query().Get("parentFileId") == "0" {
				return reply(`{"lastFileId":-1,"fileList":[{"fileId":` + id + `,"parentFileId":0,"filename":"Archive","type":1,"size":0,"trashed":0}]}`)
			}
			if r.URL.Query().Get("parentFileId") != id {
				t.Error("a different root was queried")
			}
			return reply(`{"lastFileId":-1,"fileList":[]}`)
		}
		return unexpectedAuthRequest(r)
	})
	var output, prompts bytes.Buffer
	input := strings.NewReader("pan123\nopen\nauthorize\nfixture-app\nenv:MWEAVE_123_FIXTURE_SECRET\n/Archive\ny\n")
	if code := testApp.Run(t.Context(), []string{"--config", config, "mount", "add", "/app"}, input, &output, &prompts, true); code != 0 ||
		!strings.Contains(prompts.String(), "Location     \"/Archive\"") ||
		!strings.Contains(prompts.String(), "Selected location verified\nSave mount?") {
		t.Fatalf("123 setup or prepared location review failed: %d %s %s", code, &output, &prompts)
	}
	saved := authSnapshot(t, config)
	ref := authRef(t, saved, "/app")
	original := readAuthGrant(t, &pan123Authorization, ref)
	if original.Account != id || original.ClientSecret != "" || original.Refresh != "" {
		t.Fatal("wrong account or raw application material stored")
	}
	const replace = "reauthorize\n\nenv:MWEAVE_123_FIXTURE_NEW_SECRET\n"
	wrong = true
	invokeCLI(t, config, replace, true, 1, "mount", "edit", "/app")
	if grant := readAuthGrant(t, &pan123Authorization, ref); grant.Access != original.Access || grant.SecretRef != original.SecretRef {
		t.Fatal("a different UID replaced the grant")
	}
	wrong = false
	invokeCLI(t, config, replace, true, 0, "mount", "edit", "/app")
	updated := readAuthGrant(t, &pan123Authorization, ref)
	expireAuthGrant(t, updated)
	invokeCLI(t, config, "", false, 0, "ls", "/app", "--json")
	if grant := readAuthGrant(t, &pan123Authorization, ref); issued != 4 || grant.Access != "token-4" ||
		grant.SecretRef.Ref != "MWEAVE_123_FIXTURE_NEW_SECRET" || authSnapshot(t, config).Digest != saved.Digest {
		t.Fatal("same-account reissue did not use the updated reference and preserve the mount")
	}
}

func TestGraphDeviceAuthorization(t *testing.T) {
	config := managedFixture(t)
	const (
		appID  = "11112222-aaaa-4333-bbbb-4444cccc5555"
		tenant = "00001111-aaaa-4222-bbbb-3333cccc4444"
		login  = "login.microsoftonline.com/" + tenant + "/oauth2/v2.0/"
		folder = `{"id":"vault","name":"Archive","folder":{},"parentReference":{"id":"root","driveId":"drive"}}`
	)
	var calls, polls, renewals int
	var last time.Time
	var cancelWait context.CancelFunc
	useSealedTransport(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Scheme != "https" {
			return unexpectedAuthRequest(r)
		}
		route := r.URL.Host + r.URL.Path
		if strings.HasPrefix(route, login) {
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" || r.PostForm.Get("client_id") != appID || r.PostForm.Has("client_secret") {
				t.Error("wrong device authorization recipient or client")
			}
		} else if auth := r.Header.Get("Authorization"); auth != "Bearer graph-issued" && auth != "Bearer graph-renewed" {
			t.Error("Graph API did not use the managed access token")
		}
		reply := func(body string) (*http.Response, error) { return authReply(r, 200, body) }
		switch route {
		case login + "devicecode":
			polls, last = 0, time.Now()
			if !strings.Contains(r.PostForm.Get("scope"), "offline_access") {
				t.Error("device request omitted renewable authorization")
			}
			return reply(`{"device_code":"private-device","user_code":"USER-CODE","verification_uri":"https://www.microsoft.com/devicelogin","expires_in":30,"interval":1}`)
		case login + "token":
			if r.PostForm.Get("grant_type") == "refresh_token" {
				renewals++
				if r.PostForm.Get("refresh_token") != "graph-refresh" {
					t.Error("refresh used the wrong grant")
				}
				return reply(`{"access_token":"graph-renewed","refresh_token":"graph-replaced","scope":"Files.Read User.Read","expires_in":3600,"token_type":"Bearer"}`)
			}
			polls++
			if time.Since(last) < time.Second || r.PostForm.Get("device_code") != "private-device" {
				t.Error("poll ignored device timing or identity")
			}
			last = time.Now()
			if cancelWait != nil {
				cancelWait()
			}
			if polls == 1 {
				return authReply(r, 400, `{"error":"authorization_pending"}`)
			}
			return reply(`{"access_token":"graph-issued","refresh_token":"graph-refresh","scope":"Files.Read User.Read","expires_in":3600,"token_type":"Bearer"}`)
		case "graph.microsoft.com/v1.0/me":
			return reply(`{"id":"fixture-user","displayName":"Fixture"}`)
		case "graph.microsoft.com/v1.0/drives/drive/root":
			return reply(`{"id":"root","name":"Drive","folder":{}}`)
		case "graph.microsoft.com/v1.0/drives/drive/items/root:/Archive", "graph.microsoft.com/v1.0/drives/drive/items/vault":
			return reply(folder)
		case "graph.microsoft.com/v1.0/drives/drive/items/vault/children":
			return reply(`{"value":[]}`)
		}
		return unexpectedAuthRequest(r)
	})
	synctest.Test(t, func(t *testing.T) {
		var output, prompts bytes.Buffer
		const destination = "/device"
		args := []string{"--config", config, "mount", "add", destination, "--timeout", "15s"}
		add := "graphdrive\n\n\n" + appID + "\n" + tenant + "\ndrive\n/Archive\ny\n"
		if code := testApp.Run(t.Context(), args, strings.NewReader(add), &output, &prompts, true); code != 0 ||
			polls != 2 || !strings.Contains(prompts.String(), "USER-CODE") || strings.Contains(prompts.String(), "private-device") {
			t.Fatalf("device flow: %d, %d polls, %s", code, polls, &prompts)
		}
		saved := authSnapshot(t, config)
		ref := authRef(t, saved, destination)
		grant := readAuthGrant(t, &graphAuthorization, ref)
		if grant.Account != tenant+"/fixture-user" || grant.Refresh != "graph-refresh" {
			t.Fatal("device grant was not bound to the verified tenant account")
		}
		const wrong = `{"at":"/elsewhere","driver":"graphdrive","options":{"drive_id":"drive","root_id":"vault","endpoint":"https://microsoftgraph.chinacloudapi.cn"},"credential":{"kind":"managed","ref":%q}}`
		before := calls
		invokeCLI(t, config, fmt.Sprintf(wrong, ref), false, 1, "mount", "add", "--from", "-", "--json")
		if calls != before {
			t.Fatal("managed Graph credential crossed to another origin")
		}
		expireAuthGrant(t, grant)
		invokeCLI(t, config, "", false, 0, "ls", destination, "--json")
		grant = readAuthGrant(t, &graphAuthorization, ref)
		if renewals != 1 || grant.Refresh != "graph-replaced" {
			t.Fatal("renewal did not confirm a replacement grant")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancelWait = cancel
		prompts.Reset()
		if code := testApp.Run(ctx, []string{"--config", config, "mount", "edit", destination}, strings.NewReader("reauthorize\n"), &output, &prompts, true); code != 1 ||
			polls != 1 || !strings.Contains(prompts.String(), "cancelled") {
			t.Fatalf("device polling cancellation: %d, %d, %s", code, polls, &prompts)
		}
		cancel()
		if readAuthGrant(t, &graphAuthorization, ref).Refresh != grant.Refresh || authSnapshot(t, config).Digest != saved.Digest {
			t.Fatal("cancelled device flow changed authorization or configuration")
		}
	})
}

func TestWeiyunStaticApproval(t *testing.T) {
	config := managedFixture(t)
	launch := cliapp.LaunchOAuthBrowser
	t.Cleanup(func() { cliapp.LaunchOAuthBrowser = launch })
	var code string
	var opens, exchanges int
	var reject bool
	cliapp.LaunchOAuthBrowser = func(_ context.Context, target string) error {
		opens++
		u, err := url.Parse(target)
		if err != nil {
			return err
		}
		code = u.Query().Get("code")
		if _, err := hex.DecodeString(code); err != nil || len(code) != 16 ||
			u.Scheme != "https" || u.Host != "www.weiyun.com" || u.Path != "/authorize/token" {
			t.Error("unexpected Weiyun approval page")
		}
		return nil
	}
	useSealedTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != "www.weiyun.com" || r.Header.Get("Authorization") != "" {
			return unexpectedAuthRequest(r)
		}
		switch r.URL.Path {
		case "/api/v3/mcp/token/code":
			var payload struct{ Code string }
			if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Code != code || code == "" || r.Header.Get("WyHeader") != "" {
				t.Error("poll did not use the approval URL's identity")
			}
			exchanges++
			if exchanges == 1 {
				return authReply(r, 200, `{"code":"11510","message":"pending"}`)
			}
			if reject {
				return authReply(r, 200, `{"token":"bad-token"}`)
			}
			return authReply(r, 200, `{"token":"static-fixture-token"}`)
		case "/api/v3/mcpserver":
			if r.Header.Get("WyHeader") == "mcp_token=bad-token" {
				return authReply(r, 403, `{}`)
			}
			var call struct {
				ID     uint64
				Method string
				Params struct{ Name string }
			}
			if json.NewDecoder(r.Body).Decode(&call) != nil || call.Method != "tools/call" || call.Params.Name != "weiyun.list" ||
				r.Header.Get("WyHeader") != "mcp_token=static-fixture-token" {
				t.Error("list lacks the approved static authorization")
			}
			return authReply(r, 200, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"{\"pdir_key\":\"abcd\",\"finish_flag\":true,\"file_list\":[{\"file_id\":\"target\",\"filename\":\"note\",\"file_size\":6}]}"}]}}`, call.ID))
		}
		return unexpectedAuthRequest(r)
	})
	var out, prompts bytes.Buffer
	if exit := testApp.Run(t.Context(), []string{"--config", config, "mount", "add", "/original"}, strings.NewReader("weiyun\n\n\n\ny\n"), &out, &prompts, true); exit != 0 ||
		!strings.Contains(prompts.String(), "operate on all files in your account") || strings.Contains(prompts.String(), "Continue after approving access") {
		t.Fatalf("static authorization did not finish automatically: %d %s %s", exit, &out, &prompts)
	}
	invokeCLI(t, config, "weiyun\n\n\n\ny\n", true, 0, "mount", "add", "/alias")
	saved := authSnapshot(t, config)
	ref := authRef(t, saved, "/original")
	grant := readAuthGrant(t, &weiyunAuthorization, ref)
	if opens != 1 || exchanges != 2 || authRef(t, saved, "/alias") != ref || !grant.Expires.IsZero() {
		t.Fatal("mount did not reuse the static grant without acquiring another credential")
	}
	reject = true
	invokeCLI(t, config, "switch\n\ny\n", true, 1, "mount", "edit", "/original")
	records, err := filepath.Glob(filepath.Join(filepath.Dir(config), "mweave", "credentials", "*.grant"))
	if err != nil || len(records) != 1 || authSnapshot(t, config).Digest != saved.Digest {
		t.Fatalf("unusable static grant changed saved state: %d %v", len(records), err)
	}
}

func TestLoopbackAuthorization(t *testing.T) {
	config := managedFixture(t)
	t.Setenv("MWEAVE_DROPBOX_APP_KEY", "fixture-app")
	originalLaunch, originalListen := cliapp.LaunchOAuthBrowser, cliapp.ListenOAuthCallback
	t.Cleanup(func() {
		cliapp.LaunchOAuthBrowser, cliapp.ListenOAuthCallback = originalLaunch, originalListen
	})
	var callbackAddress atomic.Value
	cliapp.ListenOAuthCallback = func(ctx context.Context, address string) (net.Listener, error) {
		if address != dropboxAuthorization.ListenAddress && address != googleDriveAuthorization.ListenAddress {
			return nil, fmt.Errorf("unexpected callback address %s", address)
		}
		var lc net.ListenConfig
		listener, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
		if err == nil {
			callbackAddress.Store(listener.Addr().String())
		}
		return listener, err
	}
	callbackTransport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", callbackAddress.Load().(string))
	}, DisableKeepAlives: true}
	callback := &http.Client{Transport: callbackTransport}
	const dropboxReadScope = "account_info.read files.content.read files.metadata.read"
	var browserQuery atomic.Value
	cliapp.LaunchOAuthBrowser = func(ctx context.Context, target string) error {
		u, err := url.Parse(target)
		if err != nil {
			return err
		}
		q := u.Query()
		redirect := q.Get("redirect_uri")
		switch u.Host {
		case "www.dropbox.com":
			if q.Get("client_id") != "fixture-app" || q.Get("token_access_type") != "offline" || redirect != dropboxRedirect ||
				strings.Join(cliapp.OAuthScopeSet(q.Get("scope")), " ") != dropboxReadScope {
				return errors.New("incorrect Dropbox browser authorization")
			}
		case "accounts.google.com":
			if q.Get("client_id") != "fixture-google" || q.Get("access_type") != "offline" ||
				redirect != "http://"+callbackAddress.Load().(string)+"/callback" || q.Get("scope") != googleDriveReadScope ||
				q.Has("client_secret") {
				return errors.New("incorrect Google desktop authorization")
			}
		default:
			return errors.New("unexpected browser authorization host")
		}
		if q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("code_challenge") == "" {
			return errors.New("missing callback state or PKCE challenge")
		}
		browserQuery.Store(q)
		for _, response := range []struct {
			state, code string
			status      int
		}{
			{"tampered", "untrusted", http.StatusBadRequest},
			{q.Get("state"), "fixture-code", http.StatusOK},
		} {
			params := url.Values{"state": {response.state}, "code": {response.code}}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, redirect+"?"+params.Encode(), nil)
			if err != nil {
				return err
			}
			resp, err := callback.Do(req)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(io.Discard, resp.Body)
			if err := errors.Join(copyErr, resp.Body.Close()); err != nil {
				return err
			}
			if resp.StatusCode != response.status {
				return fmt.Errorf("callback status %d, wanted %d", resp.StatusCode, response.status)
			}
		}
		return nil
	}
	var renewals int
	useSealedTransport(t, func(r *http.Request) (*http.Response, error) {
		auth := r.Header.Get("Authorization")
		switch r.URL.Host + r.URL.Path {
		case "api.dropboxapi.com/oauth2/token", "oauth2.googleapis.com/token":
			defer r.Body.Close()
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			f := r.PostForm
			isGoogle := r.URL.Host == "oauth2.googleapis.com"
			if r.Method != http.MethodPost || auth != "" ||
				isGoogle && (f.Get("client_id") != "fixture-google" || f.Get("client_secret") != "sealed-google-secret") ||
				!isGoogle && (f.Get("client_id") != "fixture-app" || f.Has("client_secret")) {
				return nil, errors.New("incorrect token recipient or application")
			}
			if isGoogle && f.Get("grant_type") == "refresh_token" && f.Get("refresh_token") == "sealed-google-refresh" {
				renewals++
				return authReply(r, http.StatusOK, `{"access_token":"google-renewed","token_type":"Bearer","expires_in":3600}`)
			}
			q := browserQuery.Load().(url.Values)
			hash := sha256.Sum256([]byte(f.Get("code_verifier")))
			if f.Get("grant_type") != "authorization_code" || f.Get("code") != "fixture-code" || f.Get("redirect_uri") != q.Get("redirect_uri") ||
				base64.RawURLEncoding.EncodeToString(hash[:]) != q.Get("code_challenge") {
				return nil, errors.New("exchange does not match the accepted callback")
			}
			if isGoogle {
				return authReply(r, http.StatusOK, fmt.Sprintf(`{"access_token":"google-access","refresh_token":"sealed-google-refresh","scope":%q,"token_type":"Bearer","expires_in":3600}`, googleDriveReadScope))
			}
			return authReply(r, http.StatusOK, fmt.Sprintf(`{"access_token":"dropbox-access","refresh_token":"sealed-dropbox-refresh","account_id":"db-account","scope":%q,"token_type":"Bearer","expires_in":3600}`, dropboxReadScope))
		case "api.dropboxapi.com/2/users/get_current_account", "api.dropboxapi.com/2/files/get_metadata", "api.dropboxapi.com/2/files/list_folder":
			if auth != "Bearer dropbox-access" {
				return nil, errors.New("incorrect Dropbox access credential")
			}
			switch r.URL.Path {
			case "/2/users/get_current_account":
				return authReply(r, http.StatusOK, `{"account_id":"db-account","email":"db@fixture.invalid"}`)
			case "/2/files/get_metadata":
				return authReply(r, http.StatusOK, `{".tag":"folder","id":"id:folder","name":"Folder"}`)
			default:
				return authReply(r, http.StatusOK, `{"entries":[{".tag":"file","id":"id:file","name":"dropbox.txt","size":1}],"cursor":"cursor","has_more":false}`)
			}
		case "www.googleapis.com/drive/v3/about", "www.googleapis.com/drive/v3/files/vault", "www.googleapis.com/drive/v3/files":
			if auth != "Bearer google-access" && auth != "Bearer google-renewed" {
				return nil, errors.New("incorrect Google access credential")
			}
			switch r.URL.Path {
			case "/drive/v3/about":
				return authReply(r, http.StatusOK, `{"user":{"permissionId":"google-account","emailAddress":"google@fixture.invalid"}}`)
			case "/drive/v3/files/vault":
				return authReply(r, http.StatusOK, `{"id":"vault","name":"Vault","mimeType":"application/vnd.google-apps.folder"}`)
			default:
				return authReply(r, http.StatusOK, `{"files":[{"id":"file","name":"drive.txt","mimeType":"text/plain","size":"1","parents":["vault"]}]}`)
			}
		default:
			return unexpectedAuthRequest(r)
		}
	})

	invokeCLI(t, config, "dropbox\n\n\n\n/Folder\ny\n", true, 0, "mount", "add", "/dropbox")
	if got := invokeCLI(t, config, "", false, 0, "ls", "/dropbox", "--json"); !bytes.Contains(got.Result, []byte("dropbox.txt")) {
		t.Fatalf("Dropbox listing: %s", got.Result)
	}
	appFile := filepath.Join(filepath.Dir(config), "desktop.json")
	writeFixture(t, appFile, []byte(`{"installed":{"client_id":"fixture-google","client_secret":"sealed-google-secret","token_uri":"https://untrusted.invalid"}}`))
	invokeCLI(t, config, "googledrive\n\n\n"+appFile+"\nvault\ny\n", true, 0, "mount", "add", "/drive")
	saved := authSnapshot(t, config)
	ref := authRef(t, saved, "/drive")
	grant := readAuthGrant(t, googleDriveAuthorization, ref)
	if _, err := dropboxAuthorization.LoadGrant(ref); err == nil || grant.Account != "google-account" {
		t.Fatalf("Google account binding is invalid")
	}
	if err := os.Remove(appFile); err != nil {
		t.Fatal(err)
	}
	expireAuthGrant(t, grant)
	if got := invokeCLI(t, config, "", false, 0, "ls", "/drive", "--json"); !bytes.Contains(got.Result, []byte("drive.txt")) ||
		renewals != 1 || authSnapshot(t, config).Digest != saved.Digest || readAuthGrant(t, googleDriveAuthorization, ref).Access != "google-renewed" {
		t.Fatalf("Google grant did not renew from the protected record: %s", got.Result)
	}
	if data, err := os.ReadFile(config); err != nil || bytes.Contains(data, []byte("sealed-")) || bytes.Contains(data, []byte("@fixture.invalid")) {
		t.Fatal("saved configuration includes credentials or private account labels")
	}
}
