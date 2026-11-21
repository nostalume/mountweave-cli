package cliapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"slices"

	core "github.com/nostalume/mountweave"
)

const PoolSourceID = "pool"

// PoolBuilder transfers already-open members only on success.
// Drivers follow opts.Members order. An offline member has a nil Driver.
type PoolBuilder func(ctx context.Context, opts PoolOptions, initialize bool, drivers []core.Driver) (core.Driver, error)

func PoolRoute(build PoolBuilder) Route {
	r := SourceRoute(nil, func(opts PoolOptions, base string) (MountSource, error) {
		opts.MetadataPath = HostPath(base, opts.MetadataPath)
		return MountSource{pool: &opts, ProbeOnChange: true}, nil
	})
	r.Interactive, r.pool = false, build
	return r
}

func (app *App) openPool(ctx context.Context, src MountSource, saved ConfigSnapshot, m, prior MountRecord, initialize bool) (_ core.Driver, err error) {
	opts := *src.pool
	base := filepath.Dir(saved.Path)
	sources := make([]MountSource, len(opts.Members))
	var references []MountRecord
	for i, member := range opts.Members {
		record := member.Record(m.At)
		s, err := PrepareMountSource(app, record, base)
		if err == nil {
			if record.Credential != nil && references == nil {
				references = slices.Collect(SourceRecords(saved.Value.Mounts...))
				references = slices.AppendSeq(references, SourceRecords(prior))
			}
			err = CheckAudience(app, record, s, saved, slices.Values(references))
		}
		if err != nil {
			return nil, app.memberProblem(member.ID, err)
		}
		sources[i] = s
	}
	drivers := make([]core.Driver, len(sources))
	defer func() {
		for _, d := range slices.Backward(drivers) {
			if d != nil {
				err = errors.Join(err, d.Close())
			}
		}
	}()
	for i, member := range opts.Members {
		if member.Offline {
			continue
		}
		s := sources[i]
		d, err := s.OpenDriver(ctx, member.Credential, base)
		if err != nil {
			return nil, app.memberProblem(member.ID, s.Route.ProjectError(err))
		}
		drivers[i] = d
		if support, ok := d.(core.NamespaceSupport); ok && !support.SupportsNamespace(core.TreeNamespace) {
			return nil, app.memberProblem(member.ID, core.Fail(core.UnsupportedOperation, nil))
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, err := src.Route.pool(ctx, opts, initialize, drivers)
	if err == nil {
		drivers = nil // The Pool owns the members after construction.
		if ctx.Err() != nil {
			return nil, errors.Join(ctx.Err(), d.Close())
		}
	}
	return d, err
}

func (app *App) memberProblem(id string, err error) error {
	p := *AppProblem(app, err)
	p.Message = fmt.Sprintf("Pool member %q. %s", id, p.Message)
	return &p
}

type PoolOptions struct {
	MetadataPath     string       `json:"metadata_path"`
	DataShards       int          `json:"data_shards"`
	ParityShards     int          `json:"parity_shards"`
	ShardBytes       int          `json:"shard_bytes"`
	MaxBufferedBytes int64        `json:"max_buffered_bytes"`
	Concurrency      int          `json:"concurrency,omitempty"`
	Members          []PoolMember `json:"members"`
}

type PoolMember struct {
	ID            string          `json:"id"`
	Identity      string          `json:"identity"`
	FailureDomain string          `json:"failure_domain"`
	MaxBytes      int64           `json:"max_bytes,omitempty"`
	Offline       bool            `json:"offline,omitempty"`
	Driver        string          `json:"driver"`
	Connection    string          `json:"connection,omitempty"`
	Options       json.RawMessage `json:"options"`
	Credential    *CredentialRef  `json:"credential,omitempty"`
}

func (m PoolMember) Record(at string) MountRecord {
	return MountRecord{At: at, Driver: m.Driver, Connection: m.Connection, Options: m.Options, Credential: m.Credential}
}

func validatePool(m MountRecord) error {
	if m.Connection != "" {
		return InvalidConfig("Pool connection methods belong to its member records.")
	}
	var opts PoolOptions
	if err := DecodeObject(m.Options, &opts); err != nil {
		return err
	}
	if opts.MetadataPath == "" {
		return InvalidConfig("Pool requires options.metadata_path.")
	}
	for _, member := range opts.Members {
		if !core.ValidName(member.ID) || member.Driver == PoolSourceID {
			return InvalidConfig("Pool needs named private members with non-Pool Drivers.")
		}
		if err := validateRecord(member.Record(m.At)); err != nil {
			return err
		}
	}
	return nil
}

func SourceRecords(mounts ...MountRecord) iter.Seq[MountRecord] {
	return func(yield func(MountRecord) bool) {
		for _, mount := range mounts {
			sources, _ := mount.sources()
			for source := range sources {
				if !yield(source.record) {
					return
				}
			}
		}
	}
}

type configuredSource struct {
	record  MountRecord
	member  string
	offline bool
}

// The containing mount has already passed config validation.
func (m MountRecord) sources() (iter.Seq[configuredSource], string) {
	if m.Driver != PoolSourceID {
		return func(yield func(configuredSource) bool) {
			yield(configuredSource{record: m})
		}, ""
	}
	var opts PoolOptions
	_ = json.Unmarshal(m.Options, &opts)
	return func(yield func(configuredSource) bool) {
		for _, member := range opts.Members {
			if !yield(configuredSource{record: member.Record(m.At), member: member.ID, offline: member.Offline}) {
				return
			}
		}
	}, opts.MetadataPath
}
