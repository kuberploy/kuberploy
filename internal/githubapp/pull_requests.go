package githubapp

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitHub's 2026-03-10 pull-request response has been observed returning a
// null merge_commit_sha for an already merged PR. The still-supported prior
// API version returns the authoritative merge SHA. Keep this explicit and
// narrow to same-PR merged-receipt recovery.
const pullRequestMergeCompatibilityAPIVersion = "2022-11-28"

type PullRequest struct {
	Repository    RepositoryIdentity
	Number        int64
	URL           string
	TargetRef     string
	HeadRef       string
	HeadRevision  string
	State         string
	Merged        bool
	MergeRevision string
	ObservedAt    time.Time
}

type apiPullRequest struct {
	Number         int64      `json:"number"`
	HTMLURL        string     `json:"html_url"`
	State          string     `json:"state"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	Head           struct {
		Ref  string        `json:"ref"`
		SHA  string        `json:"sha"`
		Repo apiRepository `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string        `json:"ref"`
		Repo apiRepository `json:"repo"`
	} `json:"base"`
}

func (c *Client) CreatePullRequest(ctx context.Context, token InstallationToken, repository RepositoryIdentity, targetRef, headRef, title, body string) (PullRequest, error) {
	if err := validatePullRequestToken(token, repository, PermissionWrite, c.clock.Now().UTC()); err != nil {
		return PullRequest{}, err
	}
	base, head, ok := pullRequestBranches(targetRef, headRef)
	if !ok || !validPullRequestText(title, 1, 256) || !validPullRequestText(body, 1, 4096) {
		return PullRequest{}, ErrInvalidTokenRequest
	}
	if err := c.verifyRepositoryIdentity(ctx, token.credential, repository); err != nil {
		return PullRequest{}, err
	}
	request := struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
	}{Title: title, Head: head, Base: base, Body: body}
	var response apiPullRequest
	if err := c.doJSON(ctx, http.MethodPost, token.credential,
		[]string{"repos", repository.OwnerLogin, repository.Name, "pulls"}, nil, request, http.StatusCreated, &response); err != nil {
		return PullRequest{}, err
	}
	return parsePullRequest(response, repository, targetRef, headRef, c.clock.Now().UTC())
}

func (c *Client) FindPullRequest(ctx context.Context, token InstallationToken, repository RepositoryIdentity, targetRef, headRef string) (PullRequest, bool, error) {
	if err := validatePullRequestToken(token, repository, PermissionRead, c.clock.Now().UTC()); err != nil {
		return PullRequest{}, false, err
	}
	base, head, ok := pullRequestBranches(targetRef, headRef)
	if !ok {
		return PullRequest{}, false, ErrInvalidTokenRequest
	}
	if err := c.verifyRepositoryIdentity(ctx, token.credential, repository); err != nil {
		return PullRequest{}, false, err
	}
	query := url.Values{"state": {"all"}, "base": {base}, "head": {repository.OwnerLogin + ":" + head}, "per_page": {"2"}}
	var response []apiPullRequest
	if err := c.doJSON(ctx, http.MethodGet, token.credential,
		[]string{"repos", repository.OwnerLogin, repository.Name, "pulls"}, query, nil, http.StatusOK, &response); err != nil {
		return PullRequest{}, false, err
	}
	if len(response) == 0 {
		return PullRequest{}, false, nil
	}
	if len(response) != 1 {
		return PullRequest{}, false, ErrProviderResponse
	}
	if err := c.fillMissingMergeRevision(ctx, token, repository, &response[0]); err != nil {
		return PullRequest{}, false, err
	}
	pullRequest, err := parsePullRequest(response[0], repository, targetRef, headRef, c.clock.Now().UTC())
	return pullRequest, err == nil, err
}

func (c *Client) GetPullRequest(ctx context.Context, token InstallationToken, repository RepositoryIdentity, number int64) (PullRequest, error) {
	if number <= 0 || validatePullRequestToken(token, repository, PermissionRead, c.clock.Now().UTC()) != nil {
		return PullRequest{}, ErrScopeMismatch
	}
	if err := c.verifyRepositoryIdentity(ctx, token.credential, repository); err != nil {
		return PullRequest{}, err
	}
	var response apiPullRequest
	if err := c.doJSON(ctx, http.MethodGet, token.credential,
		[]string{"repos", repository.OwnerLogin, repository.Name, "pulls", strconv.FormatInt(number, 10)}, nil, nil, http.StatusOK, &response); err != nil {
		return PullRequest{}, err
	}
	if response.Number != number {
		return PullRequest{}, ErrProviderResponse
	}
	baseRef, headRef := "refs/heads/"+response.Base.Ref, "refs/heads/"+response.Head.Ref
	if err := c.fillMissingMergeRevision(ctx, token, repository, &response); err != nil {
		return PullRequest{}, err
	}
	return parsePullRequest(response, repository, baseRef, headRef, c.clock.Now().UTC())
}

