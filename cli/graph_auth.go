package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	core "github.com/nostalume/mountweave"
)

const graphControlHost = "graph.microsoft.com"
const graphDeviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

var graphAuthorization = managedAuthorization{
	provider: "graphdrive", label: "Microsoft Graph", method: "device", verifyRoots: true,
	scopes:    func([]byte) ([]string, error) { return graphDeviceScopes(), nil },
	client:    graphDeviceApp,
	authorize: authorizeGraph,
}

func graphDeviceScopes() []string { return []string{"Files.Read", "User.Read"} }

func graphDeviceApp(ask askField, previous oauthClient) (oauthClient, error) {
	id, err := ask("Microsoft Entra Application (client) ID", previous.ClientID)
	if err != nil {
		return oauthClient{}, err
	}
	tenant := previous.Tenant
	if tenant == "" {
		tenant = "consumers"
	}
	tenant, err = ask("Microsoft Entra Directory (tenant) ID or consumers for personal accounts", tenant)
	if err != nil {
		return oauthClient{}, err
	}
	app, err := graphUUID(strings.TrimSpace(id))
	if err != nil {
		return oauthClient{}, invalidConfig("Enter the Microsoft Entra application UUID.")
	}
	tenant, err = graphTenant(strings.TrimSpace(tenant))
	if err != nil {
		return oauthClient{}, invalidConfig("Enter a directory tenant UUID, or consumers for personal accounts.")
	}
	return oauthClient{ClientID: app, Tenant: tenant}, nil
}

func graphUUID(raw string) (string, error) {
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil || len(raw) != 36 {
		return "", core.Fail(core.InvalidRequest, nil)
	}
	return id.String(), nil
}

func graphTenant(raw string) (string, error) {
	if strings.EqualFold(raw, "consumers") || strings.EqualFold(raw, "9188040d-6c67-4c5b-b112-36a304b66dad") {
		return "consumers", nil
	}
	return graphUUID(raw)
}

func graphOAuthEndpoint(app oauthClient) (string, error) {
	tenant, err := graphTenant(app.Tenant)
	if err != nil {
		return "", grantUnavailable()
	}
	return "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/", nil
}

type graphDeviceCode struct {
	DeviceCode   string `json:"device_code"`
	UserCode     string `json:"user_code"`
	Verification string `json:"verification_uri"`
	Expires      int64  `json:"expires_in"`
	Interval     int64  `json:"interval"`
	Error        string `json:"error"`
}

func authorizeGraph(ctx context.Context, app oauthClient, scopes []string, _ authorizationIntent, out io.Writer) (oauthGrant, error) {
	endpoint, err := graphOAuthEndpoint(app)
	if err != nil {
		return oauthGrant{}, err
	}
	var device graphDeviceCode
	start := time.Now()
	status, err := requestOAuthForm(ctx, endpoint+"devicecode", app, url.Values{
		"scope": {strings.Join(scopes, " ") + " offline_access"},
	}, &device)
	if err != nil {
		return oauthGrant{}, err
	}
	if status != http.StatusOK {
		return oauthGrant{}, graphAuthorizationError(status, device.Error)
	}
	verify, err := url.Parse(device.Verification)
	if err != nil || verify.Scheme != "https" || verify.User != nil || verify.Opaque != "" ||
		verify.RawQuery != "" || verify.ForceQuery || verify.Fragment != "" || !graphVerificationHost(verify.Host) ||
		device.DeviceCode == "" || device.UserCode == "" ||
		device.Expires <= 0 || device.Expires > (1<<63-1)/int64(time.Second) || device.Interval < 0 {
		return oauthGrant{}, core.Fail(core.InvalidUpstreamResponse, nil)
	}
	lifetime := time.Duration(device.Expires) * time.Second
	pollCtx, cancel := context.WithDeadlineCause(ctx, start.Add(lifetime), graphDeviceExpired())
	defer cancel()
	if err := context.Cause(pollCtx); err != nil {
		return oauthGrant{}, err
	}
	if _, err := fmt.Fprintf(out, "Open %s on a browser you trust and enter code %q. Confirm only the sign-in you started here.\n", verify, device.UserCode); err != nil {
		return oauthGrant{}, err
	}
	reply, err := pollGraphDevice(pollCtx, endpoint+"token", app, device)
	if err != nil {
		return oauthGrant{}, err
	}
	g := oauthGrant{oauthClient: app, Scopes: scopes}
	if err := acceptGraphToken(ctx, &g, reply); err != nil {
		return oauthGrant{}, err
	}
	return g, nil
}

func graphVerificationHost(host string) bool {
	switch strings.ToLower(host) {
	case "microsoft.com", "www.microsoft.com", "login.microsoftonline.com", "aka.ms":
		return true
	default:
		return false
	}
}

