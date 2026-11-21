//go:build !windows && !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package filelock

import (
	"errors"
	"os"
)

func Try(*os.File) (bool, error) { return false, errors.ErrUnsupported }
