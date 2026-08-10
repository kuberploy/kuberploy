package httpapi_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

func TestAuditTimelineIsBoundedSafeAndNoStore(t *testing.T) {
	f := newAPI(t)
	admin := f.bootstrap()
	targetID := "11111111-1111-4111-8111-111111111111"
	f.store.AddAuditEvent(domain.AuditEvent{ID: "22222222-2222-4222-8222-222222222222",
		ActorID: admin.ID, Action: "deployment.config.accepted", TargetType: "deployment",
		TargetID: targetID, Outcome: "accepted", RequestID: "request-safe", CreatedAt: time.Now().UTC()})
	response := f.request(http.MethodGet, "/v1/audit-events?targetType=deployment&targetId="+targetID+"&limit=1", "", nil)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q", response.StatusCode, response.Header.Get("Cache-Control"))
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !strings.Contains(string(body), `"outcome":"accepted"`) ||
		strings.Contains(strings.ToLower(string(body)), "detail") ||
		strings.Contains(strings.ToLower(string(body)), "secret") {
		t.Fatalf("unsafe audit response: %s", body)
	}
	response = f.request(http.MethodGet, "/v1/audit-events?targetType=deployment", "", nil)
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unpaired target status=%d", response.StatusCode)
	}
}
