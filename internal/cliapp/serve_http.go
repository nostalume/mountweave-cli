package cliapp

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/protocols"
	"github.com/nostalume/mountweave/protocols/httpapi"
	"github.com/nostalume/mountweave/protocols/s3"
	"github.com/nostalume/mountweave/protocols/webdav"
)

func passwordMatches(username, password string) func(string, string) bool {
	user, secret := sha256.Sum256([]byte(username)), sha256.Sum256([]byte(password))
	return func(username, password string) bool {
		gotUser, gotSecret := sha256.Sum256([]byte(username)), sha256.Sum256([]byte(password))
		return subtle.ConstantTimeCompare(user[:], gotUser[:])&subtle.ConstantTimeCompare(secret[:], gotSecret[:]) == 1
	}
}

func jsonExportHandler(c *core.Core, principal core.Principal, secret string) (http.Handler, func(context.Context) error, error) {
	token := strings.TrimRight(secret, "=")
	if token == "" || strings.ContainsFunc(token, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune("-._~+/", r))
	}) {
		return nil, nil, InvalidConfig("The JSON HTTP credential must be a bearer token.")
	}
	digest := sha256.Sum256([]byte(secret))
	handler, err := httpapi.New(c, httpapi.Options{
		CursorTTL: time.Minute,
		Authenticate: func(_ context.Context, value string) (core.Principal, error) {
			got := sha256.Sum256([]byte(value))
			if subtle.ConstantTimeCompare(digest[:], got[:]) == 1 {
				return principal, nil
			}
			return "", core.Fail(core.Unauthenticated, nil)
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return handler, handler.Close, nil
}

func webDAVExportHandler(c *core.Core, root core.Address, principal core.Principal, username, secret string, createOnly bool) (http.Handler, error) {
	if !utf8.ValidString(secret) || strings.ContainsFunc(secret, unicode.IsControl) {
		return nil, InvalidConfig("The WebDAV password must be UTF-8 without control characters.")
	}
	matches := passwordMatches(username, secret)
	return webdav.New(c, webdav.Options{
		Prefix: "/", Root: root, CursorTTL: time.Minute, Tree: directoryPlan, CreateOnly: createOnly,
		Limits: webdav.Limits{XMLBytes: 64 << 10, ListEntries: directoryPlan.MaxEntries, ListNameBytes: directoryPlan.MaxPlanBytes},
		Authenticate: func(w http.ResponseWriter, r *http.Request) (core.Principal, bool) {
			user, password, ok := r.BasicAuth()
			if ok && len(r.Header.Values("Authorization")) == 1 && matches(user, password) {
				return principal, true
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="mweave", charset="UTF-8"`)
			http.Error(w, "Authentication required.", http.StatusUnauthorized)
			return "", false
		},
	})
}

func s3ExportHandler(startup, requests context.Context, base string, c *core.Core, opts S3ExportOptions, scope exportScope, principal core.Principal, accessKey, secret string, logger *log.Logger) (http.Handler, func(context.Context) error, error) {
	options := s3.Options{
		Region: cmp.Or(opts.Region, DefaultS3Region), CursorTTL: time.Minute, MaxPutBytes: cmp.Or(opts.MaxPutBytes, 5<<30),
		Buckets: map[string]s3.Bucket{opts.Bucket: {Space: scope.root}},
		LookupIdentity: func(_ context.Context, key string) (s3.Identity, error) {
			if key != accessKey {
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
	state, err := openS3Multipart(startup, multipart.directory(base), principal, multipart.limits())
	if err != nil {
		return nil, nil, err
	}
	retention := cmp.Or(time.Duration(multipart.RetentionSeconds)*time.Second, multipartRetention)
	options.Multipart = &s3.MultipartOptions{
		DB: state.db, Inputs: state.inputs, MaxUploads: 1024, MaxPendingParts: 32,
		MaxObjectBytes: cmp.Or(multipart.MaxObjectBytes, multipart.MaxStagingBytes), Retention: retention,
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
	return owner, owner.Close, nil
}
