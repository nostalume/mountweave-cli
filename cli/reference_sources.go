package cli

import (
	"context"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	core "github.com/nostalume/mountweave"
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

func guideRepositoryTree(o *repositoryTreeOptions, ask askField) error {
	return askFields(ask, setupField{"Repository (owner/name)", &o.Repository},
		setupField{"Commit, branch or tag", &o.Ref}, setupField{"Folder path (/ for repository root)", &o.RootPath})
}

func (o repositoryTreeOptions) check() (owner, repo string, err error) {
	owner, repo, _ = strings.Cut(o.Repository, "/")
	if !core.ValidName(owner) || !core.ValidName(repo) || !validRepositoryRef(o.Ref) ||
		(o.RootPath != "" && core.ValidatePath(o.RootPath) != nil) || !validResponseBudget(o.MaxResponseBytes) {
		err = invalidConfig("Use an owner/name repository, a ref and valid folder and response settings.")
	}
	return
}

func validRepositoryRef(ref string) bool {
	return ref != "" && ref != "." && ref != ".." && !strings.ContainsFunc(ref, func(r rune) bool { return r < ' ' || r == 127 })
}

func prepareGitHubRepo(o repositoryTreeOptions, _ string) (mountSource, error) {
	owner, repo, err := o.check()
	if err != nil {
		return mountSource{}, err
	}
	src := bearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return nativeDriver(githubrepo.Open(ctx, githubrepo.Options{
			Owner: owner, Repository: repo, Ref: o.Ref, RootPath: o.RootPath, Client: api, MaxResponseBytes: o.MaxResponseBytes,
		}))
	}, "api.github.com")
	src.optionalCredential = true
	return src, nil
}

func prepareAtomGit(o repositoryTreeOptions, _ string) (mountSource, error) {
	owner, repo, err := o.check()
	if err != nil {
		return mountSource{}, err
	}
	src := bearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return nativeDriver(atomgit.Open(ctx, atomgit.Options{
			Owner: owner, Repository: repo, Ref: o.Ref, RootPath: o.RootPath, Client: api, MaxResponseBytes: o.MaxResponseBytes,
		}))
	}, "api.atomgit.com")
	src.optionalCredential = true
	return src, nil
}

