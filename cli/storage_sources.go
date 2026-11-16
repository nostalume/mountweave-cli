package cli

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/azureblob"
	"github.com/nostalume/mountweave/drivers/bunny"
	"github.com/nostalume/mountweave/drivers/upyun"
)

// Native Drivers own any redirect and credential handoff.
var storageHTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}}

func sourceURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") ||
		strings.Contains(strings.ToLower(u.EscapedPath()), "%2f") {
		return nil, invalidConfig("Enter an HTTP or HTTPS URL without credentials, a query or a fragment.")
	}
	return u, nil
}

func storageURL(raw string) (*url.URL, error) {
	u, err := sourceURL(raw)
	if err != nil {
		return nil, err
	}
	path := strings.TrimSuffix(u.Path, "/")
	if path == "/" || core.ValidatePath(path) != nil {
		return nil, invalidConfig("The storage URL must include the container, zone or bucket.")
	}
	return u, nil
}

func storageOrigin(service string, u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return service + " " + u.Scheme + " " + strings.ToLower(u.Hostname()) + " " + port
}

type azureOptions struct {
	ContainerURL     string `json:"container_url"`
	Prefix           string `json:"prefix,omitempty"`
	PartSize         int64  `json:"part_size,omitempty"`
	MaxBufferedBytes int64  `json:"max_buffered_bytes,omitempty"`
	PartConcurrency  int    `json:"part_concurrency,omitempty"`
}

func guideAzure(o *azureOptions, ask askField) error {
	err := askFields(ask,
		setupField{"Container URL (without SAS)", &o.ContainerURL},
		setupField{"Blob prefix (empty for container; - clears)", &o.Prefix},
	)
	if o.Prefix == "-" {
		o.Prefix = ""
	}
	return err
}

func prepareAzure(o azureOptions, _ string) (mountSource, error) {
	u, err := storageURL(o.ContainerURL)
	if err != nil {
		return mountSource{}, err
	}
	if utf8.RuneCountInString(o.Prefix) > 1024 || (o.Prefix != "" && !strings.HasSuffix(o.Prefix, "/")) {
		return mountSource{}, invalidConfig("Azure Blob requires an empty or slash-terminated prefix.")
	}
	if o.PartSize < 0 || o.PartSize > min(int64(math.MaxInt), blockblob.MaxStageBlockBytes) ||
		o.MaxBufferedBytes < o.PartSize || (o.PartSize == 0) != (o.MaxBufferedBytes == 0) ||
		o.PartConcurrency < 0 || (o.PartSize == 0 && o.PartConcurrency != 0) {
		return mountSource{}, invalidConfig("Azure writes require a valid part size and a buffer budget for at least one part.")
	}
	native := azureblob.Options{Prefix: o.Prefix, PartSize: o.PartSize, MaxBufferedBytes: o.MaxBufferedBytes, PartConcurrency: o.PartConcurrency}
	return mountSource{
		audience: credentialAudience{origin: storageOrigin("azureblob", u), account: strings.TrimSuffix(u.Path, "/")},
		verify:   verifyKeyRoot,
		secret:   "Container SAS",
		open: func(ctx context.Context, secret string) (core.Driver, error) {
			query := strings.TrimPrefix(secret, "?")
			params, err := url.ParseQuery(query)
			if err != nil || strings.ContainsAny(query, "#?") || len(params["sig"]) != 1 || params.Get("sig") == "" ||
				params.Has("comp") || params.Has("restype") || params.Has("snapshot") || params.Has("versionid") {
				return nil, &problem{Code: "credential_unavailable", Message: "The Azure credential is not a valid SAS token.", NextAction: "Use a container SAS token without the service URL."}
			}
			endpoint := *u
			endpoint.RawQuery = query
			clientOptions := &container.ClientOptions{}
			clientOptions.Transport = storageHTTPClient
			client, err := container.NewClientWithNoCredential(endpoint.String(), clientOptions)
			if err != nil {
				return nil, invalidConfig("The Azure container client could not be configured.")
			}
			options := native
			options.Client = client
			return nativeDriver(azureblob.Open(ctx, options))
		},
	}, nil
}

type bunnyOptions struct {
	RootURL          string `json:"root_url"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideBunny(o *bunnyOptions, ask askField) error {
	return askFields(ask, setupField{"Bunny Storage URL including zone and optional folder", &o.RootURL})
}

func prepareBunny(o bunnyOptions, _ string) (mountSource, error) {
	u, err := storageURL(o.RootURL)
	if err != nil {
		return mountSource{}, err
	}
	if !validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("Bunny Storage needs a valid metadata response budget.")
	}
	zone, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	return mountSource{
		audience: credentialAudience{origin: storageOrigin("bunny", u), account: zone},
		secret:   "Storage zone password",
		open: func(ctx context.Context, password string) (core.Driver, error) {
			return nativeDriver(bunny.Open(ctx, bunny.Options{RootURL: o.RootURL, AccessKey: password, MaxResponseBytes: o.MaxResponseBytes}))
		},
	}, nil
}

type upyunOptions struct {
	RootURL          string `json:"root_url"`
	Operator         string `json:"operator"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideUPYUN(o *upyunOptions, ask askField) error {
	return askFields(ask,
		setupField{"UPYUN URL including bucket and optional folder", &o.RootURL},
		setupField{"Operator", &o.Operator},
	)
}

func prepareUPYUN(o upyunOptions, _ string) (mountSource, error) {
	u, err := storageURL(o.RootURL)
	if err != nil {
		return mountSource{}, err
	}
	if o.Operator == "" || strings.ContainsAny(o.Operator, ":\x00\r\n") || !validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("UPYUN needs an operator and a valid metadata response budget.")
	}
	bucket, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	return mountSource{
		audience: credentialAudience{origin: storageOrigin("upyun", u), account: bucket + "\x00" + o.Operator},
		secret:   "Operator password",
		open: func(ctx context.Context, password string) (core.Driver, error) {
			return nativeDriver(upyun.Open(ctx, upyun.Options{RootURL: o.RootURL, Operator: o.Operator, Password: password, MaxResponseBytes: o.MaxResponseBytes}))
		},
	}, nil
}
