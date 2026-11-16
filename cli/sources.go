package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/drivers/local"
	"github.com/nostalume/mountweave/drivers/webdav"
)

type askField = Prompt

type route struct {
	label       string
	prepare     func(json.RawMessage, string, askField) (mountSource, json.RawMessage, error)
	interactive bool
	managed     managedAuthorization
	setup       func(context.Context, configSnapshot, mountRecord, string, askField, io.Writer) (any, string, error)
}

type credentialAudience struct {
	origin, account string
}

type profileMode uint8

const (
	sdkDefaultProfile profileMode = iota + 1
	sdkNamedProfile
)

type mountSource struct {
	probeOnChange      bool
	build              func(context.Context, *App, configSnapshot, mountRecord, mountRecord) (core.Driver, error)
	audience           credentialAudience
	mode               profileMode
	optionalCredential bool
	keyFile            string
	secret             string // Wizard label for a provider-specific credential.
	open               func(context.Context, string) (core.Driver, error)
	openManaged        func(context.Context, string) (core.Driver, error)
	verify             func(context.Context, core.Driver) error
}

func sourceRoute[T any](label string, prompt func(*T, askField) error, prepare func(T, string) (mountSource, error)) route {
	return route{
		label:       label,
		interactive: true,
		prepare: func(raw json.RawMessage, base string, ask askField) (mountSource, json.RawMessage, error) {
			var value T
			if len(raw) != 0 {
				if err := decodeObject(raw, &value); err != nil {
					return mountSource{}, nil, err
				}
			}
			if ask != nil && prompt != nil {
				if len(raw) != 0 {
					if _, err := prepare(value, base); err != nil {
						return mountSource{}, nil, err
					}
				}
				if err := prompt(&value, ask); err != nil {
					return mountSource{}, nil, err
				}
			}
			src, err := prepare(value, base)
			if err != nil || ask == nil {
				return src, nil, err
			}
			data, err := json.Marshal(value)
			return src, data, err
		},
	}
}

func (r route) withManaged(p managedAuthorization) route {
	r.managed = p
	return r
}

