package stock

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave/drivers/alipan"
	"github.com/nostalume/mountweave/drivers/dropbox"
	"github.com/nostalume/mountweave/drivers/googledrive"
	"github.com/nostalume/mountweave/drivers/graphdrive"
	"github.com/nostalume/mountweave/drivers/pan115"
	"github.com/nostalume/mountweave/drivers/pan123"
	"github.com/nostalume/mountweave/drivers/weiyun"
	qrcode "github.com/skip2/go-qrcode"
)

const (
	aliPanReadScope  = "file:all:read"
	aliPanWriteScope = "file:all:write"
)

var aliPanAuthorization = cliapp.ManagedAuthorization{
	Provider: "alipan", Label: "Aliyun Drive", Method: "scan", Kind: cliapp.RotatingGrant,
	VerifyRoots: true, ManagedOnly: true,
	Client: aliPanApp, Authorize: authorizeAliPan, Scopes: aliPanScopes,
	ChooseScopes: chooseAliPanScopes, ChooseRoot: chooseAliPanRoot,
	Read: readAliPanAuthorization, Probe: probeAliPanAuthorization,
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
	if err := cliapp.DecodeObject(raw, &opts); err != nil {
		return nil, err
	}
	return aliPanScopeSet(opts.PartSize > 0), nil
}

func chooseAliPanScopes(ask cliapp.Prompt) ([]string, error) {
	mode, err := cliapp.AskChoice(ask, "Requested Aliyun Drive access (read or read_write)", "read", "read", "read_write")
	if err != nil {
		return nil, err
	}
	return aliPanScopeSet(mode == "read_write"), nil
}

func aliPanApp(ask cliapp.Prompt, previous cliapp.OAuthClient) (cliapp.OAuthClient, error) {
	id, err := cliapp.AskAppID(ask, previous.ClientID, "MWEAVE_ALIPAN_APP_ID", "Approved Aliyun Drive App ID",
		"Aliyun Drive scan authorization requires an approved App ID.",
		"Set MWEAVE_ALIPAN_APP_ID or enter the approved App ID.")
	return cliapp.OAuthClient{ClientID: id}, err
}

func authorizeAliPan(ctx context.Context, app cliapp.OAuthClient, scopes []string, _ cliapp.AuthorizationIntent, out io.Writer) (cliapp.OAuthGrant, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	auth, err := aliPanOAuth(app).Authorize(ctx, scopes, func(link string) (func(), error) {
		return showAuthorizationQR(ctx, out, "Aliyun Drive", link)
	})
	if err != nil {
		return cliapp.OAuthGrant{}, projectAliPanOAuth(err)
	}
	g := cliapp.OAuthGrant{Account: auth.Drives.Account, Access: auth.Token.Access, Refresh: auth.Token.Refresh,
		Expires: auth.Token.ExpiresAt, Scopes: auth.Scopes, SetupData: auth.Drives}
	g.Label = aliPanGrantLabel(auth.Drives.Name, g)
	return g, nil
}

func projectAliPanOAuth(err error) error {
	var problem *cliapp.Problem
	switch {
	case errors.Is(err, alipan.ErrAuthorizationScope):
		return errors.Join(cliapp.AuthorizationScopeConflict(), err)
	case errors.Is(err, alipan.ErrAuthorizationRejected):
		problem = &cliapp.Problem{Code: "interaction_required", Message: "Aliyun Drive rejected the application or authorization.",
			NextAction: "Check the approved App ID and its access, then authorize again."}
	case errors.Is(err, alipan.ErrAuthorizationExpired):
		problem = &cliapp.Problem{Code: "interaction_required", Message: "The Aliyun Drive authorization code expired.",
			NextAction: "Run mount setup again."}
	case errors.Is(err, alipan.ErrAuthorizationQR):
		problem = &cliapp.Problem{Code: string(core.InvalidUpstreamResponse), Message: "Aliyun Drive did not return a usable QR code.",
			NextAction: "Check that the App ID is approved for Aliyun Drive Open scan authorization."}
	case errors.Is(err, alipan.ErrAuthorizationNotRenewable):
		problem = &cliapp.Problem{Code: "interaction_required", Message: "Aliyun Drive did not issue a renewable authorization.",
			NextAction: "Use an approved app that supports direct refresh."}
	default:
		return err
	}
	return errors.Join(problem, err)
}

func aliPanGrantLabel(name string, g cliapp.OAuthGrant) string {
	name = cmp.Or(name, g.Account)
	if slices.Contains(g.Scopes, aliPanWriteScope) {
		return name + " (read/write authorization)"
	}
	return name + " (read authorization)"
}

