package githubapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

type StatePurpose string

const (
	StatePurposeOAuth StatePurpose = "oauth"
	StatePurposeSetup StatePurpose = "setup"
)

func (p StatePurpose) valid() bool { return p == StatePurposeOAuth || p == StatePurposeSetup }

// StateRequest contains only server-resolved identifiers. ReturnKey is a key
// into a server-side route table, never a return URL.
type StateRequest struct {
	Purpose           StatePurpose
	ActorID           string
	TeamID            string
	ExpectedAccountID int64
	ReturnKey         string
}

// StateExpectation binds a callback to the current authenticated browser
// principal and to the endpoint that receives it.
type StateExpectation struct {
	Purpose StatePurpose
	ActorID string
	TeamID  string
}

// OneTimeClaim is safe to persist. ClaimKey is a SHA-256 fingerprint, not the
// signed state value or nonce.
type OneTimeClaim struct {
	Kind        string
	ClaimKey    string
	RetainUntil time.Time
	Permanent   bool
}

// ReplayClaimer must atomically insert a claim if absent and return true only
// for the first consumer. Permanent claims are tombstones that must never be
// deleted; RetainUntil then controls associated payload retention only.
type ReplayClaimer interface {
	ClaimOnce(context.Context, OneTimeClaim) (bool, error)
}

type VerifiedState struct {
	Purpose           StatePurpose
	ActorID           string
	TeamID            string
	ExpectedAccountID int64
	ReturnKey         string
	IssuedAt          time.Time
	ExpiresAt         time.Time
	replay            OneTimeClaim
}

type statePayload struct {
	Version           int          `json:"v"`
	Purpose           StatePurpose `json:"purpose"`
	ActorID           string       `json:"actor_id"`
	TeamID            string       `json:"team_id,omitempty"`
	ExpectedAccountID int64        `json:"expected_account_id,omitempty"`
	ReturnKey         string       `json:"return_key"`
	Nonce             string       `json:"nonce"`
	IssuedAt          int64        `json:"iat"`
	ExpiresAt         int64        `json:"exp"`
}

type StateManager struct {
	secretRef SecretRef
	secrets   SecretReader
	clock     Clock
	random    io.Reader
	ttl       time.Duration
}

func NewStateManager(cfg Config, secrets SecretReader, clock Clock, random io.Reader) (*StateManager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if secrets == nil {
		return nil, fmt.Errorf("%w: secret reader is required", ErrInvalidConfig)
	}
	return &StateManager{secretRef: cfg.StateSigningSecret, secrets: secrets, clock: clockOrSystem(clock), random: randomOrCrypto(random), ttl: cfg.StateTTL}, nil
}

func (m *StateManager) Issue(ctx context.Context, request StateRequest) (Credential, error) {
	if err := validateStateRequest(request); err != nil {
		return Credential{}, err
	}
	nonceBytes := make([]byte, 32)
	defer zeroBytes(nonceBytes)
	if err := readRandom(m.random, nonceBytes); err != nil {
		return Credential{}, fmt.Errorf("%w: state randomness unavailable", ErrInvalidState)
	}
	now := m.clock.Now().UTC().Truncate(time.Second)
	payload := statePayload{
		Version: 1, Purpose: request.Purpose, ActorID: request.ActorID, TeamID: request.TeamID,
		ExpectedAccountID: request.ExpectedAccountID, ReturnKey: request.ReturnKey,
		Nonce: base64.RawURLEncoding.EncodeToString(nonceBytes), IssuedAt: now.Unix(), ExpiresAt: now.Add(m.ttl).Unix(),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Credential{}, ErrInvalidState
	}
	body := base64.RawURLEncoding.EncodeToString(encoded)
	key, err := m.signingKey(ctx)
	if err != nil {
		return Credential{}, err
	}
	defer zeroBytes(key)
	signature := stateMAC(key, body)
	return newCredential(body + "." + base64.RawURLEncoding.EncodeToString(signature)), nil
}

func (m *StateManager) Verify(ctx context.Context, token Credential, expected StateExpectation) (VerifiedState, error) {
	return m.VerifyRaw(ctx, token.Reveal(), expected)
}

