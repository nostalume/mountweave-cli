package cliapp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/protocols/ftp"
	"github.com/nostalume/mountweave/protocols/sftp"
	"golang.org/x/crypto/ssh"
)

type tcpExport struct {
	serveConn func(net.Conn)
	mu        sync.Mutex
	clients   map[net.Conn]struct{}
	stopping  bool
	done      chan struct{}
}

func newTCPExport(serve func(net.Conn)) *tcpExport {
	return &tcpExport{serveConn: serve, clients: make(map[net.Conn]struct{}), done: make(chan struct{})}
}

func (s *tcpExport) Serve(listener net.Listener) error {
	const maxConnections = 32
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		s.mu.Lock()
		if s.stopping || len(s.clients) >= maxConnections {
			s.mu.Unlock()
			_ = conn.Close()
			continue
		}
		s.clients[conn] = struct{}{}
		s.mu.Unlock()
		go func() {
			s.serveConn(conn)
			s.mu.Lock()
			delete(s.clients, conn)
			if s.stopping && len(s.clients) == 0 {
				close(s.done)
			}
			s.mu.Unlock()
		}()
	}
}

func (s *tcpExport) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.stopping {
		s.stopping = true
		if len(s.clients) == 0 {
			close(s.done)
		}
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *tcpExport) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	for conn := range s.clients {
		if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			err = errors.Join(err, closeErr)
		}
	}
	return err
}

func prepareFTPExport(ctx context.Context, c *core.Core, e Export, p core.Principal, secret string, tlsConfig *tls.Config) (*tcpExport, error) {
	if strings.ContainsFunc(secret, func(r rune) bool { return r < 32 || r == 127 }) {
		return nil, InvalidConfig("The FTP password cannot contain ASCII control characters.")
	}
	matches := passwordMatches(e.Auth.Username, secret)
	options := ftp.Options{
		Root: core.Path(e.Root), CursorTTL: time.Minute, DataConnectTimeout: 10 * time.Second,
		Limits: ftp.Limits{CommandBytes: 4096, TransferBufferBytes: 32 << 10, PageNameBytes: 1 << 20},
		Authenticate: func(_ context.Context, login ftp.Login) (core.Principal, error) {
			if matches(login.Username, login.Password) {
				return p, nil
			}
			return "", core.Fail(core.Unauthenticated, nil)
		},
		ListenPassive: e.listenPassive,
		TLSConfig:     tlsConfig,
	}
	files, err := ftp.New(c, options)
	if err != nil {
		return nil, err
	}
	if tlsConfig != nil {
		files, err = files.WithPrivateData()
		if err != nil {
			return nil, err
		}
	}
	return newTCPExport(func(conn net.Conn) { _ = files.ServeConn(ctx, conn) }), nil
}

func (e Export) listenPassive(ctx context.Context, local, remote net.Addr) (net.Listener, netip.AddrPort, error) {
	ip := local.(*net.TCPAddr).AddrPort().Addr().Unmap()
	peer := remote.(*net.TCPAddr).AddrPort().Addr().Unmap()
	network := net.ListenConfig{}
	host := ip.String()
	first, last := 0, 0
	if e.PassivePorts != nil {
		first, last = int(e.PassivePorts.First), int(e.PassivePorts.Last)
	}
	for port := first; port <= last; port++ {
		listener, err := network.Listen(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err == nil {
			endpoint := listener.Addr().(*net.TCPAddr).AddrPort()
			return passivePeer{listener, peer}, netip.AddrPortFrom(ip, endpoint.Port()), nil
		}
		if ctx.Err() != nil {
			return nil, netip.AddrPort{}, ctx.Err()
		}
	}
	return nil, netip.AddrPort{}, core.Fail(core.CapacityExceeded, nil)
}

type passivePeer struct {
	net.Listener
	ip netip.Addr
}

func (l passivePeer) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if conn.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap() == l.ip {
			return conn, nil
		}
		_ = conn.Close()
	}
}

type sftpExport struct {
	ctx       context.Context
	config    *ssh.ServerConfig
	files     *sftp.Server
	principal core.Principal
}

