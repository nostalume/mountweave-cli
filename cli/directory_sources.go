package cli

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/box"
	"github.com/nostalume/mountweave/drivers/fangcloud"
	"github.com/nostalume/mountweave/drivers/feishudrive"
	"github.com/nostalume/mountweave/drivers/guangyapan"
	"github.com/nostalume/mountweave/drivers/kdocs"
	"github.com/nostalume/mountweave/drivers/tencentdocs"
)

type feishuOptions struct {
	RootID           string            `json:"root_id"`
	ExportFormats    map[string]string `json:"export_formats,omitempty"`
	MaxResponseBytes int64             `json:"max_response_bytes,omitempty"`
	MaxResolvePages  int               `json:"max_resolve_pages,omitempty"`
}

func guideFeishu(o *feishuOptions, ask askField) error {
	return askFields(ask, setupField{"Feishu folder ID (folder token)", &o.RootID})
}

func prepareFeishu(o feishuOptions, _ string) (mountSource, error) {
	if !core.ValidName(o.RootID) || !validResponseBudget(o.MaxResponseBytes) || o.MaxResolvePages < 0 {
		return mountSource{}, invalidConfig("Feishu needs a folder token and valid response settings.")
	}
	for source, format := range o.ExportFormats {
		if source == "" || format == "" {
			return mountSource{}, invalidConfig("Feishu export settings need both a document type and an output format.")
		}
	}
	return bearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return nativeDriver(feishudrive.Open(ctx, feishudrive.Options{
			RootToken: o.RootID, Client: api, ExportFormats: o.ExportFormats,
			MaxResponseBytes: o.MaxResponseBytes, MaxResolvePages: o.MaxResolvePages,
		}))
	}, "open.feishu.cn"), nil
}

func prepareFangcloud(o accountFolderOptions, _ string) (mountSource, error) {
	if err := o.check(); err != nil {
		return mountSource{}, err
	}
	var id int64
	if o.RootID != "" {
		var err error
		id, err = strconv.ParseInt(o.RootID, 10, 64)
		if err != nil || id <= 0 {
			return mountSource{}, invalidConfig("Fangcloud needs a positive numeric folder ID.")
		}
	}
	return bearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return nativeDriver(fangcloud.Open(ctx, fangcloud.Options{
			RootID: id, RootPath: o.RootPath, Client: api,
			MaxResponseBytes: o.MaxResponseBytes, MaxResolvePages: o.MaxResolvePages,
		}))
	}, "open.fangcloud.com"), nil
}

type guangyaOptions struct {
	ClientID         string `json:"client_id"`
	RootID           string `json:"root_id,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideGuangya(o *guangyaOptions, ask askField) error {
	if err := askFields(ask, setupField{"Application client ID", &o.ClientID},
		setupField{"Folder ID (empty for account root; - resets)", &o.RootID}); err != nil {
		return err
	}
	if o.RootID == "-" {
		o.RootID = ""
	}
	return nil
}

func prepareGuangya(o guangyaOptions, _ string) (mountSource, error) {
	if o.ClientID == "" || strings.ContainsAny(o.ClientID, "\r\n") ||
		!validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("Guangya Pan needs an application client ID and a valid response budget.")
	}
	source := bearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return nativeDriver(guangyapan.Open(ctx, guangyapan.Options{
			ClientID: o.ClientID, RootID: o.RootID, Client: api, MaxResponseBytes: o.MaxResponseBytes,
		}))
	}, "openapi.guangyapan.com")
	source.audience.account = o.ClientID
	return source, nil
}

type kdocsOptions struct {
	RootPath         string `json:"root_path,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideKDocs(o *kdocsOptions, ask askField) error {
	return askFields(ask, setupField{"Folder path (/ for My Documents)", &o.RootPath})
}

func prepareKDocs(o kdocsOptions, _ string) (mountSource, error) {
	if (o.RootPath != "" && core.ValidatePath(o.RootPath) != nil) ||
		!validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("KDocs needs a valid folder path and response budget.")
	}
	return mountSource{
		audience: credentialAudience{origin: "kdocs https://developer.kdocs.cn"},
		secret:   "KDocs user access token",
		open: func(ctx context.Context, token string) (core.Driver, error) {
			return nativeDriver(kdocs.Open(ctx, kdocs.Options{
				RootPath: o.RootPath, MaxResponseBytes: o.MaxResponseBytes,
				AccessToken: func(context.Context) (string, error) { return token, nil },
			}))
		},
	}, nil
}

type boxOptions struct {
	RootID           string `json:"root_id,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideBox(o *boxOptions, ask askField) error {
	return askFields(ask, setupField{"Box folder ID (0 for account root)", &o.RootID})
}

func prepareBox(o boxOptions, _ string) (mountSource, error) {
	if (o.RootID != "" && !core.ValidName(o.RootID)) ||
		!validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("Box needs a valid folder ID and response budget.")
	}
	return bearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return nativeDriver(box.Open(ctx, box.Options{
			RootID: o.RootID, Client: api, MaxResponseBytes: o.MaxResponseBytes,
		}))
	}, "api.box.com"), nil
}

type tencentDocsOptions struct {
	RootID           string `json:"root_id"`
	ClientID         string `json:"client_id"`
	OpenID           string `json:"open_id"`
	AllowExport      bool   `json:"allow_export,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideTencentDocs(o *tencentDocsOptions, ask askField) error {
	return askFields(ask, setupField{"Tencent Docs folder ID", &o.RootID},
		setupField{"Application client ID", &o.ClientID}, setupField{"User Open ID", &o.OpenID})
}

func prepareTencentDocs(o tencentDocsOptions, _ string) (mountSource, error) {
	if !core.ValidName(o.RootID) || !validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("Tencent Docs needs a folder ID and valid response budget.")
	}
	for _, id := range []string{o.ClientID, o.OpenID} {
		if id == "" || strings.TrimSpace(id) != id || strings.ContainsFunc(id, func(r rune) bool { return r < ' ' || r == 127 }) {
			return mountSource{}, invalidConfig("Tencent Docs needs a valid application client ID and user Open ID.")
		}
	}
	const host = "docs.qq.com"
	return mountSource{
		audience: credentialAudience{origin: "Access-Token https://" + host, account: o.ClientID + "\n" + o.OpenID},
		secret:   "Tencent Docs user access token",
		open: func(ctx context.Context, token string) (core.Driver, error) {
			api := &http.Client{Transport: tokenHeaderTransport{
				base: http.DefaultTransport, scheme: "https", hosts: []string{host},
				headers: map[string]string{"Access-Token": token, "Client-Id": o.ClientID, "Open-Id": o.OpenID},
			}}
			return nativeDriver(tencentdocs.Open(ctx, tencentdocs.Options{
				FolderID: o.RootID, Client: api, AllowExport: o.AllowExport, MaxResponseBytes: o.MaxResponseBytes,
			}))
		},
	}, nil
}
