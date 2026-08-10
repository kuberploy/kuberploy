package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const projectedOAuthClientSecretKey = "oauth-client-secret"

var oauthCodePattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{16,512}$`)

type CodeExchanger interface {
	ExchangeCode(context.Context, string) (Credential, error)
}

type OAuthCodeExchanger struct {
	clientID     string
	clientSecret SecretRef
	redirectURI  string
	secrets      SecretReader
	http         *http.Client
}

// NewProjectedOAuthCodeExchanger uses GitHub's fixed token endpoint and a
// fixed projected Kubernetes Secret key. Client-secret bytes are never read
// from environment variables and are erased after each exchange.
func NewProjectedOAuthCodeExchanger(config Config, redirectURI string) (*OAuthCodeExchanger, error) {
	return newOAuthCodeExchanger(config, redirectURI, NewProjectedSecretReader(), nil)
}

func newOAuthCodeExchanger(config Config, redirectURI string, secrets SecretReader, transport http.RoundTripper) (*OAuthCodeExchanger, error) {
	if err := config.Validate(); err != nil || secrets == nil || !validOAuthRedirectURI(redirectURI) {
		return nil, ErrInvalidConfig
	}
	client := &http.Client{Transport: transport, Timeout: config.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	if transport == nil {
		client.Transport = defaultHTTPTransport(config.RequestTimeout)
	}
	return &OAuthCodeExchanger{clientID: config.ClientID, clientSecret: SecretRef{Name: projectedRuntimeSecret, Key: projectedOAuthClientSecretKey},
		redirectURI: redirectURI, secrets: secrets, http: client}, nil
}

func validOAuthRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		u.EscapedPath() != "/v1/github/installations/callback" {
		return false
	}
	if port := u.Port(); port != "" {
		value, portErr := strconv.Atoi(port)
		return portErr == nil && value >= 1 && value <= 65535
	}
	return true
}

func (e *OAuthCodeExchanger) ExchangeCode(ctx context.Context, code string) (Credential, error) {
	if e == nil || e.http == nil || e.secrets == nil || !oauthCodePattern.MatchString(code) {
		return Credential{}, ErrTransport
	}
	secret, err := e.secrets.ReadSecret(ctx, e.clientSecret)
	if err != nil {
		zeroBytes(secret)
		return Credential{}, ErrSecretUnavailable
	}
	defer zeroBytes(secret)
	if len(secret) < 16 || len(secret) > 512 || !validASCIISecret(secret) {
		return Credential{}, ErrSecretUnavailable
	}
	form := url.Values{"client_id": {e.clientID}, "client_secret": {string(secret)}, "code": {code}, "redirect_uri": {e.redirectURI}}
	body := []byte(form.Encode())
	defer zeroBytes(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", bytes.NewReader(body))
	if err != nil {
		return Credential{}, ErrTransport
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", defaultUserAgent)
	response, err := e.http.Do(request)
	if err != nil {
		return Credential{}, ErrTransport
	}
	if response == nil || response.Body == nil {
		return Credential{}, ErrTransport
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.CopyN(io.Discard, response.Body, 64<<10)
		return Credential{}, classifyAPIError(response.StatusCode, response.Header, systemClock{}.Now())
	}
	encoded, err := readResponseBounded(response.Body, 64<<10)
	if err != nil {
		return Credential{}, ErrProviderResponse
	}
	defer zeroBytes(encoded)
	var result struct {
		AccessToken           string `json:"access_token"`
		TokenType             string `json:"token_type"`
		Scope                 string `json:"scope"`
		ExpiresIn             int64  `json:"expires_in,omitempty"`
		RefreshToken          string `json:"refresh_token,omitempty"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in,omitempty"`
		Error                 string `json:"error,omitempty"`
		ErrorDescription      string `json:"error_description,omitempty"`
		ErrorURI              string `json:"error_uri,omitempty"`
	}
	if err = validateSingleJSON(encoded); err != nil {
		return Credential{}, ErrProviderResponse
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&result); err != nil || result.Error != "" || !strings.EqualFold(result.TokenType, "bearer") || result.Scope != "" ||
		result.ExpiresIn < 0 || result.RefreshTokenExpiresIn < 0 || result.RefreshToken != "" && !validRawCredential(result.RefreshToken) {
		result.AccessToken, result.RefreshToken = "", ""
		return Credential{}, ErrProviderResponse
	}
	credential, err := credentialFromRaw(result.AccessToken)
	result.AccessToken, result.RefreshToken = "", ""
	if err != nil {
		return Credential{}, ErrProviderResponse
	}
	return credential, nil
}

func validASCIISecret(value []byte) bool {
	for _, b := range value {
		if b <= 0x20 || b >= 0x7f || strings.ContainsRune("&=+%#", rune(b)) {
			return false
		}
	}
	return true
}
