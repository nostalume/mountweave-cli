package stock

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave/drivers/alipan"
	"github.com/nostalume/mountweave/drivers/alipan/credential"
	"github.com/nostalume/mountweave/drivers/pan115"
)

func projectRotationProblem(err error) *cliapp.Problem {
	switch {
	case errors.Is(err, credential.ErrPersistence):
		return &cliapp.Problem{Code: "credential_unavailable", Message: "The authorization checkpoint could not be saved.", NextAction: "Check the protected credential store for this user."}
	case errors.Is(err, credential.ErrRefreshUncertain):
		return &cliapp.Problem{Code: "interaction_required", Message: "The authorization refresh outcome could not be confirmed.", NextAction: "Check the authorization with the provider before retrying."}
	}
	return nil
}

func rotationCheckpoint(g cliapp.OAuthGrant) credential.Record {
	return credential.Record{Pending: g.Pending,
		Token: credential.Token{Access: g.Access, Refresh: g.Refresh, ExpiresAt: g.Expires}}
}

func acquireRotation(id string, read func(string, string) (cliapp.OAuthGrant, error),
	refresh func(context.Context, cliapp.OAuthClient, string) (credential.Token, error)) (cliapp.OAuthGrant, *credential.Source, func() error, error) {
	lease, err := cliapp.AcquireGrant(id, false)
	if err != nil {
		return cliapp.OAuthGrant{}, nil, nil, err
	}
	g, err := read(lease.Path, id)
	var source *credential.Source
	if err == nil {
		source, err = credential.New(rotationCheckpoint(g), func(ctx context.Context, previous string) (credential.Token, error) {
			return refresh(ctx, g.OAuthClient, previous)
		}, func(ctx context.Context, record credential.Record) error {
			g.Access, g.Refresh, g.Expires, g.Pending = record.Token.Access, record.Token.Refresh, record.Token.ExpiresAt, record.Pending
			if !record.Pending {
				// A confirmed rotation must survive shutdown.
				ctx = context.WithoutCancel(ctx)
			}
			_, err := g.Save(ctx, lease, true)
			return err
		})
	}
	if err != nil {
		return cliapp.OAuthGrant{}, nil, nil, errors.Join(err, lease.Lock.Close())
	}
	return g, source, sync.OnceValue(lease.Lock.Close), nil
}

type managed115 struct {
	*pan115.Driver
	closeAuthorization func() error
}

func (d *managed115) Close() error {
	return errors.Join(d.Driver.Close(), d.closeAuthorization())
}

func openManaged115(ctx context.Context, opts pan115.Options, id string) (core.Driver, error) {
	_, tokens, release, err := acquireRotation(id, pan115Authorization.ReadGrant, func(ctx context.Context, _ cliapp.OAuthClient, refresh string) (credential.Token, error) {
		token, err := pan115.AuthorizationClient(cliapp.ReadOAuthResponse).Refresh(ctx, refresh)
		return credential.Token(token), authorization115Error(err)
	})
	if err != nil {
		if errors.Is(err, credential.ErrRefreshUncertain) {
			err = errors.Join(&cliapp.Problem{Code: "interaction_required", Message: "The 115 Open refresh outcome could not be confirmed.",
				NextAction: "Run mweave mount edit and choose reauthorize."}, err)
		}
		return nil, err
	}
	closeAuthorization := func() error { return errors.Join(tokens.Close(), release()) }
	opts.Client = &http.Client{Transport: cliapp.TokenHeaderTransport{
		Base: http.DefaultTransport, Scheme: "https", Hosts: []string{pan115.ControlHost}, Bearer: tokens.Access,
	}}
	d, err := pan115.Open(ctx, opts)
	if d == nil {
		return nil, errors.Join(err, closeAuthorization())
	}
	return &managed115{Driver: d, closeAuthorization: closeAuthorization}, err
}

