package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kuberploy/kuberploy/internal/httpapi"
	"github.com/kuberploy/kuberploy/internal/store/memory"
)

type valkeyHTTPReadiness struct {
	err    error
	called int
}

func (p *valkeyHTTPReadiness) Probe(context.Context) error {
	p.called++
	return p.err
}

func TestReadyzRequiresConfiguredValkeyDependency(t *testing.T) {
	probe := &valkeyHTTPReadiness{}
	server := httptest.NewServer(httpapi.New(httpapi.Options{
		Store: memory.New(), ValkeyReadiness: probe,
	}))
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || probe.called != 1 {
		response.Body.Close()
		t.Fatalf("healthy Valkey readiness status=%d calls=%d", response.StatusCode, probe.called)
	}
	response.Body.Close()

	probe.err = errors.New("Valkey connection refused")
	response, err = http.Get(server.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	problem := decode[httpapi.Problem](t, response)
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "ValkeyUnavailable" || probe.called != 2 {
		t.Fatalf("unhealthy Valkey readiness status=%d calls=%d problem=%#v", response.StatusCode, probe.called, problem)
	}
}

func TestReadyzDoesNotProbeOptionalFeatureRuntimes(t *testing.T) {
	probe := &valkeyHTTPReadiness{err: errors.New("optional runtime unavailable")}
	server := httptest.NewServer(httpapi.New(httpapi.Options{
		Store:                             memory.New(),
		BuildReadiness:                    probe,
		BuildLogReadiness:                 probe,
		RuntimeReadiness:                  probe,
		GitProjectionReadiness:            probe,
		ArgoReadiness:                     probe,
		RuntimeSecretReadiness:            probe,
		RegistryPullReadiness:             probe,
		EdgeReadiness:                     probe,
		RegistryReadiness:                 probe,
		CertificateReadiness:              probe,
		AutoDeployReadiness:               probe,
		CertificateIssuerRuntimeReadiness: probe,
	}))
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || probe.called != 0 {
		t.Fatalf("optional feature runtime affected API readiness: status=%d calls=%d", response.StatusCode, probe.called)
	}
}
