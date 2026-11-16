package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthorizedDirectoryBatch(t *testing.T) {
	const (
		feishuFiles  = "/open-apis/drive/v1/files"
		feishuExport = "/open-apis/drive/v1/export_tasks"
		fangFolder   = "/api/v2/folder/9007199254740993"
		guangyaFiles = "/openapi/v1/file/"
		kdocsFiles   = "/api/v1/openapi/personal/files"
	)
	var requests, starts, polls, exports int
	var cancelExport context.CancelFunc
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		w := httptest.NewRecorder()
		q, path := r.URL.Query(), r.URL.Path
		auth := r.Header.Get("Authorization")
		check := func(ok bool) {
			if !ok {
				t.Errorf("unexpected scope or authorization for %s %s%s", r.Method, r.URL.Host, path)
			}
		}
		switch r.URL.Host + path {
		case "open.feishu.cn" + feishuFiles:
			check(auth == "Bearer feishu-secret" && q.Get("folder_token") == "folder")
			io.WriteString(w, `{"code":0,"data":{"has_more":false,"files":[{"token":"file","parent_token":"folder","name":"note","type":"file"},{"token":"doc","parent_token":"folder","name":"Online","type":"docx"}]}}`)
		case "open.feishu.cn" + feishuFiles + "/file/download":
			check(auth == "Bearer feishu-secret" && r.Method == "GET")
			io.WriteString(w, "sealed")
		case "open.feishu.cn" + feishuExport:
			starts++
			defer r.Body.Close()
			var input map[string]string
			check(auth == "Bearer feishu-secret" && r.Method == "POST" && json.NewDecoder(r.Body).Decode(&input) == nil &&
				input["token"] == "doc" && input["type"] == "docx" && input["file_extension"] == "pdf")
			io.WriteString(w, `{"code":0,"data":{"ticket":"task"}}`)
		case "open.feishu.cn" + feishuExport + "/task":
			polls++
			check(auth == "Bearer feishu-secret" && q.Get("token") == "doc")
			if cancelExport != nil {
				io.WriteString(w, `{"code":0,"data":{"result":{"job_status":1}}}`)
				cancelExport()
			} else {
				io.WriteString(w, `{"code":0,"data":{"result":{"job_status":0,"token":"doc","type":"docx","file_extension":"pdf","file_token":"pdf","file_size":10}}}`)
			}
		case "open.feishu.cn" + feishuExport + "/file/pdf/download":
			exports++
			check(auth == "Bearer feishu-secret" && r.Method == "GET")
			w.Header().Set("Content-Type", "application/pdf")
			io.WriteString(w, "%PDF-synth")
		case "open.fangcloud.com" + fangFolder + "/info":
			check(auth == "Bearer fang-secret")
			io.WriteString(w, `{"id":9007199254740993,"name":"Work","type":"folder","space":{"type":"personal"}}`)
		case "open.fangcloud.com" + fangFolder + "/children":
			check(auth == "Bearer fang-secret" && q.Get("folder_id") == "9007199254740993")
			io.WriteString(w, `{"files":[{"id":37,"name":"note","type":"file","size":6}],"page_id":0,"page_capacity":20,"page_count":1,"total_count":1}`)
		case "open.fangcloud.com/api/v2/file/37/download_v2":
			check(auth == "Bearer fang-secret")
			io.WriteString(w, `{"download_url":"https://directory-body.fixture/fang?sig=sealed%2Bfang"}`)
		case "openapi.guangyapan.com" + guangyaFiles + "get_file_detail",
			"openapi.guangyapan.com" + guangyaFiles + "get_file_list",
			"openapi.guangyapan.com" + guangyaFiles + "get_res_download_url":
			check(auth == "Bearer guangya-secret" && r.Header.Get("X-Client-ID") == "approved")
			switch strings.TrimPrefix(path, guangyaFiles) {
			case "get_file_detail":
				check(q.Get("fileId") == "folder")
				io.WriteString(w, `{"code":0,"data":{"fileInfo":{"fileId":"folder","resType":2}}}`)
			case "get_file_list":
				check(q.Get("parentId") == "folder")
				io.WriteString(w, `{"code":0,"data":{"total":1,"list":[{"fileId":"file","fileName":"note","parentId":"folder","resType":1,"fileSize":6}]}}`)
			case "get_res_download_url":
				check(q.Get("fileId") == "file")
				io.WriteString(w, `{"code":0,"data":{"signedURL":"https://directory-body.fixture/guangya?sig=sealed%2Bguangya","urlDuration":60}}`)
			}
		case "developer.kdocs.cn" + kdocsFiles:
			check(auth == "" && q.Get("access_token") == "kdocs-secret")
			switch q.Get("parent_id") + "/" + q.Get("offset") + "/" + q.Get("filter") {
			case "//":
				io.WriteString(w, `{"code":0,"data":{"files":[{"id":{"open_id":"folder"},"fname":"Reports","ftype":"folder"}],"next_offset":-1}}`)
			case "folder//":
				io.WriteString(w, `{"code":0,"data":{"files":[],"next_offset":0,"next_filter":"file"}}`)
			case "folder/0/file":
				io.WriteString(w, `{"code":0,"data":{"files":[{"id":{"open_id":"file"},"parent":{"open_id":"folder"},"fname":"note","ftype":"file","fsize":6}],"next_offset":-1}}`)
			default:
				return nil, fmt.Errorf("unexpected KDocs parent or continuation")
			}
		case "developer.kdocs.cn" + kdocsFiles + "/file/download":
			check(auth == "" && q.Get("access_token") == "kdocs-secret")
			io.WriteString(w, `{"code":0,"data":{"fsize":6,"url":"https://directory-body.fixture/kdocs?sig=sealed%2Bkdocs"}}`)
		case "directory-body.fixture/fang", "directory-body.fixture/guangya", "directory-body.fixture/kdocs":
			check(auth == "" && r.Header.Get("X-Client-ID") == "" && r.Header.Get("Cookie") == "" &&
				r.URL.RawQuery == "sig=sealed%2B"+path[1:])
			w.Header().Set("Content-Length", "6")
			io.WriteString(w, "sealed")
		default:
			return nil, fmt.Errorf("request outside the closed directory fixture: %s %s%s", r.Method, r.URL.Host, path)
		}
		resp := w.Result()
		resp.Request = r
		return resp, nil
	})

	config := filepath.Join(t.TempDir(), "config.json")
	for _, source := range []struct{ driver, token, options string }{
		{"feishudrive", "feishu-secret", `{"root_id":"folder"}`},
		{"fangcloud", "fang-secret", `{"root_id":"9007199254740993"}`},
		{"guangyapan", "guangya-secret", `{"client_id":"approved","root_id":"folder"}`},
		{"kdocs", "kdocs-secret", `{"root_path":"/Reports"}`},
	} {
		at, ref := "/"+source.driver, "MWEAVE_TEST_"+strings.ToUpper(source.driver)
		t.Setenv(ref, source.token)
		record := func(options string) string {
			return fmt.Sprintf(`{"at":%q,"driver":%q,"options":%s,"credential":{"kind":"env","ref":%q}}`, at, source.driver, options, ref)
		}
		if source.driver == "feishudrive" {
			invokeCLI(t, config, "feishudrive\nfolder\nenv:"+ref+"\ny\n", true, 0, "mount", "add", at)
		} else {
			invokeCLI(t, config, record(source.options), false, 0, "mount", "add", "--from", "-", "--json")
		}
		listed := invokeCLI(t, config, "", false, 0, "ls", at, "--json")
		if !bytes.Contains(listed.Result, []byte(`"name":"note"`)) || bytes.Contains(listed.Result, []byte("Online")) {
			t.Fatalf("list %s: %s", at, listed.Result)
		}
		assertSourceBody(t, config, at+"/note", "sealed")
		switch source.driver {
		case "feishudrive":
			invokeCLI(t, config, record(`{"root_id":"folder","export_formats":{"docx":"pdf"}}`), false, 0, "mount", "edit", at, "--from", "-", "--json")
			invokeCLI(t, config, "", false, 0, "stat", at+"/Online", "--json")
			if starts != 0 {
				t.Fatal("browsing or stat started an export")
			}
			assertSourceBody(t, config, at+"/Online", "%PDF-synth")
		case "guangyapan":
			before := requests
			got := invokeCLI(t, config, record(`{"client_id":"different","root_id":"folder"}`), false, 1, "mount", "edit", at, "--from", "-", "--json")
			if got.Error == nil || got.Error.Code != "credential_scope_conflict" || requests != before {
				t.Fatalf("credential crossed the configured app: %+v", got.Error)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelExport = cancel
	dir := t.TempDir()
	var out, diagnostics bytes.Buffer
	code := testApp.Run(ctx, []string{"--config", config, "--timeout", "5s", "get", "/feishudrive/Online", filepath.Join(dir, "Online.pdf"), "--json"},
		strings.NewReader(""), &out, &diagnostics, false)
	files, err := os.ReadDir(dir)
	if code != 1 || !bytes.Contains(out.Bytes(), []byte(`"code":"cancelled"`)) || starts != 2 || polls != 2 || exports != 1 || err != nil || len(files) != 0 {
		t.Fatalf("cancelled export: exit=%d result=%s diagnostics=%s starts=%d polls=%d downloads=%d files=%v err=%v",
			code, &out, &diagnostics, starts, polls, exports, files, err)
	}
}

func TestBoxAndTencentDocs(t *testing.T) {
	const docs = "/openapi/drive/v2"
	const doc = docs + "/files/document"
	var requests, submissions, polls, exports int
	var cancelExport context.CancelFunc
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		w := httptest.NewRecorder()
		q, path := r.URL.Query(), r.URL.Path
		auth := r.Header.Get("Authorization")
		check := func(ok bool) {
			if !ok {
				t.Errorf("unexpected identity, pagination or credentials at %s %s%s", r.Method, r.URL.Host, path)
			}
		}
		docsToken, app, user := r.Header.Get("Access-Token"), r.Header.Get("Client-Id"), r.Header.Get("Open-Id")
		check(r.URL.Scheme == "https" && r.Header.Get("Cookie") == "")
		switch r.URL.Host + path {
		case "api.box.com/2.0/folders/0", "api.box.com/2.0/folders/7":
			check(auth == "Bearer box-secret")
			fmt.Fprintf(w, `{"id":%q,"type":"folder"}`, path[len(path)-1:])
		case "api.box.com/2.0/folders/0/items", "api.box.com/2.0/folders/7/items":
			check(auth == "Bearer box-secret" && q.Get("usemarker") == "true")
			if q.Get("marker") == "" {
				io.WriteString(w, `{"entries":[],"next_marker":"a+/="}`)
			} else {
				check(q.Get("marker") == "a+/=")
				parent, name := "0", "root-only"
				if strings.Contains(path, "/7/") {
					parent, name = "7", "note"
				}
				fmt.Fprintf(w, `{"entries":[{"type":"file","id":"16","name":%q,"size":6,"parent":{"type":"folder","id":%q}}],"next_marker":null}`, name, parent)
			}
		case "api.box.com/2.0/files/16/content":
			check(auth == "Bearer box-secret")
			w.Header().Set("Location", "https://api.box.com/delegated?sig=box%2B")
			w.WriteHeader(http.StatusFound)
		case "api.box.com/delegated", "docs.qq.com/delegated":
			check(auth == "" && docsToken == "" && app == "" && user == "")
			provider := "box"
			if r.URL.Host == "docs.qq.com" {
				provider = "docs"
			}
			check(r.URL.RawQuery == "sig="+provider+"%2B")
			w.Header().Set("Location", "https://document-body.fixture/"+provider)
			w.WriteHeader(http.StatusFound)
		case "document-body.fixture/box", "document-body.fixture/docs":
			check(auth == "" && docsToken == "" && app == "" && user == "")
			if path == "/docs" {
				exports++
			}
			w.Header().Set("Content-Length", "6")
			io.WriteString(w, "sealed")
		case "docs.qq.com" + docs + "/folders/folder/metadata", "docs.qq.com" + docs + "/folders/folder",
			"docs.qq.com" + doc + "/async-export", "docs.qq.com" + doc + "/export-progress":
			check(auth == "" && docsToken == "docs-secret" && app == "approved" && user == "owner")
			switch path {
			case docs + "/folders/folder/metadata":
				io.WriteString(w, `{"ret":0,"data":{"folderInfo":{"ID":"folder","status":"normal"}}}`)
			case docs + "/folders/folder":
				check(q.Get("start") == "0" && q.Get("limit") == "20")
				io.WriteString(w, `{"ret":0,"data":{"next":0,"list":[{"ID":"document","title":"Report","type":"doc","status":"normal"}]}}`)
			case doc + "/async-export":
				submissions++
				check(r.Method == "POST" && r.GetBody == nil && r.ContentLength == 0)
				io.WriteString(w, `{"ret":0,"data":{"operationID":"op+/="}}`)
			case doc + "/export-progress":
				polls++
				check(r.Method == "GET" && q.Get("operationID") == "op+/=")
				if cancelExport != nil {
					io.WriteString(w, `{"ret":0,"data":{"progress":42}}`)
					cancelExport()
				} else {
					io.WriteString(w, `{"ret":0,"data":{"progress":100,"url":"https://docs.qq.com/delegated?sig=docs%2B"}}`)
				}
			}
		default:
			return nil, fmt.Errorf("unexpected closed document fixture request at %s %s", r.URL.Host, path)
		}
		res := w.Result()
		res.Request = r
		return res, nil
	})

	t.Setenv("MWEAVE_TEST_BOX", "box-secret")
	t.Setenv("MWEAVE_TEST_TENCENT_DOCS", "docs-secret")
	cfg := filepath.Join(t.TempDir(), "config.json")
	invokeCLI(t, cfg, "box\n\nenv:MWEAVE_TEST_BOX\ny\n", true, 0, "mount", "add", "/box")
	if got := invokeCLI(t, cfg, "", false, 0, "ls", "/box", "--json"); !bytes.Contains(got.Result, []byte(`"name":"root-only"`)) {
		t.Fatalf("Box account root pagination: %s", got.Result)
	}
	invokeCLI(t, cfg, `{"at":"/box","driver":"box","options":{"root_id":"7"}}`, false, 0, "mount", "edit", "/box", "--from", "-")
	assertSourceBody(t, cfg, "/box/note", "sealed")

	const record = `{"at":"/docs","driver":"tencentdocs","options":{"root_id":"folder","client_id":"approved","open_id":"owner"},"credential":{"kind":"env","ref":"MWEAVE_TEST_TENCENT_DOCS"}}`
	invokeCLI(t, cfg, record, false, 0, "mount", "add", "--from", "-")
	before := requests
	for _, id := range []string{`"approved"`, `"owner"`} {
		changed := strings.Replace(record, id, `"different"`, 1)
		got := invokeCLI(t, cfg, changed, false, 1, "mount", "edit", "/docs", "--from", "-", "--json")
		if got.Error == nil || got.Error.Code != "credential_scope_conflict" || requests != before {
			t.Fatalf("Tencent Docs credential crossed its configured identity: %+v", got.Error)
		}
	}
	dest := t.TempDir()
	file := filepath.Join(dest, "Report.docx")
	if got := invokeCLI(t, cfg, "", false, 1, "get", "/docs/Report", file, "--json"); got.Error == nil ||
		got.Error.Code != "unsupported_operation" || submissions != 0 {
		t.Fatalf("disabled export: %+v, submissions=%d", got.Error, submissions)
	}
	enabled := strings.Replace(record, `"open_id":"owner"`, `"open_id":"owner","allow_export":true`, 1)
	invokeCLI(t, cfg, enabled, false, 0, "mount", "edit", "/docs", "--from", "-")
	invokeCLI(t, cfg, "", false, 0, "ls", "/docs", "--json")
	invokeCLI(t, cfg, "", false, 0, "stat", "/docs/Report", "--json")
	if submissions != 0 {
		t.Fatal("browsing or stat started a Tencent Docs export")
	}
	assertSourceBody(t, cfg, "/docs/Report", "sealed")
	if submissions != 1 || polls != 1 || exports != 1 {
		t.Fatalf("successful export submitted %d, polled %d, downloaded %d", submissions, polls, exports)
	}
	localDir := t.TempDir()
	localRoot, _ := json.Marshal(localDir)
	invokeCLI(t, cfg, fmt.Sprintf(`{"at":"/local","driver":"local","options":{"root":%s}}`, localRoot), false, 0, "mount", "add", "--from", "-")
	invokeCLI(t, cfg, "", false, 0, "cp", "/docs/Report", "/local/Report.docx")
	if body, err := os.ReadFile(filepath.Join(localDir, "Report.docx")); err != nil || string(body) != "sealed" || submissions != 2 {
		t.Fatalf("copied export: %q, %v, submissions=%d", body, err, submissions)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelExport = cancel
	var out, diag bytes.Buffer
	code := testApp.Run(ctx, []string{"--config", cfg, "--timeout", "5s", "get", "/docs/Report", file, "--json"}, strings.NewReader(""), &out, &diag, false)
	files, err := os.ReadDir(dest)
	if code != 1 || !bytes.Contains(out.Bytes(), []byte(`"code":"cancelled"`)) ||
		!bytes.Contains(out.Bytes(), []byte(`"effect":"unchanged"`)) || submissions != 3 || polls != 3 || exports != 2 || err != nil || len(files) != 0 {
		t.Fatalf("cancelled export: exit=%d result=%s diagnostics=%s submissions=%d polls=%d downloads=%d files=%v err=%v",
			code, &out, &diag, submissions, polls, exports, files, err)
	}
}
