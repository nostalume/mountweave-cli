package stock

import (
	"context"
	"errors"
	"sync"

	"github.com/chromedp/chromedp"
	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave/drivers/alipan"
	"github.com/nostalume/mountweave/drivers/lanzou"
	"github.com/nostalume/mountweave/drivers/mobilepan"
	"github.com/nostalume/mountweave/drivers/pan123"
	pan123cdp "github.com/nostalume/mountweave/drivers/pan123/cdp"
	"github.com/nostalume/mountweave/drivers/quark"
	quarkcdp "github.com/nostalume/mountweave/drivers/quark/cdp"
	"github.com/nostalume/mountweave/drivers/uc"
)

func pan123WebsiteRoute() *cliapp.Route {
	const entry = pan123cdp.Origin + "/"
	source := websiteSource{
		ID: "pan123", Name: "123 Cloud Drive", Entry: entry,
		SignIn: pan123.ErrWebsiteSignIn, BindingMismatch: pan123.ErrWebsiteBindingMismatch,
	}
	route := source.withPage(entry, false, func(page websitePage) websiteSession {
		session := pan123cdp.New(page.Tab)
		return websiteSession{
			Account: session.Account,
			Mount: func(ctx context.Context, account, root, rootID string, release func() error) (websiteDriver, error) {
				return pan123.OpenWebsite(ctx, pan123.WebsiteOptions{
					Session: session, Account: account, RootPath: root, RootID: rootID,
					ContentClient: storageHTTPClient, Release: release,
				})
			},
		}
	}).route()
	return &route
}

func mobilePanWebsiteRoute() cliapp.Route {
	source := websiteSource{
		ID: cliapp.MobilePanSource, Name: "China Mobile Cloud", Entry: mobilepan.WebsiteURL,
		Access: "Browse, download, upload and create directories",
		SignIn: mobilepan.ErrWebsiteSignIn, BindingMismatch: mobilepan.ErrWebsiteBindingMismatch,
	}
	return source.withPage(mobilepan.WebsiteURL, false, func(page websitePage) websiteSession {
		return websiteSession{
			Account: (mobilepan.Website{Call: page.Call}).AccountID,
			Mount: func(ctx context.Context, account, root, rootID string, release func() error) (websiteDriver, error) {
				directory, err := websiteUploadDirectory()
				if err != nil {
					return nil, err
				}
				session := mobilepan.Website{
					Call: page.Call, ExpectedRootID: rootID, Release: release,
					Upload: &mobilepan.WebsiteUploadOptions{StagingDirectory: directory, MaxStagingBytes: maxWebsiteStagingBytes},
				}
				return session.Open(ctx, account, root, storageHTTPClient)
			},
		}
	}).route()
}

func ucWebsiteRoute() cliapp.Route {
	source := websiteSource{
		ID: cliapp.UcSource, Name: "UC Cloud Drive", Entry: uc.WebsiteURL,
		SignIn: uc.ErrWebsiteSignIn, BindingMismatch: uc.ErrWebsiteBindingMismatch,
	}
	return source.withPage(uc.WebsiteURL, true, func(page websitePage) websiteSession {
		return websiteSession{
			Account: func(ctx context.Context) (string, error) { return uc.WebsiteAccount(ctx, page.Call) },
			Mount: func(ctx context.Context, account, root, rootID string, release func() error) (websiteDriver, error) {
				return uc.OpenWebsite(ctx, uc.WebsiteOptions{
					Call: page.Call, Tab: page.Tab, Account: account, RootPath: root, RootID: rootID, Release: release,
				})
			},
		}
	}).route()
}

func quarkWebsiteRoute() cliapp.Route {
	source := websiteSource{
		ID: cliapp.QuarkSource, Name: "Quark Cloud Drive", Entry: quark.WebsiteOrigin + "/",
		SignIn: quark.ErrWebsiteSignIn, BindingMismatch: quark.ErrWebsiteBindingMismatch,
	}
	return source.withPage(quark.WebsiteOrigin+"/list", false, func(page websitePage) websiteSession {
		return quarkWebsiteSession(page.Call, quarkcdp.Transfer(page.Tab))
	}).route()
}

