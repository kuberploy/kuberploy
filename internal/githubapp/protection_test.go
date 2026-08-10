package githubapp

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func protectedRepositoryFixture(writerAppID int64) map[string]any {
	return map[string]any{
		"url":                           "https://api.github.com/repos/kuberploy/platform/branches/platform/protection",
		"required_status_checks":        nil,
		"required_pull_request_reviews": nil,
		"required_signatures":           map[string]any{"enabled": false},
		"enforce_admins":                map[string]any{"enabled": true},
		"restrictions": map[string]any{
			"url": "https://api.github.com/restrictions", "users_url": "https://api.github.com/users",
			"teams_url": "https://api.github.com/teams", "apps_url": "https://api.github.com/apps",
			"users": []any{}, "teams": []any{}, "apps": []any{map[string]any{"id": writerAppID, "slug": "kuberploy"}},
		},
		"required_linear_history":          map[string]any{"enabled": true},
		"allow_force_pushes":               map[string]any{"enabled": false},
		"allow_deletions":                  map[string]any{"enabled": false},
		"block_creations":                  map[string]any{"enabled": true},
		"required_conversation_resolution": map[string]any{"enabled": false},
		"lock_branch":                      map[string]any{"enabled": false},
		"allow_fork_syncing":               map[string]any{"enabled": false},
	}
}

func protectionClient(t *testing.T, now time.Time, responder func(*http.Request) (*http.Response, error)) *Client {
	t.Helper()
	cfg := validTestConfig(t)
	cfg.MaximumTokenPermissions = clonePermissions(cfg.MaximumTokenPermissions)
	cfg.MaximumTokenPermissions["administration"] = PermissionRead
	client, err := NewClient(cfg, staticAppTokens{token: testAppToken()}, roundTripFunc(responder), &fixedClock{now: now})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func protectionToken(now time.Time) InstallationToken {
	return scopedToken(now, Permissions{"metadata": PermissionRead, "contents": PermissionRead, "administration": PermissionRead})
}

func TestObserveRepositoryProtectionBindsExactIdentityHeadPolicyAndRules(t *testing.T) {
	now := time.Date(2026, 8, 9, 11, 0, 0, 0, time.UTC)
	repository := RepositoryIdentity{ID: 101, Name: "platform", OwnerID: 55, OwnerLogin: "kuberploy"}
	head := strings.Repeat("a", 40)
	var paths []string
	client := protectionClient(t, now, func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.EscapedPath()+"?"+request.URL.RawQuery)
		switch request.URL.EscapedPath() {
		case "/repositories/101":
			return httpResponse(http.StatusOK, marshalFixture(t, apiRepositoryFixture(repository, "Organization")), nil), nil
		case "/repos/kuberploy/platform/git/ref/heads/platform":
			return httpResponse(http.StatusOK, marshalFixture(t, map[string]any{"ref": "refs/heads/platform", "object": map[string]any{"type": "commit", "sha": head}}), nil), nil
		case "/repos/kuberploy/platform/branches/platform/protection":
			return httpResponse(http.StatusOK, marshalFixture(t, protectedRepositoryFixture(12345)), nil), nil
		case "/repos/kuberploy/platform/rules/branches/platform":
			return httpResponse(http.StatusOK, marshalFixture(t, []any{
				map[string]any{"type": "required_linear_history", "ruleset_source_type": "Organization", "ruleset_source": "kuberploy", "ruleset_id": 42},
				map[string]any{"type": "non_fast_forward", "ruleset_source_type": "Repository", "ruleset_source": "kuberploy/platform", "ruleset_id": 41},
			}), nil), nil
		default:
			t.Fatalf("unexpected protection request %s", request.URL)
			return nil, nil
		}
	})

	observation, err := client.ObserveRepositoryProtection(t.Context(), protectionToken(now), repository, "refs/heads/platform", head, 12345)
	if err != nil {
		t.Fatal(err)
	}
	if observation.InstallationID != 77 || observation.RepositoryID != repository.ID || observation.Ref != "refs/heads/platform" ||
		observation.Head != head || observation.WriterAppID != 12345 || observation.ObservedAt != now ||
		!strings.HasPrefix(observation.PolicyDigest, "sha256:") || len(observation.PolicyDigest) != 71 {
		t.Fatalf("observation=%#v", observation)
	}
	wantPaths := []string{
		"/repositories/101?",
		"/repos/kuberploy/platform/git/ref/heads/platform?",
		"/repos/kuberploy/platform/branches/platform/protection?",
		"/repos/kuberploy/platform/rules/branches/platform?page=1&per_page=100",
	}
	if !slices.Equal(paths, wantPaths) {
		t.Fatalf("paths=%#v", paths)
	}
}

