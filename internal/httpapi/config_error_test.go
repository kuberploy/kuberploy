package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kuberploy/kuberploy/internal/certificates"
	"github.com/kuberploy/kuberploy/internal/store"
)

func TestMappedDeploymentConfigTransactionErrorPreservesRetryableCertificateFailure(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want int
		code string
	}{
		{name: "provider unavailable", err: certificates.ErrUnavailable, want: http.StatusServiceUnavailable, code: "CertificateReferenceRuntimeUnavailable"},
		{name: "observation unavailable", err: certificates.ErrObservationUnavailable, want: http.StatusServiceUnavailable, code: "CertificateReferenceRuntimeUnavailable"},
		{name: "normal store mapping", err: store.ErrPreconditionFailed, want: http.StatusPreconditionFailed, code: "PreconditionFailed"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPut, "/v1/deployments/test/config", nil)
			mappedDeploymentConfigTransactionError(response, request, testCase.err)
			var problem Problem
			if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
				t.Fatal(err)
			}
			if response.Code != testCase.want || problem.Code != testCase.code {
				t.Fatalf("status=%d code=%q problem=%#v", response.Code, problem.Code, problem)
			}
		})
	}
}
