package cliapp_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/cli/host"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave/drivers/local"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type startup struct {
	Result struct {
		Status  string
		Exports []cliapp.EndpointInfo
	}
	Error *cliapp.Problem
}

func TestProtocolExports(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	putFixture(t, filepath.Join(dir, "files/public/report"), []byte("shared"))
	putFixture(t, filepath.Join(dir, "files/private/secret"), []byte("hidden"))
	putFixture(t, filepath.Join(dir, "api-token"), []byte("sealed-api-token"))
	t.Setenv("MWEAVE_TEST_EXPORT_PASSWORD", "sealed-password")
	tlsClient := exportTLSClient(t, dir)
	serverKey := sshKey(t, filepath.Join(dir, "host.key"), 0)
	clientKey := sshKey(t, filepath.Join(dir, "client.key"), 1)
	putFixture(t, filepath.Join(dir, "client.pub"), ssh.MarshalAuthorizedKey(clientKey.PublicKey()))

	var opens, closes, handles atomic.Int32
	source, err := host.NewSource("tree-fixture", "Local fixture", nil, func(o struct {
		Root string `json:"root"`
	}, base string) (host.PreparedSource, error) {
		return host.PreparedSource{Open: func(context.Context, string) (core.Driver, error) {
			d, err := local.New(filepath.Join(base, o.Root))
			if err != nil {
				return nil, err
			}
			opens.Add(1)
			return &exportTree{d, &closes, &handles}, nil
		}}, nil
	})
	require(t, err)
	app, err := host.New(source)
	require(t, err)
	run := func(input string, want int, terminal bool, args ...string) (string, string) {
		t.Helper()
		var out, diagnostics bytes.Buffer
		argv := append([]string{"--config", config}, args...)
		if code := app.Run(ctx, argv, strings.NewReader(input), &out, &diagnostics, terminal); code != want {
			t.Fatalf("%v: exit=%d, want %d: %s %s", args, code, want, &out, &diagnostics)
		}
		return out.String(), diagnostics.String()
	}
	run(`{"at":"/files","driver":"tree-fixture","options":{"root":"files"}}`, 0, false, "mount", "add", "--from", "-")
	addresses := reserveAddresses(t, 4)
	dav, api, sshAddr, ftp := addresses[0], addresses[1], addresses[2], addresses[3]
	run("\n/files/public\n"+dav+"\n\nreader\nenv:MWEAVE_TEST_EXPORT_PASSWORD\n\ny\n", 0, true, "export", "add")
	for _, record := range []string{
		fmt.Sprintf(`{"protocol":"http","listen":%q,"root":"/files/public","auth":{"credential":{"kind":"file","ref":"api-token"}},"tls":{"cert_file":"cert.pem","key_file":"key.pem"}}`, api),
		fmt.Sprintf(`{"protocol":"sftp","listen":%q,"root":"/files/public","auth":{"username":"reader","public_key_file":"client.pub"},"host_key_file":"host.key"}`, sshAddr),
		fmt.Sprintf(`{"protocol":"ftp","listen":%q,"root":"/files/public","auth":{"username":"reader","credential":{"kind":"env","ref":"MWEAVE_TEST_EXPORT_PASSWORD"}}}`, ftp),
	} {
		run(record, 0, false, "export", "add", "--from", "-")
	}

	blocker, err := net.Listen("tcp", api)
	require(t, err)
	defer blocker.Close()
	result, _ := run("", 1, false, "serve", "--json")
	require(t, blocker.Close())
	if strings.Contains(result, `"ready"`) || !strings.Contains(result, `"listener_failed"`) {
		t.Fatalf("partial startup receipt: %s", result)
	}
	assertPortsFree(t, addresses)
	if opens.Load() != closes.Load() {
		t.Fatal("partial bind leaked the mounted source")
	}

	startCtx, stop := context.WithCancel(ctx)
	defer stop()
	read, write := io.Pipe()
	defer read.Close()
	done := make(chan int, 1)
	var diagnostics bytes.Buffer
	startOpens := opens.Load()
	go func() {
		done <- app.Run(startCtx, []string{"--config", config, "serve", "--json"}, nil, write, &diagnostics, false)
		_ = write.Close()
	}()
	var ready startup
	timeout := time.AfterFunc(5*time.Second, func() { _ = read.CloseWithError(context.DeadlineExceeded) })
	err = json.NewDecoder(read).Decode(&ready)
	timeout.Stop()
	if err != nil || ready.Error != nil || ready.Result.Status != "ready" || len(ready.Result.Exports) != 4 ||
		!ready.Result.Exports[1].TLS || opens.Load() != startOpens+1 {
		t.Fatalf("shared startup: %+v, %v, opens=%d", ready, err, opens.Load()-startOpens)
	}

	request := func(method, target, authorization string, status int) string {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader("denied"))
		require(t, err)
		if authorization == "basic" {
			req.SetBasicAuth("reader", "sealed-password")
		} else if authorization == "bearer" {
			req.Header.Set("Authorization", "Bearer sealed-api-token")
		}
		resp, err := tlsClient.Do(req)
		require(t, err)
		body, err := io.ReadAll(resp.Body)
		require(t, errors.Join(err, resp.Body.Close()))
		if resp.StatusCode != status {
			t.Fatalf("%s %s: status %d, want %d: %q", method, target, resp.StatusCode, status, body)
		}
		return string(body)
	}
	if body := request("GET", "http://"+dav+"/report", "basic", 200); body != "shared" {
		t.Fatalf("WebDAV body: %q", body)
	}
	request("GET", "http://"+dav+"/report", "bearer", 401)
	request("PUT", "http://"+dav+"/report", "basic", 403)
	target := "https://" + api + "/api/v1/fs/content?path="
	request("GET", target+url.QueryEscape("/files/private/secret"), "bearer", 403)
	if body := request("GET", target+url.QueryEscape("/files/public/report"), "bearer", 200); body != "shared" {
		t.Fatalf("JSON HTTP body: %q", body)
	}

	connection, err := net.DialTimeout("tcp", sshAddr, time.Second)
	require(t, err)
	defer connection.Close()
	require(t, connection.SetDeadline(time.Now().Add(12*time.Second)))
	conn, messages, requests, err := ssh.NewClientConn(connection, sshAddr, &ssh.ClientConfig{
		User: "reader", Auth: []ssh.AuthMethod{ssh.PublicKeys(clientKey)}, HostKeyCallback: ssh.FixedHostKey(serverKey.PublicKey()),
	})
	require(t, err)
	sshClient := ssh.NewClient(conn, messages, requests)
	defer sshClient.Close()
	files, err := sftp.NewClient(sshClient)
	require(t, err)
	defer files.Close()
	file, err := files.Open("/report")
	require(t, err)
	body, err := io.ReadAll(file)
	require(t, err)
	if string(body) != "shared" || handles.Load() != 1 {
		t.Fatalf("SFTP body and ownership: %q, open handles %d", body, handles.Load())
	}
	checkFTPRead(t, ftp)
	stop()
	select {
	case code := <-done:
		if code != 0 || handles.Load() != 0 || closes.Load() != opens.Load() {
			t.Fatalf("shutdown: exit=%d, handles=%d, opens=%d, closes=%d: %s", code, handles.Load(), opens.Load(), closes.Load(), &diagnostics)
		}
	case <-ctx.Done():
		t.Fatal("exports did not shut down")
	}
	assertPortsFree(t, addresses)
}

