package cli

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/protocols/ftp"
)

func prepareFTPExport(ctx context.Context, c *core.Core, e export, p core.Principal, secret string, tlsConfig *tls.Config) (*tcpExport, error) {
	if strings.ContainsFunc(secret, func(r rune) bool { return r < 32 || r == 127 }) {
		return nil, invalidConfig("The FTP password cannot contain ASCII control characters.")
	}
	user, password := sha256.Sum256([]byte(e.Auth.Username)), sha256.Sum256([]byte(secret))
	options := ftp.Options{
		Root: core.Path(e.Root), CursorTTL: time.Minute, DataConnectTimeout: 10 * time.Second,
		Limits: ftp.Limits{CommandBytes: 4096, TransferBufferBytes: 32 << 10, PageNameBytes: 1 << 20},
		Authenticate: func(_ context.Context, login ftp.Login) (core.Principal, error) {
			gotUser, gotPassword := sha256.Sum256([]byte(login.Username)), sha256.Sum256([]byte(login.Password))
			if subtle.ConstantTimeCompare(user[:], gotUser[:])&subtle.ConstantTimeCompare(password[:], gotPassword[:]) == 1 {
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
		protected, ok := any(files).(interface{ WithPrivateData() (*ftp.Server, error) })
		if !ok {
			return nil, &problem{Code: "unsupported_operation", Message: "This build cannot enforce FTPS data protection.", NextAction: "Use a build with a Mountweave FTP adapter that supports private data connections."}
		}
		files, err = protected.WithPrivateData()
		if err != nil {
			return nil, err
		}
	}
	return newTCPExport(func(conn net.Conn) { _ = files.ServeConn(ctx, conn) }), nil
}

type portRange struct {
	First uint16 `json:"first"`
	Last  uint16 `json:"last"`
}

func (e export) listenPassive(ctx context.Context, local, remote net.Addr) (net.Listener, netip.AddrPort, error) {
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
