# CLI specification

This reference defines the CLI's observable contract. See the
[README](../README.md) for installation and first use, [configuration](configuration.md)
for saved records, and [storage sources](sources.md) for provider options.

## Connect a mount

See [setup values](sources.md#find-setup-values) for where to obtain each input.

1. Run `mweave mount add` and enter a service name or access type. Enter `?`
   to list sources. Choose by number from the displayed list or enter a name.
   Connection and other setup menus accept their displayed numbers too.
   For unattended setup, supply the source and connection in the record.
2. Reuse an authorized profile when it covers the intended access. Otherwise
   request the narrowest supported grant through the provider's consent flow
   or enter a service-issued credential through protected input. Account
   registration, application approval and consent remain on the provider side.
   Show an official link and the missing action when blocked.
3. Offer a storage location the authorization covers, or show the permitted
   default. Ask for a native identifier when the provider cannot offer a
   directory choice. Confirm the mount path and verify access to the chosen
   location with a read operation. Report what the setup verified.
4. Save the mount after the check succeeds. If the credential was issued during
   setup, make its record durable before the mount refers to it. If a later step
   fails, report whether a reusable authorization was saved. An existing
   mount path is a conflict. Use `mount edit` to update it.

`mount list` shows saved mounts and local credential problems without connecting. Use `stat /nas` to see
the entry at that path or `ls /nas` to browse its contents. File operations
report access failures from the selected mount. Credential changes follow
the [editing contract](configuration.md#editing-credentials).

## Commands and arguments

Use `mweave [--config FILE] COMMAND [OPTIONS] OPERANDS`. The config flag
selects a file. Otherwise read `config.json` in the
[default application directory](configuration.md#configuration-and-secrets).
Command options can appear before or after operands. `--` ends option
parsing. Unknown options and duplicate scalar values are usage errors. Local
file operands use the host's path rules. Mounted addresses are relative to the
mounted root when the leading slash is omitted.

### Command navigation

`mweave help` and `mweave --help` show the available commands. Show file
operations first. Show `serve` under protocol access when the build includes
it. Under settings, list `mount` and `export` for their respective settings.

`mweave mount --help` and `mweave export --help` show the commands for
each group. `mweave mount add --help` shows the supported source types,
their option names and how to use `--from`. A leaf command's `--help`
shows its usage and options. Every help scope works without loading
configuration, constructing a Driver or opening a listener. `version`
reports the build under the same conditions.

### File commands

| Command | Behavior |
| --- | --- |
| `ls [ADDRESS]` | List one directory, defaulting to the mounted root `/`. Text shows one name per line with `/` after directory names. `--limit N` caps the number of entries. |
| `stat ADDRESS` | Show information about one entry. |
| `get ADDRESS FILE\|-` | Download to a new local file or stream bytes to stdout with `-`. The named form publishes the completed file and refuses to replace an existing destination. |
| `put FILE\|- ADDRESS` | Upload a local file or stream from stdin. The opened file supplies its length when known. For stdin, `--size BYTES` supplies an explicit length when the selected Driver requires it. |
| `cp SOURCE TARGET`, `mv SOURCE TARGET` | Use a complete target path or end the target with `/` to place the source in an existing directory. `cp -r` copies a tree. |
| `mkdir PATH` | Create a directory. |
| `trash ADDRESS` | Move one file to the source's recycle bin. |
| `remove ADDRESS`, `rmdir PATH` | Permanently delete one file or an empty directory, respectively. |

Listing above a saved mount reads the mount layout without opening the source.

For `mv`, `cp` and local-file `put`, a target ending in `/` is an existing
mounted directory. For example, `mweave mv /drive/report.pdf /drive/archive/`
prefers `report.pdf` and may place it under an unused name. The command
reports the actual destination in text and JSON. Directory placement
preserves occupants. A driver may allocate the name during its native move;
otherwise Core selects a candidate and uses an exclusive operation. A
concurrent creation can occupy that candidate before the operation commits.
Standard input and `--replace` take complete target paths. An explicit
`key:` address always names the exact key, including its trailing slash.

Ordinary writes and copies create a new destination. A file `put --replace`
or `cp --replace` selects Core complete publication and allows replacement
or creation. A conflicting directory or an unavailable native guarantee
returns the Core error. Recursive copy takes tree paths and is incompatible
with `--replace`. Directory plans use serial execution and host defaults of
10,000 entries and 8 MiB of relative-path bytes.

Deletion asks Core for the selected effect. The source must guarantee that
effect for the selected item or return `unsupported_operation` before
changing it. `rmdir` also requires the directory to be empty at deletion time.
The former `rm` command now reports the replacement commands before loading
settings. Protocol exports keep their own deletion contracts.

Local complete publication uses `staging_directory` and `max_staging_bytes`.
Choose a private staging directory outside the mounted directory on the
same filesystem. On Linux, Local writes that require exact name lookup can
return `unsupported_operation` on unverified filesystems such as WSL Windows
shares (9p). S3 complete publication uses `part_size` and
`max_buffered_bytes`. The selected Driver and its service determine which
mutation guarantees are available.

### Interactive shell

`mweave shell [PATH]` opens a terminal session at the specified mounted
directory or at `/`. Enter file commands with their usual options. `cd PATH`
changes the mounted directory, `pwd` prints it, and `exit` or end of input
closes the session. Bare `ls` lists the current directory. Mounted paths
starting with a separator are absolute. Relative mounted paths can use
`.` and `..`. Local paths resolve from the process working directory and
`key:` addresses keep their exact meaning.

Quote arguments with spaces using single or double quotes. Backslashes remain
literal so Windows paths can be entered as written. File transfers in the
shell use named local files. Use one-shot commands with `-` for pipelines.

The shell reads settings once and opens sources on demand. A failed command
returns to the prompt. Ctrl+C cancels the current operation. On exit, the
session closes the sources it opened.

### Settings and protocol access

| Command | Behavior |
| --- | --- |
| `mount list` | Show saved mounts, Driver availability and local credential problems. |
| `mount add [PATH]`, `mount edit [PATH]`, `mount remove PATH` | Change saved mount records. Interactive edit can select a mount by number. Edit with `--from` or `--json` requires a path. Add and edit accept `--from FILE` or `--from -`. |
| `export add` | Save an export through guided setup or with `--from FILE` or `--from -`. |
| `serve` | Start listeners for the saved exports. See [Protocol setup](#protocol-setup). |

`mount add --from FILE` reads a complete mount record, including `at`. For
`mount edit PATH --from FILE`, `PATH` identifies the saved mount and the
record describes its replacement. These commands use the same validation and
protected references as the main configuration.

### Addressing

`nas/report.pdf` and `/nas/report.pdf` address the same mounted path. The CLI
accepts both slash styles in mounted paths. Directory commands such as `ls`
and `mkdir`, mount paths and export roots also accept a final separator.
Path segments retain their case and spaces. Empty segments and `.` or `..` are
invalid. Saved configuration and records passed with `--from` use absolute
mounted paths.

Use a single quoted argument such as `'key:/objects:a//b'` for a native key.
The colon after the mount separates it from the key. Encode a literal colon
or percent in the mount name as `%3A` or `%25`. Decode the mount once and
preserve the entire key suffix, including slashes, colons and percent escapes.
In `ls`, that suffix is a literal key prefix. `'key:/objects:'` lists the
whole mounted key space.

```sh
mweave ls /nas --limit 20 --json
mweave get /nas/report.pdf ./report.pdf
mweave put --size 4096 - /nas/report.bin
mweave cp 'key:/objects:a//b' /nas/object.bin
```

### Results and exit status

File and configuration commands with `--json` emit one object with
`result` and `error`. The error is null on success. On failure it contains a
stable `code`, a sanitized `message` and an optional `next_action`. Use
Core's file error codes when available. Host errors distinguish usage
(`usage`), an invalid config (`invalid_config`), concurrent edits
(`config_conflict`), a busy credential (`credential_in_use`), an unavailable
credential (`credential_unavailable`), an incompatible credential destination (`credential_scope_conflict`) and setup that needs user input
(`interaction_required`). A timeout and cancellation report `timeout` and
`cancelled` respectively.

Preserve the result when it contains known effects or partial output. Use a
null result when there is no receipt. Text output shows names as written and
escapes control characters. `stat` shows the address, type and available metadata.
`mount list` shows each path followed by its source and connection. A local credential problem includes its cause and next action. An interactive `mount edit` without a path puts mounts with local problems first.

| Operation | JSON result |
| --- | --- |
| `stat` | Core entry with its structured address. |
| `ls` | `items` of the same entry type and a `state` of `complete`, `limited` or `failed`. |
| `get ADDRESS FILE`, `put`, file `cp`, `mkdir`, `trash`, `remove`, `rmdir` | `effect`. |
| Recursive `cp` | `effect` and per-item `items`. |
| `mv` | `source_effect` and `target_effect`, with per-item `items` for a tree execution and `target_removed_effect` if Core reports removal of the prior target. |
| `mount list` | `items` containing each saved `at`, `driver` and the build-time boolean `driver_available`, plus `connection` when saved and `problem` when a local credential problem is detected. Per-mount problems do not change the listing exit status. |
| `mount add`, `mount edit`, `mount remove`, `export add` | The saved `at` or `listen` and the configuration `effect`. |

Interactive reauthorization reports the authorization effect and the affected
mounts in the selected configuration. When an unattended command needs a new
grant, its error directs the caller to the interactive edit.

Effects use Core's `unchanged`, `applied`, `incomplete` and `unknown`
vocabulary. Tree items use `relative_path`, `side`, `phase` and `effect` in
Core receipt order. A failed item uses its stable code as `error` and includes
`error_side` when Core identifies it. An unknown effect requires
reconciliation before retry.

JSON mode is noninteractive. An invocation with redirected input also avoids
prompts. Setup requiring interactive input returns a safe next action. Exit
status is 0 for success, 2 for usage errors and 1 for execution failures.

`get ADDRESS -` reserves stdout for bytes and uses stderr for diagnostics.
Combining it with `--json` is a usage error. A stream may end after delivering
partial bytes. A named download streams to a private temporary file in the
destination directory, closes it and publishes it with a native no-replace
operation. A competing creation is a conflict. If the destination filesystem
cannot support this commit, report the failure and clean up the temporary file.
A caller can explicitly choose stdout to manage its own destination. Once
committed, preserve the applied effect even if cancellation or cleanup fails.

### Process lifecycle

A one-shot invocation that uses configuration reads one snapshot and
constructs each required mount once. Listing a virtual ancestor can involve
descendant mounts. Saved mount listing, help and version construct zero
Drivers.
File commands submit the selected Core operation. File bodies stream
directly, including stdin of unknown length where supported. A Driver may
use native staging under an explicit write policy.

`ls` emits pages incrementally in text and JSON. A positive `--limit` stops
the listing successfully and releases any unconsumed cursor. Report
`complete` on known exhaustion and `limited` when the limit stops further
discovery. Cursors stay in the process. Use Core's default page size unless
the remaining limit is smaller. The host supplies positive cursor lifetimes
and finite request and tree-plan budgets. Ordinary commands share those defaults. Measure Driver construction,
provider calls and streaming memory when setting numeric defaults.

An optional `--timeout` requests cancellation of a one-shot operation. A
closed output pipe cancels a download or releases a listing cursor. For
`put -`, cancellation interrupts the host's pending stdin read. The host
owns its read end and waits for the Driver's result and cleanup. Native
cleanup can run beyond the operation deadline.

An undelivered result is an output failure. Report known effects on stderr
when writable and exit 1 after cleanup. If the caller cannot see the effect
of a mutation, it must reconcile the target before retrying.
[Go SIGPIPE behavior](https://pkg.go.dev/os/signal#hdr-SIGPIPE)

## Protocol setup

`mweave export add` guides an operator through choosing a protocol, a root
in the configured mount namespace and client authentication. The wizard
defaults to `127.0.0.1:8787` with read-only access. For scripts,
`mweave export add --from FILE` or `--from -` reads one export record.
Both paths save the same `exports` records as direct configuration.
Saving checks the settings without starting a listener or resolving the
client credential. Use `mweave serve` to start the saved exports in the
foreground. Changes take effect on restart.

Each record has `protocol`, `listen`, `root` and `auth`. It can also set
`access` and protocol-specific fields. Supported protocols are `webdav`,
`http`, `sftp`, `ftp` and `s3`.
`listen` is a host and port. `root` selects a directory in an active mount
or the exact object mount path for S3.
WebDAV `auth` has `username` and a password `credential` reference. JSON HTTP
`auth` has a bearer token `credential` reference. WebDAV presents the
directory at its URL root. JSON HTTP clients use its absolute Core address.
SFTP and FTP present the selected directory as `/`.
The authenticated principal belongs to one export. Core checks every address
against the selected directory or object key space. Directory copy and
move check both endpoints.

The exact export root admits stat and list only. Body access and mutation
apply to descendants according to the export's access policy. The default
`access` is `read`. Explicit `read_write` admits supported mutations
below the root. All directory exports use this rule.

A directory export selects a path relative to one active mount binding.
At startup the path must resolve to a directory. If an external change removes it,
new requests surface the corresponding Core lookup error. If a file
replaces it, metadata may report that kind, list fails with the type
mismatch, and the root body stays inaccessible. A new directory at the
same path can serve fresh requests under the same binding and access
policy. Existing streams and cursors retain Core's lifecycle. The Driver
owns its native root identity. Replacing a filesystem path outside the active
binding does not transfer that binding to a new native root. Changing the
mount binding requires applying a new configuration.

Client credentials use `env` or `file` references separate from outgoing
storage credentials. A relative credential file path in a saved record
resolves against the configuration directory. For the HTTP profiles,
`tls` names a `cert_file` and `key_file` relative to that same directory
or by absolute path. TLS requires version 1.2 or newer. Cleartext listeners
must resolve to a loopback address before binding. A TLS front end can use
a loopback backend if access to it is restricted to trusted local callers.
[RFC 7617](https://www.rfc-editor.org/rfc/rfc7617.html#section-4) · [RFC 6750](https://www.rfc-editor.org/rfc/rfc6750.html#section-5)

| Protocol | Settings and behavior owned by its adapter |
| --- | --- |
| WebDAV | Username and password reference. Map the WebDAV root to the selected tree path. PUT requires Core complete publication. A writable Local mount needs the Driver's explicit staging directory and byte budget for this operation. |
| JSON HTTP | Bearer token reference. Keep the published file routes and absolute tree addresses, with Core restricting access to the selected subtree. |
| SFTP | SSH host private key and client public key. Reads need a bound handle. Uploads need native live editing. Both opens refuse links. |
| FTP | Username and password reference, passive ports and optional explicit TLS. Ordinary STOR uses Core complete publication. |
| S3 | Bucket name, signing region and signing identity mapped to a Core key space. Preserve the original request target and the literal key. |

The exports reuse Mountweave's file adapters. JSON HTTP exposes the
existing synchronous file operations.

For a Rustic repository on an Aliyun Drive website mount, bind the mount to
the repository folder and set `options.write` with its own `staging_root_id`,
`part_size`, and `max_buffered_bytes`. Use a private staging folder on the
same resource drive outside the mounted tree. Set the WebDAV export to
`"access":"read_write","create_only":true`. An unconditional PUT can create a
file or confirm a byte-equivalent retry. Changed content at an existing path
returns 409. `If-None-Match: *` still returns 412 when the target exists.
This Driver supports file deletion and directory creation for the repository.

Set Rustic's repository to `opendal:webdav`, supply the loopback endpoint
and WebDAV username, and provide the password via `OPENDAL_PASSWORD`.
Use `connections = "1"` for Aliyun Drive. The Rustic repository encryption
password is configured independently.


At startup validate the export records and resolve client credentials.
Install each required mount once, check each export directory or object key
binding, and bind
all listeners before reporting readiness or serving requests. A failed
startup closes the listeners and runtime resources acquired by that
attempt. An empty export list is a configuration error. Running exports
keep the startup config snapshot.

With `serve --json`, stdout receives one startup result. On success,
`result.status` is `ready` and `result.exports` lists the protocol,
bound address, root, access policy and whether TLS is active for each
listener. SFTP uses SSH and reports `tls: false`. On failure, the startup
result contains the error.

An interrupt or unexpected listener exit stops acceptance on every listener.
Give active HTTP requests and SSH sessions up to four seconds to finish.
Then cancel remaining work, close connections, and wait for adapters and
Core within a shared five-second shutdown budget. After readiness, listener and cleanup failures go to stderr
and exit with status 1. A successful intentional stop exits 0. Request-level
errors leave the other requests and listeners active.

### Example WebDAV configuration

This example serves `data/` read-only on loopback.
Save this configuration as `share.json` next to `data/`.

```json
{
  "version": 1,
  "mounts": [
    {"at": "/files", "driver": "local", "options": {"root": "data"}}
  ],
  "exports": [
    {
      "protocol": "webdav",
      "listen": "127.0.0.1:8787",
      "root": "/files",
      "auth": {
        "username": "reader",
        "credential": {"kind": "file", "ref": "webdav-password.txt"}
      }
    }
  ]
}
```

Create `webdav-password.txt` alongside `share.json` with a single password of
your choice. Restrict the file to your account and keep it outside `data/`.
Run `mweave --config share.json serve`. After the server reports readiness,
open `http://127.0.0.1:8787/sample.txt` with a WebDAV client or run the following
in another terminal (`curl.exe` on Windows), entering the password when asked:

```sh
curl --fail --user reader http://127.0.0.1:8787/sample.txt
```

Successful authentication returns the contents of `sample.txt`. Stop the
server with Ctrl+C. See the [README](../README.md#share-the-mount) for guided setup.

### Set up an SFTP export

Use a dedicated unencrypted host private key and the client's public key.
The wizard defaults to `127.0.0.1:2222` and read-only access. A record for
`mweave export add --from FILE` looks like this:

~~~json
{
  "protocol": "sftp",
  "listen": "127.0.0.1:2222",
  "root": "/files/share",
  "access": "read_write",
  "host_key_file": "ssh/host_ed25519",
  "auth": {
    "username": "reader",
    "public_key_file": "ssh/reader.pub"
  }
}
~~~

Keep both key files outside exported roots. Use filesystem permissions to
protect the host private key and control who can change the trusted public key. The record's key paths are absolute or relative to
the configuration file. The wizard resolves typed paths from the working
directory. `serve` reads the keys at startup. The client should trust the
host's public key and connect with the private key matching `reader.pub`,
for example `sftp -P 2222 -i ~/.ssh/id_ed25519 reader@127.0.0.1`.
Each connection accepts one SFTP session for the configured username and
public key. Shells, remote commands and forwarding are refused. The host
admits up to 32 SSH connections per listener and the adapter permits up to
16 open file or directory handles per session.

Read-only and read-write exports advertise the corresponding logical modes.
Storage permissions and Driver capabilities may narrow the operations available.
A source can list entries while lacking the native bound file access needed
for download or live editing. An SFTP upload changes the bound file as bytes
arrive. A disconnect can leave partial data. `put -f` requests native sync
when the Driver supports it.

### Set up an FTP export

Choose `ftp` in `export add`. The default listener is `127.0.0.1:2121`
with read-only access. Enter a username and an `env` or `file` password
reference. Cleartext FTP is limited to loopback. A passive transfer uses
an available port on the same interface as its control connection. Set
`passive_ports` to an inclusive range, for example
`{"first":41000,"last":41031}`, when the firewall needs fixed ports.
The wizard accepts `41000-41031`. Use passive mode with EPSV or IPv4 PASV.

The `tls` fields select explicit FTPS (`AUTH TLS`). A client must use
`PROT P` and resume the control connection's TLS session on each data
connection. For example, curl uses `--ssl-reqd` with an `ftp://` URL.
Both FTP profiles check that the data peer has the control peer's IP.
FTPS additionally proves that it holds that session's TLS ticket.

Ordinary `STOR` publishes after the full input succeeds. A writable
Local mount needs an explicit staging directory and byte budget.
`ABOR` cancels the transfer and releases its data resources. Protected
uploads also require the TLS end-of-stream alert before publication.
Cleartext clients signal completion with TCP EOF. `APPE` and nonzero
`REST` writes require native editing and can leave partial data.

### Set up an S3 export

Choose `s3` in `export add` or pass an export record with `--from`:

~~~json
{
  "protocol": "s3",
  "listen": "127.0.0.1:9000",
  "root": "/objects",
  "auth": {
    "access_key_id": "mweave-client",
    "credential": {"kind": "env", "ref": "MWEAVE_S3_SECRET"}
  },
  "s3": {"bucket": "files", "region": "us-east-1"}
}
~~~

Configure the client with the listener URL, the bucket and region, the
access key ID and secret, and path-style addressing. The default region
is `us-east-1` and the default access is read-only. Use `tls` for HTTPS.
The client signs each request with SigV4. The mounted storage keeps
its own credentials.

The bucket exposes the exact key space of `root`. Set a narrower native
prefix on the source mount when needed. Keys such as `a` and `a/b` can
coexist. Repeated slashes and literal percent escapes retain their meaning.
Forward the original request target unchanged if using a proxy.

The profile supports ListObjectsV2, HEAD, GET and range reads. Set
`access` to `read_write` for PutObject on a Driver that supports complete
publication. `s3.max_put_bytes` limits a single PUT and defaults to 5 GiB.
An S3 source mount requires `part_size` and `max_buffered_bytes` for
complete publication. Source limits also apply. Enable inbound multipart with `s3.multipart`. Object DELETE and presigned
URLs remain disabled.

### Resume S3 multipart uploads

Add `multipart` inside the export's `s3` settings and set `access` to
`read_write`. The wizard asks for the same directory and staging budget.

~~~json
"s3": {
  "bucket": "files",
  "multipart": {
    "state_directory": "private/s3-files",
    "max_staging_bytes": 10737418240
  }
}
~~~

Use one private directory per export on a local filesystem. Relative paths
in a record resolve against the configuration file. Keep the directory
outside mounted and exported storage. Protect it with Windows filesystem
permissions or owner-only Unix permissions. The CLI holds a database lock
while using the directory. Back up or restore the manifest and inputs
together with the service stopped.

The storage budget counts logical journal bytes, including metadata and
temporary overlap during part replacement. It excludes database overhead
and filesystem allocation. Optional `max_object_bytes` defaults to the
staging budget. Optional `retention_seconds` defaults to 86,400. Each
export allows 1,024 upload records, 32 pending parts and 4,096 retained
inputs. The object limit and `max_put_bytes` also apply when admitting
a single part.

The client can list confirmed parts and resume with the same upload ID
after restart. Keep the same bucket, access key ID and source mount
definition. You can move the listener or rotate its signing secret.
Use a new directory for a different target or signing identity. If an
external SDK profile changes accounts, use a new directory even when
its name stays the same.

Complete streams the selected parts into one Core publication. An
equivalent retry of a confirmed Complete returns its saved receipt.
If the process stopped during publication and the outcome is unknown,
the service retains the parts and reports the upload during startup.
Check the target before retrying or starting a replacement upload.
Abort cancels in-flight part intake and releases its private state after
cleanup. Expired uploads and failed cleanup are revisited on startup
and during quiet periods. Use a client that supports explicit multipart
upload operations. Storage-level upload continuation is separate from
resuming confirmed incoming parts.


## Verification and delivery

The [contributor checks](../CONTRIBUTING.md#verification-and-delivery) specify the acceptance cases and where to run them.
