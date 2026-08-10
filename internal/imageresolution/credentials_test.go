package imageresolution

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/imagepull"
)

type credentialMaterialReader struct {
	value []byte
	err   error
}

func (r *credentialMaterialReader) ReadDockerConfig(context.Context, imagepull.Profile) ([]byte, error) {
	return append([]byte(nil), r.value...), r.err
}

func TestProjectedCredentialSourceExtractsOnlyExactProfileAuthorization(t *testing.T) {
	for name, fixture := range map[string]struct {
		raw, want string
	}{
		"encoded basic":  {`{"auths":{"registry.example.test:5000":{"auth":"dXNlcjpwYXNz"}}}`, "Basic dXNlcjpwYXNz"},
		"user password":  {`{"auths":{"registry.example.test:5000":{"username":"robot","password":"secret"}}}`, "Basic cm9ib3Q6c2VjcmV0"},
		"identity token": {`{"auths":{"registry.example.test:5000":{"identitytoken":"opaque-token"}}}`, "Bearer opaque-token"},
	} {
		t.Run(name, func(t *testing.T) {
			source := &ProjectedCredentialSource{Reader: &credentialMaterialReader{value: []byte(fixture.raw)}}
			authorization, err := source.Authorization(t.Context(), resolutionProfile())
			if err != nil {
				t.Fatal(err)
			}
			header, ok := authorization.header()
			if !ok || header != fixture.want {
				t.Fatalf("header=%q ok=%v", header, ok)
			}
			issued := authorization.value
			authorization.destroy()
			for _, value := range issued {
				if value != 0 {
					t.Fatal("authorization bytes retained")
				}
			}
		})
	}
}

func TestProjectedCredentialSourceFailsClosedWithoutLeakingMaterial(t *testing.T) {
	for name, reader := range map[string]*credentialMaterialReader{
		"wrong host": {value: []byte(`{"auths":{"evil.example.test":{"identitytoken":"secret-token-marker"}}}`)},
		"provider":   {err: errors.New("secret-provider-marker")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (&ProjectedCredentialSource{Reader: reader}).Authorization(t.Context(), resolutionProfile())
			if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
