//go:build mweave_pool

package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/pool"
	bolt "go.etcd.io/bbolt/errors"
)

func poolRoute() route {
	r := sourceRoute("Private storage pool", nil, preparePool)
	r.interactive = false
	return r
}

func memberProblem(id string, err error) error {
	p := *publicError(err)
	p.Message = fmt.Sprintf("Pool member %q. %s", id, p.Message)
	return &p
}

func preparePool(opts poolOptions, base string) (mountSource, error) {
	return mountSource{probeOnChange: true, build: func(ctx context.Context, app *App, saved configSnapshot, m, prior mountRecord) (core.Driver, error) {
		return app.openPool(ctx, saved, m, prior, opts, base)
	}}, nil
}

func (app *App) openPool(ctx context.Context, saved configSnapshot, m, prior mountRecord, opts poolOptions, base string) (_ core.Driver, err error) {
	native := pool.Options{
		MetadataPath: hostPath(base, opts.MetadataPath), DataShards: opts.DataShards, ParityShards: opts.ParityShards,
		ShardBytes: opts.ShardBytes, MaxBufferedBytes: opts.MaxBufferedBytes, Concurrency: opts.Concurrency,
		Members: make([]pool.Member, len(opts.Members)),
	}
	sources := make([]mountSource, len(opts.Members))
	previous := sourceRecords(prior)
	for i, member := range opts.Members {
		record := member.record(m.At)
		src, err := app.prepareMountSource(record, base)
		if err == nil {
			err = app.checkAudience(record, src, saved, true, previous...)
		}
		if err != nil {
			return nil, memberProblem(member.ID, err)
		}
		sources[i] = src
		native.Members[i] = pool.Member{ID: member.ID, Identity: member.Identity, FailureDomain: member.FailureDomain, MaxBytes: member.MaxBytes}
	}
	defer func() {
		if err != nil {
			for i := len(native.Members) - 1; i >= 0; i-- {
				if d := native.Members[i].Driver; d != nil {
					err = errors.Join(err, d.Close())
				}
			}
		}
	}()
	for i, member := range opts.Members {
		if !member.Offline {
			d, err := sources[i].openDriver(ctx, member.Credential, base)
			if err != nil {
				return nil, memberProblem(member.ID, err)
			}
			native.Members[i].Driver = d
			if support, ok := d.(core.NamespaceSupport); ok && !support.SupportsNamespace(core.TreeNamespace) {
				return nil, memberProblem(member.ID, core.Fail(core.UnsupportedOperation, nil))
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	native.LockTimeout = time.Second
	if deadline, ok := ctx.Deadline(); ok {
		native.LockTimeout = max(time.Nanosecond, min(native.LockTimeout, time.Until(deadline)))
	}
	d, err := pool.New(native)
	if errors.Is(err, bolt.ErrTimeout) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &problem{Code: "pool_in_use", Message: "The Pool metadata database is in use.", NextAction: "Use the running export or stop its process before opening this Pool locally."}
	}
	return nativeDriver(d, err)
}
