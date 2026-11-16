package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/protocols/sftp"
	"golang.org/x/crypto/ssh"
)

type sftpExport struct {
	ctx       context.Context
	config    *ssh.ServerConfig
	files     *sftp.Server
	principal core.Principal
}

func prepareSFTPExport(ctx context.Context, c *core.Core, e export, p core.Principal, base string) (*tcpExport, error) {
	hostKey, err := readCredentialFile(hostPath(base, e.HostKeyFile))
	if err != nil {
		return nil, invalidConfig("The SFTP host key file could not be read.")
	}
	defer clear(hostKey)
	host, err := ssh.ParsePrivateKey(hostKey)
	if err != nil {
		return nil, invalidConfig("SFTP needs a valid unencrypted SSH host private key.")
	}
	clientKey, err := readCredentialFile(hostPath(base, e.Auth.PublicKeyFile))
	if err != nil {
		return nil, invalidConfig("The SFTP client public key file could not be read.")
	}
	line := bytes.TrimSpace(clientKey)
	key, _, options, _, err := ssh.ParseAuthorizedKey(line)
	if err != nil || len(options) != 0 || bytes.ContainsAny(line, "\r\n") {
		return nil, invalidConfig("SFTP needs one client SSH public key without options.")
	}
	if _, ok := key.(*ssh.Certificate); ok {
		return nil, invalidConfig("SFTP needs a client public key.")
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
