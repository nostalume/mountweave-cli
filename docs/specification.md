# CLI specification

This specification defines the target `mweave` behavior. The
[architecture decisions](architecture.md#architecture-decisions) explain its
boundaries, and the [README](../README.md) shows the available workflow.

## Connect a mount

1. Run `mweave mount add` and enter a service name or access type. Enter `?`
   to list sources. If several services match, show their names and descriptions.
   Choose a connection method if the service offers more than one. For unattended
   setup, supply the saved source and connection in the record.
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

`mount list` shows saved mounts without connecting. Use `stat /nas` to see
the entry at that path or `ls /nas` to browse its contents. File operations
report access failures from the selected mount. Credential changes follow
the [editing contract](#editing-credentials).

## Configuration and secrets

Use one strict JSON configuration file. CLI edits write the same configuration
that unattended callers may supply directly. Reject unknown fields, duplicate
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

~~~json
{
  "version": 1,
  "mounts": [
    {
      "at": "/nas",
      "driver": "webdav",
      "options": {
        "root_url": "https://storage.example/dav/documents/",
        "username": "alice"
      },
      "credential": { "kind": "env", "ref": "NAS_APP_PASSWORD" }
    }
  ],
  "exports": [
    {
      "protocol": "webdav",
      "listen": "127.0.0.1:8787",
      "root": "/nas/docs",
      "auth": {
        "username": "reader",
        "credential": { "kind": "env", "ref": "NAS_EXPORT_PASSWORD" }
      }
    }
  ]
}
~~~

The file contains `version`, `mounts` and optional `exports`. An omitted
export list is empty. A mount record has `at`, `driver`, `options` and an
optional `credential`. The `driver` field names the source. Use `connection`
when that source offers multiple methods: `website` or `open` for `alipan`,
and `webdav` or `open` for `pan123`. Existing records for these two sources
that omit the method use Open. Earlier `alipan-web` records still load as
website connections. A build without the website connection lists those
saved mounts as unavailable. Local omits the credential and takes `options.root`.
For example, `{"at":"/files","driver":"local","options":{"root":"./data"}}`
mounts a directory relative to the configuration file. A reference has `kind`
and `ref`. The kinds identify an environment variable (`env`), a protected
file (`file`), an external provider or SDK profile (`profile`), or a record
in the host's protected store (`managed`). A route advertises which kinds it
can resolve.

The config holds references and public settings. Resolve secrets at
construction and pass them to native options in memory. A URL containing
authorization material uses a protected reference where the Driver supports it.

### Source options

| Driver | Required setup and root selection |
| --- | --- |
| `alipan` | Choose [website sign-in](#connect-aliyun-drive-through-the-website) or [official scan authorization](#connect-aliyun-drive-open). The wizard records the validated drive and folder and stores a protected authorization reference. |
| `local` | `root` is a host directory. |
| `webdav` | `root_url` selects the collection. Set `username` and a credential reference for Basic authentication, or omit both for anonymous access. |
| `pan115` | Use [phone scan authorization](#connect-115-open) or reference an existing access token. Select a folder with `root_path` or `root_id`, or omit both for the account root. |
| `pan123` | Use [application credentials](#connect-123-open) or reference an existing access token. Select a folder with `root_path` or `root_id`, or omit both for the account root. |
| `baidupan` | Set `root_path` to an explicit absolute directory covered by your Baidu Netdisk authorization. Reference an existing access token. |
| `graphdrive` | Set a known `drive_id` and a drive-relative `root_path` or `root_id`. Use `/` for the drive root. Choose [device authorization](#connect-microsoft-graph) for global Microsoft Graph, or reference an existing access token for the selected endpoint. |
| `googledrive` | Use [browser authorization](#connect-google-drive) or reference an existing Drive access token. Set `root_id` or `root_path`, or omit both for the authorized root. Use a folder ID for a shared drive or one of its folders. |
| `dropbox` | Use [browser authorization](#connect-dropbox) or reference an existing Dropbox access token. Set `root` to `default` for the app's authorized root, `account` for the account root, or `namespace` with `namespace_id`. An optional `root_path` selects a folder within that root. |
| `cloudreve` | Set the V4 server `endpoint` and `root_uri` for a folder in the account tree, such as `cloudreve://my/Documents`. The guide defaults to `cloudreve://my/`. Reference an existing access token. |
| `seafile` | Set `server_url`, including the installation path if present, and the `library_id` of an unencrypted library. `root_path` defaults to the library root. Reference a Seafile API token. |
| `feishudrive` | Set `root_id` to the native folder token. Reference an app or user access token that can read the folder on the mainland Feishu service. |
| `fangcloud` | Set `root_path` or a positive numeric `root_id` as a string. Omit both for the personal root. Reference an enterprise integration's user access token. |
| `guangyapan` | Set the approved application `client_id` and optionally `root_id`. Reference a user token for that application. Omit `root_id` for the account root. |
| `kdocs` | Set `root_path` to a folder under My Documents or omit it for the root. Reference a user token with personal-file listing and download access. |
| `box` | Reference a Box access token with read access. Set `root_id` to a folder ID or omit it for the account root (`0`). |
| `tencentdocs` | Set the folder `root_id`, application `client_id`, and user `open_id`. Reference the matching user access token. Run the integration from the approved application's backend. |
| `lanzou` | Use [website sign-in](#browse-a-lanzou-cloud-account) to browse and download from a private folder. The wizard stores a protected session reference and the selected folder ID. |
| `weiyun` | Use [browser authorization](#connect-tencent-weiyun) or reference a personal MCP token. Browser authorization covers the entire account. Mount its root or select a read-only subfolder with `root_path`. |
| `yunpan360` | Reference a personal 360 AI Cloud Disk API key. The key selects the account's personal root. |
| `gokuai` | Set `library_id` and optionally `root_path`. Reference the secret issued for that library's developer authorization. |
| `dingtalkdrive` | Set `space_id`, `root_id` (folder dentry ID), and `operator_union_id`. Reference an approved internal app access token. The operator needs access to the folder. |
| `wecomdrive` | Set `space_id` and `root_id` (folder ID). Reference an app access token authorized for Wedrive. |
| `tencentsmh` | Set `library_id` and, for a multi-tenant library, `space_id`. An optional `root_path` selects a directory. Reference a business access token for the library and space. Set `endpoint` in the mount record for a dedicated HTTPS API origin. |
| `sftp` | Set `address` and `username`. `root_path` defaults to the remote home. `known_hosts` defaults to the current user's SSH known hosts file. Use a password reference, or set `identity_file` and reference its passphrase if encrypted. |
| `ftp` | Set `address`, `username` and a password reference. `root_path` defaults to `/`. `security` defaults to `explicit_tls`. Use `implicit_tls` for implicit FTPS or `plain` for a server that requires unencrypted FTP. A `ca_file` supplies trusted PEM certificates for FTPS in place of system trust. |
| `smb` | Set `server`, `share`, `username` and an NTLM password reference. Set `domain` if required. `root_path` defaults to the share root. The client requires SMB message signing. |
| `azureblob` | Set `container_url` without a query and reference a container SAS token. An optional slash-terminated `prefix` limits the exact-key view. |
| `bunny` | Set `root_url` to the regional storage URL including the zone and optional folder. Reference the zone password. |
| `upyun` | Set `root_url` including the bucket and optional folder, and `operator`. Reference the operator password. |
| `githubrepo`, `atomgit` | Set `repository` to `owner/name` and `ref` to a commit or a supported branch or tag reference. An optional `root_path` selects a subtree. Use a token reference for private access. |
| `githubreleases` | Set `repository` to `owner/name` and `tag` to a published release tag. Use a token reference for private access. |
| `cnbreleases` | Set `repository` to the CNB path, such as `org/team/project`, and `tag` to a published release tag. Reference a token with `repo-release:r`. |
| `codeartsrepo` | Set the regional HTTPS `endpoint`, numeric `repository_id` and full `commit_sha`. An optional `root_path` selects a subtree. Reference a Huawei Cloud IAM user token. |
| `ipfs` | Set `root_cid` to a UnixFS directory CID. `endpoint` defaults to the local Kubo RPC at `http://127.0.0.1:5001`. HTTPS endpoints can use a Kubo bearer token reference. |
| `s3` | Set `bucket`. An optional slash-terminated `prefix` limits the view. The AWS SDK supplies credentials and can supply the AWS region. A custom `endpoint` takes an explicit `region` and a named SDK `profile` with its own credential source. |

Sources with a password or token use an `env` or `file` reference. A
credential file contains a single value and may end with a line break.
Relative paths in saved records, including SSH trust and identity files
and FTPS CA files, resolve against the configuration directory. A server
address can include a port. The defaults are 22 for SFTP, 21 for FTP and
explicit FTPS, 990 for implicit FTPS, and 445 for SMB.

For 115 Open and 123 Open, each token goes to its own control API.
Download URLs use a separate content client. Baidu Netdisk uses the token
in the native query or form fields and keeps it scoped to the initial
authorized origin in a download chain. For Azure Blob, put the SAS token
in the credential, with or without its leading `?`. Add the mount with a SAS
that can list the container. Keep browser authorization and renewal
separate from setup with an existing credential.

SFTP and SMB support exclusive file creation and directory creation.
Baidu Netdisk supports single-step exclusive creation and directory
creation. FTP/FTPS and UPYUN support directory creation. Bunny supports browsing
and reading. Upload and deletion commands require the selected Driver to
support the command's effect, including typed removal where applicable.

Azure Blob uses `key:/MOUNT:KEY`. Browse it with `ls key:/MOUNT:`. Its
default is read-only. Set both `part_size` and `max_buffered_bytes` to
enable native exclusive creation and complete replacement. The SAS
also needs the relevant mutation permission.
[Azure Blob source capabilities](https://github.com/nostalume/mountweave/blob/v0.2.0/drivers/azureblob/README.md)

Graph Drive defaults to `https://graph.microsoft.com`. Set `endpoint` in
the mount record for another Graph deployment and use a credential issued
for that deployment. The file body uses Graph's preauthorized download URL.
[Graph deployments](https://learn.microsoft.com/en-us/graph/deployments) ·
[Graph downloads](https://learn.microsoft.com/en-us/graph/api/driveitem-get-content?view=graph-rest-1.0)

The folder prompt for 115 Open, 123 Open, Graph Drive, Google Drive and
Fangcloud accepts a path beginning with `/` or a native folder ID. Enter
`-` to restore the default root.

Set Google Drive's `export_formats` in a mount record to map a Workspace
MIME type to an output MIME type, for example
`{"application/vnd.google-apps.document":"application/pdf"}`. Dropbox downloads
ordinary files with the same account token. To enable export for files the
download endpoint cannot return, set `export_format` in the mount record.
Use an empty string for the provider's default format. Omit the field to
request ordinary downloads only.
[Google Drive export](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/export) ·
[Dropbox namespace setup](https://developers.dropbox.com/en-us/dbx-team-files-guide)

Cloudreve and Seafile use the account token for their service API and a
separate client for temporary download URLs. Use an HTTPS origin for a
remote server. Seafile binds a saved token reference to the server's
installation URL, including its path.
[Cloudreve authentication](https://docs.cloudreve.org/en/api/auth)

Feishu lists folders and uploaded files by default. Set `export_formats` in
the mount record to enable selected online document types, for example
`{"docx":"pdf"}`. A `get` or `cp` of an enabled document starts an export and
streams the completed result. Use a destination name with the chosen
extension, such as `mweave get /team/Notes ./Notes.pdf`. Listing and metadata
queries leave export tasks alone. A cancelled command ends local polling.
Feishu can finish a task already accepted by the service.
[Feishu export formats and source capabilities](https://github.com/nostalume/mountweave/blob/drivers/feishudrive/v0.1.0/drivers/feishudrive/README.md)

A Guangya Pan token reference binds to its application ID. Supply a
compatible reference when switching applications. Guangya Pan, Fangcloud and
KDocs use their control API to obtain a temporary URL for the original file.
A separate client streams the result.
[Guangya Pan business API](https://github.com/guangyapan/agent-skills/blob/master/skills/guangyapan-open-api/references/business-api.md) ·
[Fangcloud setup](https://open.fangcloud.com/wiki/v3/) ·
[KDocs list](https://developer.kdocs.cn/server/personal/files.html) ·
[KDocs download](https://developer.kdocs.cn/server/personal/download.html)

Box streams ordinary files. A web link can appear in a listing and returns
an unsupported-operation error when opened. Downloads can redirect to a
temporary URL. That request uses a separate client.
[Box folder IDs](https://developer.box.com/reference/get-folders-id) ·
[Box downloads](https://developer.box.com/reference/get-files-id-content)

Tencent Docs browsing requires `scope.drive.readonly`. A credential
reference binds to the configured `client_id` and `open_id`. To stream the
provider's default export for a supported document, set `allow_export` to
`true` and grant `scope.drive.exportable`. Listing and stat do not submit
exports. Each export request counts toward the provider's daily limit of
nine per user, including a submitted export whose local copy later fails.
Cancellation ends local polling. A task already accepted by Tencent Docs
may continue at the provider.
[Tencent Docs authorization](https://docs.qq.com/open/document/app/oauth2/) ·
[Folder listing](https://docs.qq.com/open/document/app/openapi/v2/file/folders/list.html) ·
[Export and quota](https://docs.qq.com/open/document/app/openapi/v2/file/export/async_export.html)

Weiyun and 360 AI Cloud Disk use `options: {}` for their default setup.
Weiyun can use a saved browser authorization and an optional `root_path`
inside the authorized folder. With explicit references, use
a separate `env` or `file` credential for each provider.
Mount setup checks the authorized root by listing it. Both sources provide
read-only file access. Downloads depend on an HTTPS grant from the provider.
360 describes its personal API keys as a limited noncommercial trial.
[Weiyun token](https://www.weiyun.com/act/openclaw) ·
[360 API key](https://open.yunpan.360.cn/docs/mcp-server/preparation/) ·
[360 download link tool](https://open.yunpan.360.cn/docs/skills/tools/#get-download-url)

Gokuai, DingTalk, WeCom and SMH support browsing and ordinary file reads.
For Gokuai, choose the library ID and secret from its developer grant.
For DingTalk, use an internal app with `Storage.File.Read` and
`Storage.DownloadInfo.Read`. For WeCom, an online document may appear in a
listing while its body is unavailable. Tencent SMH defaults to
`https://smh.tencentcs.com`.
[Gokuai library setup](https://developer.gokuai.com/grant/library.html) ·
[DingTalk authorization](https://www.alibabacloud.com/help/zh/polardb/polardb-for-postgresql/sync-dingtalk-documents-to-the-knowledge-base) ·
[Wedrive downloads](https://developer.work.weixin.qq.com/document/path/97881) ·
[SMH business token](https://cloud.tencent.com/document/product/1339/71159)

A Gokuai secret reference binds to the selected library. An SMH token
reference binds to its API origin, library and space. Directory changes
within that scope can keep the reference. Supply a compatible reference
when changing the bound scope.

Repository trees, release assets and IPFS are read-only sources. GitHub Repo
and AtomGit resolve a ref once per Driver instance. A later CLI invocation or
serve restart resolves a moving ref again. Specify a full commit SHA for
repeatable selection across invocations. Repository reads return stored Git
bytes, including Git LFS pointers. CodeArts Repo requires a commit SHA.

GitHub Releases resolves a tag to a release ID for the life of the instance.
Its assets remain live. CNB resolves the current published tag on each native
lookup. Both expose uploaded assets. Download redirects use a separate
credential-free client.
[GitHub release assets](https://docs.github.com/en/rest/releases/assets#get-a-release-asset) ·
[CNB Open API](https://docs.cnb.cool/zh/develops/openapi.html)

IPFS trusts the selected Kubo RPC service to read the configured CID. Use a
trusted local or private endpoint. Kubo RPC is a privileged service. Optional
Bearer authentication is available over HTTPS and binds to the configured
origin and proxy path prefix.
[Kubo RPC and authentication](https://docs.ipfs.tech/reference/kubo/rpc/#authentication)

Choose `nutstore` in `mount add` to start with the Nutstore WebDAV
address. Use your account email and a third-party app password reference.
The saved record keeps the selected collection URL.
[Nutstore setup](https://help.jianguoyun.com/?p=5677)

Run `mweave mount add /123Pan`, choose `pan123`, and select `webdav`.
The wizard opens 123 Cloud Drive. In Tools → Third-party mounting, create
a WebDAV app for the intended folder and choose its permissions. Back in
mweave, accept the default URL and enter the app username and password
reference.
[123 Cloud Drive WebDAV guide](https://www.weibo.com/ttarticle/p/show?id=2309405214103219339373)

Run `mweave mount add /FebBox` and choose `febbox`. The wizard opens
the FEB console. In Settings → WebDAV, create an account for the intended
folder. Back in mweave, enter the URL and username shown by FEB and a
reference to its WebDAV password. mweave prints the setup address if it
cannot open your browser.
[FEB WebDAV](https://www.febbox.com/faq)

### Browse a LanZou Cloud account

Run `mweave mount add /LanZouPan` and choose `lanzou`. Sign in to the
official website, close the sign-in window, and confirm in the terminal.
Choose a private folder or accept `/`. The mount supports listing, metadata,
and downloads through the current password-protected LanZou download page.
Downloads use the file's existing share and stream the original from an
isolated browser context. The dedicated sign-in opens in a visible window.
Use `mweave mount edit /LanZouPan` to sign in again.

### Browse a China Mobile Cloud personal drive

In the [candidate build](#candidate-build), run
`mweave mount add /MobilePan` and choose
`mobilepan`. Sign in on the official website, close the window, and confirm in
the terminal. Choose a folder or accept `/`. The mount supports listing and
original-file downloads. Use `mweave mount edit /MobilePan` to sign in again
with the same account and folder.

### Connect an existing OpenList export

If OpenList already has access to a drive, use its WebDAV export as the
source. In OpenList, use an account whose base path is the directory you
intend to mount and grant it WebDAV read access. Add write permissions in
OpenList if you need to make changes. In `mweave mount add /ali`, choose
`webdav`, enter the instance's `https://HOST/dav/` URL, and supply the OpenList
username and password reference. Set the storage's WebDAV policy to
**Native proxy** for downloads. The OpenList host then streams the file and
uses its own network bandwidth. OpenList obtains and renews the provider
authorization. The same WebDAV mount works from scripts using a saved record.
For a fresh AliPan setup in OpenList, follow its own authorization instructions.
[OpenList WebDAV](https://doc.oplist.org.cn/guide/advanced/webdav) ·
[OpenList WebDAV policy](https://doc.oplist.org.cn/guide/drivers/common#webdav-policy) ·
[OpenList AliPan setup](https://doc.oplist.org.cn/guide/drivers/aliyundrive_open)

S3 setup offers `aws`, `bos`, `cos`, `kodo`, `ks3`, `oss`, `r2`,
`us3` and `zos`. Regional presets provide an example endpoint. Enter
the endpoint for your bucket's region if it differs. R2 asks for the
account's S3 endpoint. The saved record holds the endpoint, signing
region and list settings. For example, a named reference
has `"credential":{"kind":"profile","ref":"my-storage"}`. Use `path_style`
for a service configured for path-style addressing. Add a mount or replace
its credential to verify the selected prefix with a listing request.
A bucket can also be listed later with `ls key:/objects:`.
[AWS SDK settings and credentials](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html) ·
[Compatible S3 settings](https://github.com/nostalume/mountweave/blob/v0.2.0/drivers/s3/README.md#compatible-s3-services) ·
[ZOS endpoints](https://www.ctyun.cn/document/10026735/11093379)

## Cloud authorization

| Method | Sources | First connection |
| --- | --- | --- |
| Browser | Dropbox, Google Drive, Tencent Weiyun | Approve access in the browser. Dropbox and Google Drive use an application you provide. |
| Phone scan | Aliyun Drive Open, 115 Open | Enter the approved App ID and scan the QR code with the provider's app. |
| Device code | Microsoft Graph | Open the Microsoft verification page and enter the displayed code. |
| Application credentials | 123 Open | Enter the Client ID and a reference to its Client Secret. |
| Existing credential | Sources that show `reference` or ask for an existing credential | Provide the credential reference shown in the wizard. |

The wizard waits for the provider to complete authorization. It then shows the
remaining setup choices and asks before saving the mount. A compatible saved
authorization is offered before starting a new one. A mistyped menu option
can be entered again at the same prompt.

## Connect Dropbox

On Windows or Linux, run `mweave mount add /dropbox`, select `dropbox`,
then choose `browser`. Pick a saved account or authorize one in the
browser, select the root and folder, and confirm. The CLI checks the
directory before saving the mount. An authorization that was already saved
can be reused if the directory check fails.

The first browser authorization needs a Dropbox user-scoped app key.
In the [Dropbox App Console](https://www.dropbox.com/developers/apps),
register the exact redirect `http://localhost:53682/callback` and enable
`account_info.read`, `files.metadata.read` and `files.content.read`.
For write access, enable `files.content.write`, set `allow_create` on the
mount with `mount edit --from`, then reauthorize.
Enter the public app key in the wizard or set `MWEAVE_DROPBOX_APP_KEY`.
When switching accounts, the wizard offers the current mount's app key.
The local port must be available during consent. No app secret is needed.
Choose `reference` to supply an existing token through `env:` or `file:`.
[Dropbox OAuth flow](https://docs.dropboxapi.com/riviera/docs/get-started/authorization)

For a browser-authorized mount, `mweave mount edit /dropbox` offers
`settings`, `reauthorize` and `switch`. Reauthorization keeps the same
account and app and updates the shared record. It retains the original scopes
and requests any additional scope required by saved mounts in the current
configuration. A switch probes the new
account's selected folder and updates just this mount. `--from` can
reuse a saved `managed` reference and runs without a browser.

## Connect Google Drive

On Windows or Linux, run `mweave mount add /drive`, select `googledrive`,
and choose `browser`. Pick a saved account or provide the path to a
Desktop OAuth client JSON downloaded from Google Cloud. Enable the Drive
API in that project and make sure the account can consent to
`drive.readonly`. This scope lets the CLI browse and download existing
Drive files. The wizard uses a local port selected by the operating system.
[Desktop app setup](https://developers.google.com/identity/protocols/oauth2/native-app) ·
[Drive scopes](https://developers.google.com/workspace/drive/api/guides/api-specific-auth)

For an external OAuth app in Testing, Google issues refresh tokens for
this scope that expire after seven days. Public apps and managed accounts
are subject to Google's verification and organization policies.
[Google token expiration](https://developers.google.com/identity/protocols/oauth2/#expiration) ·
[App state and organization policy](https://developers.google.com/identity/protocols/oauth2/production-readiness/overview)

Choose `reference` to use an existing access token instead. For browser
authorization, the CLI stores the application settings and grant in the
protected store. Use `mount edit` to reauthorize or switch accounts.

## Connect Aliyun Drive Open

On Windows or Linux, run `mweave mount add /AliPan`, choose `alipan`, and
select `open`. Choose read access for browsing and recovery. Enter an approved App ID or set
`MWEAVE_ALIPAN_APP_ID` before running the wizard. Scan the QR code in
the Aliyun Drive app. The CLI opens the image or displays it in the terminal.
After authorization, choose the drive and directory. Reusing a saved
authorization skips the scan. For an existing
migrated resource drive, choose `resource` and the original directory.
The wizard obtains the numeric drive ID from Aliyun Drive and saves the
authorization in the protected store.

The application must be allowed to issue a renewable grant. Use the same
account and resource drive to reopen an existing Rustic repository. A WebDAV
export for recovery should be read-only and rooted at the repository directory.
Choose read/write access only when you need ordinary file creation. That
mode enables exclusive creation through the driver. WebDAV backup writes
require complete publication, which this integration does not yet provide.
[Aliyun Drive developer portal](https://www.alipan.com/developer/) ·
[Official QR authorization](https://github.com/alibaba/aliyunpan-ios-sdk/blob/main/Sources/AliyunpanSDK/AliyunpanCredentials/AliyunpanQRCodeCredentials.swift)

## Connect Aliyun Drive through the website

In the [candidate build](#candidate-build), run `mweave mount add /AliPan`,
choose `alipan`, and accept the default `website` connection. Sign in in
the browser window mweave opens. Leave
the site's remember sign-in option enabled if offered. When a plain text
page appears, close that window and accept any browser close confirmation.
On macOS, quit the sign-in browser. Then answer `y` in the terminal.
mweave verifies the sign-in before asking for the resource drive folder
and saving the mount. The session belongs to the account; the mount can
browse and download from the selected folder.

On Windows, the sign-in window uses the system's default HTTPS browser if
supported. On Linux and macOS mweave looks for a supported Chromium-based
browser. The window uses a private profile kept by mweave. The website
supplies a temporary download URL, and mweave streams the file over HTTPS.

To sign in again, use `mweave mount edit /AliPan`. mweave verifies the same
account, drive and folder before replacing the session. Stop a running
`serve` instance before editing, then restart it. If the website's
verification fails, try its QR sign-in option if available.

## Connect Tencent Weiyun

Run `mweave mount add /Tencent` and select `weiyun`. Choose
`browser`, approve access on the Weiyun page, and wait for the CLI to detect it.
After approval, enter a folder path such as `/Documents` or press Enter to mount
the authorized root. Weiyun grants permission to operate on all files in the
account. The mount supports browsing and downloading within the selected
folder. To replace its authorization, use `mount edit /Tencent` and
select `switch`. The `reference` option accepts an existing personal MCP
token via `env:` or `file:`.
[Weiyun authorization](https://www.weiyun.com/act/openclaw)

## Connect 115 Open

On Windows or Linux, run `mweave mount add /115`, choose `pan115` and
select `scan`. Pick a saved account or provide an approved App ID, then
scan the QR code with the 115 app and confirm. The CLI opens the image or
shows it in the terminal. Set
`MWEAVE_115_APP_ID` to reuse the App ID. The scan flow uses PKCE with the App ID. [115 Open developer registration](https://open.115.com/) ·
[115 Open scan authorization](https://open.115.com/doc/115开放平台/接入指南/接入授权/手机扫码授权PKCE模式/获取设备码和二维码内容.md?raw=true)

Choose `reference` to continue using an existing access token. A managed
mount checks the token expiry before a file request. Only one running Driver
can own a rotating authorization record at a time. Multiple saved mounts
can reuse that record in separate runs. Stop the running owner before
reauthorizing it through `mount edit /115`. The CLI checks the account ID
and probes each sharing mount in the selected configuration before saving
the replacement. An unconfirmed refresh requires reauthorization. The CLI
keeps the unresolved checkpoint across process restarts. If 115 rejects an
access token before its local expiry, reauthorize from the mount editor.
[115 Open refresh contract](https://open.115.com/doc/115开放平台/接入指南/接入授权/刷新access_token.md?raw=true)

## Connect 123 Open

On Windows or Linux, run `mweave mount add /123`, choose `pan123`, select
`open`, and then choose `app`.
Pick a saved account or enter a Client ID and a Client Secret reference
such as `env:MY_123_SECRET` or `file:/path/to/secret`. Use credentials
issued for the 123 Cloud Drive account you intend to mount.
[123 Open application setup](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/hpengmyg32blkbg8)

The CLI verifies the account UID and probes the selected directory.
It saves the secret reference and expiring access token in the protected
store. The secret is resolved again only when the CLI needs a new token.
Shared mounts reuse the cached token across runs. Choose `reference` to
keep using a token supplied by another tool. The `webdav` connection uses
its own WebDAV credentials.
[Token issuance](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/gn1nai4x0v0ry9ki) ·
[User identity](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/zgf9gyh7gvmdl4a3)

Use `mount edit /123` and choose `reauthorize` to update the application
credentials for the same account. The CLI probes every mount in the
selected configuration that shares the grant. Choose `switch` to bind
only this mount to another account.

## Connect Microsoft Graph

On Windows or Linux, run `mweave mount add /onedrive`, select
`graphdrive`, and choose `device`. Pick a saved account or enter the
Application (client) ID and the Directory (tenant) ID. Use `consumers`
for personal Microsoft accounts. The Entra application must allow public
client flows and the applicable account type. The CLI requests delegated
`Files.Read`, `User.Read`, and `offline_access`. The app registration and
any required organization consent are handled in Entra.
[App registration](https://learn.microsoft.com/en-us/entra/identity-platform/scenario-desktop-app-registration)

Open the Microsoft verification URL shown in the terminal on a browser
you trust and enter the code. After confirmation, enter the drive ID and
choose the folder. The CLI verifies the account in the selected tenant and
checks the folder with the native Driver. Use `mount edit` to reauthorize
the same account or switch this mount to a different account. The existing
`reference` option supports external tokens, including for custom Graph
endpoints. Managed device authorization uses global Microsoft Graph.
[Device authorization](https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-device-code) ·
[Graph drive permissions](https://learn.microsoft.com/en-us/graph/api/drive-get?view=graph-rest-1.0)

## Custom builds

A Go executable can import `github.com/nostalume/mountweave-cli/cli`.
Call `cli.NewSource(id, label, prompt, prepare)` for each extra Driver, pass
the returned sources to `cli.New`, and use `os.Exit(app.Main())`. `app.Run`
accepts a context, arguments and streams for embedding. A source ID must be
unique within the build. Stock binaries can list saved external mounts and
show that the required source is unavailable.

Use a struct with explicit JSON tags for public settings. `prepare(options,
configDir)` validates settings locally and returns a `cli.PreparedSource`
whose `Open` constructs a fresh native Driver. `prompt` can collect public
settings or be nil for `--from` only. Store credentials separately as existing
environment or file references. If the source uses a credential, set
`CredentialOrigin` to a stable identifier for its actual recipient and
`CredentialAccount` when the account is part of that boundary. The host
compares recipients for shared references. The native constructor controls
where it sends the secret.

### Candidate build

The default build uses published modules. A saved mount that requires a
candidate source remains visible in `mount list` and reports the source
as unavailable. Select the candidate build to use the Aliyun Drive website,
China Mobile Cloud website, a Weiyun subfolder, or the WebDAV
`create_only` export policy.

Create a local Go workspace containing this CLI checkout, Mountweave Core,
and the `alipan`, `mobilepan` and `weiyun` Driver modules from that Core
checkout. With that workspace active, build with
`go build -tags=mweave_candidate .`. Add `mweave_pool` to the comma-separated
tag list when the same executable also needs Pool.

### Pool mounts

Build with `go build -tags mweave_pool -o mweave .` (use `mweave.exe` on
Windows). Add a `pool` mount with `mount add --from FILE`:

~~~json
{
  "at": "/pool",
  "driver": "pool",
  "options": {
    "metadata_path": "pool.db",
    "data_shards": 1,
    "parity_shards": 1,
    "shard_bytes": 1048576,
    "max_buffered_bytes": 2097152,
    "members": [
      {"id":"a","identity":"private-a","failure_domain":"disk-a","driver":"local","options":{"root":"private/a"}},
      {"id":"b","identity":"private-b","failure_domain":"disk-b","driver":"local","options":{"root":"private/b"}}
    ]
  }
}
~~~

Create the member directories and the metadata database's parent directory
first. Put the database on private local storage outside the member roots
and back it up. The mount probe can initialize the database before the
configuration save. If the save conflicts, reuse the database for the same
Pool on retry. Member paths and the
metadata path are relative to the configuration file unless absolute.
Each member describes its own Driver, settings and optional credential
reference. Use separate roots for other mounts. The CLI checks overlapping
Local paths and checks an offline member by its configured path. For remote services, supply stable identities for the actual
roots and use the same failure domain for roots that can fail together. Two
directories on the same disk share a failure domain.

Use the ordinary file commands and export setup for `/pool`. A member
explicitly set to `"offline": true` is skipped on startup. Other startup
failures report the affected member. Existing files remain readable while
enough verified shards are reachable. New data needs full writable
redundancy across distinct failure domains. A read-only member can serve
existing shards and adds no write capacity. Member Drivers must support
tree paths. The Pool database is exclusive
to its owning process. While `serve` holds it, access files through the
export. A local invocation that needs the same database returns
`pool_in_use`. Apply configuration changes with the Pool stopped and
restart the export when ready. Layout and member identity are checked by the
stored Pool metadata. The optional `max_bytes` sets a member cap on initial
creation. Use the Pool library for membership changes and repair.
[Pool capabilities and maintenance](https://github.com/nostalume/mountweave/tree/drivers/pool/v0.2.0/drivers/pool)

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
| Reauthorize the current account | Choose **Reauthorize current account** at the start of interactive `mount edit PATH` when the route supports managed authorization. Show the saved mounts that share the record before starting authorization. Verify the same provider account and required grant scope, then replace the protected record without rewriting the mount configuration. |
| Switch one mount to a different account | Select an existing compatible reference or set up a new one, then change that mount's reference. Verify the selected destination and access before saving the switch. Other mounts retain their own references. |

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

## Commands and arguments

Use `mweave [--config FILE] COMMAND [OPTIONS] OPERANDS`. The config flag
selects a file. Otherwise use `mweave/config.json` under the platform's
[per-user configuration directory](https://pkg.go.dev/os#UserConfigDir).
Command options can appear before or after operands. `--` ends option
parsing. Unknown options and duplicate scalar values are usage errors. Local
file operands use the host's path rules. Mount operands use absolute addresses
in the configured namespace.

### Command navigation

`mweave help` and `mweave --help` show the available commands. Show file
operations first. Show `serve` under protocol access when the build includes
it. Under settings, list `mount` and `export` as entry points for their
respective settings.

`mweave mount --help` and `mweave export --help` show the commands for
each group. `mweave mount add --help` shows the supported source types,
their option names and how to use `--from`. A leaf command's `--help`
shows its usage and options. Every help scope works without loading
configuration, constructing a Driver or opening a listener. `version`
reports the build under the same conditions.

### File commands

| Command | Behavior |
| --- | --- |
| `ls ADDRESS`, `stat ADDRESS` | List children or inspect one entry. `ls --limit N` limits the number of emitted entries. |
| `get ADDRESS FILE\|-` | Download to a new local file or stream bytes to stdout with `-`. The named form publishes the completed file and refuses to replace an existing destination. |
| `put FILE\|- ADDRESS` | Upload a local file or stream from stdin. The opened file supplies its length when known. For stdin, `--size BYTES` supplies an explicit length when the selected Driver requires it. |
| `cp SOURCE TARGET`, `mv SOURCE TARGET` | Use exact destinations and the same syntax across mounts. `cp -r` selects Core tree copy. `mv` uses Core Move for both files and directories. |
| `mkdir PATH`, `rm ADDRESS`, `rmdir PATH` | Create a directory, remove a non-directory, or remove an empty directory. `rm -r` selects the bounded Core tree removal for a tree path. |

Ordinary writes and copies create a new destination. A file `put --replace`
or `cp --replace` selects Core complete publication and allows replacement
or creation. A conflicting directory or an unavailable native guarantee
returns the Core error. Recursive copy and removal take tree paths. The
file-only `--replace` option is incompatible with `cp -r`. The CLI passes
typed removal constraints to Core. Directory plans use serial execution and
host defaults of 10,000 entries and 8 MiB of relative-path bytes.

Local complete publication uses `staging_directory` and `max_staging_bytes`.
Choose a private staging directory outside the mounted directory on the
same filesystem. On Linux, Local writes that require exact name lookup can
return `unsupported_operation` on unverified filesystems such as WSL Windows
shares (9p). S3 complete publication uses `part_size` and
`max_buffered_bytes`. The selected Driver and its service determine which
mutation guarantees are available.

### Settings and protocol access

| Command | Behavior |
| --- | --- |
| `mount list` | Show saved mount records and Driver availability in this build. |
| `mount add [PATH]`, `mount edit PATH`, `mount remove PATH` | Change saved mount records. Add and edit accept `--from FILE` or `--from -`. |
| `export add` | Save an export through guided setup or with `--from FILE` or `--from -`. |
| `serve` | Start listeners for the saved exports. See [Protocol setup](#protocol-setup). |

`mount add --from FILE` reads a complete mount record, including `at`. For
`mount edit PATH --from FILE`, `PATH` identifies the saved mount and the
record describes its replacement. These commands use the same validation and
protected references as the main configuration.

### Addressing

A tree path such as `/nas/report.pdf` is passed to Core as a tree address.
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
null result when there is no receipt. Human output presents the same effects
and quotes untrusted names so that they cannot control the terminal.

| Operation | JSON result |
| --- | --- |
| `stat` | Core entry with its structured address. |
| `ls` | `items` of the same entry type and a `state` of `complete`, `limited` or `failed`. |
| `get ADDRESS FILE`, `put`, file `cp`, `mkdir`, `rm`, `rmdir` | `effect`. |
| Recursive `cp` or `rm` | `effect` and per-item `items`. |
| `mv` | `source_effect` and `target_effect`, with per-item `items` for a tree execution and `target_removed_effect` if Core reports removal of the prior target. |
| `mount list` | `items` containing each saved `at`, `driver` and the build-time boolean `driver_available`, plus `connection` when saved. |
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

The [complete configuration example](#configuration-and-secrets) shows a
WebDAV export of a directory below `/nas`.

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

FTPS requires a Mountweave dependency with private data protection.
The released `v0.2.0` build accepts loopback FTP. With that dependency,
a configured FTPS export fails at startup. The protected profile is
verified against the local Mountweave candidate.

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

For an independent build, set `GOWORK=off` and build both the default
binary and the `mweave_pool` variant. To run the existing S3 restart fixture
against a built executable, set `MWEAVE_ACCEPTANCE_BIN` to its absolute path
and run `go test ./cli -run '^TestS3ExportWorkflow$' -count=1`.
The fixture changes the listener after a forced stop, recovers confirmed
parts, and replays Complete after dropping its reply.

The Local export fixture opens one Driver instance for all four adapters and
runs them over real loopback sockets
on Windows and WSL Linux. It covers HTTP cleartext and TLS, authorization of
both operands, root replacement, startup rollback and listener failure. It
also checks SSH authentication, scope, SFTP reads and writes, and native file
release after EOF followed by disconnect or shutdown. The FTP case covers
passive port selection, foreign data peers, upload abort and listener release.
System curl on Windows and WSL Linux verifies FTP and candidate FTPS listing,
download and upload. OpenSSH batch clients
on both platforms verify listing, download, upload with sync, and rename. The Linux CLI binary also exercises SIGTERM with a WebDAV PUT that
finishes during the grace period and with a stalled replacement that is
cancelled and cleaned up.

Each advertised route needs a build-availability indication, minimum
prerequisites and a recoverable failure with a next action. Browser onboarding
depends on the provider approving the app and flow. Persistent rotation also
requires a protected store and evidence of cross-process exclusion,
acknowledgement, restart and uncertain-rotation recovery. Several live
Drivers sharing one rotating grant need a supported ownership contract.
These checks qualify the affected route. Aliases and later protocol exports
need evidence for their own address and resource guarantees.
