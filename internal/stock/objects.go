package stock

import (
	"context"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/logging"
	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave/drivers/azureblob"
	"github.com/nostalume/mountweave/drivers/bunny"
	s3driver "github.com/nostalume/mountweave/drivers/s3"
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
		return nil, cliapp.InvalidConfig("Enter an HTTP or HTTPS URL without credentials, a query or a fragment.")
	}
	return u, nil
}

func storageURL(raw string) (*url.URL, string, error) {
	u, err := sourceURL(raw)
	if err != nil {
		return nil, "", err
	}
	root := strings.TrimSuffix(u.Path, "/")
	if root == "/" || core.ValidatePath(root) != nil {
		return nil, "", cliapp.InvalidConfig("The storage URL must include the container, zone or bucket.")
	}
	return u, root, nil
}

type azureOptions struct {
	ContainerURL     string `json:"container_url"`
	Prefix           string `json:"prefix,omitempty"`
	PartSize         int64  `json:"part_size,omitempty"`
	MaxBufferedBytes int64  `json:"max_buffered_bytes,omitempty"`
	PartConcurrency  int    `json:"part_concurrency,omitempty"`
}

func guideAzure(o *azureOptions, ask cliapp.Prompt) error {
	if err := cliapp.AskFields(ask, cliapp.Field("Container URL (without SAS)", &o.ContainerURL)); err != nil {
		return err
	}
	return askClearable(ask, "Blob prefix (empty for container; - clears)", &o.Prefix)
}

func prepareAzure(o azureOptions, _ string) (cliapp.MountSource, error) {
	u, containerPath, err := storageURL(o.ContainerURL)
	if err != nil {
		return cliapp.MountSource{}, err
	}
	if utf8.RuneCountInString(o.Prefix) > 1024 || (o.Prefix != "" && !strings.HasSuffix(o.Prefix, "/")) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Azure Blob requires an empty or slash-terminated prefix.")
	}
	if o.PartSize < 0 || o.PartSize > min(int64(math.MaxInt), blockblob.MaxStageBlockBytes) ||
		o.MaxBufferedBytes < o.PartSize || (o.PartSize == 0) != (o.MaxBufferedBytes == 0) ||
		o.PartConcurrency < 0 || (o.PartSize == 0 && o.PartConcurrency != 0) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Azure writes require a valid part size and a buffer budget for at least one part.")
	}
	native := azureblob.Options{Prefix: o.Prefix, PartSize: o.PartSize, MaxBufferedBytes: o.MaxBufferedBytes, PartConcurrency: o.PartConcurrency}
	return cliapp.MountSource{
		Audience: cliapp.CredentialAudience{Origin: cliapp.StorageOrigin("azureblob", u), Account: containerPath},
		Verify:   verifyKeyRoot,
		Secret:   "Container SAS",
		Open: func(ctx context.Context, secret string) (core.Driver, error) {
			query := strings.TrimPrefix(secret, "?")
			params, err := url.ParseQuery(query)
			if err != nil || strings.ContainsAny(query, "#?") || len(params["sig"]) != 1 || params.Get("sig") == "" ||
				params.Has("comp") || params.Has("restype") || params.Has("snapshot") || params.Has("versionid") {
				return nil, &cliapp.Problem{Code: "credential_unavailable", Message: "The Azure credential is not a valid SAS token.", NextAction: "Use a container SAS token without the service URL."}
			}
			endpoint := *u
			endpoint.RawQuery = query
			clientOptions := &container.ClientOptions{}
			clientOptions.Transport = storageHTTPClient
			client, err := container.NewClientWithNoCredential(endpoint.String(), clientOptions)
			if err != nil {
				return nil, cliapp.InvalidConfig("The Azure container client could not be configured.")
			}
			options := native
			options.Client = client
			return cliapp.NativeDriver(azureblob.Open(ctx, options))
		},
	}, nil
}

