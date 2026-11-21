# Mountweave CLI

`mweave` mounts local folders, cloud drives, network shares and object
storage. Browse the same files from a terminal or script, or serve them over
WebDAV, JSON HTTP, SFTP, FTP/FTPS or S3. [Mountweave](https://github.com/nostalume/mountweave)
provides file operations and protocol adapters.

## Install

Release archives target Windows and Linux on x86-64. The installers select
the latest stable release by default. Scoop and Nix follow the package
metadata in this repository. To use a source checkout, see
[Build from source](CONTRIBUTING.md#build-from-source).

### Windows

With Scoop installed, add the bucket and install the package.

```powershell
scoop bucket add mweave https://github.com/nostalume/mountweave-cli
scoop install mweave/mweave
```

You can also download and run the PowerShell installer.

```powershell
Invoke-WebRequest -UseBasicParsing 'https://github.com/nostalume/mountweave-cli/releases/latest/download/install.ps1' -OutFile install.ps1
.\install.ps1
```

### Linux

With Nix and flakes enabled, add the package to your profile.

```sh
nix profile add github:nostalume/mountweave-cli#mweave
```

You can also download and run the POSIX installer.

```sh
curl -fL https://github.com/nostalume/mountweave-cli/releases/latest/download/install.sh -o install.sh
sh ./install.sh
```

Run `mweave version` to check the installed version. The scripts print the
install location and any `PATH` setup it needs. To install a specific tag
or use another directory, pass `--version TAG` or `--bin-dir DIR` on Unix
and `-Version TAG` or `-BinDir DIR` on Windows. You can also download a
versioned archive from [Releases](https://github.com/nostalume/mountweave-cli/releases),
compare its SHA-256 with that release's `SHA256SUMS`, then extract the binary.
The commands above follow the stable channels. Use your package manager to
upgrade or uninstall. For a script install, rerun the script to upgrade or
delete the installed executable to uninstall. Configuration and credentials
remain in their user directories.

## Mount a folder

In a new working directory, create a directory named `data` and save a short
file named `sample.txt` inside it. Run the first command, choose `local`,
enter `data` at the **Local directory** prompt and confirm.

```sh
mweave --config demo.json mount add /files
mweave --config demo.json ls /files
mweave --config demo.json get /files/sample.txt copy.txt
```

`/files` is the mounted address inside mweave. `ls` shows `sample.txt`, and
`copy.txt` contains its bytes. Use a new output filename if `copy.txt` already
exists. To connect cloud storage, use `mount add /cloud` and choose a route
from [Storage sources](docs/sources.md#cloud-authorization).

## Share the mount

Save a password in a private file named `webdav-password.txt` beside
`demo.json`, outside `data/`. In the export wizard choose WebDAV, `/files`,
the default loopback listener `127.0.0.1:8787` and read-only access. Enter
`reader` as the username and `file:webdav-password.txt` as the password
reference. Leave the TLS certificate blank for this loopback example and
confirm.

```sh
mweave --config demo.json export add
mweave --config demo.json serve
```

After readiness, open `http://127.0.0.1:8787/sample.txt` in a WebDAV client
with those credentials. It should return the sample bytes. Stop the server
with Ctrl+C.

## Configure and automate

The default config is `%LOCALAPPDATA%\mweave\config.json` on Windows and
`${XDG_CONFIG_HOME:-$HOME/.config}/mweave/config.json` on Linux. `--config`
selects another JSON file. The configuration stores references to secrets.

Use `mweave --config demo.json mount add --from mount.json` to load a
[prepared record](docs/configuration.md#use-a-prepared-record). Add `--json`
to commands that return structured [results](docs/specification.md#results-and-exit-status).

- [Configuration and recovery](docs/configuration.md)
- [Commands and protocol details](docs/specification.md)
- [Architecture](docs/architecture.md) and [contributing](CONTRIBUTING.md)

## License

MIT. See [LICENSE](LICENSE).
