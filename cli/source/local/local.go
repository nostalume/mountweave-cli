// Package local supplies the Local directory source for explicit CLI builds.
package local

import (
	"cmp"
	"context"
	"path/filepath"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/cli/host"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	localdriver "github.com/nostalume/mountweave/drivers/local"
)

var source = cliapp.SingleSource("local", "Local directory", cliapp.SourceRoute(guide, prepare))

// Source returns the Local directory connection used by the standard CLI.
func Source() host.Source { return source }

func guide(opts *cliapp.LocalOptions, ask cliapp.Prompt) error {
	value, err := ask("Local directory", cmp.Or(opts.Root, "."))
	if err != nil || opts.Root != "" && value == opts.Root {
		return err
	}
	opts.Root, err = filepath.Abs(value)
	return err
}

func prepare(opts cliapp.LocalOptions, base string) (cliapp.MountSource, error) {
	if opts.Root == "" {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Local requires options.root.")
	}
	if opts.MaxConcurrentCommits < 0 || opts.MaxStagingBytes < 0 ||
		(opts.StagingDirectory == "") != (opts.MaxStagingBytes == 0) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Local staging requires a directory and a positive byte budget.")
	}
	root, staging := opts.HostPaths(base)
	native := localdriver.Options{StagingDirectory: staging, MaxStagingBytes: opts.MaxStagingBytes, MaxConcurrentCommits: opts.MaxConcurrentCommits}
	return cliapp.MountSource{Open: func(context.Context, string) (core.Driver, error) {
		return cliapp.NativeDriver(localdriver.New(root, native))
	}}, nil
}
