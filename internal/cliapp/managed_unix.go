//go:build linux

package cliapp

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const ManagedStoreSupported = true

func privateGrantFile(info fs.FileInfo, mode fs.FileMode) bool {
	return info.Mode().Perm()&^mode == 0 && info.Sys().(*syscall.Stat_t).Uid == uint32(os.Geteuid())
}

// The store is readable only by its owner. The staged file already has mode 0600.
func protectGrant(data []byte) ([]byte, error)   { return data, nil }
func unprotectGrant(data []byte) ([]byte, error) { return data, nil }

func publishGrant(stage, path string, replace bool) (bool, error) {
	dir := filepath.Dir(path)
	if replace {
		if err := replaceConfig(stage, path, true); err != nil {
			return false, err
		}
		return true, syncGrantDirectory(dir)
	}
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, path, unix.RENAME_NOREPLACE); err != nil {
		return false, err
	}
	// A first save can also have created the credential and app directories.
	app := filepath.Dir(dir)
	root := filepath.Dir(app)
	return true, errors.Join(syncGrantDirectory(dir), syncGrantDirectory(app), syncGrantDirectory(root))
}

func syncGrantDirectory(name string) error {
	dir, err := os.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
