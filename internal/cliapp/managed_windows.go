package cliapp

import (
	"io/fs"
	"slices"
	"unsafe"

	"golang.org/x/sys/windows"
)

const ManagedStoreSupported = true

// The current user's DPAPI key protects the record independently of file mode.
func privateGrantFile(fs.FileInfo, fs.FileMode) bool { return true }

func protectGrant(data []byte) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(data)), Data: unsafe.SliceData(data)}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(out.Data))))
	return slices.Clone(unsafe.Slice(out.Data, out.Size)), nil
}

func unprotectGrant(data []byte) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(data)), Data: unsafe.SliceData(data)}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	plain := unsafe.Slice(out.Data, out.Size)
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(out.Data))))
	defer clear(plain)
	return slices.Clone(plain), nil
}

func publishGrant(stage, path string, replace bool) (bool, error) {
	if replace {
		err := replaceConfig(stage, path, true)
		return err == nil, err
	}
	from, err := windows.UTF16PtrFromString(stage)
	if err != nil {
		return false, err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	err = windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
	return err == nil, err
}
