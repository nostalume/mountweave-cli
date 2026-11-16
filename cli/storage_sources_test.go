package cli

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/nostalume/mountweave"
)

func assertSourceBody(t *testing.T, configPath, address, want string) {
	t.Helper()
	var body, diagnostics bytes.Buffer
	code := testApp.Run(context.Background(), []string{"--config", configPath, "--timeout", "5s", "get", address, "-"},
		strings.NewReader(""), &body, &diagnostics, false)
	if code != 0 || body.String() != want {
		t.Fatalf("%s: %d %q %s, want %q", address, code, body.String(), &diagnostics, want)
	}
}

func TestHTTPStorageSources(t *testing.T) {
	const (
		blobKey = "a//b%2F+ 中"
		sas     = "sv=2025-11-05&sr=c&sp=rwl&sig=fixture%2Bsignature"
		secret  = "synthetic-storage-password"
	)
	t.Setenv("MWEAVE_TEST_STORAGE_SECRET", secret)
	t.Setenv("MWEAVE_TEST_AZURE_SAS", "?"+sas)
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	blob := "old!"
	mkdir := false
	http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		q := r.URL.Query()
		switch r.URL.Host {
		case "azure.invalid":
			if q.Get("sig") != "fixture+signature" || q.Get("sr") != "c" {
				t.Error("Azure SAS was lost or changed")
			}
			w.Header().Set("x-ms-request-id", "fixture")
			switch {
			case r.Method == "GET" && q.Get("comp") == "list" && r.URL.Path == "/container":
				if q.Get("prefix") != "archive/" {
					t.Error("Azure list escaped the configured prefix")
				}
				fmt.Fprintf(w, "<EnumerationResults><Blobs><Blob><Name>archive/%s</Name><Properties><Content-Length>4</Content-Length><BlobType>BlockBlob</BlobType></Properties></Blob></Blobs><NextMarker/></EnumerationResults>", blobKey)
			case r.URL.Path == "/container/archive/"+blobKey:
				// Azure encodes the whole blob name as one URL segment.
				if r.URL.EscapedPath() != "/container/"+url.PathEscape("archive/"+blobKey) {
					t.Error("Azure SDK changed the exact key")
				}
				w.Header().Set("ETag", `"fixture"`)
				w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
				w.Header().Set("x-ms-blob-type", "BlockBlob")
				switch r.Method {
				case "GET":
					w.Header().Set("Content-Length", "4")
					fmt.Fprint(w, blob)
				case "PUT":
					defer r.Body.Close()
					if r.Header.Get("If-None-Match") == "*" {
						w.Header().Set("x-ms-error-code", "ConditionNotMet")
						w.WriteHeader(http.StatusPreconditionFailed)
						break
					}
					if kind := r.Header["x-ms-blob-type"]; q.Has("comp") || len(kind) != 1 || kind[0] != "BlockBlob" {
						t.Error("expected native Put Blob")
					}
					b, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					blob = string(b)
					w.WriteHeader(http.StatusCreated)
				default:
					return nil, errors.New("unexpected Azure object request")
				}
			default:
				return nil, errors.New("unexpected Azure endpoint or request")
			}
		case "bunny.invalid":
			if r.Header.Get("AccessKey") != secret || r.Method != "GET" {
				t.Error("unexpected Bunny authorization or mutation")
			}
			switch r.URL.Path {
			case "/zone/archive/":
				fmt.Fprint(w, `[{"ObjectName":"note","Path":"/zone/archive/","IsDirectory":false,"Length":4}]`)
			case "/zone/archive/note":
				w.Header().Set("Content-Length", "4")
				fmt.Fprint(w, "buny")
			default:
				return nil, errors.New("Bunny request outside the rooted directory")
			}
		case "upyun.invalid":
			sum := md5.Sum([]byte(secret))
			mac := hmac.New(sha1.New, []byte(hex.EncodeToString(sum[:])))
			fmt.Fprintf(mac, "%s&%s&%s", r.Method, r.URL.RequestURI(), r.Header.Get("Date"))
			if r.Header.Get("Authorization") != "UPYUN operator:"+base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
				t.Error("UPYUN did not sign the native request")
			}
			switch {
			case r.Method == "HEAD" && r.URL.Path == "/bucket/archive/":
				w.Header().Set("X-Upyun-File-Type", "folder")
			case r.Method == "GET" && r.URL.Path == "/bucket/archive/":
				if r.Header.Get("X-List-Iter") == "" {
					fmt.Fprint(w, `{"files":[],"iter":"opaque&marker"}`)
				} else if r.Header.Get("X-List-Iter") == "opaque&marker" {
					io.WriteString(w, `{"files":[{"name":"p%+.txt","type":"text/plain","length":4}],"iter":"g2gCZAAEbmV4dGQAA2VvZg"}`)
				} else {
					return nil, errors.New("UPYUN changed its continuation")
				}
			case r.Method == "GET" && r.URL.Path == "/bucket/archive/p%+.txt":
				w.Header().Set("Content-Length", "4")
				w.Header().Set("X-Upyun-File-Type", "file")
				fmt.Fprint(w, "upyn")
			case r.Method == "POST" && r.URL.Path == "/bucket/archive/new":
				mkdir = r.Header.Get("X-Upyun-Folder") == "true"
			default:
				return nil, errors.New("unexpected UPYUN request")
			}
		default:
			return nil, errors.New("request outside the closed storage fixture")
		}
		res := w.Result()
		res.Request = r
		return res, nil
	})

	path := filepath.Join(t.TempDir(), "config.json")
	writeFixture(t, filepath.Join(filepath.Dir(path), "upyun.secret"), []byte(secret+"\n"))
	for _, record := range []string{
		`{"at":"/az","driver":"azureblob","options":{"container_url":"https://azure.invalid/container","prefix":"archive/","part_size":1024,"max_buffered_bytes":1024},"credential":{"kind":"env","ref":"MWEAVE_TEST_AZURE_SAS"}}`,
		`{"at":"/bunny","driver":"bunny","options":{"root_url":"https://bunny.invalid/zone/archive/"},"credential":{"kind":"env","ref":"MWEAVE_TEST_STORAGE_SECRET"}}`,
		`{"at":"/upyun","driver":"upyun","options":{"root_url":"https://upyun.invalid/bucket/archive/","operator":"operator"},"credential":{"kind":"file","ref":"upyun.secret"}}`,
	} {
		invokeCLI(t, path, record, false, 0, "mount", "add", "--from", "-")
	}
	for _, item := range []struct{ dir, name, address, body string }{
		{"key:/az:", blobKey, "key:/az:" + blobKey, "old!"},
		{"/bunny", "note", "/bunny/note", "buny"},
		{"/upyun", "p%+.txt", "/upyun/p%+.txt", "upyn"},
	} {
		listed := invokeCLI(t, path, "", false, 0, "ls", item.dir, "--json")
		if !bytes.Contains(listed.Result, []byte(item.name)) {
			t.Fatalf("listing %s: %s", item.dir, listed.Result)
		}
		assertSourceBody(t, path, item.address, item.body)
	}

	key := "key:/az:" + blobKey
	if got := invokeCLI(t, path, "nope", false, 1, "put", "-", key, "--size", "4", "--json"); got.Error == nil || !bytes.Contains(got.Result, []byte(`"effect":"unchanged"`)) || blob != "old!" {
		t.Fatalf("Azure exclusive create: %+v, %s, %q", got.Error, got.Result, blob)
	}
	invokeCLI(t, path, "new!", false, 0, "put", "-", key, "--size", "4", "--replace")
	assertSourceBody(t, path, key, "new!")
	for _, dst := range []string{"/bunny/note", "/upyun/p%+.txt"} {
		if got := invokeCLI(t, path, "nope", false, 1, "put", "-", dst, "--json"); got.Error == nil || got.Error.Code != string(core.UnsupportedOperation) {
			t.Fatalf("unsupported upload %s: %+v", dst, got.Error)
		}
	}
	invokeCLI(t, path, "", false, 0, "mkdir", "/upyun/new")
	if !mkdir {
		t.Fatal("UPYUN mkdir did not use the native folder request")
	}
}
