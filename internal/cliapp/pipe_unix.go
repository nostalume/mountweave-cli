//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package cliapp

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// An inherited pipe is usually blocking. Wrap a nonblocking duplicate so a
// concurrent Close wakes Go's poller. Restore the shared descriptor's flags.
func pollablePipe(file *os.File) (*os.File, func() error, error) {
	info, err := file.Stat()
	if err != nil || info.Mode()&(os.ModeNamedPipe|os.ModeSocket) == 0 {
		return file, nil, err
	}
	raw, err := file.SyscallConn()
	if err != nil {
		return nil, nil, err
	}
	var duplicate, flags int
	var setup error
	err = raw.Control(func(fd uintptr) {
		flags, setup = unix.FcntlInt(fd, unix.F_GETFL, 0)
		if setup != nil {
			return
		}
		duplicate, setup = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0)
		if setup == nil {
			setup = unix.SetNonblock(duplicate, true)
			if setup != nil {
				_ = unix.Close(duplicate)
			}
		}
	})
	if err := errors.Join(err, setup); err != nil {
		return nil, nil, err
	}
	pipe := os.NewFile(uintptr(duplicate), file.Name())
	return pipe, func() error {
		closeErr := pipe.Close()
		if errors.Is(closeErr, os.ErrClosed) {
			closeErr = nil
		}
		if flags&unix.O_NONBLOCK != 0 {
			return closeErr
		}
		var restore error
		err := raw.Control(func(fd uintptr) { restore = unix.SetNonblock(int(fd), false) })
		return errors.Join(closeErr, err, restore)
	}, nil
}
