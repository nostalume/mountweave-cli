package cli

import (
	"context"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/weiyun"
	"github.com/nostalume/mountweave/drivers/yunpan360"
)

type authorizedRootOptions struct {
	MaxResponseBytes int64 `json:"max_response_bytes,omitempty"`
}

func (o authorizedRootOptions) source(origin, credential string, open func(context.Context, string) (core.Driver, error)) (mountSource, error) {
	if !validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("The source needs a valid response budget.")
	}
	return mountSource{
		audience: credentialAudience{origin: origin},
		secret:   credential,
		open:     open,
	}, nil
}

type weiyunOptions struct {
	RootPath         string `json:"root_path,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideWeiyun(o *weiyunOptions, ask askField) error {
	if !weiyunSubfoldersAvailable {
		return nil
	}
	root := o.RootPath
	if root == "" {
		root = "/"
	}
	prompt := "Folder path inside Weiyun"
	for {
		answer, err := ask(prompt, root)
		if err != nil {
			return err
		}
		if core.ValidatePath(answer) == nil {
			o.RootPath = answer
			return nil
		}
		prompt = "Enter an absolute folder path inside Weiyun, such as /Documents.\nFolder path inside Weiyun"
	}
}

func prepareWeiyun(o weiyunOptions, _ string) (mountSource, error) {
	if o.RootPath != "" && core.ValidatePath(o.RootPath) != nil {
		return mountSource{}, invalidConfig("Enter an absolute folder path inside Weiyun, such as /Documents.")
	}
	if !weiyunSubfoldersAvailable && o.RootPath != "" && o.RootPath != "/" {
		return mountSource{}, &problem{Code: string(core.MountUnavailable), Message: "This build supports only the Weiyun account root."}
	}
	src, err := (authorizedRootOptions{MaxResponseBytes: o.MaxResponseBytes}).source(weiyunOrigin,
		"Weiyun MCP token", func(ctx context.Context, token string) (core.Driver, error) {
			return nativeDriver(openWeiyun(ctx, weiyun.Options{
				Token: token, MaxResponseBytes: o.MaxResponseBytes,
			}, o.RootPath))
		})
	if err != nil {
		return mountSource{}, err
	}
	src.probeOnChange = true
	src.openManaged = func(ctx context.Context, id string) (core.Driver, error) {
		grant, err := weiyunAuthorization.loadGrant(id)
		if err != nil {
			return nil, err
		}
		return src.open(ctx, grant.Access)
	}
	return src, nil
}

func prepareYunpan360(o authorizedRootOptions, _ string) (mountSource, error) {
	return o.source("https://mcp.yunpan.com", "360 AI Cloud Disk API key", func(ctx context.Context, key string) (core.Driver, error) {
		return nativeDriver(yunpan360.Open(ctx, yunpan360.Options{APIKey: key, MaxResponseBytes: o.MaxResponseBytes}))
	})
}
