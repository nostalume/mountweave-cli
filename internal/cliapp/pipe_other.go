//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package cliapp

import "os"

func pollablePipe(file *os.File) (*os.File, func() error, error) {
	return file, nil, nil
}
