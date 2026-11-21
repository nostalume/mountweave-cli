package stock

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave-cli/internal/website"
)

type websiteMountOptions struct {
	Root string `json:"root"`
}

type websiteMountGrant struct {
	Provider string `json:"provider"`
	Profile  string `json:"profile"`
	Account  string `json:"account"`
	Root     string `json:"root"`
	RootID   string `json:"root_id"`
}

type websiteDriver interface {
	core.Driver
	RootID() string
}

type websiteSession struct {
	Account func(context.Context) (string, error)
	// The native constructor checks any expected root ID before taking release.
	// On failure the session stays open for another root or caller cleanup.
	Mount func(ctx context.Context, account, root, rootID string, release func() error) (websiteDriver, error)
	Close func() error
}

type websiteSource struct {
	ID, Name, Entry string
	SignIn          error
	BindingMismatch error
	Open            func(context.Context, string, []chromedp.ExecAllocatorOption, string) (websiteSession, error)
}

func (s websiteSource) route() cliapp.Route {
	r := cliapp.SourceRoute(nil, s.prepare)
	r.SignIn, r.SignInProblem, r.BindingMismatch = s.SignIn, websiteSignInProblem(s.Name), s.BindingMismatch
	return attachWebsiteSetup(r, websiteLogin{Driver: s.ID, Name: s.Name, Entry: s.Entry}, s.restore, s.verify)
}

func (s websiteSource) prepare(options websiteMountOptions, _ string) (cliapp.MountSource, error) {
	if err := core.ValidatePath(options.Root); err != nil {
		return cliapp.MountSource{}, cliapp.InvalidConfig(s.Name + " requires a verified folder. Use mweave mount add to choose it.")
	}
	return cliapp.MountSource{ProbeOnChange: true, Audience: cliapp.CredentialAudience{Origin: s.Entry, Account: options.Root},
		OpenManaged: func(ctx context.Context, id string) (core.Driver, error) {
			return s.openMount(ctx, id, options.Root)
		},
	}, nil
}

func (s websiteSource) readGrant(read func() ([]byte, error), root string) (websiteMountGrant, error) {
	return readWebsiteGrant(read, func(grant websiteMountGrant) bool {
		return grant.Provider == s.ID && cliapp.ValidGrantID(grant.Profile) &&
			grant.Account != "" && grant.Root == root && grant.RootID != ""
	})
}

func (s websiteSource) restore(options websiteMountOptions, read func() ([]byte, error)) (websiteBinding[websiteMountGrant], error) {
	grant, err := s.readGrant(read, options.Root)
	grant.Root = options.Root
	return websiteBinding[websiteMountGrant]{Native: grant, Profile: grant.Profile, Folder: options.Root, Verified: grant.Account != ""}, err
}

func (s websiteSource) verify(ctx context.Context, candidate *websiteCandidate, grant websiteMountGrant) (websiteSelection, error) {
	session, err := s.Open(ctx, candidate.Browser, candidate.Options, candidate.Profile)
	if err != nil {
		return websiteSelection{}, err
	}
	candidate.Release = session.Close
	if grant.Account == "" {
		grant.Account, err = session.Account(ctx)
		if err != nil {
			return websiteSelection{}, err
		}
	}
	folder, d, err := chooseWebsiteFolder(candidate, s.Name, grant.Root, !candidate.Existing, func(folder string) (websiteDriver, error) {
		return session.Mount(ctx, grant.Account, folder, grant.RootID, session.Close)
	})
	if err != nil {
		return websiteSelection{}, err
	}
	candidate.Release = d.Close
	grant.Provider, grant.Profile, grant.Root, grant.RootID = s.ID, candidate.ID, folder, d.RootID()
	return websiteSelection{Options: websiteMountOptions{Root: folder}, Grant: grant, Folder: folder}, nil
}

func (s websiteSource) openMount(ctx context.Context, id, root string) (core.Driver, error) {
	return reopenWebsite(id, func(read func() ([]byte, error)) (websiteMountGrant, string, error) {
		grant, err := s.readGrant(read, root)
		return grant, grant.Profile, err
	}, func(grant websiteMountGrant, binary string, options []chromedp.ExecAllocatorOption, profile string, closeGrant func() error) (core.Driver, error) {
		session, err := s.Open(ctx, binary, options, profile)
		if err != nil {
			return nil, err
		}
		release := func() error { return errors.Join(session.Close(), closeGrant()) }
		d, err := session.Mount(ctx, grant.Account, grant.Root, grant.RootID, release)
		if err != nil {
			return nil, errors.Join(err, session.Close())
		}
		return d, nil
	})
}