func quarkWebsiteSession(call quark.WebsiteCall, transfer quark.WebsiteTransfer) websiteSession {
	return websiteSession{
		Account: func(ctx context.Context) (string, error) { return quark.WebsiteAccount(ctx, call) },
		Mount: func(ctx context.Context, account, root, rootID string, release func() error) (websiteDriver, error) {
			return quark.OpenWebsite(ctx, quark.WebsiteOptions{
				Call: call, Transfer: transfer, Account: account, RootPath: root, RootID: rootID, Release: release,
			})
		},
	}
}

var openLanZouWebsite = func(ctx context.Context, binary string, options []chromedp.ExecAllocatorOption, profile string) (lanzou.WebsiteOptions, error) {
	page, err := openWebsitePage(ctx, binary, append(options, chromedp.VisibleWindow), profile, lanzou.WebsiteURL, nil)
	return lanzou.WebsiteOptions{Call: page.Call, Tab: page.Tab, Release: page.Release}, err
}

type lanZouOptions struct {
	Root string `json:"root_id"`
}

type lanZouGrant struct {
	Provider string `json:"provider"`
	Account  string `json:"account,omitempty"`
	Root     string `json:"root_id"`
}

func lanZouWebsiteRoute() cliapp.Route {
	r := cliapp.SourceRoute(nil, prepareLanZouWebsite)
	r.SignIn, r.BindingMismatch = lanzou.ErrWebsiteSignIn, lanzou.ErrWebsiteBindingMismatch
	r.SignInProblem = &cliapp.Problem{Code: string(core.UpstreamAccessDenied), Message: "The LanZou sign-in has expired.", NextAction: "Sign in again.", Reconnect: true}
	return attachWebsiteSetup(r, websiteLogin{Name: "LanZou Cloud", Entry: lanzou.LoginURL, ReplaceReference: true}, restoreLanZou, verifyLanZou)
}

func prepareLanZouWebsite(opts lanZouOptions, _ string) (cliapp.MountSource, error) {
	if !lanzou.ValidRootID(opts.Root) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("LanZou needs a verified folder ID. Use mweave mount add to select it.")
	}
	return cliapp.MountSource{
		ProbeOnChange: true,
		Audience:      cliapp.CredentialAudience{Origin: lanzou.WebsiteURL},
		OpenManaged: func(ctx context.Context, id string) (core.Driver, error) {
			return openLanZouMount(ctx, id, opts.Root)
		},
	}, nil
}

func loadLanZouGrant(read func() ([]byte, error), root string) (lanZouGrant, error) {
	return readWebsiteGrant(read, func(bound lanZouGrant) bool {
		return bound.Provider == cliapp.LanZouSource && bound.Root == root
	})
}

func restoreLanZou(opts lanZouOptions, read func() ([]byte, error)) (websiteBinding[lanZouGrant], error) {
	bound, err := loadLanZouGrant(read, opts.Root)
	if err == nil && bound.Provider == "" && !lanzou.ValidRootID(opts.Root) {
		err = cliapp.InvalidConfig("LanZou needs a verified folder ID. Use mweave mount add to select it.")
	}
	bound.Root = opts.Root
	folder := "Saved LanZou folder"
	if bound.Root == lanzou.RootFolderID {
		folder = "/"
	}
	return websiteBinding[lanZouGrant]{Native: bound, Folder: folder, Verified: bound.Account != ""}, err
}

func verifyLanZou(ctx context.Context, candidate *websiteCandidate, bound lanZouGrant) (websiteSelection, error) {
	if !candidate.Existing {
		bound.Root = lanzou.RootFolderID
	}
	d, err := openLanZouDriver(ctx, candidate.Browser, candidate.Options, candidate.Profile, bound, nil)
	if err != nil {
		return websiteSelection{}, err
	}
	candidate.Release = d.Close
	folder, root, err := chooseWebsiteFolder(candidate, "LanZou Cloud", "/", !candidate.Existing, func(path string) (string, error) {
		return d.FolderID(ctx, path)
	})
	if err != nil {
		return websiteSelection{}, err
	}
	return websiteSelection{Options: lanZouOptions{Root: root}, Grant: lanZouGrant{cliapp.LanZouSource, d.Account(), root}, Folder: folder}, nil
}

