package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/alipan"
	"github.com/nostalume/mountweave/drivers/alipan/credential"
)

type aliPanGrant struct {
	ID           string            `json:"id"`
	Account      string            `json:"account"`
	Scopes       []string          `json:"scopes"`
	ClientID     string            `json:"client_id"`
	ClientSecret string            `json:"client_secret,omitempty"`
	Checkpoint   credential.Record `json:"checkpoint"`
}

type managedAliPan struct {
	*alipan.Driver
	release func() error
}

func (d *managedAliPan) Close() error {
	// Native Close waits for the refresh writer even when it reports a failed checkpoint.
	return errors.Join(d.Driver.Close(), d.release())
}

func guideAliPan(opts *alipan.Options, ask askField) error {
	drive, err := ask("Drive ID", opts.DriveID)
	if err != nil {
		return err
	}
	opts.DriveID = drive
	return guideAliPanRoot(opts, ask)
}

func guideAliPanRoot(opts *alipan.Options, ask askField) error {
	root := opts.RootPath
	if root == "" {
		root = opts.RootID
	}
	if root == "" {
		root = "/"
	}
	root, err := ask("Directory path or ID", root)
	if err != nil {
		return err
	}
	opts.RootID, opts.RootPath = root, ""
	if len(root) != 0 && root[0] == '/' {
		opts.RootID, opts.RootPath = "", root
	}
	return nil
}

func prepareAliPan(opts alipan.Options, _ string) (mountSource, error) {
	if opts.Endpoint != "" && opts.Endpoint != alipan.Endpoint {
		return mountSource{}, invalidConfig("Managed AliPan credentials require the official API origin.")
	}
	if err := opts.Validate(); err != nil {
		return mountSource{}, invalidConfig("AliPan requires a drive ID and one valid directory path or ID.")
	}
	return mountSource{
		audience: credentialAudience{origin: "alipan " + alipan.Endpoint},
		openManaged: func(ctx context.Context, id string) (core.Driver, error) {
			return openManagedAliPan(ctx, opts, id)
		},
		verify: verifyAliPanRoot,
	}, nil
}

func verifyAliPanRoot(ctx context.Context, d core.Driver) error {
	it, err := d.Enumerate(ctx, core.NativeChildren(core.NativePath("/")))
	if err != nil {
		return err
	}
	_, err = it.Next(ctx, 1)
	if err == io.EOF {
		err = nil
	}
	return errors.Join(err, it.Close())
}

func openManagedAliPan(ctx context.Context, opts alipan.Options, id string) (core.Driver, error) {
	lease, grant, source, err := acquireAliPanAuthorization(id)
	if err != nil {
		return nil, err
	}
	if opts.PartSize > 0 && !slices.Contains(grant.Scopes, aliPanWriteScope) {
		return nil, errors.Join(authorizationScopeConflict(), source.Close(), lease.lock.Close())
	}
	opts.Client, opts.Credentials = &http.Client{}, source
	d, err := alipan.Open(ctx, opts)
	if d == nil {
		return nil, errors.Join(err, source.Close(), lease.lock.Close())
	}
	return &managedAliPan{Driver: d, release: sync.OnceValue(lease.lock.Close)}, err
}

func acquireAliPanAuthorization(id string) (*grantLease, oauthGrant, *credential.Source, error) {
	lease, err := acquireGrant(id, false)
	if err != nil {
		return nil, oauthGrant{}, nil, err
	}
	grant, err := readAliPanAuthorization(lease.path, id)
	if err != nil {
		return nil, oauthGrant{}, nil, errors.Join(err, lease.lock.Close())
	}
	oauth := alipan.OAuth{Client: &http.Client{}, ClientID: grant.ClientID, ClientSecret: grant.ClientSecret}
	source, err := credential.New(aliPanCheckpoint(grant), func(ctx context.Context, refresh string) (credential.Token, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if oauth.ClientSecret != "" {
			return oauth.Refresh(ctx, refresh)
		}
		return exchangeAliPan(ctx, map[string]string{
			"client_id": oauth.ClientID, "grant_type": "refresh_token", "refresh_token": refresh,
		})
	}, func(ctx context.Context, checkpoint credential.Record) error {
		grant.Access, grant.Refresh, grant.Expires = checkpoint.Token.Access, checkpoint.Token.Refresh, checkpoint.Token.ExpiresAt
		grant.Pending = checkpoint.Pending
		if !checkpoint.Pending {
			// A rotated credential is already committed at the provider.
			ctx = context.WithoutCancel(ctx)
		}
		return grant.save(ctx, lease, true)
	})
	if err != nil {
		return nil, oauthGrant{}, nil, errors.Join(err, lease.lock.Close())
	}
	return lease, grant, source, nil
}

func aliPanCheckpoint(g oauthGrant) credential.Record {
	return credential.Record{Pending: g.Pending,
		Token: credential.Token{Access: g.Access, Refresh: g.Refresh, ExpiresAt: g.Expires}}
}

func readAliPanAuthorization(path, id string) (oauthGrant, error) {
	data, err := readGrant(path)
	if err != nil {
		return oauthGrant{}, err
	}
	defer clear(data)
	var grant oauthGrant
	if err := json.Unmarshal(data, &grant); err != nil || grant.ID != id || grant.Account == "" || grant.ClientID == "" {
		return oauthGrant{}, grantUnavailable()
	}
	if grant.Provider == "" {
		var legacy aliPanGrant
		if err := json.Unmarshal(data, &legacy); err != nil {
			return oauthGrant{}, grantUnavailable()
		}
		grant.Provider = "alipan"
		grant.Label = aliPanGrantLabel("", grant)
		grant.Access, grant.Refresh, grant.Expires = legacy.Checkpoint.Token.Access, legacy.Checkpoint.Token.Refresh, legacy.Checkpoint.Token.ExpiresAt
		grant.Pending = legacy.Checkpoint.Pending
	}
	if grant.Provider != "alipan" || grant.Refresh == "" {
		return oauthGrant{}, grantUnavailable()
	}
	return grant, nil
}
