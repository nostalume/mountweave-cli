package cli

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const shareFile = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

func openReplaceableFile(name string) (*os.File, error) {
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(path, windows.GENERIC_READ, shareFile, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), name), nil
}

func tryConfigLock(file *os.File) (bool, error) {
	var offset windows.Overlapped
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &offset)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func replaceConfig(source, target string, exists bool) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	name, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(from, windows.DELETE, shareFile, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)

	// FILE_RENAME_INFO carries an inline UTF-16 target.
	type renameInfo struct {
		Flags      uint32
		Root       windows.Handle
		NameLength uint32
		Name       [1]uint16
	}
	buffer := make([]byte, int(unsafe.Sizeof(renameInfo{}))+len(name)*2)
	request := (*renameInfo)(unsafe.Pointer(&buffer[0]))
	if exists {
		const replaceExisting, posixSemantics = 1, 2
		request.Flags = replaceExisting | posixSemantics
	}
	request.NameLength = uint32((len(name) - 1) * 2)
	copy(unsafe.Slice(&request.Name[0], len(name)), name)
	const fileRenameInfoEx = 22
	return windows.SetFileInformationByHandle(h, fileRenameInfoEx, &buffer[0], uint32(len(buffer)))
}
