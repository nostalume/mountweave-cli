package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/internal/website"
)

var signInWebsite = func(ctx context.Context, binary, profile, page string, ask askField, out io.Writer) error {
	err := website.SignIn(ctx, binary, profile, page, func() error {
		return confirm(ask, "Continue after signing in and closing the window?")
	}, out)
	switch err {
	case website.ErrBrowserMissing:
		return websiteBrowserMissing()
	case website.ErrSignInStart:
		return &problem{Code: "browser_unavailable", Message: err.Error(), NextAction: "Check that the selected browser can open a separate profile, then retry."}
	case website.ErrSignInExit:
		return &problem{Code: "browser_unavailable", Message: err.Error(), NextAction: "Retry sign-in and close the window normally before continuing."}
	default:
		return err
	}
}

func websiteBrowser() (string, []chromedp.ExecAllocatorOption) {
	return website.Browser()
}

func privateWebsiteDirectory(path string, create bool) error {
	err := website.PrivateDirectory(path, create)
	if err == website.ErrUnsafeDirectory {
		return grantUnavailable()
	}
	return err
}

func websiteBrowserMissing() error {
	return &problem{Code: "browser_unavailable", Message: website.BrowserUnavailable, NextAction: "Use another connection method offered for this source."}
}

func openWebsite(ctx context.Context, binary string, options []chromedp.ExecAllocatorOption, profile, page string, blockedPaths []string) (call func(context.Context, string, json.RawMessage) (json.RawMessage, error), releaseSession func() error, tab context.Context, err error) {
	origin, err := url.Parse(page)
	if err != nil || origin.Scheme != "https" || origin.Host == "" {
		return nil, nil, nil, core.Fail(core.InvalidRequest, nil)
	}
	browser, release, err := website.Launch(ctx, binary, options, profile)
	if err != nil {
		switch err {
		case website.ErrBrowserMissing:
			return nil, nil, nil, websiteBrowserMissing()
		case website.ErrLaunch:
			return nil, nil, nil, &problem{Code: "browser_unavailable", Message: err.Error(), NextAction: "Make sure the browser can open a separate profile, then retry."}
		default:
			return nil, nil, nil, err
		}
	}
	closeBrowser := func() error {
		if err := release(); err != nil {
			return core.Fail(core.UpstreamFailure, err)
		}
		return nil
	}
	navigation, stopNavigation := context.WithTimeout(browser, 15*time.Second)
	stopOnCancel := context.AfterFunc(ctx, stopNavigation)
	patterns := make([]*network.BlockPattern, 0, len(blockedPaths))
	for _, path := range blockedPaths {
		patterns = append(patterns, &network.BlockPattern{URLPattern: "https://*:*" + path, Block: true})
	}
	_, err = chromedp.Call(navigation, network.SetBlockedURLs, network.SetBlockedURLsParams{URLPatterns: patterns})
	if err == nil {
		_, err = chromedp.Run(navigation, chromedp.Navigate(page))
	}
	stopOnCancel()
	stopNavigation()
	if err != nil {
		return nil, nil, nil, errors.Join(core.Fail(core.UpstreamFailure, errors.New("website navigation failed")), closeBrowser())
	}
	turn := make(chan struct{}, 1)
	site := strconv.Quote(origin.Scheme + "://" + origin.Host)
	path := strconv.Quote(origin.Path)
	subpath := strconv.Quote(strings.TrimSuffix(origin.Path, "/") + "/")
	return func(ctx context.Context, function string, args json.RawMessage) (json.RawMessage, error) {
		select {
		case turn <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-browser.Done():
			return nil, core.Fail(core.MountUnavailable, nil)
		}
		defer func() { <-turn }()
		action, cancel := context.WithTimeout(browser, 10*time.Second)
		defer cancel()
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// A sign-in redirect may stay on the same origin but leave the app.
		expression := "(async()=>location.origin===" + site +
			"&&window===window.top&&(location.pathname===" + path + "||location.pathname.startsWith(" + subpath + "))?await (" + function + ")(" + string(args) +
			"):" + strconv.Quote(`{"sign_in":true}`) + ")()"
		reply, err := chromedp.Run(action, chromedp.Evaluate[string](expression, chromedp.EvalAwaitPromise))
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case browser.Err() != nil:
			return nil, core.Fail(core.MountUnavailable, nil)
		case errors.Is(err, context.DeadlineExceeded):
			return nil, core.Fail(core.UpstreamTimeout, nil)
		case err != nil:
			return nil, core.Fail(core.InvalidUpstreamResponse, nil)
		}
		return json.RawMessage(reply), nil
	}, closeBrowser, browser, nil
}

func newWebsiteID() string {
	var key [16]byte
	_, _ = rand.Read(key[:])
	return hex.EncodeToString(key[:])
}

func websiteProfile(id string, create bool) (string, error) {
	if !validGrantID(id) {
		return "", grantUnavailable()
	}
	grants, err := grantDirectory(create)
	if err != nil {
		return "", err
	}
	root := filepath.Join(grants, "website")
	profile := filepath.Join(root, id)
	for _, path := range []string{root, profile} {
		if err := privateWebsiteDirectory(path, create); err != nil {
			return "", grantUnavailable()
		}
	}
	return profile, nil
}

func removeWebsiteProfile(profile string) error {
	base, err := grantDirectory(false)
	if err != nil || filepath.Dir(profile) != filepath.Join(base, "website") || !validGrantID(filepath.Base(profile)) {
		return grantUnavailable()
	}
	return os.RemoveAll(profile)
}

func checkWebsiteMount(saved configSnapshot, at, replacing string) error {
	if !managedStoreSupported {
		return grantUnavailable()
	}
	if err := core.ValidatePath(at); err != nil {
		return err
	}
	if mountPathConflicts(saved.value.Mounts, at, replacing) {
		return configConflict()
	}
	return checkWebsiteStorage(saved)
}

// replaceWebsiteGrant adopts a verified profile under an existing grant lease.
func replaceWebsiteGrant(ctx context.Context, saved configSnapshot, at string, lease *grantLease, oldProfile string, data []byte) (_ mountReceipt, _ string, err error) {
	unchanged := mountReceipt{At: at, Effect: core.Unchanged.String()}
	applied := mountReceipt{At: at, Effect: core.Applied.String()}
	lock, err := saved.lockUnchanged(ctx)
	if err != nil {
		return unchanged, "", err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := lease.save(ctx, data, true); err != nil {
		written, readErr := lease.read()
		if readErr == nil && bytes.Equal(written, data) {
			return applied, "", err
		}
		return unchanged, "", errors.Join(err, readErr)
	}
	dir, err := websiteProfile(oldProfile, false)
	if err == nil {
		err = removeWebsiteProfile(dir)
	} else if grants, dirErr := grantDirectory(false); dirErr == nil {
		if _, statErr := os.Lstat(filepath.Join(grants, "website", oldProfile)); errors.Is(statErr, os.ErrNotExist) {
			err = nil
		}
	}
	return applied, fmt.Sprintf("Signed in again for %q.\n", at), err
}
