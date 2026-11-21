# Configuration

## Configuration and secrets

CLI edits write the same strict JSON file that unattended callers may supply
directly. Reject unknown fields, duplicate
object keys and unsupported versions. Expand service presets into concrete
settings and use the selected Driver's typed options.

Check the file structure, mount addresses, reference shapes and export scopes
before loading a runtime. Decode and validate native options for the mounts
selected by a command or needed by the exports. Saved mount listing can show
a Driver unavailable in the current build without opening it. CLI edits
validate the changed records and the shared structure. A saved definition
can remain valid while a referenced credential or service is temporarily
unavailable.

Resolve saved host filesystem paths against the directory of the actual
configuration file, after following a config symlink. Use that same file
for read and write. For a new file, use its resolved parent directory.
Examples include a Local root, a certificate path and a file-backed secret
reference. Values inside a mount record supplied via `--from` use the
destination configuration directory, including when the record comes from
stdin. Local command-line file operands and interactive path selection use
the invocation's working directory. Resolve an interactive selection before
saving it, and preview the effective path. Provider paths, URLs and literal
keys keep their native interpretation.

The file contains `version`, `mounts` and optional `exports`. An omitted
export list is empty. A mount record has `at`, `driver`, `options` and an
optional `credential`. The `driver` field names the source. Use `connection`
when that source offers multiple methods: `website` or `open` for `alipan`,
and `webdav` or `open` for `pan123`. Existing records for these two sources
that omit the method use Open. Earlier `alipan-web` records still load as
website connections. Local omits the credential and takes `options.root`.
For example, `{"at":"/files","driver":"local","options":{"root":"./data"}}`
mounts a directory relative to the configuration file. A reference has `kind`
and `ref`. The kinds identify an environment variable (`env`), a protected
file (`file`), an external provider or SDK profile (`profile`), or a record
in the host's protected store (`managed`). A route advertises which kinds it
can resolve.

The config holds references and public settings. Resolve secrets at
construction and pass them to native options in memory. A URL containing
authorization material uses a protected reference where the Driver supports it.

The default file is `%LOCALAPPDATA%\mweave\config.json` on Windows and
`${XDG_CONFIG_HOME:-$HOME/.config}/mweave/config.json` on Linux. Other systems
use `mweave/config.json` under the system's per-user configuration directory.
Use `--config FILE` to select a different file.

Managed credentials use `credentials` under the default application directory.
Windows protects managed records with the current user's DPAPI key. Linux
protects these records with file ownership and permissions and stores them
without encryption. Managed credentials and saved website sessions require
Windows or Linux. `--config` only changes which JSON file is read.

Write JSON as UTF-8 without a byte-order mark.

At a reference prompt, use `env:DAV_APP_PASSWORD` for an environment variable
you set, or `file:PATH` for a private file containing the credential. You
choose the environment variable name. The service issues its credential.
Keep credential files outside mounted or exported directories.

## Changing configuration

Use `mweave mount edit /nas` to change a saved mount and `mount remove`
to remove it. Removing a mount leaves shared authorization available.
Changes must leave export references valid. Ordinary edits keep the reference
and can be saved without reading it. In an edit record, omitting the credential
keeps the prior reference when the same Driver still accepts it. For a source
with optional credentials, set `"credential": null` in an edit record or
enter `-` in the wizard to switch to anonymous access. The CLI verifies
the selected location before saving this switch. A WebDAV
reference is associated with the configured origin and username. Changing that
destination requires a different reference. An explicit credential replacement
is checked before the edit is saved. After an ordinary edit, use
`stat /nas` with the saved mount path to access the configured location.

A CLI writer reads a snapshot, prepares and validates the change, then
acquires the writer lock for the resolved configuration path. Under that
lock it compares the current file with the snapshot and publishes the
complete replacement only if they match. Competing changes report a
conflict. Publication must leave readers with a complete old or new file.
A filesystem that cannot provide the required replacement fails the save
before exposing a partial file. Manual writers must coordinate with CLI
writers to share this guarantee. The schema version identifies the format.
The snapshot identity is used for conflict detection.

Saving reports persisted intent. The next one-shot invocation reads the
saved file. Restart `serve` to load changes to mounts, roots, client
authentication or listeners. Startup reports the loaded snapshot and any
activation failure.