func aliPanOAuth(app cliapp.OAuthClient) alipan.OAuth {
	return alipan.OAuth{Client: &http.Client{Timeout: 30 * time.Second}, ClientID: app.ClientID, ClientSecret: app.ClientSecret}
}

var pan115Authorization = cliapp.ManagedAuthorization{
	Provider: "pan115", Label: "115 Open", Method: "scan", Kind: cliapp.RotatingGrant, VerifyRoots: true,
	Client: pan115App, Authorize: authorize115,
}

func pan115App(ask cliapp.Prompt, previous cliapp.OAuthClient) (cliapp.OAuthClient, error) {
	id, err := cliapp.AskAppID(ask, previous.ClientID, "MWEAVE_115_APP_ID", "115 Open App ID",
		"115 Open scan authorization requires an approved App ID.",
		"Use an approved 115 Open App ID (open.115.com), or choose reference if you already have an access token.")
	return cliapp.OAuthClient{ClientID: id}, err
}

func authorize115(ctx context.Context, app cliapp.OAuthClient, _ []string, _ cliapp.AuthorizationIntent, out io.Writer) (cliapp.OAuthGrant, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	verifier, challenge := cliapp.Pkce()
	token, uid, name, err := pan115.AuthorizationClient(cliapp.ReadOAuthResponse).Authorize(ctx, app.ClientID, verifier, challenge,
		func(code string) (func(), error) { return showAuthorizationQR(ctx, out, "115", code) })
	if err != nil {
		return cliapp.OAuthGrant{}, authorization115Error(err)
	}
	id := strconv.FormatInt(uid, 10)
	return cliapp.OAuthGrant{Account: id, Label: name + " (" + id + ")", Access: token.Access, Refresh: token.Refresh, Expires: token.ExpiresAt}, nil
}

func authorization115Error(err error) error {
	switch {
	case errors.Is(err, pan115.ErrApplicationRejected):
		return &cliapp.Problem{Code: "interaction_required", Message: err.Error(), NextAction: "Check the App ID and its approval in the 115 Open platform."}
	case errors.Is(err, pan115.ErrRefreshRejected):
		return &cliapp.Problem{Code: "interaction_required", Message: err.Error(), NextAction: "Choose reauthorize.", Reconnect: true}
	case errors.Is(err, pan115.ErrScanExpired):
		return &cliapp.Problem{Code: "interaction_required", Message: err.Error(), NextAction: "Start the interactive authorization again."}
	}
	return err
}

var pan123Authorization = cliapp.ManagedAuthorization{
	Provider: "pan123", Label: "123 Open", Method: "app", Kind: cliapp.ApplicationGrant, VerifyRoots: true,
	Client: pan123App, Renew: renew123, ReferenceInGrant: true,
}

func pan123App(ask cliapp.Prompt, previous cliapp.OAuthClient) (cliapp.OAuthClient, error) {
	id, err := cliapp.AskAppID(ask, previous.ClientID, "", "123 Open Client ID (approved application)",
		"123 Open requires a Client ID and Client Secret.",
		"Apply through the 123 Open platform or choose reference for an existing access token.")
	if err != nil {
		return cliapp.OAuthClient{}, err
	}
	var prior *cliapp.CredentialRef
	if previous.SecretRef.Valid() {
		prior = &previous.SecretRef
	}
	ref, err := cliapp.AskCredentialReference(ask, "Client Secret reference (env:NAME or file:PATH)", prior, cliapp.PromptReference, func(ref *cliapp.CredentialRef) error {
		if !ref.Valid() || ref.Kind != "env" && ref.Kind != "file" {
			return cliapp.InvalidConfig("Use an env or file reference for the 123 Open Client Secret.")
		}
		return nil
	})
	if err != nil {
		return cliapp.OAuthClient{}, err
	}
	return cliapp.OAuthClient{ClientID: id, SecretRef: *ref}, nil
}

func renew123(ctx context.Context, g *cliapp.OAuthGrant) error {
	secret, err := cliapp.LoadCredential(&g.SecretRef, "")
	if err != nil {
		return err
	}
	grant, err := pan123.Authorize(ctx, nil, g.ClientID, secret, g.Account)
	switch {
	case core.CodeOf(err) == core.PreconditionFailed:
		return &cliapp.Problem{Code: "credential_scope_conflict", Message: "The 123 Open application credentials returned a different account.",
			NextAction: "Restore credentials for the original account or use mount edit to switch this mount."}
	case core.CodeOf(err) == core.UpstreamAccessDenied:
		return &cliapp.Problem{Code: "interaction_required", Message: "123 Open denied authorization.",
			NextAction: "Check the application credentials and permissions, then retry."}
	case core.CodeOf(err) == core.UpstreamFailure && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded):
		return &cliapp.Problem{Code: "upstream_failure", Message: err.Error(), NextAction: "Check the application credentials, permissions and service limits."}
	case err != nil:
		return err
	}
	g.Account, g.Label, g.Access, g.Expires = grant.UID, grant.UID, grant.AccessToken, grant.ExpiresAt
	if grant.Nickname != "" {
		g.Label = grant.Nickname + " (" + grant.UID + ")"
	}
	return nil
}

