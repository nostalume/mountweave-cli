package cliapp

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type BrowserAuthorization struct {
	ManagedAuthorization
	AuthorizeURL, TokenURL                       string
	ListenAddress                                string
	RedirectURI                                  string // Empty selects the listener's bound loopback address.
	OfflineParam                                 string
	FirstPrompt, SwitchPrompt, ReauthorizePrompt string
	Identity                                     func(context.Context, OAuthTokenReply) (account, label string, err error)
}

type authorizationCode struct {
	code, verifier, redirectURI string
}

var LaunchOAuthBrowser = openBrowser

var ListenOAuthCallback = func(ctx context.Context, address string) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp4", address)
}

func (p *BrowserAuthorization) browserCode(ctx context.Context, app OAuthClient, scopes []string, intent AuthorizationIntent, out io.Writer) (authorizationCode, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	listener, err := ListenOAuthCallback(ctx, p.ListenAddress)
	if err != nil {
		if ctx.Err() != nil {
			return authorizationCode{}, ctx.Err()
		}
		return authorizationCode{}, &Problem{Code: "interaction_required", Message: "The local authorization callback could not start.",
			NextAction: "Check the local callback address " + p.ListenAddress + " and retry."}
	}
	defer listener.Close()
	redirect := p.RedirectURI
	if redirect == "" {
		redirect = "http://" + listener.Addr().String() + "/callback"
	}
	callbackURL, err := url.Parse(redirect)
	if err != nil {
		return authorizationCode{}, err
	}
	state := rand.Text()
	verifier, challenge := Pkce()
	query := url.Values{
		"client_id": {app.ClientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"state": {state}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "scope": {strings.Join(scopes, " ")},
		p.OfflineParam: {"offline"},
	}
	prompt := p.FirstPrompt
	switch intent {
	case authorizeSwitch:
		prompt = p.SwitchPrompt
	case authorizeAgain:
		prompt = p.ReauthorizePrompt
	}
	if prompt != "" {
		query.Set("prompt", prompt)
	}
	target := p.AuthorizeURL + "?" + query.Encode()
	type callback struct{ code, failure string }
	result := make(chan callback, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q, err := url.ParseQuery(r.URL.RawQuery)
			code, failure := q.Get("code"), q.Get("error")
			hasCode := len(q["code"]) == 1 && code != "" && len(q["error"]) == 0
			hasFailure := len(q["error"]) == 1 && failure != "" && len(q["code"]) == 0
			if err != nil || r.Method != http.MethodGet || r.URL.Path != callbackURL.Path || r.Host != callbackURL.Host ||
				len(q["state"]) != 1 || q.Get("state") != state || !(hasCode || hasFailure) {
				http.Error(w, "Invalid authorization callback.", http.StatusBadRequest)
				return
			}
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Cache-Control", "no-store")
			if code == "" {
				io.WriteString(w, "Access was not granted. Return to mweave.")
			} else {
				io.WriteString(w, "The authorization service responded. Return to mweave to continue.")
			}
			// Flush the browser reply before the caller closes the listener.
			_ = http.NewResponseController(w).Flush()
			select {
			case result <- callback{code, failure}:
			default:
			}
		}),
	}
	defer server.Close()
	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(listener) }()
	if _, err := fmt.Fprintf(out, "Complete %s authorization in your browser.\n", p.Label); err != nil {
		return authorizationCode{}, err
	}
	launched := make(chan error, 1)
	launch := LaunchOAuthBrowser
	go func() { launched <- launch(ctx, target) }()
	for {
		select {
		case err := <-launched:
			launched = nil
			if err != nil {
				if _, err := fmt.Fprintf(out, "Open this URL in a browser on this computer: %s\n", target); err != nil {
					return authorizationCode{}, err
				}
			}
		case reply := <-result:
			if err := ctx.Err(); err != nil {
				return authorizationCode{}, err
			}
			if reply.failure == "access_denied" {
				return authorizationCode{}, context.Canceled
			}
			if reply.failure != "" {
				return authorizationCode{}, &Problem{Code: "interaction_required", Message: p.Label + " could not complete browser authorization.",
					NextAction: "Check the app registration, redirect URI and permissions, then retry."}
			}
			return authorizationCode{reply.code, verifier, redirect}, nil
		case <-ctx.Done():
			return authorizationCode{}, ctx.Err()
		case err := <-stopped:
			return authorizationCode{}, err
		}
	}
}

func NewBrowserAuthorization(p BrowserAuthorization) *BrowserAuthorization {
	p.Authorize, p.Renew = p.authorize, p.renew
	return &p
}
