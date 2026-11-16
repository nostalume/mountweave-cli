//go:build mweave_candidate

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/nostalume/mountweave/drivers/mobilepan"
)

func TestMobilePanWebsiteMountWorkflow(t *testing.T) {
	if !managedStoreSupported {
		t.Skip("managed credentials unavailable")
	}
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	configPath := filepath.Join(dir, "mobile.json")
	open, signIn, content := openMobilePanWebsite, signInWebsite, storageHTTPClient
	t.Cleanup(func() { openMobilePanWebsite, signInWebsite, storageHTTPClient = open, signIn, content })
	account, root := strings.Repeat("a", 64), "folder-1"
	var loginProfile string
	var expired bool
	var opens, closes, bodyCalls int
	signInWebsite = func(_ context.Context, _, profile, page string, ask askField, _ io.Writer) error {
		if page != mobilepan.WebsiteURL {
			t.Fatalf("sign-in page %s", page)
		}
		loginProfile = profile
		return confirm(ask, "Continue after signing in and closing the window?")
	}
	openMobilePanWebsite = func(_ context.Context, _ string, _ []chromedp.ExecAllocatorOption, profile string) (mobilepan.Website, error) {
		if loginProfile != "" && profile != loginProfile {
			t.Fatal("setup reopened a different profile")
		}
		loginProfile = ""
		opens++
		return mobilepan.Website{Release: func() error { closes++; return nil },
			Call: func(_ context.Context, _ string, raw json.RawMessage) (json.RawMessage, error) {
				if expired {
					return json.RawMessage(`{"sign_in":true}`), nil
				}
				var req struct {
					Route, Account string
					Data           struct {
						FileID, ParentFileID string
					}
				}
				if err := json.Unmarshal(raw, &req); err != nil {
					t.Fatal(err)
				}
				if req.Route == "account" {
					return json.RawMessage(fmt.Sprintf(`{"success":true,"data":{"account":%q}}`, account)), nil
				}
				if req.Account != account {
					return json.RawMessage(`{"status":403}`), nil
				}
				switch req.Route {
				case "list":
					switch req.Data.ParentFileID {
					case "/":
						return json.RawMessage(fmt.Sprintf(`{"success":true,"data":{"items":[{"fileId":%q,"parentFileId":"/","name":"Archive","type":"folder"}]}}`, root)), nil
					case root:
						return json.RawMessage(fmt.Sprintf(`{"success":true,"data":{"items":[{"fileId":"file-1","parentFileId":%q,"name":"file.txt","type":"file","size":5}]}}`, root)), nil
					}
				case "getDownloadUrl":
					if req.Data.FileID == "file-1" {
						return json.RawMessage(`{"success":true,"data":{"url":"https://content.fixture/file?signature=fixture"}}`), nil
					}
				}
				t.Fatalf("unexpected operation %+v", req)
				return nil, nil
			},
		}, nil
	}
	storageHTTPClient = &http.Client{Transport: sealedTransport(func(r *http.Request) (*http.Response, error) {
		bodyCalls++
		if r.URL.Host != "content.fixture" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("unexpected body recipient or account credential")
		}
		return &http.Response{StatusCode: http.StatusOK, ContentLength: 5, Body: io.NopCloser(strings.NewReader("hello"))}, nil
	})}
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	run := func(input string, args ...string) (int, string) {
		var output, diagnostics bytes.Buffer
		code := app.Run(t.Context(), append([]string{"--config", configPath}, args...), strings.NewReader(input), &output, &diagnostics, true)
		return code, output.String() + diagnostics.String()
	}
	if code, out := run("移动云盘\ny\n/Archive\ny\n", "mount", "add", "/MobilePan"); code != 0 {
		t.Fatalf("mount add failed (%d): %s", code, out)
	}
	configured, err := os.ReadFile(configPath)
	if err != nil || bytes.Contains(configured, []byte(account)) || !bytes.Contains(configured, []byte("/Archive")) {
		t.Fatalf("unexpected published configuration: %v", err)
	}
	if code, out := run("", "ls", "/MobilePan"); code != 0 || !strings.Contains(out, "file.txt") {
		t.Fatalf("list failed (%d): %s", code, out)
	}
	target := filepath.Join(dir, "sample")
	if code, out := run("", "get", "/MobilePan/file.txt", target); code != 0 {
		t.Fatalf("download failed (%d): %s", code, out)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "hello" {
		t.Fatalf("unexpected original bytes: %v", err)
	}
	root = "replacement"
	if code, out := run("", "ls", "/MobilePan"); code == 0 {
		t.Fatalf("same-name replacement folder accepted: %s", out)
	}
	root = "folder-1"
	account = strings.Repeat("b", 64)
	if code, out := run("y\n", "mount", "edit", "/MobilePan"); code == 0 {
		t.Fatalf("different account accepted: %s", out)
	}
	account = strings.Repeat("a", 64)
	if code, out := run("y\n", "mount", "edit", "/MobilePan"); code != 0 {
		t.Fatalf("same-account sign-in failed (%d): %s", code, out)
	}
	if code, out := run("", "ls", "/MobilePan"); code != 0 || !strings.Contains(out, "file.txt") {
		t.Fatalf("reopen failed (%d): %s", code, out)
	}
	expired = true
	if code, out := run("", "ls", "/MobilePan", "--json"); code == 0 || !strings.Contains(out, "interaction_required") || !strings.Contains(out, "mount edit") {
		t.Fatalf("expired session omitted recovery action (%d): %s", code, out)
	}
	if bodyCalls != 1 || opens != closes {
		t.Fatalf("body requests %d; sessions opened %d and closed %d", bodyCalls, opens, closes)
	}
	if now, err := os.ReadFile(configPath); err != nil || !bytes.Equal(configured, now) {
		t.Fatalf("sign-in changed the mount configuration: %v", err)
	}
}