func prepareSFTPExport(ctx context.Context, c *core.Core, e Export, p core.Principal, base string) (*tcpExport, error) {
	hostPath, clientPath := e.sshKeyPaths(base)
	hostKey, err := ReadCredentialFile(hostPath)
	if err != nil {
		return nil, InvalidConfig("The SFTP host key file could not be read.")
	}
	defer clear(hostKey)
	host, err := ssh.ParsePrivateKey(hostKey)
	if err != nil {
		return nil, InvalidConfig("SFTP needs a valid unencrypted SSH host private key.")
	}
	clientKey, err := ReadCredentialFile(clientPath)
	if err != nil {
		return nil, InvalidConfig("The SFTP client public key file could not be read.")
	}
	line := bytes.TrimSpace(clientKey)
	key, _, options, _, err := ssh.ParseAuthorizedKey(line)
	if err != nil || len(options) != 0 || bytes.ContainsAny(line, "\r\n") {
		return nil, InvalidConfig("SFTP needs one client SSH public key without options.")
	}
	if _, ok := key.(*ssh.Certificate); ok {
		return nil, InvalidConfig("SFTP needs a client public key.")
	}
	trusted := key.Marshal()
	config := &ssh.ServerConfig{PublicKeyCallback: func(conn ssh.ConnMetadata, candidate ssh.PublicKey) (*ssh.Permissions, error) {
		if conn.User() == e.Auth.Username && bytes.Equal(trusted, candidate.Marshal()) {
			return nil, nil
		}
		return nil, core.Fail(core.Unauthenticated, nil)
	}}
	config.AddHostKey(host)
	mode := sftp.ReadOnlyView
	if e.Access == "read_write" {
		mode = sftp.ReadWriteView
	}
	files, err := sftp.New(c, sftp.Options{
		Root: core.Path(e.Root), ModeView: mode, CursorTTL: time.Minute,
		Limits: sftp.Limits{Handles: 16, PageNameBytes: 1 << 20},
	})
	if err != nil {
		return nil, err
	}
	session := &sftpExport{ctx: ctx, config: config, files: files, principal: p}
	return newTCPExport(session.serveSSH), nil
}

func (s *sftpExport) serveSSH(raw net.Conn) {
	ctx, cancel := context.WithCancel(s.ctx)
	var sessionDone chan struct{}
	var peerDone chan struct{}
	var channels <-chan ssh.NewChannel
	var requests, channelRequests <-chan *ssh.Request
	defer func() {
		cancel()
		_ = raw.Close()
		// SSH's mux may be trying to publish a queued request while the
		// socket closes. Drain its queues until the transport has exited.
		for peerDone != nil {
			select {
			case <-peerDone:
				peerDone = nil
			case _, ok := <-channels:
				if !ok {
					channels = nil
				}
			case _, ok := <-requests:
				if !ok {
					requests = nil
				}
			case _, ok := <-channelRequests:
				if !ok {
					channelRequests = nil
				}
			}
		}
		if sessionDone != nil {
			<-sessionDone
		}
	}()

	// The deadline covers handshake and selection of the SFTP subsystem.
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	conn, incoming, globalRequests, err := ssh.NewServerConn(raw, s.config)
	if err != nil {
		return
	}
	channels, requests = incoming, globalRequests
	peerDone = make(chan struct{})
	go func(done chan struct{}) {
		defer close(done)
		_ = conn.Wait()
		cancel()
	}(peerDone)
	var channel ssh.Channel
	for {
		select {
		case <-ctx.Done():
			return
		case <-sessionDone:
			return
		case r, ok := <-requests:
			if !ok {
				return
			}
			_ = r.Reply(false, nil)
		case next, ok := <-channels:
			if !ok {
				return
			}
			if channel != nil || next.ChannelType() != "session" {
				_ = next.Reject(ssh.Prohibited, "SFTP session only")
				continue
			}
			channel, channelRequests, err = next.Accept()
			if err != nil {
				return
			}
		case r, ok := <-channelRequests:
			if !ok {
				return
			}
			var subsystem struct{ Name string }
			accept := sessionDone == nil && r.Type == "subsystem" &&
				ssh.Unmarshal(r.Payload, &subsystem) == nil && subsystem.Name == "sftp"
			if err := r.Reply(accept, nil); err != nil {
				return
			}
			if accept {
				_ = raw.SetDeadline(time.Time{})
				sessionDone = make(chan struct{})
				go func() {
					defer close(sessionDone)
					_ = s.files.ServeConn(ctx, s.principal, sftpChannel{channel, raw})
				}()
			}
		}
	}
}

type sftpChannel struct {
	ssh.Channel
	transport net.Conn
}

// The channel owns this dedicated SSH transport. Closing the socket wakes
// blocked reads and writes even if the peer never acknowledges SSH close.
func (c sftpChannel) Close() error {
	err := c.transport.Close()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
