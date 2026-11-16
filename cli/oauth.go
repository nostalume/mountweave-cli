package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	core "github.com/nostalume/mountweave"
)

type oauthClient struct {
	ClientID     string        `json:"client_id"`
	ClientSecret string        `json:"client_secret,omitempty"`
	SecretRef    credentialRef `json:"secret_ref,omitzero"`
	Tenant       string        `json:"tenant,omitempty"`
}

type oauthGrant struct {
	Provider string   `json:"provider"`
	ID       string   `json:"id"`
	Account  string   `json:"account"`
	Label    string   `json:"label"`
	Scopes   []string `json:"scopes"`
	oauthClient
	Access       string    `json:"access_token"`
	Refresh      string    `json:"refresh_token,omitempty"`
	Expires      time.Time `json:"expires_at"`
	Pending      bool      `json:"pending,omitempty"`
	aliPanDrives *aliPanDriveInfo
}

func (g oauthGrant) covers(scopes []string) bool {
	for _, scope := range scopes {
		if !slices.Contains(g.Scopes, scope) {
			return false
		}
	}
	return true
}

func (p *managedAuthorization) loadGrant(id string) (oauthGrant, error) {
	if !validGrantID(id) {
		return oauthGrant{}, grantUnavailable()
	}
	dir, err := grantDirectory(false)
	if err != nil {
		return oauthGrant{}, err
	}
	return p.readGrant(filepath.Join(dir, id+".grant"), id)
}

func (p *managedAuthorization) readGrant(path, id string) (oauthGrant, error) {
	if p.read != nil {
		return p.read(path, id)
	}
	data, err := readGrant(path)
	if err != nil {
		return oauthGrant{}, err
	}
	defer clear(data)
	var g oauthGrant
	if json.Unmarshal(data, &g) != nil || g.Provider != p.provider || g.ID != id {
		return oauthGrant{}, grantUnavailable()
	}
	if p.staticOnly {
		if g.Access == "" || g.Pending {
			return oauthGrant{}, grantUnavailable()
		}
		return g, nil
	}
	if g.Account == "" || g.ClientID == "" || (!p.appCredentials && g.Refresh == "") ||
		(p.appCredentials && (!g.SecretRef.valid() || (g.SecretRef.Kind != "env" && g.SecretRef.Kind != "file"))) ||
		(!p.exclusive && (g.Access == "" || g.Expires.IsZero())) {
		return oauthGrant{}, grantUnavailable()
	}
	return g, nil
}

func (g oauthGrant) save(ctx context.Context, lease *grantLease, replace bool) error {
	data, err := json.Marshal(g)
	if err != nil {
		return err
	}
	defer clear(data)
	return lease.save(ctx, data, replace)
}

func authorizationScopeConflict() error {
	return &problem{Code: "credential_scope_conflict", Message: "The authorization does not cover the requested permissions.",
		NextAction: "Authorize with the required permissions or choose another authorization."}
}

// A fresh token is an in-memory read. Reusable refresh tokens and app
// credentials share the same writer lock when the access token is due for renewal.
type cachedAccessTokens struct {
	provider *managedAuthorization
	current  atomic.Pointer[oauthGrant]
	renew    func(context.Context, *oauthGrant) error
}

func (t *cachedAccessTokens) access(ctx context.Context) (_ string, err error) {
	g := t.current.Load()
	if time.Until(g.Expires) > 30*time.Second {
		return g.Access, nil
	}
	lease, err := lockGrant(ctx, g.ID, false)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, lease.lock.Close()) }()
	current, err := t.provider.readGrant(lease.path, g.ID)
	if err != nil {
		return "", err
	}
	if current.Account != g.Account || (!t.provider.appCredentials && current.oauthClient != g.oauthClient) || !current.covers(g.Scopes) {
		return "", authorizationScopeConflict()
	}
	if time.Until(current.Expires) <= 30*time.Second {
		if err := t.renew(ctx, &current); err != nil {
			return "", err
		}
		if err := current.save(ctx, lease, true); err != nil {
			return "", errors.Join(grantUnavailable(), err)
		}
	}
	t.current.Store(&current)
	return current.Access, nil
}

func (p *browserAuthorization) renew(ctx context.Context, current *oauthGrant) error {
	reply, err := p.exchange(ctx, current.oauthClient, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {current.Refresh},
	})
	if err != nil {
		return err
	}
	if (reply.Account != "" && reply.Account != current.Account) ||
		(reply.Scope != "" && !slices.Equal(oauthScopeSet(reply.Scope), current.Scopes)) ||
		(reply.Refresh != "" && reply.Refresh != current.Refresh) {
		return core.Fail(core.InvalidUpstreamResponse, nil)
	}
	current.Access, current.Expires = reply.Access, reply.expiry()
	return nil
}

