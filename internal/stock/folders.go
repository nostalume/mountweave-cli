package stock

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave/drivers/alipan"
	"github.com/nostalume/mountweave/drivers/baidupan"
	"github.com/nostalume/mountweave/drivers/box"
	"github.com/nostalume/mountweave/drivers/cloudreve"
	"github.com/nostalume/mountweave/drivers/dingtalkdrive"
	"github.com/nostalume/mountweave/drivers/dropbox"
	"github.com/nostalume/mountweave/drivers/fangcloud"
	"github.com/nostalume/mountweave/drivers/feishudrive"
	"github.com/nostalume/mountweave/drivers/gokuai"
	"github.com/nostalume/mountweave/drivers/googledrive"
	"github.com/nostalume/mountweave/drivers/graphdrive"
	"github.com/nostalume/mountweave/drivers/guangyapan"
	"github.com/nostalume/mountweave/drivers/kdocs"
	"github.com/nostalume/mountweave/drivers/pan115"
	"github.com/nostalume/mountweave/drivers/pan123"
	"github.com/nostalume/mountweave/drivers/seafile"
	"github.com/nostalume/mountweave/drivers/tencentdocs"
	"github.com/nostalume/mountweave/drivers/tencentsmh"
	"github.com/nostalume/mountweave/drivers/wecomdrive"
	"github.com/nostalume/mountweave/drivers/weiyun"
	"github.com/nostalume/mountweave/drivers/yunpan360"
)

type accountFolderOptions struct {
	RootPath         string `json:"root_path,omitempty"`
	RootID           string `json:"root_id,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
	MaxResolvePages  int    `json:"max_resolve_pages,omitempty"`
}

func askFolder(ask cliapp.Prompt, id, path *string, label string) error {
	selected := cmp.Or(*id, *path)
	if err := askClearable(ask, label, &selected); err != nil {
		return err
	}
	*id, *path = folderLocation(selected)
	return nil
}

func askClearable(ask cliapp.Prompt, label string, field *string) error {
	answer, err := ask(label, *field)
	if err != nil {
		return err
	}
	if answer == "-" {
		answer = ""
	}
	*field = answer
	return nil
}

func folderLocation(selected string) (id, path string) {
	if strings.HasPrefix(selected, "/") {
		return "", selected
	}
	return selected, ""
}

func guideAccountFolder(opts *accountFolderOptions, ask cliapp.Prompt) error {
	return askFolder(ask, &opts.RootID, &opts.RootPath, "Folder path (/...) or ID (empty for account root; - resets)")
}

func validFolderSettings(path string, responseBytes int64) bool {
	return (path == "" || core.ValidatePath(path) == nil) && cliapp.ValidResponseBudget(responseBytes)
}

func (o accountFolderOptions) check() error {
	if (o.RootID != "" && (o.RootPath != "" || (len(o.RootID) > 1 && o.RootID[0] == '0') || strings.Trim(o.RootID, "0123456789") != "")) ||
		!validFolderSettings(o.RootPath, o.MaxResponseBytes) || o.MaxResolvePages < 0 {
		return cliapp.InvalidConfig("Use a valid folder ID or path and response settings.")
	}
	return nil
}

func hasEmptyExportFormat(formats map[string]string) bool {
	for source, target := range formats {
		if source == "" || target == "" {
			return true
		}
	}
	return false
}

type feishuOptions struct {
	RootID           string            `json:"root_id"`
	ExportFormats    map[string]string `json:"export_formats,omitempty"`
	MaxResponseBytes int64             `json:"max_response_bytes,omitempty"`
	MaxResolvePages  int               `json:"max_resolve_pages,omitempty"`
}

func guideFeishu(o *feishuOptions, ask cliapp.Prompt) error {
	return cliapp.AskFields(ask, cliapp.Field("Feishu folder ID (folder token)", &o.RootID))
}

func prepareFeishu(o feishuOptions, _ string) (cliapp.MountSource, error) {
	if !core.ValidName(o.RootID) || !cliapp.ValidResponseBudget(o.MaxResponseBytes) || o.MaxResolvePages < 0 {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Feishu needs a folder token and valid response settings.")
	}
	if hasEmptyExportFormat(o.ExportFormats) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Feishu export settings need both a document type and an output format.")
	}
	return cliapp.BearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return cliapp.NativeDriver(feishudrive.Open(ctx, feishudrive.Options{
			RootToken: o.RootID, Client: api, ExportFormats: o.ExportFormats,
			MaxResponseBytes: o.MaxResponseBytes, MaxResolvePages: o.MaxResolvePages,
		}))
	}, "open.feishu.cn"), nil
}

func prepareFangcloud(o accountFolderOptions, _ string) (cliapp.MountSource, error) {
	if err := o.check(); err != nil {
		return cliapp.MountSource{}, err
	}
	var id int64
	if o.RootID != "" {
		var err error
		id, err = strconv.ParseInt(o.RootID, 10, 64)
		if err != nil || id <= 0 {
			return cliapp.MountSource{}, cliapp.InvalidConfig("Fangcloud needs a positive numeric folder ID.")
		}
	}
	return cliapp.BearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return cliapp.NativeDriver(fangcloud.Open(ctx, fangcloud.Options{
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

func guideGuangya(o *guangyaOptions, ask cliapp.Prompt) error {
	if err := cliapp.AskFields(ask, cliapp.Field("Application client ID", &o.ClientID)); err != nil {
		return err
	}
	return askClearable(ask, "Folder ID (empty for account root; - resets)", &o.RootID)
}

func prepareGuangya(o guangyaOptions, _ string) (cliapp.MountSource, error) {
	if o.ClientID == "" || strings.ContainsAny(o.ClientID, "\r\n") ||
		!cliapp.ValidResponseBudget(o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Guangya Pan needs an application client ID and a valid response budget.")
	}
	source := cliapp.BearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return cliapp.NativeDriver(guangyapan.Open(ctx, guangyapan.Options{
			ClientID: o.ClientID, RootID: o.RootID, Client: api, MaxResponseBytes: o.MaxResponseBytes,
		}))
	}, "openapi.guangyapan.com")
	source.Audience.Account = o.ClientID
	return source, nil
}

type kdocsOptions struct {
	RootPath         string `json:"root_path,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideKDocs(o *kdocsOptions, ask cliapp.Prompt) error {
	return cliapp.AskFields(ask, cliapp.Field("Folder path (/ for My Documents)", &o.RootPath))
}