type releaseOptions struct {
	Repository       string `json:"repository"`
	Tag              string `json:"tag"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideGitHubRelease(o *releaseOptions, ask askField) error {
	return askFields(ask, setupField{"Repository (owner/name)", &o.Repository}, setupField{"Release tag", &o.Tag})
}

func guideCNBRelease(o *releaseOptions, ask askField) error {
	return askFields(ask, setupField{"Repository (organization/name or organization/team/name)", &o.Repository}, setupField{"Release tag", &o.Tag})
}

func prepareGitHubRelease(o releaseOptions, _ string) (mountSource, error) {
	owner, repo, _ := strings.Cut(o.Repository, "/")
	if !core.ValidName(owner) || !core.ValidName(repo) || !validRepositoryRef(o.Tag) || !validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("GitHub Releases needs an owner/name repository, a tag and valid response settings.")
	}
	src := bearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return nativeDriver(githubreleases.Open(ctx, githubreleases.Options{
			Owner: owner, Repository: repo, Tag: o.Tag, Client: api, MaxResponseBytes: o.MaxResponseBytes,
		}))
	}, "api.github.com")
	src.optionalCredential = true
	return src, nil
}

func prepareCNBRelease(o releaseOptions, _ string) (mountSource, error) {
	if !strings.Contains(o.Repository, "/") || !validRepositoryRef(o.Tag) || !validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("CNB Releases needs a repository path, a tag and valid response settings.")
	}
	for part := range strings.SplitSeq(o.Repository, "/") {
		if !core.ValidName(part) || part == "-" {
			return mountSource{}, invalidConfig("CNB Releases needs a valid repository path.")
		}
	}
	return bearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return nativeDriver(cnbreleases.Open(ctx, cnbreleases.Options{
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

func guideCodeArtsRepo(o *codeArtsRepoOptions, ask askField) error {
	id := ""
	if o.RepositoryID != 0 {
		id = strconv.FormatInt(int64(o.RepositoryID), 10)
	}
	if err := askFields(ask, setupField{"Regional HTTPS API origin", &o.Endpoint}, setupField{"Repository ID", &id},
		setupField{"Commit SHA", &o.CommitSHA}, setupField{"Folder path (/ for repository root)", &o.RootPath}); err != nil {
		return err
	}
	value, err := strconv.ParseInt(id, 10, 32)
	if err != nil || value < 1 {
		return invalidConfig("CodeArts Repo needs a positive numeric repository ID.")
	}
	o.RepositoryID = int32(value)
	return nil
}

func prepareCodeArtsRepo(o codeArtsRepoOptions, _ string) (mountSource, error) {
	origin, err := sourceURL(o.Endpoint)
	if err != nil {
		return mountSource{}, err
	}
	validSHA := false
	if len(o.CommitSHA) == 40 {
		_, err := hex.DecodeString(o.CommitSHA)
		validSHA = err == nil
	}
	if origin.Scheme != "https" || (origin.Path != "" && origin.Path != "/") || o.RepositoryID < 1 ||
		!validSHA ||
		(o.RootPath != "" && core.ValidatePath(o.RootPath) != nil) || !validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("CodeArts Repo needs an HTTPS API origin, a numeric repository ID, a full commit SHA and valid folder and response settings.")
	}
	src := headerTokenSource("https", "X-Auth-Token", "", func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return nativeDriver(codeartsrepo.Open(ctx, codeartsrepo.Options{
			Endpoint: o.Endpoint, RepositoryID: o.RepositoryID, CommitSHA: o.CommitSHA, RootPath: o.RootPath,
			Client: api, MaxResponseBytes: o.MaxResponseBytes,
		}))
	}, origin.Host)
	src.secret = "Huawei Cloud IAM token"
	return src, nil
}

type ipfsOptions struct {
	Endpoint         string `json:"endpoint,omitempty"`
	RootCID          string `json:"root_cid"`
	MaxResponseBytes int64  `json:"max_response_bytes,omitempty"`
}

func guideIPFS(o *ipfsOptions, ask askField) error {
	if o.Endpoint == "" {
		o.Endpoint = "http://127.0.0.1:5001"
	}
	return askFields(ask, setupField{"Trusted Kubo RPC URL", &o.Endpoint}, setupField{"Directory CID", &o.RootCID})
}

func prepareIPFS(o ipfsOptions, _ string) (mountSource, error) {
	if o.Endpoint == "" {
		o.Endpoint = "http://127.0.0.1:5001"
	}
	endpoint, err := sourceURL(o.Endpoint)
	if err != nil {
		return mountSource{}, err
	}
	prefix := strings.TrimRight(endpoint.Path, "/")
	if (prefix != "" && core.ValidatePath(prefix) != nil) || !core.ValidName(o.RootCID) || !validResponseBudget(o.MaxResponseBytes) {
		return mountSource{}, invalidConfig("IPFS needs a directory CID and a valid response budget.")
	}
	open := func(ctx context.Context, api *http.Client) (core.Driver, error) {
		return nativeDriver(ipfs.Open(ctx, ipfs.Options{
			Endpoint: o.Endpoint, RootCID: o.RootCID, Client: api, MaxResponseBytes: o.MaxResponseBytes,
		}))
	}
	if endpoint.Scheme == "http" {
		return mountSource{optionalCredential: true, open: func(ctx context.Context, _ string) (core.Driver, error) {
			return open(ctx, nil)
		}}, nil
	}
	src := bearerSource("https", open, endpoint.Host)
	src.audience.account = strings.TrimRight(endpoint.EscapedPath(), "/")
	src.secret = "Kubo RPC bearer token"
	src.optionalCredential = true
	return src, nil
}
