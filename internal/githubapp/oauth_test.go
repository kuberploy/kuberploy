package githubapp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestOAuthCodeExchangerUsesFixedEndpointAndProjectedSecret(t *testing.T) {
	cfg := validTestConfig(t)
	secretRef := SecretRef{Name: projectedRuntimeSecret, Key: projectedOAuthClientSecretKey}
	secrets := &mapSecrets{values: map[SecretRef][]byte{secretRef: []byte("oauth-client-secret-value")}}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "https://github.com/login/oauth/access_token" || request.Header.Get("Authorization") != "" {
			t.Fatalf("request=%s %s auth=%q", request.Method, request.URL, request.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(request.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil || form.Get("client_id") != cfg.ClientID || form.Get("client_secret") != "oauth-client-secret-value" ||
			form.Get("code") != "oauth-code-0123456789" || form.Get("redirect_uri") != "https://kuberploy.example.test/v1/github/installations/callback" {
			t.Fatalf("form=%v err=%v", form, err)
		}
		return httpResponse(http.StatusOK, `{"access_token":"opaque-user-access-token-value","token_type":"bearer","scope":"","expires_in":28800}`, nil), nil
	})
	exchanger, err := newOAuthCodeExchanger(cfg, "https://kuberploy.example.test/v1/github/installations/callback", secrets, transport)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := exchanger.ExchangeCode(context.Background(), "oauth-code-0123456789")
	if err != nil || credential.Reveal() != "opaque-user-access-token-value" {
		t.Fatalf("credential=%v err=%v", credential, err)
	}
	for _, b := range secrets.last {
		if b != 0 {
			t.Fatal("projected OAuth secret was not erased")
		}
	}
}

func TestOAuthCodeExchangerRejectsRedirectAndAmbiguousResponseWithoutLeak(t *testing.T) {
	cfg := validTestConfig(t)
	marker := "oauth-client-secret-marker"
	for name, response := range map[string]*http.Response{
		"redirect":  httpResponse(http.StatusFound, marker, map[string]string{"Location": "https://attacker.invalid/"}),
		"duplicate": httpResponse(http.StatusOK, `{"access_token":"opaque-user-access-token-value","access_token":"attacker-token-value-opaque","token_type":"bearer","scope":""}`, nil),
		"unknown":   httpResponse(http.StatusOK, `{"access_token":"opaque-user-access-token-value","token_type":"bearer","scope":"","credential":"`+marker+`"}`, nil),
	} {
		t.Run(name, func(t *testing.T) {
			secrets := &mapSecrets{values: map[SecretRef][]byte{{Name: projectedRuntimeSecret, Key: projectedOAuthClientSecretKey}: []byte(marker)}}
			exchanger, _ := newOAuthCodeExchanger(cfg, "https://kuberploy.example.test/v1/github/installations/callback", secrets, roundTripFunc(func(*http.Request) (*http.Response, error) { return response, nil }))
			_, err := exchanger.ExchangeCode(context.Background(), "oauth-code-0123456789")
			if err == nil || strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "attacker") {
				t.Fatalf("unsafe error=%v", err)
			}
		})
	}
	if _, err := newOAuthCodeExchanger(cfg, "http://kuberploy.example.test/v1/github/installations/callback", &mapSecrets{}, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("HTTP callback accepted: %v", err)
	}
	if _, err := newOAuthCodeExchanger(cfg, "https://kuberploy.example.test:99999/v1/github/installations/callback", &mapSecrets{}, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid callback port accepted: %v", err)
	}
}
