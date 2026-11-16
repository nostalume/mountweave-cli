package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/nostalume/mountweave"
	ftpexport "github.com/nostalume/mountweave/protocols/ftp"
	sftpexport "github.com/nostalume/mountweave/protocols/sftp"
	"github.com/sonroyaalmerol/go-smb-server/smb/ntlmssp"
	smbserver "github.com/sonroyaalmerol/go-smb-server/smb/server"
	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func writeFixture(t *testing.T, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(name, content, 0600); err != nil {
		t.Fatal(err)
	}
}

func storageFixture(t *testing.T, serve func(context.Context, net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var sessions sync.WaitGroup
	sessions.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			sessions.Go(func() {
				defer conn.Close()
				defer context.AfterFunc(ctx, func() { _ = conn.Close() })()
				serve(ctx, conn)
			})
		}
	})
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		sessions.Wait()
	})
	return listener.Addr().String()
}

func TestNetworkStorageSources(t *testing.T) {
	const password = "synthetic-source-password"
	home := t.TempDir()
	configPath := filepath.Join(home, "config.json")
	root := filepath.Join(home, "files")
	if err := os.MkdirAll(filepath.Join(root, "selected"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, "selected", "note"), []byte("fixture-body"))
	writeFixture(t, filepath.Join(root, "outside"), []byte("outside"))
	upstream, err := testApp.openFor(context.Background(), configSnapshot{path: configPath, value: config{
		Mounts: []mountRecord{{At: "/data", Driver: "local", Options: json.RawMessage(`{"root":"files"}`)}},
	}}, core.Path("/data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeRuntime(upstream); err != nil {
			t.Error(err)
		}
	})
	// Separate references keep each credential associated with one recipient.
	for _, ref := range []string{"MWEAVE_TEST_FTP", "MWEAVE_TEST_FTPS", "MWEAVE_TEST_IMPLICIT", "MWEAVE_TEST_SFTP", "MWEAVE_TEST_SMB"} {
		t.Setenv(ref, password)
	}
	const ref = `,"credential":{"kind":"env","ref":%q}`

	tlsFixture := httptest.NewTLSServer(nil)
	cert := &tls.Config{Certificates: tlsFixture.TLS.Certificates}
	writeFixture(t, filepath.Join(home, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsFixture.Certificate().Raw}))
	tlsFixture.Close()
	for _, profile := range []struct{ name, security, ref, ca string }{
		{"ftp", "plain", "MWEAVE_TEST_FTP", ""},
		{"ftps", "", "MWEAVE_TEST_FTPS", "ca.pem"},
		{"implicit", "implicit_tls", "MWEAVE_TEST_IMPLICIT", "ca.pem"},
	} {
		opts := ftpexport.Options{Root: core.Path("/data"), CursorTTL: time.Minute, DataConnectTimeout: time.Second,
			Limits: ftpexport.Limits{CommandBytes: 4096, TransferBufferBytes: 4096, PageNameBytes: 8192},
			Authenticate: func(_ context.Context, l ftpexport.Login) (core.Principal, error) {
				if l.Username != "fixture" || l.Password != password || (l.TLS != nil) != (profile.ca != "") {
					return "", errors.New("invalid fixture account or transport")
				}
				return localActor, nil
			},
			ListenPassive: func(ctx context.Context, _, _ net.Addr) (net.Listener, netip.AddrPort, error) {
				l, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:0")
				if err != nil {
					return nil, netip.AddrPort{}, err
				}
				return l, l.Addr().(*net.TCPAddr).AddrPort(), nil
			},
		}
		if profile.ca != "" {
			opts.TLSConfig = cert
		}
		export, err := ftpexport.New(upstream, opts)
		if err != nil {
			t.Fatal(err)
		}
		addr := storageFixture(t, func(ctx context.Context, conn net.Conn) {
			if profile.security == "implicit_tls" {
				conn = tls.Server(conn, cert)
			}
			_ = export.ServeConn(ctx, conn)
		})
		record := fmt.Sprintf(`{"at":"/%s","driver":"ftp","options":{"address":%q,"username":"fixture","root_path":"/selected","security":%q,"ca_file":%q}`+ref+`}`, profile.name, addr, profile.security, profile.ca, profile.ref)
		invokeCLI(t, configPath, record, false, 0, "mount", "add", "--from", "-")
	}

	sftp, err := sftpexport.New(upstream, sftpexport.Options{Root: core.Path("/data"), CursorTTL: time.Minute,
		ModeView: sftpexport.ReadWriteView, Limits: sftpexport.Limits{Handles: 8, PageNameBytes: 8192}})
	if err != nil {
		t.Fatal(err)
	}
	hostKey, _ := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
	var authCalls atomic.Int64
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	userKey, _ := ssh.NewPublicKey(key.Public())
	sshConfig := &ssh.ServerConfig{PublicKeyCallback: func(user ssh.ConnMetadata, public ssh.PublicKey) (*ssh.Permissions, error) {
		authCalls.Add(1)
		if user.User() != "fixture" || !bytes.Equal(public.Marshal(), userKey.Marshal()) {
			return nil, errors.New("unexpected SSH identity")
		}
		return nil, nil
	}}
	sshConfig.AddHostKey(hostKey)
	sshAddr := storageFixture(t, func(ctx context.Context, conn net.Conn) {
		server, channels, requests, err := ssh.NewServerConn(conn, sshConfig)
		if err != nil {
			return
		}
		defer server.Close()
		go ssh.DiscardRequests(requests)
		if incoming, ok := <-channels; ok {
			channel, requests, err := incoming.Accept()
			if err != nil {
				return
			}
			defer channel.Close()
			if req, ok := <-requests; ok {
				var subsystem struct{ Name string }
				ok := req.Type == "subsystem" && ssh.Unmarshal(req.Payload, &subsystem) == nil && subsystem.Name == "sftp"
				_ = req.Reply(ok, nil)
				if ok {
					_ = sftp.ServeConn(ctx, localActor, channel)
				}
			}
		}
	})
	keyBlock, err := ssh.MarshalPrivateKeyWithPassphrase(key, "fixture", []byte(password))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(home, "identity"), pem.EncodeToMemory(keyBlock))
	writeFixture(t, filepath.Join(home, "hosts"), []byte(knownhosts.Line([]string{sshAddr}, userKey)+"\n"))
	sshRecord := fmt.Sprintf(`{"at":"/ssh","driver":"sftp","options":{"address":%q,"username":"fixture","root_path":"/selected","known_hosts":"hosts","identity_file":"identity"}`+ref+`}`, sshAddr, "MWEAVE_TEST_SFTP")
	got := invokeCLI(t, configPath, sshRecord, false, 1, "mount", "add", "--from", "-", "--json")
	if authCalls.Load() != 0 || got.Error == nil || got.Error.Code != "host_key_rejected" {
		t.Fatalf("untrusted SSH host: auth calls=%d, error=%+v", authCalls.Load(), got.Error)
	}
	writeFixture(t, filepath.Join(home, "hosts"), []byte(knownhosts.Line([]string{sshAddr}, hostKey.PublicKey())+"\n"))
	invokeCLI(t, configPath, sshRecord, false, 0, "mount", "add", "--from", "-")

	smbAddr := smbStorageFixture(t, root, password)
	smbRecord := fmt.Sprintf(`{"at":"/smb","driver":"smb","options":{"server":%q,"share":"data","username":"fixture","domain":"FIXTURE","root_path":"/selected"}`+ref+`}`, smbAddr, "MWEAVE_TEST_SMB")
	invokeCLI(t, configPath, smbRecord, false, 0, "mount", "add", "--from", "-")

	for _, at := range []string{"/ftp", "/ftps", "/implicit", "/ssh", "/smb"} {
		page := invokeCLI(t, configPath, "", false, 0, "ls", at, "--json")
		if !bytes.Contains(page.Result, []byte("note")) || bytes.Contains(page.Result, []byte("outside")) {
			t.Fatalf("%s root: %s", at, page.Result)
		}
		assertSourceBody(t, configPath, at+"/note", "fixture-body")
	}
	invokeCLI(t, configPath, "", false, 0, "mkdir", "/ftps/new")
	invokeCLI(t, configPath, "new", false, 1, "put", "-", "/ftps/note")
	for _, at := range []string{"/ssh", "/smb"} {
		address := at + "/new-" + at[1:]
		invokeCLI(t, configPath, "created", false, 0, "put", "-", address)
		invokeCLI(t, configPath, "overwrite", false, 1, "put", "-", address)
		got := invokeCLI(t, configPath, "overwrite", false, 1, "put", "-", address, "--replace", "--json")
		if got.Error == nil || got.Error.Code != string(core.UnsupportedOperation) {
			t.Fatalf("%s complete replacement: %+v", at, got.Error)
		}
		assertSourceBody(t, configPath, address, "created")
	}
}