func probeAliPanAuthorization(ctx context.Context, m cliapp.MountRecord, fresh cliapp.OAuthGrant) error {
	var opts alipan.Options
	if err := cliapp.DecodeObject(m.Options, &opts); err != nil {
		return err
	}
	if drives, _ := fresh.SetupData.(alipan.DriveInfo); !drives.HasDrive(opts.DriveID) {
		return cliapp.AuthorizationScopeConflict()
	}
	// Verification uses the freshly issued access token without claiming a second
	// refresh lease or publishing credentials before the existing roots are checked.
	unavailable := core.Fail(core.UpstreamAccessDenied, nil)
	tokens, err := credential.New(rotationCheckpoint(fresh), func(context.Context, string) (credential.Token, error) {
		return credential.Token{}, unavailable
	}, func(context.Context, credential.Record) error { return unavailable })
	if err != nil {
		return err
	}
	opts.Client, opts.Credentials = &http.Client{}, tokens
	driver, err := alipan.Open(ctx, opts)
	if driver == nil {
		return errors.Join(err, tokens.Close())
	}
	if err == nil {
		err = verifyAliPanRoot(ctx, driver)
	}
	return errors.Join(err, driver.Close())
}

var verifyAliPanRoot = cliapp.VerifyNativeRoot(core.NativeChildren(core.NativePath("/")))

type managedAliPan struct {
	*alipan.Driver
	release func() error
}

func (d *managedAliPan) Close() error {
	// Native Close waits for the refresh writer even when it reports a failed checkpoint.
	return errors.Join(d.Driver.Close(), d.release())
}

func openManagedAliPan(ctx context.Context, opts alipan.Options, id string) (core.Driver, error) {
	grant, source, release, err := acquireAliPanAuthorization(id)
	if err != nil {
		return nil, err
	}
	if opts.PartSize > 0 && !slices.Contains(grant.Scopes, aliPanWriteScope) {
		return nil, errors.Join(cliapp.AuthorizationScopeConflict(), source.Close(), release())
	}
	opts.Client, opts.Credentials = &http.Client{}, source
	d, err := alipan.Open(ctx, opts)
	if d == nil {
		return nil, errors.Join(err, source.Close(), release())
	}
	return &managedAliPan{Driver: d, release: release}, err
}

func acquireAliPanAuthorization(id string) (cliapp.OAuthGrant, *credential.Source, func() error, error) {
	return acquireRotation(id, readAliPanAuthorization, func(ctx context.Context, app cliapp.OAuthClient, refresh string) (credential.Token, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		oauth := aliPanOAuth(app)
		if app.ClientSecret != "" {
			return oauth.Refresh(ctx, refresh)
		}
		token, err := oauth.RefreshScan(ctx, refresh)
		return token, projectAliPanOAuth(err)
	})
}

func readAliPanAuthorization(path, id string) (cliapp.OAuthGrant, error) {
	data, err := cliapp.ReadGrant(path)
	if err != nil {
		return cliapp.OAuthGrant{}, err
	}
	defer clear(data)
	var stored struct {
		cliapp.OAuthGrant
		Checkpoint json.RawMessage `json:"checkpoint"`
	}
	defer func() { clear(stored.Checkpoint) }()
	if err := json.Unmarshal(data, &stored); err != nil || stored.ID != id || stored.Account == "" || stored.ClientID == "" {
		return cliapp.OAuthGrant{}, cliapp.GrantUnavailable()
	}
	grant := stored.OAuthGrant
	if grant.Provider == "" {
		var checkpoint credential.Record
		if err := json.Unmarshal(stored.Checkpoint, &checkpoint); err != nil {
			return cliapp.OAuthGrant{}, cliapp.GrantUnavailable()
		}
		grant.Provider = "alipan"
		grant.Label = aliPanGrantLabel("", grant)
		grant.Access, grant.Refresh, grant.Expires, grant.Pending = checkpoint.Token.Access, checkpoint.Token.Refresh, checkpoint.Token.ExpiresAt, checkpoint.Pending
	}
	if grant.Provider != "alipan" || grant.Refresh == "" {
		return cliapp.OAuthGrant{}, cliapp.GrantUnavailable()
	}
	return grant, nil
}
