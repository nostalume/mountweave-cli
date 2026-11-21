// Package host runs mweave with the sources chosen by the caller.
package host

import "github.com/nostalume/mountweave-cli/internal/cliapp"

type (
	App            = cliapp.App
	Source         = cliapp.Source
	Prompt         = cliapp.Prompt
	PreparedSource = cliapp.PreparedSource
)

// New constructs an app with exactly the supplied sources.
func New(sources ...Source) (*App, error) { return cliapp.NewApp(nil, sources...) }

// NewSource registers a typed source with optional guided setup.
func NewSource[T any](id, label string, prompt func(*T, Prompt) error, prepare func(T, string) (PreparedSource, error)) (Source, error) {
	return cliapp.NewSource(id, label, prompt, prepare)
}
