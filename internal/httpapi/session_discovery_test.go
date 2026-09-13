package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

func TestSessionDiscoveryAnonymousDoesNotWeakenProtectedMe(t *testing.T) {
	f := newAPI(t)
	r := f.request("GET", "/v1/auth/session", "", nil)
	assertSessionDiscoveryHeaders(t, r)
	got := decode[map[string]any](t, r)
	if r.StatusCode != http.StatusOK || len(got) != 1 || got["principal"] != nil {
		t.Fatalf("anonymous discovery status=%d body=%v", r.StatusCode, got)
	}
	r = f.request("GET", "/v1/me", "", nil)
	defer r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("protected me status=%d", r.StatusCode)
	}
}

func TestSessionDiscoveryReturnsCurrentHumanPrincipalAndObservesLogout(t *testing.T) {
	f := newAPI(t)
	f.bootstrap()
	me := decode[map[string]any](t, f.request("GET", "/v1/me", "", nil))
	r := f.request("GET", "/v1/auth/session", "", nil)
	assertSessionDiscoveryHeaders(t, r)
	got := decode[struct {
		Principal map[string]any `json:"principal"`
	}](t, r)
	if r.StatusCode != http.StatusOK || !reflect.DeepEqual(got.Principal, me) {
		t.Fatal("discovery did not return the current protected human principal")
	}
	r = f.request("POST", "/v1/auth/logout", "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status=%d", r.StatusCode)
	}
	r = f.request("GET", "/v1/auth/session", "", nil)
	got = decode[struct {
		Principal map[string]any `json:"principal"`
	}](t, r)
	if r.StatusCode != http.StatusOK || got.Principal != nil {
		t.Fatal("logout retained a discovered principal")
	}
}