func TestObserveRepositoryProtectionRejectsBypassAndWriterBlockingPolicy(t *testing.T) {
	now := time.Date(2026, 8, 9, 11, 0, 0, 0, time.UTC)
	repository := RepositoryIdentity{ID: 101, Name: "platform", OwnerID: 55, OwnerLogin: "kuberploy"}
	head := strings.Repeat("b", 40)
	tests := []struct {
		name   string
		mutate func(map[string]any)
		rules  []any
		want   error
	}{
		{name: "tenant user push", mutate: func(value map[string]any) {
			value["restrictions"].(map[string]any)["users"] = []any{map[string]any{"id": 9}}
		}, want: ErrRepositoryProtection},
		{name: "tenant team push", mutate: func(value map[string]any) {
			value["restrictions"].(map[string]any)["teams"] = []any{map[string]any{"id": 9}}
		}, want: ErrRepositoryProtection},
		{name: "wrong app", mutate: func(value map[string]any) {
			value["restrictions"].(map[string]any)["apps"] = []any{map[string]any{"id": 999}}
		}, want: ErrRepositoryProtection},
		{name: "admin bypass", mutate: func(value map[string]any) { value["enforce_admins"] = map[string]any{"enabled": false} }, want: ErrRepositoryProtection},
		{name: "force push", mutate: func(value map[string]any) { value["allow_force_pushes"] = map[string]any{"enabled": true} }, want: ErrRepositoryProtection},
		{name: "delete", mutate: func(value map[string]any) { value["allow_deletions"] = map[string]any{"enabled": true} }, want: ErrRepositoryProtection},
		{name: "locked writer", mutate: func(value map[string]any) { value["lock_branch"] = map[string]any{"enabled": true} }, want: ErrRepositoryProtection},
		{name: "pull request gate", mutate: func(value map[string]any) {
			value["required_pull_request_reviews"] = map[string]any{"required_approving_review_count": 1}
		}, want: ErrRepositoryProtection},
		{name: "status gate", mutate: func(value map[string]any) {
			value["required_status_checks"] = map[string]any{"contexts": []string{"ci"}}
		}, want: ErrRepositoryProtection},
		{name: "signature gate", mutate: func(value map[string]any) { value["required_signatures"] = map[string]any{"enabled": true} }, want: ErrRepositoryProtection},
		{name: "unknown protection control", mutate: func(value map[string]any) { value["future_bypass"] = map[string]any{"enabled": true} }, want: ErrProviderResponse},
		{name: "ruleset update gate", rules: []any{map[string]any{"type": "update", "ruleset_source_type": "Organization", "ruleset_source": "kuberploy", "ruleset_id": 42}}, want: ErrRepositoryProtection},
		{name: "unknown ruleset rule", rules: []any{map[string]any{"type": "future_rule", "ruleset_source_type": "Repository", "ruleset_source": "kuberploy/platform", "ruleset_id": 43}}, want: ErrRepositoryProtection},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := protectionClient(t, now, func(request *http.Request) (*http.Response, error) {
				switch request.URL.EscapedPath() {
				case "/repositories/101":
					return httpResponse(http.StatusOK, marshalFixture(t, apiRepositoryFixture(repository, "Organization")), nil), nil
				case "/repos/kuberploy/platform/git/ref/heads/platform":
					return httpResponse(http.StatusOK, marshalFixture(t, map[string]any{"ref": "refs/heads/platform", "object": map[string]any{"type": "commit", "sha": head}}), nil), nil
				case "/repos/kuberploy/platform/branches/platform/protection":
					value := protectedRepositoryFixture(12345)
					if test.mutate != nil {
						test.mutate(value)
					}
					return httpResponse(http.StatusOK, marshalFixture(t, value), nil), nil
				case "/repos/kuberploy/platform/rules/branches/platform":
					return httpResponse(http.StatusOK, marshalFixture(t, test.rules), nil), nil
				default:
					t.Fatalf("unexpected request %s", request.URL)
					return nil, nil
				}
			})
			_, err := client.ObserveRepositoryProtection(t.Context(), protectionToken(now), repository, "refs/heads/platform", head, 12345)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}
}

