//go:build linux || darwin

package website

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/chromedp/chromedp"
)

const BrowserUnavailable = "No compatible browser is available for an isolated website session."

func Browser() (string, []chromedp.ExecAllocatorOption) {
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "microsoft-edge", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", nil
}

func PrivateDirectory(path string, create bool) error {
	if create {
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	native, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || native.Uid != uint32(os.Geteuid()) {
		return ErrUnsafeDirectory
	}
	return nil
}
