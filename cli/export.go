package cli

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/protocols/httpapi"
	"github.com/nostalume/mountweave/protocols/webdav"
)

type export struct {
	Protocol     string           `json:"protocol"`
	Listen       string           `json:"listen"`
	Root         string           `json:"root"`
	Access       string           `json:"access,omitempty"`
	Auth         exportAuth       `json:"auth"`
	TLS          *exportTLS       `json:"tls,omitempty"`
	HostKeyFile  string           `json:"host_key_file,omitempty"`
	PassivePorts *portRange       `json:"passive_ports,omitempty"`
	S3           *s3ExportOptions `json:"s3,omitempty"`
	CreateOnly   bool             `json:"create_only,omitempty"`
}

type exportAuth struct {
	Username      string         `json:"username,omitempty"`
	Credential    *credentialRef `json:"credential,omitempty"`
	PublicKeyFile string         `json:"public_key_file,omitempty"`
	AccessKeyID   string         `json:"access_key_id,omitempty"`
}

type exportTLS struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

var exportProtocols = []string{"webdav", "http", "sftp", "ftp", "s3"}

func (e export) validate() error {
	host, port, err := net.SplitHostPort(e.Listen)
	number, portErr := strconv.ParseUint(port, 10, 16)
	ref := e.Auth.Credential
	if err != nil || portErr != nil || number == 0 ||
		!slices.Contains(exportProtocols, e.Protocol) ||
		!slices.Contains([]string{"", "read", "read_write"}, e.Access) {
		return invalidConfig("An export needs a supported protocol, listener and access policy.")
	}
	if e.CreateOnly && (e.Protocol != "webdav" || e.Access != "read_write") {
		return invalidConfig("Create-only publication requires a read-write WebDAV export.")
	}
	if e.PassivePorts != nil && (e.Protocol != "ftp" || e.PassivePorts.First == 0 || e.PassivePorts.Last < e.PassivePorts.First) {
		return invalidConfig("FTP passive ports need an ordered range of ports from 1 to 65535.")
	}
	if e.Protocol == "s3" {
		if e.S3 == nil || !bucketName.MatchString(e.S3.Bucket) || e.S3.MaxPutBytes < 0 ||
			e.S3.Region != "" && !sigV4Name(e.S3.Region) || !sigV4Name(e.Auth.AccessKeyID) {
			return invalidConfig("S3 needs a bucket, a valid signing region and access key ID, and a nonnegative PUT limit.")
		}
		if m := e.S3.Multipart; m != nil && (e.Access != "read_write" || strings.TrimSpace(m.StateDirectory) == "" ||
			m.MaxStagingBytes < 4096 || m.MaxObjectBytes < 0 || m.MaxObjectBytes > m.MaxStagingBytes ||
			m.RetentionSeconds < 0 || m.RetentionSeconds > int64((1<<63-1)/time.Second)) {
			return invalidConfig("S3 multipart requires read_write access, a private state directory, a staging budget of at least 4096 bytes and valid optional limits.")
		}
	} else if e.S3 != nil || e.Auth.AccessKeyID != "" {
		return invalidConfig("Bucket and access key ID settings are only used by S3 exports.")
	}
	if e.Protocol == "sftp" {
		if e.TLS != nil || ref != nil || strings.TrimSpace(e.HostKeyFile) == "" || strings.TrimSpace(e.Auth.PublicKeyFile) == "" ||
			strings.TrimSpace(e.Auth.Username) == "" || !utf8.ValidString(e.Auth.Username) || strings.ContainsFunc(e.Auth.Username, unicode.IsControl) {
			return invalidConfig("SFTP needs an SSH host key file, username and client public key file.")
		}
		return nil
	}
	if e.HostKeyFile != "" || e.Auth.PublicKeyFile != "" {
		return invalidConfig("SSH key files are only used by SFTP exports.")
	}
	if !ref.valid() || (ref.Kind != "env" && ref.Kind != "file") {
		return invalidConfig("An export needs an env or file credential reference.")
	}
	user := e.Auth.Username
	if (e.Protocol == "webdav" || e.Protocol == "ftp") != (user != "") || !utf8.ValidString(user) || strings.ContainsFunc(user, unicode.IsControl) ||
		e.Protocol == "webdav" && strings.ContainsRune(user, ':') ||
		e.Protocol == "ftp" && strings.TrimSpace(user) == "" {
		return invalidConfig("WebDAV and FTP need a valid username. Use the protocol's authentication fields.")
	}
	if e.TLS != nil {
		if e.TLS.CertFile == "" || e.TLS.KeyFile == "" {
			return invalidConfig("An export TLS configuration needs certificate and key files.")
		}
	} else if ip := net.ParseIP(host); host == "" || (ip != nil && !ip.IsLoopback()) {
		return invalidConfig("A cleartext export must use a loopback listener.")
	}
	return nil
}

func (e export) checkAvailability() error {
	if e.CreateOnly && !webdavCreateOnlyAvailable {
		return createOnlyUnavailable()
	}
	return nil
}

