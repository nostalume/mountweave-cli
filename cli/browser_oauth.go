package cli

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

type browserAuthorization struct {
	managedAuthorization
	authorizeURL, tokenURL                       string
	listenAddress                                string
	redirectURI                                  string // Empty selects the listener's bound loopback address.
	offlineParam                                 string
	firstPrompt, switchPrompt, reauthorizePrompt string
	identity                                     func(context.Context, oauthTokenReply) (account, label string, err error)
}

type authorizationCode struct {
	code, verifier, redirectURI string
}

var launchOAuthBrowser = openBrowser

var listenOAuthCallback = func(ctx context.Context, address string) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp4", address)
}

func (p *browserAuthorization) browserCode(ctx context.Context, app oauthClient, scopes []string, intent authorizationIntent, out io.Writer) (authorizationCode, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	listener, err := listenOAuthCallback(ctx, p.listenAddress)
	if err != nil {
		if ctx.Err() != nil {
			return authorizationCode{}, ctx.Err()
		}
		return authorizationCode{}, &problem{Code: "interaction_required", Message: "The local authorization callback could not start.",
			NextAction: "Check the local callback address " + p.listenAddress + " and retry."}
	}
	defer listener.Close()
	redirect := p.redirectURI
	if redirect == "" {
		redirect = "http://" + listener.Addr().String() + "/callback"
	}
	callbackURL, err := url.Parse(redirect)
	if err != nil {
		return authorizationCode{}, err
	}
	state := rand.Text()
	verifier, challenge := pkce()
	query := url.Values{
		"client_id": {app.ClientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"state": {state}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "scope": {strings.Join(scopes, " ")},
		p.offlineParam: {"offline"},
	}
	prompt := p.firstPrompt
	switch intent {
	case authorizeSwitch:
		prompt = p.switchPrompt
	case authorizeAgain:
		prompt = p.reauthorizePrompt
	}
	if prompt != "" {
		query.Set("prompt", prompt)
	}
	target := p.authorizeURL + "?" + query.Encode()
	type callback struct{ code, failure string }
	result := make(chan callback, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q, err := url.ParseQuery(r.URL.RawQuery)
			if err != nil || r.Method != http.MethodGet || r.URL.Path != callbackURL.Path || r.Host != callbackURL.Host ||
				len(q["state"]) != 1 || q.Get("state") != state {
				http.Error(w, "Invalid authorization callback.", http.StatusBadRequest)
				return
			}
			code := q.Get("code")
			if !(len(q["code"]) == 1 && code != "" && len(q["error"]) == 0 ||
				len(q["error"]) == 1 && q.Get("error") != "" && len(q["code"]) == 0) {
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
			case result <- callback{code, q.Get("error")}:
			default:
			}
		}),
	}
	defer server.Close()
	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(listener) }()
	if _, err := fmt.Fprintf(out, "Complete %s authorization in your browser.\n", p.label); err != nil {
		return authorizationCode{}, err
	}
	launched := make(chan error, 1)
	launch := launchOAuthBrowser
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
				return authorizationCode{}, &problem{Code: "interaction_required", Message: p.label + " could not complete browser authorization.",
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
