package cliapp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"

	core "github.com/nostalume/mountweave"
)

// Source registers one named source. Use NewSource to register a custom source.
type Source struct {
	id, label   string
	connections []SourceConnection
}

// Prompt asks for a public setting and displays its current value.
type Prompt func(label, current string) (string, error)

// PreparedSource binds validated public options to a native constructor.
// CredentialOrigin is a stable identifier for the native credential recipient.
// Include the protocol and endpoint, or the fixed provider identity. Leave it
// empty for a source that needs no credential.
type PreparedSource struct {
	CredentialOrigin   string
	CredentialAccount  string
	OptionalCredential bool
	Open               func(context.Context, string) (core.Driver, error)
}

var sourceID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// NewSource registers typed JSON options. Prepare validates public settings
// without contacting the service or reading credentials. Its directory argument
// is the configuration file's directory. Prompt may be nil for --from setup.
// Public settings must exclude secrets. Open constructs a fresh native Driver
// per call and may return a non-nil Driver with an error for the host to close.
func NewSource[T any](id, label string, prompt func(*T, Prompt) error, prepare func(T, string) (PreparedSource, error)) (Source, error) {
	if !sourceID.MatchString(id) || strings.TrimSpace(label) == "" || prepare == nil || reflect.TypeFor[T]().Kind() != reflect.Struct {
		return Source{}, errors.New("a source needs a lowercase ID, label, options struct and prepare function")
	}
	record := MountRecord{Driver: id}
	if _, method := savedSource(record); method != "" || record.hasProtectedState() || id == "local" || id == PoolSourceID {
		return Source{}, errors.New("source ID is reserved by saved configuration")
	}
	r := SourceRoute(prompt, func(options T, dir string) (MountSource, error) {
		p, err := prepare(options, dir)
		if err != nil {
			return MountSource{}, err
		}
		if p.Open == nil || p.CredentialOrigin == "" && (p.OptionalCredential || p.CredentialAccount != "") {
			return MountSource{}, InvalidConfig("The source did not provide a valid constructor and credential policy.")
		}
		var audience CredentialAudience
		if p.CredentialOrigin != "" {
			audience = CredentialAudience{Origin: id + "\x00" + p.CredentialOrigin, Account: p.CredentialAccount}
		}
		return MountSource{Open: p.Open, ProbeOnChange: true, Audience: audience, OptionalCredential: p.OptionalCredential, Secret: "Credential"}, nil
	})
	r.Interactive = prompt != nil
	return SingleSource(id, label, r), nil
}

func SingleSource(id, label string, r Route) Source {
	return ServiceSource(id, label, SourceConnection{Route: &r})
}

func ServiceSource(id, label string, connections ...SourceConnection) Source {
	return Source{id: id, label: label, connections: connections}
}

type Route struct {
	SignInProblem   *Problem
	SignIn          error
	BindingMismatch error
	prepare         func(json.RawMessage, string, Prompt) (MountSource, json.RawMessage, error)
	Interactive     bool
	managed         ManagedAuthorization
	review          string
	pool            PoolBuilder
	Setup           func(context.Context, ConfigSnapshot, MountRecord, string, Prompt, io.Writer) (MountReceipt, string, error)
}

func (r *Route) recoveryProblem(err error) *Problem {
	switch {
	case r == nil:
		return nil
	case r.BindingMismatch != nil && errors.Is(err, r.BindingMismatch):
		return WebsiteMountScopeConflict()
	case r.SignIn != nil && errors.Is(err, r.SignIn):
		return r.SignInProblem
	default:
		return nil
	}
}

func (r *Route) ProjectError(err error) error {
	var p *Problem
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.As(err, &p) {
		if p := r.recoveryProblem(err); p != nil {
			return errors.Join(p, err)
		}
	}
	return err
}

type CredentialAudience struct {
	Origin, Account string
}

type profileMode uint8

const (
	SDKDefaultProfile profileMode = iota + 1
	SDKNamedProfile
)

