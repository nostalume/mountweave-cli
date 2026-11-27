package stock

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"

	"github.com/chromedp/chromedp"
	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave-cli/internal/website"
)

// websiteLogin describes the browser entry and how the source identifies its saved grant.
type websiteLogin struct {
	Name, Entry, Instructions, Access string
	// ReplaceReference leaves the old grant and profile in place when replacing a mount.
	ReplaceReference bool
	// SeparateGrantID lets multiple roots share a grant whose profile can change.
	SeparateGrantID bool
}

type websiteBinding[T any] struct {
	Native                  T
	Profile, Folder, Access string
	Verified                bool
}

type websiteCandidate struct {
	ID, Profile, Browser string
	Options              []chromedp.ExecAllocatorOption
	Release              func() error
	Prompt               cliapp.Prompt
	Out                  io.Writer
	Existing             bool
	Shared               iter.Seq[cliapp.MountRecord]
}

type websiteSelection struct {
	Options, Grant  any
	Folder, Account string
}

// attachWebsiteSetup gives the source only the prior binding and native verification.
// The host owns confirmation, closing the browser, and publishing the grant.
func attachWebsiteSetup[O, T any](route cliapp.Route, login websiteLogin,
	restore func(O, func() ([]byte, error)) (websiteBinding[T], error),
	verify func(context.Context, *websiteCandidate, T) (websiteSelection, error),
) cliapp.Route {
	route.InspectWebsite = func(id string, raw json.RawMessage, read func() ([]byte, error)) error {
		var options O
		if err := cliapp.DecodeObject(raw, &options); err != nil {
			return err
		}
		if read == nil {
			return cliapp.GrantUnavailable()
		}
		binding, err := restore(options, read)
		if err != nil {
			return err
		}
		_, err = websiteProfile(cmp.Or(binding.Profile, id), false)
		return err
	}
	route.Setup = func(ctx context.Context, saved cliapp.ConfigSnapshot, prior, record cliapp.MountRecord, ask cliapp.Prompt, out io.Writer) (receipt cliapp.MountReceipt, _ string, err error) {
		at := record.At
		unchanged := cliapp.MountReceipt{At: at, Effect: core.Unchanged.String()}
		if !cliapp.ManagedStoreSupported {
			return unchanged, "", cliapp.GrantUnavailable()
		}
		if err := core.ValidatePath(at); err != nil {
			return unchanged, "", err
		}
		if cliapp.MountPathConflicts(saved.Value.Mounts, at, prior.At) {
			return unchanged, "", cliapp.ConfigConflict()
		}
		if err := cliapp.CheckWebsiteStorage(saved); err != nil {
			return unchanged, "", err
		}
		var binding websiteBinding[T]
		var old *cliapp.GrantLease
		if prior.Driver != "" {
			var options O
			if err := cliapp.DecodeObject(prior.Options, &options); err != nil {
				return unchanged, "", err
			}
			old, err = existingGrant(prior.Credential)
			if err != nil && !cliapp.MayReconnectGrant(err) {
				return unchanged, "", err
			}
			var read func() ([]byte, error)
			if old != nil {
				read = old.Read
			}
			binding, err = restore(options, read)
			if old != nil {
				if recoverGrant := cliapp.MayReconnectGrant(err); login.ReplaceReference || recoverGrant {
					closeErr := old.Lock.Close()
					old = nil
					if recoverGrant {
						// A fresh reference leaves the unreadable grant and its other mounts intact.
						binding, err = restore(options, nil)
					}
					err = errors.Join(err, closeErr)
				} else {
					defer func() { err = errors.Join(err, old.Lock.Close()) }()
				}
			}
			if err != nil {
				return unchanged, "", err
			}
		}
		candidate := websiteCandidate{ID: cliapp.NewGrantID(), Existing: prior.Driver != "", Prompt: ask, Out: out}
		candidate.Profile, err = websiteProfile(candidate.ID, true)
		if err != nil {
			return unchanged, "", err
		}
		defer func() {
			err = errors.Join(err, candidate.close())
			if receipt.Effect != core.Applied.String() {
				err = errors.Join(err, removeProfile(candidate.Profile))
			}
		}()
		if err := login.show(out, prior.At, binding.Folder, binding.Access); err != nil {
			return unchanged, "", err
		}
		candidate.Browser, candidate.Options = website.Browser()
		if err := signInWebsite(ctx, candidate.Browser, candidate.Profile, login.Entry, ask, out); err != nil {
			return unchanged, "", err
		}
		if old != nil {
			candidate.Shared = func(yield func(cliapp.MountRecord) bool) {
				for m := range cliapp.CredentialRecords(saved.Value.Mounts) {
					if *m.Credential == *prior.Credential && !yield(m) {
						return
					}
				}
			}
		}
		selected, err := verify(ctx, &candidate, binding.Native)
		if err != nil {
			return unchanged, "", err
		}
		if err := confirmWebsiteMount(ask, out, prior, at, selected.Folder, selected.Account, cmp.Or(binding.Access, login.Access, websiteReadAccess), binding.Verified); err != nil {
			return unchanged, "", err
		}
		if err := candidate.close(); err != nil {
			return unchanged, "", err
		}
		data, err := json.Marshal(selected.Grant)
		if err != nil {
			return unchanged, "", err
		}
		defer clear(data)
		if old != nil {
			return replaceGrant(ctx, saved, at, old, binding.Profile, data)
		}
		id := candidate.ID
		if login.SeparateGrantID {
			id = cliapp.NewGrantID()
		}
		record.Credential = &cliapp.CredentialRef{Kind: cliapp.ManagedCredential, Ref: id}
		record.Options, err = json.Marshal(selected.Options)
		if err != nil {
			return unchanged, "", err
		}
		return cliapp.CommitMount(ctx, saved, prior, record, nil, data)
	}
	return route
}

