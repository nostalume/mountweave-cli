//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package filelock

import (
	"errors"
	"os"
	"syscall"
)

// Try takes an exclusive lock without waiting. Closing file releases the lock.
func Try(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
		return false, nil
	}
	return err == nil, err
}
