package cli

import (
	"context"
	"net/http"
	"strings"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/cloudreve"
	"github.com/nostalume/mountweave/drivers/dropbox"
	"github.com/nostalume/mountweave/drivers/googledrive"
	"github.com/nostalume/mountweave/drivers/seafile"
)

type googleDriveOptions struct {
	RootID           string            `json:"root_id,omitempty"`
	RootPath         string            `json:"root_path,omitempty"`
	ExportFormats    map[string]string `json:"export_formats,omitempty"`
	MaxResponseBytes int64             `json:"max_response_bytes,omitempty"`
	MaxResolvePages  int               `json:"max_resolve_pages,omitempty"`
}

func guideGoogleDrive(o *googleDriveOptions, ask askField) error {
	return askFolder(ask, &o.RootID, &o.RootPath, "Folder path (/...) or ID (empty for authorized root; - resets)")
}

func prepareGoogleDrive(o googleDriveOptions, _ string) (mountSource, error) {
	if (o.RootID != "" && (o.RootPath != "" || strings.ContainsAny(o.RootID, "\r\n"))) ||
		(o.RootPath != "" && core.ValidatePath(o.RootPath) != nil) ||
		!validResponseBudget(o.MaxResponseBytes) || o.MaxResolvePages < 0 {
		return mountSource{}, invalidConfig("Google Drive needs a valid folder path or ID and response settings.")
	}
	for source, target := range o.ExportFormats {
		if source == "" || target == "" {
			return mountSource{}, invalidConfig("Google Drive export formats need both source and target MIME types.")
		}
	}
	return googleDriveAuthorization.bearerSource(func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return nativeDriver(googledrive.Open(ctx, googledrive.Options{
			RootID: o.RootID, RootPath: o.RootPath, ExportFormats: o.ExportFormats,
			MaxResponseBytes: o.MaxResponseBytes, MaxResolvePages: o.MaxResolvePages, Client: api, ContentClient: storageHTTPClient,
		}))
	}, []string{googleDriveReadScope}, googleDriveAuthorization.renew, "www.googleapis.com"), nil
}

type dropboxOptions struct {
	Root             string  `json:"root,omitempty"`
	NamespaceID      string  `json:"namespace_id,omitempty"`
	RootPath         string  `json:"root_path,omitempty"`
	ExportFormat     *string `json:"export_format,omitempty"`
	AllowCreate      bool    `json:"allow_create,omitempty"`
	MaxResponseBytes int64   `json:"max_response_bytes,omitempty"`
}

func dropboxScopes(write bool) []string {
	scopes := []string{"account_info.read", "files.metadata.read", "files.content.read"}
	if write {
		scopes = append(scopes, "files.content.write")
	}
	return scopes
}

func dropboxMountScopes(data []byte) ([]string, error) {
	if len(data) == 0 {
		return dropboxScopes(false), nil
	}
	var o dropboxOptions
	if err := decodeObject(data, &o); err != nil {
		return nil, err
	}
	return dropboxScopes(o.AllowCreate), nil
}

func guideDropbox(o *dropboxOptions, ask askField) error {
	if o.Root == "" {
		o.Root = "default"
	}
	if err := askFields(ask, setupField{"Dropbox root (default, account, or namespace)", &o.Root}); err != nil {
		return err
	}
	if o.Root == "namespace" {
		if err := askFields(ask, setupField{"Namespace ID", &o.NamespaceID}); err != nil {
			return err
		}
	} else {
		o.NamespaceID = ""
	}
	return askFields(ask, setupField{"Folder path (/ for selected root)", &o.RootPath})
}

func prepareDropbox(o dropboxOptions, _ string) (mountSource, error) {
	var root dropbox.Root
	switch o.Root {
	case "", "default":
	case "account":
		root = dropbox.AccountRoot()
	case "namespace":
		if o.NamespaceID == "" || strings.ContainsFunc(o.NamespaceID, func(r rune) bool {
			return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune("-_:", r))
		}) {
			return mountSource{}, invalidConfig("Dropbox namespace root needs a valid namespace_id.")
		}
		root = dropbox.NamespaceRoot(o.NamespaceID)
	default:
		return mountSource{}, invalidConfig("Select default, account or namespace for the Dropbox root.")
	}
	if (o.Root != "namespace" && o.NamespaceID != "") ||
		(o.RootPath != "" && core.ValidatePath(o.RootPath) != nil) ||
		!validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("Dropbox needs a valid root, folder path and response budget.")
	}
	var export *dropbox.ExportOptions
	if o.ExportFormat != nil {
		export = &dropbox.ExportOptions{Format: *o.ExportFormat}
	}
	open := func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return nativeDriver(dropbox.Open(ctx, dropbox.Options{
			Root: root, RootPath: o.RootPath, Export: export, AllowCreate: o.AllowCreate,
			MaxResponseBytes: o.MaxResponseBytes, Client: api,
		}))
	}
	return dropboxAuthorization.bearerSource(open, dropboxScopes(o.AllowCreate), dropboxAuthorization.renew, "api.dropboxapi.com", "content.dropboxapi.com"), nil
}

func guideCloudreve(o *cloudreve.Options, ask askField) error {
	if o.RootURI == "" {
		o.RootURI = "cloudreve://my/"
	}
	return askFields(ask, setupField{"Cloudreve server URL", &o.Endpoint}, setupField{"Cloudreve folder URI", &o.RootURI})
}

func prepareCloudreve(o cloudreve.Options, _ string) (mountSource, error) {
	if err := o.Validate(); err != nil {
		return mountSource{}, invalidConfig("Cloudreve needs a valid API origin, folder URI and resource settings.")
	}
	origin, err := sourceURL(o.Endpoint)
	if err != nil {
		return mountSource{}, err
	}
	return bearerSource(origin.Scheme, func(ctx context.Context, api *http.Client) (core.Driver, error) {
		options := o
		options.APIClient, options.ContentClient = api, storageHTTPClient
		return nativeDriver(cloudreve.Open(ctx, options))
	}, origin.Host), nil
}

type seafileOptions struct {
	ServerURL        string `json:"server_url"`
	LibraryID        string `json:"library_id"`
	RootPath         string `json:"root_path,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideSeafile(o *seafileOptions, ask askField) error {
	return askFields(ask, setupField{"Seafile server URL", &o.ServerURL}, setupField{"Library ID", &o.LibraryID},
		setupField{"Folder path (/ for library root)", &o.RootPath})
}

func prepareSeafile(o seafileOptions, _ string) (mountSource, error) {
	u, err := sourceURL(o.ServerURL)
	if err != nil {
		return mountSource{}, err
	}
	base := strings.TrimSuffix(u.Path, "/")
	if (base != "" && core.ValidatePath(base) != nil) || !core.ValidName(o.LibraryID) ||
		(o.RootPath != "" && core.ValidatePath(o.RootPath) != nil) ||
		!validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("Seafile needs a valid library, folder path and response budget.")
	}
	return mountSource{
		audience: credentialAudience{origin: storageOrigin("seafile", u) + base},
		secret:   "Seafile API token",
		open: func(ctx context.Context, token string) (core.Driver, error) {
			return nativeDriver(seafile.Open(ctx, seafile.Options{
				ServerURL: o.ServerURL, LibraryID: o.LibraryID, RootPath: o.RootPath,
				Token: token, MaxResponseBytes: o.MaxResponseBytes, Client: storageHTTPClient,
			}))
		},
	}, nil
}
