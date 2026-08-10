package buildlogs

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func followConfig() Config {
	config := DefaultConfig()
	config.RevalidateInterval = 10 * time.Millisecond
	config.RediscoverInterval = 10 * time.Millisecond
	config.HeartbeatInterval = 10 * time.Millisecond
	config.ReconnectDelay = 5 * time.Millisecond
	config.MaxFollowDuration = time.Second
	return config
}

func followRequest() FollowRequest {
	return FollowRequest{Access: AccessRequest{ActorID: testActorID, AttemptID: testAttemptID}, RequestID: "req-follow", Options: LogOptions{Follow: true}}
}

func TestFollowRevalidatesAuthorizationAndClosesReader(t *testing.T) {
	service, resolver, auditor, client := newTestServiceWithConfig(t, followConfig())
	reader, writer := io.Pipe()
	client.readers = []io.ReadCloser{reader}
	stream, err := service.Follow(t.Context(), followRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	resolver.setRevalidateError(ErrUnauthorized)
	event := waitStreamEvent(t, stream, time.Second, func(event StreamEvent) bool {
		return event.Type == StreamTerminal && event.Terminal != nil && event.Terminal.Code == "AuthorizationRevoked"
	})
	if strings.Contains(event.Terminal.Detail, testAttemptID) || len(auditor.events) != 1 || auditor.events[0].Action != "build.logs.follow" {
		t.Fatalf("unsafe terminal/audit: event=%#v audit=%#v", event, auditor.events)
	}
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		t.Fatal("authorization revocation did not stop session")
	}
	if _, err = writer.Write([]byte("should fail")); err == nil {
		t.Fatal("revoked stream reader remained open")
	}
	_ = writer.Close()
}

func TestFollowTerminatesWhenExactJobIdentityChanges(t *testing.T) {
	config := followConfig()
	config.RevalidateInterval = time.Hour
	service, _, _, client := newTestServiceWithConfig(t, config)
	reader, writer := io.Pipe()
	client.readers = []io.ReadCloser{reader}
	stream, err := service.Follow(t.Context(), followRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	client.mu.Lock()
	client.job["metadata"].(map[string]any)["uid"] = "99999999-9999-4999-8999-999999999999"
	client.mu.Unlock()
	event := waitStreamEvent(t, stream, time.Second, func(event StreamEvent) bool {
		return event.Type == StreamTerminal && event.Terminal != nil
	})
	if event.Terminal.Code != "SourceReplacedOrDeleted" {
		t.Fatalf("replacement terminal=%#v", event.Terminal)
	}
	_ = writer.Close()
}

func TestFollowReportsBackpressureGap(t *testing.T) {
	config := followConfig()
	config.FollowBuffer = 1
	config.RevalidateInterval = time.Hour
	config.RediscoverInterval = time.Hour
	config.HeartbeatInterval = time.Hour
	service, _, _, client := newTestServiceWithConfig(t, config)
	var body strings.Builder
	for index := 0; index < 100; index++ {
		body.WriteString("2026-08-09T11:59:59Z line\n")
	}
	client.logs = []string{body.String(), ""}
	stream, err := service.Follow(t.Context(), followRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	// Allow the one-slot channel to fill and force bounded non-blocking drops.
	time.Sleep(20 * time.Millisecond)
	event := waitStreamEvent(t, stream, time.Second, func(event StreamEvent) bool {
		return event.Type == StreamGap && event.Gap != nil
	})
	if event.Gap.DroppedLines < 1 || event.Gap.Source.ID == "" {
		t.Fatalf("invalid gap=%#v", event.Gap)
	}
}

func TestFollowByteAndDurationLimitsEmitSafeTerminal(t *testing.T) {
	t.Run("bytes", func(t *testing.T) {
		config := followConfig()
		config.DefaultLimitBytes = 16
		config.MaxSnapshotBytes = 64
		config.MaxLineBytes = 32
		config.MaxFollowBytes = 20
		config.FollowBuffer = 8
		config.RevalidateInterval = time.Hour
		config.RediscoverInterval = time.Hour
		config.HeartbeatInterval = time.Hour
		service, _, _, client := newTestServiceWithConfig(t, config)
		client.logs = []string{"2026-08-09T11:59:59Z 1234567890123456\n", "2026-08-09T11:59:59Z 1234567890123456\n"}
		request := followRequest()
		request.Options.LimitBytes = 64
		stream, err := service.Follow(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		event := waitStreamEvent(t, stream, time.Second, func(event StreamEvent) bool {
			return event.Type == StreamTerminal && event.Terminal != nil && event.Terminal.Code == "ResponseLimitReached"
		})
		if strings.Contains(event.Terminal.Detail, client.pods[0]["metadata"].(map[string]any)["name"].(string)) {
			t.Fatal("terminal leaked Pod name")
		}
	})
	t.Run("duration", func(t *testing.T) {
		config := followConfig()
		config.MaxFollowDuration = 20 * time.Millisecond
		config.RevalidateInterval = time.Hour
		config.RediscoverInterval = time.Hour
		config.HeartbeatInterval = time.Hour
		service, _, _, client := newTestServiceWithConfig(t, config)
		reader, writer := io.Pipe()
		client.readers = []io.ReadCloser{reader}
		stream, err := service.Follow(t.Context(), followRequest())
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		event := waitStreamEvent(t, stream, time.Second, func(event StreamEvent) bool {
			return event.Type == StreamTerminal && event.Terminal != nil && event.Terminal.Code == "SessionExpired"
		})
		if event.Terminal.Detail == "" {
			t.Fatal("missing bounded session detail")
		}
		_ = writer.Close()
	})
}

func TestFollowCursorMustBelongToExactOpaqueSource(t *testing.T) {
	service, _, _, _ := newTestServiceWithConfig(t, followConfig())
	request := followRequest()
	request.Cursor = &ReconnectCursor{SourceID: "build_" + strings.Repeat("0", 32), Timestamp: time.Date(2026, 8, 9, 11, 59, 0, 0, time.UTC), Fingerprint: strings.Repeat("a", 64)}
	if _, err := service.Follow(t.Context(), request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("foreign cursor accepted: %v", err)
	}
}

func TestFollowCompletedBuildEndsAfterSnapshot(t *testing.T) {
	service, resolver, _, client := newTestServiceWithConfig(t, followConfig())
	completed := time.Date(2026, 8, 9, 11, 30, 0, 0, time.UTC)
	resolver.authorized.Attempt.State = "failed"
	resolver.authorized.Attempt.CompletedAt = &completed
	client.logs = []string{"2026-08-09T11:59:59Z done\n"}
	stream, err := service.Follow(t.Context(), followRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	line := waitStreamEvent(t, stream, time.Second, func(event StreamEvent) bool { return event.Type == StreamLine })
	if line.Line == nil || line.Line.Message != "done" {
		t.Fatalf("line=%#v", line)
	}
	terminal := waitStreamEvent(t, stream, time.Second, func(event StreamEvent) bool { return event.Type == StreamTerminal })
	if terminal.Terminal == nil || terminal.Terminal.Code != "BuildCompleted" {
		t.Fatalf("terminal=%#v", terminal)
	}
}
