package buildlogs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type blockingBuildLogAuditor struct{}

func (blockingBuildLogAuditor) AuditBuildLogAccess(ctx context.Context, _ AuditEvent) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestSnapshotDerivesIdentityRedactsAndNeverExposesKubernetesNames(t *testing.T) {
	service, _, auditor, client := newTestService(t)
	client.logs = []string{"2026-08-09T11:59:59Z \x1b[31mtoken=ghp_123456789012345678901234567890\x1b[0m on build-pod-aaaaaaaa in kuberploy-build-dind\n"}
	snapshot, err := service.Snapshot(t.Context(), snapshotRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Lines) != 1 || snapshot.Lines[0].Message != "token=[REDACTED] on [REDACTED KUBERNETES ID] in [REDACTED KUBERNETES ID]" || snapshot.Source.ID == "" || snapshot.Source.ID != snapshot.Lines[0].Source.ID {
		t.Fatalf("unsafe or incomplete snapshot: %#v", snapshot)
	}
	if snapshot.Lines[0].Cursor == nil || snapshot.Lines[0].Cursor.Fingerprint == "" {
		t.Fatal("timestamped line did not receive an opaque reconnect cursor")
	}
	encoded, _ := json.Marshal(snapshot)
	for _, forbidden := range []string{"kuberploy-build-dind", "kuberploy-build-", "build-pod-aaaaaaaa", "agent", "attacker"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("Kubernetes or stored-log identity leaked: %s", encoded)
		}
	}
	if len(auditor.events) != 1 || auditor.events[0].Action != "build.logs.snapshot" || auditor.events[0].AttemptID != testAttemptID {
		t.Fatalf("audit=%#v", auditor.events)
	}
	if len(client.queries) != 1 || len(client.opens) != 1 || client.queries[0].Namespace != "kuberploy-build-dind" || client.opens[0].UID != "55555555-5555-4555-8555-555555555555" {
		t.Fatalf("derived Kubernetes calls were not exact: queries=%#v opens=%#v", client.queries, client.opens)
	}
}

func TestSnapshotFailsClosedBeforeKubernetesWhenAuthorizationOrAuditFails(t *testing.T) {
	t.Run("authorization", func(t *testing.T) {
		service, resolver, auditor, client := newTestService(t)
		resolver.resolveErr = ErrNotFound
		if _, err := service.Snapshot(t.Context(), snapshotRequest()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err=%v", err)
		}
		if len(auditor.events) != 0 || client.getJobs != 0 {
			t.Fatal("unauthorized request reached audit or Kubernetes")
		}
	})
	t.Run("audit", func(t *testing.T) {
		service, _, auditor, client := newTestService(t)
		auditor.err = errAudit
		if _, err := service.Snapshot(t.Context(), snapshotRequest()); !errors.Is(err, errAudit) {
			t.Fatalf("err=%v", err)
		}
		if client.getJobs != 0 {
			t.Fatal("unaudited request reached Kubernetes")
		}
	})
}

func TestSnapshotKeepsScopeSentinelAndRecordsFailingKubernetesStage(t *testing.T) {
	service, _, _, client := newTestService(t)
	client.openErr = ErrScopeViolation

	_, err := service.Snapshot(t.Context(), snapshotRequest())
	if !errors.Is(err, ErrScopeViolation) || !strings.Contains(err.Error(), "open builder-agent logs") {
		t.Fatalf("scope stage was not preserved: %v", err)
	}
}

func TestSnapshotRejectsResolverScopeConfusion(t *testing.T) {
	service, resolver, auditor, client := newTestService(t)
	resolver.authorized.ApplicationID = "66666666-6666-4666-8666-666666666666"
	if _, err := service.Snapshot(t.Context(), snapshotRequest()); !errors.Is(err, ErrScopeViolation) || !strings.Contains(err.Error(), "resolve attempt binding") {
		t.Fatalf("err=%v", err)
	}
	if len(auditor.events) != 0 || client.getJobs != 0 {
		t.Fatal("scope-confused resolver result reached audit or Kubernetes")
	}
}