// VerifyRaw accepts the state value received from a callback query. It exists
// so integrations do not need a public constructor for arbitrary credentials.
func (m *StateManager) VerifyRaw(ctx context.Context, raw string, expected StateExpectation) (VerifiedState, error) {
	if !expected.Purpose.valid() || !validOpaqueID(expected.ActorID) || (expected.TeamID != "" && !validOpaqueID(expected.TeamID)) {
		return VerifiedState{}, ErrInvalidState
	}
	if len(raw) == 0 || len(raw) > 4096 || strings.Count(raw, ".") != 1 {
		return VerifiedState{}, ErrInvalidState
	}
	parts := strings.SplitN(raw, ".", 2)
	if len(parts[0]) == 0 || len(parts[1]) == 0 {
		return VerifiedState{}, ErrInvalidState
	}
	supplied, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(supplied) != sha256.Size {
		return VerifiedState{}, ErrInvalidState
	}
	key, err := m.signingKey(ctx)
	if err != nil {
		return VerifiedState{}, err
	}
	expectedMAC := stateMAC(key, parts[0])
	zeroBytes(key)
	validSignature := hmac.Equal(expectedMAC, supplied)
	zeroBytes(expectedMAC)
	zeroBytes(supplied)
	if !validSignature {
		return VerifiedState{}, ErrInvalidState
	}
	payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || len(payloadBytes) == 0 || len(payloadBytes) > 2048 {
		return VerifiedState{}, ErrInvalidState
	}
	var payload statePayload
	if err = decodeClosedState(payloadBytes, &payload); err != nil {
		return VerifiedState{}, ErrInvalidState
	}
	if err = validateStatePayload(payload, expected, m.clock.Now().UTC(), m.ttl); err != nil {
		return VerifiedState{}, err
	}
	replayDigest := sha256.Sum256([]byte("githubapp-state-v1\x00" + string(payload.Purpose) + "\x00" + payload.Nonce))
	return VerifiedState{
		Purpose: payload.Purpose, ActorID: payload.ActorID, TeamID: payload.TeamID,
		ExpectedAccountID: payload.ExpectedAccountID, ReturnKey: payload.ReturnKey,
		IssuedAt: time.Unix(payload.IssuedAt, 0).UTC(), ExpiresAt: time.Unix(payload.ExpiresAt, 0).UTC(),
		replay: OneTimeClaim{Kind: "github-state", ClaimKey: hex.EncodeToString(replayDigest[:]), RetainUntil: time.Unix(payload.ExpiresAt, 0).UTC()},
	}, nil
}

// ClaimState consumes non-expired verified state through a caller-owned atomic
// store. Passing time explicitly keeps delayed callback handling fail-closed.
func ClaimState(ctx context.Context, clock Clock, claimer ReplayClaimer, state VerifiedState) error {
	if claimer == nil || state.replay.Kind != "github-state" || len(state.replay.ClaimKey) != 64 || state.replay.RetainUntil.IsZero() || state.replay.Permanent {
		return ErrInvalidState
	}
	if !clockOrSystem(clock).Now().UTC().Before(state.replay.RetainUntil) {
		return ErrExpiredState
	}
	ok, err := claimer.ClaimOnce(ctx, state.replay)
	if err != nil {
		return err
	}
	if !ok {
		return ErrStateReplay
	}
	return nil
}

func validateStateRequest(request StateRequest) error {
	if !request.Purpose.valid() || !validOpaqueID(request.ActorID) || (request.TeamID != "" && !validOpaqueID(request.TeamID)) ||
		request.ExpectedAccountID < 0 || !returnKeyPattern.MatchString(request.ReturnKey) {
		return ErrInvalidState
	}
	return nil
}

func validateStatePayload(payload statePayload, expected StateExpectation, now time.Time, maxTTL time.Duration) error {
	if payload.Version != 1 || payload.Purpose != expected.Purpose || payload.ActorID != expected.ActorID || payload.TeamID != expected.TeamID ||
		validateStateRequest(StateRequest{Purpose: payload.Purpose, ActorID: payload.ActorID, TeamID: payload.TeamID, ExpectedAccountID: payload.ExpectedAccountID, ReturnKey: payload.ReturnKey}) != nil {
		return ErrInvalidState
	}
	nonce, err := base64.RawURLEncoding.Strict().DecodeString(payload.Nonce)
	if err != nil || len(nonce) != 32 {
		return ErrInvalidState
	}
	zeroBytes(nonce)
	issued := time.Unix(payload.IssuedAt, 0).UTC()
	expires := time.Unix(payload.ExpiresAt, 0).UTC()
	if !expires.After(issued) || expires.Sub(issued) < time.Minute || expires.Sub(issued) > maxTTL || issued.After(now.Add(30*time.Second)) {
		return ErrInvalidState
	}
	if !now.Before(expires) {
		return ErrExpiredState
	}
	return nil
}

