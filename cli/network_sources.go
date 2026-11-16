package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	smb2 "github.com/hirochachacha/go-smb2/v2"
	"github.com/hirochachacha/go-smb2/v2/auth"
	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/ftp"
	"github.com/nostalume/mountweave/drivers/sftp"
	"github.com/nostalume/mountweave/drivers/smb"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func storageAddress(input string, defaultPort int) (host string, port int, err error) {
	host, service, splitErr := net.SplitHostPort(input)
	if splitErr == nil {
		n, parseErr := strconv.ParseUint(service, 10, 16)
		if parseErr != nil || n == 0 {
			return "", 0, invalidConfig("The server port must be between 1 and 65535.")
		}
		port = int(n)
	} else {
		host = input
		port = defaultPort
	}
	if ip, ipErr := netip.ParseAddr(host); ipErr == nil {
		return ip.String(), port, nil
	}
	if host == "" || strings.ContainsAny(host, ":/\\@?#%[]") || strings.ContainsFunc(host, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return "", 0, invalidConfig("Enter a server hostname or IP address, optionally with a port.")
	}
	return host, port, nil
}

type sftpOptions struct {
	Address          string `json:"address"`
	Username         string `json:"username"`
	RootPath         string `json:"root_path,omitempty"`
	KnownHosts       string `json:"known_hosts,omitempty"`
	IdentityFile     string `json:"identity_file,omitempty"`
	MaxConnections   int    `json:"max_connections,omitempty"`
	MaxRequests      int    `json:"max_requests,omitempty"`
	MaxBufferedBytes int64  `json:"max_buffered_bytes,omitempty"`
}

func guideSFTP(o *sftpOptions, ask askField) error {
	err := askFields(ask,
		setupField{"SSH server (host or host:port)", &o.Address},
		setupField{"Username", &o.Username},
		setupField{"Remote folder (empty for home)", &o.RootPath},
		setupField{"Known hosts file (empty for your SSH default)", &o.KnownHosts},
		setupField{"Private key file (empty for password; - clears)", &o.IdentityFile},
	)
	if o.IdentityFile == "-" {
		o.IdentityFile = ""
	}
	return err
}

func prepareSFTP(o sftpOptions, base string) (mountSource, error) {
	host, port, err := storageAddress(o.Address, 22)
	if err != nil {
		return mountSource{}, err
	}
	root := o.RootPath
	if root == "" {
		root = "."
	}
	keyFile := ""
	if o.IdentityFile != "" {
		keyFile = hostPath(base, o.IdentityFile)
	}
	native := sftp.Options{
		Address: net.JoinHostPort(host, strconv.Itoa(port)), RootPath: root,
		MaxConnections: o.MaxConnections, MaxRequests: o.MaxRequests, MaxBufferedBytes: o.MaxBufferedBytes,
		// Validate only needs a non-nil policy. The actual callback is loaded before dialing.
		SSHConfig: &ssh.ClientConfig{User: o.Username, HostKeyCallback: rejectUnconfiguredHostKey},
	}
	if err := native.Validate(); err != nil || strings.ContainsAny(o.Username, "\x00\r\n") {
		return mountSource{}, invalidConfig("SFTP needs a username, valid remote folder and valid connection settings.")
	}
	return mountSource{
		audience: credentialAudience{origin: "sftp://" + strings.ToLower(native.Address), account: o.Username + "\x00" + keyFile},
		keyFile:  keyFile,
		open: func(ctx context.Context, secret string) (core.Driver, error) {
			trust := o.KnownHosts
			if trust == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return nil, invalidConfig("Set known_hosts to a trusted SSH hosts file.")
				}
				trust = filepath.Join(home, ".ssh", "known_hosts")
			}
			verify, err := knownhosts.New(hostPath(base, trust))
			if err != nil {
				return nil, &problem{Code: "host_key_unavailable", Message: "The SSH known hosts file could not be loaded.", NextAction: "Check known_hosts and the server's trusted host key."}
			}
			config := *native.SSHConfig
			config.HostKeyCallback = verify
			if keyFile == "" {
				config.Auth = []ssh.AuthMethod{ssh.Password(secret)}
			} else {
				signer, err := sshIdentity(keyFile, secret)
				if err != nil {
					return nil, err
				}
				config.Auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
			}
			options := native
			options.SSHConfig = &config
			d, err := sftp.Open(ctx, options)
			var mismatch *knownhosts.KeyError
			var revoked *knownhosts.RevokedError
			if errors.As(err, &mismatch) || errors.As(err, &revoked) {
				err = &problem{Code: "host_key_rejected", Message: "The SSH server's host key was not accepted.", NextAction: "Verify the server fingerprint and update the trusted known_hosts file."}
			}
			return nativeDriver(d, err)
		},
	}, nil
}

func rejectUnconfiguredHostKey(string, net.Addr, ssh.PublicKey) error {
	return os.ErrPermission
}

func sshIdentity(path, passphrase string) (ssh.Signer, error) {
	data, err := readCredentialFile(path)
	if err == nil {
		var signer ssh.Signer
		if passphrase == "" {
			signer, err = ssh.ParsePrivateKey(data)
		} else {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
		}
		if err == nil {
			return signer, nil
		}
	}
	return nil, &problem{Code: "credential_unavailable", Message: "The SSH private key could not be loaded or unlocked.", NextAction: "Check identity_file and the passphrase reference."}
}

