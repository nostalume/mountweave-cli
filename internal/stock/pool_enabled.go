//go:build mweave_pool

package stock

import (
	"context"
	"errors"
	"os"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave/drivers/pool"
	bolt "go.etcd.io/bbolt/errors"
)

func poolRoute() cliapp.Route { return cliapp.PoolRoute(openPool) }

func openPool(ctx context.Context, opts cliapp.PoolOptions, initialize bool, drivers []core.Driver) (core.Driver, error) {
	native := pool.Options{
		MetadataPath: opts.MetadataPath, DataShards: opts.DataShards, ParityShards: opts.ParityShards,
		ShardBytes: opts.ShardBytes, MaxBufferedBytes: opts.MaxBufferedBytes, Concurrency: opts.Concurrency,
		Members: make([]pool.Member, len(opts.Members)), LockTimeout: time.Second,
	}
	for i, member := range opts.Members {
		native.Members[i] = pool.Member{ID: member.ID, Identity: member.Identity, FailureDomain: member.FailureDomain, MaxBytes: member.MaxBytes, Driver: drivers[i]}
	}
	if deadline, ok := ctx.Deadline(); ok {
		native.LockTimeout = max(time.Nanosecond, min(native.LockTimeout, time.Until(deadline)))
	}
	construct := pool.New
	if !initialize {
		construct = pool.OpenExisting
	}
	d, err := construct(native)
	if !initialize && errors.Is(err, os.ErrNotExist) {
		return nil, &cliapp.Problem{Code: "pool_metadata_missing", Message: "The saved Pool metadata is missing.", NextAction: "Restore the Pool metadata database and its matching member data."}
	}
	if errors.Is(err, bolt.ErrTimeout) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &cliapp.Problem{Code: "pool_in_use", Message: "The Pool metadata database is in use.", NextAction: "Use the running export or stop its process before opening this Pool locally."}
	}
	return cliapp.NativeDriver(d, err)
}