type MountSource struct {
	preparedOptions    json.RawMessage // Set only when prepared from a record, without prompting.
	Route              *Route
	ProbeOnChange      bool
	pool               *PoolOptions
	Audience           CredentialAudience
	Mode               profileMode
	OptionalCredential bool
	KeyFile            string
	Secret             string // Wizard label for a provider-specific credential.
	Preview            string
	Location           MountLocation
	Open               func(context.Context, string) (core.Driver, error)
	OpenManaged        func(context.Context, string) (core.Driver, error)
	Verify             func(context.Context, core.Driver) error
}

func SourceRoute[T any](prompt func(*T, Prompt) error, prepare func(T, string) (MountSource, error)) Route {
	return Route{
		Interactive: true,
		prepare: func(raw json.RawMessage, base string, ask Prompt) (MountSource, json.RawMessage, error) {
			var value T
			if len(raw) != 0 {
				if err := DecodeObject(raw, &value); err != nil {
					return MountSource{}, nil, err
				}
			}
			if ask != nil && prompt != nil {
				if err := prompt(&value, ask); err != nil {
					return MountSource{}, nil, err
				}
			}
			src, err := prepare(value, base)
			if err != nil || ask == nil {
				return src, nil, err
			}
			data, err := json.Marshal(value)
			return src, data, err
		},
	}
}

func (r Route) WithManaged(p ManagedAuthorization) Route {
	r.managed = p
	return r
}

func (r Route) WithReview(connection string) Route {
	r.review = connection
	return r
}

type SourceConnection struct {
	Method, Label, SetupURL, SetupHint string
	Defaults                           json.RawMessage
	ConfirmSetup                       bool
	Route                              *Route
}

func (c SourceConnection) available() bool {
	return c.Route != nil && c.Route.prepare != nil
}

func (c SourceConnection) guided() bool {
	return c.available() && (c.Route.Interactive || c.Route.Setup != nil)
}

func (s Source) guided() bool {
	return slices.ContainsFunc(s.connections, SourceConnection.guided)
}

func ConnectionFor(source Source, method string) (SourceConnection, bool) {
	for _, connection := range source.connections {
		if connection.Method == method {
			return connection, true
		}
	}
	return SourceConnection{}, false
}

type MountLocation struct {
	Location, Access string
	Quote            bool
}

// App owns the source catalog shared by the setup wizard and runtime.
type App struct {
	sources        map[string]Source
	projectProblem func(error) *Problem
}

func NewApp(project func(error) *Problem, sources ...Source) (*App, error) {
	app := &App{sources: make(map[string]Source, len(sources)), projectProblem: project}
	for _, source := range sources {
		if source.id == "" || len(source.connections) == 0 {
			return nil, errors.New("invalid source definition")
		}
		if _, exists := app.sources[source.id]; exists || source.id == AliPanWebsiteSource {
			return nil, fmt.Errorf("duplicate source ID %q", source.id)
		}
		app.sources[source.id] = source
	}
	return app, nil
}

func (app *App) mountRoute(m MountRecord) (*Route, error) {
	id, method := savedSource(m)
	source, known := app.sources[id]
	if connection, found := ConnectionFor(source, method); found {
		if connection.available() {
			return connection.Route, nil
		}
		return nil, unavailableSource()
	}
	if m.Connection != "" && (known || m.Driver == AliPanWebsiteSource && app.sources["alipan"].id != "") {
		return nil, InvalidConfig("This source does not offer the specified connection method.")
	}
	return nil, unavailableSource()
}

func PrepareMountSource(app *App, m MountRecord, base string) (MountSource, error) {
	r, err := app.mountRoute(m)
	if err != nil {
		return MountSource{}, err
	}
	return r.prepareMount(&m.Options, base, nil)
}

func (r *Route) prepareMount(options *json.RawMessage, base string, ask Prompt) (MountSource, error) {
	src, updated, err := r.prepare(*options, base, ask)
	src.Route = r
	if ask == nil {
		src.preparedOptions = *options
	} else if updated != nil {
		*options = updated
	}
	return src, err
}

