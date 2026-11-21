package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	core "github.com/nostalume/mountweave"
)

func ConfirmRestoredBinding(ask Prompt, out io.Writer, at string) error {
	if _, err := fmt.Fprintf(out, "The new authorization can open the saved folder for %q. The previous account could not be verified.\n", at); err != nil {
		return err
	}
	return Confirm(ask, "Bind this mount to the newly authorized account?")
}

type mountCandidate struct {
	record MountRecord
	source MountSource
	fresh  *OAuthGrant
}

func (app *App) readMountRecord(ctx context.Context, name string, prior MountRecord, priorRoute *Route, base string, in io.Reader) (mountCandidate, error) {
	var candidate mountCandidate
	m := &candidate.record
	data, err := readRecord(ctx, name, in)
	if err != nil {
		return candidate, err
	}
	if err := DecodeObject(data, m); err != nil {
		return candidate, err
	}
	if err := validateRecord(*m); err != nil {
		return candidate, err
	}
	candidate.source, err = PrepareMountSource(app, *m, base)
	src := &candidate.source
	if err != nil || m.Credential != nil || prior.Credential == nil ||
		m.Driver != prior.Driver || src.Route != priorRoute {
		return candidate, err
	}
	switch {
	case src.OptionalCredential:
		// The typed decode above has already validated the record.
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(data, &fields); err == nil && fields["credential"] == nil {
			m.Credential = prior.Credential
		}
	case src.Audience.Origin == "":
	case src.KeyFile == "" || bytes.Equal(prior.Options, m.Options):
		m.Credential = prior.Credential
	default:
		if old, err := priorRoute.prepareMount(&prior.Options, base, nil); err == nil && old.Audience == src.Audience {
			m.Credential = prior.Credential
		}
	}
	return candidate, err
}