var builtinRoutes = map[string]route{
	"alipan":         sourceRoute("Aliyun Drive Open", guideAliPan, prepareAliPan).withManaged(aliPanAuthorization),
	"atomgit":        sourceRoute("AtomGit repository tree", guideRepositoryTree, prepareAtomGit),
	"azureblob":      sourceRoute("Azure Blob container", guideAzure, prepareAzure),
	"baidupan":       sourceRoute("Baidu Netdisk", guideBaidu, prepareBaidu),
	"box":            sourceRoute("Box folder", guideBox, prepareBox),
	"bunny":          sourceRoute("Bunny Storage zone", guideBunny, prepareBunny),
	"cnbreleases":    sourceRoute("CNB release assets", guideCNBRelease, prepareCNBRelease),
	"codeartsrepo":   sourceRoute("CodeArts Repo commit", guideCodeArtsRepo, prepareCodeArtsRepo),
	"cloudreve":      sourceRoute("Cloudreve V4", guideCloudreve, prepareCloudreve),
	"dingtalkdrive":  sourceRoute("DingTalk Drive folder", guideDingTalkDrive, prepareDingTalkDrive),
	"dropbox":        sourceRoute("Dropbox", guideDropbox, prepareDropbox).withManaged(dropboxAuthorization.setup()),
	"fangcloud":      sourceRoute("Fangcloud personal folder", guideAccountFolder, prepareFangcloud),
	"feishudrive":    sourceRoute("Feishu Drive folder", guideFeishu, prepareFeishu),
	"ftp":            sourceRoute("FTP or FTPS directory", guideFTP, prepareFTP),
	"googledrive":    sourceRoute("Google Drive", guideGoogleDrive, prepareGoogleDrive).withManaged(googleDriveAuthorization.setup()),
	"githubrepo":     sourceRoute("GitHub repository tree", guideRepositoryTree, prepareGitHubRepo),
	"githubreleases": sourceRoute("GitHub release assets", guideGitHubRelease, prepareGitHubRelease),
	"gokuai":         sourceRoute("Gokuai enterprise library", guideGokuai, prepareGokuai),
	"graphdrive":     sourceRoute("Microsoft Graph drive", guideGraphDrive, prepareGraphDrive).withManaged(graphAuthorization),
	"guangyapan":     sourceRoute("Guangya Pan Open folder", guideGuangya, prepareGuangya),
	"ipfs":           sourceRoute("IPFS directory via Kubo", guideIPFS, prepareIPFS),
	"kdocs":          sourceRoute("KDocs personal folder", guideKDocs, prepareKDocs),
	lanZouSource:     lanZouWebsiteRoute(),
	"local":          sourceRoute("Local directory", guideLocal, prepareLocal),
	"pan115":         sourceRoute("115 Open cloud drive", guideAccountFolder, prepare115).withManaged(pan115Authorization),
	"pan123":         sourceRoute("123 Open cloud drive", guideAccountFolder, prepare123).withManaged(pan123Authorization),
	"s3":             sourceRoute("S3-compatible object storage", guideS3, prepareS3),
	"seafile":        sourceRoute("Seafile library", guideSeafile, prepareSeafile),
	"sftp":           sourceRoute("SFTP directory", guideSFTP, prepareSFTP),
	"smb":            sourceRoute("SMB share", guideSMB, prepareSMB),
	"tencentdocs":    sourceRoute("Tencent Docs folder", guideTencentDocs, prepareTencentDocs),
	"tencentsmh":     sourceRoute("Tencent Cloud SMH space", guideTencentSMH, prepareTencentSMH),
	"upyun":          sourceRoute("UPYUN USS bucket", guideUPYUN, prepareUPYUN),
	"wecomdrive":     sourceRoute("WeCom Wedrive folder", guideWeComDrive, prepareWeComDrive),
	"webdav":         sourceRoute("WebDAV collection", guideWebDAV, prepareWebDAV),
	"weiyun":         sourceRoute("Tencent Weiyun (腾讯微云)", guideWeiyun, prepareWeiyun).withManaged(weiyunAuthorization),
	"yunpan360":      sourceRoute("360 AI Cloud Disk personal root", nil, prepareYunpan360),
}

type sourceConnection struct {
	method, route, label, setupURL, setupHint string
	defaults                                  json.RawMessage
}

type sourceChoice struct {
	id, label   string
	connections []sourceConnection
}

var serviceChoices = map[string]sourceChoice{
	"alipan": {
		label: "Aliyun Drive (阿里云盘)",
		connections: []sourceConnection{
			{method: "website", route: aliPanWebsiteSource, label: "Website sign-in (browse and download)"},
			{method: "open", route: "alipan", label: "Scan with an approved app ID (access by scope)"},
		},
	},
	"pan123": {
		label: "123 Cloud Drive (123云盘)",
		connections: []sourceConnection{
			{method: "webdav", route: "webdav", label: "WebDAV with an app password",
				setupURL:  "https://yun.123pan.cn/",
				setupHint: "In 123 Cloud Drive, open Tools > Third-party mounting and create an app password for your folder.",
				defaults:  json.RawMessage(`{"root_url":"https://webdav.123pan.cn/webdav/"}`)},
			{method: "open", route: "pan123", label: "Open platform with approved app credentials"},
		},
	},
	"febbox": {
		label: "FEB (FebBox)",
		connections: []sourceConnection{{route: "webdav",
			setupURL:  "https://www.febbox.com/console",
			setupHint: "In the FEB console, open Settings > WebDAV and create a WebDAV account for your folder."}},
	},
	"nutstore": {
		label: "Nutstore (坚果云)",
		connections: []sourceConnection{{route: "webdav",
			defaults: json.RawMessage(`{"root_url":"https://dav.jianguoyun.com/dav/"}`)}},
	},
}

// App owns the source catalog shared by the setup wizard and runtime.
type App struct {
	routes  map[string]route
	choices map[string]sourceChoice
}