func prepareKDocs(o kdocsOptions, _ string) (cliapp.MountSource, error) {
	if !validFolderSettings(o.RootPath, o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("KDocs needs a valid folder path and response budget.")
	}
	audience := cliapp.CredentialAudience{Origin: "kdocs https://developer.kdocs.cn"}
	return cliapp.SecretSource(audience, "KDocs user access token", func(ctx context.Context, token string) (core.Driver, error) {
		return cliapp.NativeDriver(kdocs.Open(ctx, kdocs.Options{
			RootPath: o.RootPath, MaxResponseBytes: o.MaxResponseBytes,
			AccessToken: func(context.Context) (string, error) { return token, nil },
		}))
	}), nil
}

type boxOptions struct {
	RootID           string `json:"root_id,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideBox(o *boxOptions, ask cliapp.Prompt) error {
	return cliapp.AskFields(ask, cliapp.Field("Box folder ID (0 for account root)", &o.RootID))
}

func prepareBox(o boxOptions, _ string) (cliapp.MountSource, error) {
	if (o.RootID != "" && !core.ValidName(o.RootID)) ||
		!cliapp.ValidResponseBudget(o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Box needs a valid folder ID and response budget.")
	}
	return cliapp.BearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return cliapp.NativeDriver(box.Open(ctx, box.Options{
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

func guideTencentDocs(o *tencentDocsOptions, ask cliapp.Prompt) error {
	return cliapp.AskFields(ask, cliapp.Field("Tencent Docs folder ID", &o.RootID),
		cliapp.Field("Application client ID", &o.ClientID), cliapp.Field("User Open ID", &o.OpenID))
}

func prepareTencentDocs(o tencentDocsOptions, _ string) (cliapp.MountSource, error) {
	if !core.ValidName(o.RootID) || !cliapp.ValidResponseBudget(o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Tencent Docs needs a folder ID and valid response budget.")
	}
	for _, id := range []string{o.ClientID, o.OpenID} {
		if id == "" || strings.TrimSpace(id) != id || strings.ContainsFunc(id, func(r rune) bool { return r < ' ' || r == 127 }) {
			return cliapp.MountSource{}, cliapp.InvalidConfig("Tencent Docs needs a valid application client ID and user Open ID.")
		}
	}
	const host = "docs.qq.com"
	audience := cliapp.CredentialAudience{Origin: "Access-Token https://" + host, Account: o.ClientID + "\n" + o.OpenID}
	return cliapp.SecretSource(audience, "Tencent Docs user access token", func(ctx context.Context, token string) (core.Driver, error) {
		api := &http.Client{Transport: cliapp.TokenHeaderTransport{
			Base: http.DefaultTransport, Scheme: "https", Hosts: []string{host},
			Headers: map[string]string{"Access-Token": token, "Client-Id": o.ClientID, "Open-Id": o.OpenID},
		}}
		return cliapp.NativeDriver(tencentdocs.Open(ctx, tencentdocs.Options{
			FolderID: o.RootID, Client: api, AllowExport: o.AllowExport, MaxResponseBytes: o.MaxResponseBytes,
		}))
	}), nil
}

type baiduOptions struct {
	RootPath         string `json:"root_path"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideBaidu(o *baiduOptions, ask cliapp.Prompt) error {
	return cliapp.AskFields(ask, cliapp.Field("Authorized folder path (for example /apps/My App/Archive)", &o.RootPath))
}

func prepareBaidu(o baiduOptions, _ string) (cliapp.MountSource, error) {
	if core.ValidatePath(o.RootPath) != nil || !cliapp.ValidResponseBudget(o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Baidu Netdisk needs an explicit absolute folder path and a valid response budget.")
	}
	return cliapp.SecretSource(cliapp.CredentialAudience{Origin: "https://pan.baidu.com"}, "Access token", func(ctx context.Context, token string) (core.Driver, error) {
		return cliapp.NativeDriver(baidupan.Open(ctx, baidupan.Options{
			RootPath: o.RootPath, MaxResponseBytes: o.MaxResponseBytes, Client: storageHTTPClient,
			AccessToken: func(context.Context) (string, error) { return token, nil },
		}))
	}), nil
}

func guideCloudreve(o *cloudreve.Options, ask cliapp.Prompt) error {
	if o.RootURI == "" {
		o.RootURI = "cloudreve://my/"
	}
	return cliapp.AskFields(ask, cliapp.Field("Cloudreve server URL", &o.Endpoint), cliapp.Field("Cloudreve folder URI", &o.RootURI))
}

func prepareCloudreve(o cloudreve.Options, _ string) (cliapp.MountSource, error) {
	if err := o.Validate(); err != nil {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Cloudreve needs a valid API origin, folder URI and resource settings.")
	}
	origin, err := sourceURL(o.Endpoint)
	if err != nil {
		return cliapp.MountSource{}, err
	}
	return cliapp.BearerSource(origin.Scheme, func(ctx context.Context, api *http.Client) (core.Driver, error) {
		options := o
		options.APIClient, options.ContentClient = api, storageHTTPClient
		return cliapp.NativeDriver(cloudreve.Open(ctx, options))
	}, origin.Host), nil
}

type seafileOptions struct {
	ServerURL        string `json:"server_url"`
	LibraryID        string `json:"library_id"`
	RootPath         string `json:"root_path,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideSeafile(o *seafileOptions, ask cliapp.Prompt) error {
	return cliapp.AskFields(ask, cliapp.Field("Seafile server URL", &o.ServerURL), cliapp.Field("Library ID", &o.LibraryID),
		cliapp.Field("Folder path (/ for library root)", &o.RootPath))
}

func prepareSeafile(o seafileOptions, _ string) (cliapp.MountSource, error) {
	u, err := sourceURL(o.ServerURL)
	if err != nil {
		return cliapp.MountSource{}, err
	}
	base := strings.TrimSuffix(u.Path, "/")
	if (base != "" && core.ValidatePath(base) != nil) || !core.ValidName(o.LibraryID) ||
		!validFolderSettings(o.RootPath, o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Seafile needs a valid library, folder path and response budget.")
	}
	audience := cliapp.CredentialAudience{Origin: cliapp.StorageOrigin("seafile", u) + base}
	return cliapp.SecretSource(audience, "Seafile API token", func(ctx context.Context, token string) (core.Driver, error) {
		return cliapp.NativeDriver(seafile.Open(ctx, seafile.Options{
			ServerURL: o.ServerURL, LibraryID: o.LibraryID, RootPath: o.RootPath,
			Token: token, MaxResponseBytes: o.MaxResponseBytes, Client: storageHTTPClient,
		}))
	}), nil
}

func prepare115(opts accountFolderOptions, _ string) (cliapp.MountSource, error) {
	if err := opts.check(); err != nil {
		return cliapp.MountSource{}, err
	}
	native := pan115.Options{
		RootID: opts.RootID, RootPath: opts.RootPath, MaxResponseBytes: opts.MaxResponseBytes, MaxResolvePages: opts.MaxResolvePages,
	}
	src := cliapp.BearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		o := native
		o.Client = api
		return cliapp.NativeDriver(pan115.Open(ctx, o))
	}, pan115.ControlHost)
	src.OpenManaged = func(ctx context.Context, id string) (core.Driver, error) {
		return openManaged115(ctx, native, id)
	}
	return src, nil
}

type pan123Options struct {
	RootPath         string `json:"root_path,omitempty"`
	RootID           string `json:"root_id,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
	MaxResolvePages  int    `json:"max_resolve_pages,omitempty"`
	AllowWrite       bool   `json:"allow_write,omitempty"`
	MaxUploadBytes   int64  `json:"max_upload_bytes,omitempty"`
}

func guide123(o *pan123Options, ask cliapp.Prompt) error {
	if err := askFolder(ask, &o.RootID, &o.RootPath, "Folder path (/...) or ID (empty for account root; - resets)"); err != nil {
		return err
	}
	return askWriteAccess(ask, &o.AllowWrite)
}

func askWriteAccess(ask cliapp.Prompt, writable *bool) error {
	current := "read"
	if *writable {
		current = "read_write"
	}
	access, err := cliapp.AskMenu(ask, "Access", current, "read", "read_write")
	if err == nil {
		*writable = access == "read_write"
	}
	return err
}

const pan123UploadHost = "openapi-upload.123pan.com"

func prepare123(opts pan123Options, _ string) (cliapp.MountSource, error) {
	folder := accountFolderOptions{RootPath: opts.RootPath, RootID: opts.RootID,
		MaxResponseBytes: opts.MaxResponseBytes, MaxResolvePages: opts.MaxResolvePages}
	if err := folder.check(); err != nil {
		return cliapp.MountSource{}, err
	}
	if opts.MaxUploadBytes < 0 || opts.MaxUploadBytes > 1_000_000_000 {
		return cliapp.MountSource{}, cliapp.InvalidConfig("123 Open max_upload_bytes must fit the single-request limit of 1 GB.")
	}
	src := pan123Authorization.BearerSource(func(ctx context.Context, api *http.Client) (core.Driver, error) {
		native := pan123.Options{
			RootID: opts.RootID, RootPath: opts.RootPath, MaxResponseBytes: opts.MaxResponseBytes, MaxResolvePages: opts.MaxResolvePages,
			Client: api, AllowMkdir: opts.AllowWrite,
		}
		if opts.AllowWrite && api != nil {
			upload, ok := api.Transport.(cliapp.TokenHeaderTransport)
			if !ok {
				return nil, core.Fail(core.InvalidRequest, nil)
			}
			upload.Hosts = []string{pan123UploadHost, pan123UploadHost + ":443"}
			native.UploadClient = &http.Client{Transport: upload}
			native.MaxUploadBytes = cmp.Or(opts.MaxUploadBytes, 64<<20)
		}
		return cliapp.NativeDriver(pan123.Open(ctx, native))
	}, nil, pan123.ControlHost)
	location := cliapp.MountLocation{Location: "Account root", Access: "Browse and download within this folder"}
	if opts.AllowWrite {
		location.Access = "Browse, download, create files and create directories"
	}
	switch {
	case opts.RootPath != "" && opts.RootPath != "/":
		location.Location, location.Quote = opts.RootPath, true
	case opts.RootID != "" && opts.RootID != "0":
		location.Location = "Folder ID " + opts.RootID
	}
	src.Location = location
	return src, nil
}

func guideGraphDrive(o *graphdrive.Options, ask cliapp.Prompt) error {
	if err := cliapp.AskFields(ask, cliapp.Field("Drive ID", &o.DriveID)); err != nil {
		return err
	}
	if err := askFolder(ask, &o.RootID, &o.RootPath, "Folder path (/...) or ID (empty for drive root; - resets)"); err != nil {
		return err
	}
	if o.RootID == "" && o.RootPath == "" {
		o.RootPath = "/"
	}
	return nil
}

func prepareGraphDrive(o graphdrive.Options, _ string) (cliapp.MountSource, error) {
	o.Endpoint = cmp.Or(o.Endpoint, graphdrive.Endpoint)
	if err := o.Validate(); err != nil {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Graph Drive needs a drive ID, one folder path or ID, and valid settings.")
	}
	origin, err := sourceURL(o.Endpoint)
	if err != nil {
		return cliapp.MountSource{}, err
	}
	open := func(ctx context.Context, api *http.Client) (core.Driver, error) {
		options := o
		options.APIClient = api
		options.ContentClient = storageHTTPClient
		return cliapp.NativeDriver(graphdrive.Open(ctx, options))
	}
	if strings.EqualFold("https://"+origin.Host, graphdrive.Endpoint) {
		return graphAuthorization.BearerSource(open, graphdrive.AuthorizationScopes(), origin.Host), nil
	}
	return cliapp.BearerSource(origin.Scheme, open, origin.Host), nil
}

type gokuaiOptions struct {
	LibraryID        string `json:"library_id"`
	RootPath         string `json:"root_path,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideGokuai(o *gokuaiOptions, ask cliapp.Prompt) error {
	return cliapp.AskFields(ask, cliapp.Field("Library ID", &o.LibraryID),
		cliapp.Field("Folder path (/ for library root)", &o.RootPath))
}

func prepareGokuai(o gokuaiOptions, _ string) (cliapp.MountSource, error) {
	if o.LibraryID == "" || strings.ContainsAny(o.LibraryID, "\r\n") ||
		!validFolderSettings(o.RootPath, o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Gokuai needs a valid library, folder path and response budget.")
	}
	audience := cliapp.CredentialAudience{Origin: "gokuai https://yk3.gokuai.com", Account: o.LibraryID}
	return cliapp.SecretSource(audience, "Library secret", func(ctx context.Context, secret string) (core.Driver, error) {
		return cliapp.NativeDriver(gokuai.Open(ctx, gokuai.Options{
			LibraryID: o.LibraryID, LibrarySecret: secret, RootPath: o.RootPath,
			MaxResponseBytes: o.MaxResponseBytes,
		}))
	}), nil
}

func askSpaceFolder(ask cliapp.Prompt, space, root *string) error {
	return cliapp.AskFields(ask, cliapp.Field("Space ID", space), cliapp.Field("Folder ID", root))
}

type dingTalkDriveOptions struct {
	SpaceID          string `json:"space_id"`
	RootID           string `json:"root_id"`
	OperatorUnionID  string `json:"operator_union_id"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideDingTalkDrive(o *dingTalkDriveOptions, ask cliapp.Prompt) error {
	if err := askSpaceFolder(ask, &o.SpaceID, &o.RootID); err != nil {
		return err
	}
	return cliapp.AskFields(ask, cliapp.Field("Operator union ID", &o.OperatorUnionID))
}

func prepareDingTalkDrive(o dingTalkDriveOptions, _ string) (cliapp.MountSource, error) {
	if !core.ValidName(o.SpaceID) || !core.ValidName(o.RootID) || !core.ValidName(o.OperatorUnionID) ||
		!cliapp.ValidResponseBudget(o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("DingTalk needs a space ID, folder ID, operator union ID and valid response budget.")
	}
	return cliapp.HeaderTokenSource("https", "X-Acs-Dingtalk-Access-Token", "",
		func(ctx context.Context, api *http.Client) (core.Driver, error) {
			return cliapp.NativeDriver(dingtalkdrive.Open(ctx, dingtalkdrive.Options{
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

func guideWeComDrive(o *weComDriveOptions, ask cliapp.Prompt) error {
	return askSpaceFolder(ask, &o.SpaceID, &o.RootID)
}

func prepareWeComDrive(o weComDriveOptions, _ string) (cliapp.MountSource, error) {
	if o.SpaceID == "" || o.RootID == "" || o.SpaceID == o.RootID ||
		!cliapp.ValidResponseBudget(o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("WeCom needs a space ID, a distinct folder ID and a valid response budget.")
	}
	audience := cliapp.CredentialAudience{Origin: "wecomdrive https://qyapi.weixin.qq.com"}
	return cliapp.SecretSource(audience, "WeCom app access token", func(ctx context.Context, token string) (core.Driver, error) {
		return cliapp.NativeDriver(wecomdrive.Open(ctx, wecomdrive.Options{
			SpaceID: o.SpaceID, FolderID: o.RootID, MaxResponseBytes: o.MaxResponseBytes,
			AccessToken: func(context.Context) (string, error) { return token, nil },
		}))
	}), nil
}

type tencentSMHOptions struct {
	LibraryID        string `json:"library_id"`
	SpaceID          string `json:"space_id,omitempty"`
	RootPath         string `json:"root_path,omitempty"`
	Endpoint         string `json:"endpoint,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideTencentSMH(o *tencentSMHOptions, ask cliapp.Prompt) error {
	return cliapp.AskFields(ask, cliapp.Field("Library ID", &o.LibraryID), cliapp.Field("Space ID (empty for a single-tenant library)", &o.SpaceID),
		cliapp.Field("Folder path (/ for space root)", &o.RootPath))
}

func prepareTencentSMH(o tencentSMHOptions, _ string) (cliapp.MountSource, error) {
	o.Endpoint = cmp.Or(o.Endpoint, "https://smh.tencentcs.com")
	o.SpaceID = cmp.Or(o.SpaceID, "-")
	origin, err := sourceURL(o.Endpoint)
	if err != nil {
		return cliapp.MountSource{}, err
	}
	if origin.Scheme != "https" || (origin.Path != "" && origin.Path != "/") ||
		!core.ValidName(o.LibraryID) || !core.ValidName(o.SpaceID) ||
		!validFolderSettings(o.RootPath, o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Tencent SMH needs an HTTPS origin, a valid library and space, and valid folder and response settings.")
	}
	audience := cliapp.CredentialAudience{Origin: cliapp.StorageOrigin("tencentsmh", origin), Account: o.LibraryID + "/" + o.SpaceID}
	return cliapp.SecretSource(audience, "SMH business access token", func(ctx context.Context, token string) (core.Driver, error) {
		return cliapp.NativeDriver(tencentsmh.Open(ctx, tencentsmh.Options{
			LibraryID: o.LibraryID, SpaceID: o.SpaceID, RootPath: o.RootPath, Endpoint: o.Endpoint,
			MaxResponseBytes: o.MaxResponseBytes, AccessToken: func(context.Context) (string, error) { return token, nil },
		}))
	}), nil
}

type googleDriveOptions struct {
	RootID           string            `json:"root_id,omitempty"`
	RootPath         string            `json:"root_path,omitempty"`
	ExportFormats    map[string]string `json:"export_formats,omitempty"`
	MaxResponseBytes int64             `json:"max_response_bytes,omitempty"`
	MaxResolvePages  int               `json:"max_resolve_pages,omitempty"`
}

func guideGoogleDrive(o *googleDriveOptions, ask cliapp.Prompt) error {
	return askFolder(ask, &o.RootID, &o.RootPath, "Folder path (/...) or ID (empty for authorized root; - resets)")
}

func prepareGoogleDrive(o googleDriveOptions, _ string) (cliapp.MountSource, error) {
	if (o.RootID != "" && (o.RootPath != "" || strings.ContainsAny(o.RootID, "\r\n"))) ||
		!validFolderSettings(o.RootPath, o.MaxResponseBytes) || o.MaxResolvePages < 0 {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Google Drive needs a valid folder path or ID and response settings.")
	}
	if hasEmptyExportFormat(o.ExportFormats) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Google Drive export formats need both source and target MIME types.")
	}
	return googleDriveAuthorization.BearerSource(func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return cliapp.NativeDriver(googledrive.Open(ctx, googledrive.Options{
			RootID: o.RootID, RootPath: o.RootPath, ExportFormats: o.ExportFormats,
			MaxResponseBytes: o.MaxResponseBytes, MaxResolvePages: o.MaxResolvePages, Client: api, ContentClient: storageHTTPClient,
		}))
	}, []string{googledrive.ReadOnlyScope}, googledrive.ControlHost), nil
}

type dropboxOptions struct {
	Root             string  `json:"root,omitempty"`
	NamespaceID      string  `json:"namespace_id,omitempty"`
	RootPath         string  `json:"root_path,omitempty"`
	ExportFormat     *string `json:"export_format,omitempty"`
	AllowCreate      bool    `json:"allow_create,omitempty"`
	MaxResponseBytes int64   `json:"max_response_bytes,omitempty"`
}

func guideDropbox(o *dropboxOptions, ask cliapp.Prompt) error {
	o.Root = cmp.Or(o.Root, "default")
	if err := cliapp.AskFields(ask, cliapp.Field("Dropbox root (default, account, or namespace)", &o.Root)); err != nil {
		return err
	}
	if o.Root != "namespace" {
		o.NamespaceID = ""
	} else if err := cliapp.AskFields(ask, cliapp.Field("Namespace ID", &o.NamespaceID)); err != nil {
		return err
	}
	if err := cliapp.AskFields(ask, cliapp.Field("Folder path (/ for selected root)", &o.RootPath)); err != nil {
		return err
	}
	return askWriteAccess(ask, &o.AllowCreate)
}

func (o dropboxOptions) native() (dropbox.Options, error) {
	options := dropbox.Options{RootPath: o.RootPath, AllowCreate: o.AllowCreate, MaxResponseBytes: o.MaxResponseBytes}
	switch o.Root {
	case "", "default":
	case "account":
		options.Root = dropbox.AccountRoot()
	case "namespace":
		options.Root = dropbox.NamespaceRoot(o.NamespaceID)
	default:
		return dropbox.Options{}, cliapp.InvalidConfig("Select default, account or namespace for the Dropbox root.")
	}
	if o.ExportFormat != nil {
		options.Export = &dropbox.ExportOptions{Format: *o.ExportFormat}
	}
	if o.Root != "namespace" && o.NamespaceID != "" || options.Validate() != nil {
		return dropbox.Options{}, cliapp.InvalidConfig("Dropbox needs a valid root, namespace ID, folder path and response budget.")
	}
	return options, nil
}

func prepareDropbox(o dropboxOptions, _ string) (cliapp.MountSource, error) {
	options, err := o.native()
	if err != nil {
		return cliapp.MountSource{}, err
	}
	open := func(ctx context.Context, api *http.Client) (core.Driver, error) {
		opts := options
		opts.Client = api
		return cliapp.NativeDriver(dropbox.Open(ctx, opts))
	}
	return dropboxAuthorization.BearerSource(open, options.AuthorizationScopes(), dropbox.ControlHost, dropbox.ContentHost), nil
}

type authorizedRootOptions struct {
	MaxResponseBytes int64 `json:"max_response_bytes,omitempty"`
}

func authorizedRootSource(maxResponseBytes int64, origin, credential string, open func(context.Context, string) (core.Driver, error)) (cliapp.MountSource, error) {
	if !cliapp.ValidResponseBudget(maxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("The source needs a valid response budget.")
	}
	return cliapp.SecretSource(cliapp.CredentialAudience{Origin: origin}, credential, open), nil
}

type weiyunOptions struct {
	RootPath         string `json:"root_path,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideWeiyun(o *weiyunOptions, ask cliapp.Prompt) error {
	root := cmp.Or(o.RootPath, "/")
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

func prepareWeiyun(o weiyunOptions, _ string) (cliapp.MountSource, error) {
	if o.RootPath != "" && core.ValidatePath(o.RootPath) != nil {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Enter an absolute folder path inside Weiyun, such as /Documents.")
	}
	src, err := authorizedRootSource(o.MaxResponseBytes, weiyun.Origin,
		"Weiyun MCP token", func(ctx context.Context, token string) (core.Driver, error) {
			return cliapp.NativeDriver(weiyun.Open(ctx, weiyun.Options{
				Token: token, RootPath: o.RootPath, MaxResponseBytes: o.MaxResponseBytes,
			}))
		})
	if err != nil {
		return cliapp.MountSource{}, err
	}
	src.ProbeOnChange = true
	src.Preview = fmt.Sprintf("  Access  Browse and download\n  Folder  %q within the Weiyun authorization\n", cmp.Or(o.RootPath, "/"))
	src.OpenManaged = func(ctx context.Context, id string) (core.Driver, error) {
		grant, err := weiyunAuthorization.LoadGrant(id)
		if err != nil {
			return nil, err
		}
		return src.Open(ctx, grant.Access)
	}
	return src, nil
}

func prepareYunpan360(o authorizedRootOptions, _ string) (cliapp.MountSource, error) {
	return authorizedRootSource(o.MaxResponseBytes, "https://mcp.yunpan.com", "360 AI Cloud Disk API key", func(ctx context.Context, key string) (core.Driver, error) {
		return cliapp.NativeDriver(yunpan360.Open(ctx, yunpan360.Options{APIKey: key, MaxResponseBytes: o.MaxResponseBytes}))
	})
}

func guideAliPan(opts *alipan.Options, ask cliapp.Prompt) error {
	if err := cliapp.AskFields(ask, cliapp.Field("Drive ID", &opts.DriveID)); err != nil {
		return err
	}
	return guideAliPanRoot(opts, ask)
}

func guideAliPanRoot(opts *alipan.Options, ask cliapp.Prompt) error {
	root, err := ask("Directory path or ID", cmp.Or(opts.RootPath, opts.RootID, "/"))
	if err != nil {
		return err
	}
	opts.RootID, opts.RootPath = folderLocation(root)
	return nil
}

func prepareAliPan(opts alipan.Options, _ string) (cliapp.MountSource, error) {
	if opts.Endpoint != "" && opts.Endpoint != alipan.Endpoint {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Managed AliPan credentials require the official API origin.")
	}
	if err := opts.Validate(); err != nil {
		return cliapp.MountSource{}, cliapp.InvalidConfig("AliPan requires a drive ID and one valid directory path or ID.")
	}
	return cliapp.MountSource{
		Audience: cliapp.CredentialAudience{Origin: "alipan " + alipan.Endpoint},
		OpenManaged: func(ctx context.Context, id string) (core.Driver, error) {
			return openManagedAliPan(ctx, opts, id)
		},
		Verify: verifyAliPanRoot,
	}, nil
}