func unavailableSource() error {
	return &Problem{Code: string(core.MountUnavailable), Message: "The selected Driver is unavailable in this build.", NextAction: "See mweave mount add --help for the available sources."}
}

type referencePolicy struct {
	kind, prompt      string
	optional, managed bool
}

func (s MountSource) references() referencePolicy {
	managed := s.OpenManaged != nil
	switch {
	case managed && s.Open == nil:
		return referencePolicy{ManagedCredential, "Existing authorization record ID", false, true}
	case s.Mode == SDKDefaultProfile:
		return referencePolicy{"profile", "SDK profile (profile:NAME; empty uses SDK defaults)", true, managed}
	case s.Mode != 0:
		return referencePolicy{"profile", "SDK profile (profile:NAME)", false, managed}
	case s.Audience.Origin == "":
		return referencePolicy{optional: true, managed: managed}
	case s.OptionalCredential:
		return referencePolicy{"env", s.Secret + " reference (env:NAME or file:PATH; - for anonymous)", true, managed}
	case s.KeyFile != "":
		return referencePolicy{"env", "Key passphrase reference (env:NAME or file:PATH; empty or - for none)", true, managed}
	default:
		return referencePolicy{"env", cmp.Or(s.Secret, "Credential") + " reference (env:NAME or file:PATH)", false, managed}
	}
}

func (p referencePolicy) check(ref *CredentialRef) error {
	switch {
	case ref == nil && p.optional:
		return nil
	case p.kind == ManagedCredential || ref != nil && ref.Kind == ManagedCredential:
		if p.managed && ref != nil && ref.Kind == ManagedCredential && ValidGrantID(ref.Ref) {
			return nil
		}
		return InvalidConfig("This source requires an existing managed authorization reference.")
	case p.kind == "profile":
		if ref != nil && ref.Kind == "profile" {
			return nil
		}
		return InvalidConfig("Select an SDK profile for this S3 mount.")
	case p.kind == "":
		return InvalidConfig("This source does not accept a credential for the selected settings.")
	case ref == nil || ref.Kind != "env" && ref.Kind != "file":
		return InvalidConfig("This source requires an env or file credential reference.")
	}
	return nil
}

// OpenDriver opens the admitted credential reference.
func (s MountSource) OpenDriver(ctx context.Context, ref *CredentialRef, base string) (core.Driver, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	open := s.Open
	var input string
	if ref != nil && ref.Kind == ManagedCredential {
		open, input = s.OpenManaged, ref.Ref
	} else if ref != nil && ref.Kind == "profile" {
		input = ref.Ref
	} else if ref != nil {
		var err error
		input, err = LoadCredential(ref, base)
		if err != nil {
			return nil, err
		}
	}
	d, err := open(ctx, input)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil && d != nil {
		return nil, errors.Join(err, d.Close())
	}
	if d == nil && err == nil {
		return nil, InvalidConfig("The source constructor did not return a Driver.")
	}
	return d, err
}

// Keep a failed, non-nil native Driver available for openDriver to close.
func NativeDriver[T any, D interface {
	*T
	core.Driver
}](d D, err error) (core.Driver, error) {
	if d == nil {
		return nil, err
	}
	return d, err
}

func ValidResponseBudget(n int64) bool {
	return n >= 0 && n < math.MaxInt64
}

func (app *App) sourceNames(query string, include func(SourceConnection) bool) []string {
	var names []string
	for name, source := range app.sources {
		if slices.ContainsFunc(source.connections, include) &&
			(strings.Contains(name, query) || strings.Contains(strings.ToLower(source.label), query)) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func (app *App) printSources(w io.Writer, names []string) error {
	for _, name := range names {
		source := app.sources[name]
		label := source.label
		if !source.guided() {
			label += " (use --from)"
		}
		if _, err := fmt.Fprintf(w, "  %-16s %s\n", name, label); err != nil {
			return err
		}
	}
	return nil
}

func WebsiteMountScopeConflict() *Problem {
	return &Problem{Code: "credential_scope_conflict", Message: "The website account or folder differs from the saved mount.",
		NextAction: "Use the original account and folder, or add a new mount.", Reconnect: true}
}