// New constructs a CLI with the built-in sources and the supplied extensions.
func New(sources ...Source) (*App, error) {
	app := &App{routes: maps.Clone(builtinRoutes), choices: maps.Clone(serviceChoices)}
	maps.Copy(app.routes, websiteCandidateRoutes)
	if !websiteCandidateAvailable {
		alipan := app.choices["alipan"]
		alipan.connections = slices.DeleteFunc(slices.Clone(alipan.connections), func(c sourceConnection) bool {
			return c.route == aliPanWebsiteSource
		})
		app.choices["alipan"] = alipan
	}
	if r := poolRoute(); r.prepare != nil {
		app.routes[poolSourceID] = r
	}
	for _, source := range sources {
		if source.route.prepare == nil {
			return nil, errors.New("invalid source definition")
		}
		if _, ok := app.routes[source.id]; ok || source.id == poolSourceID || source.id == aliPanWebsiteSource || source.id == mobilePanSource {
			return nil, fmt.Errorf("duplicate source ID %q", source.id)
		}
		if _, ok := app.choices[source.id]; ok {
			return nil, fmt.Errorf("source ID %q conflicts with a built-in service", source.id)
		}
		app.routes[source.id] = source.route
	}
	for name, r := range app.routes {
		if _, listed := app.choices[name]; !listed && name != aliPanWebsiteSource {
			app.choices[name] = sourceChoice{label: r.label}
		}
	}
	for name, choice := range app.choices {
		choice.id = name
		app.choices[name] = choice
	}
	return app, nil
}

func (app *App) routeID(m mountRecord) string {
	if m.Connection == "" {
		if _, ok := app.routes[m.Driver]; ok {
			return m.Driver
		}
	}
	if choice, ok := app.choices[m.Driver]; ok {
		for _, connection := range choice.connections {
			if connection.method == m.Connection {
				return connection.route
			}
		}
	}
	return ""
}

func (app *App) mountRoute(m mountRecord) (route, error) {
	if r, ok := app.routes[app.routeID(m)]; ok {
		return r, nil
	}
	if !websiteCandidateAvailable && (m.aliPanWebsite() || m.Driver == mobilePanSource) {
		return route{}, unavailableSource()
	}
	if _, known := app.choices[m.Driver]; known || m.Driver == aliPanWebsiteSource {
		return route{}, invalidConfig("This source does not offer the specified connection method.")
	}
	return route{}, unavailableSource()
}

func (app *App) prepareMountSource(m mountRecord, base string) (mountSource, error) {
	r, err := app.mountRoute(m)
	if err != nil {
		return mountSource{}, err
	}
	src, _, err := r.prepare(m.Options, base, nil)
	return src, err
}

func unavailableSource() error {
	return &problem{Code: string(core.MountUnavailable), Message: "The selected Driver is unavailable in this build.", NextAction: "See mweave mount add --help for the available sources."}
}

func (s mountSource) checkReference(ref *credentialRef) error {
	if ref != nil && ref.Kind == managedCredential || s.openManaged != nil && s.open == nil {
		if s.openManaged != nil && ref != nil && ref.Kind == managedCredential && validGrantID(ref.Ref) {
			return nil
		}
		return invalidConfig("This source requires an existing managed authorization reference.")
	}
	if s.mode != 0 {
		if (s.mode == sdkDefaultProfile && ref == nil) || (ref != nil && ref.Kind == "profile") {
			return nil
		}
		return invalidConfig("Select an SDK profile for this S3 mount.")
	}
	if s.audience.origin == "" {
		if ref != nil {
			return invalidConfig("This source does not accept a credential for the selected settings.")
		}
		return nil
	}
	if ref == nil && (s.keyFile != "" || s.optionalCredential) {
		return nil
	}
	if ref == nil || (ref.Kind != "env" && ref.Kind != "file") {
		return invalidConfig("This source requires an env or file credential reference.")
	}
	return nil
}