func smbStorageFixture(t *testing.T, root, password string) string {
	t.Helper()
	local, err := vfs.NewLocalBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	credentials := ntlmssp.NewMemoryCredentials()
	credentials.Add("FIXTURE", "fixture", password)
	ready := make(smbReady, 1)
	server, err := smbserver.New(smbserver.WithAddr("127.0.0.1:0"),
		smbserver.WithAuth(ntlmssp.NewServer(credentials, "fixture")),
		smbserver.WithShares(vfs.NewDiskShare("data", smbFixtureDisk{local})),
		smbserver.WithLogger(slog.New(slog.NewTextHandler(ready, nil))))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("SMB fixture did not stop")
		}
	})
	select {
	case addr := <-ready:
		return addr
	case err := <-done:
		done <- err
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("SMB fixture did not start")
	}
	return ""
}

type smbReady chan string

func (w smbReady) Write(p []byte) (int, error) {
	if _, tail, ok := strings.Cut(string(p), "addr="); ok {
		select {
		case w <- strings.TrimSpace(tail):
		default:
		}
	}
	return len(p), nil
}

type smbFixtureDisk struct{ *vfs.LocalBackend }

func (d smbFixtureDisk) Open(ctx context.Context, opts vfs.OpenOptions) (vfs.Handle, error) {
	opts.Path = strings.ReplaceAll(opts.Path, "\\", "/")
	// The fixture backend opens newly created files read-only unless they exist.
	if opts.Disposition == vfs.DispositionCreate && !opts.CreateDir {
		f, err := os.OpenFile(filepath.Join(d.Root, filepath.FromSlash(opts.Path)), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		opts.Disposition = vfs.DispositionOpen
	}
	return d.LocalBackend.Open(ctx, opts)
}