func TestSessionDiscoveryInvalidCookiesAndCurrentSessionValidation(t *testing.T) {
	raw := make([]byte, 32)
	value := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256(raw)
	for _, scenario := range []string{"missing", "malformed", "wrong length", "noncanonical", "duplicate", "unknown", "expired", "revoked", "deleted user", "service account", "unsupported role", "valid developer"} {
		t.Run(scenario, func(t *testing.T) {
			st := memory.New()
			u := domain.User{ID: "session-user", Email: "session@example.test", Role: "developer", Issuer: "kuberploy:invitation", GrantRevision: 1}
			expires := time.Now().Add(time.Hour)
			if scenario == "expired" {
				expires = time.Now().Add(-time.Hour)
			}
			if scenario == "deleted user" {
				u.Issuer = "kuberploy:deleted"
			}
			if scenario == "service account" {
				u.Issuer = "kuberploy:service-account"
			}
			if scenario == "unsupported role" {
				u.Role = "other"
			}
			if scenario != "unknown" {
				if err := st.BootstrapAdmin(context.Background(), u, "test-only-password-hash", hash[:], expires); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "revoked" {
				if err := st.RevokeSession(context.Background(), hash[:]); err != nil {
					t.Fatal(err)
				}
			}
			req := httptest.NewRequest("GET", "/v1/auth/session", nil)
			cookie := value
			switch scenario {
			case "malformed":
				cookie = "invalid!"
			case "wrong length":
				cookie = "AA"
			case "noncanonical":
				cookie = strings.Repeat("A", 42) + "B"
			}
			if scenario != "missing" {
				req.AddCookie(&http.Cookie{Name: "kuberploy_session", Value: cookie})
			}
			if scenario == "duplicate" {
				req.AddCookie(&http.Cookie{Name: "kuberploy_session", Value: value})
			}
			w := httptest.NewRecorder()
			httpapi.New(httpapi.Options{Store: st}).ServeHTTP(w, req)
			r := w.Result()
			assertSessionDiscoveryHeaders(t, r)
			got := decode[struct {
				Principal *domain.User `json:"principal"`
			}](t, r)
			if r.StatusCode != http.StatusOK {
				t.Fatalf("discovery status=%d", r.StatusCode)
			}
			if scenario == "valid developer" {
				if got.Principal == nil || got.Principal.ID != u.ID {
					t.Fatal("valid developer session was not discovered")
				}
			} else if got.Principal != nil {
				t.Fatal("invalid session exposed a principal")
			}
		})
	}
}

type sessionDiscoveryErrorStore struct {
	*memory.Store
	calls int
}

func TestSessionDiscoveryRevokesPrincipalAfterCurrentGrantChangeOrDeletion(t *testing.T) {
	for _, change := range []string{"grant change", "user deletion"} {
		t.Run(change, func(t *testing.T) {
			f := newAPI(t)
			admin := f.bootstrap()
			ctx := context.Background()
			token := sha256.Sum256([]byte("invitation-test"))
			raw := sha256.Sum256([]byte("session-test"))
			hash := sha256.Sum256(raw[:])
			if _, err := f.store.CreateUserInvitation(ctx, admin.ID, "invited@example.test", token[:], time.Now().Add(time.Hour), "test"); err != nil {
				t.Fatal(err)
			}
			user, err := f.store.AcceptUserInvitation(ctx, token[:], "Invited user", "test-only-hash", hash[:], nil, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			read := func() *domain.User {
				t.Helper()
				req, err := http.NewRequest("GET", f.server.URL+"/v1/auth/session", nil)
				if err != nil {
					t.Fatal(err)
				}
				req.AddCookie(&http.Cookie{Name: "kuberploy_session", Value: base64.RawURLEncoding.EncodeToString(raw[:])})
				r, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				if r.StatusCode != http.StatusOK {
					r.Body.Close()
					t.Fatalf("discovery status=%d", r.StatusCode)
				}
				return decode[struct {
					Principal *domain.User `json:"principal"`
				}](t, r).Principal
			}
			if got := read(); got == nil || got.ID != user.ID {
				t.Fatal("new session missing before current-access change")
			}
			if change == "user deletion" {
				if _, err := f.store.DeleteUser(ctx, admin.ID, user.ID, user.Email, "delete", "delete", "test"); err != nil {
					t.Fatal(err)
				}
			} else {
				team, err := f.store.CreateTeam(ctx, admin.ID, "team", "team", "test", domain.CreateTeam{Name: "Session team", Slug: "session-team"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.AddTeamMember(ctx, admin.ID, team.Value.ID, "member", "member", "test", domain.AddTeamMember{UserID: user.ID, Role: "member"}); err != nil {
					t.Fatal(err)
				}
			}
			if read() != nil {
				t.Fatal("principal survived a current-access invalidation")
			}
		})
	}
}

func (s *sessionDiscoveryErrorStore) UserBySession(context.Context, []byte, time.Time) (domain.User, error) {
	s.calls++
	return domain.User{}, errors.New("private database connection details")
}

func TestSessionDiscoveryRejectsAuthorizationBeforeCookieLookupAndKeepsStoreErrors(t *testing.T) {
	for _, authorization := range [][]string{{""}, {"Bearer invalid"}, {"Basic invalid"}, {"Bearer one", "Bearer two"}, nil} {
		st := &sessionDiscoveryErrorStore{Store: memory.New()}
		req := httptest.NewRequest("GET", "/v1/auth/session", nil)
		req.AddCookie(&http.Cookie{Name: "kuberploy_session", Value: base64.RawURLEncoding.EncodeToString(make([]byte, 32))})
		if authorization != nil {
			req.Header["Authorization"] = authorization
		}
		w := httptest.NewRecorder()
		httpapi.New(httpapi.Options{Store: st}).ServeHTTP(w, req)
		r := w.Result()
		assertSessionDiscoveryHeaders(t, r)
		var problem httpapi.Problem
		if err := json.NewDecoder(r.Body).Decode(&problem); err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if authorization != nil {
			if r.StatusCode != http.StatusUnauthorized || st.calls != 0 {
				t.Fatalf("Authorization bypass status=%d lookups=%d", r.StatusCode, st.calls)
			}
		} else if r.StatusCode != http.StatusServiceUnavailable || problem.Code != "DatabaseUnavailable" || st.calls != 1 {
			t.Fatalf("store error misrepresented: status=%d code=%s lookups=%d", r.StatusCode, problem.Code, st.calls)
		}
		if strings.Contains(w.Body.String(), "private database") {
			t.Fatal("database details disclosed")
		}
	}
}

func assertSessionDiscoveryHeaders(t *testing.T, r *http.Response) {
	t.Helper()
	if r.Header.Get("Cache-Control") != "private, no-store" || r.Header.Get("X-Content-Type-Options") != "nosniff" || len(r.Cookies()) != 0 || r.Header.Get("X-CSRF-Token") != "" {
		t.Fatalf("discovery privacy headers incorrect; status=%d", r.StatusCode)
	}
}
