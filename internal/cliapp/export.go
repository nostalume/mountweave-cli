package cliapp

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	core "github.com/nostalume/mountweave"
)

var (
	exportProtocols     = []string{"webdav", "http", "sftp", "ftp", "s3"}
	exportPortOverrides = map[string]string{"sftp": "2222", "ftp": "2121", "s3": "9000"}
)

type Export struct {
	Protocol     string           `json:"protocol"`
	Listen       string           `json:"listen"`
	Root         string           `json:"root"`
	Access       string           `json:"access,omitempty"`
	Auth         ExportAuth       `json:"auth"`
	TLS          *exportTLS       `json:"tls,omitempty"`
	HostKeyFile  string           `json:"host_key_file,omitempty"`
	PassivePorts *portRange       `json:"passive_ports,omitempty"`
	S3           *S3ExportOptions `json:"s3,omitempty"`
	CreateOnly   bool             `json:"create_only,omitempty"`
}

type ExportAuth struct {
	Username      string         `json:"username,omitempty"`
	Credential    *CredentialRef `json:"credential,omitempty"`
	PublicKeyFile string         `json:"public_key_file,omitempty"`
	AccessKeyID   string         `json:"access_key_id,omitempty"`
}

type exportTLS struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

func (e exportTLS) paths(base string) (cert, key string) {
	return HostPath(base, e.CertFile), HostPath(base, e.KeyFile)
}

func (e Export) sshKeyPaths(base string) (host, client string) {
	return OptionalHostPath(base, e.HostKeyFile), OptionalHostPath(base, e.Auth.PublicKeyFile)
}

func (e Export) multipartDirectory(base string) string {
	if e.S3 == nil || e.S3.Multipart == nil {
		return ""
	}
	return HostPath(base, e.S3.Multipart.StateDirectory)
}

func (e Export) validate() error {
	host, port, err := net.SplitHostPort(e.Listen)
	number, portErr := strconv.ParseUint(port, 10, 16)
	user, ref := e.Auth.Username, e.Auth.Credential
	if err != nil || portErr != nil || number == 0 ||
		!slices.Contains(exportProtocols, e.Protocol) ||
		!slices.Contains([]string{"", "read", "read_write"}, e.Access) {
		return InvalidConfig("An export needs a supported protocol, listener and access policy.")
	}
	if e.CreateOnly && (e.Protocol != "webdav" || e.Access != "read_write") {
		return InvalidConfig("Create-only publication requires a read-write WebDAV export.")
	}
	if e.PassivePorts != nil && (e.Protocol != "ftp" || e.PassivePorts.First == 0 || e.PassivePorts.Last < e.PassivePorts.First) {
		return InvalidConfig("FTP passive ports need an ordered range of ports from 1 to 65535.")
	}
	if e.Protocol == "s3" {
		if e.S3 == nil || !bucketName.MatchString(e.S3.Bucket) || e.S3.MaxPutBytes < 0 ||
			e.S3.Region != "" && !sigV4Name(e.S3.Region) || !sigV4Name(e.Auth.AccessKeyID) {
			return InvalidConfig("S3 needs a bucket, a valid signing region and access key ID, and a nonnegative PUT limit.")
		}
		if m := e.S3.Multipart; m != nil && (e.Access != "read_write" || strings.TrimSpace(m.StateDirectory) == "" ||
			m.MaxStagingBytes < 4096 || m.MaxObjectBytes < 0 || m.MaxObjectBytes > m.MaxStagingBytes ||
			m.RetentionSeconds < 0 || m.RetentionSeconds > int64((1<<63-1)/time.Second)) {
			return InvalidConfig("S3 multipart requires read_write access, a private state directory, a staging budget of at least 4096 bytes and valid optional limits.")
		}
	} else if e.S3 != nil || e.Auth.AccessKeyID != "" {
		return InvalidConfig("Bucket and access key ID settings are only used by S3 exports.")
	}
	invalidUsername := !utf8.ValidString(user) || strings.ContainsFunc(user, unicode.IsControl)
	if e.Protocol == "sftp" {
		if e.TLS != nil || ref != nil || strings.TrimSpace(e.HostKeyFile) == "" || strings.TrimSpace(e.Auth.PublicKeyFile) == "" ||
			strings.TrimSpace(user) == "" || invalidUsername {
			return InvalidConfig("SFTP needs an SSH host key file, username and client public key file.")
		}
		return nil
	}
	if e.HostKeyFile != "" || e.Auth.PublicKeyFile != "" {
		return InvalidConfig("SSH key files are only used by SFTP exports.")
	}
	if !ref.Valid() || (ref.Kind != "env" && ref.Kind != "file") {
		return InvalidConfig("An export needs an env or file credential reference.")
	}
	if e.usesPassword() != (user != "") || invalidUsername ||
		e.Protocol == "webdav" && strings.ContainsRune(user, ':') ||
		e.Protocol == "ftp" && strings.TrimSpace(user) == "" {
		return InvalidConfig("WebDAV and FTP need a valid username. Use the protocol's authentication fields.")
	}
	if e.TLS != nil {
		if e.TLS.CertFile == "" || e.TLS.KeyFile == "" {
			return InvalidConfig("An export TLS configuration needs certificate and key files.")
		}
	} else if ip := net.ParseIP(host); host == "" || (ip != nil && !ip.IsLoopback()) {
		return InvalidConfig("A cleartext export must use a loopback listener.")
	}
	return nil
}

func (e Export) usesPassword() bool { return e.Protocol == "webdav" || e.Protocol == "ftp" }

func exportCredentialConflict() error {
	return &Problem{Code: "credential_scope_conflict", Message: "An export credential is also referenced by a storage mount.", NextAction: "Use separate credentials for clients and storage."}
}

