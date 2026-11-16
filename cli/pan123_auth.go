package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	core "github.com/nostalume/mountweave"
)

var pan123Authorization = managedAuthorization{
	provider: "pan123", label: "123 Open", method: "app", appCredentials: true, verifyRoots: true,
	client: pan123App, authorize: authorize123,
}

func pan123App(ask askField, previous oauthClient) (oauthClient, error) {
	id, err := ask("123 Open Client ID", previous.ClientID)
	if err != nil {
		return oauthClient{}, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return oauthClient{}, &problem{Code: "interaction_required", Message: "123 Open requires a Client ID and Client Secret.",
			NextAction: "Apply through the 123 Open platform or choose reference for an existing access token."}
	}
	current := ""
	if previous.SecretRef.valid() {
		current = "keep"
	}
	value, err := ask("Client Secret reference (env:NAME or file:PATH)", current)
	if err != nil {
		return oauthClient{}, err
	}
	ref := previous.SecretRef
	if value != "keep" || current == "" {
		parsed, err := promptReference(value)
		if err != nil {
			return oauthClient{}, err
		}
		ref = *parsed
	}
	if ref.Kind != "env" && ref.Kind != "file" {
		return oauthClient{}, invalidConfig("Use an env or file reference for the 123 Open Client Secret.")
	}
	return oauthClient{ClientID: id, SecretRef: ref}, nil
}

func authorize123(ctx context.Context, app oauthClient, _ []string, _ authorizationIntent, _ io.Writer) (oauthGrant, error) {
	g := oauthGrant{oauthClient: app}
	err := renew123(ctx, &g)
	return g, err
}

func renew123(ctx context.Context, g *oauthGrant) error {
	secret, err := loadCredential(&g.SecretRef, "")
	if err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		ID     string `json:"clientID"`
		Secret string `json:"clientSecret"`
	}{g.ClientID, secret})
	if err != nil {
		return err
	}
	defer clear(body)
	var token struct {
		Access  string    `json:"accessToken"`
		Expires time.Time `json:"expiredAt"`
	}
	if err := request123Auth(ctx, http.MethodPost, "/api/v1/access_token", bytes.NewReader(body), "", &token); err != nil {
		return err
	}
	if token.Access == "" || !time.Now().Before(token.Expires) {
		return core.Fail(core.InvalidUpstreamResponse, nil)
	}

	var user struct {
		ID   int64  `json:"uid"`
		Name string `json:"nickname"`
	}
	if err := request123Auth(ctx, http.MethodGet, "/api/v1/user/info", nil, token.Access, &user); err != nil {
		return err
	}
	if user.ID <= 0 {
		return core.Fail(core.InvalidUpstreamResponse, nil)
	}
	id := strconv.FormatInt(user.ID, 10)
	if g.Account != "" && g.Account != id {
		return &problem{Code: "credential_scope_conflict", Message: "The 123 Open application credentials returned a different account.",
			NextAction: "Restore credentials for the original account or use mount edit to switch this mount."}
	}
	g.Account, g.Label, g.Access, g.Expires = id, id, token.Access, token.Expires
	if user.Name != "" {
		g.Label = user.Name + " (" + id + ")"
	}
	return nil
}

func request123Auth(ctx context.Context, method, route string, body io.Reader, access string, data any) error {
	req, err := http.NewRequestWithContext(ctx, method, "https://"+pan123ControlHost+route, body)
	if err != nil {
		return err
	}
	req.Header.Set("Platform", "open_platform")
	req.Header.Set("Content-Type", "application/json")
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	reply := struct {
		Code *int `json:"code"`
		Data any  `json:"data"`
	}{Data: data}
	status, err := readOAuthResponse(req, &reply)
	if err != nil {
		return err
	}
	if status == http.StatusTooManyRequests {
		return core.Fail(core.UpstreamThrottled, nil)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return &problem{Code: "interaction_required", Message: "123 Open denied authorization.",
			NextAction: "Check the application credentials and permissions, then retry."}
	}
	if status != http.StatusOK {
		return core.Fail(core.UpstreamFailure, fmt.Errorf("123 Open authorization returned HTTP %d", status))
	}
	if reply.Code == nil {
		return core.Fail(core.InvalidUpstreamResponse, nil)
	}
	if *reply.Code != 0 {
		return &problem{Code: "upstream_failure", Message: fmt.Sprintf("123 Open rejected the authorization request (code %d).", *reply.Code),
			NextAction: "Check the application credentials, permissions and service limits."}
	}
	if reply.Data == nil {
		return core.Fail(core.InvalidUpstreamResponse, nil)
	}
	return nil
}