type exportTree struct {
	*local.Driver
	closes, handles *atomic.Int32
}

func (d *exportTree) Close() error {
	d.closes.Add(1)
	return d.Driver.Close()
}

func (d *exportTree) Open(ctx context.Context, addr core.NativeAddress, opts core.OpenOptions) (core.Opened, error) {
	opened, err := d.Driver.Open(ctx, addr, opts)
	if err == nil && opened.File != nil {
		d.handles.Add(1)
		opened.File = &exportHandle{opened.File, d.handles}
	}
	return opened, err
}

type exportHandle struct {
	core.ReadFile
	count *atomic.Int32
}

func (f *exportHandle) Close() error {
	f.count.Add(-1)
	return f.ReadFile.Close()
}

func checkFTPRead(t *testing.T, address string) {
	t.Helper()
	socket, err := net.DialTimeout("tcp", address, time.Second)
	require(t, err)
	defer socket.Close()
	require(t, socket.SetDeadline(time.Now().Add(5*time.Second)))
	control := textproto.NewConn(socket)
	reply := func(code int) string {
		t.Helper()
		_, line, err := control.ReadResponse(code)
		require(t, err)
		return line
	}
	command := func(line string, code int) string {
		t.Helper()
		require(t, control.PrintfLine("%s", line))
		return reply(code)
	}
	reply(220)
	command("USER reader", 331)
	command("PASS wrong", 530)
	command("PASS sealed-password", 230)
	passive := strings.Split(command("EPSV", 229), "|")
	if len(passive) != 5 {
		t.Fatalf("invalid passive endpoint: %q", passive)
	}
	data, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", passive[3]), time.Second)
	require(t, err)
	defer data.Close()
	require(t, data.SetDeadline(time.Now().Add(5*time.Second)))
	command("RETR report", 150)
	body, err := io.ReadAll(data)
	require(t, err)
	reply(226)
	if string(body) != "shared" {
		t.Fatalf("FTP body: %q", body)
	}
	command("QUIT", 221)
}

func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func putFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	require(t, os.MkdirAll(filepath.Dir(path), 0700))
	require(t, os.WriteFile(path, data, 0600))
}

func reserveAddresses(t *testing.T, count int) []string {
	t.Helper()
	addresses := make([]string, 0, count)
	for range count {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require(t, err)
		defer ln.Close()
		addresses = append(addresses, ln.Addr().String())
	}
	return addresses
}

func assertPortsFree(t *testing.T, addresses []string) {
	t.Helper()
	for _, address := range addresses {
		ln, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("listener still owns %s: %v", address, err)
		}
		require(t, ln.Close())
	}
}

func exportTLSClient(t *testing.T, dir string) *http.Client {
	t.Helper()
	fixture := httptest.NewTLSServer(nil)
	defer fixture.Close()
	cert := fixture.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require(t, err)
	putFixture(t, filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.Certificate().Raw}))
	putFixture(t, filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	roots := x509.NewCertPool()
	roots.AddCert(fixture.Certificate())
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}, Timeout: 3 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func sshKey(t *testing.T, path string, seed byte) ssh.Signer {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	file, err := ssh.MarshalPrivateKey(key, "")
	require(t, err)
	putFixture(t, path, pem.EncodeToMemory(file))
	signer, err := ssh.NewSignerFromKey(key)
	require(t, err)
	return signer
}
