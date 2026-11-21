package cliapp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	core "github.com/nostalume/mountweave"
)

func StorageOrigin(service string, u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return service + " " + u.Scheme + " " + strings.ToLower(u.Hostname()) + " " + port
}

func SecretSource(audience CredentialAudience, label string, open func(context.Context, string) (core.Driver, error)) MountSource {
	return MountSource{Audience: audience, Secret: label, Open: open}
}

func BearerSource(scheme string, open func(context.Context, *http.Client) (core.Driver, error), hosts ...string) MountSource {
	return HeaderTokenSource(scheme, "Authorization", "Bearer ", open, hosts...)
}

func HeaderTokenSource(scheme, header, prefix string, open func(context.Context, *http.Client) (core.Driver, error), hosts ...string) MountSource {
	return MountSource{
		Audience: CredentialAudience{Origin: header + " " + scheme + "://" + strings.ToLower(strings.Join(hosts, " "))},
		Secret:   "Access token",
		Open: func(ctx context.Context, token string) (core.Driver, error) {
			if token == "" {
				return open(ctx, nil)
			}
			return open(ctx, &http.Client{Transport: TokenHeaderTransport{
				Base: http.DefaultTransport, Scheme: scheme, Hosts: hosts,
				Headers: map[string]string{header: prefix + token},
			}})
		},
	}
}

// The native Drivers own content requests. This transport is for their
// declared credential recipients only, including on redirected requests.
type TokenHeaderTransport struct {
	Base    http.RoundTripper
	Scheme  string
	Hosts   []string
	Headers map[string]string
	Bearer  func(context.Context) (string, error)
}

func (t TokenHeaderTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	allowed := slices.ContainsFunc(t.Hosts, func(host string) bool { return strings.EqualFold(host, r.URL.Host) })
	if r.URL.Scheme != t.Scheme || !allowed || r.URL.User != nil || r.URL.Opaque != "" || (r.Host != "" && !strings.EqualFold(r.Host, r.URL.Host)) {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, core.Fail(core.PermissionDenied, errors.New("untrusted account API destination"))
	}
	req := r.Clone(r.Context())
	for name, value := range t.Headers {
		req.Header.Set(name, value)
	}
	if t.Bearer != nil {
		token, err := t.Bearer(req.Context())
		if err != nil {
			if req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return t.Base.RoundTrip(req)
}
