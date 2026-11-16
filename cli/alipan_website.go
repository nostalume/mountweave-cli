//go:build mweave_candidate

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/chromedp/chromedp"
	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/alipan"
)

// The sign-in page saves the session before navigating to back. Land on a
// static document so the uninstrumented window does not load the drive app.
const aliPanWebsiteLogin = "https://www.alipan.com/sign/in?back=%2Frobots.txt"

func aliPanWebsiteRoute() route {
	r := sourceRoute("Aliyun Drive website", nil, prepareAliPanWebsite)
	r.setup = connectAliPanWebsite
	return r
}

var openAliPanWebsite = func(ctx context.Context, binary string, options []chromedp.ExecAllocatorOption, profile string) (alipan.Website, error) {
	call, release, _, err := openWebsite(ctx, binary, options, profile, alipan.WebsiteURL, alipan.WebsiteWritePaths())
	return alipan.Website{Call: call, Release: release}, err
}

type aliPanWebsiteOptions struct {
	RootID  string                      `json:"root_id"`
	Account string                      `json:"account"`
	Drive   string                      `json:"drive_id"`
	Write   *alipan.WebsiteWriteOptions `json:"write,omitempty"`
}

type aliPanWebsiteRecord struct {
	Provider string `json:"provider"`
	Profile  string `json:"profile"`
	Account  string `json:"account"`
	Drive    string `json:"drive_id"`
}

func prepareAliPanWebsite(opts aliPanWebsiteOptions, _ string) (mountSource, error) {
	if opts.Account == "" || opts.Drive == "" || opts.RootID == "" {
		return mountSource{}, invalidConfig("The Aliyun Drive website mount needs a verified account, resource drive and folder.")
	}
	return mountSource{
		probeOnChange: true,
		audience:      credentialAudience{origin: alipan.WebsiteURL, account: opts.Account},
		openManaged: func(ctx context.Context, id string) (core.Driver, error) {
			return openAliPanWebsiteMount(ctx, id, opts)
		},
	}, nil
}

type aliPanWebsiteSessionsKey struct{}

type aliPanWebsiteSession struct {
	account alipan.WebsiteAccount
	release func() error
	users   int
}

// A runtime constructs its mounts serially; retirement can release leases in parallel.
func withAliPanWebsiteSessions(ctx context.Context) context.Context {
	return context.WithValue(ctx, aliPanWebsiteSessionsKey{}, &aliPanWebsiteSessions{byID: make(map[string]*aliPanWebsiteSession)})
}

type aliPanWebsiteSessions struct {
	mu   sync.Mutex
	byID map[string]*aliPanWebsiteSession
}

func openAliPanWebsiteMount(ctx context.Context, id string, opts aliPanWebsiteOptions) (core.Driver, error) {
	if sessions, ok := ctx.Value(aliPanWebsiteSessionsKey{}).(*aliPanWebsiteSessions); ok {
		sessions.mu.Lock()
		session := sessions.byID[id]
		if session != nil {
			session.users++
		}
		sessions.mu.Unlock()
		if session == nil {
			var err error
			session, err = loadAliPanWebsiteSession(ctx, id, opts)
			if err != nil {
				return nil, err
			}
			sessions.mu.Lock()
			sessions.byID[id] = session
			sessions.mu.Unlock()
		}
		release := sync.OnceValue(func() error {
			sessions.mu.Lock()
			session.users--
			last := session.users == 0
			if last {
				delete(sessions.byID, id)
			}
			sessions.mu.Unlock()
			if last {
				return session.release()
			}
			return nil
		})
		return session.open(ctx, opts, release)
	}
	session, err := loadAliPanWebsiteSession(ctx, id, opts)
	if err != nil {
		return nil, err
	}
	return session.open(ctx, opts, session.release)
}