// reopenWebsite holds the grant lease until the opened session closes. The native
// opener takes its close callback on success and closes its own resources on failure.
func reopenWebsite[B, T any](id string,
	bind func(func() ([]byte, error)) (B, string, error),
	open func(B, string, []chromedp.ExecAllocatorOption, string, func() error) (T, error),
) (zero T, err error) {
	lease, err := cliapp.AcquireGrant(id, false)
	if err != nil {
		return zero, err
	}
	closeGrant := lease.Lock.Close
	defer func() {
		if err != nil {
			err = errors.Join(err, closeGrant())
		}
	}()
	binding, profileID, err := bind(lease.Read)
	if err != nil {
		return zero, err
	}
	profile, err := websiteProfile(profileID, false)
	if err != nil {
		return zero, err
	}
	binary, options := website.Browser()
	return open(binding, binary, options, profile, closeGrant)
}

var signInWebsite = func(ctx context.Context, binary, profile, page string, ask cliapp.Prompt, out io.Writer) error {
	err := website.SignIn(ctx, binary, profile, page, func() error {
		return cliapp.Confirm(ask, "Continue after signing in and closing the window?")
	}, out)
	switch err {
	case website.ErrBrowserMissing:
		return websiteBrowserMissing()
	case website.ErrSignInStart:
		return &cliapp.Problem{Code: "browser_unavailable", Message: err.Error(), NextAction: "Check that the selected browser can open a separate profile, then retry."}
	case website.ErrSignInExit:
		return &cliapp.Problem{Code: "browser_unavailable", Message: err.Error(), NextAction: "Retry sign-in and close the window normally before continuing."}
	default:
		return err
	}
}

func websiteBrowserMissing() error {
	return &cliapp.Problem{Code: "browser_unavailable", Message: website.BrowserUnavailable, NextAction: "Use another connection method offered for this source."}
}

func openWebsitePage(ctx context.Context, binary string, options []chromedp.ExecAllocatorOption, profile, page string, blockedPaths []string) (websitePage, error) {
	origin, err := url.Parse(page)
	if err != nil || origin.Scheme != "https" || origin.Host == "" {
		return websitePage{}, core.Fail(core.InvalidRequest, nil)
	}
	browser, release, err := website.Launch(ctx, binary, options, profile)
	if err != nil {
		switch err {
		case website.ErrBrowserMissing:
			return websitePage{}, websiteBrowserMissing()
		case website.ErrLaunch:
			return websitePage{}, &cliapp.Problem{Code: "browser_unavailable", Message: err.Error(), NextAction: "Make sure the browser can open a separate profile, then retry."}
		default:
			return websitePage{}, err
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
		return websitePage{}, errors.Join(core.Fail(core.UpstreamFailure, errors.New("website navigation failed")), closeBrowser())
	}
	turn := make(chan struct{}, 1)
	site := strconv.Quote(origin.Scheme + "://" + origin.Host)
	path := strconv.Quote(origin.Path)
	subpath := strconv.Quote(strings.TrimSuffix(origin.Path, "/") + "/")
	call := func(ctx context.Context, function string, args json.RawMessage) (json.RawMessage, error) {
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
	}
	return websitePage{Call: call, Release: closeBrowser, Tab: browser}, nil
}

type websitePage struct {
	Call    func(context.Context, string, json.RawMessage) (json.RawMessage, error)
	Release func() error
	Tab     context.Context
}

func (s websiteSource) pageRoute(entry string, visible bool, bind func(websitePage) websiteSession) cliapp.Route {
	s.Open = func(ctx context.Context, binary string, options []chromedp.ExecAllocatorOption, profile string) (websiteSession, error) {
		if visible {
			options = append(options, chromedp.VisibleWindow)
		}
		page, err := openWebsitePage(ctx, binary, options, profile, entry, nil)
		if err != nil {
			return websiteSession{}, err
		}
		return bind(page), nil
	}
	return s.route()
}
