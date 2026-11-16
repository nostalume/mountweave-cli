package cli

import (
	"context"
	"net/http"
	"strings"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/dingtalkdrive"
	"github.com/nostalume/mountweave/drivers/gokuai"
	"github.com/nostalume/mountweave/drivers/tencentsmh"
	"github.com/nostalume/mountweave/drivers/wecomdrive"
)

type gokuaiOptions struct {
	LibraryID        string `json:"library_id"`
	RootPath         string `json:"root_path,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideGokuai(o *gokuaiOptions, ask askField) error {
	return askFields(ask, setupField{"Library ID", &o.LibraryID},
		setupField{"Folder path (/ for library root)", &o.RootPath})
}

func prepareGokuai(o gokuaiOptions, _ string) (mountSource, error) {
	if o.LibraryID == "" || strings.ContainsAny(o.LibraryID, "\r\n") ||
		(o.RootPath != "" && core.ValidatePath(o.RootPath) != nil) ||
		!validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("Gokuai needs a valid library, folder path and response budget.")
	}
	return mountSource{
		audience: credentialAudience{origin: "gokuai https://yk3.gokuai.com", account: o.LibraryID},
		secret:   "Library secret",
		open: func(ctx context.Context, secret string) (core.Driver, error) {
			return nativeDriver(gokuai.Open(ctx, gokuai.Options{
				LibraryID: o.LibraryID, LibrarySecret: secret, RootPath: o.RootPath,
				MaxResponseBytes: o.MaxResponseBytes,
			}))
		},
	}, nil
}

func askSpaceFolder(ask askField, space, root *string) error {
	return askFields(ask, setupField{"Space ID", space}, setupField{"Folder ID", root})
}

type dingTalkDriveOptions struct {
	SpaceID          string `json:"space_id"`
	RootID           string `json:"root_id"`
	OperatorUnionID  string `json:"operator_union_id"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideDingTalkDrive(o *dingTalkDriveOptions, ask askField) error {
	if err := askSpaceFolder(ask, &o.SpaceID, &o.RootID); err != nil {
		return err
	}
	return askFields(ask, setupField{"Operator union ID", &o.OperatorUnionID})
}

func prepareDingTalkDrive(o dingTalkDriveOptions, _ string) (mountSource, error) {
	if !core.ValidName(o.SpaceID) || !core.ValidName(o.RootID) || !core.ValidName(o.OperatorUnionID) ||
		!validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("DingTalk needs a space ID, folder ID, operator union ID and valid response budget.")
	}
	return headerTokenSource("https", "X-Acs-Dingtalk-Access-Token", "",
		func(ctx context.Context, api *http.Client) (core.Driver, error) {
			return nativeDriver(dingtalkdrive.Open(ctx, dingtalkdrive.Options{
				SpaceID: o.SpaceID, RootID: o.RootID, OperatorUnionID: o.OperatorUnionID,
				MaxResponseBytes: o.MaxResponseBytes, Client: api,
			}))
		}, "api.dingtalk.com"), nil
}

type weComDriveOptions struct {
	SpaceID          string `json:"space_id"`
	RootID           string `json:"root_id"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideWeComDrive(o *weComDriveOptions, ask askField) error {
	return askSpaceFolder(ask, &o.SpaceID, &o.RootID)
}

func prepareWeComDrive(o weComDriveOptions, _ string) (mountSource, error) {
	if o.SpaceID == "" || o.RootID == "" || o.SpaceID == o.RootID ||
		!validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("WeCom needs a space ID, a distinct folder ID and a valid response budget.")
	}
	return mountSource{
		audience: credentialAudience{origin: "wecomdrive https://qyapi.weixin.qq.com"},
		secret:   "WeCom app access token",
		open: func(ctx context.Context, token string) (core.Driver, error) {
			return nativeDriver(wecomdrive.Open(ctx, wecomdrive.Options{
				SpaceID: o.SpaceID, FolderID: o.RootID, MaxResponseBytes: o.MaxResponseBytes,
				AccessToken: func(context.Context) (string, error) { return token, nil },
			}))
		},
	}, nil
}

type tencentSMHOptions struct {
	LibraryID        string `json:"library_id"`
	SpaceID          string `json:"space_id,omitempty"`
	RootPath         string `json:"root_path,omitempty"`
	Endpoint         string `json:"endpoint,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideTencentSMH(o *tencentSMHOptions, ask askField) error {
	return askFields(ask, setupField{"Library ID", &o.LibraryID}, setupField{"Space ID (empty for a single-tenant library)", &o.SpaceID},
		setupField{"Folder path (/ for space root)", &o.RootPath})
}

func prepareTencentSMH(o tencentSMHOptions, _ string) (mountSource, error) {
	if o.Endpoint == "" {
		o.Endpoint = "https://smh.tencentcs.com"
	}
	if o.SpaceID == "" {
		o.SpaceID = "-"
	}
	origin, err := sourceURL(o.Endpoint)
	if err != nil {
		return mountSource{}, err
	}
	if origin.Scheme != "https" || (origin.Path != "" && origin.Path != "/") ||
		!core.ValidName(o.LibraryID) || !core.ValidName(o.SpaceID) ||
		(o.RootPath != "" && core.ValidatePath(o.RootPath) != nil) ||
		!validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("Tencent SMH needs an HTTPS origin, a valid library and space, and valid folder and response settings.")
	}
	return mountSource{
		audience: credentialAudience{origin: storageOrigin("tencentsmh", origin), account: o.LibraryID + "/" + o.SpaceID},
		secret:   "SMH business access token",
		open: func(ctx context.Context, token string) (core.Driver, error) {
			return nativeDriver(tencentsmh.Open(ctx, tencentsmh.Options{
				LibraryID: o.LibraryID, SpaceID: o.SpaceID, RootPath: o.RootPath, Endpoint: o.Endpoint,
				MaxResponseBytes: o.MaxResponseBytes, AccessToken: func(context.Context) (string, error) { return token, nil },
			}))
		},
	}, nil
}
