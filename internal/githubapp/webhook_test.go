package githubapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	officialWebhookSecret    = "It's a Secret to Everybody"
	officialWebhookPayload   = "Hello, World!"
	officialWebhookSignature = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
)

func webhookHeaders(signature, event, delivery string) http.Header {
	return http.Header{
		"X-Hub-Signature-256": {signature},
		"X-Github-Event":      {event},
		"X-Github-Delivery":   {delivery},
	}
}

func webhookSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookVerifierMatchesOfficialByteExactFixture(t *testing.T) {
	cfg := validTestConfig(t)
	secrets := testSecrets(t, cfg)
	secrets.values[cfg.WebhookSecret] = []byte(officialWebhookSecret)
	clock := &fixedClock{now: time.Date(2026, 8, 9, 4, 0, 0, 0, time.UTC)}
	verifier, err := NewWebhookVerifier(cfg, secrets, clock)
	if err != nil {
		t.Fatal(err)
	}
	headers := webhookHeaders(officialWebhookSignature, "push", "11111111-2222-4333-8444-555555555555")
	envelope, err := verifier.Verify(context.Background(), headers, strings.NewReader(officialWebhookPayload))
	if err != nil {
		t.Fatal(err)
	}
	claim, claimErr := envelope.deliveryClaim(77)
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if string(envelope.Body) != officialWebhookPayload || envelope.AppID != cfg.AppID || !envelope.ReceivedAt.Equal(clock.now) || !envelope.ReplayUntil.Equal(clock.now.Add(24*time.Hour)) || !claim.Permanent {
		t.Fatalf("envelope=%#v", envelope)
	}
	// A newline is a different byte sequence even though many JSON/body helpers
	// would otherwise normalize it.
	if _, err = verifier.Verify(context.Background(), headers, strings.NewReader(officialWebhookPayload+"\n")); !errors.Is(err, ErrInvalidWebhook) {
		t.Fatalf("mutated body accepted: %v", err)
	}
	secrets.mu.Lock()
	defer secrets.mu.Unlock()
	for _, value := range secrets.last {
		if value != 0 {
			t.Fatal("webhook secret buffer was not erased")
		}
	}
}

func TestWebhookVerifierRejectsMalformedHeadersAndPartialSignatures(t *testing.T) {
	cfg := validTestConfig(t)
	verifier, _ := NewWebhookVerifier(cfg, testSecrets(t, cfg), &fixedClock{now: time.Now()})
	body := []byte(`{"ok":true}`)
	validSignature := webhookSignature("webhook-secret-with-at-least-32-bytes", body)
	valid := webhookHeaders(validSignature, "push", "11111111-2222-4333-8444-555555555555")
	tests := map[string]http.Header{
		"missing signature": webhookHeaders("", "push", "11111111-2222-4333-8444-555555555555"),
		"wrong prefix":      webhookHeaders("sha1="+strings.Repeat("a", 64), "push", "11111111-2222-4333-8444-555555555555"),
		"partial prefix":    webhookHeaders(validSignature[:len(validSignature)-2]+"00", "push", "11111111-2222-4333-8444-555555555555"),
		"uppercase hex":     webhookHeaders(strings.ToUpper(validSignature), "push", "11111111-2222-4333-8444-555555555555"),
		"signature space":   webhookHeaders(validSignature+" ", "push", "11111111-2222-4333-8444-555555555555"),
		"bad event":         webhookHeaders(validSignature, "Push Event", "11111111-2222-4333-8444-555555555555"),
		"bad delivery":      webhookHeaders(validSignature, "push", "not-a-guid"),
	}
	duplicateSignature := valid.Clone()
	duplicateSignature["X-Hub-Signature-256"] = []string{validSignature, validSignature}
	tests["duplicate signature"] = duplicateSignature
	duplicateDelivery := valid.Clone()
	duplicateDelivery["X-Github-Delivery"] = []string{valid.Get("X-GitHub-Delivery"), valid.Get("X-GitHub-Delivery")}
	tests["duplicate delivery"] = duplicateDelivery
	for name, headers := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(context.Background(), headers, strings.NewReader(string(body))); !errors.Is(err, ErrInvalidWebhook) {
				t.Fatalf("expected generic webhook rejection, got %v", err)
			}
		})
	}
}

