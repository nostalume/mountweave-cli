package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloudSourceBatch(t *testing.T) {
	const vault = "cloudreve://my/vault"
	const googleDoc = "application/vnd.google-apps.document"
	const library = "01234567-89ab-cdef-0123-456789abcdef"
	const seafileAPI = "/team-a/api2/repos/" + library + "/"
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	requests := 0
	http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		w := httptest.NewRecorder()
		q := r.URL.Query()
		auth := r.Header.Get("Authorization")
		check := func(ok bool) {
			if !ok {
				t.Errorf("unexpected authorization or root for %s %s%s", r.Method, r.URL.Host, r.URL.Path)
			}
		}
		body := func() {
			w.Header().Set("Content-Length", "6")
			io.WriteString(w, "sealed")
		}
		switch r.URL.Host + r.URL.Path {
		case "www.googleapis.com/drive/v3/files/vault":
			check(auth == "Bearer google-token")
			io.WriteString(w, `{"id":"vault","name":"Archive","mimeType":"application/vnd.google-apps.folder","driveId":"team-drive"}`)
		case "www.googleapis.com/drive/v3/about":
			check(auth == "Bearer google-token" && q.Get("fields") == "exportFormats")
			fmt.Fprintf(w, `{"exportFormats":{%q:["application/pdf"]}}`, googleDoc)
		case "www.googleapis.com/drive/v3/files":
			check(auth == "Bearer google-token" && strings.Contains(q.Get("q"), "'vault' in parents") &&
				q.Get("corpora") == "drive" && q.Get("driveId") == "team-drive")
			fmt.Fprintf(w, `{"files":[{"id":"doc","name":"Document","mimeType":%q,"parents":["vault"],"driveId":"team-drive"}]}`, googleDoc)
		case "www.googleapis.com/drive/v3/files/doc/export":
			check(auth == "Bearer google-token" && q.Get("mimeType") == "application/pdf")
			body()
		case "api.dropboxapi.com/2/files/get_metadata", "api.dropboxapi.com/2/files/list_folder",
			"content.dropboxapi.com/2/files/download", "content.dropboxapi.com/2/files/export":
			check(auth == "Bearer dropbox-token" &&
				r.Header.Get("Dropbox-API-Path-Root") == `{".tag":"namespace_id","namespace_id":"shared-ns"}`)
			var arg struct{ Path string }
			switch r.URL.Path {
			case "/2/files/get_metadata", "/2/files/list_folder":
				defer r.Body.Close()
				check(json.NewDecoder(r.Body).Decode(&arg) == nil)
				if r.URL.Path == "/2/files/get_metadata" {
					check(arg.Path == "/Archive")
					io.WriteString(w, `{".tag":"folder","id":"id:archive","name":"Archive"}`)
				} else {
					check(arg.Path == "id:archive")
					io.WriteString(w, `{"entries":[{".tag":"file","id":"id:doc","name":"Doc.paper","size":6}],"cursor":"cursor","has_more":false}`)
				}
			case "/2/files/download", "/2/files/export":
				check(json.Unmarshal([]byte(q.Get("arg")), &arg) == nil && arg.Path == "id:archive/Doc.paper")
				if r.URL.Path == "/2/files/download" {
					w.WriteHeader(http.StatusConflict)
					io.WriteString(w, `{"error_summary":"unsupported_file/..","error":{".tag":"unsupported_file"}}`)
				} else {
					check(!strings.Contains(q.Get("arg"), "export_format"))
					w.Header().Set("Dropbox-API-Result", `{"file_metadata":{"id":"id:doc","name":"Doc.paper","size":6},"export_metadata":{"name":"Doc.html","size":6}}`)
					body()
				}
			}
		case "cloudreve.fixture/api/v4/file/info", "cloudreve.fixture/api/v4/file", "cloudreve.fixture/api/v4/file/url":
			check(auth == "Bearer cloudreve-token" && r.URL.Scheme == "http")
			const folder = `{"id":"vault","name":"vault","path":"` + vault + `","type":1,"size":0}`
			const note = `{"id":"note","name":"note","path":"` + vault + `/note","type":0,"size":6}`
			switch r.URL.Path {
			case "/api/v4/file/info":
				item := folder
				if q.Get("uri") == vault+"/note" {
					item = note
				} else {
					check(q.Get("uri") == vault)
				}
				fmt.Fprintf(w, `{"code":0,"data":%s}`, item)
			case "/api/v4/file":
				check(q.Get("uri") == vault)
				fmt.Fprintf(w, `{"code":0,"data":{"files":[%s],"parent":%s,"pagination":{"is_cursor":true}}}`, note, folder)
			case "/api/v4/file/url":
				defer r.Body.Close()
				var arg struct{ URIs []string }
				check(json.NewDecoder(r.Body).Decode(&arg) == nil && len(arg.URIs) == 1 && arg.URIs[0] == vault+"/note")
				io.WriteString(w, `{"code":0,"data":{"urls":[{"url":"https://cloud-content.fixture/note?cap=cloudreve"}]}}`)
			}
		case "seafile.fixture" + seafileAPI:
			check(auth == "Token seafile-token")
			fmt.Fprintf(w, `{"id":%q,"encrypted":false}`, library)
		case "seafile.fixture/team-a/api/v2.1/repos/" + library + "/dir/detail/":
			check(auth == "Token seafile-token" && q.Get("path") == "/archive")
			fmt.Fprintf(w, `{"repo_id":%q,"path":"/archive/","name":"archive"}`, library)
		case "seafile.fixture" + seafileAPI + "dir/":
			check(auth == "Token seafile-token" && q.Get("p") == "/archive")
			io.WriteString(w, `[{"name":"note","type":"file","size":6,"parent_dir":"/archive"}]`)
		case "seafile.fixture" + seafileAPI + "file/":
			check(auth == "Token seafile-token" && q.Get("p") == "/archive/note")
			io.WriteString(w, `"https://cloud-content.fixture/note?cap=seafile"`)
		case "cloud-content.fixture/note":
			check(auth == "" && r.Header.Get("Cookie") == "" && (q.Get("cap") == "cloudreve" || q.Get("cap") == "seafile"))
			body()
		default:
			return nil, fmt.Errorf("request outside the closed cloud fixture: %s %s%s", r.Method, r.URL.Host, r.URL.Path)
		}
		res := w.Result()
		res.Request = r
		return res, nil
	})

	config := filepath.Join(t.TempDir(), "config.json")
	for _, source := range []struct{ at, driver, token, options, name string }{
		{"/google", "googledrive", "google-token", `{"root_id":"vault","export_formats":{"application/vnd.google-apps.document":"application/pdf"}}`, "Document"},
		{"/dropbox", "dropbox", "dropbox-token", `{"root":"namespace","namespace_id":"shared-ns","root_path":"/Archive","export_format":""}`, "Doc.paper"},
		{"/cloudreve", "cloudreve", "cloudreve-token", `{"endpoint":"http://cloudreve.fixture","root_uri":"cloudreve://my/vault"}`, "note"},
		{"/seafile", "seafile", "seafile-token", `{"server_url":"https://seafile.fixture/team-a","library_id":"` + library + `","root_path":"/archive"}`, "note"},
	} {
		ref := "MWEAVE_TEST_" + strings.ToUpper(source.driver)
		t.Setenv(ref, source.token)
		record := func(options string) string {
			return fmt.Sprintf(`{"at":%q,"driver":%q,"options":%s,"credential":{"kind":"env","ref":%q}}`,
				source.at, source.driver, options, ref)
		}
		if source.driver == "googledrive" {
			choice := ""
			if managedStoreSupported {
				choice = "reference\n"
			}
			invokeCLI(t, config, "googledrive\n"+choice+"vault\nenv:"+ref+"\ny\n", true, 0, "mount", "add", source.at)
			invokeCLI(t, config, record(source.options), false, 0, "mount", "edit", source.at, "--from", "-", "--json")
		} else {
			invokeCLI(t, config, record(source.options), false, 0, "mount", "add", "--from", "-", "--json")
		}
		listed := invokeCLI(t, config, "", false, 0, "ls", source.at, "--json")
		if !bytes.Contains(listed.Result, []byte(source.name)) {
			t.Fatalf("listing %s: %s", source.at, listed.Result)
		}
		assertSourceBody(t, config, source.at+"/"+source.name, "sealed")
		if source.driver == "seafile" {
			before := requests
			changed := strings.Replace(source.options, "/team-a", "/team-b", 1)
			got := invokeCLI(t, config, record(changed), false, 1, "mount", "edit", source.at, "--from", "-", "--json")
			if got.Error == nil || got.Error.Code != "credential_scope_conflict" || requests != before {
				t.Fatalf("changing the Seafile installation sent a request or lost the audience conflict: %+v", got.Error)
			}
		}
	}
}
