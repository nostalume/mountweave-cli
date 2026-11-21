package cliapp

import (
	"context"
	"errors"
	"fmt"
	"io"
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

func (app *App) install(ctx context.Context, c *core.Core, saved ConfigSnapshot, m MountRecord) error {
	base := filepath.Dir(saved.Path)
	src, err := PrepareMountSource(app, m, base)
	if err != nil {
		return err
	}
	if err := CheckAudience(app, m, src, saved, SourceRecords(saved.Value.Mounts...)); err != nil {
		return err
	}
	d, err := app.openMountDriver(ctx, src, saved, m, MountRecord{}, false)
	if err != nil {
		return err
	}
	if _, err := c.Install(m.At, d); err != nil {
		return errors.Join(err, d.Close())
	}
	return nil
}

func OpenFor(app *App, ctx context.Context, saved ConfigSnapshot, targets ...core.Address) (*core.Core, error) {
	for _, target := range targets {
		if err := target.Validate(); err != nil {
			return nil, err
		}
	}
	c, err := core.New(core.Options{Authorize: func(context.Context, core.Principal, core.Action, core.Address) error { return nil }})
	if err != nil {
		return nil, err
	}
	ctx = withMountBindings(ctx)
	checkedPrivatePaths := false
	for _, m := range saved.Value.Mounts {
		if !slices.ContainsFunc(targets, func(target core.Address) bool { return mountIntersects(m.At, target) }) {
			continue
		}
		if !checkedPrivatePaths && (m.Driver == "local" || m.Driver == PoolSourceID) {
			if err := CheckStorageLocations(saved, true); err != nil {
				return nil, errors.Join(err, CloseRuntime(c))
			}
			checkedPrivatePaths = true
		}
		if err := app.install(ctx, c, saved, m); err != nil {
			return nil, errors.Join(app.mountFailure(err, saved, m), CloseRuntime(c))
		}
	}
	return c, nil
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

func (app *App) openMountDriver(ctx context.Context, src MountSource, saved ConfigSnapshot, m, prior MountRecord, initialize bool) (core.Driver, error) {
	if src.pool != nil {
		return app.openPool(ctx, src, saved, m, prior, initialize)
	}
	return src.OpenDriver(ctx, m.Credential, filepath.Dir(saved.Path))
}

func (app *App) probeMount(ctx context.Context, src MountSource, saved ConfigSnapshot, m, prior MountRecord) error {
	d, err := app.openMountDriver(ctx, src, saved, m, prior, prior.At == "")
	return src.Route.ProjectError(src.verifyAndClose(ctx, d, err))
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
		if err != nil {
			return err
		}
		_, err = it.Next(ctx, 1)
		if err == io.EOF {
			err = nil
		}
		return errors.Join(err, it.Close())
	}
}