var graphAuthorization = cliapp.ManagedAuthorization{
	Provider: "graphdrive", Label: "Microsoft Graph", Method: "device", VerifyRoots: true,
	Scopes:    func([]byte) ([]string, error) { return graphdrive.AuthorizationScopes(), nil },
	Client:    graphDeviceApp,
	Authorize: authorizeGraph, Renew: renewGraph,
}

func graphDeviceApp(ask cliapp.Prompt, previous cliapp.OAuthClient) (cliapp.OAuthClient, error) {
	id, err := ask("Microsoft Entra Application (client) ID", previous.ClientID)
	if err != nil {
		return cliapp.OAuthClient{}, err
	}
	tenant, err := ask("Microsoft Entra Directory (tenant) ID or consumers for personal accounts", cmp.Or(previous.Tenant, "consumers"))
	if err != nil {
		return cliapp.OAuthClient{}, err
	}
	app, err := (graphdrive.DeviceApp{ClientID: strings.TrimSpace(id), Tenant: strings.TrimSpace(tenant)}).Normalize()
	if err != nil {
		return cliapp.OAuthClient{}, cliapp.InvalidConfig("Enter an application UUID and a directory tenant UUID, or consumers for personal accounts.")
	}
	return cliapp.OAuthClient{ClientID: app.ClientID, Tenant: app.Tenant}, nil
}

func authorizeGraph(ctx context.Context, app cliapp.OAuthClient, scopes []string, _ cliapp.AuthorizationIntent, out io.Writer) (cliapp.OAuthGrant, error) {
	g := cliapp.OAuthGrant{OAuthClient: app, Scopes: scopes}
	err := exchangeGraph(ctx, &g, func(url, code string) error {
		_, err := fmt.Fprintf(out, "Open %s on a browser you trust and enter code %q. Confirm only the sign-in you started here.\n", url, code)
		return err
	})
	return g, err
}

func renewGraph(ctx context.Context, g *cliapp.OAuthGrant) error { return exchangeGraph(ctx, g, nil) }

func exchangeGraph(ctx context.Context, g *cliapp.OAuthGrant, present func(string, string) error) error {
	app := graphdrive.DeviceApp{ClientID: g.ClientID, Tenant: g.Tenant}
	token, err := app.Exchange(ctx, g.Scopes, graphdrive.Grant{Refresh: g.Refresh, Account: g.Account}, cliapp.ReadOAuthResponse, present)
	switch {
	case err == nil:
		g.Account, g.Label, g.Access, g.Refresh, g.Expires = token.Account, token.Label, token.Access, token.Refresh, token.Expires
	case errors.Is(err, graphdrive.ErrDeviceExpired):
		return &cliapp.Problem{Code: "interaction_required", Message: "The Microsoft device code expired.",
			NextAction: "Start authorization again to request a new code."}
	case errors.Is(err, graphdrive.ErrSignInRequired):
		return &cliapp.Problem{Code: "interaction_required", Message: "Microsoft Entra requires a new sign-in.",
			NextAction: "Choose reauthorize.", Reconnect: true}
	case errors.Is(err, graphdrive.ErrApplicationSettings):
		return cliapp.GrantUnavailable()
	case errors.Is(err, graphdrive.ErrScopeMissing):
		return cliapp.AuthorizationScopeConflict()
	case errors.Is(err, graphdrive.ErrAccountChanged):
		return &cliapp.Problem{Code: "credential_scope_conflict", Message: "Microsoft Graph returned a different tenant account.",
			NextAction: "Run mweave mount edit and reauthorize the original account."}
	case core.CodeOf(err) == core.InvalidRequest:
		return &cliapp.Problem{Code: "interaction_required", Message: "Microsoft Entra rejected the application or authorization settings.",
			NextAction: "Check the tenant, public client settings and delegated permissions."}
	}
	return err
}