## Editing credentials

| Intent | Interaction and effect |
| --- | --- |
| Edit a directory, name or other ordinary setting | Keep the credential reference by default. Preview the public changes and the account label or reference status. Static edit, preview and save use the reference alone. An explicit remote check may resolve it. |
| Reauthorize the current account | Choose `reauthorize` in `mount edit PATH` when offered. The editor shows the mounts sharing the authorization and verifies the account and scope before replacing the protected record. |
| Switch one mount to a different account | Select an existing compatible reference or set up a new one, then change that mount's reference. Verify the selected destination and access before saving the switch. Other mounts retain their own references. |
| Restore a missing managed authorization | Open `mount edit PATH` and reconnect the saved mount. Choose an existing compatible authorization when offered. Confirm the verified account and folder if the original binding cannot be proved. |

Preview and output show safe labels and status. Secret inputs start empty with
an explicit keep or replace choice. The editor never receives the old secret
or treats masked text as an input value. Use provider consent or a no-echo
terminal input for people. Automation uses a prepared SDK profile or an
explicit secret reference, such as one supplied by the deployment environment.
Report inaccessible stores and references. Persistent refresh requires protected
storage that the active owner can update durably.

Pass secrets only to the destination authorized for that reference. On an
endpoint or account change, verify compatibility before sending credentials
for a probe. Present structured and sanitized errors in text, JSON and logs.
Command arguments, saved config, previews and errors exclude raw secrets,
authorization headers and signed URL queries.

Confirm a new managed credential record before publishing a reference to it.
Resolve external references at use time. Report saved configuration,
authorization outcome and runtime activation separately. Browser consent is
an explicit interactive action. File commands and unattended edits report
`interaction_required` with the next action when a new grant is needed.

Dropbox and Google Drive use reusable refresh credentials. Active mounts can share the record.
Only renewal and replacement take the record's writer lock. Reopen the
mount to pick up an updated grant immediately, or let an active mount use
its cached access token until renewal is due.

A CLI-managed rotating grant has one writer by record identity. Acquire
ownership before loading its checkpoint and release it after Driver cleanup.
A busy record returns `credential_in_use` with a safe next action. The
active owner can continue to serve authorized requests. Persist the pending
checkpoint before rotation. A failed or uncertain rotation follows the
provider's reconciliation contract. A known new token that could not be
saved may retry persistence under the same owner. It must never trigger a
second rotation of the old token.

## Use a prepared record

Scripted setup can pass one CLI-shaped mount or export record from a file.
For example, save the single object below as `webdav-mount.json`:

```json
{
  "at": "/remote",
  "driver": "webdav",
  "options": {"root_url": "https://storage.example/dav/", "username": "alice"},
  "credential": {"kind": "env", "ref": "DAV_APP_PASSWORD"}
}
```

Replace the URL and username, and supply `DAV_APP_PASSWORD` in the invoking
environment. Run `mweave --config demo.json mount add --from webdav-mount.json`
and then `mweave --config demo.json ls /remote --json`. Add `--from -` in place
of the filename to read a record from stdin. Read the [results contract](specification.md#results-and-exit-status)
before retrying a failed mutation: a failure can have a partial effect.

## Restore settings on another machine

Copy the JSON config and keep its relative directory layout or update its
paths. Recreate the referenced keys, credential files and environment
variables securely. Then use the mount or start `serve` as usual. If an
error identifies a missing sign-in, follow its `mount edit` command. For
an expired grant, follow any instruction to choose `reauthorize`. Access
is verified before a replacement authorization is saved.

Use `mount list` to find a saved mount name. To check one restored folder,
run `mweave --config FILE ls /cloud --limit 1`, replacing `/cloud` with
the mount name. For an object store, use `ls key:/cloud: --limit 1` with
the same config. To restore an export, start `serve` and connect with
its client.

Local staging and S3 multipart state have separate lifetimes; do not copy
their contents as ordinary configuration. Restore Pool metadata with its
matching member data. See the [Pool settings](sources.md#pool-mounts) and
[multipart recovery](specification.md#resume-s3-multipart-uploads) before
moving either state store.