func pollGraphDevice(ctx context.Context, endpoint string, app oauthClient, device graphDeviceCode) (oauthTokenReply, error) {
	interval := device.Interval
	if interval == 0 {
		interval = 5
	}
	// Cap waits at the session lifetime. Its deadline still owns cancellation.
	limit := device.Expires
	form := url.Values{"grant_type": {graphDeviceGrant}, "device_code": {device.DeviceCode}}
	for {
		wait := time.Duration(min(interval, limit)) * time.Second
		deadline, _ := ctx.Deadline()
		if time.Until(deadline) <= wait {
			<-ctx.Done()
			return oauthTokenReply{}, context.Cause(ctx)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return oauthTokenReply{}, context.Cause(ctx)
		case <-timer.C:
		}
		if !time.Now().Before(deadline) {
			<-ctx.Done()
			return oauthTokenReply{}, context.Cause(ctx)
		}
		var reply oauthTokenReply
		status, err := requestOAuthForm(ctx, endpoint, app, form, &reply)
		if cause := context.Cause(ctx); cause != nil {
			return oauthTokenReply{}, cause
		}
		if errors.Is(err, context.DeadlineExceeded) {
			interval = min(interval, limit) * 2
			continue
		}
		if err != nil {
			return oauthTokenReply{}, err
		}
		if status == http.StatusOK {
			return reply, nil
		}
		if status == http.StatusBadRequest {
			switch reply.Error {
			case "authorization_pending":
				continue
			case "slow_down":
				interval = min(interval, limit) + 5
				continue
			}
		}
		return oauthTokenReply{}, graphAuthorizationError(status, reply.Error)
	}
}

func graphDeviceExpired() error {
	return &problem{Code: "interaction_required", Message: "The Microsoft device code expired.",
		NextAction: "Start authorization again to request a new code."}
}

func graphAuthorizationError(status int, code string) error {
	switch code {
	case "access_denied", "authorization_declined":
		return context.Canceled
	case "expired_token", "bad_verification_code":
		return graphDeviceExpired()
	case "invalid_grant", "interaction_required", "consent_required":
		return &problem{Code: "interaction_required", Message: "Microsoft Entra requires a new sign-in.",
			NextAction: "Run mweave mount edit and choose reauthorize."}
	case "invalid_client", "unauthorized_client", "invalid_scope", "invalid_request":
		return &problem{Code: "interaction_required", Message: "Microsoft Entra rejected the application or authorization settings.",
			NextAction: "Check the tenant, public client settings and delegated permissions."}
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return core.Fail(core.UpstreamAccessDenied, nil)
	case http.StatusTooManyRequests:
		return core.Fail(core.UpstreamThrottled, nil)
	default:
		return core.Fail(core.UpstreamFailure, nil)
	}
}

func renewGraph(ctx context.Context, g *oauthGrant) error {
	endpoint, err := graphOAuthEndpoint(g.oauthClient)
	if err != nil {
		return err
	}
	var reply oauthTokenReply
	status, err := requestOAuthForm(ctx, endpoint+"token", g.oauthClient, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {g.Refresh}, "scope": {strings.Join(g.Scopes, " ") + " offline_access"},
	}, &reply)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return graphAuthorizationError(status, reply.Error)
	}
	return acceptGraphToken(ctx, g, reply)
}

// The tenant-specific Entra endpoint and the Graph resource establish the
// binding. The Graph access token stays opaque.
func acceptGraphToken(ctx context.Context, g *oauthGrant, reply oauthTokenReply) error {
	if !reply.validAccess() || (reply.Refresh == "" && g.Refresh == "") {
		return core.Fail(core.InvalidUpstreamResponse, nil)
	}
	granted := strings.Fields(reply.Scope)
	for _, required := range g.Scopes {
		found := false
		for _, scope := range granted {
			if strings.EqualFold(scope, required) || strings.EqualFold(scope, "https://"+graphControlHost+"/"+required) {
				found = true
				break
			}
		}
		if !found {
			return authorizationScopeConflict()
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+graphControlHost+"/v1.0/me?$select=id,displayName", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+reply.Access)
	var me struct {
		ID          string
		DisplayName string
	}
	status, err := readOAuthResponse(req, &me)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return graphAuthorizationError(status, "")
	}
	if me.ID == "" {
		return core.Fail(core.InvalidUpstreamResponse, nil)
	}
	account := g.Tenant + "/" + me.ID
	if g.Account != "" && g.Account != account {
		return &problem{Code: "credential_scope_conflict", Message: "Microsoft Graph returned a different tenant account.",
			NextAction: "Run mweave mount edit and reauthorize the original account."}
	}
	g.Account, g.Label, g.Access, g.Expires = account, me.DisplayName, reply.Access, reply.expiry()
	if g.Label == "" {
		g.Label = me.ID
	}
	g.Label += " (" + account + ")"
	if reply.Refresh != "" {
		g.Refresh = reply.Refresh
	}
	return nil
}
