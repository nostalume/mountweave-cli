// Package webdav supplies the WebDAV collection source for explicit CLI builds.
package webdav

import (
	"context"
	"net/url"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/cli/host"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	driver "github.com/nostalume/mountweave/drivers/webdav"
)

// Options contains the public settings saved by the WebDAV source.
type Options struct {
	RootURL          string `json:"root_url"`
	Username         string `json:"username,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

var source = cliapp.SingleSource("webdav", "WebDAV collection", cliapp.SourceRoute(guide, prepare))

// Source returns the WebDAV connection used by the standard CLI.
func Source() host.Source { return source }

func guide(opts *Options, ask host.Prompt) error {
	err := cliapp.AskFields(ask, cliapp.Field("WebDAV URL", &opts.RootURL),
		cliapp.Field("WebDAV username (empty for anonymous; - clears)", &opts.Username))
	if err == nil && opts.Username == "-" {
		opts.Username = ""
	}
	return err
}

func prepare(opts Options, _ string) (cliapp.MountSource, error) {
	native := driver.Options{RootURL: opts.RootURL, Username: opts.Username, MaxResponseBytes: opts.MaxResponseBytes}
	if err := native.Validate(); err != nil {
		return cliapp.MountSource{}, cliapp.InvalidConfig("WebDAV requires a valid collection URL and settings.")
	}
	var audience cliapp.CredentialAudience
	if opts.Username != "" {
		u, _ := url.Parse(opts.RootURL)
		audience = cliapp.CredentialAudience{Origin: cliapp.StorageOrigin("webdav", u), Account: opts.Username}
	}
	src := cliapp.SecretSource(audience, "WebDAV password", func(ctx context.Context, password string) (core.Driver, error) {
		options := native
		options.Password = password
		return cliapp.NativeDriver(driver.Open(ctx, options))
	})
	access := "Anonymous access within this collection"
	if opts.Username != "" {
		access = "WebDAV permissions within this collection"
	}
	src.Location = cliapp.MountLocation{Location: opts.RootURL, Access: access, Quote: true}
	return src, nil
}
