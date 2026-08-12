package githubapp

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func pullRequestToken(now time.Time) InstallationToken {
	return scopedToken(now, Permissions{"metadata": PermissionRead, "contents": PermissionWrite, "pull_requests": PermissionWrite})
}

func pullRequestFixture(repository RepositoryIdentity, number int64, state string, mergedAt *time.Time, headSHA, mergeSHA string) map[string]any {
	return map[string]any{
		"number": number, "html_url": "https://github.com/" + repository.fullName() + "/pull/" + strconv.FormatInt(number, 10),
		"state": state, "merged_at": mergedAt, "merge_commit_sha": mergeSHA,
		"head": map[string]any{"ref": "kuberploy/operations/11111111-1111-4111-8111-111111111111", "sha": headSHA, "repo": apiRepositoryFixture(repository, "Organization")},
		"base": map[string]any{"ref": "main", "repo": apiRepositoryFixture(repository, "Organization")},
	}
}

func TestCreatePullRequestUsesExactRepositoryRefsAndBody(t *testing.T) {
	now := time.Date(2026, 8, 9, 11, 0, 0, 0, time.UTC)
	repository := testRepositories[1]
	sha := strings.Repeat("a", 40)
	var calls int
	client := pullRequestClient(t, now, func(request *http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			if request.URL.Path != "/repositories/101" {
				t.Fatalf("identity path=%s", request.URL.Path)
			}
			return httpResponse(http.StatusOK, marshalFixture(t, apiRepositoryFixture(repository, "Organization")), nil), nil
		case 2:
			if request.Method != http.MethodPost || request.URL.Path != "/repos/kuberploy/service/pulls" || request.URL.RawQuery != "" {
				t.Fatalf("create request=%s %s?%s", request.Method, request.URL.Path, request.URL.RawQuery)
			}
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"title": "kuberploy: publish", "head": "kuberploy/operations/11111111-1111-4111-8111-111111111111", "base": "main", "body": "immutable body"}
			if !reflect.DeepEqual(body, want) {
				t.Fatalf("body=%#v", body)
			}
			return httpResponse(http.StatusCreated, marshalFixture(t, pullRequestFixture(repository, 7, "open", nil, sha, strings.Repeat("f", 40))), nil), nil
		default:
			t.Fatalf("unexpected provider call %d", calls)
			return nil, nil
		}
	})
	result, err := client.CreatePullRequest(t.Context(), pullRequestToken(now), repository, "refs/heads/main",
		"refs/heads/kuberploy/operations/11111111-1111-4111-8111-111111111111", "kuberploy: publish", "immutable body")
	if err != nil || result.Number != 7 || result.HeadRevision != sha || result.Merged || result.MergeRevision != "" || calls != 2 {
		t.Fatalf("result=%#v calls=%d err=%v", result, calls, err)
	}
}

func TestCreatePullRequestRejectsMissingWriteScopeBeforeProviderCall(t *testing.T) {
	now := time.Date(2026, 8, 9, 11, 0, 0, 0, time.UTC)
	calls := 0
	client := pullRequestClient(t, now, func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not be called")
	})
	readOnly := scopedToken(now, Permissions{"metadata": PermissionRead, "contents": PermissionWrite, "pull_requests": PermissionRead})
	_, err := client.CreatePullRequest(t.Context(), readOnly, testRepositories[1], "refs/heads/main",
		"refs/heads/kuberploy/operations/11111111-1111-4111-8111-111111111111", "kuberploy: publish", "immutable body")
	if !errors.Is(err, ErrScopeMismatch) || calls != 0 {
		t.Fatalf("missing scope err=%v provider calls=%d", err, calls)
	}
}

func TestFindPullRequestRejectsMultipleOrSubstitutedMatches(t *testing.T) {
	now := time.Date(2026, 8, 9, 11, 0, 0, 0, time.UTC)
	repository := testRepositories[1]
	sha := strings.Repeat("a", 40)
	tests := []struct {
		name     string
		response []any
	}{
		{name: "multiple", response: []any{pullRequestFixture(repository, 7, "open", nil, sha, ""), pullRequestFixture(repository, 8, "open", nil, sha, "")}},
		{name: "substituted head", response: []any{func() map[string]any {
			value := pullRequestFixture(repository, 7, "open", nil, sha, "")
			value["head"].(map[string]any)["ref"] = "attacker"
			return value
		}()}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := pullRequestClient(t, now, func(request *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return httpResponse(http.StatusOK, marshalFixture(t, apiRepositoryFixture(repository, "Organization")), nil), nil
				}
				if request.URL.Query().Get("state") != "all" || request.URL.Query().Get("base") != "main" ||
					request.URL.Query().Get("head") != "kuberploy:kuberploy/operations/11111111-1111-4111-8111-111111111111" || request.URL.Query().Get("per_page") != "2" {
					t.Fatalf("query=%s", request.URL.RawQuery)
				}
				return httpResponse(http.StatusOK, marshalFixture(t, test.response), nil), nil
			})
			_, _, err := client.FindPullRequest(t.Context(), pullRequestToken(now), repository, "refs/heads/main",
				"refs/heads/kuberploy/operations/11111111-1111-4111-8111-111111111111")
			if !errors.Is(err, ErrProviderResponse) {
				t.Fatalf("response accepted: %v", err)
			}
		})
	}
}

