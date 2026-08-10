package queue

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	valkey "github.com/valkey-io/valkey-go"
)

func TestParseWorkEntriesRejectsMalformedOrExtendedEnvelopes(t *testing.T) {
	valid := valkey.XRangeEntry{ID: "1-0", FieldValues: map[string]string{
		"operationId": "11111111-1111-4111-8111-111111111111",
		"kind":        "deployment.git-write",
		"scopeId":     "22222222-2222-4222-8222-222222222222",
		"generation":  "2",
		"traceId":     "request-1",
	}}
	malformed := []valkey.XRangeEntry{
		{ID: "2-0", FieldValues: map[string]string{"operationId": "wrong"}},
		{ID: "3-0", FieldValues: map[string]string{"operationId": valid.FieldValues["operationId"], "kind": valid.FieldValues["kind"], "scopeId": valid.FieldValues["scopeId"], "generation": "0", "traceId": "request-1"}},
		{ID: "4-0", FieldValues: map[string]string{"operationId": valid.FieldValues["operationId"], "kind": valid.FieldValues["kind"], "scopeId": valid.FieldValues["scopeId"], "generation": "1", "traceId": "request-1", "payload": "must-not-enter-valkey"}},
		{ID: "invalid", FieldValues: valid.FieldValues},
	}
	messages, invalid := parseWorkEntries(append([]valkey.XRangeEntry{valid}, malformed...))
	if len(messages) != 1 || messages[0].OperationID != valid.FieldValues["operationId"] || messages[0].Generation != 2 {
		t.Fatalf("valid messages=%#v", messages)
	}
	if strings.Join(invalid, ",") != "2-0,3-0,4-0" {
		t.Fatalf("invalid delivery IDs=%v", invalid)
	}
}

func TestValkeyStreamReclaimsAndDiscardsMalformedPendingEntries(t *testing.T) {
	address := os.Getenv("KUBERPLOY_TEST_VALKEY_ADDRESS")
	if address == "" {
		t.Skip("KUBERPLOY_TEST_VALKEY_ADDRESS is not set")
	}
	stream := "kp:v1:test:" + strings.ReplaceAll(strings.ToLower(t.Name()), "/", "-")
	group := "test-group"
	options := ValkeyOptions{Addresses: []string{address}, Stream: stream, Group: group, MaxLen: 100, ClaimIdle: 20 * time.Millisecond}
	first, err := NewValkeyStream(options)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx := context.Background()
	t.Cleanup(func() { _ = first.client.Do(ctx, first.client.B().Del().Key(stream).Build()).Error() })

	want := domain.WorkMessage{OperationID: "11111111-1111-4111-8111-111111111111", Kind: "deployment.git-write", ScopeID: "22222222-2222-4222-8222-222222222222", Generation: 1, TraceID: "request-1"}
	if err = first.Publish(ctx, want); err != nil {
		t.Fatal(err)
	}
	messages, err := first.Receive(ctx, "consumer-a", 10)
	if err != nil || len(messages) != 1 {
		t.Fatalf("initial receive=%#v err=%v", messages, err)
	}
	deliveryID := messages[0].DeliveryID
	time.Sleep(30 * time.Millisecond)

	second, err := NewValkeyStream(options)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	reclaimed, err := second.Receive(ctx, "consumer-b", 10)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].DeliveryID != deliveryID || reclaimed[0].OperationID != want.OperationID {
		t.Fatalf("reclaimed=%#v err=%v", reclaimed, err)
	}
	if err = second.Ack(ctx, reclaimed[0]); err != nil {
		t.Fatal(err)
	}

	if err = second.client.Do(ctx, second.client.B().Arbitrary("XADD", stream, "*", "operationId", "malformed").Build()).Error(); err != nil {
		t.Fatal(err)
	}
	if messages, err = second.Receive(ctx, "consumer-c", 10); err != nil || len(messages) != 0 {
		t.Fatalf("malformed receive=%#v err=%v", messages, err)
	}
	pending, err := second.client.Do(ctx, second.client.B().Arbitrary("XPENDING", stream, group).Build()).ToArray()
	if err != nil || len(pending) < 1 {
		t.Fatalf("XPENDING=%#v err=%v", pending, err)
	}
	count, err := pending[0].ToInt64()
	if err != nil || count != 0 {
		t.Fatalf("pending count=%d err=%v", count, err)
	}
}

func TestValkeyOptionsAndPublisherFailClosed(t *testing.T) {
	if _, err := NewValkeyStream(ValkeyOptions{}); err == nil {
		t.Fatal("empty address was accepted")
	}
	if _, err := NewValkeyStream(ValkeyOptions{Addresses: []string{"127.0.0.1:6379"}, Stream: "bad stream"}); err == nil {
		t.Fatal("invalid stream name was accepted")
	}
	if _, err := NewValkeyStream(ValkeyOptions{Addresses: []string{"127.0.0.1:6379"}, MaxLen: -1}); err == nil {
		t.Fatal("negative stream bound was accepted")
	}
	if _, err := NewValkeyStream(ValkeyOptions{Addresses: []string{"127.0.0.1:6379"}, ClientName: "bad client"}); err == nil {
		t.Fatal("invalid client identity was accepted")
	}
}

func TestValkeyDatasetIdentityChangesOnlyAfterSentinelLoss(t *testing.T) {
	address := os.Getenv("KUBERPLOY_TEST_VALKEY_ADDRESS")
	if address == "" {
		t.Skip("KUBERPLOY_TEST_VALKEY_ADDRESS is not set")
	}
	stream, err := NewValkeyStream(ValkeyOptions{Addresses: []string{address}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	ctx := t.Context()
	t.Cleanup(func() { _ = stream.client.Do(ctx, stream.client.B().Del().Key(datasetIdentityKey).Build()).Error() })
	_ = stream.client.Do(ctx, stream.client.B().Del().Key(datasetIdentityKey).Build()).Error()
	first, err := stream.DatasetIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stable, err := stream.DatasetIdentity(ctx)
	if err != nil || stable != first {
		t.Fatalf("stable identity=%q first=%q err=%v", stable, first, err)
	}
	if err = stream.client.Do(ctx, stream.client.B().Del().Key(datasetIdentityKey).Build()).Error(); err != nil {
		t.Fatal(err)
	}
	replaced, err := stream.DatasetIdentity(ctx)
	if err != nil || replaced == first {
		t.Fatalf("replacement identity=%q first=%q err=%v", replaced, first, err)
	}
}
