package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var (
	testOrganization = AccountIdentity{ID: 55, Login: "kuberploy", Type: "Organization"}
	testUser         = AccountIdentity{ID: 900, Login: "octocat", Type: "User"}
	testRepositories = []RepositoryIdentity{
		{ID: 102, Name: "worker", OwnerID: 55, OwnerLogin: "kuberploy"},
		{ID: 101, Name: "service", OwnerID: 55, OwnerLogin: "kuberploy"},
	}
)

func apiInstallationFixture(cfg Config, id int64, account AccountIdentity, permissions map[string]string) map[string]any {
	return map[string]any{
		"id": id, "app_id": cfg.AppID, "client_id": cfg.ClientID,
		"account":              map[string]any{"id": account.ID, "login": account.Login, "type": account.Type},
		"repository_selection": "selected", "permissions": permissions, "suspended_at": nil,
	}
}

func apiRepositoryFixture(repository RepositoryIdentity, ownerType string) map[string]any {
	return map[string]any{
		"id": repository.ID, "name": repository.Name, "full_name": repository.fullName(),
		"owner": map[string]any{"id": repository.OwnerID, "login": repository.OwnerLogin, "type": ownerType},
	}
}

func marshalFixture(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestSetupCallbackBindsExactUserAndDoesNotTrustInstallationID(t *testing.T) {
	cfg := validTestConfig(t)
	userToken := "ghu_user_access_token_for_setup"
	makeClient := func(installationID int64, installations []any) (*Client, *atomic.Int64) {
		t.Helper()
		var calls atomic.Int64
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			if request.Header.Get("Authorization") != "Bearer "+userToken {
				t.Fatalf("wrong callback authorization: %q", request.Header.Get("Authorization"))
			}
			switch request.URL.Path {
			case "/user":
				return httpResponse(http.StatusOK, marshalFixture(t, map[string]any{"id": testUser.ID, "login": testUser.Login, "type": testUser.Type}), nil), nil
			case "/user/installations":
				if request.URL.Query().Get("per_page") != "100" || request.URL.Query().Get("page") != "1" {
					t.Fatalf("unbounded pagination: %s", request.URL.RawQuery)
				}
				return httpResponse(http.StatusOK, marshalFixture(t, map[string]any{"total_count": len(installations), "installations": installations}), nil), nil
			default:
				t.Fatalf("unexpected setup path %s for callback %d", request.URL.Path, installationID)
				return nil, nil
			}
		})
		client, err := NewClient(cfg, staticAppTokens{token: testAppToken()}, transport, &fixedClock{now: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return client, &calls
	}
	installation := apiInstallationFixture(cfg, 42, testOrganization, map[string]string{"metadata": "read", "contents": "read"})
	client, calls := makeClient(42, []any{installation})
	verified, err := client.VerifySetupInstallationRaw(context.Background(), userToken, 42, testUser.ID, testOrganization.ID)
	if err != nil {
		t.Fatal(err)
	}
	if verified.User.ID != testUser.ID || verified.Installation.ID != 42 || calls.Load() != 2 {
		t.Fatalf("verification=%#v calls=%d", verified, calls.Load())
	}
	t.Run("spoofed callback installation id", func(t *testing.T) {
		client, _ := makeClient(99, []any{installation})
		if _, err := client.VerifySetupInstallationRaw(context.Background(), userToken, 99, testUser.ID, testOrganization.ID); !errors.Is(err, ErrOwnershipMismatch) {
			t.Fatalf("spoofed callback accepted: %v", err)
		}
	})
	t.Run("wrong immutable user binding", func(t *testing.T) {
		client, calls := makeClient(42, []any{installation})
		if _, err := client.VerifySetupInstallationRaw(context.Background(), userToken, 42, testUser.ID+1, testOrganization.ID); !errors.Is(err, ErrOwnershipMismatch) {
			t.Fatalf("wrong user binding accepted: %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("installation list queried before /user binding, calls=%d", calls.Load())
		}
	})
	t.Run("duplicate provider rows", func(t *testing.T) {
		client, _ := makeClient(42, []any{installation, installation})
		if _, err := client.VerifySetupInstallationRaw(context.Background(), userToken, 42, testUser.ID, testOrganization.ID); !errors.Is(err, ErrProviderResponse) {
			t.Fatalf("duplicate installation accepted: %v", err)
		}
	})
	t.Run("wrong state-bound account", func(t *testing.T) {
		client, _ := makeClient(42, []any{installation})
		if _, err := client.VerifySetupInstallationRaw(context.Background(), userToken, 42, testUser.ID, 999); !errors.Is(err, ErrOwnershipMismatch) {
			t.Fatalf("wrong account binding accepted: %v", err)
		}
	})
	t.Run("account chosen on GitHub", func(t *testing.T) {
		client, _ := makeClient(42, []any{installation})
		verified, err := client.VerifySetupInstallationRaw(context.Background(), userToken, 42, testUser.ID, 0)
		if err != nil || verified.Installation.Account.ID != testOrganization.ID {
			t.Fatalf("GitHub-selected account not verified: %#v err=%v", verified, err)
		}
	})
}

func TestMintInstallationTokenUsesExplicitIDsPermissionsAndPostMintScopeCheck(t *testing.T) {
	cfg := validTestConfig(t)
	now := time.Date(2026, 8, 9, 6, 0, 0, 0, time.UTC)
	appToken := testAppToken()
	mintedRaw := "ghs_12345_stateless_installation_token_value"
	permissions := Permissions{"metadata": PermissionRead, "contents": PermissionRead}
	sortedRepositories := append([]RepositoryIdentity(nil), testRepositories...)
	slices.SortFunc(sortedRepositories, func(a, b RepositoryIdentity) int { return int(a.ID - b.ID) })
	remoteRepositories := []any{apiRepositoryFixture(sortedRepositories[0], "Organization"), apiRepositoryFixture(sortedRepositories[1], "Organization")}
	var calls atomic.Int64
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		switch request.URL.Path {
		case "/app/installations/42":
			if request.Header.Get("Authorization") != "Bearer "+appToken.Reveal() {
				t.Fatal("installation verification did not use App JWT")
			}
			return httpResponse(http.StatusOK, marshalFixture(t, apiInstallationFixture(cfg, 42, testOrganization, map[string]string{"metadata": "read", "contents": "read"})), nil), nil
		case "/app/installations/42/access_tokens":
			if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer "+appToken.Reveal() || request.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("mint request method/headers invalid: %s %#v", request.Method, request.Header)
			}
			body, _ := io.ReadAll(request.Body)
			if err := validateSingleJSON(body); err != nil {
				t.Fatalf("mint body: %v", err)
			}
			var raw map[string]json.RawMessage
			_ = json.Unmarshal(body, &raw)
			if len(raw) != 2 || raw["repository_ids"] == nil || raw["permissions"] == nil {
				t.Fatalf("implicit/broad mint body: %s", body)
			}
			var ids []int64
			var sentPermissions Permissions
			_ = json.Unmarshal(raw["repository_ids"], &ids)
			_ = json.Unmarshal(raw["permissions"], &sentPermissions)
			if !slices.Equal(ids, []int64{101, 102}) || !equalPermissions(sentPermissions, permissions) {
				t.Fatalf("mint scope ids=%v permissions=%v", ids, sentPermissions)
			}
			response := map[string]any{
				"token": mintedRaw, "expires_at": now.Add(time.Hour), "permissions": map[string]string{"metadata": "read", "contents": "read"},
				"repository_selection": "selected", "repositories": remoteRepositories,
			}
			return httpResponse(http.StatusCreated, marshalFixture(t, response), nil), nil
		case "/installation/repositories":
			if request.Header.Get("Authorization") != "Bearer "+mintedRaw {
				t.Fatal("post-mint scope was not checked through the minted token")
			}
			return httpResponse(http.StatusOK, marshalFixture(t, map[string]any{"total_count": 2, "repositories": remoteRepositories}), nil), nil
		default:
			t.Fatalf("unexpected token path %s", request.URL.Path)
			return nil, nil
		}
	})
	client, _ := NewClient(cfg, staticAppTokens{token: appToken}, transport, &fixedClock{now: now})
	token, err := client.MintInstallationToken(context.Background(), TokenRequest{
		InstallationID: 42, Account: testOrganization, Repositories: testRepositories, Permissions: permissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(token.RepositoryIDs(), []int64{101, 102}) || token.InstallationID() != 42 || token.Authorization().Reveal() != mintedRaw || calls.Load() != 3 {
		t.Fatalf("token scope=%v installation=%d calls=%d", token.RepositoryIDs(), token.InstallationID(), calls.Load())
	}
	returnedPermissions := token.Permissions()
	returnedPermissions["contents"] = PermissionWrite
	if token.Permissions()["contents"] != PermissionRead {
		t.Fatal("caller mutated token permission scope")
	}
	if got := fmt.Sprintf("%#v", token); strings.Contains(got, mintedRaw) {
		t.Fatalf("token formatting leaked credential: %s", got)
	}
}

func TestTokenRequestRejectsScopeBroadeningAndAmbiguousRepositoriesBeforeHTTP(t *testing.T) {
	cfg := validTestConfig(t)
	var calls atomic.Int64
	client, _ := NewClient(cfg, staticAppTokens{token: testAppToken()}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("must not call provider")
	}), &fixedClock{now: time.Now()})
	tests := []struct {
		name string
		req  TokenRequest
		want error
	}{
		{name: "write above configured read", req: TokenRequest{InstallationID: 42, Account: testOrganization, Repositories: testRepositories, Permissions: Permissions{"metadata": PermissionRead, "contents": PermissionWrite}}, want: ErrScopeMismatch},
		{name: "implicit metadata", req: TokenRequest{InstallationID: 42, Account: testOrganization, Repositories: testRepositories, Permissions: Permissions{"contents": PermissionRead}}, want: ErrInvalidTokenRequest},
		{name: "empty repository scope", req: TokenRequest{InstallationID: 42, Account: testOrganization, Permissions: Permissions{"metadata": PermissionRead}}, want: ErrInvalidTokenRequest},
		{name: "duplicate id", req: TokenRequest{InstallationID: 42, Account: testOrganization, Repositories: []RepositoryIdentity{testRepositories[0], testRepositories[0]}, Permissions: Permissions{"metadata": PermissionRead}}, want: ErrInvalidTokenRequest},
		{name: "ambiguous case-folded name", req: TokenRequest{InstallationID: 42, Account: testOrganization, Repositories: []RepositoryIdentity{{ID: 1, Name: "Service", OwnerID: 55, OwnerLogin: "kuberploy"}, {ID: 2, Name: "service", OwnerID: 55, OwnerLogin: "Kuberploy"}}, Permissions: Permissions{"metadata": PermissionRead}}, want: ErrInvalidTokenRequest},
		{name: "wrong owner", req: TokenRequest{InstallationID: 42, Account: testOrganization, Repositories: []RepositoryIdentity{{ID: 1, Name: "service", OwnerID: 99, OwnerLogin: "attacker"}}, Permissions: Permissions{"metadata": PermissionRead}}, want: ErrOwnershipMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := client.MintInstallationToken(context.Background(), test.req); !errors.Is(err, test.want) {
				t.Fatalf("want %v got %v", test.want, err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("provider called %d times for locally invalid scopes", calls.Load())
	}
}

func TestMintFailsClosedOnPermissionOrRepositoryScopeBroadening(t *testing.T) {
	cfg := validTestConfig(t)
	now := time.Date(2026, 8, 9, 6, 0, 0, 0, time.UTC)
	minted := "ghs_scope_check_token_value_123456"
	request := TokenRequest{InstallationID: 42, Account: testOrganization, Repositories: []RepositoryIdentity{testRepositories[1]}, Permissions: Permissions{"metadata": PermissionRead, "contents": PermissionRead}}
	tests := map[string]func(path string) (int, any){
		"remote installation too narrow": func(path string) (int, any) {
			if path == "/app/installations/42" {
				return http.StatusOK, apiInstallationFixture(cfg, 42, testOrganization, map[string]string{"metadata": "read"})
			}
			return http.StatusInternalServerError, map[string]any{}
		},
		"token permissions broadened": func(path string) (int, any) {
			if path == "/app/installations/42" {
				return http.StatusOK, apiInstallationFixture(cfg, 42, testOrganization, map[string]string{"metadata": "read", "contents": "read", "packages": "write"})
			}
			return http.StatusCreated, map[string]any{"token": minted, "expires_at": now.Add(time.Hour), "permissions": map[string]string{"metadata": "read", "contents": "read", "packages": "write"}, "repository_selection": "selected"}
		},
		"token repository broadened": func(path string) (int, any) {
			switch path {
			case "/app/installations/42":
				return http.StatusOK, apiInstallationFixture(cfg, 42, testOrganization, map[string]string{"metadata": "read", "contents": "read"})
			case "/app/installations/42/access_tokens":
				return http.StatusCreated, map[string]any{"token": minted, "expires_at": now.Add(time.Hour), "permissions": map[string]string{"metadata": "read", "contents": "read"}, "repository_selection": "selected"}
			default:
				return http.StatusOK, map[string]any{"total_count": 2, "repositories": []any{apiRepositoryFixture(testRepositories[1], "Organization"), apiRepositoryFixture(testRepositories[0], "Organization")}}
			}
		},
	}
	for name, responder := range tests {
		t.Run(name, func(t *testing.T) {
			client, _ := NewClient(cfg, staticAppTokens{token: testAppToken()}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				status, value := responder(request.URL.Path)
				return httpResponse(status, marshalFixture(t, value), nil), nil
			}), &fixedClock{now: now})
			if _, err := client.MintInstallationToken(context.Background(), request); !errors.Is(err, ErrScopeMismatch) {
				t.Fatalf("scope broadening result: %v", err)
			}
		})
	}
}

