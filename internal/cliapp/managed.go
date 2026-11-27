package cliapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nostalume/mountweave-cli/internal/filelock"
)

const ManagedCredential = "managed"
const maxGrantBytes = 64 << 10

func NewGrantID() string {
	var id [16]byte
	rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

func ValidGrantID(id string) bool {
	return len(id) == 32 && !strings.ContainsFunc(id, func(c rune) bool {
		return !('0' <= c && c <= '9' || 'a' <= c && c <= 'f')
	})
}

type GrantLease struct {
	Lock *os.File
	Path string
}

func GrantDirectoryPath() (string, error) {
	root, err := userSettingsDir()
	if err != nil {
		return "", GrantUnavailable()
	}
	return filepath.Join(root, "mweave", "credentials"), nil
}

func GrantDirectory(create bool) (string, error) {
	if !ManagedStoreSupported {
		return "", &Problem{Code: "unsupported_operation", Message: "Managed credential storage is unavailable on this platform."}
	}
	dir, err := GrantDirectoryPath()
	if err != nil {
		return "", err
	}
	if create {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", GrantUnavailable()
		}
	}
	if err := checkGrantDirectory(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func checkGrantDirectory(dir string) error {
	app := filepath.Dir(dir)
	// The application parent may already hold ordinary configuration.
	for _, path := range []string{filepath.Dir(app), app, dir} {
		mode := fs.FileMode(0755)
		if path == dir {
			mode = 0700
		}
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return missingManagedGrant
		}
		if err != nil || !info.IsDir() || !privateGrantFile(info, mode) {
			return GrantUnavailable()
		}
	}
	return nil
}

func grantPath(id string) (string, error) {
	if !ManagedStoreSupported || !ValidGrantID(id) {
		return "", GrantUnavailable()
	}
	dir, err := GrantDirectory(false)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".grant"), nil
}

func MissingGrant(id string) (bool, error) {
	path, err := grantPath(id)
	if err == nil {
		err = checkGrantFile(path)
	}
	if err == missingManagedGrant {
		return true, nil
	}
	return false, err
}

// InspectGrant reads the current record without acquiring a lease.
// The caller owns the sensitive result. Use a lease for credential updates.
func InspectGrant(id string) ([]byte, error) {
	path, err := grantPath(id)
	if err != nil {
		return nil, err
	}
	return ReadGrant(path)
}

func AcquireGrant(id string, create bool) (*GrantLease, error) {
	return LockGrant(nil, id, create)
}

// Reusable grants wait only for an in-flight credential update. Rotating grants
// pass a nil context and preserve the existing exclusive-owner error.
func LockGrant(ctx context.Context, id string, create bool) (*GrantLease, error) {
	if !ValidGrantID(id) {
		return nil, InvalidConfig("A managed credential needs a valid authorization record ID.")
	}
	dir, err := GrantDirectory(create)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, id+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, GrantUnavailable()
	}
	ok, err := filelock.Acquire(ctx, lock)
	switch {
	case ok:
		return &GrantLease{Lock: lock, Path: filepath.Join(dir, id+".grant")}, nil
	case ctx != nil && ctx.Err() != nil:
		err = ctx.Err()
	case err != nil:
		err = GrantUnavailable()
	default:
		err = &Problem{Code: "credential_in_use", Message: "Another active mount is using this authorization.",
			NextAction: "Stop the process using this authorization, or access its existing export."}
	}
	return nil, errors.Join(err, lock.Close())
}

func GrantUnavailable() error {
	return &Problem{Code: "credential_unavailable", Message: "The managed authorization record is unavailable.", NextAction: "Check the protected credential store for this user."}
}

var (
	missingManagedGrant   = &Problem{Code: "credential_unavailable", Message: "The saved authorization is missing on this machine.", Reconnect: true}
	grantDecryptionFailed = &Problem{Code: "credential_unavailable", Message: "The saved authorization could not be decrypted.", Reconnect: true}
)

// MayReconnectGrant reports that a new authorization can replace the old reference.
func MayReconnectGrant(err error) bool {
	return errors.Is(err, missingManagedGrant) || errors.Is(err, grantDecryptionFailed)
}

func (g *GrantLease) Read() ([]byte, error) { return ReadGrant(g.Path) }

func checkGrantFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return missingManagedGrant
	}
	if err != nil || !info.Mode().IsRegular() || !privateGrantFile(info, 0600) || info.Size() == 0 || info.Size() > maxGrantBytes {
		return GrantUnavailable()
	}
	return nil
}

