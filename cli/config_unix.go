//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package cli

import (
	"errors"
	"os"
	"syscall"
)

func openReplaceableFile(name string) (*os.File, error) { return os.Open(name) }

func tryConfigLock(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
		return false, nil
	}
	return err == nil, err
}

func replaceConfig(source, target string, _ bool) error { return os.Rename(source, target) }
