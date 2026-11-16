//go:build !windows && !linux

package cli

import (
	"errors"
	"io/fs"
)

const managedStoreSupported = false

func privateGrantFile(fs.FileInfo, fs.FileMode) bool { return false }
func protectGrant([]byte) ([]byte, error)            { return nil, errors.ErrUnsupported }
func unprotectGrant([]byte) ([]byte, error)          { return nil, errors.ErrUnsupported }
func publishGrant(string, string, bool) error        { return errors.ErrUnsupported }
