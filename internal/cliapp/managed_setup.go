package cliapp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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

type AuthorizationIntent uint8

const (
	authorizeFirst AuthorizationIntent = iota
	authorizeSwitch
	authorizeAgain
)

// GrantKind is the lifecycle contract for a saved authorization.
type GrantKind uint8

const (
	ReusableGrant GrantKind = iota
	RotatingGrant
	ApplicationGrant
	StaticGrant
)

type ManagedAuthorization struct {
	Provider, Label, Method string
	Kind                    GrantKind
	VerifyRoots             bool // Re-check native access if the grant does not report scope.
	ConfigureBeforeGrant    bool // Mount options determine the authorization scopes.
	ManagedOnly             bool
	ReferenceInGrant        bool
	Scopes                  func([]byte) ([]string, error)
	ChooseScopes            func(Prompt) ([]string, error)
	ChooseRoot              func(context.Context, json.RawMessage, *CredentialRef, *OAuthGrant, []string, Prompt, bool) (json.RawMessage, error)
	Read                    func(string, string) (OAuthGrant, error)
	Probe                   func(context.Context, MountRecord, OAuthGrant) error
	Client                  func(Prompt, OAuthClient) (OAuthClient, error)
	Authorize               func(context.Context, OAuthClient, []string, AuthorizationIntent, io.Writer) (OAuthGrant, error)
	Renew                   func(context.Context, *OAuthGrant) error
}

func (p ManagedAuthorization) available() bool {
	return p.Kind == ApplicationGrant && p.Renew != nil ||
		p.Kind != ApplicationGrant && p.Authorize != nil
}

func (p *ManagedAuthorization) requiredScopes(options []byte) ([]string, error) {
	if p.Scopes == nil {
		return nil, nil
	}
	return p.Scopes(options)
}

func AskAppID(ask Prompt, previous, env, label, missing, nextAction string) (string, error) {
	if id := strings.TrimSpace(os.Getenv(env)); id != "" {
		return id, nil
	}
	id, err := ask(label, previous)
	if err != nil {
		return "", err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", &Problem{Code: "interaction_required", Message: missing, NextAction: nextAction}
	}
	return id, nil
}

func Pkce() (verifier, challenge string) {
	var nonce [32]byte
	rand.Read(nonce[:])
	verifier = base64.RawURLEncoding.EncodeToString(nonce[:])
	hash := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(hash[:])
}

func (p *ManagedAuthorization) newGrant(ctx context.Context, app OAuthClient, scopes []string, intent AuthorizationIntent, out io.Writer) (OAuthGrant, error) {
	g := OAuthGrant{OAuthClient: app}
	var err error
	if p.Kind == ApplicationGrant {
		err = p.Renew(ctx, &g)
	} else {
		g, err = p.Authorize(ctx, app, scopes, intent, out)
	}
	if err != nil {
		return OAuthGrant{}, err
	}
	g.ID, g.Provider, g.OAuthClient = NewGrantID(), p.Provider, app
	return g, nil
}

func (g OAuthGrant) persistNew(ctx context.Context) (err error) {
	lease, err := LockGrant(ctx, g.ID, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Lock.Close()) }()
	if _, err := g.Save(ctx, lease, false); err != nil {
		return errors.Join(GrantUnavailable(), err)
	}
	return nil
}

type grantSelection struct {
	ref   *CredentialRef
	fresh *OAuthGrant
	label string
}

// The wizard lists only compatible, explicitly saved grants. Reads stay
// local and the protected store remains the authority for each reference.
func (p *ManagedAuthorization) selectGrant(ctx context.Context, ask Prompt, out io.Writer, scopes []string, old OAuthGrant, allowReference, reconnecting bool) (grantSelection, error) {
	dir, err := GrantDirectory(true)
	if err != nil {
		return grantSelection{}, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return grantSelection{}, GrantUnavailable()
	}
	var choices []grantSelection
	var answers, labels []string
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".grant")
		if !ok || !ValidGrantID(id) {
			continue
		}
		g, err := p.ReadGrant(filepath.Join(dir, entry.Name()), id)
		if err != nil || g.Pending || id == old.ID || p.Kind != StaticGrant && g.Account == old.Account || !g.Covers(scopes) {
			continue
		}
		choices = append(choices, grantSelection{ref: &CredentialRef{Kind: ManagedCredential, Ref: id}, label: g.Label})
		answers = append(answers, strconv.Itoa(len(choices)))
		label := p.Label + " account"
		if p.Kind == StaticGrant {
			if info, err := entry.Info(); err == nil {
				label += " · saved " + info.ModTime().Local().Format("2006-01-02 15:04:05")
			}
		} else {
			label = fmt.Sprintf("%q (app %q)", g.Label, g.ClientID)
		}
		labels = append(labels, label)
	}
	answers = append(answers, "authorize")
	labels = append(labels, "authorize   Authorize a new account")
	label, choice := p.Label+" authorization", ""
	switch {
	case len(choices) == 1:
		choice = "1"
	case len(choices) == 0 && !allowReference:
		label += " (Enter to start)"
		choice = "authorize"
	}
	if allowReference {
		answers = append(answers, "reference")
		labels = append(labels, "reference   Use an access token reference")
	}
	if !reconnecting || len(choices) != 0 {
		answer, err := askMenu(ask, label, choice, answers, labels)
		if err != nil || answer == "reference" {
			return grantSelection{}, err
		}
		if answer != "authorize" {
			selected := choices[slices.Index(answers, answer)]
			_, err := fmt.Fprintf(out, "Using saved %s authorization.\n", p.Label)
			return selected, err
		}
	}
	var app OAuthClient
	if p.Client != nil {
		app, err = p.Client(ask, old.OAuthClient)
		if err != nil {
			return grantSelection{}, err
		}
	}
	intent := authorizeFirst
	if old.ID != "" {
		intent = authorizeSwitch
	}
	g, err := p.newGrant(ctx, app, scopes, intent, out)
	if err != nil {
		return grantSelection{}, err
	}
	if p.Kind != StaticGrant && g.Account == old.Account {
		return grantSelection{}, &Problem{Code: "credential_scope_conflict", Message: p.Label + " returned the account already used by this mount.",
			NextAction: "Choose another account or reauthorize the current account."}
	}
	if !reconnecting {
		if p.Kind == StaticGrant {
			_, err = fmt.Fprintf(out, "Authorized %s.\n", p.Label)
		} else {
			_, err = fmt.Fprintf(out, "Authorized %s account %q.\n", p.Label, g.Label)
		}
	}
	return grantSelection{ref: &CredentialRef{Kind: ManagedCredential, Ref: g.ID}, fresh: &g, label: g.Label}, err
}

