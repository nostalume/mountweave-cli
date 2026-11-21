package stock

import (
	"cmp"
	"context"
	"net/http"
	"strconv"
	"strings"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave/drivers/atomgit"
	"github.com/nostalume/mountweave/drivers/cnbreleases"
	"github.com/nostalume/mountweave/drivers/codeartsrepo"
	"github.com/nostalume/mountweave/drivers/githubreleases"
	"github.com/nostalume/mountweave/drivers/githubrepo"
	"github.com/nostalume/mountweave/drivers/ipfs"
)

type repositoryTreeOptions struct {
	Repository       string `json:"repository"`
	Ref              string `json:"ref"`
	RootPath         string `json:"root_path,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideRepositoryTree(o *repositoryTreeOptions, ask cliapp.Prompt) error {
	return cliapp.AskFields(ask, cliapp.Field("Repository (owner/name)", &o.Repository),
		cliapp.Field("Commit, branch or tag", &o.Ref), cliapp.Field("Folder path (/ for repository root)", &o.RootPath))
}

func splitRepository(repository string) (owner, repo string, valid bool) {
	owner, repo, _ = strings.Cut(repository, "/")
	return owner, repo, core.ValidName(owner) && core.ValidName(repo)
}

func validRepositoryRef(ref string) bool {
	return ref != "" && ref != "." && ref != ".." && !strings.ContainsFunc(ref, func(r rune) bool { return r < ' ' || r == 127 })
}

func prepareGitHubRepo(o repositoryTreeOptions, _ string) (cliapp.MountSource, error) {
	return prepareRepositoryTree(o, "api.github.com", func(ctx context.Context, options githubrepo.Options) (core.Driver, error) {
		return cliapp.NativeDriver(githubrepo.Open(ctx, options))
	})
}

func prepareAtomGit(o repositoryTreeOptions, _ string) (cliapp.MountSource, error) {
	return prepareRepositoryTree(o, "api.atomgit.com", func(ctx context.Context, options githubrepo.Options) (core.Driver, error) {
		return cliapp.NativeDriver(atomgit.Open(ctx, atomgit.Options(options)))
	})
}

func prepareRepositoryTree(o repositoryTreeOptions, host string, open func(context.Context, githubrepo.Options) (core.Driver, error)) (cliapp.MountSource, error) {
	owner, repo, valid := splitRepository(o.Repository)
	if !valid || !validRepositoryRef(o.Ref) || !validFolderSettings(o.RootPath, o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("Use an owner/name repository, a ref and valid folder and response settings.")
	}
	src := cliapp.BearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return open(ctx, githubrepo.Options{
			Owner: owner, Repository: repo, Ref: o.Ref, RootPath: o.RootPath, Client: api, MaxResponseBytes: o.MaxResponseBytes,
		})
	}, host)
	src.OptionalCredential = true
	return src, nil
}

type releaseOptions struct {
	Repository       string `json:"repository"`
	Tag              string `json:"tag"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideGitHubRelease(o *releaseOptions, ask cliapp.Prompt) error {
	return cliapp.AskFields(ask, cliapp.Field("Repository (owner/name)", &o.Repository), cliapp.Field("Release tag", &o.Tag))
}

func guideCNBRelease(o *releaseOptions, ask cliapp.Prompt) error {
	return cliapp.AskFields(ask, cliapp.Field("Repository (organization/name or organization/team/name)", &o.Repository), cliapp.Field("Release tag", &o.Tag))
}

func prepareGitHubRelease(o releaseOptions, _ string) (cliapp.MountSource, error) {
	owner, repo, valid := splitRepository(o.Repository)
	if !valid || !validRepositoryRef(o.Tag) || !cliapp.ValidResponseBudget(o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("GitHub Releases needs an owner/name repository, a tag and valid response settings.")
	}
	src := cliapp.BearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return cliapp.NativeDriver(githubreleases.Open(ctx, githubreleases.Options{
			Owner: owner, Repository: repo, Tag: o.Tag, Client: api, MaxResponseBytes: o.MaxResponseBytes,
		}))
	}, "api.github.com")
	src.OptionalCredential = true
	return src, nil
}

func prepareCNBRelease(o releaseOptions, _ string) (cliapp.MountSource, error) {
	if !strings.Contains(o.Repository, "/") || !validRepositoryRef(o.Tag) || !cliapp.ValidResponseBudget(o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("CNB Releases needs a repository path, a tag and valid response settings.")
	}
	for part := range strings.SplitSeq(o.Repository, "/") {
		if !core.ValidName(part) || part == "-" {
			return cliapp.MountSource{}, cliapp.InvalidConfig("CNB Releases needs a valid repository path.")
		}
	}
	return cliapp.BearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return cliapp.NativeDriver(cnbreleases.Open(ctx, cnbreleases.Options{
			Repository: o.Repository, Tag: o.Tag, Client: api, MaxResponseBytes: o.MaxResponseBytes,
		}))
	}, "api.cnb.cool"), nil
}

