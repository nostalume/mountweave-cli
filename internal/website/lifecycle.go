// Package website manages isolated browser processes and private profile directories.
package website

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

var (
	ErrBrowserMissing  = errors.New(BrowserUnavailable)
	ErrSignInStart     = errors.New("The browser could not open a separate sign-in window.")
	ErrSignInExit      = errors.New("The sign-in browser closed unexpectedly.")
	ErrLaunch          = errors.New("The browser could not open an isolated website session.")
	ErrClose           = errors.New("website browser did not close cleanly")
	ErrUnsafeDirectory = errors.New("website profile directory is unavailable or unsafe")
)

// SignIn opens a visible browser in the provided private profile. The caller
// supplies confirmation that the user has signed in and closed the window.
func SignIn(ctx context.Context, binary, profile, page string, confirm func() error, out io.Writer) error {
	if binary == "" {
		return ErrBrowserMissing
	}
	if _, err := fmt.Fprintf(out, "Opening %s for sign-in.\n", filepath.Base(binary)); err != nil {
		return err
	}
	login, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(login, binary, "--user-data-dir="+profile, "--no-first-run",
		"--no-default-browser-check", "--disable-background-mode", page)
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrSignInStart
	}
	done := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = cmd.Wait()
		close(done)
	}()
	defer func() {
		stop()
		<-done
	}()
	for {
		if err := confirm(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if exitErr != nil {
				return ErrSignInExit
			}
			return nil
		case <-time.After(time.Second):
			if _, err := fmt.Fprintln(out, "The sign-in window is still open or the browser is still exiting. Finish closing it, then try again."); err != nil {
				return err
			}
		}
	}
}

// Launch opens an isolated browser and its initial tab without navigating.
// The caller owns the tab until close returns. The context bounds startup.
func Launch(ctx context.Context, binary string, options []chromedp.ExecAllocatorOption, profile string) (context.Context, func() error, error) {
	if binary == "" {
		return nil, nil, ErrBrowserMissing
	}
	options = append([]chromedp.ExecAllocatorOption{chromedp.Flag("headless", "new")}, options...)
	options = append(options, chromedp.ExecPath(binary), chromedp.UserDataDir(profile), chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck)
	owner, stopOwner := context.WithCancel(context.Background())
	allocator, release := chromedp.NewExecAllocator(owner, options...)
	browser, stopBrowser := chromedp.NewContext(allocator)
	stopStartup := context.AfterFunc(ctx, stopOwner)

	started := make(chan error, 1)
	go func() { started <- chromedp.Do(browser) }()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	var err error
	select {
	case err = <-started:
	case <-timer.C:
		stopOwner()
		err = context.DeadlineExceeded
		<-started
	}
	stopStartup()
	if err != nil || ctx.Err() != nil {
		stopOwner()
		stopBrowser()
		release()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, ErrLaunch
	}
	close := sync.OnceValue(func() error {
		closing, done := context.WithTimeout(browser, 5*time.Second)
		defer done()
		err := chromedp.Cancel(closing)
		stopOwner()
		stopBrowser()
		release()
		if err != nil && !errors.Is(err, context.Canceled) {
			return ErrClose
		}
		return nil
	})
	return browser, close, nil
}