func TestWebhookVerifierEnforcesBodyLimitBeforeParsing(t *testing.T) {
	cfg := validTestConfig(t)
	cfg.MaxWebhookBytes = 1024
	secret := "webhook-secret-with-at-least-32-bytes"
	body := []byte(strings.Repeat("x", 1025))
	headers := webhookHeaders(webhookSignature(secret, body), "push", "11111111-2222-4333-8444-555555555555")
	verifier, _ := NewWebhookVerifier(cfg, testSecrets(t, cfg), &fixedClock{now: time.Now()})
	if _, err := verifier.Verify(context.Background(), headers, strings.NewReader(string(body))); !errors.Is(err, ErrWebhookTooLarge) {
		t.Fatalf("oversized body result: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), headers, errorReader{}); !errors.Is(err, ErrInvalidWebhook) {
		t.Fatalf("reader failure result: %v", err)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestWebhookDeliveryClaimIsPermanentAndConcurrentSafe(t *testing.T) {
	cfg := validTestConfig(t)
	secret := "webhook-secret-with-at-least-32-bytes"
	body := []byte(`not-json-and-unknown-is-fine`)
	headers := webhookHeaders(webhookSignature(secret, body), "future_event", "11111111-2222-4333-8444-555555555555")
	verifier, _ := NewWebhookVerifier(cfg, testSecrets(t, cfg), &fixedClock{now: time.Now()})
	envelope, err := verifier.Verify(context.Background(), headers, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	event, supported, err := ParseEvent(envelope)
	if err != nil || supported || event != nil {
		t.Fatalf("unknown event parsed: event=%#v supported=%t err=%v", event, supported, err)
	}
	claim, err := envelope.deliveryClaim(77)
	if err != nil || !claim.Permanent || claim.RetainUntil.IsZero() {
		t.Fatalf("delivery claim may be purged/reused: %#v err=%v", claim, err)
	}
	otherInstallation, _ := envelope.deliveryClaim(78)
	otherApp := envelope
	otherApp.AppID++
	otherAppClaim, _ := otherApp.deliveryClaim(77)
	if claim.ClaimKey == otherInstallation.ClaimKey || claim.ClaimKey == otherAppClaim.ClaimKey {
		t.Fatal("delivery uniqueness is not scoped by app and installation")
	}
	claimer := &memoryClaimer{}
	var accepted atomic.Int64
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if claimErr := ClaimEventDelivery(context.Background(), claimer, envelope, PushEvent{InstallationID: 77}); claimErr == nil {
				accepted.Add(1)
			} else if !errors.Is(claimErr, ErrWebhookReplay) {
				t.Errorf("claim: %v", claimErr)
			}
		}()
	}
	wait.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("delivery accepted %d times", accepted.Load())
	}
	if err := ClaimEventDelivery(context.Background(), &memoryClaimer{}, envelope, nil); !errors.Is(err, ErrInvalidWebhook) {
		t.Fatalf("untyped event claimed: %v", err)
	}
}

func TestWebhookSecretErrorsDoNotLeak(t *testing.T) {
	cfg := validTestConfig(t)
	secrets := testSecrets(t, cfg)
	secrets.err = errors.New("webhook-secret-marker")
	body := []byte(`{}`)
	headers := webhookHeaders(webhookSignature("anything-with-sixteen-bytes", body), "push", "11111111-2222-4333-8444-555555555555")
	verifier, _ := NewWebhookVerifier(cfg, secrets, &fixedClock{now: time.Now()})
	_, err := verifier.Verify(context.Background(), headers, strings.NewReader(string(body)))
	if !errors.Is(err, ErrSecretUnavailable) || strings.Contains(err.Error(), "webhook-secret-marker") {
		t.Fatalf("secret error leaked: %v", err)
	}
}
