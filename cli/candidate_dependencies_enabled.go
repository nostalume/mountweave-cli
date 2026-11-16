//go:build mweave_candidate

package cli

import (
	"context"
	"errors"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/alipan"
	"github.com/nostalume/mountweave/drivers/mobilepan"
	"github.com/nostalume/mountweave/drivers/weiyun"
	"github.com/nostalume/mountweave/protocols/webdav"
)

const websiteCandidateAvailable = true
const weiyunSubfoldersAvailable = true
const webdavCreateOnlyAvailable = true

var websiteCandidateRoutes = map[string]route{
	aliPanWebsiteSource: aliPanWebsiteRoute(),
	mobilePanSource:     mobilePanWebsiteRoute(),
}

func websiteSignInProvider(err error) string {
	switch {
	case errors.Is(err, alipan.ErrWebsiteSignIn):
		return "Aliyun Drive"
	case errors.Is(err, mobilepan.ErrWebsiteSignIn):
		return "China Mobile Cloud"
	default:
		return ""
	}
}

func openWeiyun(ctx context.Context, opts weiyun.Options, root string) (*weiyun.Driver, error) {
	opts.RootPath = root
	return weiyun.Open(ctx, opts)
}

func openWebDAV(c *core.Core, options webdav.Options, createOnly bool) (*webdav.Handler, error) {
	options.CreateOnly = createOnly
	return webdav.New(c, options)
}