func createOnlyUnavailable() error {
	return &problem{Code: string(core.UnsupportedOperation), Message: "Create-only WebDAV is unavailable in this build."}
}

func exportCredentialConflict() error {
	return &problem{Code: "credential_scope_conflict", Message: "An export credential is also referenced by a storage mount.", NextAction: "Use separate credentials for clients and storage."}
}

func (e export) containsMount(at string) bool {
	if e.Protocol == "s3" {
		return e.Root == at
	}
	return core.Within(e.Root, at)
}

func (e export) checkCredential(saved configSnapshot) error {
	if e.Auth.Credential == nil {
		return nil
	}
	base := filepath.Dir(saved.path)
	for _, mount := range saved.value.Mounts {
		for _, m := range sourceRecords(mount) {
			if sameReference(e.Auth.Credential, m.Credential, base) {
				return exportCredentialConflict()
			}
		}
	}
	return nil
}

type exportReceipt struct {
	Listen string `json:"listen"`
	Effect string `json:"effect"`
}

func addExport(ctx context.Context, saved configSnapshot, a arguments, in io.Reader, out io.Writer) (any, string, error) {
	var e export
	var err error
	if a.from == "" {
		if closer, ok := in.(io.Closer); ok {
			defer closeOnCancel(ctx, closer)()
		}
		e, err = promptExport(in, out)
	} else {
		data, readErr := readRecord(ctx, a.from, in)
		err = readErr
		if err == nil {
			err = decodeObject(data, &e)
		}
	}
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	if err != nil {
		return nil, "", err
	}
	receipt := exportReceipt{e.Listen, core.Unchanged.String()}
	saved.value.Exports = append(saved.value.Exports, e)
	if err := saved.value.validate(); err != nil {
		return receipt, "", err
	}
	if err := e.checkAvailability(); err != nil {
		return receipt, "", err
	}
	if err := e.checkCredential(saved); err != nil {
		return receipt, "", err
	}
	if err := checkStorageLocations(saved, false); err != nil {
		return receipt, "", err
	}
	applied, err := saved.save(ctx)
	if !applied {
		return receipt, "", err
	}
	receipt.Effect = core.Applied.String()
	return receipt, fmt.Sprintf("Saved %s export at %q.\n", e.Protocol, e.Listen), err
}

func promptExport(in io.Reader, out io.Writer) (e export, err error) {
	ask := promptInput(in, out)
	if e.Protocol, err = askChoice(ask, "Protocol ("+strings.Join(exportProtocols, ", ")+")", "webdav", exportProtocols...); err != nil {
		return e, err
	}
	rootLabel := "Mounted directory (for example /files)"
	if e.Protocol == "s3" {
		rootLabel = "Object mount (for example /objects)"
	}
	if e.Root, err = ask(rootLabel, ""); err != nil {
		return e, err
	}
	listen := "127.0.0.1:8787"
	switch e.Protocol {
	case "sftp":
		listen = "127.0.0.1:2222"
	case "ftp":
		listen = "127.0.0.1:2121"
	case "s3":
		listen = "127.0.0.1:9000"
	}
	if e.Listen, err = ask("Listen address", listen); err != nil {
		return e, err
	}
	if e.Access, err = askChoice(ask, "Access (read or read_write)", "read", "read", "read_write"); err != nil {
		return e, err
	}
	if e.Protocol == "sftp" {
		err = askFields(ask, setupField{"Username", &e.Auth.Username},
			setupField{"SSH host private key file", &e.HostKeyFile}, setupField{"Client public key file", &e.Auth.PublicKeyFile})
		if err != nil {
			return e, err
		}
		for _, path := range []*string{&e.HostKeyFile, &e.Auth.PublicKeyFile} {
			if *path != "" {
				*path, err = filepath.Abs(*path)
				if err != nil {
					return e, err
				}
			}
		}
	} else if err := e.promptCredential(ask); err != nil {
		return e, err
	}
	if e.Protocol == "ftp" {
		raw, err := ask("Passive ports (empty for automatic, or FIRST-LAST)", "")
		if err != nil {
			return e, err
		}
		if raw != "" {
			first, last, _ := strings.Cut(raw, "-")
			lo, loErr := strconv.ParseUint(first, 10, 16)
			hi, hiErr := strconv.ParseUint(last, 10, 16)
			if loErr != nil || hiErr != nil {
				return e, invalidConfig("Enter passive ports as FIRST-LAST.")
			}
			e.PassivePorts = &portRange{uint16(lo), uint16(hi)}
		}
	}
	if e.Protocol == "s3" && e.Access == "read_write" {
		state, err := ask("Multipart state directory (empty to disable)", "")
		if err != nil {
			return e, err
		}
		if state != "" {
			dir, err := filepath.Abs(state)
			if err != nil {
				return e, err
			}
			raw, err := ask("Multipart staging budget in bytes", "")
			if err != nil {
				return e, err
			}
			bytes, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return e, invalidConfig("Enter a positive multipart staging budget in bytes.")
			}
			e.S3.Multipart = &s3MultipartOptions{StateDirectory: dir, MaxStagingBytes: bytes}
		}
	}
	if err := e.validate(); err != nil {
		return e, err
	}
	if _, err := fmt.Fprintf(out, "Export %q using %s at %q with %s access.\n", e.Root, e.Protocol, e.Listen, e.Access); err != nil {
		return e, err
	}
	return e, confirm(ask, "Save export?")
}