func TestSnapshotRejectsJobSpecAndPodOwnerSubstitution(t *testing.T) {
	t.Run("job spec", func(t *testing.T) {
		service, _, _, client := newTestService(t)
		client.job["spec"].(map[string]any)["parallelism"] = int64(2)
		if _, err := service.Snapshot(t.Context(), snapshotRequest()); !errors.Is(err, ErrScopeViolation) {
			t.Fatalf("err=%v", err)
		}
		if len(client.opens) != 0 {
			t.Fatal("mutated Job reached logs")
		}
	})
	t.Run("pod owner", func(t *testing.T) {
		service, _, _, client := newTestService(t)
		owner := client.pods[0]["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)
		owner["uid"] = "77777777-7777-4777-8777-777777777777"
		if _, err := service.Snapshot(t.Context(), snapshotRequest()); !errors.Is(err, ErrScopeViolation) {
			t.Fatalf("err=%v", err)
		}
		if len(client.opens) != 0 {
			t.Fatal("foreign Pod reached logs")
		}
	})
	t.Run("multiple pods", func(t *testing.T) {
		service, _, _, client := newTestService(t)
		client.pods = append(client.pods, cloneObject(client.pods[0]))
		if _, err := service.Snapshot(t.Context(), snapshotRequest()); !errors.Is(err, ErrScopeViolation) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestSnapshotClosesPodReplacementRaceAndIgnoresStoredLogReference(t *testing.T) {
	service, resolver, _, client := newTestService(t)
	resolver.authorized.Attempt.LogReference = "k8s://victim/pods/private/containers/agent"
	client.onList = func(client *fakeKubernetes) {
		client.pods[0]["metadata"].(map[string]any)["uid"] = "88888888-8888-4888-8888-888888888888"
	}
	if _, err := service.Snapshot(t.Context(), snapshotRequest()); !errors.Is(err, ErrGone) {
		t.Fatalf("Pod replacement was not fenced: %v", err)
	}
	if len(client.opens) != 0 {
		t.Fatal("replacement race reached log subresource")
	}
}

func TestSnapshotPreviousAndBoundsAreFailClosed(t *testing.T) {
	t.Run("previous unavailable", func(t *testing.T) {
		service, _, _, client := newTestService(t)
		client.pods[0]["status"].(map[string]any)["containerStatuses"] = []any{map[string]any{"name": "agent", "restartCount": int64(0)}}
		request := snapshotRequest()
		request.Options.Previous = true
		if _, err := service.Snapshot(t.Context(), request); !errors.Is(err, ErrPreviousUnavailable) {
			t.Fatalf("err=%v", err)
		}
	})
	for _, mutate := range []func(*SnapshotRequest){
		func(r *SnapshotRequest) { r.Options.TailLines = 2_001 },
		func(r *SnapshotRequest) { r.Options.LimitBytes = 5<<20 + 1 },
		func(r *SnapshotRequest) {
			future := time.Date(2026, 8, 9, 13, 2, 0, 0, time.UTC)
			r.Options.SinceTime = &future
		},
		func(r *SnapshotRequest) { r.RequestID = "bad request id" },
		func(r *SnapshotRequest) { r.Access.AttemptID = "../pods/victim" },
	} {
		service, _, _, _ := newTestService(t)
		request := snapshotRequest()
		mutate(&request)
		if _, err := service.Snapshot(t.Context(), request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("unsafe bounded option accepted: %v", err)
		}
	}
}

func TestSnapshotTruncatesLongLinesAndBoundsRawBytes(t *testing.T) {
	service, _, _, client := newTestService(t)
	service.config.MaxLineBytes = 16
	service.config.DefaultLimitBytes = 32
	client.logs = []string{strings.Repeat("x", 100) + "\n"}
	snapshot, err := service.Snapshot(t.Context(), snapshotRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Lines) != 1 || len(snapshot.Lines[0].Message) > 16 || !snapshot.Lines[0].Truncated || !snapshot.Truncated {
		t.Fatalf("unbounded long line: %#v", snapshot)
	}
}

func TestServiceRejectsAndRechecksInsecureTransport(t *testing.T) {
	authorized, job, pod := buildLogFixture(t)
	client := &fakeKubernetes{security: ClientSecurity{InsecureSkipTLSVerify: true}, job: job, pods: []map[string]any{pod}}
	if _, err := NewService(&fakeResolver{authorized: authorized}, &fakeAuditor{}, client, nil, DefaultConfig()); !errors.Is(err, ErrInsecureTransport) {
		t.Fatalf("insecure client accepted: %v", err)
	}
	service, _, _, client := newTestService(t)
	client.security = ClientSecurity{TLSVerified: true, InsecureSkipTLSVerify: true}
	if _, err := service.Snapshot(t.Context(), snapshotRequest()); !errors.Is(err, ErrInsecureTransport) {
		t.Fatalf("transport downgrade accepted: %v", err)
	}
}

func TestServiceRejectsOperatorConfiguredUnboundedSessions(t *testing.T) {
	authorized, job, pod := buildLogFixture(t)
	resolver, auditor := &fakeResolver{authorized: authorized}, &fakeAuditor{}
	client := &fakeKubernetes{security: ClientSecurity{TLSVerified: true}, job: job, pods: []map[string]any{pod}}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.MaxTailLines = 2_001 },
		func(c *Config) { c.MaxSnapshotBytes = 5<<20 + 1 },
		func(c *Config) { c.SnapshotTimeout = 2*time.Minute + 1 },
		func(c *Config) { c.MaxFollowBytes = 100<<20 + 1 },
		func(c *Config) { c.MaxFollowDuration = time.Hour + 1 },
		func(c *Config) { c.FollowBuffer = 4_097 },
		func(c *Config) { c.DedupeEntries = 65_537 },
	} {
		config := DefaultConfig()
		mutate(&config)
		if _, err := NewService(resolver, auditor, client, nil, config); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("unbounded operator config accepted: %#v err=%v", config, err)
		}
	}
}

func TestSnapshotBoundsAuthorizationAuditSetupTime(t *testing.T) {
	authorized, job, pod := buildLogFixture(t)
	config := DefaultConfig()
	config.SnapshotTimeout = 5 * time.Millisecond
	client := &fakeKubernetes{security: ClientSecurity{TLSVerified: true}, job: job, pods: []map[string]any{pod}}
	service, err := NewService(&fakeResolver{authorized: authorized}, blockingBuildLogAuditor{}, client, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Snapshot(t.Context(), snapshotRequest()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unbounded setup did not time out: %v", err)
	}
	if client.getJobs != 0 {
		t.Fatal("timed-out audit reached Kubernetes")
	}
}