func (s *aliPanWebsiteSession) open(ctx context.Context, opts aliPanWebsiteOptions, release func() error) (core.Driver, error) {
	if s.account.ID != opts.Account || s.account.ResourceDrive != opts.Drive {
		return nil, errors.Join(authorizationScopeConflict(), release())
	}
	var d core.Driver
	var err error
	if opts.Write == nil {
		d, _, err = s.account.Open(ctx, opts.RootID, "", nil, release)
	} else {
		d, err = s.account.OpenWritable(ctx, opts.RootID, *opts.Write, nil, release)
	}
	if err != nil {
		return nil, errors.Join(err, release())
	}
	return d, nil
}

func loadAliPanWebsiteSession(ctx context.Context, id string, opts aliPanWebsiteOptions) (_ *aliPanWebsiteSession, err error) {
	lease, err := acquireGrant(id, false)
	if err != nil {
		return nil, err
	}
	transfer := false
	defer func() {
		if !transfer {
			err = errors.Join(err, lease.lock.Close())
		}
	}()
	record, err := readAliPanWebsite(lease, opts)
	if err != nil {
		return nil, err
	}
	profile, err := websiteProfile(record.Profile, false)
	if err != nil {
		return nil, err
	}
	binary, options := websiteBrowser()
	website, err := openAliPanWebsite(ctx, binary, options, profile)
	if err != nil {
		return nil, err
	}
	release := website.Release
	account, err := website.Account(ctx)
	if err == nil && (account.ID != opts.Account || account.ResourceDrive != opts.Drive) {
		err = authorizationScopeConflict()
	}
	if err != nil {
		return nil, errors.Join(err, release())
	}
	transfer = true
	return &aliPanWebsiteSession{account: account, users: 1, release: sync.OnceValue(func() error {
		return errors.Join(release(), lease.lock.Close())
	})}, nil
}

func readAliPanWebsite(lease *grantLease, opts aliPanWebsiteOptions) (aliPanWebsiteRecord, error) {
	raw, err := lease.read()
	if err != nil {
		return aliPanWebsiteRecord{}, err
	}
	var record aliPanWebsiteRecord
	if decodeObject(raw, &record) != nil || record.Provider != aliPanWebsiteSource || !validGrantID(record.Profile) ||
		record.Account != opts.Account || record.Drive != opts.Drive {
		return aliPanWebsiteRecord{}, authorizationScopeConflict()
	}
	return record, nil
}

