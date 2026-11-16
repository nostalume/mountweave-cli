package cli

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/baidupan"
	"github.com/nostalume/mountweave/drivers/graphdrive"
	"github.com/nostalume/mountweave/drivers/pan115"
	"github.com/nostalume/mountweave/drivers/pan123"
)

const (
	pan115ControlHost = "proapi.115.com"
	pan123ControlHost = "open-api.123pan.com"
)

type accountFolderOptions struct {
	RootPath         string `json:"root_path,omitempty"`
	RootID           string `json:"root_id,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
	MaxResolvePages  int    `json:"max_resolve_pages,omitempty"`
}

func askFolder(ask askField, id, path *string, label string) error {
	current := *path
	if *id != "" {
		current = *id
	}
	selected, err := ask(label, current)
	if err != nil {
		return err
	}
	*id, *path = "", ""
	switch {
	case strings.HasPrefix(selected, "/"):
		*path = selected
	case selected != "" && selected != "-":
		*id = selected
	}
	return nil
}

func guideAccountFolder(opts *accountFolderOptions, ask askField) error {
	return askFolder(ask, &opts.RootID, &opts.RootPath, "Folder path (/...) or ID (empty for account root; - resets)")
}

func prepare115(opts accountFolderOptions, _ string) (mountSource, error) {
	if err := opts.check(); err != nil {
		return mountSource{}, err
	}
	native := pan115.Options{
		RootID: opts.RootID, RootPath: opts.RootPath, MaxResponseBytes: opts.MaxResponseBytes, MaxResolvePages: opts.MaxResolvePages,
	}
	src := bearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		o := native
		o.Client = api
		return nativeDriver(pan115.Open(ctx, o))
	}, pan115ControlHost)
	src.openManaged = func(ctx context.Context, id string) (core.Driver, error) {
		return openManaged115(ctx, native, id)
	}
	return src, nil
}

func prepare123(opts accountFolderOptions, _ string) (mountSource, error) {
	if err := opts.check(); err != nil {
		return mountSource{}, err
	}
	return pan123Authorization.bearerSource(func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return nativeDriver(pan123.Open(ctx, pan123.Options{
			RootID: opts.RootID, RootPath: opts.RootPath, MaxResponseBytes: opts.MaxResponseBytes, MaxResolvePages: opts.MaxResolvePages,
			Client: api,
		}))
	}, nil, renew123, pan123ControlHost), nil
}

func (o accountFolderOptions) source(host string, open func(context.Context, *http.Client) (core.Driver, error)) (mountSource, error) {
	if err := o.check(); err != nil {
		return mountSource{}, err
	}
	return bearerSource("https", open, host), nil
}

func (o accountFolderOptions) check() error {
	if (o.RootID != "" && (o.RootPath != "" || (len(o.RootID) > 1 && o.RootID[0] == '0') || strings.Trim(o.RootID, "0123456789") != "")) ||
		(o.RootPath != "" && core.ValidatePath(o.RootPath) != nil) ||
		!validResponseBudget(o.MaxResponseBytes) || o.MaxResolvePages < 0 {
		return invalidConfig("Use a valid folder ID or path and response settings.")
	}
	return nil
}

func guideGraphDrive(o *graphdrive.Options, ask askField) error {
	if err := askFields(ask, setupField{"Drive ID", &o.DriveID}); err != nil {
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

func prepareGraphDrive(o graphdrive.Options, _ string) (mountSource, error) {
	if o.Endpoint == "" {
		o.Endpoint = graphdrive.Endpoint
	}
	if err := o.Validate(); err != nil {
		return mountSource{}, invalidConfig("Graph Drive needs a drive ID, one folder path or ID, and valid settings.")
	}
	origin, err := sourceURL(o.Endpoint)
	if err != nil {
		return mountSource{}, err
	}
	open := func(ctx context.Context, api *http.Client) (core.Driver, error) {
		options := o
		options.APIClient = api
		options.ContentClient = storageHTTPClient
		return nativeDriver(graphdrive.Open(ctx, options))
	}
	if strings.EqualFold(origin.Host, graphControlHost) {
		return graphAuthorization.bearerSource(open, graphDeviceScopes(), renewGraph, origin.Host), nil
	}
	return bearerSource(origin.Scheme, open, origin.Host), nil
}

type baiduOptions struct {
	RootPath         string `json:"root_path"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideBaidu(o *baiduOptions, ask askField) error {
	return askFields(ask, setupField{"Authorized folder path (for example /apps/My App/Archive)", &o.RootPath})
}

func prepareBaidu(o baiduOptions, _ string) (mountSource, error) {
	if core.ValidatePath(o.RootPath) != nil || !validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("Baidu Netdisk needs an explicit absolute folder path and a valid response budget.")
	}
	return mountSource{
		audience: credentialAudience{origin: "https://pan.baidu.com"},
		secret:   "Access token",
		open: func(ctx context.Context, token string) (core.Driver, error) {
			return nativeDriver(baidupan.Open(ctx, baidupan.Options{
				RootPath: o.RootPath, MaxResponseBytes: o.MaxResponseBytes, Client: storageHTTPClient,
				AccessToken: func(context.Context) (string, error) { return token, nil },
			}))
		},
	}, nil
}

func bearerSource(scheme string, open func(context.Context, *http.Client) (core.Driver, error), hosts ...string) mountSource {
	return headerTokenSource(scheme, "Authorization", "Bearer ", open, hosts...)
}

func headerTokenSource(scheme, header, prefix string, open func(context.Context, *http.Client) (core.Driver, error), hosts ...string) mountSource {
	return mountSource{
		audience: credentialAudience{origin: header + " " + scheme + "://" + strings.ToLower(strings.Join(hosts, " "))},
		secret:   "Access token",
		open: func(ctx context.Context, token string) (core.Driver, error) {
			if token == "" {
				return open(ctx, nil)
			}
			return open(ctx, &http.Client{Transport: tokenHeaderTransport{
				base: http.DefaultTransport, scheme: scheme, hosts: hosts,
				headers: map[string]string{header: prefix + token},
			}})
		},
	}
}

// The native Drivers own content requests. This transport is for their
// declared credential recipients only, including on redirected requests.
type tokenHeaderTransport struct {
	base    http.RoundTripper
	scheme  string
	hosts   []string
	headers map[string]string
	bearer  func(context.Context) (string, error)
}

func (t tokenHeaderTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	allowed := slices.ContainsFunc(t.hosts, func(host string) bool { return strings.EqualFold(host, r.URL.Host) })
	if r.URL.Scheme != t.scheme || !allowed || r.URL.User != nil || r.URL.Opaque != "" || (r.Host != "" && !strings.EqualFold(r.Host, r.URL.Host)) {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, core.Fail(core.PermissionDenied, errors.New("untrusted account API destination"))
	}
	req := r.Clone(r.Context())
	for name, value := range t.headers {
		req.Header.Set(name, value)
	}
	if t.bearer != nil {
		token, err := t.bearer(req.Context())
		if err != nil {
			if req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return t.base.RoundTrip(req)
}