func (p *ManagedAuthorization) probeGrant(ctx context.Context, src MountSource, mount MountRecord, grant OAuthGrant) error {
	if p.Probe != nil {
		return p.Probe(ctx, mount, grant)
	}
	d, err := constructDriver(ctx, src.Open, grant.Access)
	return src.verifyAndClose(ctx, d, err)
}

type reauthorizeReceipt struct {
	Mounts              []string `json:"mounts"`
	AuthorizationEffect string   `json:"authorization_effect"`
}

func (p *ManagedAuthorization) reauthorize(ctx context.Context, appCLI *App, saved ConfigSnapshot, selected *Route, old OAuthGrant, ask Prompt, out io.Writer) (_ any, _ string, err error) {
	receipt := reauthorizeReceipt{Mounts: make([]string, 0, 1), AuthorizationEffect: core.Unchanged.String()}
	scopes := old.Scopes
	var shared []MountRecord
	for mount := range CredentialRecords(saved.Value.Mounts) {
		if mount.Credential.Kind != ManagedCredential || mount.Credential.Ref != old.ID {
			continue
		}
		if bound, _ := appCLI.mountRoute(mount); bound != selected {
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
	if _, err := fmt.Fprintf(out, "Reauthorize %s account %q for saved mounts %q.\n", p.Label, old.Label, receipt.Mounts); err != nil {
		return receipt, "", err
	}
	app := old.OAuthClient
	if p.Kind == ApplicationGrant {
		app, err = p.Client(ask, app)
		if err != nil {
			return receipt, "", err
		}
	}
	var lease *GrantLease
	defer func() {
		if lease != nil {
			err = errors.Join(err, lease.Lock.Close())
		}
	}()
	lockGrant := func(fresh *OAuthGrant) (err error) {
		lockCtx := ctx
		if p.Kind == RotatingGrant {
			lockCtx = nil // A live rotating Driver already owns this record.
		}
		lease, err = LockGrant(lockCtx, old.ID, false)
		if err != nil {
			return err
		}
		current, err := p.ReadGrant(lease.Path, old.ID)
		if err != nil {
			return err
		}
		if current.Account != old.Account || current.OAuthClient != old.OAuthClient || current.Refresh != old.Refresh ||
			fresh != nil && !fresh.Covers(current.Scopes) {
			return ConfigConflict()
		}
		return nil
	}
	if p.Kind == RotatingGrant || p.Kind == ApplicationGrant {
		if err := lockGrant(nil); err != nil {
			return receipt, "", err
		}
	}
	fresh, err := p.newGrant(ctx, app, OAuthScopeSet(strings.Join(scopes, " ")), authorizeAgain, out)
	if err != nil {
		return receipt, "", err
	}
	if fresh.Account != old.Account {
		return receipt, "", &Problem{Code: "credential_scope_conflict", Message: p.Label + " returned a different account.",
			NextAction: "Authorize the original account or choose switch to change one mount."}
	}
	if p.VerifyRoots {
		for _, m := range shared {
			var src MountSource
			if p.Probe == nil {
				src, err = PrepareMountSource(appCLI, m, filepath.Dir(saved.Path))
				if err != nil {
					return receipt, "", err
				}
			}
			if err := p.probeGrant(ctx, src, m, fresh); err != nil {
				return receipt, "", err
			}
		}
	}
	if lease == nil {
		if err := lockGrant(&fresh); err != nil {
			return receipt, "", err
		}
	}
	lock, err := saved.LockUnchanged(ctx)
	if err != nil {
		return receipt, "", err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	fresh.ID = old.ID
	published, err := fresh.Save(ctx, lease, true)
	if published {
		receipt.AuthorizationEffect = core.Applied.String()
	}
	if err != nil {
		return receipt, "", errors.Join(GrantUnavailable(), err)
	}
	return receipt, fmt.Sprintf("Updated %s authorization for saved mounts %q.\n", p.Label, receipt.Mounts), nil
}