func scopedToken(now time.Time, shaPermissions Permissions) InstallationToken {
	return InstallationToken{
		ExpiresAt: now.Add(time.Hour), credential: newCredential("ghs_exact_ref_token_value_123456"), installationID: 77,
		repositoryIDs: []int64{101}, permissions: clonePermissions(shaPermissions),
	}
}

func TestResolveEventRefUsesExactRemoteCommitNotWebhookSHA(t *testing.T) {
	cfg := validTestConfig(t)
	now := time.Date(2026, 8, 9, 7, 0, 0, 0, time.UTC)
	webhookSHA := strings.Repeat("a", 40)
	remoteSHA := strings.Repeat("b", 40)
	repository := RepositoryIdentity{ID: 101, Name: "service", OwnerID: 55, OwnerLogin: "kuberploy"}
	var paths []string
	client, _ := NewClient(cfg, staticAppTokens{token: testAppToken()}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case "/repositories/101":
			return httpResponse(http.StatusOK, marshalFixture(t, apiRepositoryFixture(repository, "Organization")), nil), nil
		case "/repos/kuberploy/service/git/ref/heads/main":
			return httpResponse(http.StatusOK, marshalFixture(t, map[string]any{"ref": "refs/heads/main", "object": map[string]any{"type": "commit", "sha": remoteSHA}}), nil), nil
		default:
			t.Fatalf("non-exact ref endpoint used: %s", request.URL.Path)
			return nil, nil
		}
	}), &fixedClock{now: now})
	event := PushEvent{Ref: "refs/heads/main", UntrustedAfter: webhookSHA, Repository: repository, InstallationID: 77}
	resolved, err := client.ResolveEventRef(context.Background(), scopedToken(now, Permissions{"metadata": PermissionRead, "contents": PermissionRead}), event)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.CommitSHA != remoteSHA || resolved.CommitSHA == webhookSHA || !slices.Equal(paths, []string{"/repositories/101", "/repos/kuberploy/service/git/ref/heads/main"}) {
		t.Fatalf("resolved=%#v paths=%v", resolved, paths)
	}
}

