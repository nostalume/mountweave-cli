package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
	"github.com/nostalume/mountweave/drivers/local"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func TestExportWorkflow(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.json")
	write := func(name, value string) {
		writeFixture(t, filepath.Join(home, name), []byte(value))
	}
	root := filepath.Join(home, "files", "public")
	for _, name := range []string{root, filepath.Join(home, "files", "publicity")} {
		if err := os.MkdirAll(name, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("files/publicity/private", "hidden")
	if err := os.Mkdir(filepath.Join(home, "staging"), 0700); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{}, 1)
	var opens atomic.Int32
	app, err := New(Source{id: "export-fixture", route: sourceRoute("Local export fixture", nil, func(o localOptions, base string) (mountSource, error) {
		src, err := prepareLocal(o, base)
		if err != nil {
			return mountSource{}, err
		}
		open := src.open
		src.open = func(ctx context.Context, secret string) (core.Driver, error) {
			d, err := open(ctx, secret)
			if err != nil {
				return d, err
			}
			opens.Add(1)
			return &exportFixture{d.(*local.Driver), closed}, nil
		}
		return src, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	invokeApp(t, app, configPath, `{"at":"/files","driver":"export-fixture","options":{"root":"files","staging_directory":"staging","max_staging_bytes":1048576}}`, false, 0, "mount", "add", "--from", "-")

	const password, token = "fixture-dav-password", "fixture-http-token"
	t.Setenv("MWEAVE_TEST_DAV_PASSWORD", password)
	tlsClient := exportTLSFixture(t, home)

	host := fixtureSSHKey(t, filepath.Join(home, "host.key"))
	clientKey := fixtureSSHKey(t, filepath.Join(home, "client.key"))
	write("client.pub", string(ssh.MarshalAuthorizedKey(clientKey.PublicKey())))
	addresses := exportAddresses(t, 6)
	invokeCLI(t, configPath, "\n/files/public\n"+addresses[0]+"\n\nreader\nenv:MWEAVE_TEST_DAV_PASSWORD\n\ny\n", true, 0, "export", "add")
	record := fmt.Sprintf(`{"protocol":"http","listen":%q,"root":"/files/public","access":"read_write","auth":{"credential":{"kind":"file","ref":"client.token"}},"tls":{"cert_file":"cert.pem","key_file":"key.pem"}}`, addresses[1])
	got := invokeCLI(t, configPath, record, false, 0, "export", "add", "--from", "-", "--json")
	if !bytes.Contains(got.Result, []byte(`"effect":"applied"`)) {
		t.Fatal(string(got.Result))
	}
	invokeCLI(t, configPath, record, false, 1, "export", "add", "--from", "-", "--json")
	write("client.token", token+"\n")
	invokeCLI(t, configPath, "sftp\n/files/public\n"+addresses[2]+"\nread_write\nreader\n"+filepath.Join(home, "host.key")+"\n"+filepath.Join(home, "client.pub")+"\ny\n", true, 0, "export", "add")
	record = fmt.Sprintf(`{"protocol":"sftp","listen":%q,"root":"/files/public","auth":{"username":"reader","public_key_file":"client.pub"},"host_key_file":"host.key"}`, addresses[3])
	invokeCLI(t, configPath, record, false, 0, "export", "add", "--from", "-")
	_, port, err := net.SplitHostPort(addresses[5])
	if err != nil {
		t.Fatal(err)
	}
	invokeCLI(t, configPath, "ftp\n/files/public\n"+addresses[4]+"\nread_write\nreader\nenv:MWEAVE_TEST_DAV_PASSWORD\n\n"+port+"-"+port+"\ny\n", true, 0, "export", "add")
	shared := `{"at":"/remote","driver":"webdav","options":{"root_url":"http://127.0.0.1:1/dav/","username":"fixture"},"credential":{"kind":"file","ref":"client.token"}}`
	got = invokeCLI(t, configPath, shared, false, 1, "mount", "add", "--from", "-", "--json")
	if got.Error == nil || got.Error.Code != "credential_scope_conflict" {
		t.Fatalf("storage reused an inbound reference: %+v", got.Error)
	}
	saved, err := os.ReadFile(configPath)
	if err != nil || bytes.Contains(saved, []byte(password)) || bytes.Contains(saved, []byte(token)) {
		t.Fatalf("configuration contains a secret or is unreadable: %v", err)
	}

	// A failed second bind must release the first listener before returning.
	blocked, err := net.Listen("tcp", addresses[1])
	if err != nil {
		t.Fatal(err)
	}
	got = invokeApp(t, app, configPath, "", false, 1, "serve", "--json")
	_ = blocked.Close()
	if got.Error == nil || got.Error.Code != "listener_failed" || !strings.Contains(got.Error.Message, addresses[1]) || string(got.Result) != "null" {
		t.Fatalf("partial startup: %+v, %s", got.Error, got.Result)
	}

	opens.Store(0)
	ctx, stop := context.WithCancel(context.Background())
	read, ready := io.Pipe()
	done := make(chan int, 1)
	var diagnostics bytes.Buffer
	go func() {
		done <- app.Run(ctx, []string{"--config", configPath, "serve", "--json"}, nil, ready, &diagnostics, false)
		_ = ready.Close()
	}()
	t.Cleanup(func() { stop(); _ = read.Close() })
	type readiness struct {
		Result struct {
			Status  string
			Exports []endpointInfo
		}
		Error *problem
	}
	line := make(chan readiness, 1)
	go func() {
		var got readiness
		_ = json.NewDecoder(bufio.NewReader(read)).Decode(&got)
		line <- got
	}()
	select {
	case got := <-line:
		if got.Error != nil || got.Result.Status != "ready" || len(got.Result.Exports) != len(addresses)-1 || !got.Result.Exports[1].TLS || got.Result.Exports[2].Protocol != "sftp" || got.Result.Exports[2].TLS {
			t.Fatalf("readiness: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not report readiness")
	}
	if count := opens.Load(); count != 1 {
		t.Fatalf("shared mount opened %d times", count)
	}
	dav := &http.Client{Timeout: 3 * time.Second}
	t.Cleanup(dav.CloseIdleConnections)
	api := "https://" + addresses[1] + "/api/v1/fs/"
	davURL := "http://" + addresses[0]
	request := func(client *http.Client, method, target, auth, data string, status int) string {
		t.Helper()
		req, err := http.NewRequest(method, target, strings.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if auth == password {
			req.SetBasicAuth("reader", password)
		} else if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		req.Header.Set("Depth", "0")
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != status {
			t.Fatalf("%s %s: %d, %q, %v, want %d", method, target, res.StatusCode, body, err, status)
		}
		return string(body)
	}
	content := func(path string) string { return api + "content?path=" + url.QueryEscape(path) }
	request(dav, "PROPFIND", davURL+"/", "", "", 401)
	request(tlsClient, "GET", content("/files/publicity/private"), token, "", 403)
	request(tlsClient, "PUT", content("/files/public/note"), token, "same mount", 201)
	if body := request(dav, "GET", davURL+"/note", password, "", 200); body != "same mount" {
		t.Fatal(body)
	}
	request(dav, "PUT", davURL+"/note", password, "denied", 403)
	request(tlsClient, "GET", content("/files/public/note"), password, "", 401)
	for _, op := range []string{
		`{"kind":"fs.copy","input":{"source":"/files/publicity/private","target":"/files/public/copied"}}`,
		`{"kind":"fs.move","input":{"source":"/files/public/note","target":"/files/publicity/moved"}}`,
		`{"kind":"fs.remove_tree","input":{"path":"/files/public","contents_only":true,"max_entries":10,"max_plan_bytes":1000}}`,
	} {
		request(tlsClient, "POST", api+"operations", token, op, 403)
	}
	request(tlsClient, "GET", content("/files/public/note"), token, "", 200)
	for _, name := range []string{"files/public/copied", "files/publicity/moved"} {
		if _, err := os.Stat(filepath.Join(home, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unauthorized effect at %s: %v", name, err)
		}
	}
	request(dav, "PROPFIND", davURL+"/", password, "", 207)
	holdSFTP := checkSFTPExport(t, addresses[2:], host, clientKey, closed, func() {
		if body := request(dav, "GET", davURL+"/upload", password, "", 200); body != "from sftp" {
			t.Fatal(body)
		}
	})

	ftp := checkFTPExport(t, addresses[4], addresses[5], password, func() {
		if body := request(dav, "GET", davURL+"/note", password, "", 200); body != "same mount" {
			t.Fatal(body)
		}
	})

	// A saved root change takes effect on restart. Replacing the directory
	// at the original path keeps the same export policy, including root reads.
	write("config.json", strings.ReplaceAll(string(saved), "/files/public", "/files/publicity"))
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	request(dav, "PROPFIND", davURL+"/", password, "", 404)
	write("files/public", "replacement")
	request(dav, "PROPFIND", davURL+"/", password, "", 207)
	request(dav, "GET", davURL+"/", password, "", 403)
	request(tlsClient, "GET", api+"entries?path=/files/public", token, "", 409)
	request(tlsClient, "GET", content("/files/public"), token, "", 403)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	write("files/public/again", "new directory")
	request(dav, "GET", davURL+"/again", password, "", 200)

	holdSFTP()
	stop()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("intentional stop: %d, %s", code, &diagnostics)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("server did not stop")
	}
	if err := ftp.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	assertExportPortsFree(t, addresses)
	select {
	case <-closed:
	default:
		t.Fatal("server shutdown did not release the native file")
	}
}

func checkFTPExport(t *testing.T, address, passive, password string, checkAbort func()) *textproto.Conn {
	t.Helper()
	dialer := net.Dialer{Timeout: time.Second}
	dial := func(address string, source net.IP) net.Conn {
		t.Helper()
		dialer.LocalAddr = &net.TCPAddr{IP: source}
		conn, err := dialer.Dial("tcp4", address)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	control := textproto.NewConn(dial(address, net.IPv4(127, 0, 0, 1)))
	reply := func(want int) string {
		t.Helper()
		_, message, err := control.ReadResponse(want)
		if err != nil {
			t.Fatalf("FTP reply %d: %v", want, err)
		}
		return message
	}
	command := func(line string, want int) string {
		t.Helper()
		if err := control.PrintfLine("%s", line); err != nil {
			t.Fatal(err)
		}
		return reply(want)
	}
	reply(220)
	command("USER reader", 331)
	command("PASS wrong", 530)
	command("PASS "+password, 230)
	command("TYPE I", 200)
	command("PORT 127,0,0,1,1,1", 502)
	command("CWD ../publicity", 550)
	passivePort := func() {
		t.Helper()
		fields := strings.Split(command("EPSV", 229), "|")
		_, port, err := net.SplitHostPort(passive)
		if err != nil || len(fields) != 5 || fields[3] != port {
			t.Fatalf("passive endpoint: %v, %v", fields, err)
		}
	}
	passivePort()
	foreign := dial(passive, net.IPv4(127, 0, 0, 2))
	data := dial(passive, net.IPv4(127, 0, 0, 1))
	command("LIST", 150)
	// TCP reset or EOF both mean the foreign peer was rejected.
	body, err := io.ReadAll(foreign)
	var networkErr net.Error
	if len(body) != 0 || errors.As(err, &networkErr) && networkErr.Timeout() {
		t.Fatalf("foreign data peer: %q, %v", body, err)
	}
	listing, err := io.ReadAll(data)
	_ = data.Close()
	if err != nil || !strings.Contains(string(listing), "\tnote\r\n") {
		t.Fatalf("FTP LIST: %q, %v", listing, err)
	}
	reply(226)

	passivePort()
	data = dial(passive, net.IPv4(127, 0, 0, 1))
	command("STOR note", 150)
	if _, err := io.WriteString(data, "unpublished"); err != nil {
		t.Fatal(err)
	}
	if msg := command("ABOR", 426); !strings.Contains(msg, "No changes made") {
		t.Fatal(msg)
	}
	reply(226)
	_ = data.Close()
	checkAbort()
	passivePort() // Shutdown must release this unused data listener as well.
	return control
}

type exportFixture struct {
	*local.Driver
	closed chan<- struct{}
}

func (d *exportFixture) Open(ctx context.Context, addr core.NativeAddress, opts core.OpenOptions) (core.Opened, error) {
	opened, err := d.Driver.Open(ctx, addr, opts)
	if err == nil && opts.Mode == core.BoundRead {
		opened.File = &exportFixtureFile{opened.File, d.closed}
	}
	return opened, err
}

type exportFixtureFile struct {
	core.ReadFile
	closed chan<- struct{}
}

func (f *exportFixtureFile) Close() error {
	err := f.ReadFile.Close()
	select {
	case f.closed <- struct{}{}:
	default:
	}
	return err
}

func exportTLSFixture(t *testing.T, home string) *http.Client {
	t.Helper()
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	t.Cleanup(client.CloseIdleConnections)
	key, err := x509.MarshalPKCS8PrivateKey(server.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(home, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	writeFixture(t, filepath.Join(home, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	return client
}

func fixtureSSHKey(t *testing.T, name string) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, pem.EncodeToMemory(key), 0600); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func checkSFTPExport(t *testing.T, addresses []string, host, identity ssh.Signer, closed <-chan struct{}, checkUpload func()) func() {
	t.Helper()
	dial := func(address, user string, key ssh.PublicKey, signer ssh.Signer) (*ssh.Client, error) {
		raw, err := net.DialTimeout("tcp", address, 3*time.Second)
		if err != nil {
			return nil, err
		}
		_ = raw.SetDeadline(time.Now().Add(8 * time.Second))
		conn, channels, requests, err := ssh.NewClientConn(raw, address, &ssh.ClientConfig{
			User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(key),
		})
		if err != nil {
			_ = raw.Close()
			return nil, err
		}
		client := ssh.NewClient(conn, channels, requests)
		t.Cleanup(func() { _ = client.Close() })
		return client, nil
	}
	for _, auth := range []struct {
		user string
		host ssh.PublicKey
		key  ssh.Signer
	}{{"other-user", host.PublicKey(), identity}, {"reader", identity.PublicKey(), identity}, {"reader", host.PublicKey(), host}} {
		if client, err := dial(addresses[0], auth.user, auth.host, auth.key); err == nil {
			_ = client.Close()
			t.Fatal("SFTP accepted invalid SSH identity")
		}
	}
	client, err := dial(addresses[0], "reader", host.PublicKey(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Dial("tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("SSH forwarded a connection")
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Run("true"); err == nil {
		t.Fatal("SSH executed a command")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	connect := func(addr string) (*ssh.Client, *sftp.Client) {
		t.Helper()
		client, err := dial(addr, "reader", host.PublicKey(), identity)
		if err != nil {
			t.Fatal(err)
		}
		files, err := sftp.NewClient(client)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = files.Close() })
		return client, files
	}
	client, files := connect(addresses[0])
	entries, err := files.ReadDir("/")
	if err != nil || len(entries) != 1 || entries[0].Name() != "note" || entries[0].Mode().Perm() != 0600 {
		t.Fatalf("SFTP listing: %v, %v", entries, err)
	}
	for _, name := range []string{"/../publicity/private", "/publicity/private"} {
		if _, err := files.Open(name); err == nil {
			t.Fatalf("SFTP escaped the export at %q", name)
		}
	}
	download, err := files.Open("/note")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(download)
	if err != nil || string(body) != "same mount" {
		t.Fatalf("SFTP read: %q, %v", body, err)
	}
	upload, err := files.OpenFile("/upload", os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(upload, "from sftp"); err != nil {
		t.Fatal(err)
	}
	if err := upload.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := upload.Close(); err != nil {
		t.Fatal(err)
	}
	checkUpload()
	// The download reached EOF with a live handle. Disconnect the SSH transport
	// without sending SFTP CLOSE and wait for the client reader to observe it.
	select {
	case <-closed:
		t.Fatal("SFTP closed the file at EOF before disconnect")
	default:
	}
	_ = client.Close()
	_ = files.Wait()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("SSH disconnect did not release the native file")
	}

	_, readOnly := connect(addresses[1])
	info, err := readOnly.Stat("/note")
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatalf("SFTP read-only mode: %v, %v", info, err)
	}
	if _, err := readOnly.OpenFile("/denied", os.O_WRONLY|os.O_CREATE|os.O_EXCL); !os.IsPermission(err) {
		t.Fatalf("SFTP read-only write: %v", err)
	}
	return func() {
		// Keep a file open on the idle session for the forced shutdown check.
		if _, err := readOnly.Open("/again"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExportListenerFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MWEAVE_TEST_FAULT_TOKEN", "fixture-token")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	addrs := exportAddresses(t, 2)
	saved := configSnapshot{path: filepath.Join(home, "config.json"), value: config{
		Mounts: []mountRecord{{At: "/files", Driver: "local", Options: json.RawMessage(`{"root":"."}`)}},
	}}
	for _, addr := range addrs {
		saved.value.Exports = append(saved.value.Exports, export{Protocol: "http", Listen: addr, Root: "/files",
			Auth: exportAuth{Credential: &credentialRef{Kind: "env", Ref: "MWEAVE_TEST_FAULT_TOKEN"}}})
	}
	runtime, err := testApp.prepareExports(ctx, saved, nil)
	if err != nil {
		_ = runtime.shutdown(false, time.Second)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.shutdown(true, time.Second) })
	started, stopped := make(chan struct{}), make(chan struct{})
	runtime.endpoints[1].server.(*http.Server).Handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(stopped)
	})
	exited := make(chan error, 1)
	go func() { exited <- runtime.run(ctx) }()
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+addrs[1], nil)
		if res, err := http.DefaultClient.Do(req); err == nil {
			_ = res.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("second listener did not accept a request")
	}
	_ = runtime.endpoints[0].listener.Close()
	select {
	case err := <-exited:
		if err == nil || !strings.Contains(err.Error(), addrs[0]) {
			t.Fatalf("missing listener failure: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("listener failure did not reach foreground server")
	}
	if err := runtime.shutdown(true, time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("server did not cancel the active request")
	}
	assertExportPortsFree(t, addrs)
}

func exportAddresses(t *testing.T, count int) []string {
	t.Helper()
	var addrs []string
	for range count {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		addrs = append(addrs, l.Addr().String())
	}
	return addrs
}

func assertExportPortsFree(t *testing.T, addrs []string) {
	t.Helper()
	for _, addr := range addrs {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("listener still owns %s: %v", addr, err)
		}
		_ = l.Close()
	}
}
