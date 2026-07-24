package resource

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v66/github"
	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -o fakes/fake_git_hub.go . GitHub
type GitHub interface {
	ListReleases() ([]*github.RepositoryRelease, error)
	GetReleaseByTag(tag string) (*github.RepositoryRelease, error)
	GetRelease(id int) (*github.RepositoryRelease, error)
	CreateRelease(release github.RepositoryRelease) (*github.RepositoryRelease, error)
	UpdateRelease(release github.RepositoryRelease) (*github.RepositoryRelease, error)

	ListReleaseAssets(release github.RepositoryRelease) ([]*github.ReleaseAsset, error)
	UploadReleaseAsset(release github.RepositoryRelease, name string, file *os.File) error
	DeleteReleaseAsset(asset github.ReleaseAsset) error
	DownloadReleaseAsset(asset github.ReleaseAsset) (io.ReadCloser, error)

	GetTarballLink(tag string) (*url.URL, error)
	GetZipballLink(tag string) (*url.URL, error)
	ResolveTagToCommitSHA(tag string) (string, error)
}

type GitHubClient struct {
	client   *github.Client
	clientV4 *githubv4.Client

	owner      string
	repository string
	hasAuth    bool
	tokenFunc  func(ctx context.Context) (string, error)
}

func NewGitHubClient(source Source) (*GitHubClient, error) {
	if err := validateAuth(source); err != nil {
		return nil, err
	}

	var baseTransport http.RoundTripper = http.DefaultTransport
	ctx := context.TODO()

	if source.Insecure {
		baseTransport = &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: baseTransport})
	}

	var httpClient *http.Client
	var hasAuth bool
	var tokenFunc func(ctx context.Context) (string, error)

	switch {
	case source.AccessToken != "":
		var err error
		httpClient, err = oauthClient(ctx, source)
		if err != nil {
			return nil, err
		}
		hasAuth = true
		tokenFunc = func(_ context.Context) (string, error) {
			return source.AccessToken, nil
		}

	case source.AppID != 0:
		var err error
		httpClient, tokenFunc, err = appInstallationClient(ctx, baseTransport, source)
		if err != nil {
			return nil, err
		}
		hasAuth = true

	default:
		httpClient = &http.Client{Transport: baseTransport}
	}

	client := github.NewClient(httpClient)

	clientV4 := githubv4.NewClient(httpClient)

	if source.GitHubAPIURL != "" {
		var err error
		if !strings.HasSuffix(source.GitHubAPIURL, "/") {
			source.GitHubAPIURL += "/"
		}
		client.BaseURL, err = url.Parse(source.GitHubAPIURL)
		if err != nil {
			return nil, err
		}

		client.UploadURL, err = url.Parse(source.GitHubAPIURL)
		if err != nil {
			return nil, err
		}

		var v4URL string
		if s, found := strings.CutSuffix(source.GitHubAPIURL, "/v3/"); found {
			v4URL = s + "/graphql"
		} else {
			v4URL = source.GitHubAPIURL + "graphql"
		}
		clientV4 = githubv4.NewEnterpriseClient(v4URL, httpClient)
	}

	if source.GitHubV4APIURL != "" {
		clientV4 = githubv4.NewEnterpriseClient(source.GitHubV4APIURL, httpClient)
	}

	if source.GitHubUploadsURL != "" {
		var err error
		client.UploadURL, err = url.Parse(source.GitHubUploadsURL)
		if err != nil {
			return nil, err
		}
	}

	owner := source.Owner
	if source.User != "" {
		owner = source.User
	}

	return &GitHubClient{
		client:     client,
		clientV4:   clientV4,
		owner:      owner,
		repository: source.Repository,
		hasAuth:    hasAuth,
		tokenFunc:  tokenFunc,
	}, nil
}

func (g *GitHubClient) ListReleases() ([]*github.RepositoryRelease, error) {
	if g.hasAuth {
		return g.listReleasesV4()
	}
	opt := &github.ListOptions{PerPage: 100}
	var allReleases []*github.RepositoryRelease
	for {
		releases, res, err := g.client.Repositories.ListReleases(context.TODO(), g.owner, g.repository, opt)
		if err != nil {
			return []*github.RepositoryRelease{}, err
		}
		allReleases = append(allReleases, releases...)
		if res.NextPage == 0 {
			err = res.Body.Close()
			if err != nil {
				return nil, err
			}
			break
		}
		opt.Page = res.NextPage
	}

	return allReleases, nil
}

