package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrowserAuthorizationWorkflow(t *testing.T) {
	if !managedStoreSupported {
		t.Skip("managed credential storage is unavailable")
	}
	home := t.TempDir()
	t.Setenv("APPDATA", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("MWEAVE_DROPBOX_APP_KEY", "fixture-app")
	configPath := filepath.Join(home, "config.json")
	ctx := context.Background()

	oldTransport, oldLaunch, oldListen := http.DefaultTransport, launchOAuthBrowser, listenOAuthCallback
	t.Cleanup(func() {
		http.DefaultTransport, launchOAuthBrowser, listenOAuthCallback = oldTransport, oldLaunch, oldListen
	})
	var callbackAddress atomic.Value
	listenOAuthCallback = func(ctx context.Context, address string) (net.Listener, error) {
		if address != dropboxAuthorization.listenAddress && address != googleDriveAuthorization.listenAddress {
			t.Errorf("unexpected fixed callback address %q", address)
		}
		var lc net.ListenConfig
		l, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
		if err == nil {
			callbackAddress.Store(l.Addr().String())
		}
		return l, err
	}
	local := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", callbackAddress.Load().(string))
	}}}
	defer local.CloseIdleConnections()
	const full = "account_info.read files.content.read files.metadata.read"
	var mode, challenge, requestedScope, redirectURI atomic.Value
	mode.Store("initial")
	var browsers, refreshes atomic.Int32
	var rejectRefresh, loseRefresh, rejectGoogleRefresh atomic.Bool
	launchOAuthBrowser = func(ctx context.Context, target string) error {
		browsers.Add(1)
		u, err := url.Parse(target)
		if err != nil {
			return err
		}
		q := u.Query()
		redirect := q.Get("redirect_uri")
		google := u.Host == "accounts.google.com"
		if google {
			if q.Get("client_id") != "fixture-google" || q.Get("access_type") != "offline" ||
				redirect != "http://"+callbackAddress.Load().(string)+"/callback" || q.Get("scope") != googleDriveReadScope ||
				strings.Contains(target, "sealed-google-secret") {
				t.Error("unexpected Google desktop authorization request")
			}
		} else if u.Host != "www.dropbox.com" || q.Get("client_id") != "fixture-app" ||
			redirect != dropboxRedirect || q.Get("token_access_type") != "offline" {
			t.Error("unexpected Dropbox authorization request")
		}
		if q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
			t.Error("unexpected authorization request")
		}
		challenge.Store(q.Get("code_challenge"))
		requestedScope.Store(q.Get("scope"))
		redirectURI.Store(redirect)
		callback := func(params url.Values, status int) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, redirect+"?"+params.Encode(), nil)
			if err != nil {
				return err
			}
			resp, err := local.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != status {
				t.Errorf("callback status %d, want %d", resp.StatusCode, status)
			}
			_, err = io.Copy(io.Discard, resp.Body)
			return err
		}
		if err := callback(url.Values{"state": {"tampered"}, "code": {"untrusted"}}, http.StatusBadRequest); err != nil {
			return err
		}
		value := mode.Load().(string)
		params := url.Values{"state": {q.Get("state")}}
		if value == "cancel" {
			params.Set("error", "access_denied")
		} else {
			params.Set("code", value)
		}
		if value == "switch" && q.Get("prompt") != "login" ||
			(value == "same" || value == "wrong" || value == "narrow") && q.Get("prompt") != "consent" {
			t.Error("unexpected account selection behavior")
		}
		return callback(params, http.StatusOK)
	}
	http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		auth := r.Header.Get("Authorization")
		switch r.URL.Host + r.URL.Path {
		case "oauth2.googleapis.com/token":
			defer r.Body.Close()
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			f := r.PostForm
			if auth != "" || f.Get("client_id") != "fixture-google" || f.Get("client_secret") != "sealed-google-secret" {
				t.Error("Google token exchange used the wrong client")
			}
			switch f.Get("grant_type") {
			case "authorization_code":
				hash := sha256.Sum256([]byte(f.Get("code_verifier")))
				if base64.RawURLEncoding.EncodeToString(hash[:]) != challenge.Load() || f.Get("redirect_uri") != redirectURI.Load() ||
					f.Get("code") != "google" {
					t.Error("Google exchange did not use the dynamic callback or the matching PKCE verifier")
				}
				fmt.Fprintf(w, `{"access_token":"sealed-google-access","refresh_token":"sealed-google-refresh","scope":%q,"token_type":"Bearer","expires_in":3600}`, googleDriveReadScope)
			case "refresh_token":
				if f.Get("refresh_token") != "sealed-google-refresh" {
					t.Error("Google refresh used the wrong record")
				}
				if rejectGoogleRefresh.Load() {
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"error":"invalid_grant"}`)
				} else {
					io.WriteString(w, `{"access_token":"sealed-google-renewed","token_type":"Bearer","expires_in":3600}`)
				}
			default:
				t.Error("unexpected Google token request")
			}
		case "www.googleapis.com/drive/v3/about", "www.googleapis.com/drive/v3/files/vault", "www.googleapis.com/drive/v3/files":
			if auth != "Bearer sealed-google-access" && auth != "Bearer sealed-google-renewed" {
				t.Error("Google Drive received an unexpected credential")
			}
			switch r.URL.Path {
			case "/drive/v3/about":
				io.WriteString(w, `{"user":{"permissionId":"fixture-user","emailAddress":"google@fixture.invalid"}}`)
			case "/drive/v3/files/vault":
				io.WriteString(w, `{"id":"vault","name":"Vault","mimeType":"application/vnd.google-apps.folder"}`)
			default:
				io.WriteString(w, `{"files":[{"id":"file","name":"drive.txt","mimeType":"text/plain","size":"1","parents":["vault"]}]}`)
			}
		case "api.dropboxapi.com/oauth2/token":
			defer r.Body.Close()
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			f := r.PostForm
			if r.Method != http.MethodPost || auth != "" || f.Get("client_id") != "fixture-app" || f.Has("client_secret") {
				t.Error("token request crossed its credential boundary")
			}
			switch f.Get("grant_type") {
			case "authorization_code":
				hash := sha256.Sum256([]byte(f.Get("code_verifier")))
				if base64.RawURLEncoding.EncodeToString(hash[:]) != challenge.Load() || f.Get("redirect_uri") != dropboxRedirect {
					t.Error("token exchange did not match the PKCE challenge")
				}
				code := f.Get("code")
				account, scopes := "first", requestedScope.Load().(string)
				switch code {
				case "wrong", "switch":
					account = "second"
				case "narrow":
					scopes = "account_info.read"
				case "initial":
					if strings.Join(oauthScopeSet(scopes), " ") != full {
						t.Error("first authorization did not request the default read scope")
					}
				case "same":
				default:
					t.Errorf("unexpected authorization code: %q", code)
				}
				fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"account_id":%q,"scope":%q,"token_type":"bearer","expires_in":3600}`,
					"sealed-access-"+account, "sealed-refresh-"+code, account, scopes)
			case "refresh_token":
				refreshes.Add(1)
				if loseRefresh.Swap(false) {
					return nil, io.ErrUnexpectedEOF
				}
				if rejectRefresh.Load() {
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"error":"invalid_grant","error_description":"sealed-refresh-secret"}`)
				} else {
					if f.Get("refresh_token") != "sealed-refresh-initial" {
						t.Error("refresh used the wrong grant")
					}
					io.WriteString(w, `{"access_token":"sealed-access-first","token_type":"bearer","expires_in":3600}`)
				}
			default:
				t.Error("unexpected token request")
			}
		case "api.dropboxapi.com/2/users/get_current_account":
			account := strings.TrimPrefix(auth, "Bearer sealed-access-")
			if account != "first" && account != "second" {
				t.Error("identity request used the wrong credential")
			}
			fmt.Fprintf(w, `{"account_id":%q,"email":%q}`, account, account+"@fixture.invalid")
		case "api.dropboxapi.com/2/files/get_metadata":
			if auth != "Bearer sealed-access-first" && auth != "Bearer sealed-access-second" {
				t.Error("root probe used the wrong credential")
			}
			io.WriteString(w, `{".tag":"folder","id":"id:folder","name":"Folder"}`)
		case "api.dropboxapi.com/2/files/list_folder":
			account := strings.TrimPrefix(auth, "Bearer sealed-access-")
			if account != "first" && account != "second" {
				t.Error("listing used the wrong credential")
			}
			fmt.Fprintf(w, `{"entries":[{".tag":"file","id":"id:file","name":%q,"size":1}],"cursor":"cursor","has_more":false}`, account+".txt")
		default:
			return nil, fmt.Errorf("unexpected Dropbox endpoint %s%s", r.URL.Host, r.URL.Path)
		}
		return w.Result(), nil
	})

	add := "dropbox\n\n\n\n/Folder\ny\n"
	invokeCLI(t, configPath, add, true, 0, "mount", "add", "/one")
	invokeCLI(t, configPath, add, true, 0, "mount", "add", "/two")
	if browsers.Load() != 1 || refreshes.Load() != 0 {
		t.Fatal("reusing an authorization opened the browser or refreshed a current token")
	}
	snapshot := func() configSnapshot {
		t.Helper()
		saved, err := loadConfig(configPath)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	saved := snapshot()
	id := saved.value.Mounts[0].Credential.Ref
	if saved.value.Mounts[1].Credential.Ref != id {
		t.Fatal("the second mount did not reuse the first grant")
	}
	expire := func(browser *browserAuthorization, id string) {
		t.Helper()
		g, err := browser.loadGrant(id)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := lockGrant(ctx, id, false)
		if err != nil {
			t.Fatal(err)
		}
		g.Expires = time.Now().Add(-time.Minute)
		if err := g.save(ctx, lease, true); err != nil {
			lease.lock.Close()
			t.Fatal(err)
		}
		if err := lease.lock.Close(); err != nil {
			t.Fatal(err)
		}
	}
	expire(&dropboxAuthorization, id)
	loseRefresh.Store(true)
	invokeCLI(t, configPath, "", false, 1, "ls", "/one", "--json")
	// Each invocation reconstructs its own runtime from the durable record.
	var wg sync.WaitGroup
	for _, at := range []string{"/one", "/two"} {
		wg.Go(func() {
			var out, diag bytes.Buffer
			if code := testApp.Run(ctx, []string{"--config", configPath, "ls", at, "--json"}, nil, &out, &diag, false); code != 0 ||
				!strings.Contains(out.String(), "first.txt") {
				t.Errorf("reopened %s: %s %s", at, &out, &diag)
			}
		})
	}
	wg.Wait()
	if refreshes.Load() != 2 {
		t.Fatalf("lost response and shared expiry caused %d refreshes", refreshes.Load())
	}
	g, err := dropboxAuthorization.loadGrant(id)
	if err != nil || time.Until(g.Expires) < time.Minute || g.Refresh != "sealed-refresh-initial" {
		t.Fatal("the new access token expiry was not saved with the reusable refresh token")
	}

	expire(&dropboxAuthorization, id)
	rejectRefresh.Store(true)
	got := invokeCLI(t, configPath, "", false, 1, "ls", "/one", "--json")
	if got.Error == nil || got.Error.Code != "interaction_required" || strings.Contains(got.Error.Message, "sealed-refresh") {
		t.Fatalf("unexpected refresh failure: %+v", got.Error)
	}
	for _, failure := range []struct{ mode, code string }{
		{"cancel", "cancelled"}, {"wrong", "credential_scope_conflict"}, {"narrow", "credential_scope_conflict"},
	} {
		mode.Store(failure.mode)
		var out, diag bytes.Buffer
		if code := testApp.Run(ctx, []string{"--config", configPath, "mount", "edit", "/one"}, strings.NewReader("reauthorize\n"), &out, &diag, true); code != 1 ||
			!strings.Contains(diag.String(), failure.code) || strings.Contains(diag.String(), "sealed-refresh") {
			t.Fatalf("%s changed or leaked the authorization: %s %s", failure.mode, &out, &diag)
		}
		g, err := dropboxAuthorization.loadGrant(id)
		if err != nil || g.Refresh != "sealed-refresh-initial" || snapshot().digest != saved.digest {
			t.Fatalf("%s changed local state", failure.mode)
		}
	}
	mode.Store("same")
	invokeCLI(t, configPath, "reauthorize\n", true, 0, "mount", "edit", "/one")
	g, err = dropboxAuthorization.loadGrant(id)
	if err != nil || g.Refresh != "sealed-refresh-same" || snapshot().digest != saved.digest {
		t.Fatal("reauthorization did not replace the existing grant alone")
	}
	mode.Store("switch")
	invokeCLI(t, configPath, "switch\n\ny\n", true, 0, "mount", "edit", "/one")
	next := snapshot()
	if next.value.Mounts[0].Credential.Ref == id || next.value.Mounts[1].Credential.Ref != id {
		t.Fatal("switch changed more than the selected mount")
	}
	for _, pair := range []struct{ at, file string }{{"/one", "second.txt"}, {"/two", "first.txt"}} {
		got := invokeCLI(t, configPath, "", false, 0, "ls", pair.at, "--json")
		if !bytes.Contains(got.Result, []byte(pair.file)) {
			t.Fatalf("wrong account after switch: %s", got.Result)
		}
	}
	const writeMount = `{"at":"/two","driver":"dropbox","options":{"root_path":"/Folder","allow_create":true}}`
	invokeCLI(t, configPath, writeMount, false, 0, "mount", "edit", "/two", "--from", "-", "--json")
	got = invokeCLI(t, configPath, "", false, 1, "ls", "/two", "--json")
	if got.Error == nil || got.Error.Code != "credential_scope_conflict" {
		t.Fatalf("write-enabled mount reused a read grant: %+v", got.Error)
	}
	mode.Store("same")
	invokeCLI(t, configPath, "reauthorize\n", true, 0, "mount", "edit", "/two")
	invokeCLI(t, configPath, "", false, 0, "ls", "/two", "--json")
	mode.Store("google")
	appPath := filepath.Join(home, "desktop-client.json")
	app := []byte(`{"installed":{"client_id":"fixture-google","client_secret":"sealed-google-secret","token_uri":"https://untrusted.invalid"}}`)
	if err := os.WriteFile(appPath, app, 0600); err != nil {
		t.Fatal(err)
	}
	invokeCLI(t, configPath, "googledrive\n\n\n"+appPath+"\nvault\ny\n", true, 0, "mount", "add", "/drive")
	googleSaved := snapshot()
	googleRef := googleSaved.value.Mounts[len(googleSaved.value.Mounts)-1].Credential.Ref
	if _, err := dropboxAuthorization.loadGrant(googleRef); err == nil {
		t.Fatal("Dropbox accepted a Google grant")
	}
	googleGrant, err := googleDriveAuthorization.loadGrant(googleRef)
	if err != nil || googleGrant.Account != "fixture-user" {
		t.Fatalf("saved Google account identity: %v", err)
	}
	if err := os.Remove(appPath); err != nil {
		t.Fatal(err)
	}
	expire(&googleDriveAuthorization, googleRef)
	got = invokeCLI(t, configPath, "", false, 0, "ls", "/drive", "--json")
	googleGrant, err = googleDriveAuthorization.loadGrant(googleRef)
	if err != nil || googleGrant.Access != "sealed-google-renewed" || !bytes.Contains(got.Result, []byte("drive.txt")) {
		t.Fatalf("Google grant did not renew from the protected record: %v", err)
	}
	expire(&googleDriveAuthorization, googleRef)
	rejectGoogleRefresh.Store(true)
	got = invokeCLI(t, configPath, "", false, 1, "ls", "/drive", "--json")
	if got.Error == nil || got.Error.Code != "interaction_required" {
		t.Fatalf("Google expiry did not direct the user to reauthorize: %+v", got.Error)
	}
	invokeCLI(t, configPath, "reauthorize\n", true, 0, "mount", "edit", "/drive")
	got = invokeCLI(t, configPath, "", false, 0, "ls", "/drive", "--json")
	if !bytes.Contains(got.Result, []byte("drive.txt")) || snapshot().digest != googleSaved.digest {
		t.Fatalf("Google reauthorization changed the mount or lost directory access: %s", got.Result)
	}
	if data, err := os.ReadFile(configPath); err != nil || bytes.Contains(data, []byte("sealed-")) ||
		bytes.Contains(data, []byte("@fixture.invalid")) {
		t.Fatal("configuration contains credentials or account labels")
	}
}
