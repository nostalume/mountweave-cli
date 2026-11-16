package cli

import (
	"context"

	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/logging"
	core "github.com/nostalume/mountweave"
	s3driver "github.com/nostalume/mountweave/drivers/s3"
)

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

func guideS3(o *s3Options, ask askField) error {
	var service string
	if o.Bucket == "" {
		choices := strings.Join(slices.Sorted(maps.Keys(s3Presets)), ", ")
		preset, err := ask("S3 service ("+choices+")", "aws")
		if err != nil {
			return err
		}
		var ok bool
		service = strings.ToLower(preset)
		*o, ok = s3Presets[service]
		if !ok {
			return invalidConfig("Select a listed S3 service.")
		}
	}
	var err error
	o.Bucket, err = ask("Bucket", o.Bucket)
	if err != nil {
		return err
	}
	o.Region, err = ask("Signing region", o.Region)
	if err != nil {
		return err
	}
	o.Endpoint, err = ask("Service URL (empty for AWS)", o.Endpoint)
	if err != nil {
		return err
	}
	if service == "r2" && o.Endpoint == "" {
		return invalidConfig("Set the R2 account's S3 endpoint.")
	}
	o.Prefix, err = ask("Key prefix (empty for the whole bucket; - clears)", o.Prefix)
	if o.Prefix == "-" {
		o.Prefix = ""
	}
	return err
}

func prepareS3(o s3Options, _ string) (mountSource, error) {
	if o.Bucket == "" || strings.ContainsAny(o.Bucket, "/\\:") || (o.Prefix != "" && !strings.HasSuffix(o.Prefix, "/")) {
		return mountSource{}, invalidConfig("S3 requires a bucket and an empty or slash-terminated key prefix.")
	}
	if o.PartConcurrency < 0 || (o.PartSize == 0) != (o.MaxBufferedBytes == 0) ||
		(o.PartSize != 0 && (o.PartSize < 5<<20 || o.PartSize > 5<<30 || o.MaxBufferedBytes/o.PartSize < int64(max(1, o.PartConcurrency)))) {
		return mountSource{}, invalidConfig("S3 upload settings require a valid part size and a buffer budget for the requested concurrency.")
	}
	audience := credentialAudience{origin: "aws-s3"}
	mode := sdkDefaultProfile
	if o.Endpoint != "" {
		u, err := url.Parse(o.Endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" ||
			u.User != nil || u.Opaque != "" || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery || (u.Path != "" && u.Path != "/") {
			return mountSource{}, invalidConfig("S3 endpoint must be an HTTP or HTTPS service origin.")
		}
		if o.Region == "" {
			return mountSource{}, invalidConfig("A custom S3 endpoint requires a signing region.")
		}
		audience.origin = u.Scheme + "://" + strings.ToLower(u.Host)
		mode = sdkNamedProfile
	}
	return mountSource{
		audience: audience,
		mode:     mode,
		verify:   verifyKeyRoot,
		open: func(ctx context.Context, profile string) (core.Driver, error) {
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
				return nil, &problem{Code: "credential_unavailable", Message: "Could not load AWS SDK settings.", NextAction: "Check the selected profile or SDK configuration."}
			}
			if mode == sdkNamedProfile && !profileProvidesCredentials(cfg.Credentials) {
				return nil, &problem{Code: "credential_unavailable", Message: "The named S3 profile has no explicit credential source.", NextAction: "Configure credentials in that profile for the selected service."}
			}
			if cfg.Region == "" {
				return nil, invalidConfig("Set the S3 signing region in the mount or SDK configuration.")
			}
			client := sdk.NewFromConfig(cfg, func(s *sdk.Options) {
				// The saved mount chooses the service receiving signed requests.
				s.BaseEndpoint = nil
				if o.Endpoint != "" {
					s.BaseEndpoint = aws.String(o.Endpoint)
				}
				s.UsePathStyle = o.PathStyle
				s.HTTPClient = storageHTTPClient
			})
			return nativeDriver(s3driver.New(client, o.Bucket, o.Prefix, s3driver.Options{
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