func (g *GitHubClient) GetReleaseByTag(tag string) (*github.RepositoryRelease, error) {
	release, res, err := g.client.Repositories.GetReleaseByTag(context.TODO(), g.owner, g.repository, tag)
	if err != nil {
		return &github.RepositoryRelease{}, err
	}

	err = res.Body.Close()
	if err != nil {
		return nil, err
	}

	return release, nil
}

func (g *GitHubClient) GetRelease(id int) (*github.RepositoryRelease, error) {
	release, res, err := g.client.Repositories.GetRelease(context.TODO(), g.owner, g.repository, int64(id))
	if err != nil {
		return &github.RepositoryRelease{}, err
	}

	err = res.Body.Close()
	if err != nil {
		return nil, err
	}

	return release, nil
}

func (g *GitHubClient) CreateRelease(release github.RepositoryRelease) (*github.RepositoryRelease, error) {
	createdRelease, res, err := g.client.Repositories.CreateRelease(context.TODO(), g.owner, g.repository, &release)
	if err != nil {
		return &github.RepositoryRelease{}, err
	}

	err = res.Body.Close()
	if err != nil {
		return nil, err
	}

	return createdRelease, nil
}

func (g *GitHubClient) UpdateRelease(release github.RepositoryRelease) (*github.RepositoryRelease, error) {
	if release.ID == nil {
		return nil, errors.New("release did not have an ID: has it been saved yet?")
	}

	updatedRelease, res, err := g.client.Repositories.EditRelease(context.TODO(), g.owner, g.repository, *release.ID, &release)
	if err != nil {
		return &github.RepositoryRelease{}, err
	}

	err = res.Body.Close()
	if err != nil {
		return nil, err
	}

	return updatedRelease, nil
}

func (g *GitHubClient) ListReleaseAssets(release github.RepositoryRelease) ([]*github.ReleaseAsset, error) {
	opt := &github.ListOptions{PerPage: 100}
	var allAssets []*github.ReleaseAsset
	for {
		assets, res, err := g.client.Repositories.ListReleaseAssets(context.TODO(), g.owner, g.repository, *release.ID, opt)
		if err != nil {
			return []*github.ReleaseAsset{}, err
		}
		allAssets = append(allAssets, assets...)
		if res.NextPage == 0 {
			err = res.Body.Close()
			if err != nil {
				return nil, err
			}
			break
		}
		opt.Page = res.NextPage
	}

	return allAssets, nil
}

func (g *GitHubClient) UploadReleaseAsset(release github.RepositoryRelease, name string, file *os.File) error {
	_, res, err := g.client.Repositories.UploadReleaseAsset(
		context.TODO(),
		g.owner,
		g.repository,
		*release.ID,
		&github.UploadOptions{
			Name: name,
		},
		file,
	)
	if err != nil {
		return err
	}

	return res.Body.Close()
}

func (g *GitHubClient) DeleteReleaseAsset(asset github.ReleaseAsset) error {
	res, err := g.client.Repositories.DeleteReleaseAsset(context.TODO(), g.owner, g.repository, *asset.ID)
	if err != nil {
		return err
	}

	return res.Body.Close()
}

