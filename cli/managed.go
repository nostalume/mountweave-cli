package cli

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const managedCredential = "managed"
const maxGrantBytes = 64 << 10

func validGrantID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

type grantLease struct {
	lock *os.File
	path string
}

func grantDirectory(create bool) (string, error) {
	if !managedStoreSupported {
		return "", &problem{Code: "unsupported_operation", Message: "Managed credential storage is unavailable on this platform."}
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return "", grantUnavailable()
	}
	app := filepath.Join(root, "mweave")
	dir := filepath.Join(app, "credentials")
	if create {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", grantUnavailable()
		}
	}
	// The application parent may already hold ordinary configuration.
	for _, path := range []string{root, app, dir} {
		mode := fs.FileMode(0755)
		if path == dir {
			mode = 0700
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || !privateGrantFile(info, mode) {
			return "", grantUnavailable()
		}
	}
	return dir, nil
}

func acquireGrant(id string, create bool) (*grantLease, error) {
	return lockGrant(nil, id, create)
}

// Reusable grants wait only for an in-flight credential update. Rotating grants
// pass a nil context and preserve the existing exclusive-owner error.
func lockGrant(ctx context.Context, id string, create bool) (*grantLease, error) {
	if !validGrantID(id) {
		return nil, invalidConfig("A managed credential needs a valid authorization record ID.")
	}
	dir, err := grantDirectory(create)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, id+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, grantUnavailable()
	}
	ok, err := waitFileLock(ctx, lock)
	if ok {
		return &grantLease{lock: lock, path: filepath.Join(dir, id+".grant")}, nil
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, errors.Join(ctx.Err(), lock.Close())
	}
	if err != nil {
		return nil, errors.Join(grantUnavailable(), lock.Close())
	}
	return nil, errors.Join(&problem{
		Code: "credential_in_use", Message: "Another active mount is using this authorization.",
		NextAction: "Stop the process using this authorization, or access its existing export.",
	}, lock.Close())
}

func grantUnavailable() error {
	return &problem{Code: "credential_unavailable", Message: "The managed authorization record is unavailable.", NextAction: "Check the protected credential store for this user."}
}

func (g *grantLease) read() ([]byte, error) { return readGrant(g.path) }

func readGrant(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !privateGrantFile(info, 0600) {
		return nil, grantUnavailable()
	}
	file, err := openReplaceableFile(path)
	if err != nil {
		return nil, grantUnavailable()
	}
	data, err := io.ReadAll(io.LimitReader(file, maxGrantBytes+1))
	if err := errors.Join(err, file.Close()); err != nil || len(data) == 0 || len(data) > maxGrantBytes {
		return nil, grantUnavailable()
	}
	plain, err := unprotectGrant(data)
	if err != nil {
		return nil, grantUnavailable()
	}
	return plain, nil
}

func (g *grantLease) save(ctx context.Context, data []byte, replace bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(g.path)
	switch {
	case err == nil && (!replace || !info.Mode().IsRegular() || !privateGrantFile(info, 0600)):
		return grantUnavailable()
	case err != nil && (replace || !errors.Is(err, fs.ErrNotExist)):
		return grantUnavailable()
	}
	if len(data) > maxGrantBytes {
		return grantUnavailable()
	}
	protected, err := protectGrant(data)
	if err != nil {
		return err
	}
	if len(protected) > maxGrantBytes {
		return grantUnavailable()
	}
	stage, err := stageFile(filepath.Dir(g.path), ".grant-*", protected)
	if err != nil {
		return err
	}
	defer os.Remove(stage)
	if err := ctx.Err(); err != nil {
		return err
	}
	return publishGrant(stage, g.path, replace)
}