func (s mountSource) openDriver(ctx context.Context, ref *credentialRef, base string) (core.Driver, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	open := s.open
	var input string
	if ref != nil && ref.Kind == managedCredential || s.openManaged != nil && s.open == nil {
		if err := s.checkReference(ref); err != nil {
			return nil, err
		}
		open, input = s.openManaged, ref.Ref
	} else if ref != nil && ref.Kind == "profile" {
		input = ref.Ref
	} else if ref != nil {
		var err error
		input, err = loadCredential(ref, base)
		if err != nil {
			return nil, err
		}
	}
	d, err := open(ctx, input)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		if d != nil {
			err = errors.Join(err, d.Close())
		}
		return nil, err
	}
	if d == nil {
		return nil, invalidConfig("The source constructor did not return a Driver.")
	}
	return d, nil
}

// Keep a failed, non-nil native Driver available for openDriver to close.
func nativeDriver[T any, D interface {
	*T
	core.Driver
}](d D, err error) (core.Driver, error) {
	if d == nil {
		return nil, err
	}
	return d, err
}

func readCredentialFile(name string) ([]byte, error) {
	info, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fs.ErrInvalid
	}
	return os.ReadFile(name)
}

func loadCredential(ref *credentialRef, base string) (string, error) {
	failure := &problem{Code: "credential_unavailable", Message: "The credential reference could not be read.", NextAction: "Check the environment variable or protected credential file."}
	var secret string
	if ref.Kind == "env" {
		secret = os.Getenv(ref.Ref)
	} else {
		body, err := readCredentialFile(hostPath(base, ref.Ref))
		if err != nil {
			return "", failure
		}
		secret = strings.TrimRight(string(body), "\r\n")
	}
	if secret == "" || strings.ContainsAny(secret, "\x00\r\n") {
		return "", failure
	}
	return secret, nil
}

func sameReference(a, b *credentialRef, base string) bool {
	if a == nil || b == nil || a.Kind != b.Kind {
		return false
	}
	left, right := a.Ref, b.Ref
	if a.Kind == "file" {
		left, right = hostPath(base, left), hostPath(base, right)
	}
	if left == right || (a.Kind != "profile" && runtime.GOOS == "windows" && strings.EqualFold(left, right)) {
		return true
	}
	if a.Kind != "file" {
		return false
	}
	// Compare aliases without reading either file.
	first, err := os.Stat(left)
	if err != nil {
		return false
	}
	second, err := os.Stat(right)
	return err == nil && os.SameFile(first, second)
}

// Check shared references before reading a secret or sending a probe. Edits
// include the old record so a destination change cannot reuse its reference.
func (app *App) checkAudience(m mountRecord, src mountSource, saved configSnapshot, includeOld bool, previous ...mountRecord) error {
	if err := src.checkReference(m.Credential); err != nil {
		return err
	}
	if m.Credential == nil {
		return nil
	}
	base := filepath.Dir(saved.path)
	for _, e := range saved.value.Exports {
		if sameReference(m.Credential, e.Auth.Credential, base) {
			return exportCredentialConflict()
		}
	}
	check := func(prior mountRecord) error {
		if (!includeOld && prior.At == m.At) || !sameReference(m.Credential, prior.Credential, base) {
			return nil
		}
		old, err := app.prepareMountSource(prior, base)
		if err != nil || old.audience != src.audience {
			return &problem{Code: "credential_scope_conflict", Message: "This credential reference is already associated with a different or unknown destination.", NextAction: "Provide a separate credential reference for the new destination."}
		}
		return nil
	}
	for _, savedMount := range saved.value.Mounts {
		for _, prior := range sourceRecords(savedMount) {
			if err := check(prior); err != nil {
				return err
			}
		}
	}
	for _, prior := range previous {
		if err := check(prior); err != nil {
			return err
		}
	}
	return nil
}

func validResponseBudget(n int64) bool {
	return n >= 0 && n < math.MaxInt64
}

func hostPath(base, name string) string {
	if filepath.IsAbs(name) {
		return filepath.Clean(name)
	}
	return filepath.Join(base, name)
}

type localOptions struct {
	Root                 string `json:"root"`
	StagingDirectory     string `json:"staging_directory,omitempty"`
	MaxStagingBytes      int64  `json:"max_staging_bytes,omitempty"`
	MaxConcurrentCommits int    `json:"max_concurrent_commits,omitempty"`
}

