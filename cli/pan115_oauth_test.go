package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestManaged115Workflow(t *testing.T) {
	if config := os.Getenv("MWEAVE_TEST_115_REOPEN"); config != "" {
		http.DefaultTransport = sealedTransport(func(*http.Request) (*http.Response, error) {
			panic("reopening an unresolved rotation must perform zero requests")
		})
		os.Exit(testApp.Run(context.Background(), []string{"--config", config, "ls", "/115", "--json"}, nil, os.Stdout, os.Stderr, false))
	}
	if !managedStoreSupported {
		t.Skip("managed credential storage is unavailable")
	}
	home := t.TempDir()
	t.Setenv("APPDATA", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("MWEAVE_115_APP_ID", "fixture-app")
	config := filepath.Join(home, "config.json")
	ctx := t.Context()
	var challenge, id string
	var scans, refreshes, exchanges atomic.Int32
	var lose, wrong, expireCode atomic.Bool
	oldTransport, oldLaunch := http.DefaultTransport, launchOAuthBrowser
	t.Cleanup(func() { http.DefaultTransport, launchOAuthBrowser = oldTransport, oldLaunch })
	launchOAuthBrowser = func(context.Context, string) error { return errors.New("image viewer unavailable") }
	http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" {
			t.Error("authorization requires HTTPS")
		}
		var body string
		auth := r.Header.Get("Authorization")
		switch r.URL.Host + r.URL.Path {
		case "passportapi.115.com/open/authDeviceCode", "passportapi.115.com/open/deviceCodeToToken", "passportapi.115.com/open/refreshToken":
			defer r.Body.Close()
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			f := r.PostForm
			if r.Method != http.MethodPost || auth != "" || f.Has("client_secret") {
				t.Error("invalid passport request")
			}
			switch r.URL.Path {
			case "/open/authDeviceCode":
				scans.Add(1)
				challenge = f.Get("code_challenge")
				if len(challenge) != 43 || f.Get("code_challenge_method") != "sha256" || f.Get("client_id") != "fixture-app" {
					t.Error("incorrect PKCE admission")
				}
				body = `{"state":1,"code":0,"data":{"uid":"sealed-device","time":12,"sign":"sealed-sign","qrcode":"https://115.invalid/fixture"}}`
			case "/open/deviceCodeToToken":
				hash := sha256.Sum256([]byte(f.Get("code_verifier")))
				if f.Get("uid") != "sealed-device" || base64.RawURLEncoding.EncodeToString(hash[:]) != challenge {
					t.Error("exchange did not match the device challenge")
				}
				body = fmt.Sprintf(`{"state":1,"code":0,"data":{"access_token":"sealed-access","refresh_token":"sealed-confirmed-%d","expires_in":7200}}`, exchanges.Add(1))
			case "/open/refreshToken":
				n := refreshes.Add(1)
				record, err := pan115Authorization.loadGrant(id)
				if err != nil || !record.Pending || f.Get("refresh_token") != record.Refresh {
					t.Error("rotation did not publish the pending checkpoint")
				}
				if lose.Load() {
					return nil, io.ErrUnexpectedEOF
				}
				body = fmt.Sprintf(`{"state":1,"code":0,"data":{"access_token":"sealed-renewed","refresh_token":"sealed-rotated-%d","expires_in":7200}}`, n)
			}
		case "qrcodeapi.115.com/get/status/":
			q := r.URL.Query()
			if r.Method != http.MethodGet || auth != "" || q.Get("uid") != "sealed-device" || q.Get("time") != "12" || q.Get("sign") != "sealed-sign" {
				t.Error("poll used the wrong device or credentials")
			}
			body = `{"state":1,"code":0,"data":{"status":2}}`
			if expireCode.Load() {
				body = `{"state":0,"code":0,"data":{}}`
			}
		case "proapi.115.com/open/user/info":
			if auth != "Bearer sealed-access" {
				t.Error("wrong credential on identity call")
			}
			body = `{"state":true,"code":0,"data":{"user_id":9007199254740993,"user_name":"Fixture"}}`
			if wrong.Load() {
				body = `{"state":true,"code":0,"data":{"user_id":9007199254740994,"user_name":"Fixture"}}`
			}
		case "proapi.115.com/open/ufile/files":
			if auth != "Bearer sealed-access" && auth != "Bearer sealed-renewed" && auth != "Bearer fixture-reference" ||
				r.URL.Query().Get("cid") != "0" {
				t.Error("invalid file request")
			}
			if auth == "Bearer sealed-renewed" {
				g, err := pan115Authorization.loadGrant(id)
				if err != nil || g.Pending || g.Refresh != "sealed-rotated-1" {
					t.Error("file request used access before the pair was durable")
				}
			}
			body = `{"state":true,"cid":0,"count":1,"offset":0,"limit":1,"data":[{"fid":4,"pid":0,"fn":"file.txt","fc":"1","fs":6,"pc":"fixture-pick"}]}`
		default:
			return nil, errors.New("request outside the closed 115 authorization fixture")
		}
		return &http.Response{Request: r, StatusCode: 200, Header: make(http.Header), ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
	})

	const add = "115\n\n\n\ny\n"
	invokeCLI(t, config, add, true, 0, "mount", "add", "/115")
	invokeCLI(t, config, add, true, 0, "mount", "add", "/shared")
	var conflictOut, conflictDiag bytes.Buffer
	if code := testApp.Run(ctx, []string{"--config", config, "mount", "add", "/115/child"}, strings.NewReader("115\n"), &conflictOut, &conflictDiag, true); code != 1 ||
		!strings.Contains(conflictDiag.String(), "config_conflict") || strings.Contains(conflictDiag.String(), "Connection") {
		t.Fatalf("nested mount started authorization: %s %s", &conflictOut, &conflictDiag)
	}
	saved, err := loadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	id = saved.value.Mounts[0].Credential.Ref
	if saved.value.Mounts[1].Credential.Ref != id || scans.Load() != 1 || refreshes.Load() != 0 {
		t.Fatal("a compatible saved account did not reuse the grant")
	}
	expire := func() {
		t.Helper()
		lease, err := acquireGrant(id, false)
		if err != nil {
			t.Fatal(err)
		}
		g, err := pan115Authorization.readGrant(lease.path, id)
		if err == nil {
			g.Expires = time.Now()
			err = g.save(ctx, lease, true)
		}
		if err := errors.Join(err, lease.lock.Close()); err != nil {
			t.Fatal(err)
		}
	}
	expire()
	invokeCLI(t, config, "", false, 0, "ls", "/115", "--json")
	expire()
	lose.Store(true)
	for range 2 {
		got := invokeCLI(t, config, "", false, 1, "ls", "/115", "--json")
		if got.Error == nil || got.Error.Code != "interaction_required" {
			t.Fatalf("uncertain rotation: %+v", got.Error)
		}
	}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManaged115Workflow$")
	cmd.Env = append(os.Environ(), "MWEAVE_TEST_115_REOPEN="+config)
	out, err := cmd.Output()
	var result resultForTest
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || json.Unmarshal(out, &result) != nil ||
		result.Error == nil || result.Error.Code != "interaction_required" {
		t.Fatalf("fresh process: %s (%v)", out, err)
	}
	if refreshes.Load() != 2 {
		t.Fatalf("uncertain token replayed: %d refreshes", refreshes.Load())
	}
	for _, failure := range []struct {
		flag *atomic.Bool
		code string
	}{{&expireCode, "interaction_required"}, {&wrong, "credential_scope_conflict"}} {
		failure.flag.Store(true)
		var out, diag bytes.Buffer
		if code := testApp.Run(ctx, []string{"--config", config, "mount", "edit", "/115"}, strings.NewReader("reauthorize\n"), &out, &diag, true); code != 1 ||
			!strings.Contains(diag.String(), failure.code) || strings.Contains(diag.String(), "sealed-") {
			t.Fatalf("unexpected failed reauthorization: %s", &diag)
		}
		failure.flag.Store(false)
		g, err := pan115Authorization.loadGrant(id)
		if err != nil || !g.Pending || g.Refresh != "sealed-rotated-1" {
			t.Fatal("failed reauthorization replaced the checkpoint")
		}
	}
	invokeCLI(t, config, "reauthorize\n", true, 0, "mount", "edit", "/115")
	if got := invokeCLI(t, config, "", false, 0, "ls", "/shared", "--json"); !bytes.Contains(got.Result, []byte("file.txt")) {
		t.Fatalf("shared mount did not recover: %s", got.Result)
	}
	updated, err := loadConfig(config)
	if err != nil || saved.digest != updated.digest {
		t.Fatal("reauthorization changed saved mounts")
	}
	t.Setenv("MWEAVE_TEST_115_REFERENCE", "fixture-reference")
	var refOutput, refPrompt bytes.Buffer
	refInput := strings.NewReader("115\nreference\n\nraw-token\nprofile:wrong\nenv:MWEAVE_TEST_115_REFERENCE\ny\n")
	refArgs := []string{"--config", config, "mount", "add", "/external"}
	if code := testApp.Run(ctx, refArgs, refInput, &refOutput, &refPrompt, true); code != 0 ||
		strings.Count(refPrompt.String(), "Access token reference") != 3 || strings.Contains(refPrompt.String(), "raw-token") {
		t.Fatalf("115 credential retry returned %d: %s %s", code, &refOutput, &refPrompt)
	}
	if data, err := os.ReadFile(config); err != nil || bytes.Contains(data, []byte("sealed-")) || bytes.Contains(data, []byte("fixture-app")) ||
		bytes.Contains(data, []byte("fixture-reference")) {
		t.Fatal("mount configuration contains authorization material")
	}
}
