package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/alipan"
	"github.com/nostalume/mountweave/drivers/alipan/credential"
)

const (
	aliPanReadScope  = "file:all:read"
	aliPanWriteScope = "file:all:write"
	aliPanPartSize   = 8 << 20
)

var aliPanAuthorization = managedAuthorization{
	provider: "alipan", label: "Aliyun Drive", method: "scan", exclusive: true,
	verifyRoots: true, managedOnly: true,
	client: aliPanApp, authorize: authorizeAliPan, scopes: aliPanScopes,
	chooseScopes: chooseAliPanScopes, chooseRoot: chooseAliPanRoot,
	read: readAliPanAuthorization, probe: probeAliPanAuthorization,
}

type aliPanDriveInfo struct {
	Account  string `json:"user_id"`
	Name     string `json:"name"`
	Default  string `json:"default_drive_id"`
	Resource string `json:"resource_drive_id"`
	Backup   string `json:"backup_drive_id"`
}

func (d aliPanDriveInfo) hasDrive(id string) bool {
	return id != "" && (id == d.Default || id == d.Resource || id == d.Backup)
}

func aliPanScopeSet(write bool) []string {
	scopes := []string{"user:base", aliPanReadScope}
	if write {
		scopes = append(scopes, aliPanWriteScope)
	}
	return scopes
}

func aliPanScopes(raw []byte) ([]string, error) {
	var opts alipan.Options
	if len(raw) > 0 {
		if err := decodeObject(raw, &opts); err != nil {
			return nil, err
		}
	}
	return aliPanScopeSet(opts.PartSize > 0), nil
}

func chooseAliPanScopes(ask askField) ([]string, error) {
	mode, err := askChoice(ask, "Requested Aliyun Drive access (read or read_write)", "read", "read", "read_write")
	if err != nil {
		return nil, err
	}
	return aliPanScopeSet(mode == "read_write"), nil
}

func aliPanApp(ask askField, previous oauthClient) (oauthClient, error) {
	id, err := askAppID(ask, previous.ClientID, "MWEAVE_ALIPAN_APP_ID", "Approved Aliyun Drive App ID")
	if err != nil {
		return oauthClient{}, err
	}
	if id == "" {
		return oauthClient{}, &problem{Code: "interaction_required",
			Message:    "Aliyun Drive scan authorization requires an approved App ID.",
			NextAction: "Set MWEAVE_ALIPAN_APP_ID or enter the approved App ID."}
	}
	return oauthClient{ClientID: id}, nil
}

