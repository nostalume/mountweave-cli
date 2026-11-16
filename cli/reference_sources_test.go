package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestReferenceSources(t *testing.T) {
	const (
		repo   = "/repos/team/project"
		atom   = "/api/v5" + repo
		cnb    = "/org/team/project/-/releases"
		arts   = "/v4/repositories/42/repository"
		kubo   = "/proxy/api/v0/"
		cid    = "bafybeigqjv4b7qkxb4xgpyklwqzrzprjdumw6fx5q6l5sjgkmltih4eywe"
		commit = "f1edcba9876543210fedcba9876543210fedcba9"
		blob   = "f2edcba9876543210fedcba9876543210fedcba9"
		tree   = "f3edcba9876543210fedcba9876543210fedcba9"
	)
	var requests, gitRefs, releaseRefs, cnbRefs int
	var requireGitToken bool
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		w := httptest.NewRecorder()
		route := r.URL.Host + r.URL.Path
		q := r.URL.Query()
		auth := r.Header.Get("Authorization")
		check := func(ok bool) {
			if !ok {
				t.Errorf("unexpected identity or credentials for %s %s", r.Method, route)
			}
		}
		check(r.URL.Scheme == "https" && r.Header.Get("Cookie") == "")
		switch route {
		case "api.github.com" + repo + "/commits/heads/main",
			"api.github.com" + repo + "/git/trees/" + tree,
			"api.github.com" + repo + "/git/blobs/" + blob:
			if requireGitToken && auth == "" {
				w.WriteHeader(http.StatusNotFound)
				break
			}
			want := ""
			if requireGitToken {
				want = "Bearer git-secret"
			}
			check(auth == want)
			switch r.URL.Path {
			case repo + "/commits/heads/main":
				gitRefs++
				check(strings.HasSuffix(r.URL.EscapedPath(), "/heads%2Fmain"))
				fmt.Fprintf(w, `{"sha":%q,"commit":{"tree":{"sha":%q}}}`, commit, tree)
			case repo + "/git/trees/" + tree:
				fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[{"path":"note","mode":"100644","type":"blob","sha":%q,"size":6}]}`, tree, blob)
			case repo + "/git/blobs/" + blob:
				check(r.Header.Get("Accept") == "application/vnd.github.raw+json")
				io.WriteString(w, "sealed")
			}
		case "api.atomgit.com" + atom + "/commits/main":
			check(auth == "Bearer atom-secret")
			fmt.Fprintf(w, `{"sha":%q}`, commit)
		case "api.atomgit.com" + atom + "/git/trees/" + commit:
			check(auth == "Bearer atom-secret" && q.Get("file_path") == "")
			if q.Get("page") == "1" {
				fmt.Fprintf(w, `{"tree":[{"sha":%q,"path":"note","name":"note","type":"blob","mode":"100644"}]}`, blob)
			} else {
				check(q.Get("page") == "2")
				io.WriteString(w, `{"tree":[]}`)
			}
		case "api.atomgit.com" + atom + "/raw/note":
			check(auth == "Bearer atom-secret" && q.Get("ref") == commit)
			io.WriteString(w, "sealed")
		case "api.github.com" + repo + "/releases/tags/v1":
			releaseRefs++
			check(auth == "Bearer git-secret")
			io.WriteString(w, `{"id":17,"tag_name":"v1","draft":false}`)
		case "api.github.com" + repo + "/releases/17/assets":
			check(auth == "Bearer git-secret" && q.Get("page") == "1")
			io.WriteString(w, `[{"id":99,"name":"note","state":"uploaded","size":6}]`)
		case "api.github.com" + repo + "/releases/assets/99":
			check(auth == "Bearer git-secret" && r.Header.Get("Accept") == "application/octet-stream")
			w.Header().Set("Location", "https://release-body.fixture/github")
			w.WriteHeader(http.StatusFound)
		case "api.cnb.cool" + cnb + "/tags/v1":
			cnbRefs++
			check(auth == "Bearer cnb-secret")
			fmt.Fprintf(w, `{"id":"release-%d","tag_name":"v1","draft":false,"assets":[{"id":"asset","name":"note","size":6}]}`, cnbRefs)
		case "api.cnb.cool" + cnb + "/download/v1/note":
			check(auth == "Bearer cnb-secret")
			w.Header().Set("Location", "https://release-body.fixture/cnb")
			w.WriteHeader(http.StatusFound)
		case "release-body.fixture/github", "release-body.fixture/cnb":
			check(auth == "")
			io.WriteString(w, "sealed")
		case "codehub-ext.cn-north-4.myhuaweicloud.com" + arts + "/commits",
			"codehub-ext.cn-north-4.myhuaweicloud.com" + arts + "/trees",
			"codehub-ext.cn-north-4.myhuaweicloud.com" + arts + "/blobs/leaf/raw":
			check(auth == "" && r.Header.Get("X-Auth-Token") == "iam-secret")
			switch r.URL.Path {
			case arts + "/commits":
				check(q.Get("sha") == commit)
				fmt.Fprintf(w, `{"id":%q}`, commit)
			case arts + "/trees":
				check(q.Get("ref") == commit)
				w.Header().Set("X-Total", "1")
				io.WriteString(w, `[{"id":"leaf","name":"note","path":"note","type":"blob","mode":"100644"}]`)
			case arts + "/blobs/leaf/raw":
				check(q.Get("file_path") == "note")
				io.WriteString(w, "sealed")
			}
		case "kubo.fixture" + kubo + "files/stat", "kubo.fixture" + kubo + "ls", "kubo.fixture" + kubo + "cat":
			check(auth == "Bearer kubo-secret" && r.Method == "POST")
			switch r.URL.Path {
			case kubo + "files/stat":
				check(q.Get("arg") == "/ipfs/"+cid)
				fmt.Fprintf(w, `{"Hash":%q,"Type":"directory"}`, cid)
			case kubo + "ls":
				check(q.Get("arg") == cid)
				fmt.Fprintf(w, `{"Objects":[{"Hash":%q,"Links":[{"Hash":"file-cid","Name":"note","Type":2,"Size":6}]}]}`, cid)
			case kubo + "cat":
				check(q.Get("arg") == "/ipfs/file-cid")
				w.Header().Set("X-Content-Length", "6")
				w.Header().Set("Content-Length", "6")
				io.WriteString(w, "sealed")
			}
		default:
			return nil, fmt.Errorf("unexpected reference fixture route %s", route)
		}
		resp := w.Result()
		resp.Request = r
		return resp, nil
	})

	t.Setenv("MWEAVE_TEST_ATOM", "atom-secret")
	t.Setenv("MWEAVE_TEST_CNB", "cnb-secret")
	t.Setenv("MWEAVE_TEST_IAM", "iam-secret")
	t.Setenv("MWEAVE_TEST_KUBO", "kubo-secret")
	t.Setenv("MWEAVE_TEST_GIT", "git-secret")
	cfg := filepath.Join(t.TempDir(), "config.json")
	const github = `{"at":"/git","driver":"githubrepo","options":{"repository":"team/project","ref":"heads/main"}`
	for _, record := range []string{
		github + "}",
		`{"at":"/atom","driver":"atomgit","options":{"repository":"team/project","ref":"main"},"credential":{"kind":"env","ref":"MWEAVE_TEST_ATOM"}}`,
		`{"at":"/release","driver":"githubreleases","options":{"repository":"team/project","tag":"v1"},"credential":{"kind":"env","ref":"MWEAVE_TEST_GIT"}}`,
		`{"at":"/cnb","driver":"cnbreleases","options":{"repository":"org/team/project","tag":"v1"},"credential":{"kind":"env","ref":"MWEAVE_TEST_CNB"}}`,
		`{"at":"/arts","driver":"codeartsrepo","options":{"endpoint":"https://codehub-ext.cn-north-4.myhuaweicloud.com","repository_id":42,"commit_sha":"` + commit + `"},"credential":{"kind":"env","ref":"MWEAVE_TEST_IAM"}}`,
		`{"at":"/ipfs","driver":"ipfs","options":{"endpoint":"https://kubo.fixture/proxy","root_cid":"` + cid + `"},"credential":{"kind":"env","ref":"MWEAVE_TEST_KUBO"}}`,
	} {
		invokeCLI(t, cfg, record, false, 0, "mount", "add", "--from", "-")
	}
	before := requests
	const changedKubo = `{"at":"/ipfs","driver":"ipfs","options":{"endpoint":"https://kubo.fixture/other","root_cid":"` + cid + `"}}`
	if got := invokeCLI(t, cfg, changedKubo, false, 1, "mount", "edit", "/ipfs", "--from", "-", "--json"); got.Error == nil ||
		got.Error.Code != "credential_scope_conflict" || requests != before {
		t.Fatalf("Kubo credential crossed the configured proxy prefix: %+v", got.Error)
	}
	for _, at := range []string{"/git", "/atom", "/release", "/cnb", "/arts", "/ipfs"} {
		if got := invokeCLI(t, cfg, "", false, 0, "ls", at, "--json"); !bytes.Contains(got.Result, []byte(`"name":"note"`)) {
			t.Fatalf("list %s: %s", at, got.Result)
		}
		assertSourceBody(t, cfg, at+"/note", "sealed")
	}
	if gitRefs != 3 || releaseRefs != 3 || cnbRefs != 5 {
		t.Fatalf("unexpected reference resolutions: git=%d release=%d CNB=%d", gitRefs, releaseRefs, cnbRefs)
	}

	requireGitToken = true
	invokeCLI(t, cfg, github+`,"credential":{"kind":"env","ref":"MWEAVE_TEST_GIT"}}`, false, 0, "mount", "edit", "/git", "--from", "-")
	before = requests
	invokeCLI(t, cfg, github+"}", false, 0, "mount", "edit", "/git", "--from", "-")
	if requests != before {
		t.Fatal("an ordinary edit rechecked a retained credential")
	}
	if got := invokeCLI(t, cfg, github+`,"credential":null}`, false, 1, "mount", "edit", "/git", "--from", "-", "--json"); got.Error == nil ||
		!bytes.Contains(got.Result, []byte(`"effect":"unchanged"`)) {
		t.Fatalf("anonymous edit failed to preserve the private mount: %+v %s", got.Error, got.Result)
	}
	assertSourceBody(t, cfg, "/git/note", "sealed")
	requireGitToken = false
	invokeCLI(t, cfg, github+`,"credential":null}`, false, 0, "mount", "edit", "/git", "--from", "-")
	assertSourceBody(t, cfg, "/git/note", "sealed")
}