func ReadGrant(path string) ([]byte, error) {
	if err := checkGrantFile(path); err != nil {
		return nil, err
	}
	file, err := openReplaceableFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, missingManagedGrant
	}
	if err != nil {
		return nil, GrantUnavailable()
	}
	data, err := io.ReadAll(io.LimitReader(file, maxGrantBytes+1))
	if err := errors.Join(err, file.Close()); err != nil || len(data) == 0 || len(data) > maxGrantBytes {
		return nil, GrantUnavailable()
	}
	plain, err := unprotectGrant(data)
	if err != nil {
		return nil, grantDecryptionFailed
	}
	return plain, nil
}

// Save reports whether the new grant became visible. The error also reports
// failed durability checks after publication.
func (g *GrantLease) Save(ctx context.Context, data []byte, replace bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	info, err := os.Lstat(g.Path)
	switch {
	case len(data) > maxGrantBytes,
		err == nil && (!replace || !info.Mode().IsRegular() || !privateGrantFile(info, 0600)),
		err != nil && (replace || !errors.Is(err, fs.ErrNotExist)):
		return false, GrantUnavailable()
	}
	protected, err := protectGrant(data)
	if err != nil {
		return false, err
	}
	if len(protected) > maxGrantBytes {
		return false, GrantUnavailable()
	}
	return publishStagedFile(ctx, g.Path, ".grant-*", protected, func(stage string) (bool, error) {
		return publishGrant(stage, g.Path, replace)
	})
}

func ReadCredentialFile(name string) ([]byte, error) {
	info, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fs.ErrInvalid
	}
	return os.ReadFile(name)
}

func LoadCredential(ref *CredentialRef, base string) (string, error) {
	name := ref.Ref
	var secret string
	if ref.Kind == "env" {
		secret = os.Getenv(name)
	} else {
		name = HostPath(base, name)
		if body, err := ReadCredentialFile(name); err == nil {
			secret = strings.TrimRight(string(body), "\r\n")
		}
	}
	if secret != "" && !strings.ContainsAny(secret, "\x00\r\n") {
		return secret, nil
	}
	var action string
	if ref.Kind == "env" {
		action = fmt.Sprintf("Set environment variable %q to a non-empty single-line credential.", name)
	} else {
		action = fmt.Sprintf("Check that credential file %q is readable and contains one non-empty line.", name)
	}
	return "", &Problem{Code: "credential_unavailable", Message: "The credential reference could not be read.", NextAction: action}
}

func sameReference(a, b *CredentialRef, base string) bool {
	if a == nil || b == nil || a.Kind != b.Kind {
		return false
	}
	left, right := a.Ref, b.Ref
	if a.Kind == "file" {
		left, right = HostPath(base, left), HostPath(base, right)
	}
	if left == right || (a.Kind != "profile" && runtime.GOOS == "windows" && strings.EqualFold(left, right)) {
		return true
	}
	if a.Kind != "file" {
		return false
	}
	// Compare aliases without reading either file.
	first, err := os.Stat(left)
	if err != nil {
		return false
	}
	second, err := os.Stat(right)
	return err == nil && os.SameFile(first, second)
}

// Check shared references before reading a secret or sending a probe. Edits
// include the old record so a destination change cannot reuse its reference.
func admitMount(app *App, m MountRecord, src MountSource, saved ConfigSnapshot, sources iter.Seq[MountRecord]) (sourceOpener, error) {
	open, err := src.references().opener(m.Credential)
	if err != nil {
		return nil, err
	}
	if open == nil && (src.pool == nil || src.Route == nil || src.Route.pool == nil) {
		return nil, unavailableSource()
	}
	if m.Credential == nil {
		return open, nil
	}
	base := filepath.Dir(saved.Path)
	for _, e := range saved.Value.Exports {
		if sameReference(m.Credential, e.Auth.Credential, base) {
			return nil, exportCredentialConflict()
		}
	}
	fromRecord := src.preparedOptions != nil && bytes.Equal(src.preparedOptions, m.Options)
	for prior := range sources {
		if fromRecord && prior.Driver == m.Driver && prior.Connection == m.Connection && bytes.Equal(prior.Options, m.Options) {
			continue
		}
		if !sameReference(m.Credential, prior.Credential, base) {
			continue
		}
		old, err := PrepareMountSource(app, prior, base)
		if err != nil || old.Audience != src.Audience {
			return nil, &Problem{Code: "credential_scope_conflict", Message: "This credential reference is already associated with a different or unknown destination.", NextAction: "Provide a separate credential reference for the new destination."}
		}
	}
	return open, nil
}
