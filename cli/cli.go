// Package cli runs mweave with the built-in source catalog.
package cli

import (
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave-cli/internal/stock"
)

type (
	App            = cliapp.App
	Source         = cliapp.Source
	Prompt         = cliapp.Prompt
	PreparedSource = cliapp.PreparedSource
)

// New constructs a CLI with the built-in sources and the supplied extensions.
func New(sources ...Source) (*App, error) { return stock.New(sources...) }

// NewSource registers a custom source for the CLI.
func NewSource[T any](id, label string, prompt func(*T, Prompt) error, prepare func(T, string) (PreparedSource, error)) (Source, error) {
	return cliapp.NewSource(id, label, prompt, prepare)
}