func aliPanWebsiteRoute() cliapp.Route {
	r := cliapp.SourceRoute(nil, prepareAliPanWebsite)
	r.SignIn, r.SignInProblem = alipan.ErrWebsiteSignIn, websiteSignInProblem("Aliyun Drive")
	r.BindingMismatch = alipan.ErrWebsiteBindingMismatch
	return attachWebsiteSetup(r, websiteLogin{
		Name: "Aliyun Drive", Entry: alipan.WebsiteLoginURL, SeparateGrantID: true,
		Instructions: "Close the sign-in window when the plain text page appears.",
	}, restoreAliPan, verifyAliPan)
}

var openAliPanWebsite = func(ctx context.Context, binary string, options []chromedp.ExecAllocatorOption, profile string) (alipan.Website, error) {
	page, err := openWebsitePage(ctx, binary, options, profile, alipan.WebsiteURL, alipan.WebsiteWritePaths())
	return alipan.Website{Call: page.Call, Release: page.Release}, err
}

func restoreAliPan(options aliPanWebsiteOptions, read func() ([]byte, error)) (websiteBinding[aliPanWebsiteOptions], error) {
	grant, err := readAliPanWebsite(read, options)
	binding := websiteBinding[aliPanWebsiteOptions]{Native: options, Profile: grant.Profile, Folder: "Saved resource drive folder", Verified: true}
	if options.RootID == "root" {
		binding.Folder = "/"
	}
	if options.Write != nil {
		binding.Access = "Browse, download and upload"
	}
	return binding, err
}

func verifyAliPan(ctx context.Context, candidate *websiteCandidate, options aliPanWebsiteOptions) (websiteSelection, error) {
	site, err := openAliPanWebsite(ctx, candidate.Browser, candidate.Options, candidate.Profile)
	if err != nil {
		return websiteSelection{}, err
	}
	candidate.Release = site.Release
	account, err := site.Account(ctx)
	if errors.Is(err, alipan.ErrWebsiteSignIn) {
		return websiteSelection{}, &cliapp.Problem{Code: "interaction_required", Message: "The browser did not retain an Aliyun Drive sign-in.", NextAction: "Retry setup, finish signing in and close the window before continuing. Leave the site's remember sign-in option enabled if offered."}
	}
	if err != nil {
		return websiteSelection{}, err
	}
	var folder string
	if candidate.Existing {
		if account.ID != options.Account || account.ResourceDrive != options.Drive {
			return websiteSelection{}, cliapp.WebsiteMountScopeConflict()
		}
		// Verify sibling roots before the edited root, once per root ID.
		if candidate.Shared != nil {
			checked := map[string]bool{options.RootID: true}
			for source := range candidate.Shared {
				var bound aliPanWebsiteOptions
				if !source.AliPanWebsite() || cliapp.DecodeObject(source.Options, &bound) != nil || bound.Account != options.Account || bound.Drive != options.Drive {
					return websiteSelection{}, cliapp.AuthorizationScopeConflict()
				}
				if !checked[bound.RootID] {
					if err := account.CheckRoot(ctx, bound.RootID); err != nil {
						return websiteSelection{}, err
					}
					checked[bound.RootID] = true
				}
			}
		}
		if err := account.CheckRoot(ctx, options.RootID); err != nil {
			return websiteSelection{}, err
		}
	} else {
		var root string
		folder, root, err = chooseWebsiteFolder(candidate, "the resource drive", "", true, func(path string) (string, error) {
			d, root, err := account.Open(ctx, "", path, nil, nil)
			if err == nil {
				candidate.Release = d.Close
			}
			return root, err
		})
		if err != nil {
			return websiteSelection{}, err
		}
		options = aliPanWebsiteOptions{Account: account.ID, Drive: account.ResourceDrive, RootID: root}
	}
	grant := aliPanWebsiteRecord{cliapp.AliPanWebsiteSource, candidate.ID, options.Account, options.Drive}
	return websiteSelection{Options: options, Grant: grant, Folder: folder, Account: account.Name}, nil
}