func (g *GitHubClient) DownloadReleaseAsset(asset github.ReleaseAsset) (io.ReadCloser, error) {
	bodyReader, redirectURL, err := g.client.Repositories.DownloadReleaseAsset(context.TODO(), g.owner, g.repository, *asset.ID, nil)
	if err != nil {
		return nil, err
	}

	if redirectURL == "" {
		return bodyReader, err
	}

	req, err := g.client.NewRequest("GET", redirectURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	if g.hasAuth && req.URL.Host == g.client.BaseURL.Host {
		token, err := g.tokenFunc(context.TODO())
		if err != nil {
			return nil, fmt.Errorf("obtaining auth token for asset download: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	httpClient := &http.Client{}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close()
		return nil, fmt.Errorf("redirect URL %q responded with bad status code: %d", redirectURL, resp.StatusCode)
	}

	return resp.Body, nil
}

func (g *GitHubClient) GetTarballLink(tag string) (*url.URL, error) {
	opt := &github.RepositoryContentGetOptions{Ref: tag}
	u, res, err := g.client.Repositories.GetArchiveLink(context.TODO(), g.owner, g.repository, github.Tarball, opt, 10)
	if err != nil {
		return nil, err
	}
	res.Body.Close()
	return u, nil
}

func (g *GitHubClient) GetZipballLink(tag string) (*url.URL, error) {
	opt := &github.RepositoryContentGetOptions{Ref: tag}
	u, res, err := g.client.Repositories.GetArchiveLink(context.TODO(), g.owner, g.repository, github.Zipball, opt, 10)
	if err != nil {
		return nil, err
	}
	res.Body.Close()
	return u, nil
}

func (g *GitHubClient) ResolveTagToCommitSHA(tagName string) (string, error) {
	ref, res, err := g.client.Git.GetRef(context.TODO(), g.owner, g.repository, "tags/"+tagName)
	if err != nil {
		return "", err
	}
	res.Body.Close()

	if *ref.Object.Type == "commit" {
		return *ref.Object.SHA, nil
	}

	// Fail if we're not pointing to a tag or commit
	if *ref.Object.Type != "tag" {
		return "", fmt.Errorf("could not resolve tag %q to commit: ref type is %q, expected 'commit' or 'tag'", tagName, *ref.Object.Type)
	}

	// Follow the chain of annotated tags until we reach a commit
	currentSHA := *ref.Object.SHA
	maxDepth := 10

	for range maxDepth {
		tag, res, err := g.client.Git.GetTag(context.TODO(), g.owner, g.repository, currentSHA)
		if err != nil {
			return "", fmt.Errorf("could not get tag object %q: %w", currentSHA, err)
		}
		res.Body.Close()

		switch *tag.Object.Type {
		case "commit":
			return *tag.Object.SHA, nil
		case "tag":
			// Another annotated tag, continue following the chain
			currentSHA = *tag.Object.SHA
		default:
			return "", fmt.Errorf("could not resolve tag %q to commit: tag object points to %q, expected 'commit' or 'tag'", tagName, *tag.Object.Type)
		}
	}

	return "", fmt.Errorf("could not resolve tag %q to commit: exceeded maximum tag chain depth of %d", tagName, maxDepth)
}

func validateAuth(source Source) error {
	hasToken := source.AccessToken != ""
	hasAppID := source.AppID != 0
	hasKey := source.PrivateKey != ""

	if hasToken && (hasAppID || hasKey) {
		return errors.New("cannot specify both access_token and app credentials (app_id/private_key)")
	}
	if hasAppID != hasKey {
		return errors.New("both app_id and private_key must be specified for GitHub App auth")
	}
	return nil
}

func appInstallationClient(ctx context.Context, baseTransport http.RoundTripper, source Source) (*http.Client, func(context.Context) (string, error), error) {
	privateKey := []byte(source.PrivateKey)
	apiURL := normalizeURL(source.GitHubAPIURL)

	// Create an app-level transport to discover the installation
	appsTransport, err := ghinstallation.NewAppsTransport(baseTransport, int64(source.AppID), privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("creating GitHub App transport: %w", err)
	}

	if apiURL != "" {
		appsTransport.BaseURL = apiURL
	}

	// Discover the installation ID for this repository
	appClient := github.NewClient(&http.Client{Transport: appsTransport})
	if apiURL != "" {
		appClient.BaseURL, err = url.Parse(apiURL)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing GitHub API URL: %w", err)
		}
	}

	owner := source.Owner
	if source.User != "" {
		owner = source.User
	}

	installation, _, err := appClient.Apps.FindRepositoryInstallation(ctx, owner, source.Repository)
	if err != nil {
		return nil, nil, fmt.Errorf("finding GitHub App installation for %s/%s: %w", owner, source.Repository, err)
	}

	// Create installation-level transport for authenticated API access
	itr, err := ghinstallation.New(baseTransport, int64(source.AppID), installation.GetID(), privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("creating installation transport: %w", err)
	}

	if apiURL != "" {
		itr.BaseURL = apiURL
	}

	httpClient := &http.Client{Transport: itr}
	tokenFunc := func(ctx context.Context) (string, error) {
		return itr.Token(ctx)
	}

	return httpClient, tokenFunc, nil
}

func normalizeURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	if !strings.HasSuffix(rawURL, "/") {
		return rawURL + "/"
	}
	return rawURL
}

func oauthClient(ctx context.Context, source Source) (*http.Client, error) {
	ts := oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: source.AccessToken,
	})

	oauthClient := oauth2.NewClient(ctx, ts)

	githubHTTPClient := &http.Client{
		Transport: oauthClient.Transport,
	}

	return githubHTTPClient, nil
}
