//go:build !windows && !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package cli

import (
	"errors"
	"os"
)

func openReplaceableFile(name string) (*os.File, error) { return os.Open(name) }

func tryConfigLock(*os.File) (bool, error) {
	return false, &problem{Code: "unsupported_operation", Message: "Atomic configuration edits are unavailable on this platform."}
}

func replaceConfig(string, string, bool) error { return errors.ErrUnsupported }
