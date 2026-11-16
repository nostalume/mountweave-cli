package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/nostalume/mountweave"
)

func TestCredentialBoundCloudRoots(t *testing.T) {
	const weiyun = "www.weiyun.com/api/v3/mcpserver"
	const yunpan = "mcp.yunpan.com/mcp"
	const nid = "9223372036854775809"
	var wyPage, ypPage []string
	for i := range 50 {
		wyPage = append(wyPage, fmt.Sprintf(`{"file_id":%q,"filename":%q,"file_size":6}`, fmt.Sprint(i), fmt.Sprintf("entry-%02d", i)))
		ypPage = append(ypPage, fmt.Sprintf(`{"nid":%q,"fname":%q,"type":"file","size":"6"}`, fmt.Sprint(i), fmt.Sprintf("entry-%02d", i)))
	}
	var requests, bodyRequests int
	var insecure360 bool
	oldTransport, oldLaunch := http.DefaultTransport, launchOAuthBrowser
	t.Cleanup(func() { http.DefaultTransport, launchOAuthBrowser = oldTransport, oldLaunch })
	var authCode string
	var browserVisits, exchanges int
	var cancelWait context.CancelFunc
	launchOAuthBrowser = func(_ context.Context, target string) error {
		u, err := url.Parse(target)
		if err != nil {
			return err
		}
		authCode = u.Query().Get("code")
		if _, err := hex.DecodeString(authCode); err != nil || len(authCode) != 16 ||
			u.Scheme != "https" || u.Host != "www.weiyun.com" || u.Path != "/authorize/token" {
			t.Fatal("invalid Weiyun browser URL")
		}
		browserVisits++
		return nil
	}
	http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		w := httptest.NewRecorder()
		route := r.URL.Host + r.URL.Path
		check := func(ok bool) {
			if !ok {
				t.Errorf("unexpected scope or credential at %s %s", r.Method, route)
			}
		}
		if route == "www.weiyun.com/api/v3/mcp/token/code" {
			check(r.Method == http.MethodPost && r.URL.Scheme == "https" && r.URL.RawQuery == "" &&
				r.Header.Get("Authorization") == "" && r.Header.Get("WyHeader") == "" && r.Header.Get("Cookie") == "")
			defer r.Body.Close()
			var request struct{ Code string }
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				return nil, err
			}
			check(request.Code == authCode && authCode != "")
			if cancelWait != nil {
				cancelWait()
				io.WriteString(w, `{"code":"11510","message":"pending"}`)
				return w.Result(), nil
			}
			exchanges++
			if exchanges == 1 {
				io.WriteString(w, `{"code":"11510","message":"pending"}`)
			} else if exchanges == 3 {
				io.WriteString(w, `{"token":"rejected"}`)
			} else {
				io.WriteString(w, `{"token":"wy-fixture"}`)
			}
			res := w.Result()
			res.Request = r
			return res, nil
		}
		if route == weiyun || route == yunpan {
			check(r.Method == "POST" && r.URL.Scheme == "https" && r.URL.RawQuery == "" && r.Header.Get("Cookie") == "")
			defer r.Body.Close()
			var req struct {
				ID     uint64
				Method string
				Params struct {
					Name      string
					Arguments struct {
						Dir                 string `json:"dir_key"`
						Parent              string `json:"pdir_key"`
						Offset, Limit, Page int
						PageSize            int `json:"page_size"`
						Path, NID           string
						Items               []struct {
							ID     string `json:"file_id"`
							Parent string `json:"pdir_key"`
						}
					}
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				return nil, err
			}
			check(req.ID != 0 && req.Method == "tools/call")
			a := req.Params.Arguments
			var data string
			switch route + " " + req.Params.Name {
			case weiyun + " weiyun.list":
				check(r.Header.Get("X-API-Key") == "" && a.Limit == 50)
				if a.Dir == "cdef" && a.Parent == "abcd" && a.Offset == 0 {
					data = `{"pdir_key":"cdef","finish_flag":true,"file_list":[{"file_id":"target","filename":"note","file_size":6}]}`
					break
				}
				check(a.Dir == "" && a.Parent == "")
				if r.Header.Get("WyHeader") == "mcp_token=rejected" {
					w.WriteHeader(http.StatusForbidden)
					return w.Result(), nil
				}
				check(r.Header.Get("WyHeader") == "mcp_token=wy-fixture")
				switch a.Offset {
				case 0:
					data = `{"pdir_key":"abcd","finish_flag":false,"file_list":[` + strings.Join(wyPage, ",") + `]}`
				case 50:
					data = `{"pdir_key":"abcd","finish_flag":true,"dir_list":[{"dir_key":"cdef","dir_name":"Docs"}],"file_list":[{"file_id":"target","filename":"note","file_size":6}]}`
				}
			case weiyun + " weiyun.download":
				check(r.Header.Get("WyHeader") == "mcp_token=wy-fixture" && r.Header.Get("X-API-Key") == "" &&
					len(a.Items) == 1 && a.Items[0].ID == "target" && (a.Items[0].Parent == "abcd" || a.Items[0].Parent == "cdef"))
				data = `{"items":[{"file_id":"target","file_size":6,"cookie":"FTN5K=receipt","https_download_url":"https://download.weiyun.com/start?sig=sealed%2b"}]}`
			case yunpan + " file-list":
				check(r.Header.Get("X-API-Key") == "yp-fixture" && r.Header.Get("WyHeader") == "" &&
					r.Header.Get("X-ECS-Env") == "prod" && r.Header.Get("X-Sub-Channel") == "open" &&
					a.Path == "/" && a.PageSize == 50)
				switch a.Page {
				case 0:
					data = `{"data":{"total":51,"list":[` + strings.Join(ypPage, ",") + `]}}`
				case 1:
					data = `{"data":{"total":51,"list":[{"nid":` + nid + `,"fname":"note","type":"file","size":"6"}]}}`
				}
			case yunpan + " get-download-url":
				check(r.Header.Get("X-API-Key") == "yp-fixture" && a.NID == nid)
				target := "https://mcp.yunpan.com/body?sig=sealed%2b"
				if insecure360 {
					target = "http://mcp.yunpan.com/body?sig=sealed%2b"
				}
				data = fmt.Sprintf(`{"errno":0,"data":{"downloadUrl":%q,"fname":"note","size":6}}`, target)
			}
			if data == "" {
				return nil, fmt.Errorf("unexpected cloud tool or continuation at %s", route)
			}
			text, _ := json.Marshal(data)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%s}]}}`, req.ID, text)
		} else {
			bodyRequests++
			check(r.Method == "GET" && r.URL.Scheme == "https" && r.Header.Get("WyHeader") == "" &&
				r.Header.Get("X-API-Key") == "" && r.Header.Get("Authorization") == "")
			switch route {
			case "download.weiyun.com/start":
				check(r.Header.Get("Cookie") == "FTN5K=receipt" && r.URL.RawQuery == "sig=sealed%2b")
				w.Header().Set("Location", "https://cloud-body.fixture/edge")
				w.WriteHeader(http.StatusFound)
			case "cloud-body.fixture/edge":
				check(r.Header.Get("Cookie") == "")
				w.Header().Set("Location", "https://download.weiyun.com/body")
				w.WriteHeader(http.StatusFound)
			case "download.weiyun.com/body", "mcp.yunpan.com/body":
				check(r.Header.Get("Cookie") == "")
				w.Header().Set("Content-Length", "6")
				io.WriteString(w, "sealed")
			default:
				return nil, fmt.Errorf("unexpected body endpoint %s", route)
			}
		}
		res := w.Result()
		res.Request = r
		return res, nil
	})

	t.Setenv("MWEAVE_TEST_WY", "wy-fixture")
	t.Setenv("MWEAVE_TEST_YP", "yp-fixture")
	t.Setenv("MWEAVE_TEST_BAD_WY", "rejected")
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	rootPrompt := ""
	reuseRoot := ""
	editError := core.MountUnavailable
	if weiyunSubfoldersAvailable {
		rootPrompt = "\n"
		reuseRoot = "/Docs\n"
		editError = core.NotFound
	}
	if managedStoreSupported {
		t.Setenv("APPDATA", dir)
		t.Setenv("XDG_CONFIG_HOME", dir)
		ctx, cancel := context.WithCancel(t.Context())
		cancelWait = cancel
		var output, prompt bytes.Buffer
		if code := testApp.Run(ctx, []string{"--config", cfg, "mount", "add", "/wy-cancel"}, strings.NewReader("weiyun\n\n\n"), &output, &prompt, true); code != 1 ||
			!strings.Contains(prompt.String(), "cancelled") || strings.Contains(prompt.String(), "Review mount") {
			t.Fatalf("cancelled authorization returned %d: %s %s", code, &output, &prompt)
		}
		cancelWait = nil
		output.Reset()
		prompt.Reset()
		input := strings.NewReader("Tencent\nweiyun\nbrowesr\nbrowser\nauthroize\nauthorize\n" + rootPrompt + "ys\ny\n")
		code := testApp.Run(t.Context(), []string{"--config", cfg, "mount", "add", "/wy-browser"}, input, &output, &prompt, true)
		transcript := prompt.String()
		if code != 0 || !strings.Contains(output.String(), "Saved mount") ||
			strings.Contains(transcript, "max_response_bytes") || strings.Contains(transcript, "Continue after approving access") {
			t.Fatalf("Weiyun setup returned %d: %s %s", code, &output, &prompt)
		}
		for _, section := range []string{
			"Add mount", "Matching sources", "Connect to Tencent Weiyun",
			"Connection", "Invalid choice", "authorization (Enter to start)", "Invalid choice",
			"operate on all files in your account", "Waiting for browser approval", "Authorized Weiyun",
			"Review mount", "Browse and download", "Save mount? [y/N]", "Invalid choice", "Save mount? [y/N]",
		} {
			_, rest, found := strings.Cut(transcript, section)
			if !found {
				t.Fatalf("missing or misplaced %q in prompt: %s", section, &prompt)
			}
			transcript = rest
		}
		invokeCLI(t, cfg, "微云\n\n\n"+reuseRoot+"y\n", true, 0, "mount", "add", "/wy-reused")
		if browserVisits != 2 || exchanges != 2 {
			t.Fatal("reused grant triggered an authorization request")
		}
		saved, err := loadConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		first, second := saved.value.Mounts[0].Credential, saved.value.Mounts[1].Credential
		if first == nil || second == nil || first.Kind != managedCredential || *first != *second {
			t.Fatal("Weiyun token was not stored as one reusable protected authorization")
		}
		assertSourceBody(t, cfg, "/wy-reused/note", "sealed")
		edit := `{"at":"/wy-reused","driver":"weiyun","options":{"root_path":"/missing"}}`
		got := invokeCLI(t, cfg, edit, false, 1, "mount", "edit", "/wy-reused", "--from", "-", "--json")
		if got.Error == nil || got.Error.Code != string(editError) {
			t.Fatalf("missing Weiyun root was saved: %+v", got.Error)
		}
		invokeCLI(t, cfg, "swtich\nswitch\n\ny\n", true, 1, "mount", "edit", "/wy-browser")
		dir, err := grantDirectory(false)
		if err != nil {
			t.Fatal(err)
		}
		records, err := filepath.Glob(filepath.Join(dir, "*.grant"))
		if err != nil || len(records) != 1 {
			t.Fatalf("rejected static authorization was persisted: %v", err)
		}
		saved, err = loadConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if *saved.value.Mounts[0].Credential != *first || *saved.value.Mounts[1].Credential != *second {
			t.Fatal("failed Weiyun replacement changed a saved mount")
		}
	}
	setup := "weiyun\n" + rootPrompt + "env:MWEAVE_TEST_WY\ny\n"
	if managedStoreSupported {
		setup = "weiyun\nreference\n" + rootPrompt + "env:MWEAVE_TEST_WY\ny\n"
	}
	invokeCLI(t, cfg, setup, true, 0, "mount", "add", "/wy")
	record := `{"at":"/yp","driver":"yunpan360","options":{},"credential":{"kind":"env","ref":"MWEAVE_TEST_YP"}}`
	before := requests
	got := invokeCLI(t, cfg, strings.Replace(record, "MWEAVE_TEST_YP", "MWEAVE_TEST_WY", 1), false, 1, "mount", "add", "--from", "-", "--json")
	if got.Error == nil || got.Error.Code != "credential_scope_conflict" || requests != before {
		t.Fatalf("credential crossed providers: %+v", got.Error)
	}
	invokeCLI(t, cfg, record, false, 0, "mount", "add", "--from", "-")
	edit := `{"at":"/wy","driver":"weiyun","options":{},"credential":{"kind":"env","ref":"MWEAVE_TEST_BAD_WY"}}`
	if got := invokeCLI(t, cfg, edit, false, 1, "mount", "edit", "/wy", "--from", "-", "--json"); got.Error == nil ||
		got.Error.Code != string(core.UpstreamAccessDenied) {
		t.Fatalf("invalid replacement credential: %+v", got.Error)
	}
	for _, at := range []string{"/wy", "/yp"} {
		if got := invokeCLI(t, cfg, "", false, 0, "ls", at, "--json"); !bytes.Contains(got.Result, []byte(`"name":"note"`)) {
			t.Fatalf("listing %s lost the second page: %s", at, got.Result)
		}
		assertSourceBody(t, cfg, at+"/note", "sealed")
	}

	localDir := t.TempDir()
	localRoot, _ := json.Marshal(localDir)
	invokeCLI(t, cfg, fmt.Sprintf(`{"at":"/local","driver":"local","options":{"root":%s}}`, localRoot), false, 0, "mount", "add", "--from", "-")
	invokeCLI(t, cfg, "", false, 0, "cp", "/wy/note", "/local/copied")
	if b, err := os.ReadFile(filepath.Join(localDir, "copied")); err != nil || string(b) != "sealed" {
		t.Fatalf("copy result: %q, %v", b, err)
	}

	insecure360 = true
	before = bodyRequests
	got = invokeCLI(t, cfg, "", false, 1, "get", "/yp/note", filepath.Join(dir, "unpublished"), "--json")
	if got.Error == nil || got.Error.Code != string(core.InvalidUpstreamResponse) ||
		!bytes.Contains(got.Result, []byte(`"effect":"unchanged"`)) || bodyRequests != before {
		t.Fatalf("insecure grant reached a body or changed the result: %+v %s", got.Error, got.Result)
	}
}
