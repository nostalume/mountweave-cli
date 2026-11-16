package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	core "github.com/nostalume/mountweave"
)

const localActor core.Principal = "local-cli"

func (app *App) install(ctx context.Context, c *core.Core, saved configSnapshot, m mountRecord) error {
	base := filepath.Dir(saved.path)
	src, err := app.prepareMountSource(m, base)
	if err != nil {
		return err
	}
	if err := app.checkAudience(m, src, saved, false); err != nil {
		return err
	}
	d, err := app.openMountDriver(ctx, src, saved, m, mountRecord{})
	if err != nil {
		return err
	}
	if _, err := c.Install(m.At, d); err != nil {
		return errors.Join(err, d.Close())
	}
	return nil
}

func (app *App) openFor(ctx context.Context, saved configSnapshot, targets ...core.Address) (*core.Core, error) {
	for _, target := range targets {
		if err := target.Validate(); err != nil {
			return nil, err
		}
	}
	c, err := core.New(core.Options{Authorize: func(context.Context, core.Principal, core.Action, core.Address) error { return nil }})
	if err != nil {
		return nil, err
	}
	ctx = withAliPanWebsiteSessions(ctx)
	checkedPrivatePaths := false
	for _, m := range saved.value.Mounts {
		for _, target := range targets {
			match := target.Space() == m.At
			if target.Namespace() == core.TreeNamespace {
				match = core.Within(target.Value(), m.At) || core.Within(m.At, target.Value())
			}
			if !match {
				continue
			}
			if !checkedPrivatePaths && (m.Driver == "local" || m.Driver == poolSourceID) {
				if err := checkStorageLocations(saved, true); err != nil {
					return nil, errors.Join(err, closeRuntime(c))
				}
				checkedPrivatePaths = true
			}
			if err := app.install(ctx, c, saved, m); err != nil {
				return nil, errors.Join(err, closeRuntime(c))
			}
			break
		}
	}
	return c, nil
}

func closeRuntime(c *core.Core) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Close(ctx)
}

type mountSummary struct {
	At              string `json:"at"`
	Driver          string `json:"driver"`
	DriverAvailable bool   `json:"driver_available"`
	Connection      string `json:"connection,omitempty"`
}

type mountReceipt struct {
	At     string `json:"at"`
	Effect string `json:"effect"`
}

func (app *App) runMount(ctx context.Context, saved configSnapshot, a arguments, in io.Reader, diagnostics io.Writer) (any, string, error) {
	switch a.command {
	case "mount list":
		items := make([]mountSummary, 0, len(saved.value.Mounts))
		var lines strings.Builder
		for _, m := range saved.value.Mounts {
			_, err := app.mountRoute(m)
			available := err == nil
			items = append(items, mountSummary{m.At, m.Driver, available, m.Connection})
			if !a.json {
				source, method := m.Driver, m.Connection
				if m.aliPanWebsite() {
					source, method = "alipan", "website"
				}
				fmt.Fprintf(&lines, "%q\t%q", m.At, source)
				if method != "" {
					fmt.Fprintf(&lines, " (%s)", method)
				}
				if !available {
					lines.WriteString("\t(Driver unavailable in this build)")
				}
				lines.WriteByte('\n')
			}
		}
		return struct {
			Items []mountSummary `json:"items"`
		}{items}, lines.String(), nil
	case "mount add", "mount edit", "mount remove":
		return app.changeMount(ctx, saved, a, in, diagnostics)
	}
	return nil, "", usage("Unknown command. Run mweave help.")
}

