//go:build mweave_candidate

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
	"testing"

	"github.com/chromedp/chromedp"
	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/alipan"
)

func readAliPanWebsiteProfile(m mountRecord) (string, error) {
	var opts aliPanWebsiteOptions
	if err := decodeObject(m.Options, &opts); err != nil {
		return "", err
	}
	lease, err := acquireGrant(m.Credential.Ref, false)
	if err != nil {
		return "", err
	}
	defer lease.lock.Close()
	r, err := readAliPanWebsite(lease, opts)
	return r.Profile, err
}

func TestWebsiteReadAliPan(t *testing.T) {
	if !managedStoreSupported {
		t.Skip("managed credentials unavailable")
	}
	home := t.TempDir()
	t.Setenv("APPDATA", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	configPath := filepath.Join(home, "candidate.json")
	originalBrowser, originalSignIn, originalTransport := openAliPanWebsite, signInWebsite, http.DefaultTransport
	t.Cleanup(func() {
		openAliPanWebsite, signInWebsite, http.DefaultTransport = originalBrowser, originalSignIn, originalTransport
	})
	const accountID = "fixture-account"
	subject := accountID
	var launches, releases, continuation, bodies int
	var loginBinary, loginProfile string
	var signedOut bool
	signInWebsite = func(_ context.Context, binary, profile, page string, ask askField, _ io.Writer) error {
		if page != "https://www.alipan.com/sign/in?back=%2Frobots.txt" {
			return errors.New("unexpected sign-in entry")
		}
		loginBinary, loginProfile = binary, profile
		return confirm(ask, "Continue after signing in and closing the window?")
	}
	root := `{"file_id":"archive-id","drive_id":"resource","parent_file_id":"root","name":"Archive","type":"folder"}`
	entry := `{"file_id":"item","drive_id":"resource","parent_file_id":"archive-id","name":"item.txt","type":"file","size":5}`
	openAliPanWebsite = func(_ context.Context, binary string, _ []chromedp.ExecAllocatorOption, profile string) (alipan.Website, error) {
		launches++
		if loginProfile != "" && (profile != loginProfile || binary != loginBinary) {
			return alipan.Website{}, errors.New("verification did not reopen the sign-in profile in the same browser")
		}
		loginProfile = ""
		if err := privateWebsiteDirectory(profile, false); err != nil {
			return alipan.Website{}, err
		}
		return alipan.Website{
			Release: func() error { releases++; return nil },
			Call: func(_ context.Context, source string, args json.RawMessage) (json.RawMessage, error) {
				if !strings.Contains(source, "page-channel-port:requestClient") {
					return nil, errors.New("missing source-owned page call")
				}
				var call struct {
					Operation string
					Data      map[string]any
				}
				if err := json.Unmarshal(args, &call); err != nil {
					return nil, err
				}
				var value string
				switch call.Operation {
				case "account":
					if signedOut {
						return json.RawMessage(`{"sign_in":true}`), nil
					}
					value = fmt.Sprintf(`{"user_id":%q,"resource_drive_id":"resource","nick_name":"Example"}`, subject)
				case "file":
					value = root
				case "page":
					switch call.Data["parent_file_id"] {
					case "root":
						if call.Data["marker"] == "" {
							value = `{"items":[],"next_marker":"next"}`
						} else {
							continuation++
							value = `{"items":[` + root + `]}`
						}
					case "archive-id":
						value = `{"items":[` + entry + `]}`
					default:
						return nil, errors.New("listing accessed the wrong folder")
					}
				case "download":
					if call.Data["drive_id"] != "resource" || call.Data["file_id"] != "item" {
						return nil, errors.New("download accessed the wrong object")
					}
					value = `{"url":"https://content.fixture/file","method":"GET"}`
				default:
					return nil, errors.New("website received an operation outside the read contract")
				}
				return json.RawMessage(`{"value":` + value + `}`), nil
			},
		}, nil
	}
	http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "content.fixture" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("file request received account credentials or reached another host")
		}
		if r.Header.Get("Referer") != "https://www.alipan.com/" {
			t.Error("file request did not include the website Referer required by the download grant")
		}
		w := httptest.NewRecorder()
		bodies++
		if r.Header.Get("Range") == "bytes=1-3" {
			w.Header().Set("Content-Length", "3")
			w.Header().Set("Content-Range", "bytes 1-3/5")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.WriteString("ell")
		} else {
			w.Header().Set("Content-Length", "5")
			_, _ = w.WriteString("hello")
		}
		return w.Result(), nil
	})
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	run := func(input string, args ...string) (int, string) {
		var output, diagnostics bytes.Buffer
		code := app.Run(context.Background(), append([]string{"--config", configPath}, args...), strings.NewReader(input), &output, &diagnostics, true)
		return code, output.String() + diagnostics.String()
	}
	if !t.Run("reopen", func(t *testing.T) {
		if code, output := run("阿里云盘\n\nyse\ny\n/Missing\n/Archive\ny\n", "mount", "add", "/AliPan"); code != 0 || !strings.Contains(output, "Invalid choice. Try again.") || !strings.Contains(output, "Browse and download") || !strings.Contains(output, "Try another folder") || launches != 1 || releases != 1 || continuation != 2 {
			t.Fatalf("setup = %d %s launches=%d releases=%d pages=%d", code, output, launches, releases, continuation)
		}
	}) {
		return
	}
	if code, output := run("", "mount", "add", "--help"); code != 0 || strings.Count(output, "Aliyun Drive") != 1 || strings.Contains(output, "alipan-web") {
		t.Fatalf("source help = %d %s", code, output)
	}
	original, err := os.ReadFile(configPath)
	if err != nil || !bytes.Contains(original, []byte("archive-id")) || !bytes.Contains(original, []byte(`"connection": "website"`)) || bytes.Contains(original, []byte("https://content.fixture")) {
		t.Fatal("mount did not retain its native directory")
	}
	t.Run("list", func(t *testing.T) {
		if code, output := run("", "ls", "/AliPan"); code != 0 || !strings.Contains(output, "item.txt") {
			t.Fatalf("listing = %d %s", code, output)
		}
	})
	t.Run("full", func(t *testing.T) {
		dest := filepath.Join(home, "downloaded")
		if code, output := run("", "get", "/AliPan/item.txt", dest); code != 0 || bodies != 1 {
			t.Fatalf("download = %d %s", code, output)
		}
		if data, err := os.ReadFile(dest); err != nil || string(data) != "hello" {
			t.Fatalf("downloaded %q: %v", data, err)
		}
	})
	saved, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	mount := saved.value.Mounts[0]
	for _, method := range []string{"open", "unknown"} {
		edit := mount
		edit.Connection = method
		if method == "open" {
			edit.Options = json.RawMessage(`{"drive_id":"resource","root_id":"archive-id"}`)
		}
		edit.Credential = nil // An omitted reference must never move to another route.
		record, err := json.Marshal(edit)
		if err != nil {
			t.Fatal(err)
		}
		before := launches
		if code, _ := run(string(record), "mount", "edit", "/AliPan", "--from", "-"); code == 0 || launches != before {
			t.Fatalf("unsafe edit with connection %q: code=%d browser launches=%d", method, code, launches-before)
		}
	}
	installedProfile, err := readAliPanWebsiteProfile(mount)
	if err != nil {
		t.Fatal(err)
	}
	second := mount
	second.At, second.Driver, second.Connection = "/Alias", aliPanWebsiteSource, ""
	saved.value.Mounts = append(saved.value.Mounts, second)
	beforeLaunches, beforeReleases := launches, releases
	c, err := app.openFor(context.Background(), saved, core.Path("/"))
	if err != nil {
		t.Fatal(err)
	}
	if launches != beforeLaunches+1 {
		t.Fatal("shared authorization opened multiple browsers")
	}
	input := strings.NewReader("must remain unread")
	change, err := c.Write(context.Background(), localActor, core.Path("/Alias/new.txt"), input, nil)
	if core.CodeOf(err) != core.UnsupportedOperation || change.Effect != core.Unchanged || int64(input.Len()) != input.Size() {
		t.Fatalf("write effect %v, error %v, unread bytes %d", change.Effect, err, input.Len())
	}
	t.Run("range", func(t *testing.T) {
		checkWebsiteRange(t, c, "/Alias/item.txt", "hello")
	})
	if err := closeRuntime(c); err != nil || beforeReleases+1 != releases {
		t.Fatalf("shared close %v, releases %d", err, releases)
	}
	beforeLaunches = launches
	if code, output := run("n\n", "mount", "edit", "/AliPan"); code == 0 || launches != beforeLaunches || !strings.Contains(output, "cancelled") {
		t.Fatalf("declining sign-in started verification: %d %s", code, output)
	}
	if _, err := os.Stat(loginProfile); !os.IsNotExist(err) {
		t.Fatalf("declined candidate profile was not removed: %v", err)
	}
	signedOut = true
	if code, output := run("y\n", "mount", "edit", "/AliPan"); code == 0 || !strings.Contains(output, "did not retain") {
		t.Fatalf("accepting without sign-in = %d %s", code, output)
	}
	signedOut, subject = false, "other-account"
	if code, _ := run("y\n", "mount", "edit", "/AliPan"); code == 0 {
		t.Fatal("changing account during reauthorization succeeded")
	}
	before, err := readAliPanWebsiteProfile(mount)
	if err != nil || before != installedProfile {
		t.Fatalf("failed sign-in changed the stored authorization profile: %v", err)
	}
	subject = accountID
	if code, output := run("y\n", "mount", "edit", "/AliPan"); code != 0 {
		t.Fatalf("reauthorization = %d %s", code, output)
	}
	after, err := readAliPanWebsiteProfile(mount)
	if err != nil || after == installedProfile {
		t.Fatalf("reauthorization did not replace the profile: %v", err)
	}
	if current, _ := os.ReadFile(configPath); !bytes.Equal(original, current) || launches != releases {
		t.Fatalf("record or session lifecycle changed: launches=%d releases=%d", launches, releases)
	}
}
