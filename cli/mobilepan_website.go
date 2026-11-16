//go:build mweave_candidate

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/chromedp/chromedp"
	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/mobilepan"
)

type mobilePanOptions struct {
	Root string `json:"root"`
}

type mobilePanGrant struct {
	Provider string `json:"provider"`
	Profile  string `json:"profile"`
	Account  string `json:"account"`
	Root     string `json:"root"`
	RootID   string `json:"root_id"`
}

func mobilePanWebsiteRoute() route {
	r := sourceRoute("China Mobile Cloud (移动云盘)", nil, prepareMobilePan)
	r.setup = connectMobilePanWebsite
	return r
}

var openMobilePanWebsite = func(ctx context.Context, binary string, options []chromedp.ExecAllocatorOption, profile string) (mobilepan.Website, error) {
	call, release, _, err := openWebsite(ctx, binary, options, profile, mobilepan.WebsiteURL, nil)
	return mobilepan.Website{Call: call, Release: release}, err
}

func prepareMobilePan(opts mobilePanOptions, _ string) (mountSource, error) {
	if err := core.ValidatePath(opts.Root); err != nil {
		return mountSource{}, invalidConfig("Mobile Cloud requires a verified folder. Use mweave mount add to choose it.")
	}
	return mountSource{probeOnChange: true, audience: credentialAudience{origin: mobilepan.WebsiteURL, account: opts.Root},
		openManaged: func(ctx context.Context, id string) (core.Driver, error) {
			return openMobilePanMount(ctx, id, opts.Root)
		},
	}, nil
}

func readMobilePanGrant(lease *grantLease, root string) (mobilePanGrant, error) {
	raw, err := lease.read()
	if err != nil {
		return mobilePanGrant{}, err
	}
	var grant mobilePanGrant
	if decodeObject(raw, &grant) != nil || grant.Provider != mobilePanSource || !validGrantID(grant.Profile) ||
		grant.Account == "" || grant.Root != root || grant.RootID == "" {
		return mobilePanGrant{}, authorizationScopeConflict()
	}
	return grant, nil
}

func openMobilePanMount(ctx context.Context, id, root string) (_ core.Driver, err error) {
	lease, err := acquireGrant(id, false)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			err = errors.Join(err, lease.lock.Close())
		}
	}()
	grant, err := readMobilePanGrant(lease, root)
	if err != nil {
		return nil, err
	}
	profile, err := websiteProfile(grant.Profile, false)
	if err != nil {
		return nil, err
	}
	binary, options := websiteBrowser()
	website, err := openMobilePanWebsite(ctx, binary, options, profile)
	if err != nil {
		return nil, err
	}
	release := website.Release
	website.Release = func() error { return errors.Join(release(), lease.lock.Close()) }
	d, err := website.Open(ctx, grant.Account, grant.Root, storageHTTPClient)
	if err != nil {
		return nil, errors.Join(err, release())
	}
	transferred = true
	if d.RootID() != grant.RootID {
		return nil, errors.Join(authorizationScopeConflict(), d.Close())
	}
	return d, nil
}

func connectMobilePanWebsite(ctx context.Context, saved configSnapshot, prior mountRecord, at string, ask askField, out io.Writer) (_ any, _ string, err error) {
	unchanged := mountReceipt{At: at, Effect: core.Unchanged.String()}
	if err := checkWebsiteMount(saved, at, prior.At); err != nil {
		return unchanged, "", err
	}
	var previous mobilePanGrant
	var old *grantLease
	if prior.Driver != "" {
		var opts mobilePanOptions
		if prior.Credential == nil || prior.Credential.Kind != managedCredential || decodeObject(prior.Options, &opts) != nil {
			return unchanged, "", grantUnavailable()
		}
		old, err = acquireGrant(prior.Credential.Ref, false)
		if err != nil {
			return unchanged, "", err
		}
		defer func() { err = errors.Join(err, old.lock.Close()) }()
		previous, err = readMobilePanGrant(old, opts.Root)
		if err != nil {
			return unchanged, "", err
		}
	}

	id := newWebsiteID()
	profile, err := websiteProfile(id, true)
	if err != nil {
		return unchanged, "", err
	}
	applied := false
	defer func() {
		if !applied {
			err = errors.Join(err, removeWebsiteProfile(profile))
		}
	}()
	if _, err := fmt.Fprintln(out, "Sign in to China Mobile Cloud. Close the sign-in window when your files appear."); err != nil {
		return unchanged, "", err
	}
	binary, options := websiteBrowser()
	if err := signInWebsite(ctx, binary, profile, mobilepan.WebsiteURL, ask, out); err != nil {
		return unchanged, "", err
	}
	website, err := openMobilePanWebsite(ctx, binary, options, profile)
	if err != nil {
		return unchanged, "", err
	}
	closeCandidate := website.Release
	defer func() {
		if closeCandidate != nil {
			err = errors.Join(err, closeCandidate())
		}
	}()
	account, err := website.AccountID(ctx)
	if err != nil {
		return unchanged, "", err
	}
	if old != nil && account != previous.Account {
		return unchanged, "", authorizationScopeConflict()
	}
	folder := previous.Root
	var d *mobilepan.Driver
	for {
		if old == nil {
			folder, err = ask("Folder on China Mobile Cloud", "/")
			if err != nil {
				return unchanged, "", err
			}
		}
		d, err = website.Open(ctx, account, folder, storageHTTPClient)
		switch core.CodeOf(err) {
		case core.InvalidPath, core.NotFound, core.NotDirectory, core.PathAmbiguous:
			if old != nil {
				return unchanged, "", err
			}
			if _, err := fmt.Fprintln(out, "Enter an existing folder path such as /Documents."); err != nil {
				return unchanged, "", err
			}
			continue
		}
		if err != nil {
			return unchanged, "", err
		}
		break
	}
	closeCandidate = d.Close
	if old != nil && d.RootID() != previous.RootID {
		return unchanged, "", authorizationScopeConflict()
	}
	if _, err := fmt.Fprintf(out, "\nReview mount\n  Folder   %q\n  Mount    %q\n  Access   Browse and download\n", folder, at); err != nil {
		return unchanged, "", err
	}
	if old == nil {
		if err := confirm(ask, "Save mount?"); err != nil {
			return unchanged, "", err
		}
	}
	err = closeCandidate()
	closeCandidate = nil
	if err != nil {
		return unchanged, "", err
	}
	grant := mobilePanGrant{Provider: mobilePanSource, Profile: id, Account: account, Root: folder, RootID: d.RootID()}
	data, err := json.Marshal(grant)
	if err != nil {
		return unchanged, "", err
	}
	if old != nil {
		result, message, err := replaceWebsiteGrant(ctx, saved, at, old, previous.Profile, data)
		applied = result.Effect == core.Applied.String()
		return result, message, err
	}
	raw, err := json.Marshal(mobilePanOptions{Root: folder})
	if err != nil {
		return unchanged, "", err
	}
	m := mountRecord{
		At: at, Driver: mobilePanSource, Options: raw, Credential: &credentialRef{Kind: managedCredential, Ref: id},
	}
	result, message, err := commitMount(ctx, saved, prior, m, nil, data)
	applied = result.Effect == core.Applied.String()
	return result, message, err
}
