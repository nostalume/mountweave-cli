# Mountweave CLI

A headless command-line host for [Mountweave](https://github.com/nostalume/mountweave).
It is designed for local file work, scripting and serving the same mounts to
external clients through Mountweave's protocol adapters.

`mweave` connects local directories, network shares, cloud drives and object
storage. It supports guided setup, browsing, file transfer and file management,
and serves directories over WebDAV, JSON HTTP, SFTP and FTP, and object
mounts over S3.

Build from this directory with Go 1.27 or newer. For a smaller binary, use
`go build -trimpath -ldflags="-s -w" -o mweave .` on Linux or change the output
name to `mweave.exe` on Windows. Omit `-ldflags` when you need debug symbols.
The default build uses published dependencies and can be built with
`GOWORK=off`. For capabilities under development, see the
[candidate build](docs/specification.md#candidate-build).

## Connect storage

~~~sh
mweave mount add /files
mweave mount list
~~~

See [source setup and capabilities](docs/specification.md#source-options)
for network shares, cloud drives, self-hosted storage and object stores.
For direct cloud connections, see [Aliyun Drive Open](docs/specification.md#connect-aliyun-drive-open) and [Tencent Weiyun](docs/specification.md#connect-tencent-weiyun).

## Work with files

~~~sh
mweave ls /files --limit 20
mweave stat /files --json
mweave put ./report.pdf /files/report.pdf
mweave get /files/report.pdf ./copy.pdf
~~~

Run the built binary from your shell or add it to your PATH. For automation,
use `mount add --from FILE` or `mount add --from -` with a
[mount record](docs/specification.md#configuration-and-secrets).
Use `mweave help` for file commands, `mweave mount --help` for mount
maintenance, and `mweave mount add --help` for source names.
[Custom builds](docs/specification.md#custom-builds) can register an external
Go Driver or enable a storage Pool.

## Serve a mounted directory

~~~sh
mweave export add
mweave serve
~~~

Choose a mounted directory and a client credential reference. The guide
defaults to WebDAV on loopback with read-only access. Scripts can supply a
record with `export add --from FILE` and use `serve --json` to wait for
readiness. SFTP uses a host private key and a client's public key. FTP
uses passive transfers, with cleartext available only on loopback.
S3 clients use path-style addressing and their own signing credentials.
See [protocol setup](docs/specification.md#protocol-setup).

- [Architecture](docs/architecture.md#architecture-decisions) explains the
  decisions and ownership boundaries.
- [CLI specification](docs/specification.md) defines commands, configuration,
  output and protocol behavior.
- [Verification and delivery](docs/specification.md#verification-and-delivery)
  gives the acceptance cases and current evidence.