func (app *App) changeMount(ctx context.Context, saved configSnapshot, a arguments, in io.Reader, diagnostics io.Writer) (any, string, error) {
	base := filepath.Dir(saved.path)
	var prior mountRecord
	i := -1
	if a.command != "mount add" {
		if err := core.ValidatePath(a.operands[0]); err != nil {
			return nil, "", err
		}
		i = slices.IndexFunc(saved.value.Mounts, func(m mountRecord) bool { return m.At == a.operands[0] })
		if i < 0 {
			return nil, "", core.Fail(core.NotFound, nil)
		}
		prior = saved.value.Mounts[i]
	}
	switch a.command {
	case "mount remove":
		saved.value.Mounts = slices.Delete(saved.value.Mounts, i, i+1)
		return saveMount(ctx, saved, prior.At, "Removed mount %q from configuration.\n")
	}
	var ask askField
	if a.from == "" {
		if reader, ok := in.(io.Closer); ok {
			defer closeOnCancel(ctx, reader)()
		}
		ask = promptInput(in, diagnostics)
	}
	priorRoute, _ := app.mountRoute(prior)
	if ask != nil && priorRoute.setup != nil {
		return priorRoute.setup(ctx, saved, prior, prior.At, ask, diagnostics)
	}
	var m mountRecord
	var src mountSource
	var grant *oauthGrant
	var err error
	action := "settings"
	authorization := priorRoute.managed
	if ask != nil && authorization.available() && prior.Credential != nil && prior.Credential.Kind == managedCredential {
		choices := []string{"settings", "switch"}
		if !authorization.staticOnly {
			choices = append(choices, "reauthorize")
		}
		action, err = askChoice(ask, "Edit mount ("+strings.Join(choices, ", ")+")", action, choices...)
		if err != nil {
			return nil, "", err
		}
	}
	switch action {
	case "settings":
		if ask == nil {
			m, src, err = app.readMountRecord(ctx, a.from, prior, base, in)
		} else {
			var at string
			if len(a.operands) != 0 {
				at = a.operands[0]
			}
			m, src, err = app.promptMount(ctx, at, prior, saved, ask, diagnostics, &grant)
			if err == nil && prior.Driver == "" {
				selected, lookupErr := app.mountRoute(m)
				if lookupErr != nil {
					return nil, "", lookupErr
				}
				if selected.setup != nil {
					return selected.setup(ctx, saved, prior, m.At, ask, diagnostics)
				}
			}
		}
	case "reauthorize", "switch":
		m = prior
		src, err = app.prepareMountSource(prior, base)
		if err == nil {
			err = app.checkAudience(prior, src, saved, true)
		}
		if err != nil {
			return nil, "", err
		}
		old, err := authorization.loadGrant(prior.Credential.Ref)
		if err != nil {
			return nil, "", err
		}
		if action == "reauthorize" {
			return authorization.reauthorize(ctx, app, saved, app.routeID(prior), old, ask, diagnostics)
		}
		scopes, err := authorization.requiredScopes(prior.Options)
		if err != nil {
			return nil, "", err
		}
		m.Credential, grant, err = authorization.selectGrant(ctx, ask, diagnostics, scopes, old)
		if err == nil && authorization.chooseRoot != nil {
			m.Options, err = authorization.chooseRoot(ctx, m.Options, m.Credential, grant, scopes, ask, true)
			if err == nil {
				src, err = app.prepareMountSource(m, base)
			}
		}
		if err == nil {
			err = confirm(ask, "Use this authorization for the mount?")
		}
		if err != nil {
			return nil, "", err
		}
	}
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	if err != nil {
		return nil, "", err
	}
	if err := validateRecord(m); err != nil {
		return nil, "", err
	}
	unchanged := mountReceipt{m.At, core.Unchanged.String()}
	if err := app.checkAudience(m, src, saved, true); err != nil {
		return unchanged, "", err
	}
	credentialChanged := (m.Credential != nil || src.optionalCredential && prior.Credential != nil) &&
		!sameReference(m.Credential, prior.Credential, base)
	probe := i < 0 || credentialChanged || m.Driver != prior.Driver || app.routeID(m) != app.routeID(prior) || src.probeOnChange && !bytes.Equal(m.Options, prior.Options)
	if !probe && src.keyFile != "" {
		old, err := app.prepareMountSource(prior, base)
		probe = err != nil || old.keyFile != src.keyFile
	}
	verifyNewStaticGrant := grant != nil && app.routes[app.routeID(m)].managed.staticOnly
	return commitMount(ctx, saved, prior, m, func(next configSnapshot) (string, error) {
		if verifyNewStaticGrant {
			driver, err := src.open(ctx, grant.Access)
			if driver != nil {
				err = errors.Join(err, driver.Close())
			}
			if err != nil {
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
		if probe && !verifyNewStaticGrant {
			if err := app.probeMount(ctx, src, next, m, prior); err != nil {
				return "", err
			}
		}
		if probe || verifyNewStaticGrant {
			return verifiedMountMessage, nil
		}
		return "Saved mount %q.\n", nil
	}, nil)
}

const verifiedMountMessage = "Saved mount %q after verifying the selected location.\n"

// commitMount publishes the new mount record only after validating the full
// configuration and completing the relevant verification. A new website grant
// belongs to this mount and is removed if the configuration is not published.
func commitMount(ctx context.Context, saved configSnapshot, prior, m mountRecord, verify func(configSnapshot) (string, error), websiteGrant []byte) (receipt mountReceipt, message string, err error) {
	unchanged := mountReceipt{m.At, core.Unchanged.String()}
	if err := ctx.Err(); err != nil {
		return unchanged, "", err
	}
	if mountPathConflicts(saved.value.Mounts, m.At, prior.At) {
		return unchanged, "", configConflict()
	}
	if prior.At == "" {
		saved.value.Mounts = append(saved.value.Mounts, m)
	} else {
		index := slices.IndexFunc(saved.value.Mounts, func(item mountRecord) bool { return item.At == prior.At })
		if index < 0 {
			return unchanged, "", configConflict()
		}
		saved.value.Mounts = slices.Clone(saved.value.Mounts)
		saved.value.Mounts[index] = m
	}
	// Resolve export conflicts before the new destination can receive a probe.
	if err := saved.value.validate(); err != nil {
		return unchanged, "", err
	}
	message = verifiedMountMessage
	if verify != nil {
		if err := checkStorageLocations(saved, true); err != nil {
			return unchanged, "", err
		}
		message, err = verify(saved)
		if err != nil {
			return unchanged, "", err
		}
	}
	if websiteGrant != nil {
		if m.Credential == nil || m.Credential.Kind != managedCredential {
			return unchanged, "", grantUnavailable()
		}
		lease, err := acquireGrant(m.Credential.Ref, true)
		if err != nil {
			return unchanged, "", err
		}
		defer func() { err = errors.Join(err, lease.lock.Close()) }()
		if err := lease.save(ctx, websiteGrant, false); err != nil {
			if written, readErr := lease.read(); readErr == nil && bytes.Equal(written, websiteGrant) {
				err = errors.Join(err, os.Remove(lease.path))
			}
			return unchanged, "", err
		}
		defer func() {
			if receipt.Effect != core.Applied.String() {
				err = errors.Join(err, os.Remove(lease.path))
			}
		}()
	}
	return saveMount(ctx, saved, m.At, message)
}

func saveMount(ctx context.Context, saved configSnapshot, at, message string) (mountReceipt, string, error) {
	applied, err := saved.save(ctx)
	if !applied {
		return mountReceipt{at, core.Unchanged.String()}, "", err
	}
	return mountReceipt{at, core.Applied.String()}, fmt.Sprintf(message, at), err
}

func (app *App) readMountRecord(ctx context.Context, name string, prior mountRecord, base string, in io.Reader) (mountRecord, mountSource, error) {
	var m mountRecord
	data, err := readRecord(ctx, name, in)
	if err != nil {
		return m, mountSource{}, err
	}
	if err := decodeObject(data, &m); err != nil {
		return m, mountSource{}, err
	}
	if err := validateRecord(m); err != nil {
		return m, mountSource{}, err
	}
	src, err := app.prepareMountSource(m, base)
	if err != nil || m.Credential != nil || prior.Credential == nil ||
		m.Driver != prior.Driver || app.routeID(m) != app.routeID(prior) {
		return m, src, err
	}
	switch {
	case src.optionalCredential:
		// The typed decode above has already validated the record.
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(data, &fields); err == nil && fields["credential"] == nil {
			m.Credential = prior.Credential
		}
	case src.audience.origin == "":
	case src.keyFile == "":
		m.Credential = prior.Credential
	default:
		if old, err := app.prepareMountSource(prior, base); err == nil && old.audience == src.audience {
			m.Credential = prior.Credential
		}
	}
	return m, src, err
}

func readRecord(ctx context.Context, name string, in io.Reader) (_ []byte, err error) {
	var data []byte
	if name == "-" {
		if file, ok := in.(*os.File); ok {
			pipe, release, setupErr := pollablePipe(file)
			if setupErr != nil {
				return nil, setupErr
			}
			if release != nil {
				defer func() { err = errors.Join(err, release()) }()
			}
			in = pipe
		}
		if closer, ok := in.(io.Closer); ok {
			defer closeOnCancel(ctx, closer)()
		}
		data, err = io.ReadAll(in)
	} else {
		data, err = os.ReadFile(name)
	}
	return data, err
}

type setupField struct {
	label string
	value *string
}

func askFields(ask askField, fields ...setupField) error {
	for _, field := range fields {
		value, err := ask(field.label, *field.value)
		if err != nil {
			return err
		}
		*field.value = value
	}
	return nil
}

func promptInput(in io.Reader, out io.Writer) func(string, string) (string, error) {
	input := bufio.NewReader(in)
	return func(label, current string) (string, error) {
		prompt := label
		if current != "" {
			prompt += fmt.Sprintf(" [%q]", current)
		}
		if _, err := fmt.Fprint(out, prompt+": "); err != nil {
			return "", err
		}
		line, err := input.ReadString('\n')
		if err != nil && !(err == io.EOF && line != "") {
			return "", err
		}
		value := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if value == "" {
			value = current
		}
		return value, nil
	}
}

func promptReference(raw string) (*credentialRef, error) {
	kind, name, ok := strings.Cut(raw, ":")
	if !ok || name == "" {
		return nil, invalidConfig("Use a credential reference such as env:NAME, file:PATH, or profile:NAME as requested.")
	}
	if kind == "file" {
		var err error
		name, err = filepath.Abs(name)
		if err != nil {
			return nil, err
		}
	}
	return &credentialRef{Kind: kind, Ref: name}, nil
}

func askChoice(ask askField, label, current string, choices ...string) (string, error) {
	prompt := label
	for {
		answer, err := ask(prompt, current)
		if err != nil {
			return "", err
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if slices.Contains(choices, answer) {
			return answer, nil
		}
		prompt = "Invalid choice. Try again.\n" + label
	}
}

func confirm(ask askField, label string) error {
	answer, err := askChoice(ask, label+" [y/N]", "", "y", "yes", "n", "no", "")
	if err != nil {
		return err
	}
	if answer == "y" || answer == "yes" {
		return nil
	}
	return context.Canceled
}

func mountPathConflicts(mounts []mountRecord, at, replacing string) bool {
	return slices.ContainsFunc(mounts, func(other mountRecord) bool {
		return other.At != replacing && (core.Within(at, other.At) || core.Within(other.At, at))
	})
}

func (app *App) promptMount(ctx context.Context, at string, prior mountRecord, saved configSnapshot, ask askField, out io.Writer, grant **oauthGrant) (mountRecord, mountSource, error) {
	m := prior
	var choice sourceChoice
	if prior.Driver == "" {
		heading := "Add mount"
		if at != "" {
			heading = fmt.Sprintf("Add mount %q", at)
		}
		if _, err := fmt.Fprintln(out, heading+"\n"); err != nil {
			return mountRecord{}, mountSource{}, err
		}
		var err error
		choice, err = app.promptSource(ask, out)
		if err != nil {
			return mountRecord{}, mountSource{}, err
		}
		m.At, m.Driver = at, choice.id
	} else {
		choice = app.choices[m.Driver]
	}
	if prior.Driver != "" || m.At == "" {
		var err error
		m.At, err = ask("Mount path (for example /files)", m.At)
		if err != nil {
			return mountRecord{}, mountSource{}, err
		}
	}
	if err := core.ValidatePath(m.At); err != nil {
		return mountRecord{}, mountSource{}, err
	}
	if mountPathConflicts(saved.value.Mounts, m.At, prior.At) {
		return mountRecord{}, mountSource{}, configConflict()
	}
	if choice.label == "" {
		return mountRecord{}, mountSource{}, unavailableSource()
	}
	if _, err := fmt.Fprintf(out, "\nConnect to %s\n", choice.label); err != nil {
		return mountRecord{}, mountSource{}, err
	}
	var connection sourceConnection
	if prior.Driver == "" && len(choice.connections) != 0 {
		connection = choice.connections[0]
		if len(choice.connections) > 1 {
			var methods []string
			for _, item := range choice.connections {
				methods = append(methods, item.method)
				if _, err := fmt.Fprintf(out, "  %-8s %s\n", item.method, item.label); err != nil {
					return mountRecord{}, mountSource{}, err
				}
			}
			method, err := askChoice(ask, "Connection", connection.method, methods...)
			if err != nil {
				return mountRecord{}, mountSource{}, err
			}
			connection = choice.connections[slices.Index(methods, method)]
		}
		m.Connection, m.Options = connection.method, connection.defaults
	}
	r, err := app.mountRoute(m)
	if err != nil {
		return mountRecord{}, mountSource{}, err
	}
	if r.setup != nil {
		return m, mountSource{}, nil
	}
	if connection.setupHint != "" {
		if _, err := fmt.Fprintln(out, connection.setupHint); err != nil {
			return mountRecord{}, mountSource{}, err
		}
	}
	if connection.setupURL != "" {
		openCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		openErr := launchOAuthBrowser(openCtx, connection.setupURL)
		stop()
		if err := ctx.Err(); err != nil {
			return mountRecord{}, mountSource{}, err
		}
		if openErr != nil {
			if _, err := fmt.Fprintf(out, "Open this address in your browser: %s\n", connection.setupURL); err != nil {
				return mountRecord{}, mountSource{}, err
			}
		}
	}
	preparePrompt := ask
	if r.managed.available() && prior.Driver == "" && managedStoreSupported {
		method := r.managed.method
		if !r.managed.managedOnly {
			method, err = askChoice(ask, "Connection ("+method+" or reference)", method, method, "reference")
			if err != nil {
				return mountRecord{}, mountSource{}, err
			}
		}
		if method == r.managed.method {
			var scopes []string
			if r.managed.chooseScopes != nil {
				scopes, err = r.managed.chooseScopes(ask)
			} else {
				scopes, err = r.managed.requiredScopes(m.Options)
			}
			if err != nil {
				return mountRecord{}, mountSource{}, err
			}
			m.Credential, *grant, err = r.managed.selectGrant(ctx, ask, out, scopes, oauthGrant{})
			if err != nil {
				return mountRecord{}, mountSource{}, err
			}
			if r.managed.chooseRoot != nil {
				m.Options, err = r.managed.chooseRoot(ctx, m.Options, m.Credential, *grant, scopes, ask, false)
				if err != nil {
					return mountRecord{}, mountSource{}, err
				}
				preparePrompt = nil
			}
		}
	}
	if !r.interactive {
		return mountRecord{}, mountSource{}, &problem{Code: "interaction_required", Message: "This source uses a configuration record.", NextAction: "Provide a record with --from FILE or --from -."}
	}
	src, options, err := r.prepare(m.Options, filepath.Dir(saved.path), preparePrompt)
	if err != nil {
		return mountRecord{}, mountSource{}, err
	}
	if options != nil {
		m.Options = options
	}
	keepManaged := m.Credential != nil && m.Credential.Kind == managedCredential && r.managed.available()
	if src.audience.origin == "" {
		m.Credential = nil
	} else if !keepManaged {
		keep := ""
		if prior.Credential != nil {
			keep = "keep"
		}
		label := "Credential reference (env:NAME or file:PATH)"
		if src.secret != "" {
			label = src.secret + " reference (env:NAME or file:PATH)"
		}
		managedOnly := src.openManaged != nil && src.open == nil
		if managedOnly {
			label = "Existing authorization record ID"
		} else if src.optionalCredential {
			label = src.secret + " reference (env:NAME or file:PATH; - for anonymous)"
		} else if src.keyFile != "" {
			label = "Key passphrase reference (env:NAME or file:PATH; empty or - for none)"
		} else if src.mode != 0 {
			label = "SDK profile (profile:NAME)"
			if src.mode == sdkDefaultProfile {
				label = "SDK profile (profile:NAME; empty uses SDK defaults)"
			}
		}
		prompt := label
		for {
			raw, err := ask(prompt, keep)
			if err != nil {
				return mountRecord{}, mountSource{}, err
			}
			next := m.Credential
			switch {
			case raw == "keep" && keep != "":
			case (src.optionalCredential || src.keyFile != "" || src.mode == sdkDefaultProfile) && (raw == "" || raw == "-"):
				next = nil
			case managedOnly:
				next = &credentialRef{Kind: managedCredential, Ref: raw}
			default:
				next, err = promptReference(raw)
			}
			if err == nil {
				err = src.checkReference(next)
			}
			if err == nil {
				m.Credential = next
				break
			}
			prompt = publicError(err).Message + "\n" + label
		}
	}
	if _, err := fmt.Fprintf(out, "\nReview mount\n  Source  %s\n  Mount   %q\n", choice.label, m.At); err != nil {
		return mountRecord{}, mountSource{}, err
	}
	if m.Driver == "weiyun" {
		var o weiyunOptions
		if err := decodeObject(m.Options, &o); err != nil {
			return mountRecord{}, mountSource{}, err
		}
		root := o.RootPath
		if root == "" {
			root = "/"
		}
		if _, err := fmt.Fprintf(out, "  Access  Browse and download\n  Folder  %q within the Weiyun authorization\n", root); err != nil {
			return mountRecord{}, mountSource{}, err
		}
	}
	return m, src, confirm(ask, "Save mount?")
}

func (app *App) promptSource(ask askField, out io.Writer) (sourceChoice, error) {
	for {
		query, err := ask("Source name or type (? to list)", "local")
		if err != nil {
			return sourceChoice{}, err
		}
		query = strings.ToLower(strings.TrimSpace(query))
		if choice, ok := app.choices[query]; ok {
			return choice, nil
		}
		if query == "?" {
			if err := app.printSources(out, app.sourceNames("")); err != nil {
				return sourceChoice{}, err
			}
			continue
		}
		names := app.sourceNames(query)
		if len(names) == 1 {
			return app.choices[names[0]], nil
		}
		if len(names) == 0 {
			if _, err := fmt.Fprintln(out, "No matching source. Enter ? to list available sources."); err != nil {
				return sourceChoice{}, err
			}
			continue
		}
		if _, err := fmt.Fprintln(out, "Matching sources"); err != nil {
			return sourceChoice{}, err
		}
		if err := app.printSources(out, names); err != nil {
			return sourceChoice{}, err
		}
	}
}
