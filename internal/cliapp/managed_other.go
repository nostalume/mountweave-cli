//go:build !windows && !linux

package cliapp

import (
	"errors"
	"io/fs"
)

const ManagedStoreSupported = false

func privateGrantFile(fs.FileInfo, fs.FileMode) bool  { return false }
func protectGrant([]byte) ([]byte, error)             { return nil, errors.ErrUnsupported }
func unprotectGrant([]byte) ([]byte, error)           { return nil, errors.ErrUnsupported }
func publishGrant(string, string, bool) (bool, error) { return false, errors.ErrUnsupported }
