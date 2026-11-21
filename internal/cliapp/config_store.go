package cliapp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/nostalume/mountweave-cli/internal/filelock"
)

type ConfigSnapshot struct {
	Path   string
	Value  Config
	Digest [32]byte
	exists bool
}

func LoadConfig(name string) (ConfigSnapshot, error) {
	name, err := resolveHostPath(name)
	if err != nil {
		return ConfigSnapshot{}, InvalidConfig("The configuration path could not be resolved.")
	}
	data, exists, err := readConfig(name)
	s := ConfigSnapshot{Path: name, exists: exists, Digest: sha256.Sum256(data)}
	if err != nil {
		return s, InvalidConfig("The configuration file could not be read.")
	}
	if !exists {
		s.Value = Config{Version: 1, Mounts: []MountRecord{}}
		return s, nil
	}
	if err := DecodeObject(data, &s.Value); err != nil {
		return s, err
	}
	return s, s.Value.validate()
}

func readConfig(name string) ([]byte, bool, error) {
	info, err := os.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("configuration must be a regular file")
	}
	f, err := openReplaceableFile(name)
	if err != nil {
		return nil, true, err
	}
	data, err := io.ReadAll(f)
	return data, true, errors.Join(err, f.Close())
}

func (s ConfigSnapshot) Save(ctx context.Context) (bool, error) {
	if err := s.Value.validate(); err != nil {
		return false, err
	}
	return s.saveValidated(ctx)
}

func (s ConfigSnapshot) saveValidated(ctx context.Context) (applied bool, err error) {
	data, err := json.MarshalIndent(s.Value, "", "  ")
	if err != nil {
		return false, err
	}
	lock, err := s.LockUnchanged(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	stage, err := stageFile(filepath.Dir(s.Path), ".mweave-config-*", append(data, '\n'))
	if err != nil {
		return false, err
	}
	defer func() {
		if !applied {
			err = errors.Join(err, os.Remove(stage))
		}
	}()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := replaceConfig(stage, s.Path, s.exists); err != nil {
		return false, err
	}
	return true, nil
}

func (s ConfigSnapshot) LockUnchanged(ctx context.Context) (_ *os.File, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// Keep the lock file's identity stable while other processes may wait on it.
	lock, err := os.OpenFile(filepath.Join(dir, "."+filepath.Base(s.Path)+".mweave.lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, lock.Close())
		}
	}()
	if _, err := filelock.Acquire(ctx, lock); err != nil {
		if err == errors.ErrUnsupported {
			return nil, &Problem{Code: "unsupported_operation", Message: "Atomic configuration edits are unavailable on this platform."}
		}
		return nil, err
	}
	current, exists, err := readConfig(s.Path)
	if err != nil {
		return nil, err
	}
	if exists != s.exists || sha256.Sum256(current) != s.Digest {
		return nil, ConfigConflict()
	}
	return lock, nil
}

// stageFile syncs a private file before a caller publishes it in the same directory.
func stageFile(dir, pattern string, data []byte) (string, error) {
	temp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	if _, err := temp.Write(data); err != nil {
		return "", errors.Join(err, temp.Close(), os.Remove(temp.Name()))
	}
	if err := errors.Join(temp.Sync(), temp.Close()); err != nil {
		return "", errors.Join(err, os.Remove(temp.Name()))
	}
	return temp.Name(), nil
}
