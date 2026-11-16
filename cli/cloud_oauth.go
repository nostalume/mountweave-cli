package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	core "github.com/nostalume/mountweave"
)

func (p *browserAuthorization) setup() managedAuthorization {
	setup := p.managedAuthorization
	setup.authorize = p.authorize
	return setup
}

const dropboxRedirect = "http://localhost:53682/callback"

var dropboxAuthorization = browserAuthorization{
	managedAuthorization: managedAuthorization{
		provider: "dropbox", label: "Dropbox", method: "browser",
		scopes: dropboxMountScopes, client: dropboxOAuthClient,
	},
	authorizeURL: "https://www.dropbox.com/oauth2/authorize", tokenURL: "https://api.dropboxapi.com/oauth2/token",
	listenAddress: "127.0.0.1:53682", redirectURI: dropboxRedirect,
	offlineParam: "token_access_type",
	switchPrompt: "login", reauthorizePrompt: "consent",
	identity: dropboxIdentity,
}

func dropboxOAuthClient(ask askField, previous oauthClient) (oauthClient, error) {
	app, err := askAppID(ask, previous.ClientID, "MWEAVE_DROPBOX_APP_KEY", "Dropbox app key")
	if err != nil {
		return oauthClient{}, err
	}
	if app == "" {
		return oauthClient{}, &problem{Code: "interaction_required", Message: "Dropbox browser authorization requires an app key.",
			NextAction: "Register the loopback callback " + dropboxRedirect + " in your Dropbox app or use an existing access token."}
	}
	return oauthClient{ClientID: app}, nil
}

func dropboxIdentity(ctx context.Context, reply oauthTokenReply) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.dropboxapi.com/2/users/get_current_account", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+reply.Access)
	var identity struct {
		Account string `json:"account_id"`
		Email   string `json:"email"`
	}
	status, err := readOAuthResponse(req, &identity)
	if err != nil {
		return "", "", err
	}
	if status != http.StatusOK {
		return "", "", core.Fail(core.UpstreamFailure, nil)
	}
	if identity.Account == "" || identity.Account != reply.Account || identity.Email == "" {
		return "", "", core.Fail(core.InvalidUpstreamResponse, nil)
	}
	return identity.Account, identity.Email, nil
}

const googleDriveReadScope = "https://www.googleapis.com/auth/drive.readonly"

var googleDriveAuthorization = browserAuthorization{
	managedAuthorization: managedAuthorization{
		provider: "googledrive", label: "Google Drive", method: "browser",
		scopes: func([]byte) ([]string, error) { return []string{googleDriveReadScope}, nil },
		client: googleDriveOAuthClient,
	},
	authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth", tokenURL: "https://oauth2.googleapis.com/token",
	listenAddress: "127.0.0.1:0",
	offlineParam:  "access_type",
	firstPrompt:   "consent select_account", switchPrompt: "consent select_account", reauthorizePrompt: "consent",
	identity: googleDriveIdentity,
}

func googleDriveOAuthClient(ask askField, previous oauthClient) (oauthClient, error) {
	label := "Google Desktop OAuth client JSON file"
	if previous.ClientID != "" {
		label += " (empty to reuse current app)"
	}
	name, err := ask(label, "")
	if err != nil {
		return oauthClient{}, err
	}
	if name == "" && previous.ClientID != "" {
		return previous, nil
	}
	data, err := readCredentialFile(name)
	if err != nil {
		return oauthClient{}, &problem{Code: "interaction_required", Message: "Google Drive browser authorization requires a Desktop OAuth client.",
			NextAction: "Provide the Desktop app client JSON downloaded from Google Cloud, or use an existing access token."}
	}
	defer clear(data)
	var client struct {
		Installed *oauthClient `json:"installed"`
	}
	if err := json.Unmarshal(data, &client); err != nil || client.Installed == nil || strings.TrimSpace(client.Installed.ClientID) == "" {
		return oauthClient{}, invalidConfig("Provide a Google Desktop app client JSON file.")
	}
	return *client.Installed, nil
}

func googleDriveIdentity(ctx context.Context, reply oauthTokenReply) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://www.googleapis.com/drive/v3/about?fields=user(permissionId,emailAddress,displayName)", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+reply.Access)
	var about struct {
		User struct {
			ID    string `json:"permissionId"`
			Email string `json:"emailAddress"`
			Name  string `json:"displayName"`
		} `json:"user"`
	}
	status, err := readOAuthResponse(req, &about)
	if err != nil {
		return "", "", err
	}
	if status != http.StatusOK {
		return "", "", core.Fail(core.UpstreamFailure, nil)
	}
	if about.User.ID == "" {
		return "", "", core.Fail(core.InvalidUpstreamResponse, nil)
	}
	label := about.User.Email
	if label == "" {
		label = about.User.Name
	}
	if label == "" {
		label = about.User.ID
	}
	return about.User.ID, label, nil
}