// showAuthorizationQR keeps the temporary image available until the scan
// session ends. The terminal remains usable when an image viewer cannot start.
func showAuthorizationQR(ctx context.Context, out io.Writer, app, payload string) (func(), error) {
	qr, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		return nil, core.Fail(core.InvalidUpstreamResponse, nil)
	}
	if _, err := fmt.Fprintf(out, "Scan the QR code in the %s app and approve access.\n", app); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "mweave-authorization-*.png")
	if err == nil {
		name := file.Name()
		if err := errors.Join(qr.Write(384, file), file.Close()); err == nil {
			openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := cliapp.LaunchOAuthBrowser(openCtx, name)
			cancel()
			if ctx.Err() != nil {
				_ = os.Remove(name)
				return nil, ctx.Err()
			}
			if err == nil {
				return func() { _ = os.Remove(name) }, nil
			}
		}
		_ = os.Remove(name)
	}
	_, err = fmt.Fprintf(out, "\x1b[30;107m%s\x1b[0m\n", qr.ToSmallString(true))
	return func() {}, err
}

const dropboxRedirect = "http://localhost:53682/callback"

var dropboxAuthorization = cliapp.NewBrowserAuthorization(cliapp.BrowserAuthorization{
	ManagedAuthorization: cliapp.ManagedAuthorization{
		Provider: "dropbox", Label: "Dropbox", Method: "browser",
		Scopes: dropboxMountScopes, Client: dropboxOAuthClient,
	},
	AuthorizeURL: "https://www.dropbox.com/oauth2/authorize", TokenURL: "https://" + dropbox.ControlHost + "/oauth2/token",
	ListenAddress: "127.0.0.1:53682", RedirectURI: dropboxRedirect,
	OfflineParam: "token_access_type",
	SwitchPrompt: "login", ReauthorizePrompt: "consent",
	Identity: dropboxIdentity,
})

func dropboxOAuthClient(ask cliapp.Prompt, previous cliapp.OAuthClient) (cliapp.OAuthClient, error) {
	id, err := cliapp.AskAppID(ask, previous.ClientID, "MWEAVE_DROPBOX_APP_KEY", "Dropbox app key",
		"Dropbox browser authorization requires an app key.",
		"Register the loopback callback "+dropboxRedirect+" in your Dropbox app or use an existing access token.")
	return cliapp.OAuthClient{ClientID: id}, err
}

func dropboxIdentity(ctx context.Context, reply cliapp.OAuthTokenReply) (string, string, error) {
	email, err := dropbox.VerifyAccount(ctx, accountIdentityClient(dropbox.ControlHost, reply.Access), reply.Account)
	return reply.Account, email, err
}

const googleDriveReadScope = "https://www.googleapis.com/auth/drive.readonly"

var googleDriveAuthorization = cliapp.NewBrowserAuthorization(cliapp.BrowserAuthorization{
	ManagedAuthorization: cliapp.ManagedAuthorization{
		Provider: "googledrive", Label: "Google Drive", Method: "browser",
		Scopes: func([]byte) ([]string, error) { return []string{googleDriveReadScope}, nil },
		Client: googleDriveOAuthClient,
	},
	AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token",
	ListenAddress: "127.0.0.1:0",
	OfflineParam:  "access_type",
	FirstPrompt:   "consent select_account", SwitchPrompt: "consent select_account", ReauthorizePrompt: "consent",
	Identity: googleDriveIdentity,
})

func googleDriveOAuthClient(ask cliapp.Prompt, previous cliapp.OAuthClient) (cliapp.OAuthClient, error) {
	label := "Google Desktop OAuth client JSON file"
	if previous.ClientID != "" {
		label += " (empty to reuse current app)"
	}
	name, err := ask(label, "")
	if err != nil {
		return cliapp.OAuthClient{}, err
	}
	if name == "" && previous.ClientID != "" {
		return previous, nil
	}
	data, err := cliapp.ReadCredentialFile(name)
	if err != nil {
		return cliapp.OAuthClient{}, &cliapp.Problem{Code: "interaction_required", Message: "Google Drive browser authorization requires a Desktop OAuth client.",
			NextAction: "Provide the Desktop app client JSON downloaded from Google Cloud, or use an existing access token."}
	}
	defer clear(data)
	var client struct {
		Installed *cliapp.OAuthClient `json:"installed"`
	}
	if err := json.Unmarshal(data, &client); err != nil || client.Installed == nil || strings.TrimSpace(client.Installed.ClientID) == "" {
		return cliapp.OAuthClient{}, cliapp.InvalidConfig("Provide a Google Desktop app client JSON file.")
	}
	return *client.Installed, nil
}

func googleDriveIdentity(ctx context.Context, reply cliapp.OAuthTokenReply) (string, string, error) {
	return googledrive.Account(ctx, accountIdentityClient(googledrive.ControlHost, reply.Access))
}

