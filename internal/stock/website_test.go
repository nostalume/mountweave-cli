package stock

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave-cli/internal/website"
	"github.com/nostalume/mountweave/drivers/alipan"
	"github.com/nostalume/mountweave/drivers/lanzou"
	"github.com/nostalume/mountweave/drivers/quark"
	quarkcdp "github.com/nostalume/mountweave/drivers/quark/cdp"
	"github.com/nostalume/mountweave/drivers/uc"
)

func websiteTestApp(t *testing.T, login string, app *cliapp.App) (string, func(string, bool, ...string) string, func() cliapp.ConfigSnapshot) {
	t.Helper()
	if !cliapp.ManagedStoreSupported {
		t.Skip("managed storage is unavailable")
	}
	config := filepath.Join(isolatedSettingsHome(t), "config.json")
	old := signInWebsite
	t.Cleanup(func() { signInWebsite = old })
	signInWebsite = func(_ context.Context, _, _, page string, ask cliapp.Prompt, _ io.Writer) error {
		if page != login {
			return errors.New("unexpected login URL")
		}
		return cliapp.Confirm(ask, "Continue?")
	}
	run := func(input string, ok bool, args ...string) string {
		t.Helper()
		var result bytes.Buffer
		code := app.Run(t.Context(), append([]string{"--config", config}, args...), strings.NewReader(input), &result, &result, true)
		if (code == 0) != ok {
			t.Fatalf("%v => %d: %s", args, code, result.String())
		}
		return result.String()
	}
	snapshot := func() cliapp.ConfigSnapshot {
		t.Helper()
		cfg, err := cliapp.LoadConfig(config)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	return config, run, snapshot
}

func TestAliPanSharedWebsiteGrant(t *testing.T) {
	config, run, snapshot := websiteTestApp(t, alipan.WebsiteLoginURL, testApp)
	old := openAliPanWebsite
	t.Cleanup(func() { openAliPanWebsite = old })
	launches, releases := 0, 0
	var sharedRef string
	var refuseSibling bool
	const originalAccount = `{"value":{"user_id":"fixture","resource_drive_id":"resource"}}`
	accountReply := originalAccount
	openAliPanWebsite = func(_ context.Context, _ string, _ []chromedp.ExecAllocatorOption, profile string) (alipan.Website, error) {
		if err := website.PrivateDirectory(profile, false); err != nil {
			return alipan.Website{}, err
		}
		launches++
		return alipan.Website{
			Release: func() error {
				releases++
				if sharedRef != "" {
					if lease, err := cliapp.AcquireGrant(sharedRef, false); err == nil {
						_ = lease.Lock.Close()
						return errors.New("grant unlocked while the browser was active")
					}
				}
				return nil
			},
			Call: func(_ context.Context, _ string, raw json.RawMessage) (json.RawMessage, error) {
				var request struct {
					Operation string
					Data      map[string]any
				}
				if err := json.Unmarshal(raw, &request); err != nil {
					return nil, err
				}
				const folder = `{"file_id":"archive","drive_id":"resource","parent_file_id":"root","name":"Archive","type":"folder"}`
				switch request.Operation {
				case "account":
					return json.RawMessage(accountReply), nil
				case "file":
					return json.RawMessage(`{"value":` + folder + `}`), nil
				case "page":
					switch request.Data["parent_file_id"] {
					case "root":
						if refuseSibling {
							return nil, core.Fail(core.PermissionDenied, nil)
						}
						return json.RawMessage(`{"value":{"items":[` + folder + `]}}`), nil
					case "archive":
						return json.RawMessage(`{"value":{"items":[]}}`), nil
					}
				}
				return nil, fmt.Errorf("unexpected AliPan operation %q", request.Operation)
			},
		}, nil
	}
	run("alipan\n\ny\n/Archive\ny\n", true, "mount", "add", "/Archive")
	cfg := snapshot()
	primary := cfg.Value.Mounts[0]
	sibling := primary
	sibling.At, sibling.Driver, sibling.Connection = "/Root", cliapp.AliPanWebsiteSource, ""
	sibling.Options = json.RawMessage(`{"root_id":"root","account":"fixture","drive_id":"resource"}`)
	cfg.Value.Mounts = append(cfg.Value.Mounts, sibling)
	if applied, err := cfg.Save(t.Context()); err != nil || !applied {
		t.Fatalf("save sibling: %v", err)
	}
	cfg, sharedRef = snapshot(), primary.Credential.Ref
	before := launches
	c, err := cliapp.OpenFor(testApp, t.Context(), cfg, core.Path("/"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cliapp.CloseRuntime(c); err != nil || launches != before+1 || launches != releases {
		t.Fatalf("one grant should share one browser and release it once: %d opens, %d closes: %v", launches, releases, err)
	}
	profile := func() string {
		t.Helper()
		lease, err := cliapp.AcquireGrant(sharedRef, false)
		if err != nil {
			t.Fatal(err)
		}
		record, err := readAliPanWebsite(lease.Read, aliPanWebsiteOptions{Account: "fixture", Drive: "resource"})
		if err := errors.Join(err, lease.Lock.Close()); err != nil {
			t.Fatal(err)
		}
		return record.Profile
	}
	original := profile()
	const replacedAccount = `{"value":{"user_id":"different","resource_drive_id":"resource"}}`
	for _, failure := range []struct {
		reply, code string
		reconnect   bool
	}{
		{replacedAccount, "credential_scope_conflict", true},
		{`{"sign_in":true}`, "interaction_required", true},
		{`{"status":403}`, string(core.UpstreamAccessDenied), false},
	} {
		accountReply = failure.reply
		got := invokeCLI(t, config, "", false, 1, "ls", "/Archive", "--json")
		if got.Error == nil || got.Error.Code != failure.code ||
			strings.Contains(got.Error.NextAction, cliapp.MountEditAction(cfg.Path, "/Archive")) != failure.reconnect {
			t.Errorf("website failure %s: got %+v, want %s with reconnect=%t", failure.reply, got.Error, failure.code, failure.reconnect)
		}
	}
	accountReply = replacedAccount
	output := run("2\ny\n", false, "mount", "edit", "/Archive")
	if !strings.Contains(output, cliapp.WebsiteMountScopeConflict().Message) {
		t.Errorf("editing with a different website account returned the wrong diagnostic: %s", output)
	}
	if profile() != original || snapshot().Digest != cfg.Digest || launches != releases {
		t.Error("editing with a different website account should preserve the saved binding and release the candidate")
	}
	accountReply = originalAccount
	refuseSibling = true
	run("2\ny\n", false, "mount", "edit", "/Archive")
	if profile() != original {
		t.Fatal("a failed sibling proof published the candidate")
	}
	refuseSibling = false
	run("2\ny\n", true, "mount", "edit", "/Archive")
	if profile() == original || snapshot().Digest != cfg.Digest || launches != releases {
		t.Fatal("reconnecting changed the saved roots or retained an extra browser")
	}
}

func TestLanZouWebsiteReceipt(t *testing.T) {
	websiteRoute := lanZouWebsiteRoute()
	app, err := cliapp.NewApp(nil, cliapp.ServiceSource(cliapp.LanZouSource, "LanZou Cloud",
		cliapp.SourceConnection{Method: "website", Route: &websiteRoute}))
	if err != nil {
		t.Fatal(err)
	}
	config, run, snapshot := websiteTestApp(t, lanzou.LoginURL, app)
	old := openLanZouWebsite
	t.Cleanup(func() { openLanZouWebsite = old })
	wrongAction := false
	opens, releases, transfers := 0, 0, 0
	openLanZouWebsite = func(_ context.Context, _ string, _ []chromedp.ExecAllocatorOption, _ string) (lanzou.WebsiteOptions, error) {
		opens++
		return lanzou.WebsiteOptions{
			Release: func() error { releases++; return nil },
			Call: func(_ context.Context, _ string, raw json.RawMessage) (json.RawMessage, error) {
				if string(raw) == `"2"` {
					return json.RawMessage(`{"code":"1","origin":"https://sample.lanzoub.com","id":"file-key","password":"fixture"}`), nil
				}
				var q struct {
					Task, Page int
					Folder     string
				}
				if err := json.Unmarshal(raw, &q); err != nil {
					return nil, err
				}
				items := "[]"
				switch {
				case q.Task == 47 && q.Folder == "-1":
					items = `[{"id":"42","name":"Archive"}]`
				case q.Task == 5 && q.Folder == "42" && q.Page == 1:
					items = `[{"id":"2","name":"note.txt"}]`
				case (q.Task == 47 || q.Task == 5) && (q.Folder == "-1" || q.Folder == "42"):
				default:
					return nil, fmt.Errorf("unexpected folder query %+v", q)
				}
				return json.RawMessage(`{"code":"1","account":"123","entries":` + items + `}`), nil
			},
			ShareTransport: sealedTransport(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
					return nil, errors.New("private credentials reached the download service")
				}
				w := httptest.NewRecorder()
				switch {
				case r.Method == http.MethodGet && r.URL.String() == "https://sample.lanzoub.com/file-key":
					id := 2
					if wrongAction {
						id = 3
					}
					fmt.Fprintf(w, `function down_p(){ var isngis = 'website_fixture_signature'; url : 'https://apifile.woozooo.com/ajaxfile.php?file=%d' }`, id)
				case r.Method == http.MethodPost && r.URL.String() == "https://apifile.woozooo.com/ajaxfile.php?file=2":
					_, _ = io.WriteString(w, `{"zt":1,"dom":"https://fixture.lanrar.com","url":"body-token","inf":"note.txt"}`)
				default:
					return nil, fmt.Errorf("unexpected download request %s", r.URL)
				}
				return w.Result(), nil
			}),
			Download: func(_ context.Context, target *url.URL, _ *core.Range) (core.Opened, error) {
				transfers++
				if target.String() != "https://fixture.lanrar.com/file/body-token" {
					return core.Opened{}, errors.New("unexpected native body URL")
				}
				size := int64(8)
				return core.Opened{Metadata: core.Metadata{Kind: core.File, Size: &size}, Length: &size, Body: io.NopCloser(strings.NewReader("original"))}, nil
			},
		}, nil
	}
	run("lanzou\ny\n/Archive\ny\n", true, "mount", "add", "/drive")
	saved := snapshot().Value.Mounts[0]
	var options lanZouOptions
	if err := cliapp.DecodeObject(saved.Options, &options); err != nil || options.Root != "42" {
		t.Fatalf("expected native root ID 42: %s, %v", saved.Options, err)
	}
	run("2\ny\n", true, "mount", "edit", "/drive")
	reopened := snapshot().Value.Mounts[0]
	if saved.Connection != "website" || reopened.Connection != saved.Connection ||
		saved.Credential.Ref == reopened.Credential.Ref || !bytes.Equal(saved.Options, reopened.Options) {
		t.Fatal("LanZou reconnect must replace the reference and keep the connection and root ID")
	}
	dest := filepath.Join(filepath.Dir(config), "download")
	run("", true, "get", "/drive/note.txt", dest)
	if data, err := os.ReadFile(dest); err != nil || string(data) != "original" {
		t.Fatalf("original bytes: %q, %v", data, err)
	}
	wrongAction = true
	run("", false, "get", "/drive/note.txt", dest+"-invalid")
	if transfers != 1 || opens != releases {
		t.Fatalf("accepted an unrelated file action or retained the session: %d transfers, %d opens, %d closes", transfers, opens, releases)
	}
}
func websiteFixtureBrowser(t *testing.T) (string, []chromedp.ExecAllocatorOption) {
	t.Helper()
	binary, options := website.Browser()
	if binary == "" {
		t.Skip("No supported browser")
	}
	return binary, append(options, chromedp.Flag("ignore-certificate-errors", true))
}

func readWebsiteBody(t *testing.T, body io.ReadCloser, err error, want string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if body == nil {
		t.Fatal("download did not yield a body")
	}
	data, err := io.ReadAll(body)
	if err := errors.Join(err, body.Close()); err != nil || string(data) != want {
		t.Fatalf("read %q, want %q: %v", data, want, err)
	}
}

func TestUCWebsiteTransfer(t *testing.T) {
	binary, options := websiteFixtureBrowser(t)
	var preflights, grantCalls, bodyCalls, uiPageLoads atomic.Int32
	var ignoredRange, stalledBody, shortBody atomic.Bool
	var keptSession, keptReferrer atomic.Bool
	bodyCancelled := make(chan struct{}, 1)
	grant := httptest.NewUnstartedServer(nil)
	grantURL := "https://" + grant.Listener.Addr().String() + "/grant"
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/attachment":
			bodyCalls.Add(1)
			w.Header().Set("Content-Disposition", "attachment; filename=fixture")
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if r.Header.Get("Range") == "bytes=1-4" && !ignoredRange.Load() {
				_, err := r.Cookie("sealed-session")
				keptSession.Store(err == nil)
				keptReferrer.Store(strings.HasPrefix(r.Referer(), "https://127.0.0.1:"))
				w.Header().Set("Content-Range", "bytes 1-4/6")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "ampl")
				_ = http.NewResponseController(w).Flush()
				return
			}
			if stalledBody.Load() {
				w.Header().Set("Content-Length", "6")
				_, _ = io.WriteString(w, "s")
				_ = http.NewResponseController(w).Flush()
				<-r.Context().Done()
				bodyCancelled <- struct{}{}
				return
			}
			body := "sample"
			if shortBody.Load() {
				body = "short"
			}
			_, _ = io.WriteString(w, body)
			_ = http.NewResponseController(w).Flush()
		case "/account/info":
			w.Header().Set("Content-Type", "application/json")
			if _, err := r.Cookie("sealed-session"); err != nil {
				_, _ = io.WriteString(w, `{"success":true,"data":{}}`)
				return
			}
			_, _ = io.WriteString(w, `{"success":true,"data":{"uid":"sealed-account"}}`)
		case "/1/clouddrive/file/sort":
			w.Header().Set("Content-Type", "application/json")
			query := r.URL.Query()
			if _, err := r.Cookie("sealed-session"); err != nil || query.Get("pr") != "UCBrowser" ||
				query.Get("fr") != "pc" || query.Get("pdir_fid") != "0" || query.Get("_fetch_total") != "1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch query.Get("_page") + ":" + query.Get("_size") {
			case "1:50":
				_, _ = io.WriteString(w, `{"status":200,"metadata":{"_page":1,"_total":51},"data":{"list":[`)
				for i := 0; i < 50; i++ {
					if i != 0 {
						_, _ = io.WriteString(w, ",")
					}
					_, _ = fmt.Fprintf(w, `{"fid":"other-%d","file_name":"other-%d.txt","file_type":1,"size":1}`, i, i)
				}
				_, _ = io.WriteString(w, `]}}`)
			case "2:50":
				if query.Get("ui") == "1" {
					uiPageLoads.Add(1)
				}
				_, _ = io.WriteString(w, `{"status":200,"metadata":{"_page":2,"_total":51},"data":{"list":[{"fid":"chosen","file_name":"sample.txt","file_type":1,"size":6}]}}`)
			case "1:1", "2:1":
				index := 0
				if query.Get("_page") == "2" {
					index++
				}
				_, _ = fmt.Fprintf(w, `{"status":200,"metadata":{"_page":%s,"_total":51},"data":{"list":[{"fid":"other-%d","file_name":"other-%d.txt","file_type":1,"size":1}]}}`,
					query.Get("_page"), index, index)
			default:
				w.WriteHeader(http.StatusBadRequest)
			}
		default:
			http.SetCookie(w, &http.Cookie{Name: "sealed-session", Value: "fixture", Path: "/", Secure: true, HttpOnly: true})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w, `<!doctype html><style>
			  .ant-table-row{height:60px;width:450px;padding:10px}.hover-oper-item{display:none}
			  .hover-oper-show .hover-oper-item{display:block}
			  </style>
			  <div class="file-list table-all-list">
				<div class="ant-table-body"><div class="ant-table-row" data-row-key="other-0">First page</div></div>
				<ul class="ant-table-pagination ant-pagination">
					<li class="ant-pagination-next" aria-disabled="false"><button type="button">Next</button></li>
				</ul>
			  </div><script>
				const next = document.querySelector(".ant-pagination-next");
				next.onclick = async event => {
					if (!event.isTrusted) return;
					const response = await fetch("/1/clouddrive/file/sort?pr=UCBrowser&fr=pc&pdir_fid=0&_page=2&_size=50&_fetch_total=1&ui=1");
					const reply = await response.json();
					const file = reply.data.list[0];
					const row = document.createElement("div");
					row.className = "ant-table-row";
					row.dataset.rowKey = file.fid;
					row.addEventListener("mouseover", () => row.classList.add("hover-oper-show"));
					row.addEventListener("mouseleave", () => row.classList.remove("hover-oper-show"));
					const button = document.createElement("button");
					button.className = "hover-oper-item hoitem-down";
					button.title = "下载";
					button.textContent = "Download";
					button.onclick = async event => {
						if (!event.isTrusted) return;
						const grant = await fetch(%q, {
							method:"POST", headers:{"Content-Type":"application/json"},
							body: JSON.stringify({fids:[window.selected || file.fid]}),
							credentials:"include"
						});
						const receipt = await grant.json();
						const frame = document.createElement("iframe");
						frame.src = receipt.data[0].download_url;
						document.body.appendChild(frame);
					};
					row.appendChild(button);
					document.querySelector(".ant-table-body").replaceChildren(row);
					next.setAttribute("aria-disabled", "true");
				};
			</script>`, grantURL+"?pr=UCBrowser&fr=pc")
		}
	}))
	defer app.Close()
	grant.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/grant" || r.Header.Get("Origin") != app.URL {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", app.URL)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		if r.Method == http.MethodOptions {
			_, cookieErr := r.Cookie("sealed-session")
			if r.Header.Get("Access-Control-Request-Method") != http.MethodPost ||
				r.Header.Get("Access-Control-Request-Headers") != "content-type" || !errors.Is(cookieErr, http.ErrNoCookie) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			preflights.Add(1)
			w.Header().Set("Access-Control-Allow-Methods", http.MethodPost)
			w.Header().Set("Access-Control-Allow-Headers", "content-type")
			w.Header().Set("Access-Control-Max-Age", "0")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost || preflights.Load() == 0 || r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		grantCalls.Add(1)
		if _, err := r.Cookie("sealed-session"); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"code":0,"data":[{"fid":"chosen","size":6,"download_url":%q}]}`, app.URL+"/attachment?hidden=fixture")
	})
	grant.StartTLS()
	defer grant.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	var config, profileID, profile string
	if cliapp.ManagedStoreSupported {
		config, profileID = filepath.Join(isolatedSettingsHome(t), "config.json"), cliapp.NewGrantID()
		var err error
		profile, err = websiteProfile(profileID, true)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		profile = filepath.Join(t.TempDir(), "profile")
		if err := website.PrivateDirectory(profile, true); err != nil {
			t.Fatal(err)
		}
	}
	rewrite := strings.NewReplacer("https://drive.uc.cn", app.URL, "https://pc-api.uc.cn", app.URL)
	openPage := func(ctx context.Context, profile string) (websitePage, error) {
		page, err := openWebsitePage(ctx, binary, options, profile, app.URL+"/#/list/all", nil)
		if err == nil {
			call := page.Call
			page.Call = func(ctx context.Context, script string, args json.RawMessage) (json.RawMessage, error) {
				return call(ctx, rewrite.Replace(script), args)
			}
		}
		return page, err
	}
	openDriver := func(ctx context.Context, page websitePage, account, root, rootID string, release func() error) (*uc.Driver, error) {
		return uc.OpenWebsite(ctx, uc.WebsiteOptions{
			Tab: page.Tab, Call: page.Call, Release: release, Origin: app.URL,
			GrantEndpoint: grantURL, Account: account, RootPath: root, RootID: rootID,
		})
	}
	page, err := openPage(ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = page.Release() })
	account, err := uc.WebsiteAccount(ctx, page.Call)
	if err != nil {
		t.Fatal(err)
	}
	d, err := openDriver(ctx, page, account, "/", "", page.Release)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	target := core.NativePath("/sample.txt")
	checkDownload := func() {
		t.Helper()
		if uiPageLoads.Load() != 1 || preflights.Load() != 1 || grantCalls.Load() != 1 || bodyCalls.Load() != 1 {
			t.Fatal("the selected file should use page two and exactly one download")
		}
	}
	if config == "" {
		opened, err := d.Open(ctx, target, core.OpenOptions{})
		readWebsiteBody(t, opened.Body, err, "sample")
		checkDownload()
	}
	ranged := core.OpenOptions{Range: &core.Range{Start: 1, End: 4}}
	opened, err := d.Open(ctx, target, ranged)
	readWebsiteBody(t, opened.Body, err, "ampl")
	if opened.Range == nil || *opened.Range != (core.ContentRange{Start: 1, End: 4, Total: 6}) ||
		!keptSession.Load() || !keptReferrer.Load() {
		t.Fatal("range lost the receipt, session or referrer")
	}
	ignoredRange.Store(true)
	opened, err = d.Open(ctx, target, ranged)
	if opened.Body != nil {
		_ = opened.Body.Close()
	}
	if err == nil {
		t.Fatal("accepted HTTP 200 for a nonzero range")
	}
	ignoredRange.Store(false)

	stalledBody.Store(true)
	stream, stop := context.WithCancel(ctx)
	opened, err = d.Open(stream, target, core.OpenOptions{})
	stop()
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-bodyCancelled:
	case <-ctx.Done():
		t.Fatal("cancelled read retained the browser body")
	}
	stalledBody.Store(false)
	if _, err := page.Call(ctx, `async () => { window.selected = "different"; return "{}"; }`, json.RawMessage("null")); err != nil {
		t.Fatal(err)
	}
	beforeGrant, beforeBody := grantCalls.Load(), bodyCalls.Load()
	opened, err = d.Open(ctx, target, core.OpenOptions{})
	if opened.Body != nil {
		_ = opened.Body.Close()
	}
	if err == nil || grantCalls.Load() != beforeGrant || bodyCalls.Load() != beforeBody {
		t.Fatal("admitted a download request for another file")
	}
	if config == "" {
		return
	}
	rootID := d.RootID()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	source := websiteSource{ID: cliapp.UcSource, Name: "UC Cloud Drive", Entry: uc.WebsiteURL}
	source.Open = func(ctx context.Context, _ string, _ []chromedp.ExecAllocatorOption, reopenedProfile string) (websiteSession, error) {
		if reopenedProfile != profile {
			return websiteSession{}, errors.New("reopened a different browser profile")
		}
		page, err := openPage(ctx, reopenedProfile)
		if err != nil {
			return websiteSession{}, err
		}
		return websiteSession{Close: page.Release, Mount: func(ctx context.Context, account, root, rootID string, release func() error) (websiteDriver, error) {
			return openDriver(ctx, page, account, root, rootID, release)
		}}, nil
	}
	host, err := cliapp.NewApp(nil, cliapp.SingleSource(cliapp.UcSource, source.Name, source.route()))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := cliapp.LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	mount := cliapp.MountRecord{At: "/uc", Driver: cliapp.UcSource, Options: json.RawMessage(`{"root":"/"}`),
		Credential: &cliapp.CredentialRef{Kind: cliapp.ManagedCredential, Ref: profileID}}
	binding, err := json.Marshal(websiteMountGrant{Provider: cliapp.UcSource, Profile: profileID, Account: account, Root: "/", RootID: rootID})
	if err != nil {
		t.Fatal(err)
	}
	receipt, _, err := cliapp.CommitMount(ctx, saved, cliapp.MountRecord{}, mount, nil, binding)
	if err != nil || receipt.Effect != core.Applied.String() {
		t.Fatalf("save fixture mount: %v", err)
	}
	for _, count := range []*atomic.Int32{&uiPageLoads, &preflights, &grantCalls, &bodyCalls} {
		count.Store(0)
	}
	dest := filepath.Join(filepath.Dir(config), "download")
	invokeApp(t, host, config, "", false, 0, "get", "/uc/sample.txt", dest)
	if data, err := os.ReadFile(dest); err != nil || string(data) != "sample" {
		t.Fatalf("original bytes: %q, %v", data, err)
	}
	checkDownload()

	shortBody.Store(true)
	got := invokeApp(t, host, config, "", false, 1, "get", "/uc/sample.txt", dest+"-short", "--json")
	if got.Error == nil || got.Error.Code != string(core.InvalidUpstreamResponse) || got.Error.NextAction != "" {
		t.Fatalf("incomplete website body: %+v", got.Error)
	}
	if _, err := os.Stat(dest + "-short"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete website body was published: %v", err)
	}
}

func TestQuarkCDPReceipt(t *testing.T) {
	binary, options := websiteFixtureBrowser(t)
	const subject, otherSubject = "fixture%2Fsubject", "another-subject"
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/1/clouddrive/file/") {
			if cookie, err := r.Cookie("sealed-quark"); err != nil || cookie.Value != "fixture" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/1/clouddrive/file/sort":
				q := r.URL.Query()
				size := q.Get("_size")
				if r.Method != http.MethodGet || q.Get("pdir_fid") != "0" || q.Get("_page") != "1" ||
					(size != "1" && size != "50") || q.Get("_fetch_total") != "1" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_, _ = io.WriteString(w, `{"code":0,"metadata":{"_page":1,"_total":1},"data":{"list":[{"fid":"chosen","pdir_fid":"0","file_name":"sample.txt","file":true,"dir":false,"size":6}]}}`)
			case "/1/clouddrive/file/download":
				var request struct{ FIDs []string }
				if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&request) != nil ||
					len(request.FIDs) != 1 || request.FIDs[0] != "chosen" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_, _ = io.WriteString(w, `{"code":0,"data":[{"fid":"chosen","size":6,"download_url":"https://CONTENT.QUARK.CN:443/other/../file?signature=fixture%3F*"}]}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		if r.URL.Path == "/list" {
			http.SetCookie(w, &http.Cookie{Name: "sealed-quark", Value: "fixture", Secure: true, HttpOnly: true, Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "__uid", Value: subject, Secure: true, Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "__uid_sdk", Value: "decoy", Secure: true, Path: "/"})
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><title>Fixture</title>")
	}))
	defer app.Close()
	var wrongRange, stall atomic.Bool
	var contentRequests atomic.Int64
	cancelled := make(chan struct{}, 1)
	content := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentRequests.Add(1)
		cookie, cookieErr := r.Cookie("sealed-quark")
		if r.Method != http.MethodGet || r.URL.Path != "/file" || r.URL.RawQuery != "signature=fixture%3F*" ||
			r.Header.Get("Origin") != app.URL || cookieErr != nil || cookie.Value != "fixture" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", app.URL)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Header.Get("Range") == "bytes=1-4" {
			receipt := "bytes 1-4/6"
			if wrongRange.Load() {
				receipt = "bytes 0-3/6"
			}
			w.Header().Set("Content-Range", receipt)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "ampl")
			_ = http.NewResponseController(w).Flush()
			return
		}
		w.Header().Set("Content-Length", "6")
		if stall.Load() {
			_, _ = io.WriteString(w, "s")
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
			cancelled <- struct{}{}
			return
		}
		_, _ = io.WriteString(w, "sample")
	}))
	defer content.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	profile := filepath.Join(t.TempDir(), "profile")
	if err := website.PrivateDirectory(profile, true); err != nil {
		t.Fatal(err)
	}
	page, err := openWebsitePage(ctx, binary, options, profile, app.URL+"/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := page.Release(); err != nil {
			t.Error(err)
		}
	}()
	rewrite := strings.NewReplacer("https://pan.quark.cn", app.URL, "https://drive-pc.quark.cn", app.URL)
	nativeCall := func(ctx context.Context, script string, data json.RawMessage) (json.RawMessage, error) {
		var input map[string]string
		if json.Unmarshal(data, &input) == nil && input["url"] != "" {
			target, err := url.Parse(input["url"])
			if err != nil {
				return nil, err
			}
			input["url"] = content.URL + target.RequestURI()
			data, _ = json.Marshal(input)
		}
		return page.Call(ctx, rewrite.Replace(script), data)
	}
	account, err := quark.WebsiteAccount(ctx, nativeCall)
	if err != nil {
		t.Fatal(err)
	}
	setSubject := func(ctx context.Context, value string) error {
		data, _ := json.Marshal(value)
		_, err := page.Call(ctx, `(value) => {
			document.cookie = "__uid=" + value + "; Secure; Path=/";
			return "{}";
		}`, data)
		return err
	}
	changeOnGrant := false
	receive := quarkcdp.Transfer(page.Tab)
	transfer := func(ctx context.Context, prepare func(context.Context) (quark.WebsiteDownload, error)) (io.ReadCloser, error) {
		return receive(ctx, func(ctx context.Context) (quark.WebsiteDownload, error) {
			download, err := prepare(ctx)
			if err != nil {
				return download, err
			}
			target, err := url.Parse(download.URL)
			if err != nil || target.Host != "content.quark.cn" {
				return download, errors.New("browser grant authority is not canonical")
			}
			download.URL = content.URL + target.RequestURI()
			if changeOnGrant {
				err = setSubject(ctx, otherSubject)
			}
			return download, err
		})
	}
	driver, err := quarkWebsiteSession(nativeCall, transfer).Mount(ctx, account, "/", "", page.Release)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()
	target := core.NativePath("/sample.txt")
	rangeRequest := core.OpenOptions{Range: &core.Range{Start: 1, End: 4}}
	checkRange := func() {
		t.Helper()
		opened, err := driver.Open(ctx, target, rangeRequest)
		readWebsiteBody(t, opened.Body, err, "ampl")
		if opened.Range == nil || *opened.Range != (core.ContentRange{Start: 1, End: 4, Total: 6}) {
			t.Fatalf("wrong range receipt: %v", opened.Range)
		}
	}
	checkRange()

	before := contentRequests.Load()
	changeOnGrant = true
	opened, err := driver.Open(ctx, target, core.OpenOptions{})
	if opened.Body != nil {
		_ = opened.Body.Close()
	}
	if !errors.Is(err, quark.ErrWebsiteBindingMismatch) || contentRequests.Load() != before {
		t.Fatalf("account change during the grant reached the content service: %v", err)
	}
	changeOnGrant = false
	if err := setSubject(ctx, subject); err != nil {
		t.Fatal(err)
	}

	wrongRange.Store(true)
	opened, err = driver.Open(ctx, target, rangeRequest)
	if opened.Body != nil {
		_ = opened.Body.Close()
	}
	if core.CodeOf(err) != core.InvalidUpstreamResponse {
		t.Fatalf("invalid receipt was not rejected: %v", err)
	}
	wrongRange.Store(false)

	stall.Store(true)
	stream, stop := context.WithCancel(ctx)
	opened, err = driver.Open(stream, target, core.OpenOptions{})
	if err != nil {
		stop()
		t.Fatal(err)
	}
	var first [1]byte
	_, readErr := io.ReadFull(opened.Body, first[:])
	stop()
	if err := errors.Join(readErr, opened.Body.Close()); err != nil || first[0] != 's' {
		t.Fatalf("incomplete transfer: %q: %v", first, err)
	}
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("cancelling the browser transfer left the body active")
	}
	stall.Store(false)
	checkRange()
}
