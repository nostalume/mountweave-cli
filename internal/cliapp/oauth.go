package cliapp

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

type OAuthClient struct {
	ClientID     string        `json:"client_id"`
	ClientSecret string        `json:"client_secret,omitempty"`
	SecretRef    CredentialRef `json:"secret_ref,omitzero"`
	Tenant       string        `json:"tenant,omitempty"`
}

type OAuthGrant struct {
	Provider string   `json:"provider"`
	ID       string   `json:"id"`
	Account  string   `json:"account"`
	Label    string   `json:"label"`
	Scopes   []string `json:"scopes"`
	OAuthClient
	Access    string    `json:"access_token"`
	Refresh   string    `json:"refresh_token,omitempty"`
	Expires   time.Time `json:"expires_at"`
	Pending   bool      `json:"pending,omitempty"`
	SetupData any       `json:"-"` // Verified provider data used during setup only.
}

func (g OAuthGrant) Covers(scopes []string) bool {
	for _, scope := range scopes {
		if !slices.Contains(g.Scopes, scope) {
			return false
		}
	}
	return true
}

func (p *ManagedAuthorization) LoadGrant(id string) (OAuthGrant, error) {
	if !ValidGrantID(id) {
		return OAuthGrant{}, GrantUnavailable()
	}
	dir, err := GrantDirectory(false)
	if err != nil {
		return OAuthGrant{}, err
	}
	return p.ReadGrant(filepath.Join(dir, id+".grant"), id)
}

func (p *ManagedAuthorization) ReadGrant(path, id string) (OAuthGrant, error) {
	if p.Read != nil {
		return p.Read(path, id)
	}
	data, err := ReadGrant(path)
	if err != nil {
		return OAuthGrant{}, err
	}
	defer clear(data)
	var g OAuthGrant
	if json.Unmarshal(data, &g) != nil || g.Provider != p.Provider || g.ID != id {
		return OAuthGrant{}, GrantUnavailable()
	}
	if p.Kind == StaticGrant {
		if g.Access == "" || g.Pending {
			return OAuthGrant{}, GrantUnavailable()
		}
		return g, nil
	}
	application := p.Kind == ApplicationGrant
	if g.Account == "" || g.ClientID == "" || (!application && g.Refresh == "") ||
		(application && (!g.SecretRef.Valid() || (g.SecretRef.Kind != "env" && g.SecretRef.Kind != "file"))) ||
		(p.Kind != RotatingGrant && (g.Access == "" || g.Expires.IsZero())) {
		return OAuthGrant{}, GrantUnavailable()
	}
	return g, nil
}

func (g OAuthGrant) Save(ctx context.Context, lease *GrantLease, replace bool) (bool, error) {
	data, err := json.Marshal(g)
	if err != nil {
		return false, err
	}
	defer clear(data)
	return lease.Save(ctx, data, replace)
}

func AuthorizationScopeConflict() error {
	return &Problem{Code: "credential_scope_conflict", Message: "The authorization does not cover the requested permissions.",
		NextAction: "Authorize with the required permissions or choose another authorization."}
}

// A fresh token is an in-memory read. Reusable refresh tokens and app
// credentials share the same writer lock when the access token is due for renewal.
type cachedAccessTokens struct {
	provider *ManagedAuthorization
	current  atomic.Pointer[OAuthGrant]
}

func (t *cachedAccessTokens) access(ctx context.Context) (_ string, err error) {
	g := t.current.Load()
	if time.Until(g.Expires) > 30*time.Second {
		return g.Access, nil
	}
	lease, err := LockGrant(ctx, g.ID, false)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, lease.Lock.Close()) }()
	current, err := t.provider.ReadGrant(lease.Path, g.ID)
	if err != nil {
		return "", err
	}
	if current.Account != g.Account || (t.provider.Kind != ApplicationGrant && current.OAuthClient != g.OAuthClient) || !current.Covers(g.Scopes) {
		return "", AuthorizationScopeConflict()
	}
	if time.Until(current.Expires) <= 30*time.Second {
		if err := t.provider.Renew(ctx, &current); err != nil {
			return "", err
		}
		if _, err := current.Save(ctx, lease, true); err != nil {
			return "", errors.Join(GrantUnavailable(), err)
		}
	}
	t.current.Store(&current)
	return current.Access, nil
}

func (p *BrowserAuthorization) renew(ctx context.Context, current *OAuthGrant) error {
	reply, err := p.exchange(ctx, current.OAuthClient, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {current.Refresh},
	})
	if err != nil {
		return err
	}
	if (reply.Account != "" && reply.Account != current.Account) ||
		(reply.Scope != "" && !slices.Equal(OAuthScopeSet(reply.Scope), current.Scopes)) ||
		(reply.Refresh != "" && reply.Refresh != current.Refresh) {
		return core.Fail(core.InvalidUpstreamResponse, nil)
	}
	current.Access, current.Expires = reply.Access, reply.Expiry()
	return nil
}