type ftpOptions struct {
	Address        string `json:"address"`
	Username       string `json:"username"`
	RootPath       string `json:"root_path,omitempty"`
	Security       string `json:"security,omitempty"`
	CAFile         string `json:"ca_file,omitempty"`
	MaxConnections int    `json:"max_connections,omitempty"`
}

func guideFTP(o *ftpOptions, ask askField) error {
	if o.Security == "" {
		o.Security = "explicit_tls"
	}
	if err := askFields(ask,
		setupField{"FTP server (host or host:port)", &o.Address},
		setupField{"Username", &o.Username},
		setupField{"Remote folder (empty for /)", &o.RootPath},
	); err != nil {
		return err
	}
	var err error
	o.Security, err = askChoice(ask, "Transport (explicit_tls, implicit_tls or plain)", o.Security, "explicit_tls", "implicit_tls", "plain")
	if err != nil {
		return err
	}
	if o.Security == "plain" {
		o.CAFile = ""
		return nil
	}
	return askFields(ask, setupField{"TLS CA file (empty for system trust)", &o.CAFile})
}

func prepareFTP(o ftpOptions, base string) (mountSource, error) {
	security, port := ftp.ExplicitTLS, 21
	switch o.Security {
	case "", "explicit_tls":
	case "implicit_tls":
		security, port = ftp.ImplicitTLS, 990
	case "plain":
		security = ftp.Plain
	default:
		return mountSource{}, invalidConfig("Select explicit_tls, implicit_tls or plain for FTP.")
	}
	host, port, err := storageAddress(o.Address, port)
	if err != nil {
		return mountSource{}, err
	}
	root := o.RootPath
	if root == "" {
		root = "/"
	}
	native := ftp.Options{Address: net.JoinHostPort(host, strconv.Itoa(port)), Username: o.Username,
		RootPath: root, Security: security, MaxConnections: o.MaxConnections}
	if native.Validate() != nil || (security == ftp.Plain && o.CAFile != "") {
		return mountSource{}, invalidConfig("FTP needs a username, a valid remote folder and compatible TLS settings.")
	}
	return mountSource{
		audience: credentialAudience{origin: "ftp " + strconv.Itoa(int(security)) + " " + strings.ToLower(native.Address), account: o.Username},
		open: func(ctx context.Context, password string) (core.Driver, error) {
			options := native
			options.Password = password
			if security != ftp.Plain {
				options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
				if o.CAFile != "" {
					pem, err := readCredentialFile(hostPath(base, o.CAFile))
					certs := x509.NewCertPool()
					if err != nil || !certs.AppendCertsFromPEM(pem) {
						return nil, invalidConfig("The FTPS ca_file could not be loaded as PEM certificates.")
					}
					options.TLSConfig.RootCAs = certs
				}
			}
			return nativeDriver(ftp.Open(ctx, options))
		},
	}, nil
}

type smbOptions struct {
	Server           string `json:"server"`
	Share            string `json:"share"`
	Username         string `json:"username"`
	Domain           string `json:"domain,omitempty"`
	RootPath         string `json:"root_path,omitempty"`
	MaxSessions      int    `json:"max_sessions,omitempty"`
	MaxBufferedBytes int64  `json:"max_buffered_bytes,omitempty"`
}

func guideSMB(o *smbOptions, ask askField) error {
	return askFields(ask,
		setupField{"SMB server (host or host:port)", &o.Server},
		setupField{"Share name", &o.Share},
		setupField{"Username", &o.Username},
		setupField{"Domain (empty for server default)", &o.Domain},
		setupField{"Folder within the share (empty for share root)", &o.RootPath},
	)
}

func prepareSMB(o smbOptions, _ string) (mountSource, error) {
	host, port, err := storageAddress(o.Server, 445)
	if err != nil {
		return mountSource{}, err
	}
	account := auth.NTLMCredential{User: o.Username}
	if o.Domain != "" {
		account.Domain = &o.Domain
	}
	dialer := smb2.Dialer{Credentials: account, RequireMessageSigning: true, TransportDialer: smb2.TCPDialer{Port: port}}
	native := smb.Options{Server: host, Share: o.Share, RootPath: o.RootPath, Dialer: &dialer,
		MaxSessions: o.MaxSessions, MaxBufferedBytes: o.MaxBufferedBytes}
	if o.Username == "" || strings.ContainsAny(o.Username+o.Domain, "\x00\r\n") || native.Validate() != nil {
		return mountSource{}, invalidConfig("SMB needs a share, username, and valid share-relative folder and session settings.")
	}
	return mountSource{
		audience: credentialAudience{origin: "smb://" + strings.ToLower(net.JoinHostPort(host, strconv.Itoa(port))), account: o.Domain + "\x00" + o.Username},
		open: func(ctx context.Context, password string) (core.Driver, error) {
			credentials := account
			credentials.Password = password
			client := dialer
			client.Credentials = credentials
			options := native
			options.Dialer = &client
			return nativeDriver(smb.Open(ctx, options))
		},
	}, nil
}