type codeArtsRepoOptions struct {
	Endpoint         string `json:"endpoint"`
	RepositoryID     int32  `json:"repository_id"`
	CommitSHA        string `json:"commit_sha"`
	RootPath         string `json:"root_path,omitempty"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideCodeArtsRepo(o *codeArtsRepoOptions, ask cliapp.Prompt) error {
	id := ""
	if o.RepositoryID != 0 {
		id = strconv.FormatInt(int64(o.RepositoryID), 10)
	}
	if err := cliapp.AskFields(ask, cliapp.Field("Regional HTTPS API origin", &o.Endpoint), cliapp.Field("Repository ID", &id),
		cliapp.Field("Commit SHA", &o.CommitSHA), cliapp.Field("Folder path (/ for repository root)", &o.RootPath)); err != nil {
		return err
	}
	value, err := strconv.ParseInt(id, 10, 32)
	if err != nil || value < 1 {
		return cliapp.InvalidConfig("CodeArts Repo needs a positive numeric repository ID.")
	}
	o.RepositoryID = int32(value)
	return nil
}

func prepareCodeArtsRepo(o codeArtsRepoOptions, _ string) (cliapp.MountSource, error) {
	origin, err := sourceURL(o.Endpoint)
	if err != nil {
		return cliapp.MountSource{}, err
	}
	validSHA := len(o.CommitSHA) == 40 && !strings.ContainsFunc(o.CommitSHA, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F')
	})
	if origin.Scheme != "https" || (origin.Path != "" && origin.Path != "/") || o.RepositoryID < 1 ||
		!validSHA || !validFolderSettings(o.RootPath, o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("CodeArts Repo needs an HTTPS API origin, a numeric repository ID, a full commit SHA and valid folder and response settings.")
	}
	src := cliapp.HeaderTokenSource("https", "X-Auth-Token", "", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return cliapp.NativeDriver(codeartsrepo.Open(ctx, codeartsrepo.Options{
			Endpoint: o.Endpoint, RepositoryID: o.RepositoryID, CommitSHA: o.CommitSHA, RootPath: o.RootPath,
			Client: api, MaxResponseBytes: o.MaxResponseBytes,
		}))
	}, origin.Host)
	src.Secret = "Huawei Cloud IAM token"
	return src, nil
}

type ipfsOptions struct {
	Endpoint         string `json:"endpoint,omitempty"`
	RootCID          string `json:"root_cid"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

const defaultKuboEndpoint = "http://127.0.0.1:5001"

func guideIPFS(o *ipfsOptions, ask cliapp.Prompt) error {
	o.Endpoint = cmp.Or(o.Endpoint, defaultKuboEndpoint)
	return cliapp.AskFields(ask, cliapp.Field("Trusted Kubo RPC URL", &o.Endpoint), cliapp.Field("Directory CID", &o.RootCID))
}

func prepareIPFS(o ipfsOptions, _ string) (cliapp.MountSource, error) {
	o.Endpoint = cmp.Or(o.Endpoint, defaultKuboEndpoint)
	endpoint, err := sourceURL(o.Endpoint)
	if err != nil {
		return cliapp.MountSource{}, err
	}
	prefix := strings.TrimRight(endpoint.Path, "/")
	if !core.ValidName(o.RootCID) || !validFolderSettings(prefix, o.MaxResponseBytes) {
		return cliapp.MountSource{}, cliapp.InvalidConfig("IPFS needs a directory CID and a valid response budget.")
	}
	open := func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return cliapp.NativeDriver(ipfs.Open(ctx, ipfs.Options{
			Endpoint: o.Endpoint, RootCID: o.RootCID, Client: api, MaxResponseBytes: o.MaxResponseBytes,
		}))
	}
	if endpoint.Scheme == "http" {
		return cliapp.MountSource{OptionalCredential: true, Open: func(ctx context.Context, _ string) (core.Driver, error) {
			return open(ctx, nil)
		}}, nil
	}
	src := cliapp.BearerSource("https", open, endpoint.Host)
	src.Audience.Account = strings.TrimRight(endpoint.EscapedPath(), "/")
	src.Secret = "Kubo RPC bearer token"
	src.OptionalCredential = true
	return src, nil
}
