package githubapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	webhookEventPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	deliveryIDPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// WebhookEnvelope contains the byte-exact authenticated body and the local
// receipt/replay timestamps. GitHub does not sign a delivery timestamp, so
// ReceivedAt is intentionally the receiver's clock, not a payload claim.
type WebhookEnvelope struct {
	AppID       int64
	Event       string
	DeliveryID  string
	Body        []byte
	ReceivedAt  time.Time
	ReplayUntil time.Time
}

type WebhookVerifier struct {
	secretRef    SecretRef
	appID        int64
	secrets      SecretReader
	clock        Clock
	maxBody      int64
	replayWindow time.Duration
}

func NewWebhookVerifier(cfg Config, secrets SecretReader, clock Clock) (*WebhookVerifier, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if secrets == nil {
		return nil, fmt.Errorf("%w: secret reader is required", ErrInvalidConfig)
	}
	return &WebhookVerifier{
		secretRef: cfg.WebhookSecret, appID: cfg.AppID, secrets: secrets, clock: clockOrSystem(clock),
		maxBody: cfg.MaxWebhookBytes, replayWindow: cfg.WebhookReplayWindow,
	}, nil
}

// Verify authenticates the exact body bytes before typed JSON parsing.
func (v *WebhookVerifier) Verify(ctx context.Context, headers http.Header, body io.Reader) (WebhookEnvelope, error) {
	signature, ok := exactlyOneHeader(headers, "X-Hub-Signature-256")
	if !ok || len(signature) != len("sha256=")+sha256.Size*2 || !strings.HasPrefix(signature, "sha256=") {
		return WebhookEnvelope{}, ErrInvalidWebhook
	}
	supplied, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil || len(supplied) != sha256.Size || signature != strings.ToLower(signature) {
		return WebhookEnvelope{}, ErrInvalidWebhook
	}
	event, ok := exactlyOneHeader(headers, "X-GitHub-Event")
	if !ok || !webhookEventPattern.MatchString(event) {
		return WebhookEnvelope{}, ErrInvalidWebhook
	}
	deliveryID, ok := exactlyOneHeader(headers, "X-GitHub-Delivery")
	if !ok || !deliveryIDPattern.MatchString(deliveryID) {
		return WebhookEnvelope{}, ErrInvalidWebhook
	}
	if body == nil {
		return WebhookEnvelope{}, ErrInvalidWebhook
	}
	exactBody, err := io.ReadAll(io.LimitReader(body, v.maxBody+1))
	if err != nil {
		return WebhookEnvelope{}, ErrInvalidWebhook
	}
	if int64(len(exactBody)) > v.maxBody {
		return WebhookEnvelope{}, ErrWebhookTooLarge
	}
	secret, err := v.secrets.ReadSecret(ctx, v.secretRef)
	if err != nil {
		zeroBytes(secret)
		return WebhookEnvelope{}, ErrSecretUnavailable
	}
	if len(secret) < 16 || len(secret) > 4096 {
		zeroBytes(secret)
		return WebhookEnvelope{}, ErrSecretUnavailable
	}
	mac := hmac.New(sha256.New, secret)
	zeroBytes(secret)
	_, _ = mac.Write(exactBody)
	expected := mac.Sum(nil)
	valid := hmac.Equal(expected, supplied)
	zeroBytes(expected)
	zeroBytes(supplied)
	if !valid {
		zeroBytes(exactBody)
		return WebhookEnvelope{}, ErrInvalidWebhook
	}
	now := v.clock.Now().UTC()
	return WebhookEnvelope{
		AppID: v.appID, Event: event, DeliveryID: deliveryID, Body: exactBody, ReceivedAt: now,
		ReplayUntil: now.Add(v.replayWindow),
	}, nil
}

// deliveryClaim scopes permanent delivery uniqueness by provider, GitHub App,
// installation, and delivery id. RetainUntil governs associated payload data;
// the claim tombstone itself must never be purged.
func (envelope WebhookEnvelope) deliveryClaim(installationID int64) (OneTimeClaim, error) {
	if envelope.AppID <= 0 || installationID <= 0 || !deliveryIDPattern.MatchString(envelope.DeliveryID) || envelope.ReceivedAt.IsZero() || !envelope.ReplayUntil.After(envelope.ReceivedAt) {
		return OneTimeClaim{}, ErrInvalidWebhook
	}
	material := "githubapp-delivery-v2\x00github\x00" + strconv.FormatInt(envelope.AppID, 10) + "\x00" + strconv.FormatInt(installationID, 10) + "\x00" + envelope.DeliveryID
	digest := sha256.Sum256([]byte(material))
	return OneTimeClaim{Kind: "github-delivery", ClaimKey: hex.EncodeToString(digest[:]), RetainUntil: envelope.ReplayUntil, Permanent: true}, nil
}

func claimDelivery(ctx context.Context, claimer ReplayClaimer, envelope WebhookEnvelope, installationID int64) error {
	if claimer == nil {
		return ErrInvalidWebhook
	}
	claim, err := envelope.deliveryClaim(installationID)
	if err != nil {
		return err
	}
	ok, err := claimer.ClaimOnce(ctx, claim)
	if err != nil {
		return err
	}
	if !ok {
		return ErrWebhookReplay
	}
	return nil
}

// ClaimEventDelivery is the preferred handler entry point: it obtains the
// installation scope only from a supported typed event.
func ClaimEventDelivery(ctx context.Context, claimer ReplayClaimer, envelope WebhookEnvelope, event Event) error {
	installationID, ok := EventInstallationID(event)
	if !ok {
		return ErrInvalidWebhook
	}
	return claimDelivery(ctx, claimer, envelope, installationID)
}

func exactlyOneHeader(headers http.Header, name string) (string, bool) {
	values := headers.Values(name)
	if len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] || strings.Contains(values[0], ",") {
		return "", false
	}
	return values[0], true
}