func openLanZouMount(ctx context.Context, id, root string) (core.Driver, error) {
	return reopenWebsite(id, func(read func() ([]byte, error)) (lanZouGrant, string, error) {
		grant, err := loadLanZouGrant(read, root)
		return grant, id, err
	}, func(grant lanZouGrant, binary string, options []chromedp.ExecAllocatorOption, profile string, closeGrant func() error) (core.Driver, error) {
		return cliapp.NativeDriver(openLanZouDriver(ctx, binary, options, profile, grant, closeGrant))
	})
}

func openLanZouDriver(ctx context.Context, binary string, options []chromedp.ExecAllocatorOption, profile string, bound lanZouGrant, closeGrant func() error) (_ *lanzou.Driver, err error) {
	site, err := openLanZouWebsite(ctx, binary, options, profile)
	if err != nil {
		return nil, err
	}
	closeBrowser := site.Release
	defer func() {
		if err != nil {
			err = errors.Join(err, closeBrowser())
		}
	}()
	site.RootID, site.Account = bound.Root, bound.Account
	if closeGrant != nil {
		site.Release = func() error { return errors.Join(closeBrowser(), closeGrant()) }
	}
	return lanzou.OpenWebsite(ctx, site)
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

func prepareAliPanWebsite(opts aliPanWebsiteOptions, _ string) (cliapp.MountSource, error) {
	if opts.Account == "" || opts.Drive == "" || opts.RootID == "" {
		return cliapp.MountSource{}, cliapp.InvalidConfig("The Aliyun Drive website mount needs a verified account, resource drive and folder.")
	}
	return cliapp.MountSource{
		ProbeOnChange: true,
		Audience:      cliapp.CredentialAudience{Origin: alipan.WebsiteURL, Account: opts.Account},
		OpenManaged: func(ctx context.Context, id string) (core.Driver, error) {
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

type aliPanWebsiteSessions struct {
	mu   sync.Mutex
	byID map[string]*aliPanWebsiteSession
}

func openAliPanWebsiteMount(ctx context.Context, id string, opts aliPanWebsiteOptions) (core.Driver, error) {
	if bindings, ok := ctx.Value(cliapp.MountBindingsKey{}).(map[any]any); ok {
		key := aliPanWebsiteSessionsKey{}
		sessions, _ := bindings[key].(*aliPanWebsiteSessions)
		if sessions == nil {
			sessions = &aliPanWebsiteSessions{byID: make(map[string]*aliPanWebsiteSession)}
			bindings[key] = sessions
		}
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
	d, err := s.account.OpenBound(ctx, opts.Account, opts.Drive, opts.RootID, opts.Write, nil, release)
	if err != nil {
		return nil, errors.Join(err, release())
	}
	return d, nil
}

func loadAliPanWebsiteSession(ctx context.Context, id string, opts aliPanWebsiteOptions) (*aliPanWebsiteSession, error) {
	return reopenWebsite(id, func(read func() ([]byte, error)) (aliPanWebsiteRecord, string, error) {
		record, err := readAliPanWebsite(read, opts)
		return record, record.Profile, err
	}, func(_ aliPanWebsiteRecord, binary string, options []chromedp.ExecAllocatorOption, profile string, closeGrant func() error) (*aliPanWebsiteSession, error) {
		site, err := openAliPanWebsite(ctx, binary, options, profile)
		if err != nil {
			return nil, err
		}
		account, err := site.Account(ctx)
		if err != nil {
			return nil, errors.Join(err, site.Release())
		}
		release := sync.OnceValue(func() error { return errors.Join(site.Release(), closeGrant()) })
		return &aliPanWebsiteSession{account: account, users: 1, release: release}, nil
	})
}

func readAliPanWebsite(read func() ([]byte, error), opts aliPanWebsiteOptions) (aliPanWebsiteRecord, error) {
	return readWebsiteGrant(read, func(record aliPanWebsiteRecord) bool {
		return record.Provider == cliapp.AliPanWebsiteSource && cliapp.ValidGrantID(record.Profile) &&
			record.Account == opts.Account && record.Drive == opts.Drive
	})
}