func (e *export) promptCredential(ask askField) (err error) {
	label := "Bearer token reference (env:NAME or file:PATH)"
	switch e.Protocol {
	case "s3":
		e.S3 = &s3ExportOptions{Region: defaultS3Region}
		if err := askFields(ask, setupField{"Bucket", &e.S3.Bucket}, setupField{"Signing region", &e.S3.Region}, setupField{"Access key ID", &e.Auth.AccessKeyID}); err != nil {
			return err
		}
		label = "Secret access key reference (env:NAME or file:PATH)"
	case "webdav", "ftp":
		if e.Auth.Username, err = ask("Username", ""); err != nil {
			return err
		}
		label = "Password reference (env:NAME or file:PATH)"
	}
	raw, err := ask(label, "")
	if err != nil {
		return err
	}
	if e.Auth.Credential, err = promptReference(raw); err != nil {
		return err
	}
	cert, err := ask("TLS certificate file (empty for cleartext)", "")
	if err != nil || cert == "" {
		return err
	}
	key, err := ask("TLS key file", "")
	if err == nil {
		e.TLS = &exportTLS{cert, key}
	}
	return err
}

func (e export) scope() exportScope {
	return exportScope{root: e.Root, writable: e.Access == "read_write", keys: e.Protocol == "s3"}
}

type exportScope struct {
	root     string
	writable bool
	keys     bool
}

func (s exportScope) allows(action core.Action, addr core.Address) bool {
	if s.root == "" {
		return false
	}
	var entry bool
	if s.keys {
		if addr.Namespace() != core.KeyNamespace || addr.Space() != s.root {
			return false
		}
		entry = !addr.IsPrefix()
	} else {
		if addr.Namespace() != core.TreeNamespace || !core.Within(addr.Value(), s.root) {
			return false
		}
		entry = addr.Value() != s.root
	}
	switch action {
	case core.StatAction, core.ListAction:
		return true
	case core.ReadAction:
		return entry
	case core.CreateAction, core.WriteAction, core.RemoveAction, core.RemoveTreeAction:
		return s.writable && entry
	default:
		return false
	}
}

func exportHandler(c *core.Core, e export, principal core.Principal, secret string) (http.Handler, func(context.Context) error, error) {
	digest := sha256.Sum256([]byte(secret))
	const cursorTTL = time.Minute
	if e.Protocol == "http" {
		token := strings.TrimRight(secret, "=")
		if token == "" || strings.ContainsFunc(token, func(r rune) bool {
			return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune("-._~+/", r))
		}) {
			return nil, nil, invalidConfig("The JSON HTTP credential must be a bearer token.")
		}
		handler, err := httpapi.New(c, httpapi.Options{
			CursorTTL: cursorTTL,
			Authenticate: func(_ context.Context, value string) (core.Principal, error) {
				got := sha256.Sum256([]byte(value))
				if subtle.ConstantTimeCompare(digest[:], got[:]) == 1 {
					return principal, nil
				}
				return "", core.Fail(core.Unauthenticated, nil)
			},
		})
		if err != nil {
			return nil, nil, err
		}
		return handler, handler.Close, nil
	}

	if !utf8.ValidString(secret) || strings.ContainsFunc(secret, unicode.IsControl) {
		return nil, nil, invalidConfig("The WebDAV password must be UTF-8 without control characters.")
	}
	username := sha256.Sum256([]byte(e.Auth.Username))
	handler, err := openWebDAV(c, webdav.Options{
		Prefix: "/", Root: core.Path(e.Root), CursorTTL: cursorTTL, Tree: directoryPlan,
		Limits: webdav.Limits{XMLBytes: 64 << 10, ListEntries: directoryPlan.MaxEntries, ListNameBytes: directoryPlan.MaxPlanBytes},
		Authenticate: func(w http.ResponseWriter, r *http.Request) (core.Principal, bool) {
			user, password, ok := r.BasicAuth()
			if ok && len(r.Header.Values("Authorization")) == 1 {
				u, p := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(password))
				if subtle.ConstantTimeCompare(username[:], u[:])&subtle.ConstantTimeCompare(digest[:], p[:]) == 1 {
					return principal, true
				}
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="mweave", charset="UTF-8"`)
			http.Error(w, "Authentication required.", http.StatusUnauthorized)
			return "", false
		},
	}, e.CreateOnly)
	return handler, nil, err
}