func TestObserveRepositoryProtectionRejectsScopeIdentityHeadAndRedirect(t *testing.T) {
	now := time.Date(2026, 8, 9, 11, 0, 0, 0, time.UTC)
	repository := RepositoryIdentity{ID: 101, Name: "platform", OwnerID: 55, OwnerLogin: "kuberploy"}
	head := strings.Repeat("c", 40)
	noAdmin := scopedToken(now, Permissions{"metadata": PermissionRead, "contents": PermissionRead})
	client := protectionClient(t, now, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid scope must fail before HTTP")
		return nil, nil
	})
	if _, err := client.ObserveRepositoryProtection(t.Context(), noAdmin, repository, "refs/heads/platform", head, 12345); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("missing administration read=%v", err)
	}
	if _, err := client.ObserveRepositoryProtection(t.Context(), protectionToken(now), repository, "refs/tags/platform", head, 12345); !errors.Is(err, ErrInvalidTokenRequest) {
		t.Fatalf("tag ref accepted=%v", err)
	}

	calls := 0
	client = protectionClient(t, now, func(request *http.Request) (*http.Response, error) {
		calls++
		switch request.URL.EscapedPath() {
		case "/repositories/101":
			mutated := repository
			mutated.Name = "renamed"
			return httpResponse(http.StatusOK, marshalFixture(t, apiRepositoryFixture(mutated, "Organization")), nil), nil
		default:
			t.Fatalf("identity mismatch reached %s", request.URL)
			return nil, nil
		}
	})
	if _, err := client.ObserveRepositoryProtection(t.Context(), protectionToken(now), repository, "refs/heads/platform", head, 12345); !errors.Is(err, ErrOwnershipMismatch) || calls != 1 {
		t.Fatalf("identity mismatch calls=%d err=%v", calls, err)
	}

	calls = 0
	client = protectionClient(t, now, func(request *http.Request) (*http.Response, error) {
		calls++
		switch request.URL.EscapedPath() {
		case "/repositories/101":
			return httpResponse(http.StatusOK, marshalFixture(t, apiRepositoryFixture(repository, "Organization")), nil), nil
		case "/repos/kuberploy/platform/git/ref/heads/platform":
			return httpResponse(http.StatusOK, marshalFixture(t, map[string]any{"ref": "refs/heads/platform", "object": map[string]any{"type": "commit", "sha": strings.Repeat("d", 40)}}), nil), nil
		default:
			t.Fatalf("head mismatch reached %s", request.URL)
			return nil, nil
		}
	})
	if _, err := client.ObserveRepositoryProtection(t.Context(), protectionToken(now), repository, "refs/heads/platform", head, 12345); !errors.Is(err, ErrRepositoryProtection) || calls != 2 {
		t.Fatalf("head mismatch calls=%d err=%v", calls, err)
	}

	client = protectionClient(t, now, func(request *http.Request) (*http.Response, error) {
		switch request.URL.EscapedPath() {
		case "/repositories/101":
			return httpResponse(http.StatusOK, marshalFixture(t, apiRepositoryFixture(repository, "Organization")), nil), nil
		case "/repos/kuberploy/platform/git/ref/heads/platform":
			return httpResponse(http.StatusOK, marshalFixture(t, map[string]any{"ref": "refs/heads/platform", "object": map[string]any{"type": "commit", "sha": head}}), nil), nil
		case "/repos/kuberploy/platform/branches/platform/protection":
			return httpResponse(http.StatusFound, `{"token":"must-not-leak"}`, map[string]string{"Location": "https://attacker.invalid/collect"}), nil
		default:
			t.Fatalf("redirect followed to %s", request.URL)
			return nil, nil
		}
	})
	_, err := client.ObserveRepositoryProtection(context.Background(), protectionToken(now), repository, "refs/heads/platform", head, 12345)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Class != APIErrorRedirect || strings.Contains(err.Error(), "attacker") {
		t.Fatalf("redirect error=%v", err)
	}
}
