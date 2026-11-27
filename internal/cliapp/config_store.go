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
	return publishStagedFile(ctx, s.Path, ".mweave-config-*", append(data, '\n'), func(stage string) (bool, error) {
		err := replaceConfig(stage, s.Path, s.exists)
		return err == nil, err
	})
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

// publishStagedFile owns a private file until the native commit consumes it.
func publishStagedFile(ctx context.Context, target, pattern string, data []byte, commit func(string) (bool, error)) (published bool, err error) {
	file, err := os.CreateTemp(filepath.Dir(target), pattern)
	if err != nil {
		return false, err
	}
	stage := file.Name()
	defer func() {
		if !published {
			err = errors.Join(err, os.Remove(stage))
		}
	}()
	if _, err := file.Write(data); err != nil {
		return false, errors.Join(err, file.Close())
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return commit(stage)
}
