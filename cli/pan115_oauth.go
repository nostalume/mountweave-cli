package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/alipan/credential"
	"github.com/nostalume/mountweave/drivers/pan115"
)

const pan115Passport = "https://passportapi.115.com/open/"

var pan115Authorization = managedAuthorization{
	provider: "pan115", label: "115 Open", method: "scan", exclusive: true, verifyRoots: true,
	client: pan115App, authorize: authorize115,
}

func pan115App(ask askField, previous oauthClient) (oauthClient, error) {
	app, err := askAppID(ask, previous.ClientID, "MWEAVE_115_APP_ID", "115 Open App ID")
	if err != nil {
		return oauthClient{}, err
	}
	if app == "" {
		return oauthClient{}, &problem{Code: "interaction_required", Message: "115 Open scan authorization requires an approved App ID.",
			NextAction: "Use an approved 115 Open App ID (open.115.com), or choose reference if you already have an access token."}
	}
	return oauthClient{ClientID: app}, nil
}

func authorize115(ctx context.Context, app oauthClient, _ []string, _ authorizationIntent, out io.Writer) (oauthGrant, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	verifier, challenge := pkce()
	var device struct {
		UID  string `json:"uid"`
		Time *int64 `json:"time"`
		Sign string `json:"sign"`
		QR   string `json:"qrcode"`
	}
	if err := request115(ctx, pan115Passport+"authDeviceCode", url.Values{
		"client_id": {app.ClientID}, "code_challenge": {challenge}, "code_challenge_method": {"sha256"},
	}, "", &device); err != nil {
		return oauthGrant{}, err
	}
	if device.UID == "" || device.Time == nil || device.Sign == "" || device.QR == "" {
		return oauthGrant{}, core.Fail(core.InvalidUpstreamResponse, nil)
	}
	closeQR, err := showAuthorizationQR(ctx, out, "115", device.QR)
	if err != nil {
		return oauthGrant{}, err
	}
	defer closeQR()

	statusURL := "https://qrcodeapi.115.com/get/status/?" + (url.Values{
		"uid": {device.UID}, "time": {strconv.FormatInt(*device.Time, 10)}, "sign": {device.Sign},
	}).Encode()
	for {
		var state struct{ Status int }
		err := request115(ctx, statusURL, nil, "", &state)
		switch {
		case err != nil && !errors.Is(err, context.DeadlineExceeded):
			return oauthGrant{}, err
		case err == nil && state.Status == 2:
			token, err := token115(ctx, "deviceCodeToToken", url.Values{"uid": {device.UID}, "code_verifier": {verifier}})
			if err != nil {
				return oauthGrant{}, err
			}
			var account struct {
				ID   int64  `json:"user_id"`
				Name string `json:"user_name"`
			}
			if err := request115(ctx, "https://"+pan115ControlHost+"/open/user/info", nil, token.Access, &account); err != nil {
				return oauthGrant{}, err
			}
			if account.ID <= 0 {
				return oauthGrant{}, core.Fail(core.InvalidUpstreamResponse, nil)
			}
			id := strconv.FormatInt(account.ID, 10)
			return oauthGrant{Account: id, Label: account.Name + " (" + id + ")", Access: token.Access, Refresh: token.Refresh, Expires: token.ExpiresAt}, nil
		case err == nil && state.Status != 0 && state.Status != 1:
			return oauthGrant{}, core.Fail(core.InvalidUpstreamResponse, nil)
		}
		// The endpoint long-polls. Also bound the rate of immediate replies.
		select {
		case <-ctx.Done():
			return oauthGrant{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func token115(ctx context.Context, operation string, fields url.Values) (credential.Token, error) {
	var reply oauthTokenReply
	if err := request115(ctx, pan115Passport+operation, fields, "", &reply); err != nil {
		return credential.Token{}, err
	}
	if reply.Refresh == "" {
		return credential.Token{}, core.Fail(core.InvalidUpstreamResponse, nil)
	}
	// A known new refresh token must be saved even when its access token is unusable.
	token := credential.Token{Refresh: reply.Refresh}
	if reply.Access == "" || reply.Lifetime <= 0 || reply.Lifetime > (1<<63-1)/int64(time.Second) {
		return token, core.Fail(core.InvalidUpstreamResponse, nil)
	}
	token.Access, token.ExpiresAt = reply.Access, reply.expiry()
	return token, nil
}

func request115(ctx context.Context, endpoint string, fields url.Values, access string, data any) error {
	method, body := http.MethodGet, io.Reader(nil)
	if fields != nil {
		method, body = http.MethodPost, strings.NewReader(fields.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if fields != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	var reply struct {
		State json.RawMessage
		Code  int
		Data  json.RawMessage
	}
	status, err := readOAuthResponse(req, &reply)
	if err != nil {
		return err
	}
	switch reply.Code {
	case 40140100, 40140106, 40140107, 40140108, 40140109, 40140110, 40140118:
		return &problem{Code: "interaction_required", Message: fmt.Sprintf("115 Open rejected the application (%d).", reply.Code),
			NextAction: "Check the App ID and its approval in the 115 Open platform."}
	case 40140114, 40140115, 40140116, 40140119, 40140120, 40140137:
		return &problem{Code: "interaction_required", Message: fmt.Sprintf("115 Open rejected the refresh credential (%d).", reply.Code),
			NextAction: "Run mweave mount edit and choose reauthorize."}
	case 40140117:
		return core.Fail(core.UpstreamThrottled, nil)
	}
	if status == http.StatusOK && reply.Code == 0 && (string(reply.State) == "1" || string(reply.State) == "true") {
		if len(reply.Data) == 0 || json.Unmarshal(reply.Data, data) != nil {
			return core.Fail(core.InvalidUpstreamResponse, nil)
		}
		return nil
	}
	if status == http.StatusOK && (string(reply.State) == "0" || string(reply.State) == "false") &&
		strings.HasPrefix(endpoint, "https://qrcodeapi.115.com/") {
		return &problem{Code: "interaction_required", Message: "The 115 authorization code is no longer valid.",
			NextAction: "Start the interactive authorization again."}
	}
	return core.Fail(core.UpstreamFailure, fmt.Errorf("115 authorization returned HTTP %d and code %d", status, reply.Code))
}

type managed115 struct {
	*pan115.Driver
	closeAuthorization func() error
}

func (d *managed115) Close() error {
	return errors.Join(d.Driver.Close(), d.closeAuthorization())
}

func openManaged115(ctx context.Context, opts pan115.Options, id string) (core.Driver, error) {
	lease, err := acquireGrant(id, false)
	if err != nil {
		return nil, err
	}
	g, err := pan115Authorization.readGrant(lease.path, id)
	if err != nil {
		return nil, errors.Join(err, lease.lock.Close())
	}
	checkpoint := credential.Record{
		Token: credential.Token{Access: g.Access, Refresh: g.Refresh, ExpiresAt: g.Expires}, Pending: g.Pending,
	}
	tokens, err := credential.New(checkpoint, func(ctx context.Context, refresh string) (credential.Token, error) {
		return token115(ctx, "refreshToken", url.Values{"refresh_token": {refresh}})
	}, func(ctx context.Context, checkpoint credential.Record) error {
		g.Access, g.Refresh, g.Expires = checkpoint.Token.Access, checkpoint.Token.Refresh, checkpoint.Token.ExpiresAt
		g.Pending = checkpoint.Pending
		if !checkpoint.Pending {
			// Once the provider rotated the credential, publish the known pair even during shutdown.
			ctx = context.WithoutCancel(ctx)
		}
		return g.save(ctx, lease, true)
	})
	if err != nil {
		if errors.Is(err, credential.ErrRefreshUncertain) {
			err = &problem{Code: "interaction_required", Message: "The 115 Open refresh outcome could not be confirmed.",
				NextAction: "Run mweave mount edit and choose reauthorize."}
		}
		return nil, errors.Join(err, lease.lock.Close())
	}
	closeAuthorization := sync.OnceValue(func() error { return errors.Join(tokens.Close(), lease.lock.Close()) })
	opts.Client = &http.Client{Transport: tokenHeaderTransport{
		base: http.DefaultTransport, scheme: "https", hosts: []string{pan115ControlHost}, bearer: tokens.Access,
	}}
	d, err := pan115.Open(ctx, opts)
	if d == nil {
		return nil, errors.Join(err, closeAuthorization())
	}
	return &managed115{Driver: d, closeAuthorization: closeAuthorization}, err
}
