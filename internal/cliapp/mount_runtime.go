package cliapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"path/filepath"
	"slices"
	"strings"
	"time"

	core "github.com/nostalume/mountweave"
)

const LocalActor core.Principal = "local-cli"

type MountBindingsKey struct{}

// Bindings are populated during serial mount construction within one runtime.
func withMountBindings(ctx context.Context) context.Context {
	return context.WithValue(ctx, MountBindingsKey{}, make(map[any]any))
}

func (app *App) install(ctx context.Context, c *core.Core, saved ConfigSnapshot, m MountRecord, sources iter.Seq[MountRecord]) error {
	base := filepath.Dir(saved.Path)
	src, err := PrepareMountSource(app, m, base)
	if err != nil {
		return err
	}
	open, err := admitMount(app, m, src, saved, sources)
	if err != nil {
		return err
	}
	d, err := app.openMountDriver(ctx, src, open, saved, m, sources, false)
	if err != nil {
		return err
	}
	if _, err := c.Install(m.At, d); err != nil {
		return errors.Join(err, d.Close())
	}
	return nil
}

func OpenFor(app *App, ctx context.Context, saved ConfigSnapshot, targets ...core.Address) (*core.Core, error) {
	return app.openFor(ctx, saved, false, targets...)
}

// A directory listing above a mount only needs its name in the Core namespace.
type mountPointDriver struct{}

func (mountPointDriver) Stat(context.Context, core.NativeAddress, ...core.ReadSelection) (core.Metadata, error) {
	return core.Metadata{}, core.Fail(core.UnsupportedOperation, nil)
}

func (mountPointDriver) Enumerate(context.Context, core.NativeQuery) (core.Iterator, error) {
	return nil, core.Fail(core.UnsupportedOperation, nil)
}

func (mountPointDriver) Open(context.Context, core.NativeAddress, core.OpenOptions) (core.Opened, error) {
	return core.Opened{}, core.Fail(core.UnsupportedOperation, nil)
}

func (mountPointDriver) Close() error { return nil }

type fileRuntime struct {
	core                *core.Core
	bindings            map[any]any
	mounts              map[string]*core.Mount // nil denotes a connected mount, a handle denotes a placeholder.
	checkedPrivatePaths bool
}

func newFileRuntime() (*fileRuntime, error) {
	c, err := core.New(core.Options{Authorize: func(context.Context, core.Principal, core.Action, core.Address) error { return nil }})
	if err != nil {
		return nil, err
	}
	return &fileRuntime{core: c, bindings: make(map[any]any), mounts: make(map[string]*core.Mount)}, nil
}

func (app *App) openFor(ctx context.Context, saved ConfigSnapshot, virtual bool, targets ...core.Address) (*core.Core, error) {
	rt, err := newFileRuntime()
	if err != nil {
		return nil, err
	}
	if err := app.installFor(ctx, saved, rt, virtual, targets...); err != nil {
		return nil, errors.Join(err, CloseRuntime(rt.core))
	}
	return rt.core, nil
}

func (app *App) installFor(ctx context.Context, saved ConfigSnapshot, rt *fileRuntime, virtual bool, targets ...core.Address) error {
	for _, target := range targets {
		if err := target.Validate(); err != nil {
			return err
		}
	}
	var parent string
	if virtual && len(targets) == 1 && targets[0].Namespace() == core.TreeNamespace {
		parent = targets[0].Value()
	}
	ctx = context.WithValue(ctx, MountBindingsKey{}, rt.bindings)
	sources := CredentialRecords(saved.Value.Mounts)
	for _, m := range saved.Value.Mounts {
		if !slices.ContainsFunc(targets, func(target core.Address) bool { return mountIntersects(m.At, target) }) {
			continue
		}
		placeholder := parent != "" && parent != m.At && core.Within(m.At, parent)
		prior, installed := rt.mounts[m.At]
		if installed && (prior == nil || placeholder) {
			continue
		}
		if placeholder {
			point, err := rt.core.Install(m.At, mountPointDriver{})
			if err != nil {
				return err
			}
			rt.mounts[m.At] = point
			continue
		}
		if !rt.checkedPrivatePaths && (m.Driver == "local" || m.Driver == PoolSourceID) {
			if err := CheckStorageLocations(saved, true); err != nil {
				return err
			}
			rt.checkedPrivatePaths = true
		}
		if prior != nil {
			if err := prior.Retire(ctx); err != nil {
				return err
			}
		}
		if err := app.install(ctx, rt.core, saved, m, sources); err != nil {
			return app.mountFailure(err, saved, m)
		}
		rt.mounts[m.At] = nil
	}
	return nil
}

func mountIntersects(mount string, address core.Address) bool {
	if address.Namespace() == core.KeyNamespace {
		return address.Space() == mount
	}
	return core.Within(address.Value(), mount) || core.Within(mount, address.Value())
}

func (app *App) mountRecoveryProblem(m MountRecord, err error) *Problem {
	sources, _ := m.sources()
	var recovery *Problem
	for source := range sources {
		if source.offline {
			continue
		}
		route, _ := app.mountRoute(source.record)
		p := route.recoveryProblem(err)
		if p != nil {
			if recovery != nil && *recovery != *p {
				return nil
			}
			recovery = p
		}
	}
	return recovery
}

func (app *App) mountFailure(err error, saved ConfigSnapshot, m MountRecord) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	route, _ := app.mountRoute(m)
	p := *AppProblem(app, route.ProjectError(err))
	p.Message = fmt.Sprintf("Mount %q. %s", m.At, p.Message)
	p.addMountRecovery(saved.Path, m)
	return &p
}

func (p *Problem) addMountRecovery(configPath string, m MountRecord) {
	if p.Reconnect && m.Driver != PoolSourceID {
		p.NextAction = strings.TrimSuffix(MountEditAction(configPath, m.At)+"\n"+p.NextAction, "\n")
	}
}

func CloseRuntime(c *core.Core) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Close(ctx)
}

func (app *App) openMountDriver(ctx context.Context, src MountSource, open sourceOpener, saved ConfigSnapshot, m MountRecord, sources iter.Seq[MountRecord], initialize bool) (core.Driver, error) {
	if src.pool != nil {
		return app.openPool(ctx, src, saved, m, sources, initialize)
	}
	return openDriver(ctx, open, m.Credential, filepath.Dir(saved.Path))
}

func (src MountSource) verifyAndClose(ctx context.Context, d core.Driver, err error) error {
	if d == nil {
		return err
	}
	if err == nil && src.Verify != nil {
		err = src.Verify(ctx, d)
	}
	return errors.Join(err, d.Close())
}

func VerifyNativeRoot(root core.NativeQuery) func(context.Context, core.Driver) error {
	return func(ctx context.Context, d core.Driver) error {
		it, err := d.Enumerate(ctx, root)
		if it == nil {
			if err == nil {
				return core.Fail(core.InvalidUpstreamResponse, nil)
			}
			return err
		}
		if err == nil {
			_, err = it.Next(ctx, 1)
			if err == io.EOF {
				err = nil
			}
		}
		return errors.Join(err, it.Close())
	}
}
