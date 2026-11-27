package stock

import (
	"context"

	"github.com/chromedp/chromedp"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	transport "github.com/nostalume/mountweave/browser"
	"github.com/nostalume/mountweave/drivers/baidupan"
	baiducdp "github.com/nostalume/mountweave/drivers/baidupan/cdp"
	"github.com/nostalume/mountweave/drivers/guangyapan"
	"github.com/nostalume/mountweave/drivers/mediafire"
	"github.com/nostalume/mountweave/drivers/pan115"
	pan115cdp "github.com/nostalume/mountweave/drivers/pan115/cdp"
	"github.com/nostalume/mountweave/drivers/tianyipan"
)

func baiduWebsiteRoute() *cliapp.Route {
	const entry = baiducdp.WebsiteURL
	source := websiteSource{ID: "baidupan", Name: "Baidu Netdisk", Entry: entry}
	source.Open = func(ctx context.Context, binary string, options []chromedp.ExecAllocatorOption, profile string) (websiteSession, error) {
		session, close, err := baiducdp.OpenGuardedSession(ctx, profile, options,
			func(options []chromedp.ExecAllocatorOption, check func(context.Context) error) (context.Context, func() error, error) {
				page, err := openWebsitePage(ctx, binary, options, profile, entry, nil, check)
				return page.Tab, page.Release, err
			})
		if err != nil {
			return websiteSession{}, err
		}
		return websiteSession{
			Account: session.Account,
			Mount: func(ctx context.Context, account, root, rootID string, release func() error) (websiteDriver, error) {
				return baidupan.OpenWebsite(ctx, baidupan.WebsiteOptions{
					Account: account, RootPath: root, RootID: rootID,
					ReadPage: session.ReadPage, ReadFile: session.ReadOrdinary, Release: release,
				})
			},
			Close: close,
		}, nil
	}
	route := source.route()
	return &route
}

func pan115WebsiteRoute() *cliapp.Route {
	const entry = pan115cdp.Origin + "/"
	source := websiteSource{
		ID: "pan115", Name: "115 Cloud Drive", Entry: entry,
		SignIn: pan115.ErrWebsiteSignIn, BindingMismatch: pan115.ErrWebsiteBindingMismatch,
	}
	route := source.withPage(entry, false, func(page websitePage) websiteSession {
		session := pan115cdp.New(page.Tab)
		return websiteSession{
			Account: session.Account,
			Mount: func(ctx context.Context, account, root, rootID string, release func() error) (websiteDriver, error) {
				return pan115.OpenWebsite(ctx, pan115.WebsiteOptions{
					Session: session, Account: account, RootPath: root, RootID: rootID, Release: release,
				})
			},
		}
	}).route()
	return &route
}

func mediaFireWebsiteRoute() *cliapp.Route {
	route := mediaFireWebsiteSource().route()
	return &route
}

func guangyaWebsiteRoute() *cliapp.Route {
	route := guangyaWebsiteSource().route()
	return &route
}

func guangyaWebsiteSource() websiteSource {
	const entry = "https://www.guangyapan.com/#/home/all"
	source := websiteSource{
		ID: "guangyapan", Name: "Guangya Pan", Entry: entry,
		RestoreSession: true,
		SignIn:         guangyapan.ErrWebsiteSignIn, BindingMismatch: guangyapan.ErrWebsiteBindingMismatch,
	}
	return source.withPage(entry, false, func(page websitePage) websiteSession {
		session := guangyapan.NewBrowserSession(page.Call)
		return websiteSession{
			Account: session.Account,
			Mount: func(ctx context.Context, account, root, rootID string, release func() error) (websiteDriver, error) {
				return guangyapan.OpenWebsite(ctx, guangyapan.WebsiteOptions{
					Session: session, Account: account, RootPath: root, RootID: rootID,
					ContentClient: storageHTTPClient, Release: release,
				})
			},
		}
	})
}

func tianyiWebsiteRoute() *cliapp.Route {
	route := tianyiWebsiteSource().route()
	return &route
}

func tianyiWebsiteSource() websiteSource {
	source := websiteSource{
		ID: "tianyipan", Name: "Tianyi Cloud Drive", Entry: tianyipan.WebsiteLoginURL,
		RestoreSession: true,
		SignIn:         tianyipan.ErrWebsiteSignIn, BindingMismatch: tianyipan.ErrWebsiteBindingMismatch,
	}
	return source.withPage(tianyipan.WebsiteURL, false, func(page websitePage) websiteSession {
		session := tianyipan.NewBrowserSession(page.Call)
		return websiteSession{
			Account: session.Account,
			Mount: func(ctx context.Context, account, root, rootID string, release func() error) (websiteDriver, error) {
				return tianyipan.OpenWebsite(ctx, tianyipan.WebsiteOptions{
					Session: session, Account: account, RootPath: root, RootID: rootID,
					ContentClient: storageHTTPClient, Release: release,
				})
			},
		}
	})
}

func mediaFireWebsiteSource() websiteSource {
	const origin = "https://app.mediafire.com"
	source := websiteSource{
		ID: "mediafire", Name: "MediaFire", Entry: origin + "/", Access: "Browse, download, upload and create directories",
		Instructions:   "For email and password, keep “Keep me logged in” selected. Open My Files before closing the sign-in window.",
		RestoreSession: true,
		SignIn:         mediafire.ErrWebsiteSignIn, BindingMismatch: mediafire.ErrWebsiteBindingMismatch,
	}
	return source.withPage(origin+"/folder/myfiles", false, func(page websitePage) websiteSession {
		session := mediafire.NewBrowserSession(page.Call, storageHTTPClient, func(ctx context.Context, entryURL, pathPrefix, expression string) (string, error) {
			return transport.ReadPublicDocument(ctx, page.Tab, entryURL, pathPrefix, expression)
		})
		return websiteSession{
			Account: session.Account,
			Mount: func(ctx context.Context, account, root, rootID string, release func() error) (websiteDriver, error) {
				directory, err := websiteUploadDirectory()
				if err != nil {
					return nil, err
				}
				return mediafire.OpenWebsite(ctx, mediafire.WebsiteOptions{
					Session: session, Account: account, RootPath: root, RootID: rootID, Release: release,
					Upload: &mediafire.WebsiteUploadOptions{StagingDirectory: directory, MaxStagingBytes: maxWebsiteStagingBytes},
				})
			},
		}
	})
}