func guideLocal(opts *localOptions, ask askField) error {
	root := opts.Root
	if root == "" {
		root = "."
	}
	value, err := ask("Local directory", root)
	if err != nil {
		return err
	}
	if opts.Root != "" && value == opts.Root {
		return nil
	}
	opts.Root, err = filepath.Abs(value)
	return err
}

func prepareLocal(opts localOptions, base string) (mountSource, error) {
	if opts.Root == "" {
		return mountSource{}, invalidConfig("Local requires options.root.")
	}
	if opts.MaxConcurrentCommits < 0 || opts.MaxStagingBytes < 0 ||
		(opts.StagingDirectory == "") != (opts.MaxStagingBytes == 0) {
		return mountSource{}, invalidConfig("Local staging requires a directory and a positive byte budget.")
	}
	root := hostPath(base, opts.Root)
	native := local.Options{MaxStagingBytes: opts.MaxStagingBytes, MaxConcurrentCommits: opts.MaxConcurrentCommits}
	if opts.StagingDirectory != "" {
		native.StagingDirectory = hostPath(base, opts.StagingDirectory)
	}
	return mountSource{open: func(context.Context, string) (core.Driver, error) {
		return nativeDriver(local.New(root, native))
	}}, nil
}

type webDAVOptions struct {
	RootURL          string `json:"root_url"`
	Username         string `json:"username,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideWebDAV(opts *webDAVOptions, ask askField) error {
	var err error
	opts.RootURL, err = ask("WebDAV URL", opts.RootURL)
	if err != nil {
		return err
	}
	opts.Username, err = ask("WebDAV username (empty for anonymous; - clears)", opts.Username)
	if opts.Username == "-" {
		opts.Username = ""
	}
	return err
}

func prepareWebDAV(opts webDAVOptions, _ string) (mountSource, error) {
	native := webdav.Options{RootURL: opts.RootURL, Username: opts.Username, MaxResponseBytes: opts.MaxResponseBytes}
	if err := native.Validate(); err != nil {
		return mountSource{}, invalidConfig("WebDAV requires a valid collection URL and settings.")
	}
	var audience credentialAudience
	if opts.Username != "" {
		u, _ := url.Parse(opts.RootURL)
		audience = credentialAudience{origin: storageOrigin("webdav", u), account: opts.Username}
	}
	return mountSource{audience: audience, secret: "WebDAV password", open: func(ctx context.Context, password string) (core.Driver, error) {
		options := native
		options.Password = password
		return nativeDriver(webdav.Open(ctx, options))
	}}, nil
}

func (app *App) openMountDriver(ctx context.Context, src mountSource, saved configSnapshot, m, prior mountRecord) (core.Driver, error) {
	if src.build == nil {
		return src.openDriver(ctx, m.Credential, filepath.Dir(saved.path))
	}
	d, err := src.build(ctx, app, saved, m, prior)
	if err == nil && ctx.Err() != nil {
		return nil, errors.Join(ctx.Err(), d.Close())
	}
	return d, err
}

func (app *App) probeMount(ctx context.Context, src mountSource, saved configSnapshot, m, prior mountRecord) error {
	d, err := app.openMountDriver(ctx, src, saved, m, prior)
	if err != nil {
		return err
	}
	if src.verify != nil {
		err = src.verify(ctx, d)
	}
	return errors.Join(err, d.Close())
}

// Object constructors are local. Probe the prefix only when confirming a mount
// or new credential.
func verifyKeyRoot(ctx context.Context, d core.Driver) error {
	it, err := d.Enumerate(ctx, core.NativeKeys("", ""))
	if err != nil {
		return err
	}
	_, err = it.Next(ctx, 1)
	if err == io.EOF {
		err = nil
	}
	return errors.Join(err, it.Close())
}

func (app *App) sourceNames(query string) []string {
	var names []string
	for name, choice := range app.choices {
		if strings.Contains(name, query) || strings.Contains(strings.ToLower(choice.label), query) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func (app *App) printSources(w io.Writer, names []string) error {
	for _, name := range names {
		if _, err := fmt.Fprintf(w, "  %-16s %s\n", name, app.choices[name].label); err != nil {
			return err
		}
	}
	return nil
}