func oauthScopeSet(raw string) []string {
	fields := strings.Fields(raw)
	slices.Sort(fields)
	return slices.Compact(fields)
}

type oauthTokenReply struct {
	Access   string `json:"access_token"`
	Refresh  string `json:"refresh_token"`
	Account  string `json:"account_id"`
	Scope    string `json:"scope"`
	Type     string `json:"token_type"`
	Lifetime int64  `json:"expires_in"`
	Error    string `json:"error"`
}

func (r oauthTokenReply) expiry() time.Time {
	return time.Now().Add(time.Duration(r.Lifetime) * time.Second)
}

func (r oauthTokenReply) validAccess() bool {
	return r.Access != "" && strings.EqualFold(r.Type, "bearer") &&
		r.Lifetime > 0 && r.Lifetime <= (1<<63-1)/int64(time.Second)
}

func requestOAuthForm(ctx context.Context, endpoint string, app oauthClient, form url.Values, result any) (int, error) {
	form.Set("client_id", app.ClientID)
	if app.ClientSecret != "" {
		form.Set("client_secret", app.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return readOAuthResponse(req, result)
}

func (p *browserAuthorization) exchange(ctx context.Context, app oauthClient, form url.Values) (oauthTokenReply, error) {
	var reply oauthTokenReply
	status, err := requestOAuthForm(ctx, p.tokenURL, app, form, &reply)
	if err != nil {
		return oauthTokenReply{}, err
	}
	if status == http.StatusBadRequest && reply.Error == "invalid_grant" {
		return oauthTokenReply{}, &problem{Code: "interaction_required", Message: p.label + " rejected the authorization.",
			NextAction: "Run mweave mount edit for this mount and reauthorize, or repeat the interactive setup."}
	}
	if (status == http.StatusBadRequest || status == http.StatusUnauthorized) && reply.Error == "invalid_client" {
		return oauthTokenReply{}, &problem{Code: "interaction_required", Message: p.label + " rejected the application credentials.",
			NextAction: "Check the application registration and authorize with valid application credentials."}
	}
	if status != http.StatusOK {
		return oauthTokenReply{}, core.Fail(core.UpstreamFailure, nil)
	}
	if !reply.validAccess() {
		return oauthTokenReply{}, core.Fail(core.InvalidUpstreamResponse, nil)
	}
	return reply, nil
}

func readOAuthResponse(req *http.Request, result any) (int, error) {
	client := http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return 0, req.Context().Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, context.DeadlineExceeded
		}
		return 0, core.Fail(core.UpstreamFailure, nil)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusUnauthorized {
		data, err := io.ReadAll(io.LimitReader(res.Body, maxGrantBytes+1))
		defer clear(data)
		if req.Context().Err() != nil {
			return 0, req.Context().Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, context.DeadlineExceeded
		}
		if err != nil || len(data) > maxGrantBytes || json.Unmarshal(data, result) != nil {
			return 0, core.Fail(core.InvalidUpstreamResponse, nil)
		}
	}
	return res.StatusCode, nil
}

func (p *browserAuthorization) authorize(ctx context.Context, app oauthClient, scopes []string, intent authorizationIntent, out io.Writer) (oauthGrant, error) {
	code, err := p.browserCode(ctx, app, scopes, intent, out)
	if err != nil {
		return oauthGrant{}, err
	}
	reply, err := p.exchange(ctx, app, url.Values{
		"grant_type": {"authorization_code"}, "redirect_uri": {code.redirectURI},
		"code": {code.code}, "code_verifier": {code.verifier},
	})
	if err != nil {
		return oauthGrant{}, err
	}
	if reply.Refresh == "" {
		return oauthGrant{}, core.Fail(core.InvalidUpstreamResponse, nil)
	}
	g := oauthGrant{
		Scopes: oauthScopeSet(reply.Scope), Access: reply.Access, Refresh: reply.Refresh, Expires: reply.expiry(),
	}
	if !g.covers(scopes) {
		return oauthGrant{}, authorizationScopeConflict()
	}
	g.Account, g.Label, err = p.identity(ctx, reply)
	return g, err
}

func (p *managedAuthorization) bearerSource(open func(context.Context, *http.Client) (core.Driver, error), scopes []string, renew func(context.Context, *oauthGrant) error, hosts ...string) mountSource {
	src := bearerSource("https", open, hosts...)
	src.openManaged = func(ctx context.Context, id string) (core.Driver, error) {
		grant, err := p.loadGrant(id)
		if err != nil {
			return nil, err
		}
		if !grant.covers(scopes) {
			return nil, authorizationScopeConflict()
		}
		tokens := cachedAccessTokens{provider: p, renew: renew}
		tokens.current.Store(&grant)
		return open(ctx, &http.Client{Transport: tokenHeaderTransport{
			base: http.DefaultTransport, scheme: "https", hosts: hosts, bearer: tokens.access,
		}})
	}
	return src
}
