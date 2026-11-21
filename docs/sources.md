# Storage sources

The CLI source name selects a configuration and authorization route. Choose
it in `mweave mount add PATH` or use its `driver` value in a saved record.
`mweave mount add --help` lists sources available in the installed binary.
For the full record format and common rules, see the
[configuration reference](configuration.md).

For first-time sign-in, skip to [cloud authorization](#cloud-authorization).
Managed authorization and saved website sessions require Windows or Linux.
For file behavior, this page describes the CLI route; the
[Mountweave capability matrix](https://github.com/nostalume/mountweave/blob/v0.3.0/docs/drivers.md)
covers the native Driver. Successful authorization does not imply write or
delete access. The configured root, provider permissions and Driver capability
all apply. Pool is an [optional build](#pool-mounts).

## Cloud authorization

Choose a route based on what you already have.

| What you have | Route and what to prepare |
| --- | --- |
| An ordinary account | Use website sign-in for [Aliyun Drive](#connect-aliyun-drive-through-the-website), [LanZou, Mobile Cloud, UC or Quark](#browser-based-file-access), or account consent for [Weiyun](#connect-tencent-weiyun). The wizard keeps the resulting authorization. |
| Server or app credentials | Use [WebDAV presets](#webdav-presets) for Nutstore, 123 WebDAV or FEB. For WebDAV, SFTP, FTP and SMB, get the address and sign-in details from the service or server administrator. |
| An approved or configured application | Get application settings before setup for [Aliyun Drive Open](#connect-aliyun-drive-open), [115 Open](#connect-115-open), [123 Open](#connect-123-open), [Dropbox](#connect-dropbox), [Google Drive](#connect-google-drive) or [Microsoft Graph](#connect-microsoft-graph). The provider may require an eligible account or administrator consent. |
| A provider token or API key | Use `reference` where offered. Obtain a credential for the selected account or space with the permissions you need. See [drive and team integrations](#drive-and-team-integrations) for service-specific inputs. |
| An S3 bucket and credentials | Use [S3-compatible storage](#s3-compatible-storage) with credentials issued for that bucket, normally through the AWS SDK credential chain or a named profile. |

Run `mweave mount add /cloud`, choose the service and complete its sign-in or
provide the requested credential reference. For website sign-in, finish in
the browser window, then close it and confirm in the terminal when prompted.
Choose the authorized location and save. Use `ls /cloud` for a mounted folder
or `ls key:/cloud:` for an object mount to check access.

### Find setup values

| Setting | Where to find it |
| --- | --- |
| Mount path | Choose an address in mweave, such as `/cloud`. |
| Server or collection address | Use the wizard's preset or the address from the service or administrator. A file share link cannot substitute for a WebDAV collection URL. |
| Application ID or client settings | Use the provider's registered application or ask the organization that owns it. A client ID and a client secret are separate values. |
| Password, access token or profile | Sign in when supported, or supply an [existing reference](configuration.md#configuration-and-secrets) from the service or administrator. |
| Drive, collection, bucket or folder | Choose the space or path, or use the native ID requested by the source. `/` is the root of the selected space. |

When a folder prompt accepts both a path and an ID, start a path with `/`.
Enter `-` at that prompt to restore its default during editing.

## Provider-specific access

### Servers and hosted storage

The default ports are 22 for SFTP, 21 for FTP and explicit FTPS, 990 for
implicit FTPS, and 445 for SMB. For Azure Blob, issue a container SAS with
list permission in Azure and give mweave the SAS token, with or without
its leading `?`.

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

For Bunny, use the regional URL and zone password from the storage zone's
settings. For UPYUN, use the bucket address and a storage operator assigned
to the bucket. Its operator account supplies the password credential.

### Drive and team integrations

For Baidu Netdisk, obtain an access token authorized for the Baidu developer
application and account. Choose an absolute directory covered by that grant.
The granted directory can be narrower than the account root.

Set Google Drive's `export_formats` in a mount record to map a Workspace
MIME type to an output MIME type, for example
`{"application/vnd.google-apps.document":"application/pdf"}`. Dropbox downloads
ordinary files with the same account token. To enable export for files the
download endpoint cannot return, set `export_format` in the mount record.
Use an empty string for the provider's default format. Omit the field to
request ordinary downloads only.
[Google Drive export](https://developers.google.com/workspace/drive/api/reference/rest/v3/files/export) ·
[Dropbox namespace setup](https://developers.dropbox.com/en-us/dbx-team-files-guide)

Use the HTTPS installation address for a remote Cloudreve or Seafile
server. Ask its operator for a token with access to the location. For
Seafile, get the unencrypted library's `id` from the authorized library
list. The saved token reference is scoped to the server's installation URL,
including its path.
[Cloudreve authentication](https://docs.cloudreve.org/en/api/auth) ·
[Seafile library list](https://seafile-api.readme.io/reference/get_api2-repos)

Get the Feishu folder token from an authorized Drive listing or the
organization's app administrator. It identifies the folder. An app or user
access token with permission is required to read it.
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

Find a Box folder ID in the folder URL (`/folder/123`), or use `0` for the
account root. Box streams ordinary files. A web link can appear in a listing and returns
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

360 AI Cloud Disk supplies a personal API key for read-only access to the
personal root. The default record uses `options: {}`. 360 describes the key
as a limited noncommercial trial.
[360 API key](https://open.yunpan.360.cn/docs/mcp-server/preparation/) ·
[360 download link tool](https://open.yunpan.360.cn/docs/skills/tools/#get-download-url)

Gokuai, DingTalk, WeCom and SMH support browsing and ordinary file reads.
For Gokuai, choose the library ID and secret from its developer grant.
For DingTalk, get the space ID, folder dentry ID and operator union ID
from the internal app integration or administrator. The app needs
`Storage.File.Read` and `Storage.DownloadInfo.Read`. For WeCom, get the
space and folder IDs from the authorized Wedrive integration or administrator.
A listed online document can have an unavailable body. In Tencent SMH,
the library setup supplies its ID. For a multi-tenant library, get the
space ID from the integration that provisioned the tenant. Use the
business token issued for that library and space. The default API origin
is `https://smh.tencentcs.com`.
[Gokuai library setup](https://developer.gokuai.com/grant/library.html) ·
[DingTalk authorization](https://www.alibabacloud.com/help/zh/polardb/polardb-for-postgresql/sync-dingtalk-documents-to-the-knowledge-base) ·
[Wedrive downloads](https://developer.work.weixin.qq.com/document/path/97881) ·
[SMH library setup](https://cloud.tencent.com/document/product/1339/69098) ·
[SMH business token](https://cloud.tencent.com/document/product/1339/71159)

A Gokuai secret reference binds to the selected library. An SMH token
reference binds to its API origin, library and space. Directory changes
within that scope can keep the reference. Supply a compatible reference
when changing the bound scope.

### Repositories and IPFS

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

## WebDAV presets

Use the WebDAV collection address, username and app password issued by
the provider. [Reference the password](configuration.md#configuration-and-secrets)
and check that the app or account has the desired folder permissions.

| Service | Where to get the values |
| --- | --- |
| Nutstore (`nutstore`) | The wizard offers the Nutstore address. Use your account email and generate a third-party app password in [Nutstore settings](https://help.jianguoyun.com/?p=5677). |
| 123 Cloud Drive (`pan123` → `webdav`) | Choose `existing` if you have the WebDAV values. Choose `setup` to open 123 Cloud Drive and create an app for the folder under Tools → Third-party mounting. Use its username and password. Choose the service-issued URL or accept the default. [123 WebDAV guide](https://www.weibo.com/ttarticle/p/show?id=2309405214103219339373) |
| FEB (`febbox`) | In the FEB console, open Settings → WebDAV and create an account for the folder. FEB supplies the URL, username and password. The wizard prints the setup address if it cannot open a browser. [FEB WebDAV](https://www.febbox.com/faq) |

## Browser-based file access

These sources reuse the protected website session created during setup.
Use `mount edit PATH` if sign-in expires and select the original account
and folder.

### Browse a LanZou Cloud account

`lanzou` browses private folders. Downloads use the file's existing share
through the password-protected LanZou download page.

### Browse a China Mobile Cloud personal drive

`mobilepan` browses the personal drive and downloads original files.

### Browse a UC Cloud Drive account

`uc` reads nonempty files sequentially or with a single byte range.
It uses the site's Download action for the selected file.

### Browse a Quark Cloud Drive account

`quark` reads original files, including a single byte range, through the
saved website session.

## S3-compatible storage

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

## Connect Dropbox

In the [Dropbox App Console](https://www.dropbox.com/developers/apps),
create or use a user-scoped app and enter its public app key in the wizard
or set `MWEAVE_DROPBOX_APP_KEY`. Register the exact redirect
`http://localhost:53682/callback` and enable `account_info.read`,
`files.metadata.read` and `files.content.read`. The local port must be
available during consent. For write access, enable `files.content.write`,
set `allow_create` on the mount with `mount edit --from`, then reauthorize.
[Dropbox OAuth flow](https://docs.dropboxapi.com/riviera/docs/get-started/authorization)

The app's authorized root is the default. For a team namespace, get its
`namespace_id` from the authorized Dropbox API or the team administrator.
Reauthorization keeps the account and app and requests any additional
scope needed by mounts sharing the grant in the selected configuration.
[Dropbox team namespaces](https://developers.dropbox.com/en-us/dbx-team-files-guide)

## Connect Google Drive

In Google Cloud, enable the Drive API and download a Desktop OAuth client
JSON file. Give the wizard the file path and consent to `drive.readonly`.
The wizard uses a local port assigned by the operating system. For a shared
drive, use its ID or a folder ID from the authorized Drive API or the
workspace administrator.
[Desktop app setup](https://developers.google.com/identity/protocols/oauth2/native-app) ·
[Drive scopes](https://developers.google.com/workspace/drive/api/guides/api-specific-auth)

For an external OAuth app in Testing, Google issues refresh tokens for this
scope that expire after seven days. Public apps and managed accounts are
subject to Google's verification and organization policies.
[Google token expiration](https://developers.google.com/identity/protocols/oauth2/#expiration) ·
[App state and organization policy](https://developers.google.com/identity/protocols/oauth2/production-readiness/overview)

## Connect Aliyun Drive Open

Choose `alipan` → `open`. Obtain a renewable-grant-capable App ID from an
approved Aliyun Drive application. Enter it in the wizard or set
`MWEAVE_ALIPAN_APP_ID`, then scan the QR code in the Aliyun Drive app.
The wizard obtains the drive ID after you select a drive. For an existing
Rustic repository on the resource drive, choose `resource` and the original
folder.

Choose read access for browse and recovery. Read/write enables ordinary
exclusive creation. A WebDAV recovery export can be read-only. WebDAV
backup writes require complete publication, which the Open route does not
provide.
[Aliyun Drive developer portal](https://www.alipan.com/developer/) ·
[Official QR authorization](https://github.com/alibaba/aliyunpan-ios-sdk/blob/main/Sources/AliyunpanSDK/AliyunpanCredentials/AliyunpanQRCodeCredentials.swift)

## Connect Aliyun Drive through the website

Choose `alipan` → `website` for ordinary account sign-in and selection of
a resource-drive folder. Keep the site's remember sign-in option enabled
if offered. A plain-text page is the sign-in completion signal. The wizard
sets up browsing and downloads. Supported repository writes use the
[explicit Rustic export settings](specification.md#protocol-setup).
Stop a running `serve` before replacing its website session. If website
verification fails, try the site's QR sign-in if available.

## Connect Tencent Weiyun

Weiyun asks you to approve account-wide access on its website. Select the
mounted folder in mweave after consent. The mount is read-only and scoped
to that folder. To replace this grant choose `switch` in `mount edit`.
The `reference` route uses a personal MCP token issued by Weiyun.
[Weiyun authorization and personal token](https://www.weiyun.com/act/openclaw)

## Connect 115 Open

Get an approved App ID from 115 Open. Enter it on the `scan` route or set
`MWEAVE_115_APP_ID`. Scan the displayed QR code with the 115 app.
[115 Open developer registration](https://open.115.com/) ·
[115 Open scan authorization](https://open.115.com/doc/115开放平台/接入指南/接入授权/手机扫码授权PKCE模式/获取设备码和二维码内容.md?raw=true)

A rotating authorization can have one running owner. Stop it before
reauthorizing. The CLI probes each sharing mount in the selected
configuration before replacing the grant. If refresh cannot be confirmed or
115 rejects the token before its local expiry, reauthorize in `mount edit`.
[115 Open refresh contract](https://open.115.com/doc/115开放平台/接入指南/接入授权/刷新access_token.md?raw=true)

## Connect 123 Open

Choose `pan123` → `open` if you have a Client ID and Client Secret issued
by 123 Open for the account you intend to mount. Supply the ID and a secret
reference. The CLI uses them to issue a short-lived access token and
verifies the account UID. It resolves the secret again when it needs a new
token. For an existing provider token, choose `reference`.

Use `mount edit` to reauthorize the same account. The CLI probes mounts
in the selected configuration sharing that grant. Choose `switch` to
bind only the selected mount to another account.
[123 Open application setup](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/hpengmyg32blkbg8) ·
[Token issuance](https://123yunpan.yuque.com/org-wiki-123yunpan-muaork/cr6ced/gn1nai4x0v0ry9ki)

## Connect Microsoft Graph

Get the Application (client) ID and Directory (tenant) ID from Microsoft
Entra. Use `consumers` for personal Microsoft accounts. Enable public
client flows for the applicable account type. The CLI requests delegated
`Files.Read`, `User.Read` and `offline_access`. On the `device` route,
open the Microsoft verification URL and enter the displayed code.
[App registration](https://learn.microsoft.com/en-us/entra/identity-platform/scenario-desktop-app-registration) ·
[Device authorization](https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-device-code)

Supply the drive ID separately. Get the `id` from an authorized Graph
request such as `GET /me/drives` or `GET /sites/{siteId}/drives`, or ask
the organization's Graph administrator. Then select a folder in that
drive. The managed device route uses global Microsoft Graph. Use an
external token issued for the configured endpoint for other deployments.
[List drives](https://learn.microsoft.com/en-us/graph/api/drive-list?view=graph-rest-1.0) ·
[Graph deployments](https://learn.microsoft.com/en-us/graph/deployments)

## Pool mounts

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
Reopening a saved mount requires an existing database with its Pool layout.
The Driver checks the layout in the database transaction before serving files.
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
[Pool capabilities and maintenance](https://github.com/nostalume/mountweave/tree/drivers/pool/v0.3.0/drivers/pool)

## Source options

Use this reference with [`mount add --from`](specification.md#connect-a-mount).
The installed binary lists available IDs in `mweave mount add --help`.

| `driver` | Record settings and route |
| --- | --- |
| `alipan` | Use the wizard-generated record from [website sign-in](#connect-aliyun-drive-through-the-website) or [official scan authorization](#connect-aliyun-drive-open). |
| `local` | `root` is a host directory. |
| `webdav` | `root_url` selects the collection. Set `username` and a credential reference for Basic authentication, or omit both for anonymous access. |
| `nutstore`, `febbox` | Use the provider's WebDAV username and app password. See [WebDAV presets](#webdav-presets). |
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
| `lanzou`, `mobilepan`, `uc`, `quark` | Use the wizard-generated record from [website sign-in](#browser-based-file-access). |
| `weiyun` | Use [browser authorization](#connect-tencent-weiyun) or reference a personal MCP token. Browser authorization covers the entire account. Mount its root or use `root_path` to mount a read-only subfolder. |
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
