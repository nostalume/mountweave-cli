//go:build !windows && !linux && !darwin

package website

import "github.com/chromedp/chromedp"

const BrowserUnavailable = "Isolated website sessions are unavailable on this platform."

func Browser() (string, []chromedp.ExecAllocatorOption) { return "", nil }
func PrivateDirectory(string, bool) error               { return ErrUnsafeDirectory }
