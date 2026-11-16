package cli

import (
	"encoding/json"

	core "github.com/nostalume/mountweave"
)

const poolSourceID = "pool"

type poolOptions struct {
	MetadataPath     string       `json:"metadata_path"`
	DataShards       int          `json:"data_shards"`
	ParityShards     int          `json:"parity_shards"`
	ShardBytes       int          `json:"shard_bytes"`
	MaxBufferedBytes int64        `json:"max_buffered_bytes"`
	Concurrency      int          `json:"concurrency,omitempty"`
	Members          []poolMember `json:"members"`
}

type poolMember struct {
	ID            string          `json:"id"`
	Identity      string          `json:"identity"`
	FailureDomain string          `json:"failure_domain"`
	MaxBytes      int64           `json:"max_bytes,omitempty"`
	Offline       bool            `json:"offline,omitempty"`
	Driver        string          `json:"driver"`
	Connection    string          `json:"connection,omitempty"`
	Options       json.RawMessage `json:"options"`
	Credential    *credentialRef  `json:"credential,omitempty"`
}

func (m poolMember) record(at string) mountRecord {
	return mountRecord{At: at, Driver: m.Driver, Connection: m.Connection, Options: m.Options, Credential: m.Credential}
}

func decodePool(m mountRecord) (opts poolOptions, err error) {
	if err := decodeObject(m.Options, &opts); err != nil {
		return opts, err
	}
	return opts, opts.validate(m.At)
}

func (opts poolOptions) validate(at string) error {
	if opts.MetadataPath == "" {
		return invalidConfig("Pool requires options.metadata_path.")
	}
	for _, member := range opts.Members {
		if !core.ValidName(member.ID) || member.Driver == poolSourceID {
			return invalidConfig("Pool needs named private members with non-Pool Drivers.")
		}
		if err := validateRecord(member.record(at)); err != nil {
			return err
		}
	}
	return nil
}

func sourceRecords(m mountRecord) []mountRecord {
	if m.Driver != poolSourceID {
		return []mountRecord{m}
	}
	var opts poolOptions
	_ = json.Unmarshal(m.Options, &opts)
	records := make([]mountRecord, 0, len(opts.Members))
	for _, member := range opts.Members {
		records = append(records, member.record(m.At))
	}
	return records
}
