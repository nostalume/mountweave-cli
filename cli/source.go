package cli

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"

	core "github.com/nostalume/mountweave"
)

// Source is one build-time Driver registration. Construct it with NewSource.
type Source struct {
	id    string
	route route
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
	r := sourceRoute(label, prompt, func(options T, dir string) (mountSource, error) {
		p, err := prepare(options, dir)
		if err != nil {
			return mountSource{}, err
		}
		if p.Open == nil || p.CredentialOrigin == "" && (p.OptionalCredential || p.CredentialAccount != "") {
			return mountSource{}, invalidConfig("The source did not provide a valid constructor and credential policy.")
		}
		var audience credentialAudience
		if p.CredentialOrigin != "" {
			audience = credentialAudience{origin: id + "\x00" + p.CredentialOrigin, account: p.CredentialAccount}
		}
		return mountSource{open: p.Open, probeOnChange: true, audience: audience, optionalCredential: p.OptionalCredential, secret: "Credential"}, nil
	})
	r.interactive = prompt != nil
	return Source{id: id, route: r}, nil
}
