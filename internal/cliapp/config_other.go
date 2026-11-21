//go:build !windows && !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package cliapp

import (
	"errors"
	"os"
)

func openReplaceableFile(name string) (*os.File, error) { return os.Open(name) }

func replaceConfig(string, string, bool) error { return errors.ErrUnsupported }
