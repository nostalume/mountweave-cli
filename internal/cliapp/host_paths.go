package cliapp

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type localDirectory struct {
	path    string
	actual  string
	private bool
}

type LocalOptions struct {
	Root                 string `json:"root"`
	StagingDirectory     string `json:"staging_directory,omitempty"`
	MaxStagingBytes      int64  `json:"max_staging_bytes,omitempty"`
	MaxConcurrentCommits int    `json:"max_concurrent_commits,omitempty"`
}

func (opts LocalOptions) HostPaths(base string) (root, staging string) {
	return HostPath(base, opts.Root), OptionalHostPath(base, opts.StagingDirectory)
}

func OptionalHostPath(base, name string) string {
	if name == "" {
		return ""
	}
	return HostPath(base, name)
}

func CheckStorageLocations(saved ConfigSnapshot, checkPool bool) error {
	return checkHostPaths(saved, checkPool, false)
}

func CheckWebsiteStorage(saved ConfigSnapshot) error {
	return checkHostPaths(saved, false, true)
}

func checkHostPaths(saved ConfigSnapshot, checkPool, websiteSetup bool) error {
	base := filepath.Dir(saved.Path)
	var roots []configuredSource
	var private []string
	protectGrants := websiteSetup
	for _, mount := range saved.Value.Mounts {
		sources, metadata := mount.sources()
		if checkPool && metadata != "" {
			private = append(private, HostPath(base, metadata))
		}
		for source := range sources {
			m := source.record
			protectGrants = protectGrants || m.hasProtectedState()
			if m.Driver == "local" {
				roots = append(roots, source)
			}
		}
	}
	grantPath, grantErr := GrantDirectoryPath()
	checkExports := !websiteSetup && grantErr == nil && len(saved.Value.Exports) != 0
	if len(roots) != 0 && grantErr == nil && !protectGrants {
		_, err := os.Lstat(grantPath)
		protectGrants = !errors.Is(err, os.ErrNotExist)
	}
	var states []string
	if !websiteSetup {
		for _, e := range saved.Value.Exports {
			if dir := e.multipartDirectory(base); dir != "" {
				states = append(states, dir)
			}
		}
	}
	if (protectGrants || checkExports) && len(roots) != 0 {
		if grantErr != nil {
			return grantErr
		}
		grantPath, grantErr = resolveHostPath(grantPath)
		if grantErr != nil {
			return GrantUnavailable()
		}
	}
	if !protectGrants && !checkExports && len(private) == 0 && len(states) == 0 {
		return nil
	}
	var locals []localDirectory
	for _, source := range roots {
		var opts LocalOptions
		if err := DecodeObject(source.record.Options, &opts); err != nil {
			return err
		}
		root, _ := opts.HostPaths(base)
		actual := root
		if protectGrants || len(states) != 0 || !source.offline {
			resolved, err := resolveHostPath(root)
			if err != nil {
				return InvalidConfig("The Local root could not be resolved.")
			}
			actual = resolved
		}
		if grantPath != "" && hostPathsOverlap(grantPath, actual) &&
			(protectGrants || checkExports && source.member == "" && slices.ContainsFunc(saved.Value.Exports, func(e Export) bool {
				if !e.scope().containsMount(source.record.At) {
					return false
				}
				subtree := strings.TrimPrefix(strings.TrimPrefix(e.Root, source.record.At), "/")
				return hostPathsOverlap(grantPath, filepath.Join(actual, filepath.FromSlash(subtree)))
			})) {
			return InvalidConfig("A Local mount overlaps protected authorization data.")
		}
		if !source.offline {
			root = actual
		}
		locals = append(locals, localDirectory{path: root, actual: actual, private: source.member != ""})
	}
	if len(private) != 0 {
		paths := []localDirectory{{path: saved.Path}}
		for _, path := range private {
			resolved, err := resolveHostPath(path)
			if err != nil {
				return InvalidConfig("A Pool metadata path could not be resolved.")
			}
			paths = append(paths, localDirectory{path: resolved, private: true})
		}
		paths = append(paths, locals...)
		for i, a := range paths {
			for _, b := range paths[i+1:] {
				if (a.private || b.private) && hostPathsOverlap(a.path, b.path) {
					return InvalidConfig("Pool metadata and private Local roots must be separate from other mounts and configuration.")
				}
			}
		}
	}
	for i, state := range states {
		actual, err := resolveHostPath(state)
		if err != nil {
			return InvalidConfig("The S3 multipart state directory could not be resolved.")
		}
		if withinHostPath(saved.Path, actual) {
			return InvalidConfig("S3 multipart state must be separate from the configuration.")
		}
		if slices.ContainsFunc(states[:i], func(other string) bool { return hostPathsOverlap(actual, other) }) {
			return InvalidConfig("S3 multipart exports need separate state directories.")
		}
		if slices.ContainsFunc(locals, func(root localDirectory) bool { return hostPathsOverlap(actual, root.actual) }) {
			return InvalidConfig("S3 multipart state must be outside Local mount roots.")
		}
		states[i] = actual
	}
	return nil
}

func hostPathsOverlap(a, b string) bool {
	return withinHostPath(a, b) || withinHostPath(b, a)
}

func withinHostPath(name, root string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolveHostPath(name string) (string, error) {
	name, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	var missing []string
	for {
		_, err := os.Lstat(name)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(name)
			slices.Reverse(missing)
			return filepath.Join(append([]string{resolved}, missing...)...), err
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(name)
		if parent == name {
			return "", err
		}
		missing = append(missing, filepath.Base(name))
		name = parent
	}
}

func HostPath(base, name string) string {
	if filepath.IsAbs(name) {
		return filepath.Clean(name)
	}
	return filepath.Join(base, name)
}