func accountIdentityClient(host, access string) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: cliapp.TokenHeaderTransport{
		Base: http.DefaultTransport, Scheme: "https", Hosts: []string{host},
		Headers: map[string]string{"Authorization": "Bearer " + access},
	}}
}

func dropboxMountScopes(data []byte) ([]string, error) {
	var o dropboxOptions
	if len(data) != 0 {
		if err := cliapp.DecodeObject(data, &o); err != nil {
			return nil, err
		}
	}
	options, err := o.native()
	return options.AuthorizationScopes(), err
}

var weiyunAuthorization = cliapp.ManagedAuthorization{
	Provider: "weiyun", Label: "Weiyun", Method: "browser", Kind: cliapp.StaticGrant,
	Authorize: authorizeWeiyun,
}

func authorizeWeiyun(ctx context.Context, _ cliapp.OAuthClient, _ []string, _ cliapp.AuthorizationIntent, out io.Writer) (cliapp.OAuthGrant, error) {
	token, err := weiyun.Authorize(ctx, cliapp.ReadOAuthResponse, func(ctx context.Context, target string) error {
		if _, err := fmt.Fprintln(out, "\nWeiyun authorization\n  Weiyun grants permission to operate on all files in your account.\n  This version of mweave uses that access to browse and download files.\n  Approve access in the browser. mweave will continue automatically."); err != nil {
			return err
		}
		launchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		launchErr := cliapp.LaunchOAuthBrowser(launchCtx, target)
		cancel()
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if launchErr != nil {
			if _, err := fmt.Fprintf(out, "  Open this URL: %s\n", target); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintln(out, "  Waiting for browser approval (Ctrl+C to cancel).")
		return err
	})
	switch {
	case errors.Is(err, weiyun.ErrAuthorizationExpired):
		return cliapp.OAuthGrant{}, &cliapp.Problem{Code: "interaction_required", Message: weiyun.ErrAuthorizationExpired.Error() + ".",
			NextAction: "Start authorization again to open a new link."}
	case errors.Is(err, weiyun.ErrAuthorizationRejected):
		return cliapp.OAuthGrant{}, &cliapp.Problem{Code: "interaction_required", Message: weiyun.ErrAuthorizationRejected.Error() + ".",
			NextAction: "Start a new Weiyun authorization or use a token reference."}
	case err != nil:
		return cliapp.OAuthGrant{}, err
	}
	return cliapp.OAuthGrant{Label: "Weiyun account", Access: token}, nil
}

const aliPanPartSize = 8 << 20

func chooseAliPanRoot(ctx context.Context, raw json.RawMessage, ref *cliapp.CredentialRef, fresh *cliapp.OAuthGrant, scopes []string, ask cliapp.Prompt, switching bool) (json.RawMessage, error) {
	var opts alipan.Options
	if len(raw) != 0 {
		if err := cliapp.DecodeObject(raw, &opts); err != nil {
			return nil, err
		}
	}
	if switching {
		opts.DriveID, opts.RootID = "", ""
	}
	var drives alipan.DriveInfo
	var verified bool
	if fresh != nil {
		drives, verified = fresh.SetupData.(alipan.DriveInfo)
	}
	if !verified {
		record, tokens, release, err := acquireAliPanAuthorization(ref.Ref)
		if err != nil {
			return nil, err
		}
		access, err := tokens.Access(ctx)
		if err == nil {
			drives, err = aliPanOAuth(record.OAuthClient).DriveInfo(ctx, record.Account, access)
			err = projectAliPanOAuth(err)
		}
		if err := errors.Join(err, tokens.Close(), release()); err != nil {
			return nil, err
		}
	}
	if opts.DriveID != "" && !drives.HasDrive(opts.DriveID) {
		return nil, cliapp.AuthorizationScopeConflict()
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

func pickAliPanDrive(drives alipan.DriveInfo, ask cliapp.Prompt) (string, error) {
	choices := []struct{ label, id string }{
		{"resource", drives.Resource}, {"default", drives.Default}, {"backup", drives.Backup},
	}
	var labels, ids []string
	for _, drive := range choices {
		if drive.id != "" && !slices.Contains(ids, drive.id) {
			labels = append(labels, drive.label)
			ids = append(ids, drive.id)
		}
	}
	if len(labels) == 0 {
		return "", core.Fail(core.InvalidUpstreamResponse, nil)
	}
	if len(labels) == 1 {
		return ids[0], nil
	}
	name, err := cliapp.AskChoice(ask, "Drive ("+strings.Join(labels, ", ")+")", labels[0], labels...)
	if err != nil {
		return "", err
	}
	return ids[slices.Index(labels, name)], nil
}
