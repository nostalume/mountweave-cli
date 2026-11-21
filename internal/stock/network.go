package stock

import (
	"cmp"
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
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave/drivers/ftp"
	"github.com/nostalume/mountweave/drivers/sftp"
	"github.com/nostalume/mountweave/drivers/smb"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func storageAddress(input string, port int) (string, int, error) {
	host, service, err := net.SplitHostPort(input)
	if err != nil {
		host = input
	} else {
		n, err := strconv.ParseUint(service, 10, 16)
		if err != nil || n == 0 {
			return "", 0, cliapp.InvalidConfig("The server port must be between 1 and 65535.")
		}
		port = int(n)
	}
	if ip, ipErr := netip.ParseAddr(host); ipErr == nil {
		return ip.String(), port, nil
	}
	if host == "" || strings.ContainsAny(host, ":/\\@?#%[]") || strings.ContainsFunc(host, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return "", 0, cliapp.InvalidConfig("Enter a server hostname or IP address, optionally with a port.")
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

func guideSFTP(o *sftpOptions, ask cliapp.Prompt) error {
	if err := cliapp.AskFields(ask,
		cliapp.Field("SSH server (host or host:port)", &o.Address),
		cliapp.Field("Username", &o.Username),
		cliapp.Field("Remote folder (empty for home)", &o.RootPath),
		cliapp.Field("Known hosts file (empty for your SSH default)", &o.KnownHosts),
	); err != nil {
		return err
	}
	return askClearable(ask, "Private key file (empty for password; - clears)", &o.IdentityFile)
}

func prepareSFTP(o sftpOptions, base string) (cliapp.MountSource, error) {
	host, port, err := storageAddress(o.Address, 22)
	if err != nil {
		return cliapp.MountSource{}, err
	}
	keyFile := cliapp.OptionalHostPath(base, o.IdentityFile)
	trust, trustErr := o.trustFile(base)
	native := sftp.Options{
		Address: net.JoinHostPort(host, strconv.Itoa(port)), RootPath: cmp.Or(o.RootPath, "."),
		MaxConnections: o.MaxConnections, MaxRequests: o.MaxRequests, MaxBufferedBytes: o.MaxBufferedBytes,
		// Validate only needs a non-nil policy. The actual callback is loaded before dialing.
		SSHConfig: &ssh.ClientConfig{User: o.Username, HostKeyCallback: rejectUnconfiguredHostKey},
	}
	if err := native.Validate(); err != nil || strings.ContainsAny(o.Username, "\x00\r\n") {
		return cliapp.MountSource{}, cliapp.InvalidConfig("SFTP needs a username, valid remote folder and valid connection settings.")
	}
	return cliapp.MountSource{
		Audience: cliapp.CredentialAudience{Origin: "sftp://" + strings.ToLower(native.Address), Account: o.Username + "\x00" + keyFile},
		KeyFile:  keyFile,
		Open: func(ctx context.Context, secret string) (core.Driver, error) {
			if trustErr != nil {
				return nil, trustErr
			}
			verify, err := knownhosts.New(trust)
			if err != nil {
				return nil, &cliapp.Problem{Code: "host_key_unavailable", Message: "The SSH known hosts file could not be loaded.", NextAction: "Check known_hosts and the server's trusted host key."}
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
				err = &cliapp.Problem{Code: "host_key_rejected", Message: "The SSH server's host key was not accepted.", NextAction: "Verify the server fingerprint and update the trusted known_hosts file."}
			}
			return cliapp.NativeDriver(d, err)
		},
	}, nil
}

func (o sftpOptions) trustFile(base string) (string, error) {
	if o.KnownHosts != "" {
		return cliapp.HostPath(base, o.KnownHosts), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", cliapp.InvalidConfig("Set known_hosts to a trusted SSH hosts file.")
	}
	return cliapp.HostPath(base, filepath.Join(home, ".ssh", "known_hosts")), nil
}

func rejectUnconfiguredHostKey(string, net.Addr, ssh.PublicKey) error {
	return os.ErrPermission
}

func sshIdentity(path, passphrase string) (ssh.Signer, error) {
	data, err := cliapp.ReadCredentialFile(path)
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
	return nil, &cliapp.Problem{Code: "credential_unavailable", Message: "The SSH private key could not be loaded or unlocked.", NextAction: "Check identity_file and the passphrase reference."}
}

type ftpOptions struct {
	Address        string `json:"address"`
	Username       string `json:"username"`
	RootPath       string `json:"root_path,omitempty"`
	Security       string `json:"security,omitempty"`
	CAFile         string `json:"ca_file,omitempty"`
	MaxConnections int    `json:"max_connections,omitempty"`
}

func guideFTP(o *ftpOptions, ask cliapp.Prompt) error {
	if err := cliapp.AskFields(ask,
		cliapp.Field("FTP server (host or host:port)", &o.Address),
		cliapp.Field("Username", &o.Username),
		cliapp.Field("Remote folder (empty for /)", &o.RootPath),
	); err != nil {
		return err
	}
	var err error
	o.Security, err = cliapp.AskChoice(ask, "Transport (explicit_tls, implicit_tls or plain)", cmp.Or(o.Security, "explicit_tls"), "explicit_tls", "implicit_tls", "plain")
	if err != nil {
		return err
	}
	if o.Security == "plain" {
		o.CAFile = ""
		return nil
	}
	return cliapp.AskFields(ask, cliapp.Field("TLS CA file (empty for system trust)", &o.CAFile))
}

func prepareFTP(o ftpOptions, base string) (cliapp.MountSource, error) {
	security, port := ftp.ExplicitTLS, 21
	switch o.Security {
	case "", "explicit_tls":
	case "implicit_tls":
		security, port = ftp.ImplicitTLS, 990
	case "plain":
		security = ftp.Plain
	default:
		return cliapp.MountSource{}, cliapp.InvalidConfig("Select explicit_tls, implicit_tls or plain for FTP.")
	}
	host, port, err := storageAddress(o.Address, port)
	if err != nil {
		return cliapp.MountSource{}, err
	}
	native := ftp.Options{Address: net.JoinHostPort(host, strconv.Itoa(port)), Username: o.Username,
		RootPath: cmp.Or(o.RootPath, "/"), Security: security, MaxConnections: o.MaxConnections}
	if native.Validate() != nil || (security == ftp.Plain && o.CAFile != "") {
		return cliapp.MountSource{}, cliapp.InvalidConfig("FTP needs a username, a valid remote folder and compatible TLS settings.")
	}
	caFile := cliapp.OptionalHostPath(base, o.CAFile)
	audience := cliapp.CredentialAudience{Origin: "ftp " + strconv.Itoa(int(security)) + " " + strings.ToLower(native.Address), Account: o.Username}
	return cliapp.SecretSource(audience, "", func(ctx context.Context, password string) (core.Driver, error) {
		options := native
		options.Password = password
		if security != ftp.Plain {
			options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			if caFile != "" {
				pem, err := cliapp.ReadCredentialFile(caFile)
				certs := x509.NewCertPool()
				if err != nil || !certs.AppendCertsFromPEM(pem) {
					return nil, cliapp.InvalidConfig("The FTPS ca_file could not be loaded as PEM certificates.")
				}
				options.TLSConfig.RootCAs = certs
			}
		}
		return cliapp.NativeDriver(ftp.Open(ctx, options))
	}), nil
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

func guideSMB(o *smbOptions, ask cliapp.Prompt) error {
	return cliapp.AskFields(ask,
		cliapp.Field("SMB server (host or host:port)", &o.Server),
		cliapp.Field("Share name", &o.Share),
		cliapp.Field("Username", &o.Username),
		cliapp.Field("Domain (empty for server default)", &o.Domain),
		cliapp.Field("Folder within the share (empty for share root)", &o.RootPath),
	)
}

func prepareSMB(o smbOptions, _ string) (cliapp.MountSource, error) {
	host, port, err := storageAddress(o.Server, 445)
	if err != nil {
		return cliapp.MountSource{}, err
	}
	account := auth.NTLMCredential{User: o.Username}
	if o.Domain != "" {
		account.Domain = &o.Domain
	}
	dialer := smb2.Dialer{Credentials: account, RequireMessageSigning: true, TransportDialer: smb2.TCPDialer{Port: port}}
	native := smb.Options{Server: host, Share: o.Share, RootPath: o.RootPath, Dialer: &dialer,
		MaxSessions: o.MaxSessions, MaxBufferedBytes: o.MaxBufferedBytes}
	if o.Username == "" || strings.ContainsAny(o.Username+o.Domain, "\x00\r\n") || native.Validate() != nil {
		return cliapp.MountSource{}, cliapp.InvalidConfig("SMB needs a share, username, and valid share-relative folder and session settings.")
	}
	audience := cliapp.CredentialAudience{Origin: "smb://" + strings.ToLower(net.JoinHostPort(host, strconv.Itoa(port))), Account: o.Domain + "\x00" + o.Username}
	return cliapp.SecretSource(audience, "", func(ctx context.Context, password string) (core.Driver, error) {
		credentials := account
		credentials.Password = password
		client := dialer
		client.Credentials = credentials
		options := native
		options.Dialer = &client
		return cliapp.NativeDriver(smb.Open(ctx, options))
	}), nil
}