func TestGetMergedPullRequestUsesExactCompatibilityReceiptWhenGitHubOmitsMergeSHA(t *testing.T) {
	now := time.Date(2026, 8, 12, 11, 5, 0, 0, time.UTC)
	mergedAt := now.Add(-time.Minute)
	repository := testRepositories[1]
	headSHA, targetSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	calls := 0
	client := pullRequestClient(t, now, func(request *http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			if request.URL.Path != "/repositories/101" {
				t.Fatalf("identity path=%s", request.URL.Path)
			}
			return httpResponse(http.StatusOK, marshalFixture(t, apiRepositoryFixture(repository, "Organization")), nil), nil
		case 2:
			if request.URL.Path != "/repos/kuberploy/service/pulls/7" {
				t.Fatalf("pull request path=%s", request.URL.Path)
			}
			if got := request.Header.Get("X-GitHub-Api-Version"); got != validTestConfig(t).APIVersion {
				t.Fatalf("current API version=%q", got)
			}
			return httpResponse(http.StatusOK, marshalFixture(t, pullRequestFixture(repository, 7, "closed", &mergedAt, headSHA, "")), nil), nil
		case 3:
			if request.URL.Path != "/repos/kuberploy/service/pulls/7" {
				t.Fatalf("compatibility PR path=%s", request.URL.Path)
			}
			if got := request.Header.Get("X-GitHub-Api-Version"); got != pullRequestMergeCompatibilityAPIVersion {
				t.Fatalf("compatibility API version=%q", got)
			}
			return httpResponse(http.StatusOK, marshalFixture(t, pullRequestFixture(repository, 7, "closed", &mergedAt, headSHA, targetSHA)), nil), nil
		default:
			t.Fatalf("unexpected provider call %d", calls)
			return nil, nil
		}
	})
	result, err := client.GetPullRequest(t.Context(), pullRequestToken(now), repository, 7)
	if err != nil || !result.Merged || result.MergeRevision != targetSHA || result.HeadRevision != headSHA || calls != 3 {
		t.Fatalf("result=%#v calls=%d err=%v", result, calls, err)
	}
}

func TestMissingMergeSHAFallbackRejectsSubstitutedPullRequestIdentity(t *testing.T) {
	now := time.Date(2026, 8, 12, 11, 5, 0, 0, time.UTC)
	mergedAt := now.Add(-time.Minute)
	repository := testRepositories[1]
	calls := 0
	client := pullRequestClient(t, now, func(request *http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			return httpResponse(http.StatusOK, marshalFixture(t, apiRepositoryFixture(repository, "Organization")), nil), nil
		case 2:
			return httpResponse(http.StatusOK, marshalFixture(t, pullRequestFixture(repository, 7, "closed", &mergedAt, strings.Repeat("a", 40), "")), nil), nil
		case 3:
			fixture := pullRequestFixture(repository, 7, "closed", &mergedAt, strings.Repeat("a", 40), strings.Repeat("b", 40))
			fixture["head"].(map[string]any)["ref"] = "substituted"
			return httpResponse(http.StatusOK, marshalFixture(t, fixture), nil), nil
		default:
			t.Fatalf("unexpected provider call %d", calls)
			return nil, nil
		}
	})
	if _, err := client.GetPullRequest(t.Context(), pullRequestToken(now), repository, 7); !errors.Is(err, ErrProviderResponse) {
		t.Fatalf("substituted pull request accepted: %v", err)
	}
}

func TestCommitAncestryRequiresExactCompareProof(t *testing.T) {
	now := time.Date(2026, 8, 9, 11, 0, 0, 0, time.UTC)
	repository := testRepositories[1]
	ancestor, descendant := strings.Repeat("a", 40), strings.Repeat("b", 40)
	client := pullRequestClient(t, now, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/repositories/101" {
			return httpResponse(http.StatusOK, marshalFixture(t, apiRepositoryFixture(repository, "Organization")), nil), nil
		}
		if request.URL.Path != "/repos/kuberploy/service/compare/"+ancestor+"..."+descendant {
			t.Fatalf("compare path=%s", request.URL.Path)
		}
		return httpResponse(http.StatusOK, marshalFixture(t, map[string]any{
			"status": "ahead", "ahead_by": 1, "behind_by": 0,
			"base_commit": map[string]any{"sha": ancestor}, "merge_base_commit": map[string]any{"sha": strings.Repeat("c", 40)},
		}), nil), nil
	})
	present, err := client.IsCommitAncestor(t.Context(), pullRequestToken(now), repository, ancestor, descendant)
	if err != nil || present {
		t.Fatalf("substituted merge base accepted: present=%v err=%v", present, err)
	}
}

func pullRequestClient(t *testing.T, now time.Time, responder func(*http.Request) (*http.Response, error)) *Client {
	t.Helper()
	cfg := validTestConfig(t)
	cfg.MaximumTokenPermissions = Permissions{"metadata": PermissionRead, "contents": PermissionWrite, "pull_requests": PermissionWrite}
	client, err := NewClient(cfg, staticAppTokens{token: testAppToken()}, roundTripFunc(responder), &fixedClock{now: now})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