func decodeClosedState(data []byte, dst *statePayload) error {
	if err := validateSingleJSON(data); err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func (m *StateManager) signingKey(ctx context.Context) ([]byte, error) {
	key, err := m.secrets.ReadSecret(ctx, m.secretRef)
	if err != nil {
		zeroBytes(key)
		return nil, ErrSecretUnavailable
	}
	if len(key) < 32 || len(key) > 4096 {
		zeroBytes(key)
		return nil, ErrSecretUnavailable
	}
	return key, nil
}

func stateMAC(key []byte, encodedPayload string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("githubapp-state-v1."))
	_, _ = mac.Write([]byte(encodedPayload))
	return mac.Sum(nil)
}

// HandoffRecord is the only handoff material that durable storage receives.
type HandoffRecord struct {
	Digest    [sha256.Size]byte
	ExpiresAt time.Time
}

type IssuedHandoff struct {
	Token  Credential
	Record HandoffRecord
}

func IssueHandoff(clock Clock, random io.Reader, ttl time.Duration) (IssuedHandoff, error) {
	if ttl < time.Minute || ttl > 10*time.Minute {
		return IssuedHandoff{}, ErrInvalidHandoff
	}
	randomBytes := make([]byte, 32)
	defer zeroBytes(randomBytes)
	if err := readRandom(randomOrCrypto(random), randomBytes); err != nil {
		return IssuedHandoff{}, ErrInvalidHandoff
	}
	cleartext := base64.RawURLEncoding.EncodeToString(randomBytes)
	digest := handoffDigest(cleartext)
	return IssuedHandoff{Token: newCredential(cleartext), Record: HandoffRecord{Digest: digest, ExpiresAt: clockOrSystem(clock).Now().UTC().Add(ttl)}}, nil
}

// VerifyHandoff validates format, expiry, and the stored digest. The caller must
// atomically mark the matching durable record consumed in the same operation
// that exchanges it for a session.
func VerifyHandoff(clock Clock, record HandoffRecord, candidate Credential) error {
	return VerifyHandoffRaw(clock, record, candidate.Reveal())
}

// VerifyHandoffRaw is the callback-friendly form of VerifyHandoff. Raw is
// accepted only here, is strictly bounded below, and is never retained.
func VerifyHandoffRaw(clock Clock, record HandoffRecord, raw string) error {
	now := clockOrSystem(clock).Now().UTC()
	if !now.Before(record.ExpiresAt) {
		return ErrExpiredHandoff
	}
	if len(raw) != 43 {
		return ErrInvalidHandoff
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(decoded) != 32 {
		return ErrInvalidHandoff
	}
	zeroBytes(decoded)
	actual := handoffDigest(raw)
	valid := hmac.Equal(actual[:], record.Digest[:])
	zeroBytes(actual[:])
	if !valid {
		return ErrInvalidHandoff
	}
	return nil
}

func handoffDigest(cleartext string) [sha256.Size]byte {
	return sha256.Sum256([]byte("githubapp-handoff-v1\x00" + cleartext))
}

// HandoffConsumer atomically matches an unconsumed, unexpired hash and marks it
// consumed. Only the digest and timestamps are persisted.
type HandoffConsumer interface {
	ConsumeHandoff(context.Context, [sha256.Size]byte, time.Time) (bool, error)
}

// ConsumeHandoffRaw validates and hashes the callback token before asking the
// durable store to consume it exactly once. False is intentionally reported as
// a generic invalid token to avoid an existence/expiry oracle.
func ConsumeHandoffRaw(ctx context.Context, clock Clock, consumer HandoffConsumer, raw string) error {
	if consumer == nil || len(raw) != 43 {
		return ErrInvalidHandoff
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(decoded) != 32 {
		return ErrInvalidHandoff
	}
	zeroBytes(decoded)
	digest := handoffDigest(raw)
	ok, err := consumer.ConsumeHandoff(ctx, digest, clockOrSystem(clock).Now().UTC())
	zeroBytes(digest[:])
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidHandoff
	}
	return nil
}