func chooseWebsiteFolder[T any](candidate *websiteCandidate, name, folder string, pick bool, verify func(string) (T, error)) (string, T, error) {
	for {
		if pick {
			chosen, err := candidate.Prompt("Folder on "+name, "/")
			if err != nil {
				var zero T
				return "", zero, err
			}
			folder = chosen
		}
		result, err := verify(folder)
		if err == nil || !pick {
			return folder, result, err
		}
		switch core.CodeOf(err) {
		case core.InvalidPath, core.NotFound, core.NotDirectory, core.PathAmbiguous:
		default:
			return folder, result, err
		}
		if _, err := fmt.Fprintln(candidate.Out, "Try another folder. Enter an existing, unambiguous path such as /Documents."); err != nil {
			return folder, result, err
		}
	}
}

func websiteSignInProblem(name string) *cliapp.Problem {
	return &cliapp.Problem{Code: "interaction_required", Message: "The " + name + " website needs sign-in.", NextAction: "Sign in again.", Reconnect: true}
}

const (
	websiteReadAccess  = "Browse and download"
	closeWebsiteSignIn = "Close the sign-in window when your files appear."
)

func (login websiteLogin) show(out io.Writer, at, folder, access string) error {
	message := fmt.Sprintf("Sign in to %s. %s\n", login.Name, cmp.Or(login.Instructions, closeWebsiteSignIn))
	if at != "" {
		message = fmt.Sprintf("Reconnect %q to %s\n  Folder   %q\n  Access   %s\n\n%s", at, login.Name, folder, cmp.Or(access, login.Access, websiteReadAccess), message)
	}
	_, err := io.WriteString(out, message)
	return err
}

func confirmWebsiteMount(ask cliapp.Prompt, out io.Writer, prior cliapp.MountRecord, at, folder, account, access string, bindingVerified bool) error {
	if prior.Driver != "" {
		if bindingVerified {
			return nil
		}
		return cliapp.ConfirmRestoredBinding(ask, out, at)
	}
	review := "\nReview mount\n"
	if account != "" {
		review += fmt.Sprintf("  Account  %q\n", account)
	}
	return cliapp.Confirm(ask, fmt.Sprintf("%s  Folder   %q\n  Mount    %q\n  Access   %s\nSave mount?", review, folder, at, access))
}

func (c *websiteCandidate) close() error {
	if close := c.Release; close != nil {
		c.Release = nil
		return close()
	}
	return nil
}

// An absent grant can be replaced for the selected mount.
func existingGrant(ref *cliapp.CredentialRef) (*cliapp.GrantLease, error) {
	if ref == nil || ref.Kind != cliapp.ManagedCredential {
		return nil, cliapp.GrantUnavailable()
	}
	missing, err := cliapp.MissingGrant(ref.Ref)
	if err != nil || missing {
		return nil, err
	}
	return cliapp.AcquireGrant(ref.Ref, false)
}

func readWebsiteGrant[T any](read func() ([]byte, error), bound func(T) bool) (zero T, _ error) {
	if read == nil {
		return zero, nil
	}
	raw, err := read()
	if err != nil {
		return zero, err
	}
	defer clear(raw)
	var grant T
	if cliapp.DecodeObject(raw, &grant) != nil || !bound(grant) {
		return zero, cliapp.AuthorizationScopeConflict()
	}
	return grant, nil
}

func websiteProfileRoot(grants string) string {
	return filepath.Join(grants, "website")
}

var missingWebsiteProfile = &cliapp.Problem{
	Code: "interaction_required", Message: "The saved browser session is missing on this machine.",
	NextAction: "Sign in again.", Reconnect: true,
}

func websiteProfile(id string, create bool) (string, error) {
	if !cliapp.ValidGrantID(id) {
		return "", cliapp.GrantUnavailable()
	}
	grants, err := cliapp.GrantDirectory(create)
	if err != nil {
		return "", err
	}
	root := websiteProfileRoot(grants)
	profile := filepath.Join(root, id)
	for _, path := range []string{root, profile} {
		if err := website.PrivateDirectory(path, create); err != nil {
			if !create && errors.Is(err, os.ErrNotExist) {
				return "", missingWebsiteProfile
			}
			return "", cliapp.GrantUnavailable()
		}
	}
	return profile, nil
}

func removeProfile(profile string) error {
	base, err := cliapp.GrantDirectory(false)
	if err != nil || filepath.Dir(profile) != websiteProfileRoot(base) || !cliapp.ValidGrantID(filepath.Base(profile)) {
		return cliapp.GrantUnavailable()
	}
	return os.RemoveAll(profile)
}

// replaceGrant adopts a verified profile under an existing grant lease.
func replaceGrant(ctx context.Context, saved cliapp.ConfigSnapshot, at string, lease *cliapp.GrantLease, oldProfile string, data []byte) (cliapp.MountReceipt, string, error) {
	receipt := cliapp.MountReceipt{At: at, Effect: core.Unchanged.String()}
	lock, err := saved.LockUnchanged(ctx)
	if err != nil {
		return receipt, "", err
	}
	published, err := lease.Save(ctx, data, true)
	err = errors.Join(err, lock.Close())
	if published {
		receipt.Effect = core.Applied.String()
	}
	if err != nil {
		return receipt, "", err
	}
	dir, err := websiteProfile(oldProfile, false)
	if err == nil {
		err = removeProfile(dir)
	} else if errors.Is(err, missingWebsiteProfile) {
		err = nil
	}
	return receipt, fmt.Sprintf("Signed in again for %q.\n", at), err
}
