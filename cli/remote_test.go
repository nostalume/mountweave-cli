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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/nostalume/mountweave"
)

func TestRemoteMountWorkflow(t *testing.T) {
	t.Run("WebDAV configuration and credential audience", func(t *testing.T) {
		var requests atomic.Int64
		fixture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			user, password, ok := r.BasicAuth()
			if !ok || user != "fixture-user" || password != "fixture-password" {
				http.Error(w, "fixture authentication required", http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/report.txt") {
				w.Header().Set("Content-Length", "6")
				_, _ = io.WriteString(w, "report")
				return
			}
			if r.Method != "PROPFIND" {
				t.Errorf("unexpected WebDAV method %s", r.Method)
				http.Error(w, "unexpected method", http.StatusBadRequest)
				return
			}
			entry := func(href, kind string) string {
				return fmt.Sprintf(`<d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype>%s</d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, href, kind)
			}
			w.WriteHeader(http.StatusMultiStatus)
			kind := "<d:collection/>"
			if strings.HasSuffix(r.URL.Path, "/report.txt") {
				kind = ""
			}
			fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:">`, entry(r.URL.Path, kind))
			if r.Header.Get("Depth") == "1" {
				fmt.Fprint(w, entry(r.URL.Path+"report.txt", ""))
			}
			fmt.Fprint(w, "</d:multistatus>")
		})
		dav := httptest.NewServer(fixture)
		defer dav.Close()
		home := t.TempDir()
		config := filepath.Join(home, "config.json")
		secret := filepath.Join(home, "dav.secret")
		if err := os.WriteFile(secret, []byte("fixture-password\r\n"), 0600); err != nil {
			t.Fatal(err)
		}
		record := func(at, root, username, ref string) string {
			return fmt.Sprintf(`{"at":%q,"driver":"webdav","options":{"root_url":%q,"username":%q}%s}`, at, root, username, ref)
		}
		original := `,"credential":{"kind":"file","ref":"dav.secret"}`
		invokeCLI(t, config, record("/dav", dav.URL+"/docs/", "fixture-user", original), false, 0, "mount", "add", "--from", "-", "--json")
		if requests.Load() != 1 {
			t.Fatalf("add performed %d requests, want one native root probe", requests.Load())
		}
		if got := invokeCLI(t, config, "", false, 0, "ls", "/dav", "--json"); !bytes.Contains(got.Result, []byte("report.txt")) {
			t.Fatal(string(got.Result))
		}
		before := requests.Load()
		changed := record("/dav", dav.URL+"/docs/", "other-user", original)
		if got := invokeCLI(t, config, changed, false, 1, "mount", "edit", "/dav", "--from", "-", "--json"); got.Error == nil || got.Error.Code != "credential_scope_conflict" {
			t.Fatalf("credential audience change: %+v", got.Error)
		}
		if requests.Load() != before {
			t.Fatal("audience conflict sent a request")
		}
		if err := os.Remove(secret); err != nil {
			t.Fatal(err)
		}
		invokeCLI(t, config, record("/moved", dav.URL+"/other/", "fixture-user", ""), false, 0, "mount", "edit", "/dav", "--from", "-", "--json")
		invokeCLI(t, config, "\n\n\n\ny\n", true, 0, "mount", "edit", "/moved")
		invokeCLI(t, config, "", false, 0, "mount", "list", "--json")
		if got := invokeCLI(t, config, "", false, 1, "stat", "/moved", "--json"); got.Error == nil || got.Error.Code != "credential_unavailable" || string(got.Result) != "null" {
			t.Fatalf("missing saved reference: %s, %+v", got.Result, got.Error)
		}
		if requests.Load() != before {
			t.Fatal("static edit, list or missing credential contacted WebDAV")
		}
		t.Setenv("MWEAVE_DAV_FIXTURE", "fixture-password")
		replacement := `,"credential":{"kind":"env","ref":"MWEAVE_DAV_FIXTURE"}`
		invokeCLI(t, config, record("/moved", dav.URL+"/other/", "fixture-user", replacement), false, 0, "mount", "edit", "/moved", "--from", "-", "--json")
		t.Setenv("MWEAVE_DAV_WRONG_FIXTURE", "fixture-rejected-password")
		badReference := strings.ReplaceAll(replacement, "MWEAVE_DAV_FIXTURE", "MWEAVE_DAV_WRONG_FIXTURE")
		invokeCLI(t, config, record("/moved", dav.URL+"/other/", "fixture-user", badReference), false, 1, "mount", "edit", "/moved", "--from", "-", "--json")
		if got := invokeCLI(t, config, "", false, 0, "stat", "/moved", "--json"); !bytes.Contains(got.Result, []byte(`"path":"/moved"`)) {
			t.Fatal(string(got.Result))
		}
		t.Setenv("MWEAVE_DAV_FIXTURE", "fixture-rejected-password")
		for _, cmd := range []string{"stat", "ls"} {
			if got := invokeCLI(t, config, "", false, 1, cmd, "/moved", "--json"); got.Error == nil || got.Error.Code != string(core.UpstreamAccessDenied) {
				t.Fatalf("%s remote rejection: %+v", cmd, got.Error)
			}
		}
		invokeCLI(t, config, "", false, 0, "mount", "remove", "/moved", "--json")
		if requests.Load() != before+6 {
			t.Fatal("expected one probe per replacement attempt, two calls for stat and one probe per denied operation")
		}

		oldTransport := http.DefaultTransport
		t.Cleanup(func() { http.DefaultTransport = oldTransport })
		http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
			location := r.URL.Host + strings.TrimSuffix(r.URL.Path, "report.txt")
			switch location {
			case "dav.jianguoyun.com/dav/", "webdav.123pan.cn/webdav/", "febbox.invalid/dav/", "openlist.invalid/dav/":
				if r.URL.Scheme != "https" {
					return nil, errors.New("WebDAV preset downgraded HTTPS")
				}
			default:
				return nil, errors.New("unexpected WebDAV preset address")
			}
			w := httptest.NewRecorder()
			fixture.ServeHTTP(w, r)
			res := w.Result()
			res.Request = r
			return res, nil
		})
		oldLaunch := launchOAuthBrowser
		t.Cleanup(func() { launchOAuthBrowser = oldLaunch })
		var opened string
		launchOAuthBrowser = func(_ context.Context, target string) error {
			if opened != "" {
				t.Fatal("duplicate browser launch")
			}
			opened = target
			if target == "https://www.febbox.com/console" {
				return errors.New("default browser unavailable")
			}
			return nil
		}
		for _, source := range []struct{ choice, path, url, enteredURL, ref, hint, driver, connection, website string }{
			{"坚果云", "/nuts", "https://dav.jianguoyun.com/dav/", "", "MWEAVE_NUTSTORE_FIXTURE", "", "nutstore", "", ""},
			{"123云盘", "/123Pan", "https://webdav.123pan.cn/webdav/", "", "MWEAVE_123_WEBDAV_FIXTURE", "Third-party mounting", "pan123", "webdav", "https://yun.123pan.cn/"},
			{"FebBox", "/FebBox", "https://febbox.invalid/dav/", "https://febbox.invalid/dav/", "MWEAVE_FEBBOX_WEBDAV_FIXTURE", "Settings > WebDAV", "febbox", "", "https://www.febbox.com/console"},
			{"webdav", "/export", "https://openlist.invalid/dav/", "https://openlist.invalid/dav/", "MWEAVE_OPENLIST_WEBDAV_FIXTURE", "", "webdav", "", ""},
		} {
			t.Setenv(source.ref, "fixture-password")
			input := source.choice + "\n"
			if source.connection != "" {
				input += source.connection + "\n"
			}
			input += source.enteredURL + "\nfixture-user\nenv:" + source.ref + "\ny\n"
			var output, diagnostics bytes.Buffer
			if code := testApp.Run(t.Context(), []string{"--config", config, "mount", "add", source.path},
				strings.NewReader(input), &output, &diagnostics, true); code != 0 {
				t.Fatalf("mount setup failed (%d): %s", code, diagnostics.String())
			}
			prompt := diagnostics.String()
			if opened != source.website || (source.driver == "febbox" && !strings.Contains(prompt, source.website)) {
				t.Fatalf("unexpected browser handoff for %s: %q, %s", source.driver, opened, prompt)
			}
			opened = ""
			if source.hint != "" && (!strings.Contains(prompt, source.hint) || !strings.Contains(prompt, "WebDAV password") ||
				strings.Index(prompt, source.hint) > strings.Index(prompt, "WebDAV URL")) {
				t.Fatalf("missing provider setup guidance before credential prompts: %s", prompt)
			}
			if source.hint != "" {
				assertSourceBody(t, config, source.path+"/report.txt", "report")
			}
			if got := invokeCLI(t, config, "", false, 0, "ls", source.path, "--json"); !bytes.Contains(got.Result, []byte("report.txt")) {
				t.Fatal(string(got.Result))
			}
			saved, err := loadConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			m := saved.value.Mounts[len(saved.value.Mounts)-1]
			var opts webDAVOptions
			if m.Driver != source.driver || m.Connection != source.connection || decodeObject(m.Options, &opts) != nil || opts.RootURL != source.url {
				t.Fatalf("unexpected saved WebDAV mount: %+v", m)
			}
		}
		download := filepath.Join(home, "from-openlist.txt")
		invokeCLI(t, config, "", false, 0, "get", "/export/report.txt", download, "--json")
		if data, err := os.ReadFile(download); err != nil || string(data) != "report" {
			t.Fatalf("unexpected downloaded contents: %q (%v)", data, err)
		}
	})

	t.Run("account folder configuration and isolated content", func(t *testing.T) {
		const (
			token123 = "123-fixture-token"
			token115 = "115-fixture-token"
			folderID = "9007199254740993"
		)
		var mediaReads int
		var issues atomic.Int32
		var wrongAccount, loseIssue, denyFolder atomic.Bool
		oldTransport := http.DefaultTransport
		t.Cleanup(func() { http.DefaultTransport = oldTransport })
		http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
			var body string
			switch r.URL.Host {
			case pan123ControlHost:
				if r.URL.Scheme != "https" || r.Header.Get("Platform") != "open_platform" {
					t.Error("invalid 123 API recipient or platform")
				}
				if r.URL.Path == "/api/v1/access_token" || r.URL.Path == "/api/v1/user/info" {
					w := httptest.NewRecorder()
					if r.URL.Path == "/api/v1/access_token" {
						defer r.Body.Close()
						var app map[string]string
						if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" ||
							json.NewDecoder(r.Body).Decode(&app) != nil || app["clientID"] != "fixture-app" ||
							(app["clientSecret"] != "fixture-app-secret" && app["clientSecret"] != "fixture-new-secret") {
							t.Error("token request used the wrong application")
						}
						n := issues.Add(1)
						if loseIssue.Swap(false) {
							return nil, io.ErrUnexpectedEOF
						}
						fmt.Fprintf(w, `{"code":0,"data":{"accessToken":%q,"expiredAt":%q}}`, fmt.Sprintf("managed-%d", n), time.Now().Add(time.Hour).Format(time.RFC3339))
					} else {
						if r.Method != http.MethodGet || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer managed-") {
							t.Error("user info did not use the issued token")
						}
						uid := int64(9007199254740993)
						if wrongAccount.Load() {
							uid++
						}
						fmt.Fprintf(w, `{"code":0,"data":{"uid":%d,"nickname":"Fixture"}}`, uid)
					}
					return w.Result(), nil
				}
				authorization := r.Header.Get("Authorization")
				if authorization != "Bearer "+token123 && !strings.HasPrefix(authorization, "Bearer managed-") {
					t.Error("control request lacks the expected authentication")
				}
				var data string
				switch r.URL.Path {
				case "/api/v1/file/detail":
					if r.URL.Query().Get("fileID") != "9007199254740993" {
						t.Error("native folder ID was changed")
					}
					data = `{"fileID":9007199254740993,"parentFileID":0,"filename":"Archive","type":1,"size":0,"trashed":0}`
				case "/api/v2/file/list":
					if denyFolder.Load() {
						w := httptest.NewRecorder()
						w.WriteHeader(http.StatusForbidden)
						io.WriteString(w, `{"code":1,"data":null}`)
						return w.Result(), nil
					}
					if r.URL.Query().Get("parentFileId") == "0" {
						data = `{"lastFileId":-1,"fileList":[{"fileId":9007199254740993,"parentFileId":0,"filename":"Archive","type":1,"size":0,"trashed":0}]}`
						break
					}
					if r.URL.Query().Get("parentFileId") != "9007199254740993" {
						t.Error("list does not use the bound folder ID")
					}
					data = `{"lastFileId":-1,"fileList":[{"fileId":4,"parentFileId":9007199254740993,"filename":"file.txt","type":0,"size":6,"trashed":0}]}`
				case "/api/v1/file/download_info":
					data = `{"downloadUrl":"https://content.fixture/file?cap=private"}`
				default:
					return nil, errors.New("unexpected fixture API call")
				}
				body = `{"code":0,"data":` + data + "}"
			case pan115ControlHost:
				if r.Header.Get("Authorization") != "Bearer "+token115 || r.Header.Get("Platform") != "" {
					t.Error("wrong authentication on 115 control request")
				}
				switch r.URL.Path {
				case "/open/folder/get_info":
					if r.URL.Query().Get("file_id") != folderID {
						t.Error("native folder ID was changed")
					}
					body = `{"state":true,"data":{"file_id":9007199254740993,"file_name":"Archive","file_category":"0"}}`
				case "/open/ufile/files":
					parent := r.URL.Query().Get("cid")
					var item string
					switch parent {
					case "0":
						item = `{"fid":9007199254740993,"pid":0,"fn":"Archive","fc":"0"}`
					case folderID:
						item = `{"fid":4,"pid":9007199254740993,"fn":"file.txt","fc":"1","fs":6,"pc":"fixture-pick"}`
					default:
						return nil, errors.New("unexpected 115 directory")
					}
					body = fmt.Sprintf(`{"state":true,"cid":%s,"count":1,"offset":0,"limit":1,"data":[%s]}`, parent, item)
				case "/open/ufile/downurl":
					if err := r.ParseMultipartForm(1024); err != nil || r.FormValue("pick_code") != "fixture-pick" {
						t.Error("download did not use the bound native file reference")
					}
					body = `{"state":true,"data":{"4":{"file_size":6,"url":{"url":"https://content.fixture/file?cap=private"}}}}`
				default:
					return nil, errors.New("unexpected 115 fixture API call")
				}
			case "content.fixture":
				mediaReads++
				if r.URL.RawQuery != "cap=private" || r.Header.Get("Authorization") != "" || r.Header.Get("Platform") != "" || r.Header.Get("Cookie") != "" {
					t.Error("content request changed its capability or forwarded account credentials")
				}
				body = "sealed"
			default:
				return nil, errors.New("unexpected fixture host")
			}
			return &http.Response{Request: r, StatusCode: 200, Header: make(http.Header), ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		t.Setenv("MWEAVE_123_FIXTURE", token123)
		t.Setenv("MWEAVE_115_FIXTURE", token115)
		path := filepath.Join(t.TempDir(), "config.json")
		record := `{"at":"/cloud","driver":"pan123","options":{"root_id":"9007199254740993"},"credential":{"kind":"env","ref":"MWEAVE_123_FIXTURE"}}`
		invokeCLI(t, path, record, false, 0, "mount", "add", "--from", "-", "--json")
		pathRecord := strings.Replace(record, `"root_id":"9007199254740993"`, `"root_path":"/Archive"`, 1)
		invokeCLI(t, path, pathRecord, false, 0, "mount", "edit", "/cloud", "--from", "-", "--json")
		record115 := `{"at":"/by-id","driver":"pan115","options":{"root_id":"9007199254740993"},"credential":{"kind":"env","ref":"MWEAVE_115_FIXTURE"}}`
		wrongRef := strings.Replace(record115, "MWEAVE_115_FIXTURE", "MWEAVE_123_FIXTURE", 1)
		if got := invokeCLI(t, path, wrongRef, false, 1, "mount", "add", "--from", "-", "--json"); got.Error == nil || got.Error.Code != "credential_scope_conflict" {
			t.Fatalf("reusing a reference for another provider: %+v", got.Error)
		}
		choice := ""
		if managedStoreSupported {
			choice = "reference\n"
		}
		invokeCLI(t, path, "115\n"+choice+"/Archive\nenv:MWEAVE_115_FIXTURE\ny\n", true, 0, "mount", "add", "/by-path")
		invokeCLI(t, path, record115, false, 0, "mount", "add", "--from", "-", "--json")

		for _, at := range []string{"/cloud", "/by-path", "/by-id"} {
			if got := invokeCLI(t, path, "", false, 0, "ls", at, "--json"); !bytes.Contains(got.Result, []byte("file.txt")) {
				t.Fatalf("listing %s: %s", at, got.Result)
			}
			assertSourceBody(t, path, at+"/file.txt", "sealed")
		}
		if mediaReads != 3 {
			t.Fatalf("got %d content reads for three files", mediaReads)
		}
		if !managedStoreSupported {
			return
		}
		home := t.TempDir()
		t.Setenv("APPDATA", home)
		t.Setenv("XDG_CONFIG_HOME", home)
		t.Setenv("MWEAVE_123_APP_FIXTURE", "fixture-app-secret")
		t.Setenv("MWEAVE_123_NEW_FIXTURE", "fixture-new-secret")
		invokeCLI(t, path, "pan123\nopen\n\n\nfixture-app\nenv:MWEAVE_123_APP_FIXTURE\n/Archive\ny\n", true, 0, "mount", "add", "/app")
		invokeCLI(t, path, "pan123\nopen\n\n\n/Archive\ny\n", true, 0, "mount", "add", "/app-shared")
		snapshot := func() configSnapshot {
			t.Helper()
			saved, err := loadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			return saved
		}
		saved := snapshot()
		ref := saved.value.Mounts[len(saved.value.Mounts)-1].Credential.Ref
		if issues.Load() != 1 || saved.value.Mounts[len(saved.value.Mounts)-2].Credential.Ref != ref {
			t.Fatal("shared mount did not reuse the cached application token")
		}
		assertSourceBody(t, path, "/app/file.txt", "sealed")
		g, err := pan123Authorization.loadGrant(ref)
		if err != nil || g.Account != folderID || g.ClientSecret != "" || g.Refresh != "" {
			t.Fatalf("unexpected app grant: %v", err)
		}
		replace := "reauthorize\n\nenv:MWEAVE_123_NEW_FIXTURE\n"
		for _, flag := range []*atomic.Bool{&wrongAccount, &denyFolder} {
			flag.Store(true)
			invokeCLI(t, path, replace, true, 1, "mount", "edit", "/app")
			flag.Store(false)
			current, err := pan123Authorization.loadGrant(ref)
			if err != nil || current.SecretRef != g.SecretRef || current.Access != g.Access || snapshot().digest != saved.digest {
				t.Fatal("failed replacement changed the grant or mount")
			}
		}
		invokeCLI(t, path, replace, true, 0, "mount", "edit", "/app")
		g, err = pan123Authorization.loadGrant(ref)
		if err != nil || g.SecretRef.Ref != "MWEAVE_123_NEW_FIXTURE" || snapshot().digest != saved.digest {
			t.Fatal("successful same-account update did not replace the secret reference alone")
		}
		lease, err := lockGrant(t.Context(), ref, false)
		if err != nil {
			t.Fatal(err)
		}
		g.Expires = time.Now().Add(-time.Minute)
		if err := errors.Join(g.save(t.Context(), lease, true), lease.lock.Close()); err != nil {
			t.Fatal(err)
		}
		wrongAccount.Store(true)
		got := invokeCLI(t, path, "", false, 1, "ls", "/app", "--json")
		wrongAccount.Store(false)
		current, err := pan123Authorization.loadGrant(ref)
		if got.Error == nil || got.Error.Code != "credential_scope_conflict" || err != nil || current.Access != g.Access {
			t.Fatal("automatic exchange accepted or saved a token for another account")
		}
		before := issues.Load()
		loseIssue.Store(true)
		invokeCLI(t, path, "", false, 1, "ls", "/app", "--json")
		var wg sync.WaitGroup
		for _, at := range []string{"/app", "/app-shared"} {
			wg.Go(func() {
				var out, diag bytes.Buffer
				if code := testApp.Run(t.Context(), []string{"--config", path, "ls", at, "--json"}, nil, &out, &diag, false); code != 0 ||
					!strings.Contains(out.String(), "file.txt") {
					t.Errorf("failed to reopen %s: %s %s", at, &out, &diag)
				}
			})
		}
		wg.Wait()
		if issues.Load() != before+2 {
			t.Fatalf("response loss and concurrent reopen caused %d issues", issues.Load()-before)
		}
		if data, err := os.ReadFile(path); err != nil || bytes.Contains(data, []byte("managed-")) ||
			bytes.Contains(data, []byte("fixture-app")) || bytes.Contains(data, []byte("MWEAVE_123_NEW_FIXTURE")) {
			t.Fatal("mount configuration contains application credentials")
		}
	})
	t.Run("Baidu authorized folder and native transfers", func(t *testing.T) {
		const (
			root      = "/apps/Fixture Space/Archive"
			name      = "note%+.txt"
			token     = "baidu-fixture-token"
			signature = "sign=a%2Fb+%2Bc"
		)
		file := root + "/" + name
		item := fmt.Sprintf(`{"errno":0,"fs_id":4,"path":%q,"server_filename":%q,"isdir":0,"size":6}`, file, name)
		t.Setenv("MWEAVE_BAIDU_FIXTURE", token)
		oldTransport := http.DefaultTransport
		t.Cleanup(func() { http.DefaultTransport = oldTransport })
		var requests, downloads int
		var uploaded string
		http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
			requests++
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
				t.Error("Baidu received an account header from the host")
			}
			w := httptest.NewRecorder()
			q := r.URL.Query()
			switch r.URL.Host {
			case "pan.baidu.com":
				switch {
				case r.Method == "POST" && r.URL.RawQuery == "":
					defer r.Body.Close()
					var targets []string
					if err := r.ParseForm(); err != nil || r.PostForm.Get("method") != "filemetas" ||
						r.PostForm.Get("access_token") != token || json.Unmarshal([]byte(r.PostForm.Get("target")), &targets) != nil || len(targets) != 1 {
						return nil, errors.New("unexpected Baidu metadata request")
					}
					switch targets[0] {
					case root:
						fmt.Fprintf(w, `{"errno":0,"info":[{"errno":0,"fs_id":3,"path":%q,"server_filename":"Archive","isdir":1,"size":0}]}`, root)
					case file:
						meta := item
						if r.PostForm.Get("dlink") == "1" {
							meta = strings.TrimSuffix(item, "}") + `,"dlink":"https://download.fixture/note?` + signature + `&access_token=stale"}`
						}
						fmt.Fprintf(w, `{"errno":0,"info":[%s]}`, meta)
					default:
						return nil, errors.New("metadata escaped the authorized folder")
					}
				case r.Method == "GET" && q.Get("method") == "list" && q.Get("dir") == root && q.Get("access_token") == token:
					entries := ""
					if q.Get("start") == "0" {
						entries = item
					} else if q.Get("start") != "1000" {
						return nil, errors.New("unexpected Baidu page")
					}
					fmt.Fprintf(w, `{"errno":0,"list":[%s]}`, entries)
				default:
					return nil, errors.New("unexpected Baidu control request")
				}
			case "download.fixture":
				if r.URL.Scheme != "https" || r.URL.RawQuery != signature+"&access_token="+token || r.Header.Get("Range") != "bytes=0-" {
					t.Error("authorized download changed the signed URL or token")
				}
				w.Header().Set("Location", "https://cdn.fixture/note?cap=private")
				w.WriteHeader(http.StatusFound)
			case "cdn.fixture":
				downloads++
				if r.URL.RawQuery != "cap=private" || r.Header.Get("Range") != "bytes=0-" {
					t.Error("delegated download changed the capability or forwarded the token")
				}
				w.Header().Set("Content-Range", "bytes 0-5/6")
				w.Header().Set("Content-Length", "6")
				w.WriteHeader(http.StatusPartialContent)
				io.WriteString(w, "sealed")
			case "d.pcs.baidu.com":
				if r.Method != "GET" || r.URL.RawQuery != "method=locateupload" {
					t.Error("upload discovery carried account credentials")
				}
				io.WriteString(w, `{"error_code":0,"server":["upload.fixture"]}`)
			case "upload.fixture":
				defer r.Body.Close()
				if r.Method != "POST" || q.Get("method") != "upload" || q.Get("ondup") != "fail" ||
					q.Get("access_token") != token || q.Get("path") != root+"/new.txt" {
					return nil, errors.New("unexpected Baidu exclusive upload request")
				}
				parts, err := r.MultipartReader()
				if err != nil {
					return nil, err
				}
				part, err := parts.NextPart()
				if err != nil {
					return nil, err
				}
				data, err := io.ReadAll(part)
				if err != nil {
					return nil, err
				}
				uploaded = string(data)
				fmt.Fprintf(w, `{"fs_id":0,"path":%q,"size":%d}`, q.Get("path"), len(data))
			default:
				return nil, errors.New("request outside the Baidu fixture")
			}
			res := w.Result()
			res.Request = r
			return res, nil
		})

		config := filepath.Join(t.TempDir(), "config.json")
		record := `{"at":"/baidu","driver":"baidupan","options":%s,"credential":{"kind":"env","ref":"MWEAVE_BAIDU_FIXTURE"}}`
		if got := invokeCLI(t, config, fmt.Sprintf(record, "{}"), false, 1, "mount", "add", "--from", "-", "--json"); got.Error == nil || got.Error.Code != "invalid_config" || requests != 0 {
			t.Fatalf("missing authorized root: %+v, %d requests", got.Error, requests)
		}
		options := fmt.Sprintf(`{"root_path":%q}`, file)
		if got := invokeCLI(t, config, fmt.Sprintf(record, options), false, 1, "mount", "add", "--from", "-", "--json"); got.Error == nil || got.Error.Code != string(core.NotDirectory) {
			t.Fatalf("file as authorized root: %+v", got.Error)
		}
		before := requests
		invokeCLI(t, config, "baidupan\n"+root+"\nenv:MWEAVE_BAIDU_FIXTURE\ny\n", true, 0, "mount", "add", "/baidu")
		if requests != before+1 {
			t.Fatal("Baidu setup should use only the native root check")
		}
		if got := invokeCLI(t, config, "", false, 0, "ls", "/baidu", "--json"); !bytes.Contains(got.Result, []byte(name)) {
			t.Fatal(string(got.Result))
		}
		assertSourceBody(t, config, "/baidu/"+name, "sealed")
		if downloads != 1 {
			t.Fatalf("expected one delegated body read, got %d", downloads)
		}
		if got := invokeCLI(t, config, "sealed", false, 0, "put", "-", "/baidu/new.txt", "--size", "6", "--json"); uploaded != "sealed" || !bytes.Contains(got.Result, []byte(`"effect":"applied"`)) {
			t.Fatalf("upload body=%q, receipt=%s", uploaded, got.Result)
		}
	})

	t.Run("Graph drive and folder use native settings", func(t *testing.T) {
		const (
			token  = "graph-fixture-token"
			china  = "microsoftgraph.chinacloudapi.cn"
			file   = `{"id":"note","name":"note.txt","file":{},"size":6,"parentReference":{"id":"vault","driveId":"drive"}}`
			folder = `{"id":"vault","name":"Archive","folder":{},"parentReference":{"id":"root","driveId":"drive"}}`
			tenant = "00001111-aaaa-4222-bbbb-3333cccc4444"
			appID  = "11112222-aaaa-4333-bbbb-4444cccc5555"
			login  = "/" + tenant + "/oauth2/v2.0/"
		)
		t.Setenv("MWEAVE_GRAPH_FIXTURE", token)
		t.Setenv("MWEAVE_GRAPH_CHINA_FIXTURE", "graph-china-fixture")
		oldTransport := http.DefaultTransport
		t.Cleanup(func() { http.DefaultTransport = oldTransport })
		var calls, downloads, polls, renewals int
		var mode string
		var issuedAt, lastPoll time.Time
		var cancelWait context.CancelFunc
		http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			w := httptest.NewRecorder()
			host := strings.ToLower(r.URL.Host)
			switch host {
			case "login.microsoftonline.com":
				defer r.Body.Close()
				if err := r.ParseForm(); err != nil {
					return nil, err
				}
				form := r.PostForm
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" || form.Get("client_id") != appID ||
					form.Has("client_secret") || r.URL.Scheme != "https" {
					t.Error("wrong Microsoft Entra authorization request")
				}
				switch r.URL.Path {
				case login + "devicecode":
					polls = 0
					issuedAt = time.Now()
					if !strings.Contains(form.Get("scope"), "offline_access") || !strings.Contains(form.Get("scope"), "Files.Read") {
						t.Error("wrong device authorization scope")
					}
					expiry := 30
					if mode == "expired" {
						expiry = 1
					}
					fmt.Fprintf(w, `{"device_code":"private-device","user_code":"USER-CODE","verification_uri":"https://www.microsoft.com/devicelogin","expires_in":%d,"interval":1}`, expiry)
				case login + "token":
					switch form.Get("grant_type") {
					case graphDeviceGrant:
						polls++
						if polls == 1 {
							lastPoll = issuedAt
						}
						interval := time.Second
						if mode == "first" && polls == 3 {
							interval = 6 * time.Second
						}
						if form.Get("device_code") != "private-device" || time.Since(lastPoll) < interval {
							t.Error("polled too soon or with the wrong code")
						}
						lastPoll = time.Now()
						if mode == "cancel" {
							cancelWait()
							w.WriteHeader(http.StatusBadRequest)
							io.WriteString(w, `{"error":"authorization_pending"}`)
							break
						}
						if mode == "declined" || (mode == "first" && polls <= 2) {
							w.WriteHeader(http.StatusBadRequest)
							code := "authorization_pending"
							if mode == "declined" {
								code = "authorization_declined"
							} else if polls == 2 {
								code = "slow_down"
							}
							fmt.Fprintf(w, `{"error":%q,"error_description":"private-device"}`, code)
							break
						}
						fmt.Fprint(w, `{"access_token":"sealed-graph-access","refresh_token":"sealed-refresh-1","scope":"Files.Read User.Read","expires_in":3600,"token_type":"Bearer"}`)
					case "refresh_token":
						renewals++
						if form.Get("refresh_token") != "sealed-refresh-1" {
							t.Error("wrong refresh credential")
						}
						fmt.Fprint(w, `{"access_token":"sealed-graph-renewed","refresh_token":"sealed-refresh-2","scope":"Files.Read User.Read","expires_in":3600,"token_type":"Bearer"}`)
					default:
						return nil, errors.New("unexpected Graph grant type")
					}
				default:
					return nil, errors.New("unexpected Graph authorization tenant")
				}
			case "graph.microsoft.com", china:
				want := token
				if host == china {
					want = "graph-china-fixture"
				}
				auth := r.Header.Get("Authorization")
				managed := host == "graph.microsoft.com" && (auth == "Bearer sealed-graph-access" || auth == "Bearer sealed-graph-renewed")
				if r.Method != "GET" || (!managed && auth != "Bearer "+want) {
					t.Error("Graph request used the wrong authorization")
				}
				switch r.URL.Path {
				case "/v1.0/me":
					if !managed || r.URL.Query().Get("$select") != "id,displayName" {
						t.Error("Graph identity request did not use the issued token")
					}
					id := "fixture-user"
					if mode == "wrong" {
						id = "other-user"
					}
					fmt.Fprintf(w, `{"id":%q,"displayName":"Fixture"}`, id)
				case "/v1.0/drives/drive/root":
					io.WriteString(w, `{"id":"root","name":"Drive","folder":{}}`)
				case "/v1.0/drives/drive/items/root:/Archive", "/v1.0/drives/drive/items/vault":
					io.WriteString(w, folder)
				case "/v1.0/drives/drive/items/vault/children":
					if r.URL.Query().Get("page") == "" {
						fmt.Fprintf(w, `{"value":[],"@odata.nextLink":%q}`, "https://"+strings.ToUpper(host)+r.URL.Path+"?page=next")
					} else {
						fmt.Fprintf(w, `{"value":[%s]}`, file)
					}
				case "/v1.0/drives/drive/items/vault:/note.txt":
					io.WriteString(w, file)
				case "/v1.0/drives/drive/items/note/content":
					w.Header().Set("Location", "https://graph-content.fixture/note?cap=private")
					w.WriteHeader(http.StatusFound)
				default:
					return nil, errors.New("unexpected Graph file route")
				}
			case "graph-content.fixture":
				downloads++
				if r.URL.RawQuery != "cap=private" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("Graph content request changed its capability or received account credentials")
				}
				w.Header().Set("Content-Length", "6")
				io.WriteString(w, "sealed")
			default:
				return nil, errors.New("request outside the Graph fixture")
			}
			res := w.Result()
			res.Request = r
			return res, nil
		})

		config := filepath.Join(t.TempDir(), "config.json")
		const record = `{"at":%q,"driver":"graphdrive","options":%s,"credential":{"kind":"env","ref":%q}}`
		private := `{"drive_id":"drive","root_path":"/","share_url":"https://fixture.invalid/secret"}`
		if got := invokeCLI(t, config, fmt.Sprintf(record, "/graph", private, "MWEAVE_GRAPH_FIXTURE"), false, 1, "mount", "add", "--from", "-", "--json"); got.Error == nil || got.Error.Code != "invalid_config" || calls != 0 {
			t.Fatalf("runtime-only Graph setting was admitted: %+v, %d requests", got.Error, calls)
		}
		choice := ""
		if managedStoreSupported {
			choice = "reference\n"
		}
		invokeCLI(t, config, "graphdrive\n"+choice+"drive\n/Archive\nenv:MWEAVE_GRAPH_FIXTURE\ny\n", true, 0, "mount", "add", "/graph")
		before := calls
		chinaOptions := `{"drive_id":"drive","root_id":"vault","endpoint":"https://microsoftgraph.chinacloudapi.cn"}`
		if got := invokeCLI(t, config, fmt.Sprintf(record, "/graph", chinaOptions, "MWEAVE_GRAPH_FIXTURE"), false, 1, "mount", "edit", "/graph", "--from", "-", "--json"); got.Error == nil || got.Error.Code != "credential_scope_conflict" || calls != before {
			t.Fatalf("Graph credential crossed API origins: %+v, %d requests", got.Error, calls-before)
		}
		invokeCLI(t, config, fmt.Sprintf(record, "/china", chinaOptions, "MWEAVE_GRAPH_CHINA_FIXTURE"), false, 0, "mount", "add", "--from", "-")
		for _, at := range []string{"/graph", "/china"} {
			if got := invokeCLI(t, config, "", false, 0, "ls", at, "--json"); !bytes.Contains(got.Result, []byte("note.txt")) {
				t.Fatalf("listing %s: %s", at, got.Result)
			}
			assertSourceBody(t, config, at+"/note.txt", "sealed")
		}
		if downloads != 2 {
			t.Fatalf("expected one body request per download, got %d", downloads)
		}
		if !managedStoreSupported {
			return
		}
		home := t.TempDir()
		t.Setenv("APPDATA", home)
		t.Setenv("XDG_CONFIG_HOME", home)
		config = filepath.Join(home, "config.json")
		synctest.Test(t, func(t *testing.T) {
			mode = "first"
			add := "graphdrive\n\n\n" + appID + "\n" + tenant + "\ndrive\n/Archive\ny\n"
			var out, diag bytes.Buffer
			if code := testApp.Run(t.Context(), []string{"--config", config, "mount", "add", "/device", "--timeout", "15s"}, strings.NewReader(add), &out, &diag, true); code != 0 || polls != 3 {
				t.Fatalf("expected pending, slowdown and confirmation, got %d polls: %s %s", polls, &out, &diag)
			}
			saved, err := loadConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			id := saved.value.Mounts[0].Credential.Ref
			g, err := graphAuthorization.loadGrant(id)
			if err != nil || g.Account != tenant+"/fixture-user" || g.Refresh != "sealed-refresh-1" {
				t.Fatalf("Graph grant did not bind the tenant account: %v", err)
			}
			assertSourceBody(t, config, "/device/note.txt", "sealed")
			before = calls
			wrongOrigin := fmt.Sprintf(`{"at":"/elsewhere","driver":"graphdrive","options":%s,"credential":{"kind":"managed","ref":%q}}`, chinaOptions, id)
			invokeCLI(t, config, wrongOrigin, false, 1, "mount", "add", "--from", "-", "--json")
			if calls != before {
				t.Fatal("managed credential reached the custom Graph origin")
			}

			lease, err := lockGrant(t.Context(), id, false)
			if err != nil {
				t.Fatal(err)
			}
			g.Expires = time.Now().Add(-time.Second)
			if err := errors.Join(g.save(t.Context(), lease, true), lease.lock.Close()); err != nil {
				t.Fatal(err)
			}
			invokeCLI(t, config, "", false, 0, "ls", "/device", "--json")
			g, err = graphAuthorization.loadGrant(id)
			if err != nil || g.Access != "sealed-graph-renewed" || g.Refresh != "sealed-refresh-2" || renewals != 1 {
				t.Fatalf("renewed pair was not committed: %v", err)
			}
			for _, tc := range []struct {
				mode, code string
				polls      int
			}{{"cancel", "cancelled", 1}, {"declined", "cancelled", 1}, {"expired", "interaction_required", 0}, {"wrong", "credential_scope_conflict", 1}} {
				mode = tc.mode
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				ctx, interrupt := context.WithCancel(ctx)
				cancelWait = interrupt
				var out, diag bytes.Buffer
				exit := testApp.Run(ctx, []string{"--config", config, "mount", "edit", "/device"}, strings.NewReader("reauthorize\n"), &out, &diag, true)
				interrupt()
				cancel()
				if exit != 1 || !strings.Contains(diag.String(), tc.code) || polls != tc.polls || strings.Contains(diag.String(), "private-device") ||
					strings.Contains(diag.String(), "sealed-graph") || !strings.Contains(diag.String(), "USER-CODE") {
					t.Fatalf("%s: polls=%d, exit=%d, diagnostics=%s", tc.mode, polls, exit, &diag)
				}
				current, err := graphAuthorization.loadGrant(id)
				if err != nil || current.Refresh != g.Refresh || current.Account != g.Account {
					t.Fatalf("%s changed the existing grant", tc.mode)
				}
			}
			mode = "recover"
			invokeCLI(t, config, "reauthorize\n", true, 0, "mount", "edit", "/device")
			current, err := graphAuthorization.loadGrant(id)
			after, configErr := loadConfig(config)
			if err != nil || configErr != nil || after.digest != saved.digest || current.Refresh != "sealed-refresh-1" {
				t.Fatal("same-account reauthorization changed the mount or failed to save")
			}
			data, err := os.ReadFile(config)
			if err != nil || bytes.Contains(data, []byte("sealed-")) || bytes.Contains(data, []byte(appID)) || bytes.Contains(data, []byte(tenant)) {
				t.Fatal("Graph mount configuration exposed application credentials")
			}
		})
	})

}

type sealedTransport func(*http.Request) (*http.Response, error)

func (f sealedTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
