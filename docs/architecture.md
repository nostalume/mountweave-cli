# Architecture

The initial CLI design uses Mountweave v0.2.0. The
[specification](specification.md) defines observable behavior and the
[verification matrix](specification.md#verification-and-delivery) identifies
what delivery must demonstrate.

A person connects an account or storage location once, then works with files
through the same visible mount path. An automated caller uses that mount or
supplies a previously prepared credential reference. The provider establishes
the account and grants access. The CLI selects the available connection method,
collects the remaining inputs and builds a Mountweave Driver.

| Owner | Responsibility |
| --- | --- |
| CLI host | Command and configuration input, credential resolution and storage, caller identity, process and listener lifetime, and local download publication |
| Driver integration | Typed native settings, supported authorization methods, root discovery where available, native client and Driver construction |
| Mountweave | File addresses, authorization checks, file operations, native resources and protocol adapters |

A saved mount contains a visible path, its source name, the connection method
when the service offers a choice, native location settings and an optional
credential reference. The credential has its own lifetime. It can
authorize more than one mount, and reauthorizing it must preserve the intended
provider account. The saved mount stays separate from any live Driver instance.

## Architecture decisions

| Decision | Reason and contract |
| --- | --- |
| Choose the service and save its resolved connection | The catalog lists each service once. The selected connection fixes its typed options, credential audience and native constructor. Prompts, help and configuration use the same catalog. [Onboarding](specification.md#connect-a-mount) |
| Separate saved configuration from active bindings | A file edit and a running mount have different effects. A complete, conflict-checked save makes future intent durable. One-shot commands read it on invocation and exports use it on restart. [Configuration](specification.md#changing-configuration) |
| Own credentials by authorization record | A mount can change names and several mounts can refer to one grant. Scope checks and refresh ownership follow the grant and its intended recipient. [Credentials](specification.md#editing-credentials) |
| Use Core addresses and file operations | Humans, automation and adapters act on the same mounted files. Literal object keys survive parsing, and existing Core effects describe what changed. The CLI owns publication to a local download destination. [Commands](specification.md#commands-and-arguments) |
| Apply one Core authorization policy per export | The protocol supplies client identity and wire semantics. The host binds the identity to the configured root and access policy, and Core checks each endpoint. [Protocol setup](specification.md#protocol-setup) |
| Let the invoking process own its resources | A file command constructs the mounts it uses. A foreground server owns its listeners, active mounts and shutdown. Core and Drivers retain ownership of their operation resources. [Process lifecycle](specification.md#process-lifecycle) |
| Project one operation result for people and scripts | Human and JSON output preserve the same effects, including partial changes and uncertain outcomes. Raw streams use stdout for bytes. [Results and exit status](specification.md#results-and-exit-status) |

These decisions cover the path from a saved mount or export through an active
Core binding to an operation result. The initial verification covers Local
and WebDAV sources, an S3 exact-key fixture, and WebDAV, JSON HTTP, SFTP and FTP exports.
Additional routes use the same construction and activation path with evidence
for their native requirements.

## Command boundaries

Mount and export commands manage saved settings. File commands use mounted
addresses, and `serve` uses the selected export settings to start listeners.
The same visible mount path connects setup to file access. Saved definitions
describe intent. The file operation reports the access observed during its
invocation.

Keep frequent file operations and `serve` at the top level. Use scoped help
for mount and export maintenance and for Driver options. One command
definition provides usage, operand and option validation, and help. Help
uses build-time information. File and server commands resolve the settings
they need when invoked.
[Command navigation](specification.md#command-navigation)

## Adding services

Users choose a named storage service or an access type such as WebDAV.
A service can offer several connection methods, each with its own access and
credential requirements. The catalog chooses the Driver from the saved service
and connection. It does the same for interactive mounts, explicit records and
Pool members.

The CLI owns that choice, its configuration and any browser session. Source
modules own provider requests, native identity, paging and streaming.
A browser used only for consent can close after the grant is issued. A source
that needs the session for file requests borrows it from the host. Core sees
the resulting Driver in either case.
Use the same typed input definitions for prompts, help and unattended
configuration. The Driver still validates native settings and effects.
Routes with the same inputs and semantics share the typed setup. For example,
115 Open and 123 Open use the same account-folder options and prompts. Each
still selects its own native constructor and credential recipient. A drive
selector can reuse the folder prompt and the bearer client independently.
Use a Driver's native options directly when they declare the public fields
and exclude runtime resources from JSON. This keeps option validation with
the constructor. Pool and similar compositions describe their additional
prerequisites through the same entry point.

| What differs | Where to add it |
| --- | --- |
| URL, region, native options or provider setup instructions | A small preset or a documented example for an existing Driver. S3-compatible services share the same Driver with explicit endpoint, signing and listing settings. |
| Authorization flow or credential audience | A provider integration that uses the existing setup sequence and credential references. |
| File API or native behavior | The appropriate Mountweave Driver, with a thin CLI integration for typed options, credentials and construction. |

Keep presets bundled with the application. The wizard resolves a preset to
concrete connection settings before saving. Upgrading a preset affects new
setup and explicit edits. Existing endpoints, roots and credential references
come from the saved configuration. Explicit connection choices determine where
credentials are sent. File commands operate on a mount through Core.
[Mountweave architecture](https://github.com/nostalume/mountweave/blob/v0.2.0/docs/architecture.md) · [S3-compatible settings](https://github.com/nostalume/mountweave/blob/v0.2.0/drivers/s3/README.md#compatible-s3-services)

Reuse the SDK and native constructor for client setup and root validation.
A successful `Core.Install` transfers Driver ownership. The host closes a
rejected Driver, including any non-nil Driver returned with an error, and owns
borrowed clients. A custom binary imports `cli` and assembles its sources once.
The same catalog drives help, setup and runtime. Source constructors return
the native Driver so Core can use its optional capabilities directly.

A Pool is one visible mount with its own metadata and private members. The
host constructs each member through the same source catalog. Pool takes
ownership after it opens successfully. Core then owns the Pool through
its normal mount lifecycle. Placement, reconstruction and write admission
belong to Pool. [Custom builds and Pool setup](specification.md#custom-builds)

## Running commands and protocol endpoints

Use the same native construction for one-shot commands and `serve`. One-shot
commands pay setup only for the mounts they use, and the foreground process
shares active mounts across its listeners. Each listener authenticates a
principal scoped to its export. The selected directory supplies the protocol root and the Core authorization
boundary. JSON HTTP uses absolute Core paths under that boundary. S3 maps
one path-style bucket to a native object key binding and passes the original
HTTP request target directly to the SigV4 adapter. Core authorizes the exact
key space. The adapter owns signatures, body validation and listing tokens.
For multipart, the host owns the private manifest database, Input store,
and the stable identity of the configured target. The adapter owns part
replacement, completion and cleanup. Core publishes the selected input.
Shutdown waits for handlers and recovery to finish before releasing the
state lock. For SFTP the host owns SSH
keys, authentication and connection cancellation. The SFTP adapter owns
file handles and directory cursors. FTP shares SFTP's TCP connection
lifecycle. The host binds passive listeners and checks the remote IP. The
FTP adapter owns each data connection, transfer cancellation and publication.
All control listeners bind before readiness and share one shutdown deadline. Core checks file authority and retires native
resources.

If there is a need for online mount changes, evaluate the existing Control
package for explicit, per-mount application. Control retires an old mount
before constructing its replacement. A failed construction can leave that
mount unavailable, and applying several definitions yields per-mount results.
Preserve Core's binding and retirement semantics when changing the running
namespace. Listener changes can continue to use restart.
[Mountweave Control contract](https://github.com/nostalume/mountweave/blob/v0.2.0/control/doc.go)

## Authorization ownership

| Available provider contract | Onboarding choice |
| --- | --- |
| Existing authorized client or profile | Reuse it. S3 can use the AWS SDK configuration chain. [AWS SDK configuration](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html) |
| Scoped app password or access token | Guide the user to the provider's issuance page when needed. Import through a protected input or resolve an existing reference. |
| Browser consent with a local callback | Use the system browser and the callback and PKCE parameters the provider supports. [OAuth for native apps](https://www.rfc-editor.org/rfc/rfc8252.html) |
| Consent on another device | Use the provider's device authorization flow and observe its polling interval, terminal states and expiry. [Device authorization](https://www.rfc-editor.org/rfc/rfc8628.html) |
| App credentials exchange for access | Resolve approved application credentials through protected references and bind the returned access to its authorized subject and recipient. |

Application ownership is independent of the consent flow. A user-supplied
application can use browser consent. An approved shared public client reduces
setup for users and gives the project responsibility for registration, quotas
and availability. A distributed binary cannot keep a client secret confidential.
A confidential client belongs to its authorized operator or backend.

The selected source defines the accepted authorization methods, required
permissions, stable account identity and credential recipients. The CLI owns
interaction and persistence. The native Driver verifies the selected root
and runs file operations. For sources with interactive managed authorization,
reauthorization replaces the same account's grant and retains the scope needed
by shared mounts. Switching accounts verifies the new root and updates one
mount reference. Ordinary edits and automation can reuse a saved reference.

An external profile follows the provider or SDK's own lifecycle. For a
reusable refresh credential, a current access token can serve requests from
memory. A renewal takes the writer lock for the grant, reloads the latest
record and saves the accepted update. Retry with the previous refresh
credential only when the provider contract permits it. Dropbox and Google
Drive use this path with different callback and application requirements.
[Dropbox authorization](https://docs.dropboxapi.com/riviera/docs/get-started/authorization) ·
[Google desktop authorization](https://developers.google.com/identity/protocols/oauth2/native-app)

123 Open issues another access token from an approved application credential
pair. It shares the access-token cache and writer lock with reusable refresh
tokens. The grant stores a reference to the secret. Each newly issued token
must resolve to the original account before it can reach file requests.

Microsoft Graph device authorization pins the Entra tenant and binds the
grant to the account returned by Graph. Renewals keep the same tenant,
client and account. The cache publishes the new token pair together.
Entra permits using the previous refresh credential when the result of
renewal is lost. [Microsoft Entra refresh tokens](https://learn.microsoft.com/en-us/entra/identity-platform/refresh-tokens)

A CLI-managed grant whose refresh can consume the old credential needs
one durable record and one refresh owner at a time. Coordinate by the
record's identity across CLI and `serve` processes. It stays stable when the
mount path or token changes. Protect the record and record the attempt before
sending a refresh that can invalidate it.

With Mountweave v0.2.0, an AliPan Driver owns and closes its credential
source. Its process lock spans Driver construction, use and cleanup. Independent Drivers for the same rotating record therefore
require a proven shared-owner or borrowing contract. When the owner is
busy, a second direct invocation reports the conflict and can use an
already configured export if it covers the operation. On restart load the
latest confirmed checkpoint or reconcile an uncertain rotation before resuming.
[AliPan credential contract](https://github.com/nostalume/mountweave/blob/v0.2.0/drivers/alipan/README.md#rotating-credentials)

115 Open borrows an authenticated HTTP client. The CLI owns its rotating
credential source and closes it before releasing the writer lock. Its phone
scan and token endpoints belong to the provider adapter. The managed
authorization setup shares account selection, reauthorization and publishing
with the browser flow. The existing file Driver confirms directory access.

Cookie import from browser sessions is outside the initial design. For
browser-based OAuth, the browser may reuse its existing provider login and the
CLI receives the authorization result. Any future cookie-based connection needs
a documented service contract and separate review of scope, expiry and storage.