// GitHub may omit merge_commit_sha from a closed merged pull request even
// though merged_at is authoritative. Re-read that exact PR through the prior
// supported API contract and accept only an otherwise byte-identical identity.
func (c *Client) fillMissingMergeRevision(ctx context.Context, token InstallationToken, repository RepositoryIdentity, response *apiPullRequest) error {
	if response == nil || response.MergedAt == nil || response.MergeCommitSHA != "" {
		return nil
	}
	if response.State != "closed" || response.Number <= 0 {
		return ErrProviderResponse
	}
	var compatibility apiPullRequest
	if err := c.doJSONVersion(ctx, http.MethodGet, token.credential,
		[]string{"repos", repository.OwnerLogin, repository.Name, "pulls", strconv.FormatInt(response.Number, 10)}, nil, nil,
		http.StatusOK, &compatibility, pullRequestMergeCompatibilityAPIVersion); err != nil {
		return err
	}
	if compatibility.Number != response.Number || compatibility.HTMLURL != response.HTMLURL || compatibility.State != response.State ||
		compatibility.MergedAt == nil || !compatibility.MergedAt.Equal(*response.MergedAt) ||
		compatibility.Head.Ref != response.Head.Ref || compatibility.Head.SHA != response.Head.SHA ||
		compatibility.Head.Repo != response.Head.Repo || compatibility.Base.Ref != response.Base.Ref ||
		compatibility.Base.Repo != response.Base.Repo || !builderObjectIDPattern.MatchString(compatibility.MergeCommitSHA) {
		return ErrProviderResponse
	}
	response.MergeCommitSHA = compatibility.MergeCommitSHA
	return nil
}

func (c *Client) IsCommitAncestor(ctx context.Context, token InstallationToken, repository RepositoryIdentity, ancestor, descendant string) (bool, error) {
	if validatePullRequestToken(token, repository, PermissionRead, c.clock.Now().UTC()) != nil ||
		!builderObjectIDPattern.MatchString(ancestor) || !builderObjectIDPattern.MatchString(descendant) {
		return false, ErrScopeMismatch
	}
	if err := c.verifyRepositoryIdentity(ctx, token.credential, repository); err != nil {
		return false, err
	}
	var response struct {
		Status     string `json:"status"`
		AheadBy    int64  `json:"ahead_by"`
		BehindBy   int64  `json:"behind_by"`
		BaseCommit struct {
			SHA string `json:"sha"`
		} `json:"base_commit"`
		MergeBaseCommit struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := c.doJSON(ctx, http.MethodGet, token.credential,
		[]string{"repos", repository.OwnerLogin, repository.Name, "compare", ancestor + "..." + descendant}, nil, nil, http.StatusOK, &response); err != nil {
		return false, err
	}
	if response.BaseCommit.SHA != ancestor || !builderObjectIDPattern.MatchString(response.MergeBaseCommit.SHA) ||
		response.AheadBy < 0 || response.BehindBy < 0 {
		return false, ErrProviderResponse
	}
	switch response.Status {
	case "identical":
		return ancestor == descendant && response.MergeBaseCommit.SHA == ancestor && response.AheadBy == 0 && response.BehindBy == 0, nil
	case "ahead":
		return response.MergeBaseCommit.SHA == ancestor && response.AheadBy > 0 && response.BehindBy == 0, nil
	case "behind", "diverged":
		return false, nil
	default:
		return false, ErrProviderResponse
	}
}

func validatePullRequestToken(token InstallationToken, repository RepositoryIdentity, permission PermissionLevel, now time.Time) error {
	if token.credential.empty() || !now.Before(token.ExpiresAt) || !token.authorizes(repository.ID) || repository.validate() != nil ||
		token.installationID <= 0 || !permissionAllows(token.permissions["metadata"], PermissionRead) ||
		!permissionAllows(token.permissions["contents"], PermissionRead) || !permissionAllows(token.permissions["pull_requests"], permission) {
		return ErrScopeMismatch
	}
	return nil
}

func pullRequestBranches(targetRef, headRef string) (string, string, bool) {
	targetKind, base, targetOK := splitFullRef(targetRef)
	headKind, head, headOK := splitFullRef(headRef)
	return base, head, targetOK && headOK && targetKind == "heads" && headKind == "heads" && targetRef != headRef
}

func validPullRequestText(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum || strings.TrimSpace(value) != value {
		return false
	}
	return !strings.ContainsAny(value, "\x00\r")
}

func parsePullRequest(response apiPullRequest, repository RepositoryIdentity, targetRef, headRef string, observedAt time.Time) (PullRequest, error) {
	baseRepository, baseErr := response.Base.Repo.identity()
	headRepository, headErr := response.Head.Repo.identity()
	wantURL := "https://github.com/" + repository.OwnerLogin + "/" + repository.Name + "/pull/" + strconv.FormatInt(response.Number, 10)
	if baseErr != nil || headErr != nil || baseRepository != repository || headRepository != repository || response.Number <= 0 ||
		response.HTMLURL != wantURL || "refs/heads/"+response.Base.Ref != targetRef || "refs/heads/"+response.Head.Ref != headRef ||
		!builderObjectIDPattern.MatchString(response.Head.SHA) || (response.State != "open" && response.State != "closed") {
		return PullRequest{}, ErrProviderResponse
	}
	merged := response.MergedAt != nil
	if merged {
		if response.State != "closed" || response.MergedAt.After(observedAt) || !builderObjectIDPattern.MatchString(response.MergeCommitSHA) {
			return PullRequest{}, ErrProviderResponse
		}
	}
	if !merged {
		response.MergeCommitSHA = ""
	}
	return PullRequest{Repository: repository, Number: response.Number, URL: response.HTMLURL, TargetRef: targetRef,
		HeadRef: headRef, HeadRevision: response.Head.SHA, State: response.State, Merged: merged,
		MergeRevision: response.MergeCommitSHA, ObservedAt: observedAt}, nil
}
