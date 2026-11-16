package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	core "github.com/nostalume/mountweave"
)

type authorizationIntent uint8

const (
	authorizeFirst authorizationIntent = iota
	authorizeSwitch
	authorizeAgain
)

type managedAuthorization struct {
	provider, label, method string
	exclusive               bool // Hold the record lease across consent when a runtime owns rotation.
	appCredentials          bool // The stored app credentials can issue another access token.
	verifyRoots             bool // Re-check native access if the grant does not report scope.
	managedOnly             bool
	staticOnly              bool
	scopes                  func([]byte) ([]string, error)
	chooseScopes            func(askField) ([]string, error)
	chooseRoot              func(context.Context, json.RawMessage, *credentialRef, *oauthGrant, []string, askField, bool) (json.RawMessage, error)
	read                    func(string, string) (oauthGrant, error)
	probe                   func(context.Context, mountRecord, oauthGrant) error
	client                  func(askField, oauthClient) (oauthClient, error)
	authorize               func(context.Context, oauthClient, []string, authorizationIntent, io.Writer) (oauthGrant, error)
}

func (p managedAuthorization) available() bool {
	return p.authorize != nil
}

func (p *managedAuthorization) requiredScopes(options []byte) ([]string, error) {
	if p.scopes == nil {
		return nil, nil
	}
	return p.scopes(options)
}

func askAppID(ask askField, previous, env, label string) (string, error) {
	if id := strings.TrimSpace(os.Getenv(env)); id != "" {
		return id, nil
	}
	id, err := ask(label, previous)
	return strings.TrimSpace(id), err
}

func pkce() (verifier, challenge string) {
	var nonce [32]byte
	rand.Read(nonce[:])
	verifier = base64.RawURLEncoding.EncodeToString(nonce[:])
	hash := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(hash[:])
}

func (p *managedAuthorization) newGrant(ctx context.Context, app oauthClient, scopes []string, intent authorizationIntent, out io.Writer) (oauthGrant, error) {
	g, err := p.authorize(ctx, app, scopes, intent, out)
	if err != nil {
		return oauthGrant{}, err
	}
	var id [16]byte
	rand.Read(id[:])
	g.ID, g.Provider, g.oauthClient = hex.EncodeToString(id[:]), p.provider, app
	return g, nil
}

func (g oauthGrant) persistNew(ctx context.Context) (err error) {
	lease, err := lockGrant(ctx, g.ID, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.lock.Close()) }()
	if err := g.save(ctx, lease, false); err != nil {
		return errors.Join(grantUnavailable(), err)
	}
	return nil
}

// The wizard lists only compatible, explicitly saved grants. Reads stay
// local and the protected store remains the authority for each reference.
func (p *managedAuthorization) selectGrant(ctx context.Context, ask askField, out io.Writer, scopes []string, old oauthGrant) (*credentialRef, *oauthGrant, error) {
	if !managedStoreSupported {
		return nil, nil, &problem{Code: "unsupported_operation", Message: "Managed credential storage is unavailable on this platform."}
	}
	dir, err := grantDirectory(true)
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, grantUnavailable()
	}
	var choices []string
	answers := []string{"authorize"}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".grant")
		if !ok || !validGrantID(id) {
			continue
		}
		g, err := p.readGrant(filepath.Join(dir, entry.Name()), id)
		if err != nil || g.Pending || id == old.ID || !p.staticOnly && g.Account == old.Account || !g.covers(scopes) {
			continue
		}
		choices = append(choices, id)
		answers = append(answers, strconv.Itoa(len(choices)))
		var displayErr error
		if p.staticOnly {
			label := p.label + " account"
			if info, err := entry.Info(); err == nil {
				label += " · saved " + info.ModTime().Local().Format("2006-01-02 15:04:05")
			}
			_, displayErr = fmt.Fprintf(out, "  %d  %s\n", len(choices), label)
		} else {
			_, displayErr = fmt.Fprintf(out, "%d  %q (app %q)\n", len(choices), g.Label, g.ClientID)
		}
		if displayErr != nil {
			return nil, nil, displayErr
		}
	}
	label, choice := p.label+" authorization (Enter to start)", "authorize"
	if len(choices) > 0 {
		label, choice = p.label+" authorization (saved authorization number or authorize)", ""
		if len(choices) == 1 {
			choice = "1"
		}
	}
	answer, err := askChoice(ask, label, choice, answers...)
	if err != nil {
		return nil, nil, err
	}
	if answer != "authorize" {
		index := slices.Index(answers, answer) - 1
		if _, err := fmt.Fprintf(out, "Using saved %s authorization.\n", p.label); err != nil {
			return nil, nil, err
		}
		return &credentialRef{Kind: managedCredential, Ref: choices[index]}, nil, nil
	}
	var app oauthClient
	if p.client != nil {
		app, err = p.client(ask, old.oauthClient)
		if err != nil {
			return nil, nil, err
		}
	}
	intent := authorizeFirst
	if old.ID != "" {
		intent = authorizeSwitch
	}
	g, err := p.newGrant(ctx, app, scopes, intent, out)
	if err != nil {
		return nil, nil, err
	}
	if !p.staticOnly && g.Account == old.Account {
		return nil, nil, &problem{Code: "credential_scope_conflict", Message: p.label + " returned the account already used by this mount.",
			NextAction: "Choose another account or reauthorize the current account."}
	}
	if p.staticOnly {
		_, err = fmt.Fprintf(out, "Authorized %s.\n", p.label)
	} else {
		_, err = fmt.Fprintf(out, "Authorized %s account %q.\n", p.label, g.Label)
	}
	if err != nil {
		return nil, nil, err
	}
	return &credentialRef{Kind: managedCredential, Ref: g.ID}, &g, nil
}

