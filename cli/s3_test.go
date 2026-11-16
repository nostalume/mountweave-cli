package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
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

	bolt "go.etcd.io/bbolt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3ExportWorkflow(t *testing.T) {
	const secret = "sealed-inbound-secret"
	const exact = "a//../b%2F+ 中"
	t.Setenv("MWEAVE_TEST_S3_SECRET", secret)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	profile := filepath.Join(dir, "credentials")
	writeFixture(t, profile, []byte("[fixture]\naws_access_key_id=upstream-fixture\naws_secret_access_key=sealed-upstream-secret\n"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", profile)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "aws-config"))
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	var mu sync.Mutex
	objects := map[string]string{"a": "object", "a/b": "child", exact: "exact"}
	var calls []string
	var publishStarted, releasePublish chan struct{}
	clientTLS := exportTLSFixture(t, dir)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=upstream-fixture/") {
			t.Error("wrong credentials reached native storage")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Path == "/native-bucket" && r.URL.Query().Get("list-type") == "2" {
			calls = append(calls, "LIST "+r.URL.Query().Get("prefix"))
			fmt.Fprint(w, "<ListBucketResult><EncodingType>url</EncodingType><IsTruncated>false</IsTruncated>")
			for _, key := range []string{"a", "a/b", exact} {
				if strings.HasPrefix("vault/"+key, r.URL.Query().Get("prefix")) {
					fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size></Contents>", url.PathEscape("vault/"+key), len(objects[key]))
				}
			}
			fmt.Fprint(w, "</ListBucketResult>")
			return
		}
		key, ok := strings.CutPrefix(r.URL.Path, "/native-bucket/vault/")
		if !ok {
			t.Errorf("wrong native target: %s", r.RequestURI)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		calls = append(calls, r.Method+" "+key)
		if key == exact && !strings.Contains(r.URL.EscapedPath(), "a//../b%252F") {
			t.Error("native transport changed the escaped key")
		}
		if r.Method == http.MethodPut {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if publishStarted != nil {
				close(publishStarted)
				<-releasePublish
				publishStarted = nil
			}
			objects[key] = string(body)
		} else if _, found := objects[key]; !found {
			w.WriteHeader(http.StatusNotFound)
			return
		} else {
			http.ServeContent(w, r, key, time.Time{}, strings.NewReader(objects[key]))
			return
		}
		w.Header().Set("ETag", `"fixture"`)
	}))
	t.Cleanup(upstream.Close)
	mount := fmt.Sprintf(`{"at":"/objects","driver":"s3","options":{"endpoint":%q,"bucket":"native-bucket","region":"us-east-1","prefix":"vault/","path_style":true,"part_size":8388608,"max_buffered_bytes":8388608},"credential":{"kind":"profile","ref":"fixture"}}`, upstream.URL)
	invokeCLI(t, cfg, mount, false, 0, "mount", "add", "--from", "-")
	if err := os.Mkdir(filepath.Join(dir, "tree"), 0700); err != nil {
		t.Fatal(err)
	}
	invokeCLI(t, cfg, `{"at":"/tree","driver":"local","options":{"root":"tree"}}`, false, 0, "mount", "add", "--from", "-")
	addresses := exportAddresses(t, 3)
	record := func(root, listen string) string {
		return fmt.Sprintf(`{"protocol":"s3","listen":%q,"root":%q,"s3":{"bucket":"visible","max_put_bytes":32},"auth":{"access_key_id":"client-key","credential":{"kind":"env","ref":"MWEAVE_TEST_S3_SECRET"}}}`, listen, root)
	}
	invokeCLI(t, cfg, record("/objects/vault", addresses[2]), false, 1, "export", "add", "--from", "-")
	blockedState := strings.Replace(record("/objects", addresses[2]), `"s3":{"bucket":"visible","max_put_bytes":32}`, `"access":"read_write","s3":{"bucket":"visible","multipart":{"state_directory":"tree/state","max_staging_bytes":8192}}`, 1)
	if got := invokeCLI(t, cfg, blockedState, false, 1, "export", "add", "--from", "-", "--json"); got.Error == nil ||
		got.Error.Message != "S3 multipart state must be outside Local mount roots." {
		t.Fatalf("multipart state under Local root: %+v", got.Error)
	}
	invokeCLI(t, cfg, "s3\n/objects\n"+addresses[0]+"\nread_write\nvisible\n\nclient-key\nenv:MWEAVE_TEST_S3_SECRET\ncert.pem\nkey.pem\n"+filepath.Join(dir, "state")+"\n33554432\ny\n", true, 0, "export", "add")
	invokeCLI(t, cfg, record("/objects", addresses[1]), false, 0, "export", "add", "--from", "-")

	// A tree-only binding fails startup before any listener is opened.
	saved, err := loadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	before := len(saved.value.Exports)
	saved.value.Exports = append(saved.value.Exports, export{Protocol: "s3", Root: "/tree", Listen: addresses[2],
		S3: &s3ExportOptions{Bucket: "visible"}, Auth: saved.value.Exports[0].Auth})
	invalid, err := testApp.prepareExports(t.Context(), saved, nil)
	if cleanup := invalid.shutdown(false, time.Second); cleanup != nil {
		t.Fatal(cleanup)
	}
	if err == nil || publicError(err).Code != "invalid_config" || !strings.Contains(err.Error(), "native object keys") {
		t.Fatalf("tree-only export: %v", err)
	}
	assertExportPortsFree(t, addresses)
	saved.value.Exports = saved.value.Exports[:before]

	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	runtime, err := testApp.prepareExports(ctx, saved, nil)
	if err != nil {
		_ = runtime.shutdown(false, time.Second)
		t.Fatal(err)
	}
	shutdown := func() {
		if err := runtime.shutdown(true, time.Second); err != nil {
			t.Error(err)
		}
		assertExportPortsFree(t, addresses)
	}
	defer func() {
		if runtime.core != nil {
			shutdown()
		}
	}()
	serveDone := make(chan error, 1)
	go func(current exportRuntime) { serveDone <- current.run(ctx) }(runtime)
	client := func(endpoint, token string, transport http.RoundTripper) *sdk.Client {
		return sdk.New(sdk.Options{Region: defaultS3Region, UsePathStyle: true, BaseEndpoint: aws.String(endpoint),
			Credentials: credentials.NewStaticCredentialsProvider("client-key", token, ""), RetryMaxAttempts: 1,
			HTTPClient: &http.Client{Transport: transport, Timeout: 3 * time.Second}})
	}
	writerURL := "https://" + addresses[0]
	writer, reader := client(writerURL, secret, clientTLS.Transport), client("http://"+addresses[1], secret, nil)
	bucket := aws.String("visible")
	for key, want := range map[string]string{"a": "object", "a/b": "child", exact: "exact"} {
		got, err := writer.GetObject(ctx, &sdk.GetObjectInput{Bucket: bucket, Key: aws.String(key)})
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(got.Body)
		if closeErr := got.Body.Close(); err != nil || closeErr != nil || string(data) != want {
			t.Fatalf("get %q: %q, %v, %v", key, data, err, closeErr)
		}
	}
	head, err := reader.HeadObject(ctx, &sdk.HeadObjectInput{Bucket: bucket, Key: aws.String(exact)})
	if err != nil || aws.ToInt64(head.ContentLength) != 5 {
		t.Fatalf("head exact key: %+v, %v", head, err)
	}
	list, err := writer.ListObjectsV2(ctx, &sdk.ListObjectsV2Input{Bucket: bucket, Prefix: aws.String("a")})
	if err != nil || len(list.Contents) != 3 || aws.ToString(list.Contents[2].Key) != exact {
		t.Fatalf("exact list: %+v, %v", list, err)
	}
	put := func(c *sdk.Client, body string) error {
		_, err := c.PutObject(ctx, &sdk.PutObjectInput{Bucket: bucket, Key: aws.String(exact), Body: strings.NewReader(body)})
		return err
	}
	if err := put(writer, "updated"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	value, lastCall := objects[exact], calls[len(calls)-1]
	before = len(calls)
	mu.Unlock()
	if value != "updated" || lastCall != "PUT "+exact {
		t.Fatalf("native publish: %q, %q", value, lastCall)
	}

	status := func(want int, err error) {
		t.Helper()
		var response interface{ HTTPStatusCode() int }
		if !errors.As(err, &response) || response.HTTPStatusCode() != want {
			t.Fatalf("want HTTP %d, got %v", want, err)
		}
	}
	status(http.StatusForbidden, put(reader, "forbidden"))
	status(http.StatusRequestEntityTooLarge, put(reader, strings.Repeat("x", 33)))
	tampered := client(writerURL, secret, sealedTransport(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Path, r.URL.RawPath = "/visible/a/b", ""
		return clientTLS.Transport.RoundTrip(r)
	}))
	for _, check := range []struct {
		client *sdk.Client
		bucket string
		status int
	}{
		{tampered, "visible", 403},
		{client(writerURL, "wrong-secret", clientTLS.Transport), "visible", 403},
		{writer, "other-bucket", 404},
	} {
		_, err := check.client.GetObject(ctx, &sdk.GetObjectInput{Bucket: aws.String(check.bucket), Key: aws.String(exact)})
		status(check.status, err)
	}
	mu.Lock()
	rejectedCalls, current := len(calls)-before, objects[exact]
	mu.Unlock()
	if rejectedCalls != 0 || current != "updated" {
		t.Fatalf("rejected request reached storage: %d calls, %q", rejectedCalls, current)
	}

	objectKey := aws.String(exact)
	newUpload := func(c *sdk.Client) *string {
		t.Helper()
		result, err := c.CreateMultipartUpload(ctx, &sdk.CreateMultipartUploadInput{Bucket: bucket, Key: objectKey})
		if err != nil {
			t.Fatal(err)
		}
		return result.UploadId
	}
	upload := func(c *sdk.Client, id *string, number int32, body string) types.CompletedPart {
		t.Helper()
		result, err := c.UploadPart(ctx, &sdk.UploadPartInput{Bucket: bucket, Key: objectKey, UploadId: id, PartNumber: &number, Body: strings.NewReader(body)})
		if err != nil {
			t.Fatal(err)
		}
		return types.CompletedPart{PartNumber: &number, ETag: result.ETag}
	}
	partList := func(c *sdk.Client, id *string) []types.Part {
		t.Helper()
		result, err := c.ListParts(ctx, &sdk.ListPartsInput{Bucket: bucket, Key: objectKey, UploadId: id})
		if err != nil {
			t.Fatal(err)
		}
		return result.Parts
	}
	abort := func(c *sdk.Client, id *string) error {
		_, err := c.AbortMultipartUpload(ctx, &sdk.AbortMultipartUploadInput{Bucket: bucket, Key: objectKey, UploadId: id})
		return err
	}

	shutdown()
	<-serveDone
	runtime = exportRuntime{}
	// Kill a standalone server after it acknowledges part 2.
	startProcess := func() *exec.Cmd {
		t.Helper()
		process := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestS3MultipartProcess$")
		process.Env = append(os.Environ(), "MWEAVE_MULTIPART_TEST_CONFIG="+cfg)
		if binary := os.Getenv("MWEAVE_ACCEPTANCE_BIN"); binary != "" {
			process = exec.CommandContext(ctx, binary, "--config", cfg, "serve", "--json")
			process.Dir = filepath.Dir(binary)
		}
		output, err := process.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var childErrors bytes.Buffer
		process.Stderr = &childErrors
		if err := process.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if process.ProcessState == nil {
				_ = process.Process.Kill()
				_ = process.Wait()
			}
		})
		var ready struct {
			Result struct{ Status string }
			Error  *problem
		}
		if err := json.NewDecoder(output).Decode(&ready); err != nil || ready.Result.Status != "ready" {
			_ = process.Process.Kill()
			_ = process.Wait()
			t.Fatalf("standalone startup: %v %+v %s", err, ready.Error, &childErrors)
		}
		return process
	}
	process := startProcess()
	id := newUpload(writer)
	old := upload(writer, id, 2, "old")
	if err := process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = process.Wait()
	clientTLS.CloseIdleConnections()

	// A changed target must refuse the state before serving requests.
	changed := saved
	changed.value.Mounts = append([]mountRecord(nil), saved.value.Mounts...)
	for i, mount := range changed.value.Mounts {
		if mount.At == "/objects" {
			changed.value.Mounts[i].Options = bytes.Replace(mount.Options, []byte(`"vault/"`), []byte(`"different/"`), 1)
		}
	}
	invalid, err = testApp.prepareExports(ctx, changed, nil)
	_ = invalid.shutdown(false, time.Second)
	if err == nil || publicError(err).Code != "multipart_binding_changed" {
		t.Fatalf("changed binding: %v", err)
	}

	// Listening on a new port preserves the upload's identity.
	saved.value.Exports[0].Listen = addresses[2]
	if _, err := saved.save(ctx); err != nil {
		t.Fatal(err)
	}
	reopened := startProcess()
	writerURL = "https://" + addresses[2]
	writer = client(writerURL, secret, clientTLS.Transport)
	parts := partList(writer, id)
	if len(parts) != 1 || aws.ToInt32(parts[0].PartNumber) != 2 || aws.ToString(parts[0].ETag) != aws.ToString(old.ETag) {
		t.Fatalf("recovered parts: %+v", parts)
	}
	replacement := upload(writer, id, 2, "tail")
	if retry := upload(writer, id, 2, "tail"); aws.ToString(retry.ETag) != aws.ToString(replacement.ETag) || aws.ToString(old.ETag) == aws.ToString(replacement.ETag) {
		t.Fatal("unexpected replacement or retry receipt")
	}
	first := strings.Repeat("a", 5<<20)
	part1 := upload(writer, id, 1, first)
	multipart := &sdk.CompleteMultipartUploadInput{Bucket: bucket, Key: objectKey, UploadId: id,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{part1, replacement}}}
	mu.Lock()
	start, release := make(chan struct{}), make(chan struct{})
	publishStarted, releasePublish = start, release
	mu.Unlock()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	lostReply := client(writerURL, secret, sealedTransport(func(r *http.Request) (*http.Response, error) {
		res, err := clientTLS.Transport.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusOK {
			return res, nil
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return nil, io.ErrUnexpectedEOF
	}))
	completed := make(chan error, 1)
	go func() { _, err := lostReply.CompleteMultipartUpload(ctx, multipart); completed <- err }()
	select {
	case <-start:
	case err := <-completed:
		t.Fatalf("Complete failed before publication: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	status(http.StatusConflict, abort(writer, id))
	close(release)
	released = true
	if err := <-completed; !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lost reply: %v", err)
	}
	if _, err := writer.CompleteMultipartUpload(ctx, multipart); err != nil {
		t.Fatal(err)
	}
	aborted := newUpload(writer)
	upload(writer, aborted, 1, "discard")
	if err := abort(writer, aborted); err != nil {
		t.Fatal(err)
	}
	_, err = writer.ListParts(ctx, &sdk.ListPartsInput{Bucket: bucket, Key: objectKey, UploadId: aborted})
	status(http.StatusNotFound, err)

	mu.Lock()
	commits, final := 0, objects[exact]
	for _, call := range calls {
		if call == "PUT "+exact {
			commits++
		}
	}
	mu.Unlock()
	if commits != 2 || final != first+"tail" {
		t.Fatalf("multipart publication: %d native PUTs, %d bytes", commits, len(final))
	}
	inputs, err := os.ReadDir(filepath.Join(dir, "state", "inputs"))
	if err != nil || len(inputs) != 0 {
		t.Fatalf("retained inputs: %d, %v", len(inputs), err)
	}
	if err := reopened.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = reopened.Wait()
	clientTLS.CloseIdleConnections()
	runtime, err = testApp.prepareExports(ctx, saved, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func(current exportRuntime) { serveDone <- current.run(ctx) }(runtime)

	// An already closed client connection does not imply that a handler has
	// returned. Hold the response while checking that shutdown keeps the lock.
	owner := runtime.endpoints[0].server.(tlsExport).Handler.(*s3MultipartExport)
	blocked := &blockedS3Response{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	releaseResponse := sync.OnceFunc(func() { close(blocked.release) })
	defer releaseResponse()
	go owner.ServeHTTP(blocked, httptest.NewRequest(http.MethodGet, "/visible/a", nil))
	select {
	case <-blocked.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	wait, stop := context.WithCancel(ctx)
	stop()
	if err := owner.close(wait); !errors.Is(err, context.Canceled) {
		t.Fatalf("close returned before its handler: %v", err)
	}
	manifest := filepath.Join(dir, "state", "manifest.db")
	db, err := bolt.Open(manifest, 0600, &bolt.Options{Timeout: 100 * time.Millisecond})
	if err == nil {
		_ = db.Close()
		t.Fatal("manifest unlocked before the handler returned")
	}
	if !errors.Is(err, bolt.ErrTimeout) {
		t.Fatal(err)
	}
	releaseResponse()
	if err := owner.close(ctx); err != nil {
		t.Fatal(err)
	}
	shutdown()
	<-serveDone
	runtime = exportRuntime{}
}

type blockedS3Response struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
}

func (w *blockedS3Response) WriteHeader(status int) {
	close(w.entered)
	<-w.release
	w.ResponseRecorder.WriteHeader(status)
}

func TestS3MultipartProcess(t *testing.T) {
	if path := os.Getenv("MWEAVE_MULTIPART_TEST_CONFIG"); path != "" {
		os.Exit(testApp.Run(context.Background(), []string{"--config", path, "serve", "--json"}, nil, os.Stdout, os.Stderr, false))
	}
}

func TestS3ProfilesAndKeys(t *testing.T) {
	home := t.TempDir()
	credentials := filepath.Join(home, "credentials")
	config := filepath.Join(home, "aws-config")
	type presetFixture struct {
		name, host, region string
		plain, v1          bool
	}
	presets := []presetFixture{
		{"bos", "s3.bj.bcebos.com", "bj", false, false},
		{"cos", "cos.ap-guangzhou.myqcloud.com", "ap-guangzhou", false, true},
		{"kodo", "s3.cn-east-1.qiniucs.com", "cn-east-1", true, false},
		{"ks3", "ks3-cn-beijing.ksyuncs.com", "BEIJING", false, false},
		{"oss", "s3.oss-cn-hangzhou.aliyuncs.com", "cn-hangzhou", true, false},
		{"r2", "fixture-account.r2.cloudflarestorage.com", "auto", false, false},
		{"us3", "s3-cn-bj.ufileos.com", "cn-bj", true, false},
		{"zos", "huadong-1.ctyunzos.cn", "us-east-1", false, false},
	}
	byHost := make(map[string]presetFixture, len(presets))
	var profiles strings.Builder
	for _, p := range presets {
		fmt.Fprintf(&profiles, "[fixture-%s]\naws_access_key_id=fixture-%s-key\naws_secret_access_key=fixture-%s-secret\n", p.name, p.name, p.name)
		byHost["fixture-bucket."+p.host] = p
	}
	if err := os.WriteFile(credentials, []byte(profiles.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("[profile empty-fixture]\nregion=us-east-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentials)
	t.Setenv("AWS_CONFIG_FILE", config)
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	const exact = "a//b%2F+ 中"
	requests := make(map[string]int)
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	http.DefaultTransport = sealedTransport(func(r *http.Request) (*http.Response, error) {
		p, ok := byHost[r.URL.Host]
		if !ok {
			return nil, errors.New("unexpected S3 fixture endpoint")
		}
		auth := r.Header.Get("Authorization")
		if !strings.Contains(auth, "Credential=fixture-"+p.name+"-key/") || !strings.Contains(auth, "/"+p.region+"/s3/aws4_request") {
			t.Errorf("wrong SDK signing profile or region for %s", p.name)
		}
		requests[p.name+" "+r.Method]++
		header := make(http.Header)
		var data string
		switch {
		case r.URL.Path == "/" && r.Method == http.MethodGet:
			q := r.URL.Query()
			if (p.v1 && q.Has("list-type")) || (!p.v1 && q.Get("list-type") != "2") ||
				(p.plain && q.Has("encoding-type")) || (!p.plain && q.Get("encoding-type") != "url") {
				t.Errorf("unexpected native list settings for %s", p.name)
			}
			data = "<ListBucketResult><IsTruncated>false</IsTruncated>"
			if !p.plain {
				data += "<EncodingType>url</EncodingType>"
			}
			switch q.Get("prefix") {
			case "root/":
				// One empty page verifies that the configured prefix is accessible.
			case "root/a":
				for _, suffix := range []string{"a", exact} {
					key := "root/" + suffix
					if !p.plain {
						key = url.PathEscape(key)
					}
					data += fmt.Sprintf("<Contents><Key>%s</Key><Size>6</Size></Contents>", html.EscapeString(key))
				}
			default:
				t.Error("literal listing prefix was changed")
			}
			data += "</ListBucketResult>"
		case r.URL.Path == "/root/"+exact && (r.Method == http.MethodHead || r.Method == http.MethodGet):
			if !strings.Contains(r.URL.EscapedPath(), "/root/a//b%252F") {
				t.Error("SDK changed repeated slash or literal percent escape")
			}
			header.Set("Content-Length", "6")
			if r.Method == http.MethodGet {
				data = "sealed"
			}
		default:
			return nil, errors.New("unexpected S3 fixture request")
		}
		return &http.Response{Request: r, StatusCode: 200, Header: header, ContentLength: int64(len(data)), Body: io.NopCloser(strings.NewReader(data))}, nil
	})

	path := filepath.Join(home, "config.json")
	empty := `{"at":"/empty","driver":"s3","options":{"endpoint":"https://s3.oss-cn-hangzhou.aliyuncs.com","region":"cn-hangzhou","bucket":"fixture-bucket"},"credential":{"kind":"profile","ref":"empty-fixture"}}`
	if got := invokeCLI(t, path, empty, false, 1, "mount", "add", "--from", "-", "--json"); got.Error == nil || got.Error.Code != "credential_unavailable" || len(requests) != 0 {
		t.Fatalf("an empty profile allowed fallback: %+v, %v", got.Error, requests)
	}
	for _, preset := range presets {
		vendor := preset.name
		at := "/" + vendor + ":objects%"
		endpoint := ""
		if vendor == "r2" {
			endpoint = "https://" + preset.host
		}
		input := "s3\n" + vendor + "\nfixture-bucket\n\n" + endpoint + "\nroot/\nprofile:fixture-" + vendor + "\ny\n"
		invokeCLI(t, path, input, true, 0, "mount", "add", at)
		keyPrefix := "key:/" + vendor + "%3Aobjects%25:"
		page := invokeCLI(t, path, "", false, 0, "ls", keyPrefix+"a", "--json")
		if !bytes.Contains(page.Result, []byte(`"key":"a"`)) || !bytes.Contains(page.Result, []byte(`"key":"`+exact+`"`)) {
			t.Fatalf("%s listing: %s", vendor, page.Result)
		}
		stat := invokeCLI(t, path, "", false, 0, "stat", keyPrefix+exact, "--json")
		if !bytes.Contains(stat.Result, []byte(`"space":"`+at+`"`)) {
			t.Fatalf("%s stat: %s", vendor, stat.Result)
		}
		var body, diag bytes.Buffer
		if code := testApp.Run(context.Background(), []string{"--config", path, "get", keyPrefix + exact, "-"}, strings.NewReader(""), &body, &diag, false); code != 0 || body.String() != "sealed" {
			t.Fatalf("%s get: %d %q %s", vendor, code, &body, &diag)
		}
		if requests[vendor+" HEAD"] != 1 || requests[vendor+" GET"] != 3 {
			t.Fatalf("%s request trace: %v", vendor, requests)
		}
	}
	saved, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(saved, []byte(`"plain_list_keys": true`)) || !bytes.Contains(saved, []byte(`"list_v1": true`)) ||
		bytes.Contains(saved, []byte("fixture-oss-secret")) || bytes.Contains(saved, []byte("fixture-cos-secret")) {
		t.Fatalf("expanded preset settings were not saved safely: %v", err)
	}
}