func authorizeAliPan(ctx context.Context, app oauthClient, scopes []string, _ authorizationIntent, out io.Writer) (oauthGrant, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	verifier, challenge := pkce()
	var device struct {
		Code string `json:"qrCodeUrl"`
		ID   string `json:"sid"`
	}
	fields := map[string]any{
		"client_id": app.ClientID, "scopes": scopes,
		"code_challenge": challenge, "code_challenge_method": "S256",
	}
	if err := requestAliPan(ctx, http.MethodPost, "/oauth/authorize/qrcode", fields, "", &device); err != nil {
		return oauthGrant{}, err
	}
	link, err := url.Parse(device.Code)
	if err != nil || link.Scheme != "https" || link.User != nil || (link.Port() != "" && link.Port() != "443") ||
		!aliPanAuthorizationHost(link.Hostname()) || !validAliPanSession(device.ID) {
		return oauthGrant{}, &problem{Code: string(core.InvalidUpstreamResponse), Message: "Aliyun Drive did not return a usable QR code.",
			NextAction: "Check that the App ID is approved for Aliyun Drive Open scan authorization."}
	}
	closeQR, err := showAuthorizationQR(ctx, out, "Aliyun Drive", device.Code)
	if err != nil {
		return oauthGrant{}, err
	}
	defer closeQR()

	path := "/oauth/qrcode/" + device.ID + "/status"
	for {
		var state struct {
			Status string `json:"status"`
			Code   string `json:"authCode"`
		}
		if err := requestAliPan(ctx, http.MethodGet, path, nil, "", &state); err != nil {
			return oauthGrant{}, err
		}
		switch state.Status {
		case "LoginSuccess":
			if state.Code == "" {
				return oauthGrant{}, core.Fail(core.InvalidUpstreamResponse, nil)
			}
			return finishAliPanAuthorization(ctx, app, scopes, state.Code, verifier)
		case "WaitLogin", "ScanSuccess":
		case "QRCodeExpired":
			return oauthGrant{}, &problem{Code: "interaction_required",
				Message: "The Aliyun Drive authorization code expired.", NextAction: "Run mount setup again."}
		default:
			return oauthGrant{}, core.Fail(core.InvalidUpstreamResponse, nil)
		}
		select {
		case <-ctx.Done():
			return oauthGrant{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func aliPanAuthorizationHost(host string) bool {
	host = strings.ToLower(host)
	return host == "alipan.com" || strings.HasSuffix(host, ".alipan.com") ||
		host == "aliyundrive.com" || strings.HasSuffix(host, ".aliyundrive.com")
}

func validAliPanSession(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

func finishAliPanAuthorization(ctx context.Context, app oauthClient, scopes []string, code, verifier string) (oauthGrant, error) {
	token, err := exchangeAliPan(ctx, map[string]string{
		"client_id": app.ClientID, "grant_type": "authorization_code", "code": code, "code_verifier": verifier,
	})
	if err != nil {
		return oauthGrant{}, err
	}
	var permissions struct {
		Account string `json:"id"`
		Scopes  []struct {
			Scope string `json:"scope"`
		} `json:"scopes"`
	}
	if err := requestAliPan(ctx, http.MethodGet, "/oauth/users/scopes", nil, token.Access, &permissions); err != nil {
		return oauthGrant{}, err
	}
	g := oauthGrant{Account: permissions.Account, Access: token.Access, Refresh: token.Refresh, Expires: token.ExpiresAt}
	for _, permission := range permissions.Scopes {
		g.Scopes = append(g.Scopes, permission.Scope)
	}
	g.Scopes = oauthScopeSet(strings.Join(g.Scopes, " "))
	if g.Account == "" || !g.covers(scopes) {
		return oauthGrant{}, authorizationScopeConflict()
	}
	drives, err := fetchAliPanDrives(ctx, g.Account, token.Access)
	if err != nil {
		return oauthGrant{}, err
	}
	g.aliPanDrives = &drives
	g.Label = aliPanGrantLabel(drives.Name, g)
	return g, nil
}

func aliPanGrantLabel(name string, g oauthGrant) string {
	if name == "" {
		name = g.Account
	}
	if slices.Contains(g.Scopes, aliPanWriteScope) {
		return name + " (read/write authorization)"
	}
	return name + " (read authorization)"
}

func fetchAliPanDrives(ctx context.Context, account, access string) (aliPanDriveInfo, error) {
	var drives aliPanDriveInfo
	if err := requestAliPan(ctx, http.MethodPost, "/adrive/v1.0/user/getDriveInfo", nil, access, &drives); err != nil {
		return aliPanDriveInfo{}, err
	}
	if drives.Account == "" || drives.Account != account || drives.Default == "" && drives.Resource == "" && drives.Backup == "" {
		return aliPanDriveInfo{}, authorizationScopeConflict()
	}
	return drives, nil
}

func requestAliPan(ctx context.Context, method, path string, fields any, access string, output any) error {
	var body io.Reader
	if fields != nil {
		data, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, alipan.Endpoint+path, body)
	if err != nil {
		return core.Fail(core.InvalidRequest, nil)
	}
	if fields != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	status, err := readOAuthResponse(req, output)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return &problem{Code: "interaction_required", Message: "Aliyun Drive rejected the application or authorization.",
			NextAction: "Check the approved App ID and its access, then authorize again."}
	case http.StatusTooManyRequests:
		return core.Fail(core.UpstreamThrottled, nil)
	default:
		return core.Fail(core.UpstreamFailure, fmt.Errorf("Aliyun Drive returned HTTP %d", status))
	}
}

func exchangeAliPan(ctx context.Context, fields map[string]string) (credential.Token, error) {
	var reply oauthTokenReply
	if err := requestAliPan(ctx, http.MethodPost, "/oauth/access_token", fields, "", &reply); err != nil {
		return credential.Token{}, err
	}
	token := credential.Token{Refresh: reply.Refresh}
	if reply.Refresh == "" {
		return token, &problem{Code: "interaction_required", Message: "Aliyun Drive did not issue a renewable authorization.",
			NextAction: "Use an approved app that supports direct refresh."}
	}
	if !reply.validAccess() {
		return token, core.Fail(core.InvalidUpstreamResponse, nil)
	}
	token.Access, token.ExpiresAt = reply.Access, reply.expiry()
	return token, nil
}

func chooseAliPanRoot(ctx context.Context, raw json.RawMessage, ref *credentialRef, fresh *oauthGrant, scopes []string, ask askField, switching bool) (json.RawMessage, error) {
	var opts alipan.Options
	if len(raw) != 0 {
		if err := decodeObject(raw, &opts); err != nil {
			return nil, err
		}
	}
	if switching {
		opts.DriveID, opts.RootID = "", ""
	}
	var drives aliPanDriveInfo
	if fresh != nil && fresh.aliPanDrives != nil {
		drives = *fresh.aliPanDrives
	} else {
		lease, record, tokens, err := acquireAliPanAuthorization(ref.Ref)
		if err != nil {
			return nil, err
		}
		access, err := tokens.Access(ctx)
		if err == nil {
			drives, err = fetchAliPanDrives(ctx, record.Account, access)
		}
		if err := errors.Join(err, tokens.Close(), lease.lock.Close()); err != nil {
			return nil, err
		}
	}
	if opts.DriveID != "" && !drives.hasDrive(opts.DriveID) {
		return nil, authorizationScopeConflict()
	}
	if opts.DriveID == "" {
		drive, err := pickAliPanDrive(drives, ask)
		if err != nil {
			return nil, err
		}
		opts.DriveID = drive
	}
	if err := guideAliPanRoot(&opts, ask); err != nil {
		return nil, err
	}
	if slices.Contains(scopes, aliPanWriteScope) && opts.PartSize == 0 {
		opts.PartSize, opts.MaxBufferedBytes = aliPanPartSize, aliPanPartSize
	}
	return json.Marshal(opts)
}

func pickAliPanDrive(drives aliPanDriveInfo, ask askField) (string, error) {
	choices := []struct{ label, id string }{
		{"resource", drives.Resource}, {"default", drives.Default}, {"backup", drives.Backup},
	}
	displayed := make(map[string]string, len(choices))
	var labels []string
	seen := make(map[string]bool, len(choices))
	for _, drive := range choices {
		if drive.id != "" && !seen[drive.id] {
			labels = append(labels, drive.label)
			seen[drive.id] = true
			displayed[drive.label] = drive.id
		}
	}
	if len(labels) == 0 {
		return "", core.Fail(core.InvalidUpstreamResponse, nil)
	}
	if len(labels) == 1 {
		return displayed[labels[0]], nil
	}
	name, err := askChoice(ask, "Drive ("+strings.Join(labels, ", ")+")", labels[0], labels...)
	if err != nil {
		return "", err
	}
	return displayed[name], nil
}

func probeAliPanAuthorization(ctx context.Context, m mountRecord, fresh oauthGrant) error {
	var opts alipan.Options
	if err := decodeObject(m.Options, &opts); err != nil {
		return err
	}
	if fresh.aliPanDrives == nil || !fresh.aliPanDrives.hasDrive(opts.DriveID) {
		return authorizationScopeConflict()
	}
	// Verification uses the freshly issued access token without claiming a second
	// refresh lease or publishing credentials before the existing roots are checked.
	unavailable := core.Fail(core.UpstreamAccessDenied, nil)
	tokens, err := credential.New(aliPanCheckpoint(fresh), func(context.Context, string) (credential.Token, error) {
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
