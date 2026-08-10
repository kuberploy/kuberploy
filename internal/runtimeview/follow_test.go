package runtimeview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestFollowReconnectsWithOverlapAndDeduplicatesLines(t *testing.T) {
	resolver, client := baseFixture()
	firstAt := time.Now().UTC().Add(-3 * time.Second).Truncate(time.Millisecond)
	secondAt := firstAt.Add(time.Second)
	thirdAt := secondAt.Add(time.Second)
	firstBody := timedLine(firstAt, "line-one") + timedLine(secondAt, "line-two")
	secondReader := newBlockingReader(timedLine(secondAt, "line-two") + timedLine(thirdAt, "line-three"))
	client.openFn = func(_ PodLogRequest, index int) (io.ReadCloser, error) {
		if index == 0 {
			return io.NopCloser(strings.NewReader(firstBody)), nil
		}
		return secondReader, nil
	}
	config := testConfig()
	config.ReconnectOverlap = 1500 * time.Millisecond
	service := newTestService(resolver, client, config)
	stream, err := service.Follow(context.Background(), FollowRequest{Target: testTargetRef, Options: LogOptions{Follow: true}})
	if err != nil {
		t.Fatal(err)
	}
	messages := collectMessages(t, stream, 3, time.Second)
	stream.Close()
	waitDone(t, stream)
	if strings.Join(messages, ",") != "line-one,line-two,line-three" {
		t.Fatalf("reconnect overlap was not deduplicated: %#v", messages)
	}
	requests := client.requests()
	if len(requests) < 2 || requests[1].Options.SinceTime == nil {
		t.Fatalf("missing reconnect cursor request: %#v", requests)
	}
	wantSince := secondAt.Add(-config.ReconnectOverlap)
	if !requests[1].Options.SinceTime.Equal(wantSince) || !requests[1].Options.Timestamps || !requests[1].Options.Follow || requests[1].PodUID != testPodUID {
		t.Fatalf("unsafe reconnect options: %#v; want since %s", requests[1], wantSince)
	}
	if !secondReader.wasClosed() {
		t.Fatal("follow reader was not closed with the session")
	}
}

