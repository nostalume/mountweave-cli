package cliapp_test

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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/cli/host"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	native "github.com/nostalume/mountweave/drivers/s3"
	bolt "go.etcd.io/bbolt"
)

const sealedObject = "a//../b%2F+ 中"

func s3Client(endpoint, id, secret string, httpClient *http.Client) *sdk.Client {
	return sdk.New(sdk.Options{Region: cliapp.DefaultS3Region, BaseEndpoint: aws.String(endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(id, secret, ""), RetryMaxAttempts: 1, HTTPClient: httpClient})
}

func objectHost(t *testing.T) *host.App {
	t.Helper()
	type settings struct {
		Endpoint string `json:"endpoint"`
		Prefix   string `json:"prefix"`
	}
	source, err := host.NewSource("object-fixture", "Sealed native object store", nil, func(o settings, _ string) (host.PreparedSource, error) {
		u, err := url.Parse(o.Endpoint)
		if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
			return host.PreparedSource{}, fmt.Errorf("fixture must use a local endpoint")
		}
		return host.PreparedSource{Open: func(context.Context, string) (core.Driver, error) {
			client := s3Client(o.Endpoint, "native-key", "sealed-upstream", &http.Client{Timeout: 3 * time.Second})
			return cliapp.NativeDriver(native.New(client, "native", o.Prefix, native.Options{
				PartSize: 8 << 20, MaxBufferedBytes: 8 << 20,
			}))
		}}, nil
	})
	require(t, err)
	app, err := host.New(source)
	require(t, err)
	return app
}