func connectAliPanWebsite(ctx context.Context, saved configSnapshot, prior mountRecord, at string, ask askField, out io.Writer) (_ any, _ string, err error) {
	unchanged := mountReceipt{At: at, Effect: core.Unchanged.String()}
	if err := checkWebsiteMount(saved, at, prior.At); err != nil {
		return unchanged, "", err
	}
	var old *grantLease
	if prior.aliPanWebsite() {
		if prior.Credential == nil || prior.Credential.Kind != managedCredential {
			return unchanged, "", grantUnavailable()
		}
		old, err = acquireGrant(prior.Credential.Ref, false)
		if err != nil {
			return unchanged, "", err
		}
		defer func() { err = errors.Join(err, old.lock.Close()) }()
	}
	profileID := newWebsiteID()
	profile, err := websiteProfile(profileID, true)
	if err != nil {
		return unchanged, "", err
	}
	committed := false
	defer func() {
		if !committed {
			err = errors.Join(err, removeWebsiteProfile(profile))
		}
	}()
	if _, err := fmt.Fprintln(out, "\nSign in to Aliyun Drive. This mount can browse and download from the folder you select.\nWhen the plain text page appears, close the sign-in window. If asked, confirm the close."); err != nil {
		return unchanged, "", err
	}
	binary, browserOptions := websiteBrowser()
	if err := signInWebsite(ctx, binary, profile, aliPanWebsiteLogin, ask, out); err != nil {
		return unchanged, "", err
	}
	website, err := openAliPanWebsite(ctx, binary, browserOptions, profile)
	if err != nil {
		return unchanged, "", err
	}
	closeCandidate := website.Release
	defer func() { err = errors.Join(err, closeCandidate()) }()
	account, err := website.Account(ctx)
	if errors.Is(err, alipan.ErrWebsiteSignIn) {
		return unchanged, "", &problem{Code: "interaction_required", Message: "The browser did not retain an Aliyun Drive sign-in.", NextAction: "Retry setup, finish signing in and close the window before continuing. Leave the site's remember sign-in option enabled if offered."}
	}
	if err != nil {
		return unchanged, "", err
	}
	options := aliPanWebsiteOptions{Account: account.ID, Drive: account.ResourceDrive}
	rootPath := ""
	if old != nil {
		if err := decodeObject(prior.Options, &options); err != nil {
			return unchanged, "", err
		}
		if account.ID != options.Account || account.ResourceDrive != options.Drive {
			return unchanged, "", authorizationScopeConflict()
		}
		checked := map[string]bool{options.RootID: true}
		for _, mounted := range saved.value.Mounts {
			for _, source := range sourceRecords(mounted) {
				if source.Credential == nil || *source.Credential != *prior.Credential {
					continue
				}
				var bound aliPanWebsiteOptions
				if !source.aliPanWebsite() || decodeObject(source.Options, &bound) != nil || bound.Account != options.Account || bound.Drive != options.Drive {
					return unchanged, "", authorizationScopeConflict()
				}
				if !checked[bound.RootID] {
					if err := account.CheckRoot(ctx, bound.RootID); err != nil {
						return unchanged, "", err
					}
					checked[bound.RootID] = true
				}
			}
		}
	}

	var d core.Driver
	var root string
	for {
		if old == nil {
			rootPath, err = ask("Folder on the resource drive", "/")
			if err != nil {
				return unchanged, "", err
			}
			if core.ValidatePath(rootPath) != nil {
				if _, err := fmt.Fprintln(out, "Enter an absolute folder path such as /Documents."); err != nil {
					return unchanged, "", err
				}
				continue
			}
		}
		d, root, err = account.Open(ctx, options.RootID, rootPath, nil, nil)
		code := core.CodeOf(err)
		if old != nil || (code != core.NotFound && code != core.NotDirectory) {
			break
		}
		if _, err := fmt.Fprintln(out, "That path is not a folder on the resource drive. Try another folder."); err != nil {
			return unchanged, "", err
		}
	}
	if err != nil {
		return unchanged, "", err
	}
	closeCandidate = d.Close
	options.RootID = root
	label := account.Name
	if label == "" {
		label = account.ID
	}
	folder := rootPath
	if folder == "" {
		folder = "existing resource drive folder"
	}
	if _, err := fmt.Fprintf(out, "\nReview mount\n  Account  %q\n  Folder   %q\n  Mount    %q\n  Access   Browse and download\n", label, folder, at); err != nil {
		return unchanged, "", err
	}
	if old == nil {
		if err := confirm(ask, "Save mount?"); err != nil {
			return unchanged, "", err
		}
	}
	err = closeCandidate()
	closeCandidate = func() error { return nil }
	if err != nil {
		return unchanged, "", err
	}

	id := newWebsiteID()
	data, err := json.Marshal(aliPanWebsiteRecord{aliPanWebsiteSource, profileID, options.Account, options.Drive})
	if err != nil {
		return unchanged, "", err
	}
	if old != nil {
		record, err := readAliPanWebsite(old, options)
		if err != nil {
			return unchanged, "", err
		}
		result, message, err := replaceWebsiteGrant(ctx, saved, at, old, record.Profile, data)
		committed = result.Effect == core.Applied.String()
		return result, message, err
	}
	raw, err := json.Marshal(options)
	if err != nil {
		return unchanged, "", err
	}
	record := mountRecord{At: at, Driver: "alipan", Connection: "website", Options: raw, Credential: &credentialRef{Kind: managedCredential, Ref: id}}
	result, message, err := commitMount(ctx, saved, prior, record, nil, data)
	committed = result.Effect == core.Applied.String()
	return result, message, err
}