type bunnyOptions struct {
	RootURL          string `json:"root_url"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideBunny(o *bunnyOptions, ask cliapp.Prompt) error {
	return cliapp.AskFields(ask, cliapp.Field("Bunny Storage URL including zone and optional folder", &o.RootURL))
}

func prepareBunny(o bunnyOptions, _ string) (cliapp.MountSource, error) {
	u, root, err := storageURL(o.RootURL)
	if err != nil {
		return cliapp.MountSource{}, err
	}
	if !cliapp.ValidResponseBudget(o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Bunny Storage needs a valid metadata response budget.")
	}
	zone, _, _ := strings.Cut(root[1:], "/")
	audience := cliapp.CredentialAudience{Origin: cliapp.StorageOrigin("bunny", u), Account: zone}
	return cliapp.SecretSource(audience, "Storage zone password", func(ctx context.Context, password string) (core.Driver, error) {
		return cliapp.NativeDriver(bunny.Open(ctx, bunny.Options{RootURL: o.RootURL, AccessKey: password, MaxResponseBytes: o.MaxResponseBytes}))
	}), nil
}

type upyunOptions struct {
	RootURL          string `json:"root_url"`
	Operator         string `json:"operator"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideUPYUN(o *upyunOptions, ask cliapp.Prompt) error {
	return cliapp.AskFields(ask,
		cliapp.Field("UPYUN URL including bucket and optional folder", &o.RootURL),
		cliapp.Field("Operator", &o.Operator),
	)
}

func prepareUPYUN(o upyunOptions, _ string) (cliapp.MountSource, error) {
	u, root, err := storageURL(o.RootURL)
	if err != nil {
		return cliapp.MountSource{}, err
	}
	if o.Operator == "" || strings.ContainsAny(o.Operator, ":\x00\r\n") || !cliapp.ValidResponseBudget(o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("UPYUN needs an operator and a valid metadata response budget.")
	}
	bucket, _, _ := strings.Cut(root[1:], "/")
	audience := cliapp.CredentialAudience{Origin: cliapp.StorageOrigin("upyun", u), Account: bucket + "\x00" + o.Operator}
	return cliapp.SecretSource(audience, "Operator password", func(ctx context.Context, password string) (core.Driver, error) {
		return cliapp.NativeDriver(upyun.Open(ctx, upyun.Options{RootURL: o.RootURL, Operator: o.Operator, Password: password, MaxResponseBytes: o.MaxResponseBytes}))
	}), nil
}

type s3Options struct {
	Bucket           string `json:"bucket"`
	Region           string `json:"region,omitempty"`
	Endpoint         string `json:"endpoint,omitempty"`
	Prefix           string `json:"prefix,omitempty"`
	PathStyle        bool   `json:"path_style,omitempty"`
	PlainListKeys    bool   `json:"plain_list_keys,omitempty"`
	ListV1           bool   `json:"list_v1,omitempty"`
	PartSize         int64  `json:"part_size,omitempty"`
	MaxBufferedBytes int64  `json:"max_buffered_bytes,omitempty"`
	PartConcurrency  int    `json:"part_concurrency,omitempty"`
}

var s3Presets = map[string]s3Options{
	"aws":  {},
	"bos":  {Endpoint: "https://s3.bj.bcebos.com", Region: "bj"},
	"cos":  {Endpoint: "https://cos.ap-guangzhou.myqcloud.com", Region: "ap-guangzhou", ListV1: true},
	"kodo": {Endpoint: "https://s3.cn-east-1.qiniucs.com", Region: "cn-east-1", PlainListKeys: true},
	"ks3":  {Endpoint: "https://ks3-cn-beijing.ksyuncs.com", Region: "BEIJING"},
	"oss":  {Endpoint: "https://s3.oss-cn-hangzhou.aliyuncs.com", Region: "cn-hangzhou", PlainListKeys: true},
	"r2":   {Region: "auto"},
	"us3":  {Endpoint: "https://s3-cn-bj.ufileos.com", Region: "cn-bj", PlainListKeys: true},
	"zos":  {Endpoint: "https://huadong-1.ctyunzos.cn", Region: "us-east-1"},
}

func guideS3(o *s3Options, ask cliapp.Prompt) error {
	var service string
	var err error
	if o.Bucket == "" {
		choices := slices.Sorted(maps.Keys(s3Presets))
		service, err = cliapp.AskChoice(ask, "S3 service ("+strings.Join(choices, ", ")+")", "aws", choices...)
		if err != nil {
			return err
		}
		*o = s3Presets[service]
	}
	o.Bucket, err = ask("Bucket", o.Bucket)
	if err == nil && (service == "" || o.Region == "") {
		o.Region, err = ask("Signing region", o.Region)
	}
	if err == nil && (service == "" || o.Endpoint == "") {
		o.Endpoint, err = ask("Service URL (empty for AWS)", o.Endpoint)
	}
	if err != nil {
		return err
	}
	if service == "r2" && o.Endpoint == "" {
		return cliapp.InvalidConfig("Set the R2 account's S3 endpoint.")
	}
	return askClearable(ask, "Key prefix (empty for the whole bucket; - clears)", &o.Prefix)
}

func prepareS3(o s3Options, _ string) (cliapp.MountSource, error) {
	if o.Bucket == "" || strings.ContainsAny(o.Bucket, "/\\:") || (o.Prefix != "" && !strings.HasSuffix(o.Prefix, "/")) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("S3 requires a bucket and an empty or slash-terminated key prefix.")
	}
	if o.PartConcurrency < 0 || (o.PartSize == 0) != (o.MaxBufferedBytes == 0) ||
		(o.PartSize != 0 && (o.PartSize < 5<<20 || o.PartSize > 5<<30 || o.MaxBufferedBytes/o.PartSize < int64(max(1, o.PartConcurrency)))) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("S3 upload settings require a valid part size and a buffer budget for the requested concurrency.")
	}
	audience := cliapp.CredentialAudience{Origin: "aws-s3"}
	mode := cliapp.SDKDefaultProfile
	var endpoint *string
	if o.Endpoint != "" {
		u, err := url.Parse(o.Endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" ||
			u.User != nil || u.Opaque != "" || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || (u.Path != "" && u.Path != "/") {
			return cliapp.MountSource{}, cliapp.InvalidConfig("S3 endpoint must be an HTTP or HTTPS service origin.")
		}
		if o.Region == "" {
			return cliapp.MountSource{}, cliapp.InvalidConfig("A custom S3 endpoint requires a signing region.")
		}
		audience.Origin = u.Scheme + "://" + strings.ToLower(u.Host)
		mode = cliapp.SDKNamedProfile
		endpoint = aws.String(o.Endpoint)
	}
	return cliapp.MountSource{
		Audience: audience,
		Mode:     mode,
		Verify:   verifyKeyRoot,
		Open: func(ctx context.Context, profile string) (core.Driver, error) {
			settings := []func(*awsconfig.LoadOptions) error{awsconfig.WithLogger(logging.Nop{})}
			if o.Region != "" {
				settings = append(settings, awsconfig.WithRegion(o.Region))
			}
			if profile != "" {
				settings = append(settings, awsconfig.WithSharedConfigProfile(profile))
			}
			cfg, err := awsconfig.LoadDefaultConfig(ctx, settings...)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, &cliapp.Problem{Code: "credential_unavailable", Message: "Could not load AWS SDK settings.", NextAction: "Check the selected profile or SDK configuration."}
			}
			if mode == cliapp.SDKNamedProfile && !profileProvidesCredentials(cfg.Credentials) {
				return nil, &cliapp.Problem{Code: "credential_unavailable", Message: "The named S3 profile has no explicit credential source.", NextAction: "Configure credentials in that profile for the selected service."}
			}
			if cfg.Region == "" {
				return nil, cliapp.InvalidConfig("Set the S3 signing region in the mount or SDK configuration.")
			}
			client := sdk.NewFromConfig(cfg, func(s *sdk.Options) {
				// The saved mount chooses the service receiving signed requests.
				s.BaseEndpoint = endpoint
				s.UsePathStyle = o.PathStyle
				s.HTTPClient = storageHTTPClient
			})
			return cliapp.NativeDriver(s3driver.New(client, o.Bucket, o.Prefix, s3driver.Options{
				PlainListKeys: o.PlainListKeys, ListV1: o.ListV1, PartSize: o.PartSize,
				MaxBufferedBytes: o.MaxBufferedBytes, PartConcurrency: o.PartConcurrency,
			}))
		},
	}, nil
}

func profileProvidesCredentials(provider aws.CredentialsProvider) bool {
	sourced, ok := provider.(aws.CredentialProviderSource)
	if !ok {
		return false
	}
	for _, source := range sourced.ProviderSources() {
		switch source {
		case aws.CredentialSourceProfile, aws.CredentialSourceProfileNamedProvider, aws.CredentialSourceProfileSTSWebIDToken,
			aws.CredentialSourceProfileSSO, aws.CredentialSourceProfileSSOLegacy, aws.CredentialSourceProfileProcess, aws.CredentialSourceProfileLogin:
			return true
		}
	}
	return false
}

var verifyKeyRoot = cliapp.VerifyNativeRoot(core.NativeKeys("", ""))