type reauthorizeReceipt struct {
	Mounts              []string `json:"mounts"`
	AuthorizationEffect string   `json:"authorization_effect"`
}

func (p *managedAuthorization) reauthorize(ctx context.Context, appCLI *App, saved configSnapshot, driver string, old oauthGrant, ask askField, out io.Writer) (_ any, _ string, err error) {
	receipt := reauthorizeReceipt{Mounts: make([]string, 0, 1), AuthorizationEffect: core.Unchanged.String()}
	scopes := old.Scopes
	var shared []mountRecord
	for _, savedMount := range saved.value.Mounts {
		for _, mount := range sourceRecords(savedMount) {
			if appCLI.routeID(mount) != driver || mount.Credential == nil || mount.Credential.Kind != managedCredential || mount.Credential.Ref != old.ID {
				continue
			}
			if len(receipt.Mounts) == 0 || receipt.Mounts[len(receipt.Mounts)-1] != mount.At {
				receipt.Mounts = append(receipt.Mounts, mount.At)
			}
			shared = append(shared, mount)
			required, err := p.requiredScopes(mount.Options)
			if err != nil {
				return receipt, "", err
			}
			scopes = append(scopes, required...)
		}
	}
	if _, err := fmt.Fprintf(out, "Reauthorize %s account %q for saved mounts %q.\n", p.label, old.Label, receipt.Mounts); err != nil {
		return receipt, "", err
	}
	app := old.oauthClient
	if p.appCredentials {
		app, err = p.client(ask, app)
		if err != nil {
			return receipt, "", err
		}
	}
	var lease *grantLease
	if p.exclusive || p.appCredentials {
		if p.exclusive {
			lease, err = acquireGrant(old.ID, false)
		} else {
			lease, err = lockGrant(ctx, old.ID, false)
		}
		if err != nil {
			return receipt, "", err
		}
		defer func() { err = errors.Join(err, lease.lock.Close()) }()
		current, err := p.readGrant(lease.path, old.ID)
		if err != nil {
			return receipt, "", err
		}
		if current.Account != old.Account || current.oauthClient != old.oauthClient || current.Refresh != old.Refresh {
			return receipt, "", configConflict()
		}
	}
	fresh, err := p.newGrant(ctx, app, oauthScopeSet(strings.Join(scopes, " ")), authorizeAgain, out)
	if err != nil {
		return receipt, "", err
	}
	if fresh.Account != old.Account {
		return receipt, "", &problem{Code: "credential_scope_conflict", Message: p.label + " returned a different account.",
			NextAction: "Authorize the original account or choose switch to change one mount."}
	}
	if p.verifyRoots {
		for _, m := range shared {
			if p.probe != nil {
				if err := p.probe(ctx, m, fresh); err != nil {
					return receipt, "", err
				}
				continue
			}
			src, err := appCLI.prepareMountSource(m, filepath.Dir(saved.path))
			if err != nil {
				return receipt, "", err
			}
			d, err := src.open(ctx, fresh.Access)
			if err != nil {
				if d != nil {
					err = errors.Join(err, d.Close())
				}
				return receipt, "", err
			}
			if err := d.Close(); err != nil {
				return receipt, "", err
			}
		}
	}
	if lease == nil {
		lease, err = lockGrant(ctx, old.ID, false)
		if err != nil {
			return receipt, "", err
		}
		defer func() { err = errors.Join(err, lease.lock.Close()) }()
		current, err := p.readGrant(lease.path, old.ID)
		if err != nil {
			return receipt, "", err
		}
		if current.Account != old.Account || current.oauthClient != old.oauthClient || current.Refresh != old.Refresh || !fresh.covers(current.Scopes) {
			return receipt, "", configConflict()
		}
	}
	lock, err := saved.lockUnchanged(ctx)
	if err != nil {
		return receipt, "", err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	fresh.ID = old.ID
	if err := fresh.save(ctx, lease, true); err != nil {
		return receipt, "", errors.Join(grantUnavailable(), err)
	}
	receipt.AuthorizationEffect = core.Applied.String()
	return receipt, fmt.Sprintf("Updated %s authorization for saved mounts %q.\n", p.label, receipt.Mounts), nil
}