func OAuthScopeSet(raw string) []string {
	fields := strings.Fields(raw)
	slices.Sort(fields)
	return slices.Compact(fields)
}

type OAuthTokenReply struct {
	Access   string `json:"access_token"`
	Refresh  string `json:"refresh_token"`
	Account  string `json:"account_id"`
	Scope    string `json:"scope"`
	Type     string `json:"token_type"`
	Lifetime int64  `json:"expires_in"`
	Error    string `json:"error"`
}

func (r OAuthTokenReply) Expiry() time.Time {
	return time.Now().Add(time.Duration(r.Lifetime) * time.Second)
}

func (p *BrowserAuthorization) exchange(ctx context.Context, app OAuthClient, form url.Values) (OAuthTokenReply, error) {
	form.Set("client_id", app.ClientID)
	if app.ClientSecret != "" {
		form.Set("client_secret", app.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return OAuthTokenReply{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var reply OAuthTokenReply
	status, err := ReadOAuthResponse(req, &reply)
	if err != nil {
		return OAuthTokenReply{}, err
	}
	if status == http.StatusBadRequest && reply.Error == "invalid_grant" {
		return OAuthTokenReply{}, &Problem{Code: "interaction_required", Message: p.Label + " rejected the authorization.",
			NextAction: "Choose reauthorize.", Reconnect: true}
	}
	if (status == http.StatusBadRequest || status == http.StatusUnauthorized) && reply.Error == "invalid_client" {
		return OAuthTokenReply{}, &Problem{Code: "interaction_required", Message: p.Label + " rejected the application credentials.",
			NextAction: "Check the application registration and authorize with valid application credentials."}
	}
	if status != http.StatusOK {
		return OAuthTokenReply{}, core.Fail(core.UpstreamFailure, nil)
	}
	if reply.Access == "" || !strings.EqualFold(reply.Type, "bearer") ||
		reply.Lifetime <= 0 || reply.Lifetime > (1<<63-1)/int64(time.Second) {
		return OAuthTokenReply{}, core.Fail(core.InvalidUpstreamResponse, nil)
	}
	return reply, nil
}

func ReadOAuthResponse(req *http.Request, result any) (int, error) {
	client := http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.Do(req)
	code := core.UpstreamFailure
	var data []byte
	if err == nil {
		defer res.Body.Close()
		switch res.StatusCode {
		case http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized:
		default:
			return res.StatusCode, nil
		}
		code = core.InvalidUpstreamResponse
		data, err = io.ReadAll(io.LimitReader(res.Body, maxGrantBytes+1))
		defer clear(data)
	}
	if err := req.Context().Err(); err != nil {
		return 0, err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 0, context.DeadlineExceeded
	}
	if err != nil || len(data) > maxGrantBytes || json.Unmarshal(data, result) != nil {
		return 0, core.Fail(code, nil)
	}
	return res.StatusCode, nil
}

func (p *BrowserAuthorization) authorize(ctx context.Context, app OAuthClient, scopes []string, intent AuthorizationIntent, out io.Writer) (OAuthGrant, error) {
	code, err := p.browserCode(ctx, app, scopes, intent, out)
	if err != nil {
		return OAuthGrant{}, err
	}
	reply, err := p.exchange(ctx, app, url.Values{
		"grant_type": {"authorization_code"}, "redirect_uri": {code.redirectURI},
		"code": {code.code}, "code_verifier": {code.verifier},
	})
	if err != nil {
		return OAuthGrant{}, err
	}
	if reply.Refresh == "" {
		return OAuthGrant{}, core.Fail(core.InvalidUpstreamResponse, nil)
	}
	g := OAuthGrant{
		Scopes: OAuthScopeSet(reply.Scope), Access: reply.Access, Refresh: reply.Refresh, Expires: reply.Expiry(),
	}
	if !g.Covers(scopes) {
		return OAuthGrant{}, AuthorizationScopeConflict()
	}
	g.Account, g.Label, err = p.Identity(ctx, reply)
	return g, err
}

func (p *ManagedAuthorization) BearerSource(open func(context.Context, *http.Client) (core.Driver, error), scopes []string, hosts ...string) MountSource {
	src := BearerSource("https", open, hosts...)
	src.OpenManaged = func(ctx context.Context, id string) (core.Driver, error) {
		grant, err := p.LoadGrant(id)
		if err != nil {
			return nil, err
		}
		if !grant.Covers(scopes) {
			return nil, AuthorizationScopeConflict()
		}
		tokens := cachedAccessTokens{provider: p}
		tokens.current.Store(&grant)
		return open(ctx, &http.Client{Transport: TokenHeaderTransport{
			Base: http.DefaultTransport, Scheme: "https", Hosts: hosts, Bearer: tokens.access,
		}})
	}
	return src
}
