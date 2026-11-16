package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"sync"

	"github.com/chromedp/chromedp"
	core "github.com/nostalume/mountweave"
)

const (
	lanZouLogin  = "https://up.woozooo.com/account.php?action=login"
	lanZouFiles  = "https://up.woozooo.com/mydisk.php"
	lanZouSource = "lanzou"
)

var openLanZouWebsite = func(ctx context.Context, binary string, options []chromedp.ExecAllocatorOption, profile string) (lanZouCall, lanZouBody, func() error, error) {
	call, release, tab, err := openWebsite(ctx, binary, append(options, chromedp.VisibleWindow), profile, lanZouFiles, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	download := func(ctx context.Context, target *url.URL, wanted *core.Range) (core.Opened, error) {
		return streamLanZou(ctx, tab, target, wanted)
	}
	return call, download, release, nil
}

type lanZouOptions struct {
	Root string `json:"root_id"`
}

type lanZouGrant struct {
	Provider string `json:"provider"`
	Account  string `json:"account,omitempty"`
	Root     string `json:"root_id"`
}

func lanZouWebsiteRoute() route {
	r := sourceRoute("LanZou Cloud (蓝奏云)", nil, prepareLanZouWebsite)
	r.setup = connectLanZouWebsite
	return r
}

func prepareLanZouWebsite(opts lanZouOptions, _ string) (mountSource, error) {
	root, err := strconv.ParseInt(opts.Root, 10, 64)
	if err != nil || (root <= 0 && root != -1) || strconv.FormatInt(root, 10) != opts.Root {
		return mountSource{}, invalidConfig("LanZou needs a verified folder ID. Use mweave mount add to select it.")
	}
	return mountSource{
		probeOnChange: true,
		audience:      credentialAudience{origin: lanZouFiles},
		openManaged: func(ctx context.Context, id string) (core.Driver, error) {
			return openLanZouMount(ctx, id, opts.Root)
		},
	}, nil
}

func loadLanZouGrant(grant *grantLease, root string) (lanZouGrant, error) {
	raw, err := grant.read()
	if err != nil {
		return lanZouGrant{}, err
	}
	var bound lanZouGrant
	if decodeObject(raw, &bound) != nil || bound.Provider != lanZouSource || bound.Root != root {
		return lanZouGrant{}, authorizationScopeConflict()
	}
	return bound, nil
}

func openLanZouMount(ctx context.Context, id, root string) (_ core.Driver, err error) {
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
	bound, err := loadLanZouGrant(lease, root)
	if err != nil {
		return nil, err
	}
	profile, err := websiteProfile(id, false)
	if err != nil {
		return nil, err
	}
	binary, options := websiteBrowser()
	call, download, closeBrowser, err := openLanZouWebsite(ctx, binary, options, profile)
	if err != nil {
		return nil, err
	}
	driver := &lanZouDriver{call: call, download: download, root: lanZouEntry{id: root, meta: core.Metadata{Kind: core.Directory}}, account: bound.Account}
	driver.firstFolders, _, err = driver.page(ctx, lanZouPageRequest{Task: 47, Folder: root})
	if err != nil {
		return nil, errors.Join(err, closeBrowser())
	}
	driver.release = sync.OnceValue(func() error { return errors.Join(closeBrowser(), lease.lock.Close()) })
	transferred = true
	return driver, nil
}

func connectLanZouWebsite(ctx context.Context, saved configSnapshot, prior mountRecord, at string, ask askField, out io.Writer) (_ any, _ string, err error) {
	unchanged := mountReceipt{at, core.Unchanged.String()}
	if err := checkWebsiteMount(saved, at, prior.At); err != nil {
		return unchanged, "", err
	}
	var previous lanZouGrant
	if prior.Driver != "" {
		var opts lanZouOptions
		if prior.Credential == nil || prior.Credential.Kind != managedCredential || decodeObject(prior.Options, &opts) != nil {
			return unchanged, "", grantUnavailable()
		}
		old, err := acquireGrant(prior.Credential.Ref, false)
		if err != nil {
			return unchanged, "", err
		}
		previous, err = loadLanZouGrant(old, opts.Root)
		if err = errors.Join(err, old.lock.Close()); err != nil {
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
	if _, err := fmt.Fprintln(out, "Sign in to LanZou Cloud. Close the sign-in window when your files appear."); err != nil {
		return unchanged, "", err
	}
	binary, options := websiteBrowser()
	if err := signInWebsite(ctx, binary, profile, lanZouLogin, ask, out); err != nil {
		return unchanged, "", err
	}
	call, _, release, err := openLanZouWebsite(ctx, binary, options, profile)
	if err != nil {
		return unchanged, "", err
	}
	release = sync.OnceValue(release)
	defer func() { err = errors.Join(err, release()) }()

	boundRoot := "-1"
	if previous.Root != "" {
		boundRoot = previous.Root
	}
	d := &lanZouDriver{call: call, account: previous.Account, root: lanZouEntry{id: boundRoot, meta: core.Metadata{Kind: core.Directory}}}
	d.firstFolders, d.account, err = d.page(ctx, lanZouPageRequest{Task: 47, Folder: boundRoot})
	if err != nil {
		return unchanged, "", err
	}
	root := d.root
	folder := "existing folder"
	if previous.Root == "" {
		for {
			folder, err = ask("Folder on LanZou Cloud", "/")
			if err != nil {
				return unchanged, "", err
			}
			root, err = d.resolve(ctx, core.NativePath(folder))
			if err == nil && root.meta.Kind == core.Directory {
				break
			}
			code := core.CodeOf(err)
			if err != nil && code != core.InvalidPath && code != core.NotFound && code != core.NotDirectory && code != core.PathAmbiguous {
				return unchanged, "", err
			}
			if _, err := fmt.Fprintln(out, "Enter an existing, unambiguous folder path, such as /Documents."); err != nil {
				return unchanged, "", err
			}
		}
		if root.id != boundRoot {
			if _, _, err := d.page(ctx, lanZouPageRequest{Task: 47, Folder: root.id}); err != nil {
				return unchanged, "", err
			}
		}
	}
	if _, _, err := d.page(ctx, lanZouPageRequest{Task: 5, Folder: root.id, Page: 1}); err != nil {
		return unchanged, "", err
	}
	if _, err := fmt.Fprintf(out, "\nReview mount\n  Folder   %q\n  Mount    %q\n  Access   Browse and download\n", folder, at); err != nil {
		return unchanged, "", err
	}
	prompt := "Save mount?"
	if prior.Driver != "" {
		prompt = "Replace the saved website sign-in for this mount?"
	}
	if err := confirm(ask, prompt); err != nil {
		return unchanged, "", err
	}
	if err := release(); err != nil {
		return unchanged, "", err
	}
	raw, err := json.Marshal(lanZouOptions{Root: root.id})
	if err != nil {
		return unchanged, "", err
	}
	m := mountRecord{At: at, Driver: lanZouSource, Options: raw, Credential: &credentialRef{Kind: managedCredential, Ref: id}}
	data, err := json.Marshal(lanZouGrant{lanZouSource, d.account, root.id})
	if err != nil {
		return unchanged, "", err
	}
	result, message, err := commitMount(ctx, saved, prior, m, nil, data)
	applied = result.Effect == core.Applied.String()
	return result, message, err
}