func TestResolvedRefRejectsBuilderUnsupportedObjectFormat(t *testing.T) {
	cfg := validTestConfig(t)
	now := time.Date(2026, 8, 9, 7, 0, 0, 0, time.UTC)
	repository := RepositoryIdentity{ID: 101, Name: "service", OwnerID: 55, OwnerLogin: "kuberploy"}
	client, _ := NewClient(cfg, staticAppTokens{token: testAppToken()}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/repositories/101" {
			return httpResponse(http.StatusOK, marshalFixture(t, apiRepositoryFixture(repository, "Organization")), nil), nil
		}
		return httpResponse(http.StatusOK, marshalFixture(t, map[string]any{"ref": "refs/heads/main", "object": map[string]any{"type": "commit", "sha": strings.Repeat("c", 64)}}), nil), nil
	}), &fixedClock{now: now})
	_, err := client.ResolveRemoteRef(context.Background(), scopedToken(now, Permissions{"metadata": PermissionRead, "contents": PermissionRead}), repository, "refs/heads/main")
	if !errors.Is(err, ErrUnsupportedObjectFormat) {
		t.Fatalf("64-hex commit reached builder boundary: %v", err)
	}
}

func TestResolveRemoteRefPeelsAnnotatedTagAndRejectsDeletedPush(t *testing.T) {
	cfg := validTestConfig(t)
	now := time.Date(2026, 8, 9, 7, 0, 0, 0, time.UTC)
	repository := RepositoryIdentity{ID: 101, Name: "service", OwnerID: 55, OwnerLogin: "kuberploy"}
	tagSHA, commitSHA := strings.Repeat("d", 40), strings.Repeat("e", 40)
	client, _ := NewClient(cfg, staticAppTokens{token: testAppToken()}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/repositories/101":
			return httpResponse(http.StatusOK, marshalFixture(t, apiRepositoryFixture(repository, "Organization")), nil), nil
		case "/repos/kuberploy/service/git/ref/tags/v1.0.0":
			return httpResponse(http.StatusOK, marshalFixture(t, map[string]any{"ref": "refs/tags/v1.0.0", "object": map[string]any{"type": "tag", "sha": tagSHA}}), nil), nil
		case "/repos/kuberploy/service/git/tags/" + tagSHA:
			return httpResponse(http.StatusOK, marshalFixture(t, map[string]any{"sha": tagSHA, "object": map[string]any{"type": "commit", "sha": commitSHA}}), nil), nil
		default:
			t.Fatalf("unexpected tag path %s", request.URL.Path)
			return nil, nil
		}
	}), &fixedClock{now: now})
	token := scopedToken(now, Permissions{"metadata": PermissionRead, "contents": PermissionRead})
	resolved, err := client.ResolveRemoteRef(context.Background(), token, repository, "refs/tags/v1.0.0")
	if err != nil || resolved.CommitSHA != commitSHA {
		t.Fatalf("tag resolution=%#v err=%v", resolved, err)
	}
	if _, err = client.ResolveEventRef(context.Background(), token, PushEvent{Deleted: true, InstallationID: 77, Repository: repository}); !errors.Is(err, ErrRefDeleted) {
		t.Fatalf("deleted push resolution=%v", err)
	}
}

