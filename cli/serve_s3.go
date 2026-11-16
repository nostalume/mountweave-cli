package cli

import (
	"context"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/protocols"
	"github.com/nostalume/mountweave/protocols/s3"
)

const defaultS3Region = "us-east-1"

type s3ExportOptions struct {
	Bucket      string              `json:"bucket"`
	Region      string              `json:"region,omitempty"`
	MaxPutBytes int64               `json:"max_put_bytes,omitempty"`
	Multipart   *s3MultipartOptions `json:"multipart,omitempty"`
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func sigV4Name(s string) bool {
	return s != "" && !strings.ContainsAny(s, "/,") && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

func s3ExportHandler(startup, requests context.Context, saved configSnapshot, c *core.Core, e export, principal core.Principal, secret string, logger *log.Logger) (http.Handler, func(context.Context) error, error) {
	opts := *e.S3
	if opts.Region == "" {
		opts.Region = defaultS3Region
	}
	if opts.MaxPutBytes == 0 {
		opts.MaxPutBytes = 5 << 30
	}
	options := s3.Options{
		Region: opts.Region, CursorTTL: time.Minute, MaxPutBytes: opts.MaxPutBytes,
		Buckets: map[string]s3.Bucket{opts.Bucket: {Space: e.Root}},
		LookupIdentity: func(_ context.Context, key string) (s3.Identity, error) {
			if key != e.Auth.AccessKeyID {
				return s3.Identity{}, core.Fail(core.Unauthenticated, nil)
			}
			return s3.Identity{Principal: principal, SecretAccessKey: secret}, nil
		},
	}
	if opts.Multipart == nil {
		handler, err := s3.New(c, options)
		return handler, nil, err
	}

	multipart := *opts.Multipart
	dir := hostPath(filepath.Dir(saved.path), multipart.StateDirectory)
	state, err := openS3Multipart(startup, dir, principal, multipart.limits())
	if err != nil {
		return nil, nil, err
	}
	retention := multipartRetention
	if multipart.RetentionSeconds != 0 {
		retention = time.Duration(multipart.RetentionSeconds) * time.Second
	}
	objectLimit := multipart.MaxObjectBytes
	if objectLimit == 0 {
		objectLimit = multipart.MaxStagingBytes
	}
	scope := e.scope()
	options.Multipart = &s3.MultipartOptions{
		DB: state.db, Inputs: state.inputs, MaxUploads: 1024, MaxPendingParts: 32,
		MaxObjectBytes: objectLimit, Retention: retention,
		Bind: func(ctx context.Context, actor core.Principal, bucket string, target core.Address, expected string) (string, *core.Selection, error) {
			if actor != principal || bucket != opts.Bucket || !scope.allows(core.WriteAction, target) {
				return "", nil, core.Fail(core.PermissionDenied, nil)
			}
			if expected != "" && expected != string(principal) {
				return "", nil, core.Fail(core.MountUnavailable, nil)
			}
			selected, err := c.Capture(ctx, core.AddressAccess{Address: target, Write: true})
			return string(principal), selected, err
		},
	}
	recovering := true
	options.OnFailure = func(f protocols.Failure) {
		if !f.HasChange || f.Change.Effect != core.Unknown {
			return
		}
		if f.Phase == "recovery" {
			if recovering {
				logger.Print("s3_multipart_unknown: ", f.Err)
			}
		} else {
			logger.Print("s3_multipart_unknown: publication outcome could not be confirmed")
		}
	}
	handler, err := s3.New(c, options)
	recovering = false
	if err == nil {
		err = startup.Err()
	}
	if err != nil {
		return nil, nil, errors.Join(err, state.close())
	}
	owner := newS3MultipartExport(requests, handler, state, logger)
	return owner, owner.close, nil
}
