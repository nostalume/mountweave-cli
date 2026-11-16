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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	core "github.com/nostalume/mountweave"
)

func TestWebsiteReadLanZou(t *testing.T) {
	if !managedStoreSupported {
		t.Skip("managed credentials unavailable")
	}
	home := t.TempDir()
	t.Setenv("APPDATA", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	configPath := filepath.Join(home, "candidate.json")
	const originalBody = "original"
	oldOpen, oldSignIn := openLanZouWebsite, signInWebsite
	t.Cleanup(func() { openLanZouWebsite, signInWebsite = oldOpen, oldSignIn })
	oldHTTP := storageHTTPClient
	t.Cleanup(func() { storageHTTPClient = oldHTTP })
	wrongFile := false
	storageHTTPClient = &http.Client{Transport: sealedTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			return nil, errors.New("private session headers reached the public download service")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Host == "sample.lanzoub.com" && r.URL.Path == "/file-key":
			id := 2
			if wrongFile {
				id = 3
			}
			fmt.Fprintf(w, `function down_p(){ var isngis = 'website_fixture_signature'; url : 'https://apifile.woozooo.com/ajaxfile.php?file=%d' }`, id)
		case r.Method == http.MethodPost && r.URL.String() == "https://apifile.woozooo.com/ajaxfile.php?file=2":
			if err := r.ParseForm(); err != nil || r.PostForm.Get("p") != "fixture" || r.PostForm.Get("action") != "downprocess" {
				return nil, errors.New("provider form did not preserve the download receipt")
			}
			_, _ = io.WriteString(w, `{"zt":1,"dom":"https://fixture.lanrar.com","url":"body-token","inf":"b.txt"}`)
		default:
			return nil, fmt.Errorf("unexpected download request")
		}
		return w.Result(), nil
	})}
	subject := "123"
	var signInProfile string
	var conflictingProfile string
	const concurrentConfig = `{"version":1,"mounts":[]}`
	changeConfigOnSignIn := true
	signedOut := false
	launches, releases, continuations := 0, 0, 0
	signInWebsite = func(_ context.Context, _, profile, page string, ask askField, _ io.Writer) error {
		if page != lanZouLogin {
			return errors.New("unexpected sign-in entry")
		}
		signInProfile = profile
		if err := confirm(ask, "Continue after signing in and closing the window?"); err != nil {
			return err
		}
		if changeConfigOnSignIn {
			conflictingProfile = profile
			return os.WriteFile(configPath, []byte(concurrentConfig), 0600)
		}
		return nil
	}
	openLanZouWebsite = func(_ context.Context, _ string, _ []chromedp.ExecAllocatorOption, profile string) (lanZouCall, lanZouBody, func() error, error) {
		if signInProfile != "" && profile != signInProfile {
			return nil, nil, nil, errors.New("verification did not reopen the candidate profile")
		}
		signInProfile = ""
		if err := privateWebsiteDirectory(profile, false); err != nil {
			return nil, nil, nil, err
		}
		launches++
		return func(_ context.Context, script string, args json.RawMessage) (json.RawMessage, error) {
				if script == lanZouShareInfo && string(args) == `"2"` {
					return json.RawMessage(`{"code":"1","origin":"https://sample.lanzoub.com","id":"file-key","password":"fixture"}`), nil
				}
				if script != lanZouDirectoryRequest {
					return nil, errors.New("unexpected website operation")
				}
				var req lanZouPageRequest
				if err := json.Unmarshal(args, &req); err != nil {
					return nil, err
				}
				if signedOut {
					return json.RawMessage(`{"code":9}`), nil
				}
				var items string
				switch {
				case req.Task == 47 && req.Folder == "-1":
					items = `[{"id":"42","name":"Archive"}]`
				case req.Task == 47 && req.Folder == "42":
					items = `[]`
				case req.Task == 5 && req.Folder == "42" && req.Page == 1:
					items = `[{"id":"9007199254740993","name":"a &amp; b.txt"}]`
				case req.Task == 5 && req.Folder == "42" && req.Page == 2:
					continuations++
					items = `[{"id":"2","name":"b.txt"}]`
				case req.Task == 5 && req.Page > 0 && (req.Folder == "-1" || req.Folder == "42" && req.Page == 3):
					items = `[]`
				default:
					return nil, fmt.Errorf("unexpected directory request: %+v", req)
				}
				return json.RawMessage(fmt.Sprintf(`{"code":"1","account":%q,"entries":%s}`, subject, items)), nil
			}, func(_ context.Context, target *url.URL, rangeRequest *core.Range) (core.Opened, error) {
				if target.String() != "https://fixture.lanrar.com/file/body-token" {
					return core.Opened{}, errors.New("unexpected native body request")
				}
				status, payload := int64(http.StatusOK), originalBody
				headers := make(http.Header)
				if rangeRequest != nil {
					if rangeRequest.Start != 1 || rangeRequest.End != 6 || rangeRequest.Suffix {
						return core.Opened{}, errors.New("nonzero range was not forwarded")
					}
					status, payload = http.StatusPartialContent, "rigina"
					headers.Set("Content-Range", "bytes 1-6/8")
				}
				headers.Set("Content-Length", fmt.Sprint(len(payload)))
				opened, err := lanZouStreamMetadata(status, headers, rangeRequest)
				if err == nil {
					opened.Body = io.NopCloser(strings.NewReader(payload))
				}
				return opened, err
			}, func() error { releases++; return nil }, nil
	}
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	run := func(input string, args ...string) (int, string) {
		var output, diagnostics bytes.Buffer
		code := app.Run(t.Context(), append([]string{"--config", configPath}, args...), strings.NewReader(input), &output, &diagnostics, true)
		return code, output.String() + diagnostics.String()
	}
	const signInInput = "蓝奏云\ny\nArchive\n/Archive\ny\n"
	if !t.Run("config_conflict", func(t *testing.T) {
		if code, out := run(signInInput, "mount", "add", "/LanZouPan"); code == 0 || !strings.Contains(out, "config_conflict") {
			t.Fatalf("concurrent configuration was accepted (%d): %s", code, out)
		}
		raw, err := os.ReadFile(configPath)
		if err != nil || string(raw) != concurrentConfig {
			t.Fatalf("concurrent configuration changed: %v", err)
		}
		dir, err := grantDirectory(false)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{conflictingProfile, filepath.Join(dir, filepath.Base(conflictingProfile)+".grant")} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unused website authorization was retained: %v", err)
			}
		}
	}) {
		return
	}
	changeConfigOnSignIn = false
	if !t.Run("reopen", func(t *testing.T) {
		if code, out := run(signInInput, "mount", "add", "/LanZouPan"); code != 0 || !strings.Contains(out, "Browse and download") {
			t.Fatalf("mount add failed (%d): %s", code, out)
		}
	}) {
		return
	}
	original, err := os.ReadFile(configPath)
	if err != nil || !bytes.Contains(original, []byte(`"root_id": "42"`)) || bytes.Contains(original, []byte(`"account"`)) {
		t.Fatalf("saved mount did not bind the selected private root: %v", err)
	}
	t.Run("list", func(t *testing.T) {
		if code, out := run("", "ls", "/LanZouPan"); code != 0 || !strings.Contains(out, "a & b.txt") || !strings.Contains(out, "b.txt") || continuations != 1 {
			t.Fatalf("listing failed (%d): %s pages=%d", code, out, continuations)
		}
	})
	t.Run("full", func(t *testing.T) {
		destination := filepath.Join(home, "download")
		if code, out := run("", "get", "/LanZouPan/b.txt", destination); code != 0 {
			t.Fatalf("download failed (%d): %s", code, out)
		}
		if body, err := os.ReadFile(destination); err != nil || string(body) != originalBody {
			t.Fatalf("download did not yield original bytes: %v", err)
		}
	})
	t.Run("range", func(t *testing.T) {
		saved, err := loadConfig(configPath)
		if err != nil {
			t.Fatal(err)
		}
		c, err := app.openFor(t.Context(), saved, core.Path("/LanZouPan"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := closeRuntime(c); err != nil {
				t.Fatal(err)
			}
		})
		checkWebsiteRange(t, c, "/LanZouPan/b.txt", originalBody)
	})
	wrongFile = true
	if code, out := run("", "get", "/LanZouPan/b.txt", filepath.Join(home, "other"), "--json"); code == 0 || !strings.Contains(out, "invalid_upstream_response") {
		t.Fatalf("accepted a download action for another file (%d): %s", code, out)
	}
	wrongFile = false
	subject = "321"
	if code, out := run("y\n", "mount", "edit", "/LanZouPan"); code == 0 {
		t.Fatalf("different account was accepted: %s", out)
	}
	now, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(now, original) {
		t.Fatalf("failed sign-in replaced the saved mount: %v", err)
	}
	signedOut = true
	if code, out := run("", "ls", "/LanZouPan"); code == 0 || !strings.Contains(out, "mweave mount edit") || launches != releases {
		t.Fatalf("expired sign-in (%d): %s, opened=%d closed=%d", code, out, launches, releases)
	}
}
