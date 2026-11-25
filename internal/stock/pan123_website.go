package stock

import (
	"context"

	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave/drivers/pan123"
	pan123cdp "github.com/nostalume/mountweave/drivers/pan123/cdp"
)

func pan123WebsiteRoute() *cliapp.Route {
	const entry = pan123cdp.Origin + "/"
	source := websiteSource{
		ID: "pan123", Connection: "website", Name: "123 Cloud Drive", Entry: entry,
		SignIn: pan123.ErrWebsiteSignIn, BindingMismatch: pan123.ErrWebsiteBindingMismatch,
	}
	route := source.pageRoute(entry, false, func(page websitePage) websiteSession {
		session := pan123cdp.New(page.Tab)
		return websiteSession{
			Account: session.Account,
			Mount: func(ctx context.Context, account, root, rootID string, release func() error) (websiteDriver, error) {
				return pan123.OpenWebsite(ctx, pan123.WebsiteOptions{
					Session: session, Account: account, RootPath: root, RootID: rootID,
					ContentClient: storageHTTPClient, Release: release,
				})
			},
			Close: page.Release,
		}
	})
	return &route
}
