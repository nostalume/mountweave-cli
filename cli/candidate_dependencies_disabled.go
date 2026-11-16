//go:build !mweave_candidate

package cli

import (
	"context"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/weiyun"
	"github.com/nostalume/mountweave/protocols/webdav"
)

const websiteCandidateAvailable = false
const weiyunSubfoldersAvailable = false
const webdavCreateOnlyAvailable = false

var websiteCandidateRoutes map[string]route

func websiteSignInProvider(error) string { return "" }

func withAliPanWebsiteSessions(ctx context.Context) context.Context { return ctx }

func openWeiyun(ctx context.Context, opts weiyun.Options, _ string) (*weiyun.Driver, error) {
	return weiyun.Open(ctx, opts)
}

func openWebDAV(c *core.Core, options webdav.Options, createOnly bool) (*webdav.Handler, error) {
	if createOnly {
		return nil, createOnlyUnavailable()
	}
	return webdav.New(c, options)
}