func TestS3MultipartRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	const secret = "sealed-inbound"
	t.Setenv("MWEAVE_TEST_INBOUND_KEY", secret)
	clientTLS := exportTLSClient(t, dir)
	addresses := reserveAddresses(t, 3)
	value, commits, requests := "original", 0, 0
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		switch {
		case !strings.Contains(r.Header.Get("Authorization"), "Credential=native-key/"):
			t.Errorf("wrong credential recipient")
			w.WriteHeader(http.StatusForbidden)
		case r.URL.Path != "/native/vault/"+sealedObject || !strings.Contains(r.URL.EscapedPath(), "a//../b%252F"):
			t.Errorf("changed native object key: %q", r.RequestURI)
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			value = string(data)
			commits++
			w.Header().Set("ETag", `"sealed-publish"`)
		default:
			http.ServeContent(w, r, sealedObject, time.Time{}, strings.NewReader(value))
		}
	}))
	defer upstream.Close()
	putFixture(t, cfg, []byte(fmt.Sprintf(`{"version":1,"mounts":[{"at":"/objects","driver":"object-fixture","options":{"endpoint":%q,"prefix":"vault/"}}],
		"exports":[{"protocol":"s3","listen":%q,"root":"/objects","access":"read_write","tls":{"cert_file":"cert.pem","key_file":"key.pem"},
		"s3":{"bucket":"visible","multipart":{"state_directory":"state","max_staging_bytes":33554432}},"auth":{"access_key_id":"client-key","credential":{"kind":"env","ref":"MWEAVE_TEST_INBOUND_KEY"}}},
		{"protocol":"s3","listen":%q,"root":"/objects","s3":{"bucket":"visible","max_put_bytes":32},"auth":{"access_key_id":"client-key","credential":{"kind":"env","ref":"MWEAVE_TEST_INBOUND_KEY"}}}]}`,
		upstream.URL, addresses[0], addresses[1])))
	var details, diagnostics bytes.Buffer
	if code := objectHost(t).Run(ctx, []string{"--config", cfg, "stat", "key:objects/:" + sealedObject},
		nil, &details, &diagnostics, false); code != 0 || !strings.Contains(details.String(), "key:/objects:"+sealedObject+"\n") {
		t.Fatalf("qualified object path: %d %q %s", code, &details, &diagnostics)
	}
	start := func() func() {
		t.Helper()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMultipartServerProcess$")
		cmd.Env = append(os.Environ(), "MWEAVE_TEST_MULTIPART_CONFIG="+cfg)
		pipe, err := cmd.StdoutPipe()
		require(t, err)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		require(t, cmd.Start())
		stop := sync.OnceFunc(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			clientTLS.CloseIdleConnections()
		})
		t.Cleanup(stop)
		var ready startup
		if err := json.NewDecoder(pipe).Decode(&ready); err != nil || ready.Error != nil || ready.Result.Status != "ready" {
			stop()
			t.Fatalf("multipart process startup: %+v, %v: %s", ready, err, &stderr)
		}
		return stop
	}
	stop := start()
	reader := s3Client("http://"+addresses[1], "client-key", secret, &http.Client{Timeout: 3 * time.Second})
	writer := s3Client("https://"+addresses[0], "client-key", secret, clientTLS)
	bucket, key := aws.String("visible"), aws.String(sealedObject)
	object, err := reader.GetObject(ctx, &sdk.GetObjectInput{Bucket: bucket, Key: key})
	require(t, err)
	first, err := io.ReadAll(object.Body)
	require(t, errors.Join(err, object.Body.Close()))
	if string(first) != "original" {
		t.Fatalf("exact key returned %q", first)
	}
	mu.Lock()
	before := requests
	mu.Unlock()
	status := func(want int, err error) {
		t.Helper()
		var response interface{ HTTPStatusCode() int }
		if !errors.As(err, &response) || response.HTTPStatusCode() != want {
			t.Fatalf("want HTTP %d: %v", want, err)
		}
	}
	_, err = reader.PutObject(ctx, &sdk.PutObjectInput{Bucket: bucket, Key: key, Body: strings.NewReader("denied")})
	status(http.StatusForbidden, err)
	_, err = writer.GetObject(ctx, &sdk.GetObjectInput{Bucket: aws.String("outside"), Key: key})
	status(http.StatusNotFound, err)
	mu.Lock()
	denied := requests - before
	mu.Unlock()
	if denied != 0 {
		t.Fatalf("%d unauthorized requests reached storage", denied)
	}
	upload, err := writer.CreateMultipartUpload(ctx, &sdk.CreateMultipartUploadInput{Bucket: bucket, Key: key})
	require(t, err)
	part := func(number int32, body string) types.CompletedPart {
		t.Helper()
		saved, err := writer.UploadPart(ctx, &sdk.UploadPartInput{Bucket: bucket, Key: key, UploadId: upload.UploadId,
			PartNumber: &number, Body: strings.NewReader(body)})
		require(t, err)
		return types.CompletedPart{PartNumber: &number, ETag: saved.ETag}
	}
	old := part(2, "before restart")
	stop()

	app := objectHost(t)
	saved, err := cliapp.LoadConfig(cfg)
	require(t, err)
	oldOptions := saved.Value.Mounts[0].Options
	saved.Value.Mounts[0].Options = bytes.Replace(oldOptions, []byte("vault/"), []byte("different/"), 1)
	runtime, err := cliapp.PrepareExports(app, ctx, saved, nil)
	require(t, runtime.Shutdown(false, time.Second))
	if problem := cliapp.AppProblem(app, err); problem == nil || problem.Code != "multipart_binding_changed" {
		t.Fatalf("changed mount retained staged authorization: %v", err)
	}
	saved.Value.Mounts[0].Options = oldOptions
	saved.Value.Exports[0].Listen = addresses[2]
	applied, err := saved.Save(ctx)
	require(t, err)
	if !applied {
		t.Fatal("listener change was not published")
	}
	stop = start()
	writer = s3Client("https://"+addresses[2], "client-key", secret, clientTLS)
	recovered, err := writer.ListParts(ctx, &sdk.ListPartsInput{Bucket: bucket, Key: key, UploadId: upload.UploadId})
	require(t, err)
	if len(recovered.Parts) != 1 || aws.ToInt32(recovered.Parts[0].PartNumber) != 2 || aws.ToString(recovered.Parts[0].ETag) != aws.ToString(old.ETag) {
		t.Fatalf("recovered parts: %+v", recovered.Parts)
	}
	last := part(2, "tail")
	body := strings.Repeat("a", 5<<20)
	head := part(1, body)
	_, err = writer.CompleteMultipartUpload(ctx, &sdk.CompleteMultipartUploadInput{Bucket: bucket, Key: key, UploadId: upload.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{head, last}}})
	require(t, err)
	mu.Lock()
	final, published := value, commits
	mu.Unlock()
	if published != 1 || final != body+"tail" {
		t.Fatalf("native publication: %d writes and %d bytes", published, len(final))
	}
	inputs, err := os.ReadDir(filepath.Join(dir, "state", "inputs"))
	if err != nil || len(inputs) != 0 {
		t.Fatalf("retained inputs: %d, %v", len(inputs), err)
	}
	stop()
	assertPortsFree(t, addresses)

	runtime, err = cliapp.PrepareExports(app, ctx, saved, nil)
	require(t, err)
	defer runtime.Shutdown(false, time.Second)
	handler := runtime.Endpoints[0].Server.(cliapp.TLSExport).Handler.(*cliapp.S3MultipartExport)
	probe := &heldResponse{httptest.NewRecorder(), make(chan struct{}), make(chan struct{})}
	release := sync.OnceFunc(func() { close(probe.release) })
	defer release()
	go handler.ServeHTTP(probe, httptest.NewRequest("GET", "/visible/a", nil))
	select {
	case <-probe.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	interrupted, cancelWait := context.WithCancel(ctx)
	cancelWait()
	if err := handler.Close(interrupted); !errors.Is(err, context.Canceled) {
		t.Fatalf("adapter closed with an active handler: %v", err)
	}
	db, err := bolt.Open(filepath.Join(dir, "state", "manifest.db"), 0600, &bolt.Options{Timeout: 100 * time.Millisecond})
	if db != nil {
		_ = db.Close()
	}
	if !errors.Is(err, bolt.ErrTimeout) {
		t.Fatalf("active handler lost its manifest lock: %v", err)
	}
	release()
	require(t, handler.Close(ctx))
}

type heldResponse struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
}

func (w *heldResponse) WriteHeader(status int) {
	close(w.entered)
	<-w.release
	w.ResponseRecorder.WriteHeader(status)
}

func TestMultipartServerProcess(t *testing.T) {
	if path := os.Getenv("MWEAVE_TEST_MULTIPART_CONFIG"); path != "" {
		os.Exit(objectHost(t).Run(t.Context(), []string{"--config", path, "serve", "--json"}, nil, os.Stdout, os.Stderr, false))
	}
}