func (app *App) promptMount(ctx context.Context, at string, prior MountRecord, r *Route, saved ConfigSnapshot, ask Prompt, out io.Writer) (mountCandidate, error) {
	m := prior
	if prior.Driver == "" {
		heading := "Add mount"
		if at != "" {
			heading = fmt.Sprintf("Add mount %q", at)
		}
		if _, err := fmt.Fprintln(out, heading+"\n"); err != nil {
			return mountCandidate{}, err
		}
		source, err := app.promptSource(ask, out)
		if err != nil {
			return mountCandidate{}, err
		}
		m.At, m.Driver = at, source
	}
	choice := app.sources[m.Driver]
	if prior.Driver != "" || m.At == "" {
		var err error
		m.At, err = ask("Mount path (for example /files)", m.At)
		if err != nil {
			return mountCandidate{}, err
		}
	}
	if err := core.ValidatePath(m.At); err != nil {
		return mountCandidate{}, err
	}
	if MountPathConflicts(saved.Value.Mounts, m.At, prior.At) {
		return mountCandidate{}, ConfigConflict()
	}
	if _, err := fmt.Fprintf(out, "\nConnect to %s\n", choice.label); err != nil {
		return mountCandidate{}, err
	}
	var connection SourceConnection
	if prior.Driver == "" {
		connections := slices.DeleteFunc(slices.Clone(choice.connections), func(c SourceConnection) bool { return !c.guided() })
		connection = connections[0]
		if len(connections) > 1 {
			var methods []string
			for _, item := range connections {
				methods = append(methods, item.Method)
				if _, err := fmt.Fprintf(out, "  %-8s %s\n", item.Method, item.Label); err != nil {
					return mountCandidate{}, err
				}
			}
			method, err := AskChoice(ask, "Connection", connection.Method, methods...)
			if err != nil {
				return mountCandidate{}, err
			}
			connection = connections[slices.Index(methods, method)]
		}
		m.Connection, m.Options = connection.Method, connection.Defaults
		r = connection.Route
	}
	if r.Setup != nil {
		return mountCandidate{record: m, source: MountSource{Route: r}}, nil
	}
	if connection.ConfirmSetup {
		answer, err := AskChoice(ask, "WebDAV authorization (existing to reuse credentials, setup to open the setup page)", "existing", "existing", "setup")
		if err != nil {
			return mountCandidate{}, err
		}
		if answer == "existing" {
			connection.SetupURL, connection.SetupHint = "", ""
		}
	}
	if connection.SetupHint != "" {
		if _, err := fmt.Fprintln(out, connection.SetupHint); err != nil {
			return mountCandidate{}, err
		}
	}
	if connection.SetupURL != "" {
		openCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		openErr := LaunchOAuthBrowser(openCtx, connection.SetupURL)
		stop()
		if err := ctx.Err(); err != nil {
			return mountCandidate{}, err
		}
		if openErr != nil {
			if _, err := fmt.Fprintf(out, "Open this address in your browser: %s\n", connection.SetupURL); err != nil {
				return mountCandidate{}, err
			}
		}
	}
	preparePrompt := ask
	var fresh *OAuthGrant
	var err error
	if r.managed.available() && prior.Driver == "" && ManagedStoreSupported {
		method := r.managed.Method
		if !r.managed.ManagedOnly && !r.managed.ReferenceInGrant {
			method, err = AskChoice(ask, "Authorization ("+method+" or reference)", method, method, "reference")
			if err != nil {
				return mountCandidate{}, err
			}
		}
		if method == r.managed.Method {
			var scopes []string
			if r.managed.ChooseScopes != nil {
				scopes, err = r.managed.ChooseScopes(ask)
			} else {
				scopes, err = r.managed.requiredScopes(m.Options)
			}
			if err != nil {
				return mountCandidate{}, err
			}
			selected, err := r.managed.selectGrant(ctx, ask, out, scopes, OAuthGrant{}, r.managed.ReferenceInGrant, false)
			if err != nil {
				return mountCandidate{}, err
			}
			m.Credential, fresh = selected.ref, selected.fresh
			if m.Credential != nil && r.managed.ChooseRoot != nil {
				m.Options, err = r.managed.ChooseRoot(ctx, m.Options, m.Credential, fresh, scopes, ask, false)
				if err != nil {
					return mountCandidate{}, err
				}
				preparePrompt = nil
			}
		}
	}
	if !r.Interactive {
		return mountCandidate{}, sourceNeedsRecord()
	}
	src, err := r.prepareMount(&m.Options, filepath.Dir(saved.Path), preparePrompt)
	if err != nil {
		return mountCandidate{}, err
	}
	keepManaged := m.Credential != nil && m.Credential.Kind == ManagedCredential && r.managed.available()
	if src.Audience.Origin == "" {
		m.Credential = nil
	} else if !keepManaged {
		policy := src.references()
		m.Credential, err = AskCredentialReference(ask, policy.prompt, m.Credential, func(raw string) (*CredentialRef, error) {
			switch {
			case policy.optional && (raw == "" || raw == "-"):
				return nil, nil
			case policy.kind == ManagedCredential:
				return &CredentialRef{Kind: ManagedCredential, Ref: raw}, nil
			default:
				return PromptReference(raw)
			}
		}, policy.check)
		if err != nil {
			return mountCandidate{}, err
		}
	}
	candidate := mountCandidate{record: m, source: src, fresh: fresh}
	if r.review != "" {
		return candidate, nil
	}
	if _, err := fmt.Fprintf(out, "\nReview mount\n  Source  %s\n  Mount   %q\n%s", choice.label, m.At, src.Preview); err != nil {
		return mountCandidate{}, err
	}
	return candidate, Confirm(ask, "Save mount?")
}

func (app *App) promptSource(ask Prompt, out io.Writer) (string, error) {
	names := app.sourceNames("", SourceConnection.guided)
	switch len(names) {
	case 0:
		return "", &Problem{Code: "interaction_required", Message: "No interactive sources are available.", NextAction: "See mweave mount add --help for the available sources."}
	case 1:
		return names[0], nil
	}
	var current string
	if slices.Contains(names, "local") {
		current = "local"
	}
	for {
		query, err := ask("Source name or type (? to list)", current)
		if err != nil {
			return "", err
		}
		query = strings.ToLower(strings.TrimSpace(query))
		if slices.Contains(names, query) {
			return query, nil
		}
		if source, known := app.sources[query]; known && slices.ContainsFunc(source.connections, SourceConnection.available) {
			return "", sourceNeedsRecord()
		}
		if query == "?" {
			if err := app.printSources(out, names); err != nil {
				return "", err
			}
			continue
		}
		matches := app.sourceNames(query, SourceConnection.guided)
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) == 0 {
			if _, err := fmt.Fprintln(out, "No matching source. Enter ? to list available sources."); err != nil {
				return "", err
			}
			continue
		}
		if _, err := fmt.Fprintln(out, "Matching sources"); err != nil {
			return "", err
		}
		if err := app.printSources(out, matches); err != nil {
			return "", err
		}
	}
}

func sourceNeedsRecord() error {
	return &Problem{Code: "interaction_required", Message: "This source uses a configuration record.", NextAction: "Provide a record with --from FILE or --from -."}
}
