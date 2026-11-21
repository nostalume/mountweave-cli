# Contributing

Start with the [architecture](docs/architecture.md) to find the owner of a
change, then use the [specification](docs/specification.md) to identify the
behavior to preserve. Provider file behavior belongs in Mountweave. CLI
changes cover commands, saved settings, authorization and runtime assembly.

## Build from source

From the checkout, use Go 1.27 or newer.

```sh
GOWORK=off go build -mod=readonly -trimpath -o mweave .
```

For PowerShell, set the workspace switch before building.

```powershell
$env:GOWORK = 'off'
go build -mod=readonly -trimpath -o mweave.exe .
```

Run the executable by its path or add its directory to `PATH`. Add
`-tags=mweave_pool` to include the optional [storage Pool](docs/sources.md#pool-mounts).

## Develop one change

| Work | Entry point in the CLI | Checks to reuse |
| --- | --- | --- |
| Help, options or results | [`commands.go`](internal/cliapp/commands.go), [`run.go`](internal/cliapp/run.go) | `TestSelectedHost` |
| Saved mounts, credentials or recovery | [`mounts.go`](internal/cliapp/mounts.go), [`config.go`](internal/cliapp/config.go), [`mount_runtime.go`](internal/cliapp/mount_runtime.go) | `TestSelectedHost`, `TestConfigContenders`, `TestCredentialRecipientBeforeOpen` |
| Source or consent flow | [Stock catalog](internal/stock/sources.go), [managed authorization](internal/cliapp/managed_setup.go) | `TestDefaultConnections`, [authorization fixtures](internal/stock/authorization_test.go), [website fixtures](internal/stock/website_test.go) |
| Export scope, readiness or shutdown | [`export.go`](internal/cliapp/export.go), [`serve.go`](internal/cliapp/serve.go), the protocol adapter | `TestProtocolExports`, `TestExportListenerFailure`, `TestS3MultipartRestart` |
| Private Pool members | [`pool_enabled.go`](internal/stock/pool_enabled.go) | `TestPoolMountWorkflow` with `-tags=mweave_pool` |

For example, when a service already exposes WebDAV, add a preset only if
its default endpoint or setup differs. The `nutstore` preset reuses the
WebDAV route and saves the effective collection URL. Test that the new
preset produces the same record shape and runtime behavior as a handwritten
record. If the service returns incorrect range data, fix the Driver that
reads the response and retain the CLI's existing streamed output contract.

Run from the checkout with Go 1.27 or newer and `GOWORK=off` to verify the
published dependency graph. Use `export GOWORK=off` in Bash or
`$env:GOWORK = 'off'` in PowerShell. Format changed Go files with `gofmt`.

```sh
go test -mod=readonly ./...
go test -mod=readonly -tags=mweave_pool ./...
go vet -mod=readonly ./...
```

Use closed fixtures and dummy credentials. Prefer extending an existing
workflow to check the newly changed boundary. A browser test may skip if
there is no supported browser. Inspect `go test -v` when that path is in scope. No real account is needed for the contributor checks.

## Custom builds

Use `github.com/nostalume/mountweave-cli/cli` to extend the stock binary.
Pass custom sources from `cli.NewSource` to `cli.New` to add them to the
standard catalog.

For a binary with only selected sources, import
`github.com/nostalume/mountweave-cli/cli/host`. Register custom sources with
`host.NewSource` and pass the exact set to `host.New`. For example,
`host.New(local.Source(), webdav.Source(), custom)` uses the opt-in
[Local](cli/source/local) and [WebDAV](cli/source/webdav) packages and one custom
source. Both entry points support `app.Main()` and `app.Run(ctx, args, in,
out, diagnostics, terminal)`. Saved mounts for omitted sources remain visible
in `mount list` with their availability marked.

Use a struct with explicit JSON tags for public settings. `prepare(options,
configDir)` validates settings locally and returns a `cli.PreparedSource`
whose `Open` constructs a fresh native Driver. `prompt` can collect public
settings or be nil for `--from` only. Store credentials separately as existing
environment or file references. If the source uses a credential, set
`CredentialOrigin` to a stable identifier for its actual recipient and
`CredentialAccount` when the account is part of that boundary. The host
compares recipients for shared references. The native constructor controls
where it sends the secret.

## Verification and delivery

| Decision | Initial acceptance case |
| --- | --- |
| Route and address selection | Mount Local interactively and WebDAV from the same record shape used by the wizard. With an existing authorization, complete setup without copying its secret. Compare an S3 exact key through CLI parsing, Core authorization and the native fixture. `mount list` and `help` construct zero Drivers, and a single-mount operation constructs one. |
| Command navigation | Find file work from top-level help and mount maintenance from scoped help. Verify help with invalid configuration. Saved mount listing performs zero native requests. File access at a saved mount reports credential and location failures. |
| Configuration transaction | Reject ambiguous or invalid input and demonstrate that two CLI writers starting from the same snapshot yield one commit and one conflict. An interrupted save leaves a complete file. Relative saved paths have the same meaning from two working directories. A server keeps its loaded snapshot until restart. |
| Credential boundary | A static edit performs zero secret reads. A switch changes one reference and a renewal preserves the account and intended scope. Denied consent preserves saved mounts and reports any observed authorization effect. Check that a synthetic secret stays out of previews, JSON, logs and errors. |
| File effects and streaming | Cancel a named download before publication and race a competing creation. Check that an existing destination survives. A capped listing releases its cursor. A failed move reports both sides. Use a large body and a paginated listing to measure calls and memory when setting host budgets. |
| S3 object export | Use the real AWS SDK with sealed native storage to check exact keys, HTTPS publication, bucket scope and rejected requests that cause no storage I/O. Kill and restart the host with a confirmed multipart part, reject a changed mount, replay a lost Complete reply, and verify that shutdown holds the state lock until handlers exit. |
| Export isolation | Use WebDAV, JSON HTTP, SFTP and FTP clients against the same closed fixture. A read-only principal cannot mutate, both operands of a move or copy are checked, and sibling paths stay inaccessible. Stat and list follow the exact root policy when a directory disappears or is replaced. |
| Process lifetime | A failed second bind releases startup resources. A listener failure after readiness triggers group shutdown. A closed output pipe and cancellation with a silent upload producer release their owned resources. |
| Custom build and Pool | Build an independent Go module and register a typed source. Mount two instances through the wizard and a record, preserve the old settings after a rejected edit, and close the native instances after export shutdown. Verify a mixed Pool, a failed member startup, degraded reads, refused writes, and metadata ownership. |
| Automation result | A redirected invocation returns without a prompt. Check JSON shapes and exit codes for success, a limited listing, partial failure and output loss after a mutation. |

The S3 multipart check starts and restarts a separate host process using the
test executable and a closed storage fixture.

```sh
go test -mod=readonly -count=1 -v -run '^TestS3MultipartRestart$' ./internal/cliapp
```

## Prepare a release

CI runs on Linux and Windows against both the default and Pool builds.
Confirm CI passed for the intended commit and the repository has an approved
LICENSE. The release workflow builds and tests the default binary from a
clean tag.
Download its versioned archives, both installers and `SHA256SUMS` from the
successful run. Check that `mweave version` reports the tag and `go version -m`
reports the tagged revision and `vcs.modified=false`. Test the archive outside
the checkout using the [README workflow](README.md#mount-a-folder).

Create a draft in the verified repository with `gh release create <tag>
--verify-tag --draft`. Upload the reviewed assets to the draft and check the
complete set before publishing. After publishing, download the assets from
the public release, compare their checksums with the reviewed files, then
update the root Nix and Scoop metadata to use those same versioned URLs and
digests. Verify the package channels after the metadata is published. Keep
the README and installers in the source tag so they can be read at that
version.
