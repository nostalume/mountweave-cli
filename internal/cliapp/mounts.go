package cliapp

import (
	"bytes"
	"context"
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

type mountSummary struct {
	At              string   `json:"at"`
	Driver          string   `json:"driver"`
	DriverAvailable bool     `json:"driver_available"`
	Connection      string   `json:"connection,omitempty"`
	Problem         *Problem `json:"problem,omitempty"`
}

type MountReceipt struct {
	At     string `json:"at"`
	Effect string `json:"effect"`
}

func (app *App) runMount(ctx context.Context, saved ConfigSnapshot, a arguments, in io.Reader, diagnostics io.Writer) (any, string, error) {
	if a.command.name != "list" {
		return app.changeMount(ctx, saved, a, in, diagnostics)
	}
	items := app.mountSummaries(saved)
	var lines strings.Builder
	if !a.json {
		for _, item := range items {
			fmt.Fprintln(&lines, item.label())
			if item.Problem != nil && item.Problem.NextAction != "" {
				for _, line := range strings.Split(item.Problem.NextAction, "\n") {
					fmt.Fprintln(&lines, "  "+line)
				}
			}
		}
	}
	return struct {
		Items []mountSummary `json:"items"`
	}{items}, lines.String(), nil
}

func (app *App) changeMount(ctx context.Context, saved ConfigSnapshot, a arguments, in io.Reader, diagnostics io.Writer) (any, string, error) {
	base := filepath.Dir(saved.Path)
	var at string
	if len(a.operands) != 0 {
		at = mountedPath(a.operands[0], true)
		if err := core.ValidatePath(at); err != nil {
			return nil, "", err
		}
	}
	var ask Prompt
	if a.from == "" && a.command.name != "remove" {
		defer closeOnCancel(ctx, in)()
		ask = promptInput(in, diagnostics)
	}
	if a.command.name == "edit" && at == "" && ask != nil {
		var err error
		at, err = app.chooseMount(saved, ask)
		if err != nil {
			return nil, "", err
		}
	}
	var prior MountRecord
	if a.command.name != "add" {
		i := slices.IndexFunc(saved.Value.Mounts, func(m MountRecord) bool { return m.At == at })
		if i < 0 {
			return nil, "", core.Fail(core.NotFound, nil)
		}
		prior = saved.Value.Mounts[i]
		if a.command.name == "remove" {
			saved.Value.Mounts = slices.Delete(saved.Value.Mounts, i, i+1)
			applied, err := saved.Save(ctx)
			return mountSaveResult(prior.At, "Removed mount %q from configuration.\n", applied, err)
		}
	}
	priorRoute, priorErr := app.mountRoute(prior)
	var authorization ManagedAuthorization
	var localError error
	var websiteSettings bool
	if ask != nil && prior.Driver != "" {
		if priorErr != nil {
			return nil, "", priorErr
		}
		localError = inspectMount(prior, priorRoute, base)
		if localError != nil {
			if _, err := fmt.Fprintln(diagnostics, AppProblem(app, localError).Message); err != nil {
				return nil, "", err
			}
		}
		if priorRoute.Setup != nil {
			if priorRoute.InspectWebsite != nil {
				current := "settings"
				if p := AppProblem(app, localError); p != nil && p.Reconnect {
					current = "reconnect"
				}
				choice, err := askMenu(ask, "Edit mount", current, []string{"settings", "reconnect"}, []string{"Edit settings", "Sign in again"})
				if err != nil {
					return nil, "", err
				}
				websiteSettings = choice == "settings"
			}
			if !websiteSettings {
				receipt, line, err := priorRoute.Setup(ctx, saved, prior, prior, ask, diagnostics)
				return receipt, line, priorRoute.ProjectError(err)
			}
		}
		authorization = priorRoute.managed
	}
	var candidate mountCandidate
	var err error
	action := "settings"
	var accountLabel string
	if ask != nil && authorization.available() && prior.Credential != nil && prior.Credential.Kind == ManagedCredential {
		choices := []string{"settings", "switch"}
		prompt := "Edit mount"
		if MayReconnectGrant(localError) {
			action, choices = "reconnect", []string{"reconnect", "settings"}
			prompt = fmt.Sprintf("Reconnect %q to %s using its saved folder", prior.At, authorization.Label)
		} else if authorization.Kind != StaticGrant {
			choices = append(choices, "reauthorize")
		}
		action, err = AskMenu(ask, prompt, action, choices...)
		if err != nil {
			return nil, "", err
		}

	}
	reconnecting := action == "reconnect"
	switch {
	case ask == nil:
		candidate, err = app.readMountRecord(ctx, a.from, prior, priorRoute, base, in)
	case action == "settings":
		candidate, err = app.promptMount(ctx, at, prior, priorRoute, saved, ask, diagnostics)
		if route := candidate.source.Route; err == nil && route.Setup != nil {
			if !websiteSettings {
				receipt, line, err := route.Setup(ctx, saved, prior, candidate.record, ask, diagnostics)
				return receipt, line, route.ProjectError(err)
			}
			candidate.source, err = PrepareMountSource(app, candidate.record, base)
		}
	default:
		src, err := priorRoute.prepareMount(&prior.Options, base, nil)
		if err == nil {
			_, err = admitMount(app, prior, src, saved, CredentialRecords(saved.Value.Mounts))
		}
		if err != nil {
			return nil, "", err
		}
		var old OAuthGrant
		if !reconnecting {
			old, err = authorization.LoadGrant(prior.Credential.Ref)
			if err != nil {
				return nil, "", err
			}
			if action == "reauthorize" {
				return authorization.reauthorize(ctx, app, saved, priorRoute, old, ask, diagnostics)
			}
		}
		scopes, err := authorization.requiredScopes(prior.Options)
		if err != nil {
			return nil, "", err
		}
		selected, err := authorization.selectGrant(ctx, ask, diagnostics, scopes, old, false, reconnecting)
		if err != nil {
			return nil, "", err
		}
		m := prior
		m.Credential, accountLabel = selected.ref, selected.label
		if !reconnecting && authorization.ChooseRoot != nil {
			m.Options, err = authorization.ChooseRoot(ctx, m.Options, m.Credential, selected.fresh, scopes, ask, true)
			if err == nil && !bytes.Equal(m.Options, prior.Options) {
				src, err = priorRoute.prepareMount(&m.Options, base, nil)
			}
		}
		if err == nil && !reconnecting && src.Route.review == "" {
			err = Confirm(ask, "Use this authorization for the mount?")
		}
		if err != nil {
			return nil, "", err
		}
		candidate = mountCandidate{record: m, source: src, fresh: selected.fresh}
	}
	if cancelErr := ctx.Err(); cancelErr != nil {
		return nil, "", cancelErr
	}
	if err == nil && ask != nil {
		err = validateRecord(candidate.record)
	}
	if err != nil {
		return nil, "", err
	}
	m, src, grant := candidate.record, candidate.source, candidate.fresh
	unchanged := MountReceipt{m.At, core.Unchanged.String()}
	open, err := admitMount(app, m, src, saved, CredentialRecords(saved.Value.Mounts))
	if err != nil {
		return unchanged, "", err
	}
	credentialChanged := (m.Credential != nil || prior.Credential != nil) &&
		!sameReference(m.Credential, prior.Credential, base)
	optionsChanged := !bytes.Equal(m.Options, prior.Options)
	probe := reconnecting || grant != nil || prior.At == "" || credentialChanged || m.Driver != prior.Driver || src.Route != priorRoute || src.ProbeOnChange && optionsChanged
	reviewLocation := ask != nil && src.Route.review != ""
	if !probe && optionsChanged && (src.KeyFile != "" && !candidate.sameKeyFile || reviewLocation) {
		old, err := priorRoute.prepareMount(&prior.Options, base, nil)
		probe = err != nil || src.KeyFile != "" && old.KeyFile != src.KeyFile || reviewLocation && old.Location != src.Location
	}
	return CommitMount(ctx, saved, prior, m, func(next ConfigSnapshot) (string, error) {
		if probe {
			var err error
			if grant != nil {
				err = src.Route.managed.probeGrant(ctx, src, m, *grant)
			} else {
				d, openErr := app.openMountDriver(ctx, src, open, next, m, CredentialRecords(next.Value.Mounts, prior), prior.At == "")
				err = src.Route.ProjectError(src.verifyAndClose(ctx, d, openErr))
			}
			if err != nil {
				return "", err
			}
		}
		if reviewLocation {
			verification := "Existing location retained"
			if probe {
				verification = "Selected location verified"
			}
			location := src.Location
			if location.Quote {
				location.Location = strconv.Quote(location.Location)
			}
			if _, err := fmt.Fprintf(diagnostics, "\nReview mount\n  Connection   %s\n  Mount        %q\n  Location     %s\n  Access       %s\n  Verification %s\n",
				src.Route.review, m.At, location.Location, location.Access, verification); err != nil {
				return "", err
			}
		}
		switch {
		case reconnecting:
			if accountLabel != "" {
				if _, err := fmt.Fprintf(diagnostics, "  Account     %q\n", accountLabel); err != nil {
					return "", err
				}
			}
			if err := ConfirmRestoredBinding(ask, diagnostics, m.At); err != nil {
				return "", err
			}
		case reviewLocation:
			if err := Confirm(ask, "Save mount?"); err != nil {
				return "", err
			}
		}
		if grant != nil {
			if err := grant.persistNew(ctx); err != nil {
				return "", err
			}
			if _, err := fmt.Fprintf(diagnostics, "Saved authorization for %q.\n", grant.Label); err != nil {
				return "", err
			}
		}
		if probe {
			return verifiedMountMessage, nil
		}
		return "Saved mount %q.\n", nil
	}, nil)
}

const verifiedMountMessage = "Saved mount %q after verifying the selected location.\n"

// CommitMount retains a mount-owned website grant only if the configuration is published.
func CommitMount(ctx context.Context, saved ConfigSnapshot, prior, m MountRecord, verify func(ConfigSnapshot) (string, error), websiteGrant []byte) (receipt MountReceipt, message string, err error) {
	unchanged := MountReceipt{m.At, core.Unchanged.String()}
	if err := ctx.Err(); err != nil {
		return unchanged, "", err
	}
	if MountPathConflicts(saved.Value.Mounts, m.At, prior.At) {
		return unchanged, "", ConfigConflict()
	}
	if prior.At == "" {
		saved.Value.Mounts = append(saved.Value.Mounts, m)
	} else {
		index := slices.IndexFunc(saved.Value.Mounts, func(item MountRecord) bool { return item.At == prior.At })
		if index < 0 {
			return unchanged, "", ConfigConflict()
		}
		saved.Value.Mounts = slices.Clone(saved.Value.Mounts)
		saved.Value.Mounts[index] = m
	}
	// Resolve export conflicts before the new destination can receive a probe.
	if err := saved.Value.validate(); err != nil {
		return unchanged, "", err
	}
	message = verifiedMountMessage
	if verify != nil {
		if err := CheckStorageLocations(saved, true); err != nil {
			return unchanged, "", err
		}
		message, err = verify(saved)
		if err != nil {
			return unchanged, "", err
		}
	}
	if websiteGrant != nil {
		if m.Credential == nil || m.Credential.Kind != ManagedCredential {
			return unchanged, "", GrantUnavailable()
		}
		lease, grantErr := AcquireGrant(m.Credential.Ref, true)
		if grantErr != nil {
			return unchanged, "", grantErr
		}
		defer func() { err = errors.Join(err, lease.Lock.Close()) }()
		published, grantErr := lease.Save(ctx, websiteGrant, false)
		defer func() {
			if published && receipt.Effect != core.Applied.String() {
				err = errors.Join(err, os.Remove(lease.Path))
			}
		}()
		if grantErr != nil {
			return unchanged, "", grantErr
		}
	}
	applied, saveErr := saved.saveValidated(ctx)
	return mountSaveResult(m.At, message, applied, saveErr)
}

func mountSaveResult(at, message string, applied bool, err error) (MountReceipt, string, error) {
	if !applied {
		return MountReceipt{at, core.Unchanged.String()}, "", err
	}
	return MountReceipt{at, core.Applied.String()}, fmt.Sprintf(message, at), err
}

func MountPathConflicts(mounts []MountRecord, at, replacing string) bool {
	return slices.ContainsFunc(mounts, func(other MountRecord) bool {
		return other.At != replacing && (core.Within(at, other.At) || core.Within(other.At, at))
	})
}
