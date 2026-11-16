package website

import (
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/chromedp/chromedp"
	"golang.org/x/sys/windows"
)

const BrowserUnavailable = "The Windows default HTTPS browser could not be opened as an isolated website session."

func Browser() (string, []chromedp.ExecAllocatorOption) {
	// Ask the Shell for the effective HTTPS handler's executable. The open
	// command's arguments are intended for the user's normal browsing profile.
	query := windows.NewLazySystemDLL("shlwapi.dll").NewProc("AssocQueryStringW")
	if err := query.Find(); err != nil {
		return "", nil
	}
	scheme := [...]uint16{'h', 't', 't', 'p', 's', 0}
	verb := [...]uint16{'o', 'p', 'e', 'n', 0}
	executable := make([]uint16, 32768)
	size := uint32(len(executable))
	const (
		assocIsProtocol = 0x1000 // ASSOCF_IS_PROTOCOL
		assocExecutable = 2      // ASSOCSTR_EXECUTABLE
	)
	result, _, _ := query.Call(assocIsProtocol, assocExecutable,
		uintptr(unsafe.Pointer(&scheme[0])), uintptr(unsafe.Pointer(&verb[0])),
		uintptr(unsafe.Pointer(&executable[0])), uintptr(unsafe.Pointer(&size)))
	if result != 0 {
		return "", nil
	}
	path := windows.UTF16ToString(executable)
	if !filepath.IsAbs(path) {
		return "", nil
	}
	switch strings.ToLower(filepath.Base(path)) {
	case "msedge.exe":
		return path, []chromedp.ExecAllocatorOption{chromedp.Flag("edge-skip-compat-layer-relaunch", true)}
	case "vivaldi.exe":
		// Vivaldi's extension-owned window does not expose the initial page
		// over the inherited pipe. The isolated process can use Chromium's UI.
		return path, []chromedp.ExecAllocatorOption{chromedp.Flag("disable-vivaldi", true)}
	case "chrome.exe", "chromium.exe", "brave.exe", "opera.exe":
		return path, nil
	default:
		return "", nil
	}
}

func PrivateDirectory(path string, create bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sddl := "D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)"
	expected, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	if create {
		u, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		attr := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: expected}
		err = windows.CreateDirectory(u, &attr)
		if err != nil && err != windows.ERROR_ALREADY_EXISTS {
			return err
		}
	}
	u, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attr, err := windows.GetFileAttributes(u)
	if err != nil {
		return err
	}
	if attr&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || attr&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrUnsafeDirectory
	}
	actual, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := actual.Owner()
	_, dacl, present := strings.Cut(actual.String(), "D:")
	_, want, _ := strings.Cut(expected.String(), "D:")
	if err != nil || !owner.Equals(user.User.Sid) || !present || dacl != want {
		return ErrUnsafeDirectory
	}
	return nil
}
