# CLI architecture

mweave is a host for [Mountweave](https://github.com/nostalume/mountweave).
It connects a user's storage once, then offers the same mount to file
commands and protocol clients. The [CLI specification](specification.md)
defines behavior. This page identifies the owner of each design decision.

```text
command or wizard ──► saved config ──► typed source + credential reference
                                              │
                                      native Driver instance
                                              │
                                    Core mount and authority
                                      │                  │
                                  file command       protocol adapter
```

## Architecture decisions

| Boundary | Owner and invariant |
| --- | --- |
| Command and configuration | The CLI parses arguments and persists public intent. One complete config snapshot is saved at a time. A running `serve` keeps its startup snapshot. |
| Source and authorization | A visible service plus optional connection selects typed settings and a Driver. A grant is a separate protected record; permission to use it follows the provider account, scope and recipient. |
| Files | Core owns the file address, mount selection, authorization and operation effects. The Driver owns native requests, root checks, paging, ranges and receipts. |
| Protocol clients | The host owns listener configuration and client identity. The adapter owns wire framing, protocol sessions and request resources. Core enforces the configured root and access for each operation. |
| Results | The same outcome drives human-readable and JSON replies. Preserve partial effects and uncertain outcomes. The CLI owns local publication for named downloads and keeps diagnostics off a byte stream. |

## Command boundaries

One command definition owns help, operands and accepted options
([`commands.go`](../internal/cliapp/commands.go)). `mount` and `export` manage saved
settings. File commands construct only the mounts they need; `serve`
constructs and shares the mounts used by its listeners. `mount list` and
help do not connect to providers. A new file operation should reuse the
existing Core address and effect model.
[Command input](../internal/cliapp/run.go) provides prompts and reads `--from`
records for both mount and export setup.

Configuration publication compares the edited snapshot under the writer
lock before replacing the file. Saved local paths resolve from the actual
configuration directory; command-line local operands resolve from the
invocation directory. See the [configuration contract](configuration.md#configuration-and-secrets)
for strict input and recovery rules.

## Adding services

The [source catalog](../internal/cliapp/source.go) maps a saved service/connection to
typed options and one native constructor. The same route serves interactive
setup, explicit records and Pool members. Presets expand into concrete
settings before saving, so future preset changes do not retarget existing
mounts. A source can use the wizard or accept records only. Both paths use the
same prepared options and credential rules.

| Change | Start here |
| --- | --- |
| New settings for an existing protocol | Add a preset when the service only varies in native options, endpoint or sign-in instructions. S3 presets share the same Driver. |
| New authorization or connection method | Reuse the setup and protected credential store. Record the actual recipient and verify scope before the first probe. |
| New remote file semantics | Implement them in the [Mountweave Driver](https://github.com/nostalume/mountweave/blob/v0.3.0/docs/architecture.md); register its native constructor and typed settings in the CLI. |
| Custom program using mweave | Register a [typed source](../CONTRIBUTING.md#custom-builds). Let Core see the native Driver's optional capabilities directly. |

115 Open and 123 Open share an account-folder input but have separate
constructors and credential recipients. [Website setup](../internal/stock/website_setup.go)
owns profile creation, confirmation, and publication. Driver adapters verify
the account and folder. LanZou saves a new credential reference on reconnect.
Aliyun can reuse one authorization across several roots, and verifies every
bound root before replacing the shared authorization.

## Source code ownership

`cli.New` assembles the [stock sources](../internal/stock/sources.go) and any supplied
extensions. `host.New` assembles the supplied sources. Both return the same
App, with orchestration in `internal/cliapp`. [Local](../cli/source/local/local.go)
and [WebDAV](../cli/source/webdav/webdav.go) can be selected in an explicit
build. The stock WebDAV entry and the 123, Nutstore and FEB presets use the
same WebDAV constructor. The catalog registers mount choices. Stock keeps public
settings together by shared resource shape, with separate files for consent,
credential rotation, and website wiring.

| Code | Responsibility |
| --- | --- |
| Source catalog and setup | Select a service and connection, collect public settings, and open the native Driver. The prepared mount supplies any review details to the wizard. Directory inputs live in [account folder settings](../internal/stock/folders.go). |
| Authorization | Keep provider consent and connection setup together. [Credential HTTP transport](../internal/cliapp/credential_http.go) owns the reusable recipient checks and request headers. |
| Website mounts | [Stock setup](../internal/stock/website_setup.go) connects the native verifier to publication, cleanup, and grant leases. [Browser lifecycle](../internal/website/lifecycle.go) is shared with the browser-check tool. |
| Native storage | Drivers own account and root proofs, object IDs, listing and body receipts. |
| Shared host resources | [File locks](../internal/filelock/) own acquisition and cancellation. Config and authorization callers assign their own conflict and recovery behavior. |

## Running commands and protocol endpoints

In [`mount_runtime.go`](../internal/cliapp/mount_runtime.go), successful `Core.Install`
transfers Driver ownership. The caller closes a rejected instance. Pool
follows the same pattern. The [host](../internal/cliapp/pool_config.go) admits
members before opening any, then passes their Drivers to the selected Pool
constructor. Pool takes ownership on success. Core then owns the Pool.
Metadata, placement and degraded file access belong to Pool.

[Export setup](../internal/cliapp/export.go) defines, validates, and saves the
same record for the wizard and `--from`. [Serving](../internal/cliapp/serve.go) assigns
each listener its own Core principal and binds all listeners before reporting
readiness. Startup failure rolls back acquired resources. Shutdown stops
acceptance, drains requests within a shared budget and waits for retirement.

- WebDAV, JSON HTTP, SFTP and FTP map directory operations into the same
  Core authorization. The [TCP export](../internal/cliapp/serve_tcp.go) shares
  connection tracking and shutdown for SFTP and FTP. SFTP owns bound handles.
  FTP owns passive data transfers, with a peer check at the listener.
- [HTTP handlers](../internal/cliapp/serve_http.go) bind JSON HTTP, WebDAV and
  S3 to Core. S3 preserves the original request target for signing, routing
  and native execution. The host owns private multipart state and its lock.
  The adapter owns upload sessions, and Core owns publication. Release the
  lock after handlers and maintenance finish.

## Authorization ownership

Provider consent and file permission are distinct. A credential reference
identifies a provider grant, an external profile or a host secret. A file
command can use it or fail with a next action; it does not start interactive
authorization. Compare recipient and scope before sending a credential to a
source, including during a mount probe. Export credentials authenticate
protocol clients and stay separate from source credentials.

Reauthorization of a shared grant preserves the account and scope.
Switching accounts changes one mount. Restoring a missing grant preserves
the saved folder and asks for confirmation when native verification cannot
prove the old binding. See the [credential contract](configuration.md#editing-credentials).

The host owns protected storage, leases, and publication. Reusable refresh
and app credentials share the access-token cache. On reauthorization,
reusable grants take the writer lease after consent and recheck the current
record. App credentials take it before the exchange. Static approvals stay
in the protected store and can be replaced by switching the mount.

Reauthorizing a saved rotating grant takes an exclusive lease before
consent. A live rotating source holds that lease through cleanup. The
native rotation state machine durably records pending exchanges. The AliPan Driver owns its credential source. The host closes
the credential source borrowed by 115 Open. Uncertain exchanges require
recovery before reuse.
