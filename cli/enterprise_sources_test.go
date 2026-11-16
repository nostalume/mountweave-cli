package cli

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/nostalume/mountweave"
)

func TestEnterpriseSpaceBatch(t *testing.T) {
	const (
		dingRoot   = "/v1.0/storage/spaces/space/dentries"
		dingHeader = "X-Acs-Dingtalk-Access-Token"
		wecomRoot  = "/cgi-bin/wedrive/"
		smhRoot    = "/api/v1/directory/library/space/Reports"
		gokuaiFile = `{"hash":"file","filehash":"version","fullpath":"Reports/note","filename":"note","dir":0,"filesize":6}`
	)
	var requests int
	var cancelled *cancelledDownload
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		w := httptest.NewRecorder()
		q, path := r.URL.Query(), r.URL.Path
		check := func(ok bool) {
			if !ok {
				t.Errorf("unexpected scope or authorization for %s %s%s", r.Method, r.URL.Host, path)
			}
		}
		switch r.URL.Host {
		case "yk3.gokuai.com":
			defer r.Body.Close()
			check(r.Method == "POST" && r.ParseForm() == nil && r.Form.Get("org_client_id") == "library")
			p := r.Form
			stamp, fullPath := p.Get("dateline"), p.Get("fullpath")
			switch path {
			case "/m-open/1/file/info":
				mac := hmac.New(sha1.New, []byte("gokuai-secret"))
				fmt.Fprintf(mac, "10\n%s\n%s\nlibrary", stamp, fullPath)
				check(p.Get("sign") == base64.StdEncoding.EncodeToString(mac.Sum(nil)))
				if fullPath == "Reports" {
					io.WriteString(w, `{"hash":"folder","fullpath":"Reports","filename":"Reports","dir":1}`)
				} else {
					check(fullPath == "Reports/note")
					io.WriteString(w, gokuaiFile)
				}
			case "/m-open/1/file/ls":
				check(fullPath == "Reports" && p.Get("start") == "0")
				fmt.Fprintf(w, `{"count":1,"list":[%s]}`, gokuaiFile)
			case "/m-open/1/file/download_url":
				check(fullPath == "Reports/note" && p.Get("filehash") == "version")
				io.WriteString(w, `{"urls":["https://enterprise-body.fixture/gokuai"]}`)
			default:
				return nil, fmt.Errorf("unexpected Gokuai route %s", path)
			}
		case "api.dingtalk.com":
			check(r.Header.Get(dingHeader) == "ding-token" && r.Header.Get("Authorization") == "" && q.Get("unionId") == "member")
			if r.Body != nil {
				defer r.Body.Close()
			}
			switch path {
			case dingRoot + "/folder/query":
				io.WriteString(w, `{"dentry":{"spaceId":"space","id":"folder","name":"Reports","type":"FOLDER"}}`)
			case dingRoot:
				check(q.Get("parentId") == "folder")
				switch q.Get("nextToken") {
				case "":
					io.WriteString(w, `{"dentries":[],"nextToken":"next+?"}`)
				case "next+?":
					io.WriteString(w, `{"dentries":[{"spaceId":"space","id":"file","parentId":"folder","name":"note","type":"FILE","size":6}]}`)
				default:
					return nil, fmt.Errorf("unexpected DingTalk continuation")
				}
			case dingRoot + "/file/downloadInfos/query":
				io.WriteString(w, `{"protocol":"HEADER_SIGNATURE","headerSignatureInfo":{"resourceUrls":["https://enterprise-body.fixture/ding"],"headers":{"Authorization":"OSS scoped"}}}`)
			default:
				return nil, fmt.Errorf("unexpected DingTalk route %s", path)
			}
		case "qyapi.weixin.qq.com":
			defer r.Body.Close()
			var input struct {
				ID     string `json:"fileid"`
				Space  string `json:"spaceid"`
				Parent string `json:"fatherid"`
			}
			check(r.Method == "POST" && q.Get("access_token") == "wecom-token" && json.NewDecoder(r.Body).Decode(&input) == nil)
			switch path {
			case wecomRoot + "file_info":
				check(input.ID == "folder")
				io.WriteString(w, `{"errcode":0,"file_info":{"fileid":"folder","spaceid":"space","file_name":"Reports","file_type":1,"file_status":1}}`)
			case wecomRoot + "file_list":
				check(input.Space == "space" && input.Parent == "folder")
				io.WriteString(w, `{"errcode":0,"has_more":false,"file_list":{"item":[{"fileid":"file","fatherid":"folder","spaceid":"space","file_name":"note","file_type":2,"file_status":1,"file_size":6}]}}`)
			case wecomRoot + "file_download":
				check(input.ID == "file")
				io.WriteString(w, `{"errcode":0,"download_url":"https://enterprise-body.fixture/wecom","cookie_name":"receipt","cookie_value":"scoped"}`)
			default:
				return nil, fmt.Errorf("unexpected WeCom route %s", path)
			}
		case "smh.fixture":
			check(r.Method == "GET" && q.Get("access_token") == "smh-token")
			switch path {
			case smhRoot:
				if q.Get("info") == "1" {
					io.WriteString(w, `{"name":"Reports","path":["Reports"],"type":"dir"}`)
				} else {
					check(q.Get("by-marker") == "1")
					io.WriteString(w, `{"path":["Reports"],"contents":[{"name":"note","type":"file","size":"6"}]}`)
				}
			case smhRoot + "/note":
				io.WriteString(w, `{"name":"note","path":["Reports","note"],"type":"file","size":"6","contentCas":"version"}`)
			case "/api/v1/file/library/space/Reports/note":
				check(q.Get("content_cas") == "version")
				io.WriteString(w, `{"type":"file","size":"6","contentCas":"version","cosUrl":"https://enterprise-body.fixture/smh"}`)
			default:
				return nil, fmt.Errorf("unexpected SMH route %s", path)
			}
		case "enterprise-body.fixture", "enterprise-cdn.fixture":
			auth, cookie := "", ""
			switch path {
			case "/ding":
				auth = "OSS scoped"
			case "/wecom":
				cookie = "receipt=scoped"
			}
			check(r.Method == "GET" && r.URL.Scheme == "https" && !q.Has("access_token") &&
				r.Header.Get(dingHeader) == "" && r.Header.Get("Authorization") == auth && r.Header.Get("Cookie") == cookie)
			switch path {
			case "/ding", "/wecom":
				w.Header().Set("Location", "https://enterprise-cdn.fixture"+path+"-body")
				w.WriteHeader(http.StatusFound)
			case "/ding-body", "/wecom-body", "/gokuai", "/smh":
				if path == "/smh" && cancelled != nil {
					cancelled.ctx = r.Context()
					return &http.Response{Request: r, StatusCode: 200, ContentLength: 6, Header: make(http.Header), Body: cancelled}, nil
				}
				w.Header().Set("Content-Length", "6")
				io.WriteString(w, "sealed")
			default:
				return nil, fmt.Errorf("unexpected content route %s", path)
			}
		default:
			return nil, fmt.Errorf("request outside the closed enterprise fixture: %s", r.URL.Host)
		}
		res := w.Result()
		res.Request = r
		return res, nil
	})

	config := filepath.Join(t.TempDir(), "config.json")
	for _, source := range []struct{ driver, token, options, changed string }{
		{"gokuai", "gokuai-secret", `{"library_id":"library","root_path":"/Reports"}`, `{"library_id":"other","root_path":"/Reports"}`},
		{"dingtalkdrive", "ding-token", `{"space_id":"space","root_id":"folder","operator_union_id":"member"}`, ""},
		{"wecomdrive", "wecom-token", `{"space_id":"space","root_id":"folder"}`, ""},
		{"tencentsmh", "smh-token", `{"library_id":"library","space_id":"space","root_path":"/Reports","endpoint":"https://smh.fixture"}`,
			`{"library_id":"library","space_id":"other","root_path":"/Reports","endpoint":"https://smh.fixture"}`},
	} {
		at, ref := "/"+source.driver, "MWEAVE_TEST_"+strings.ToUpper(source.driver)
		t.Setenv(ref, source.token)
		record := func(at, options string) string {
			return fmt.Sprintf(`{"at":%q,"driver":%q,"options":%s,"credential":{"kind":"env","ref":%q}}`, at, source.driver, options, ref)
		}
		if source.driver == "dingtalkdrive" {
			invokeCLI(t, config, "dingtalkdrive\nspace\nfolder\nmember\nenv:"+ref+"\ny\n", true, 0, "mount", "add", at)
		} else {
			invokeCLI(t, config, record(at, source.options), false, 0, "mount", "add", "--from", "-", "--json")
		}
		if source.changed != "" {
			before := requests
			got := invokeCLI(t, config, record(at, source.changed), false, 1, "mount", "edit", at, "--from", "-", "--json")
			if got.Error == nil || got.Error.Code != "credential_scope_conflict" || requests != before {
				t.Fatalf("credential crossed the configured library or space: %+v", got.Error)
			}
		}
		if source.driver == "wecomdrive" {
			wrong := `{"space_id":"wrong","root_id":"folder"}`
			got := invokeCLI(t, config, record("/wrong", wrong), false, 1, "mount", "add", "--from", "-", "--json")
			if got.Error == nil || got.Error.Code != string(core.InvalidUpstreamResponse) {
				t.Fatalf("folder admitted under a different space: %+v", got.Error)
			}
		}
		if got := invokeCLI(t, config, "", false, 0, "ls", at, "--json"); !bytes.Contains(got.Result, []byte(`"name":"note"`)) {
			t.Fatalf("list %s: %s", at, got.Result)
		}
		assertSourceBody(t, config, at+"/note", "sealed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cancelled = &cancelledDownload{cancel: cancel}
	dest := t.TempDir()
	var out, diagnostics bytes.Buffer
	code := testApp.Run(ctx, []string{"--config", config, "get", "/tencentsmh/note", filepath.Join(dest, "note"), "--json"},
		strings.NewReader(""), &out, &diagnostics, false)
	entries, err := os.ReadDir(dest)
	if code != 1 || !bytes.Contains(out.Bytes(), []byte(`"code":"cancelled"`)) ||
		!bytes.Contains(out.Bytes(), []byte(`"effect":"unchanged"`)) || cancelled.closes.Load() != 1 || err != nil || len(entries) != 0 {
		t.Fatalf("cancelled download: exit=%d result=%s diagnostics=%s closes=%d files=%v err=%v",
			code, &out, &diagnostics, cancelled.closes.Load(), entries, err)
	}
}

type cancelledDownload struct {
	ctx    context.Context
	cancel context.CancelFunc
	closes atomic.Int32
}

func (b *cancelledDownload) Read([]byte) (int, error) {
	b.cancel()
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *cancelledDownload) Close() error {
	b.closes.Add(1)
	return nil
}