func TestListUserInstallationRepositoriesBindsExactAccountAndRejectsDuplicates(t *testing.T) {
	cfg := validTestConfig(t)
	account := AccountIdentity{ID: 700, Login: "kuberploy", Type: "Organization"}
	token := newCredential("opaque-user-access-token-value")
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://api.github.com/user/installations/42/repositories?page=1&per_page=100" || request.Header.Get("Authorization") != "Bearer opaque-user-access-token-value" {
			t.Fatalf("unexpected request %s auth=%q", request.URL, request.Header.Get("Authorization"))
		}
		return httpResponse(http.StatusOK, `{"total_count":2,"repositories":[{"id":2,"name":"two","full_name":"kuberploy/two","owner":{"id":700,"login":"kuberploy","type":"Organization"}},{"id":1,"name":"one","full_name":"kuberploy/one","owner":{"id":700,"login":"kuberploy","type":"Organization"}}]}`, nil), nil
	})
	client, err := NewClient(cfg, staticAppTokens{token: testAppToken()}, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := client.ListUserInstallationRepositories(context.Background(), token, 42, account)
	if err != nil || len(repositories) != 2 || repositories[0].ID != 1 || repositories[1].ID != 2 {
		t.Fatalf("repositories=%#v err=%v", repositories, err)
	}

	duplicateTransport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return httpResponse(http.StatusOK, `{"total_count":2,"repositories":[{"id":1,"name":"one","full_name":"kuberploy/one","owner":{"id":700,"login":"kuberploy","type":"Organization"}},{"id":1,"name":"one","full_name":"kuberploy/one","owner":{"id":700,"login":"kuberploy","type":"Organization"}}]}`, nil), nil
	})
	client, _ = NewClient(cfg, staticAppTokens{token: testAppToken()}, duplicateTransport, nil)
	if _, err = client.ListUserInstallationRepositories(context.Background(), token, 42, account); !errors.Is(err, ErrProviderResponse) {
		t.Fatalf("duplicate repository accepted: %v", err)
	}
}
