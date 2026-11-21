//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package cliapp

import (
	"os"
)

func openReplaceableFile(name string) (*os.File, error) { return os.Open(name) }

func replaceConfig(source, target string, _ bool) error { return os.Rename(source, target) }