func (e Export) containsMount(at string) bool {
	if e.Protocol == "s3" {
		return e.Root == at
	}
	return core.Within(e.Root, at)
}

func (e Export) checkCredential(saved ConfigSnapshot) error {
	if e.Auth.Credential == nil {
		return nil
	}
	base := filepath.Dir(saved.Path)
	for m := range SourceRecords(saved.Value.Mounts...) {
		if sameReference(e.Auth.Credential, m.Credential, base) {
			return exportCredentialConflict()
		}
	}
	return nil
}

func (e Export) scope() exportScope {
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

const DefaultS3Region = "us-east-1"

type S3ExportOptions struct {
	Bucket      string              `json:"bucket"`
	Region      string              `json:"region,omitempty"`
	MaxPutBytes int64               `json:"max_put_bytes,omitempty"`
	Multipart   *s3MultipartOptions `json:"multipart,omitempty"`
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func sigV4Name(s string) bool {
	return s != "" && !strings.ContainsAny(s, "/,") && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

type s3MultipartOptions struct {
	StateDirectory   string `json:"state_directory"`
	MaxStagingBytes  int64  `json:"max_staging_bytes"`
	MaxObjectBytes   int64  `json:"max_object_bytes,omitempty"`
	RetentionSeconds int64  `json:"retention_seconds,omitempty"`
}

type portRange struct {
	First uint16 `json:"first"`
	Last  uint16 `json:"last"`
}

type exportReceipt struct {
	Listen string `json:"listen"`
	Effect string `json:"effect"`
}

func addExport(ctx context.Context, saved ConfigSnapshot, a arguments, in io.Reader, out io.Writer) (any, string, error) {
	var e Export
	var err error
	var ask Prompt
	if a.from == "" {
		if closer, ok := in.(io.Closer); ok {
			defer closeOnCancel(ctx, closer)()
		}
		ask = promptInput(in, out)
		e, err = promptExport(ask)
	} else {
		var data []byte
		if data, err = readRecord(ctx, a.from, in); err == nil {
			err = DecodeObject(data, &e)
		}
	}
	if cancelErr := ctx.Err(); cancelErr != nil {
		return nil, "", cancelErr
	}
	if err != nil {
		return nil, "", err
	}
	receipt := exportReceipt{e.Listen, core.Unchanged.String()}
	saved.Value.Exports = append(saved.Value.Exports, e)
	if err := saved.Value.validate(); err != nil {
		return receipt, "", err
	}
	if ask != nil {
		if _, err = fmt.Fprintf(out, "Export %q using %s at %q with %s access.\n", e.Root, e.Protocol, e.Listen, e.Access); err == nil {
			err = Confirm(ask, "Save export?")
		}
		if cancelErr := ctx.Err(); cancelErr != nil {
			return nil, "", cancelErr
		}
		if err != nil {
			return nil, "", err
		}
	}
	if err := e.checkCredential(saved); err != nil {
		return receipt, "", err
	}
	if err := CheckStorageLocations(saved, false); err != nil {
		return receipt, "", err
	}
	applied, err := saved.saveValidated(ctx)
	if !applied {
		return receipt, "", err
	}
	receipt.Effect = core.Applied.String()
	return receipt, fmt.Sprintf("Saved %s export at %q.\n", e.Protocol, e.Listen), err
}

func promptExport(ask Prompt) (e Export, err error) {
	if e.Protocol, err = AskChoice(ask, "Protocol ("+strings.Join(exportProtocols, ", ")+")", "webdav", exportProtocols...); err != nil {
		return e, err
	}
	rootLabel := "Mounted directory (for example /files)"
	if e.Protocol == "s3" {
		rootLabel = "Object mount (for example /objects)"
	}
	if e.Root, err = ask(rootLabel, ""); err != nil {
		return e, err
	}
	port := cmp.Or(exportPortOverrides[e.Protocol], "8787")
	if e.Listen, err = ask("Listen address", "127.0.0.1:"+port); err != nil {
		return e, err
	}
	if e.Access, err = AskChoice(ask, "Access (read or read_write)", "read", "read", "read_write"); err != nil {
		return e, err
	}
	if err := e.promptCredential(ask); err != nil {
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
				return e, InvalidConfig("Enter passive ports as FIRST-LAST.")
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
				return e, InvalidConfig("Enter a positive multipart staging budget in bytes.")
			}
			e.S3.Multipart = &s3MultipartOptions{StateDirectory: dir, MaxStagingBytes: bytes}
		}
	}
	return e, nil
}

func (e *Export) promptCredential(ask Prompt) (err error) {
	label := "Bearer token"
	switch {
	case e.Protocol == "sftp":
		if err := AskFields(ask, Field("Username", &e.Auth.Username),
			Field("SSH host private key file", &e.HostKeyFile), Field("Client public key file", &e.Auth.PublicKeyFile)); err != nil {
			return err
		}
		for _, path := range []*string{&e.HostKeyFile, &e.Auth.PublicKeyFile} {
			if *path != "" {
				if *path, err = filepath.Abs(*path); err != nil {
					return err
				}
			}
		}
		return nil
	case e.Protocol == "s3":
		e.S3 = &S3ExportOptions{Region: DefaultS3Region}
		if err := AskFields(ask, Field("Bucket", &e.S3.Bucket), Field("Signing region", &e.S3.Region), Field("Access key ID", &e.Auth.AccessKeyID)); err != nil {
			return err
		}
		label = "Secret access key"
	case e.usesPassword():
		if e.Auth.Username, err = ask("Username", ""); err != nil {
			return err
		}
		label = "Password"
	}
	raw, err := ask(label+" reference (env:NAME or file:PATH)", "")
	if err != nil {
		return err
	}
	if e.Auth.Credential, err = PromptReference(raw); err != nil {
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