func TestFollowAcceptsOnlyCursorForResolvedSource(t *testing.T) {
	resolver, client := baseFixture()
	baseAt := time.Now().UTC().Add(-time.Second).Truncate(time.Millisecond)
	source := LogSource{ID: sourceID(testPodUID, "application", 1, false)}
	fingerprint := lineFingerprint(source, baseAt, "already-seen")
	reader := newBlockingReader(timedLine(baseAt, "already-seen") + timedLine(baseAt.Add(time.Millisecond), "new-line"))
	client.openFn = func(_ PodLogRequest, _ int) (io.ReadCloser, error) { return reader, nil }
	config := testConfig()
	service := newTestService(resolver, client, config)
	stream, err := service.Follow(context.Background(), FollowRequest{
		Target:  testTargetRef,
		Options: LogOptions{Follow: true},
		Cursors: []ReconnectCursor{{SourceID: source.ID, Timestamp: baseAt, Fingerprint: fingerprint}},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages := collectMessages(t, stream, 1, time.Second)
	stream.Close()
	waitDone(t, stream)
	if len(messages) != 1 || messages[0] != "new-line" {
		t.Fatalf("initial cursor did not suppress overlap: %#v", messages)
	}
	requests := client.requests()
	if len(requests) == 0 || requests[0].Options.SinceTime == nil || !requests[0].Options.SinceTime.Equal(baseAt.Add(-config.ReconnectOverlap)) {
		t.Fatalf("cursor was not translated to bounded overlap: %#v", requests)
	}

	_, client = baseFixture()
	service = newTestService(resolver, client, config)
	_, err = service.Follow(context.Background(), FollowRequest{
		Target:  testTargetRef,
		Options: LogOptions{Follow: true},
		Cursors: []ReconnectCursor{{SourceID: "src_" + strings.Repeat("a", 32), Timestamp: baseAt, Fingerprint: strings.Repeat("b", 64)}},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unknown source cursor was accepted: %v", err)
	}
}

func TestFollowCancellationClosesBlockedKubernetesReader(t *testing.T) {
	resolver, client := baseFixture()
	reader := newBlockingReader("")
	client.openFn = func(_ PodLogRequest, _ int) (io.ReadCloser, error) { return reader, nil }
	service := newTestService(resolver, client, testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := service.Follow(ctx, FollowRequest{Target: testTargetRef, Options: LogOptions{Follow: true}})
	if err != nil {
		t.Fatal(err)
	}
	waitForOpen(t, client)
	cancel()
	waitDone(t, stream)
	if !reader.wasClosed() {
		t.Fatal("context cancellation did not close the blocked Kubernetes reader")
	}
}

func TestFollowRevalidatesAuthorizationAndTerminatesPromptly(t *testing.T) {
	resolver, client := baseFixture()
	reader := newBlockingReader("")
	client.openFn = func(_ PodLogRequest, _ int) (io.ReadCloser, error) { return reader, nil }
	resolver.setRevalidateError(ErrUnauthorized)
	config := testConfig()
	config.RevalidateInterval = 5 * time.Millisecond
	service := newTestService(resolver, client, config)
	stream, err := service.Follow(context.Background(), FollowRequest{Target: testTargetRef, Options: LogOptions{Follow: true}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	found := false
	for !found {
		select {
		case event, ok := <-stream.Events:
			if !ok {
				t.Fatal("stream closed without authorization terminal event")
			}
			if event.Type == StreamTerminal && event.Terminal != nil {
				found = event.Terminal.Code == "AuthorizationRevoked"
				if strings.Contains(strings.ToLower(event.Terminal.Detail), "token") || strings.Contains(event.Terminal.Detail, testNamespace) {
					t.Fatalf("terminal detail leaked internals: %#v", event.Terminal)
				}
			}
		case <-deadline:
			t.Fatal("authorization was not revalidated promptly")
		}
	}
	waitDone(t, stream)
	resolver.mu.Lock()
	revalidations := resolver.revalidations
	resolver.mu.Unlock()
	if revalidations < 1 || !reader.wasClosed() {
		t.Fatalf("revalidation=%d readerClosed=%t", revalidations, reader.wasClosed())
	}
}

func TestFollowUsesBoundedBufferAndReportsDroppedLines(t *testing.T) {
	resolver, client := baseFixture()
	start := time.Now().UTC().Add(-time.Second).Truncate(time.Millisecond)
	var body strings.Builder
	for index := 0; index < 20; index++ {
		body.WriteString(timedLine(start.Add(time.Duration(index)*time.Millisecond), fmt.Sprintf("line-%02d", index)))
	}
	client.openFn = func(_ PodLogRequest, _ int) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body.String())), nil
	}
	config := testConfig()
	config.FollowBuffer = 1
	config.ReconnectDelay = 10 * time.Millisecond
	service := newTestService(resolver, client, config)
	stream, err := service.Follow(context.Background(), FollowRequest{Target: testTargetRef, Options: LogOptions{Follow: true}})
	if err != nil {
		t.Fatal(err)
	}
	// Leave the single-slot channel unread while the source bursts. Producers
	// must drop rather than allocate an unbounded queue.
	time.Sleep(30 * time.Millisecond)
	deadline := time.After(time.Second)
	var gap int64
	for gap == 0 {
		select {
		case event := <-stream.Events:
			if event.Type == StreamGap && event.Gap != nil {
				gap = event.Gap.DroppedLines
			}
		case <-deadline:
			stream.Close()
			t.Fatal("slow consumer did not receive an explicit gap")
		}
	}
	stream.Close()
	waitDone(t, stream)
	if gap < 1 || gap > 20 {
		t.Fatalf("invalid bounded-buffer gap count: %d", gap)
	}
}

func timedLine(timestamp time.Time, message string) string {
	return timestamp.UTC().Format(time.RFC3339Nano) + " " + message + "\n"
}

func collectMessages(t *testing.T, stream *Stream, count int, timeout time.Duration) []string {
	t.Helper()
	messages := make([]string, 0, count)
	deadline := time.After(timeout)
	for len(messages) < count {
		select {
		case event, ok := <-stream.Events:
			if !ok {
				t.Fatalf("stream closed after %d/%d lines", len(messages), count)
			}
			if event.Type == StreamLine && event.Line != nil {
				messages = append(messages, event.Line.Message)
			}
			if event.Type == StreamTerminal && event.Terminal != nil {
				t.Fatalf("unexpected terminal event: %#v", event.Terminal)
			}
		case <-deadline:
			t.Fatalf("timed out after %d/%d lines", len(messages), count)
		}
	}
	return messages
}

func waitForOpen(t *testing.T, client *fakeKubernetes) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(client.requests()) > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Kubernetes log stream did not open")
}

func waitDone(t *testing.T, stream *Stream) {
	t.Helper()
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		t.Fatal("stream did not stop promptly")
	}
}
