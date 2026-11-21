package stock

import (
	"encoding/json"

	localsource "github.com/nostalume/mountweave-cli/cli/source/local"
	davsource "github.com/nostalume/mountweave-cli/cli/source/webdav"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
)

var builtinSources = func() []cliapp.Source {
	webdav := davsource.Source()
	dav, _ := cliapp.ConnectionFor(webdav, "")
	aliWebsite := aliPanWebsiteRoute()
	aliOpen := cliapp.SourceRoute(guideAliPan, prepareAliPan).WithManaged(aliPanAuthorization)
	dav123 := dav.Route.WithReview("123 WebDAV")
	open123 := cliapp.SourceRoute(guideAccountFolder, prepare123).WithManaged(pan123Authorization).WithReview("123 Open")
	return []cliapp.Source{
		localsource.Source(),
		webdav,
		cliapp.ServiceSource("alipan", "Aliyun Drive (阿里云盘)",
			cliapp.SourceConnection{Method: "website", Route: &aliWebsite, Label: "Website sign-in (browse and download)"},
			cliapp.SourceConnection{Method: "open", Route: &aliOpen, Label: "Scan with an approved app ID (access by scope)"}),
		cliapp.ServiceSource("pan123", "123 Cloud Drive (123云盘)",
			cliapp.SourceConnection{Method: "webdav", Route: &dav123, Label: "WebDAV with an app password", ConfirmSetup: true,
				SetupURL:  "https://yun.123pan.cn/",
				SetupHint: "In 123 Cloud Drive, open Tools > Third-party mounting and create an app password for your folder.",
				Defaults:  json.RawMessage(`{"root_url":"https://webdav.123pan.cn/webdav/"}`)},
			cliapp.SourceConnection{Method: "open", Route: &open123, Label: "Open API"}),
		cliapp.ServiceSource("febbox", "FEB (FebBox)", cliapp.SourceConnection{Route: dav.Route,
			SetupURL:  "https://www.febbox.com/console",
			SetupHint: "In the FEB console, open Settings > WebDAV and create a WebDAV account for your folder."}),
		cliapp.ServiceSource("nutstore", "Nutstore (坚果云)", cliapp.SourceConnection{Route: dav.Route,
			Defaults: json.RawMessage(`{"root_url":"https://dav.jianguoyun.com/dav/"}`)}),
		cliapp.SingleSource(cliapp.MobilePanSource, "China Mobile Cloud (移动云盘)", mobilePanWebsiteRoute()),
		cliapp.SingleSource(cliapp.UcSource, "UC Cloud Drive (UC网盘)", ucWebsiteRoute()),
		cliapp.SingleSource(cliapp.QuarkSource, "Quark Cloud Drive (夸克网盘)", quarkWebsiteRoute()),
		cliapp.SingleSource(cliapp.LanZouSource, "LanZou Cloud (蓝奏云)", lanZouWebsiteRoute()),
		cliapp.SingleSource(cliapp.PoolSourceID, "Private storage pool", poolRoute()),
		cliapp.SingleSource("atomgit", "AtomGit repository tree", cliapp.SourceRoute(guideRepositoryTree, prepareAtomGit)),
		cliapp.SingleSource("azureblob", "Azure Blob container", cliapp.SourceRoute(guideAzure, prepareAzure)),
		cliapp.SingleSource("baidupan", "Baidu Netdisk", cliapp.SourceRoute(guideBaidu, prepareBaidu)),
		cliapp.SingleSource("box", "Box folder", cliapp.SourceRoute(guideBox, prepareBox)),
		cliapp.SingleSource("bunny", "Bunny Storage zone", cliapp.SourceRoute(guideBunny, prepareBunny)),
		cliapp.SingleSource("cnbreleases", "CNB release assets", cliapp.SourceRoute(guideCNBRelease, prepareCNBRelease)),
		cliapp.SingleSource("codeartsrepo", "CodeArts Repo commit", cliapp.SourceRoute(guideCodeArtsRepo, prepareCodeArtsRepo)),
		cliapp.SingleSource("cloudreve", "Cloudreve V4", cliapp.SourceRoute(guideCloudreve, prepareCloudreve)),
		cliapp.SingleSource("dingtalkdrive", "DingTalk Drive folder", cliapp.SourceRoute(guideDingTalkDrive, prepareDingTalkDrive)),
		cliapp.SingleSource("dropbox", "Dropbox", cliapp.SourceRoute(guideDropbox, prepareDropbox).WithManaged(dropboxAuthorization.ManagedAuthorization)),
		cliapp.SingleSource("fangcloud", "Fangcloud personal folder", cliapp.SourceRoute(guideAccountFolder, prepareFangcloud)),
		cliapp.SingleSource("feishudrive", "Feishu Drive folder", cliapp.SourceRoute(guideFeishu, prepareFeishu)),
		cliapp.SingleSource("ftp", "FTP or FTPS directory", cliapp.SourceRoute(guideFTP, prepareFTP)),
		cliapp.SingleSource("googledrive", "Google Drive", cliapp.SourceRoute(guideGoogleDrive, prepareGoogleDrive).WithManaged(googleDriveAuthorization.ManagedAuthorization)),
		cliapp.SingleSource("githubrepo", "GitHub repository tree", cliapp.SourceRoute(guideRepositoryTree, prepareGitHubRepo)),
		cliapp.SingleSource("githubreleases", "GitHub release assets", cliapp.SourceRoute(guideGitHubRelease, prepareGitHubRelease)),
		cliapp.SingleSource("gokuai", "Gokuai enterprise library", cliapp.SourceRoute(guideGokuai, prepareGokuai)),
		cliapp.SingleSource("graphdrive", "Microsoft Graph drive", cliapp.SourceRoute(guideGraphDrive, prepareGraphDrive).WithManaged(graphAuthorization)),
		cliapp.SingleSource("guangyapan", "Guangya Pan Open folder", cliapp.SourceRoute(guideGuangya, prepareGuangya)),
		cliapp.SingleSource("ipfs", "IPFS directory via Kubo", cliapp.SourceRoute(guideIPFS, prepareIPFS)),
		cliapp.SingleSource("kdocs", "KDocs personal folder", cliapp.SourceRoute(guideKDocs, prepareKDocs)),
		cliapp.SingleSource("pan115", "115 Open cloud drive", cliapp.SourceRoute(guideAccountFolder, prepare115).WithManaged(pan115Authorization)),
		cliapp.SingleSource("s3", "S3-compatible object storage", cliapp.SourceRoute(guideS3, prepareS3)),
		cliapp.SingleSource("seafile", "Seafile library", cliapp.SourceRoute(guideSeafile, prepareSeafile)),
		cliapp.SingleSource("sftp", "SFTP directory", cliapp.SourceRoute(guideSFTP, prepareSFTP)),
		cliapp.SingleSource("smb", "SMB share", cliapp.SourceRoute(guideSMB, prepareSMB)),
		cliapp.SingleSource("tencentdocs", "Tencent Docs folder", cliapp.SourceRoute(guideTencentDocs, prepareTencentDocs)),
		cliapp.SingleSource("tencentsmh", "Tencent Cloud SMH space", cliapp.SourceRoute(guideTencentSMH, prepareTencentSMH)),
		cliapp.SingleSource("upyun", "UPYUN USS bucket", cliapp.SourceRoute(guideUPYUN, prepareUPYUN)),
		cliapp.SingleSource("wecomdrive", "WeCom Wedrive folder", cliapp.SourceRoute(guideWeComDrive, prepareWeComDrive)),
		cliapp.SingleSource("weiyun", "Tencent Weiyun (腾讯微云)", cliapp.SourceRoute(guideWeiyun, prepareWeiyun).WithManaged(weiyunAuthorization)),
		cliapp.SingleSource("yunpan360", "360 AI Cloud Disk personal root", cliapp.SourceRoute(nil, prepareYunpan360)),
	}
}()

// New constructs a CLI with the built-in sources and the supplied extensions.
func New(sources ...cliapp.Source) (*cliapp.App, error) {
	return cliapp.NewApp(projectRotationProblem, append(builtinSources, sources...)...)
}
