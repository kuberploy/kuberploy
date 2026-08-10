package githubapp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStateRoundTripBindsPurposeActorAndTeam(t *testing.T) {
	cfg := validTestConfig(t)
	clock := &fixedClock{now: time.Date(2026, 8, 9, 2, 0, 0, 0, time.UTC)}
	secrets := testSecrets(t, cfg)
	manager, err := NewStateManager(cfg, secrets, clock, strings.NewReader(strings.Repeat("n", 64)))
	if err != nil {
		t.Fatal(err)
	}
	issued, err := manager.Issue(context.Background(), StateRequest{
		Purpose: StatePurposeSetup, ActorID: "user:123", TeamID: "team:456",
		ExpectedAccountID: 789, ReturnKey: "project-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := manager.VerifyRaw(context.Background(), issued.Reveal(), StateExpectation{Purpose: StatePurposeSetup, ActorID: "user:123", TeamID: "team:456"})
	if err != nil {
		t.Fatal(err)
	}
	if verified.ExpectedAccountID != 789 || verified.ReturnKey != "project-source" || !verified.ExpiresAt.Equal(clock.now.Add(cfg.StateTTL)) || verified.replay.Permanent {
		t.Fatalf("verified=%#v", verified)
	}
	for name, expectation := range map[string]StateExpectation{
		"purpose": {Purpose: StatePurposeOAuth, ActorID: "user:123", TeamID: "team:456"},
		"actor":   {Purpose: StatePurposeSetup, ActorID: "user:other", TeamID: "team:456"},
		"team":    {Purpose: StatePurposeSetup, ActorID: "user:123", TeamID: "team:other"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, verifyErr := manager.VerifyRaw(context.Background(), issued.Reveal(), expectation); !errors.Is(verifyErr, ErrInvalidState) {
				t.Fatalf("expected binding rejection, got %v", verifyErr)
			}
		})
	}
	secrets.mu.Lock()
	defer secrets.mu.Unlock()
	for _, value := range secrets.last {
		if value != 0 {
			t.Fatal("state signing secret buffer was not erased")
		}
	}
}

func TestStateRejectsTamperingMalformedAndClosedPayloads(t *testing.T) {
	cfg := validTestConfig(t)
	clock := &fixedClock{now: time.Date(2026, 8, 9, 2, 0, 0, 0, time.UTC)}
	secrets := testSecrets(t, cfg)
	manager, _ := NewStateManager(cfg, secrets, clock, strings.NewReader(strings.Repeat("r", 64)))
	issued, _ := manager.Issue(context.Background(), StateRequest{Purpose: StatePurposeOAuth, ActorID: "user-1", ReturnKey: "dashboard"})
	raw := issued.Reveal()
	tampered := raw[:len(raw)-1] + "A"
	malformed := []string{"", "one.two.three", ".signature", "body.", strings.Repeat("a", 4097), tampered}
	for _, candidate := range malformed {
		if _, err := manager.VerifyRaw(context.Background(), candidate, StateExpectation{Purpose: StatePurposeOAuth, ActorID: "user-1"}); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("candidate %q: %v", candidate[:min(len(candidate), 20)], err)
		}
	}
	base := statePayload{
		Version: 1, Purpose: StatePurposeOAuth, ActorID: "user-1", ReturnKey: "dashboard",
		Nonce:    base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32))),
		IssuedAt: clock.now.Unix(), ExpiresAt: clock.now.Add(cfg.StateTTL).Unix(),
	}
	encoded, _ := json.Marshal(base)
	unknown := append(encoded[:len(encoded)-1], []byte(`,"unknown":true}`)...)
	duplicate := []byte(fmt.Sprintf(`{"v":1,"v":2,"purpose":"oauth","actor_id":"user-1","return_key":"dashboard","nonce":%q,"iat":%d,"exp":%d}`,
		base.Nonce, base.IssuedAt, base.ExpiresAt))
	for name, payload := range map[string][]byte{"unknown field": unknown, "duplicate key": duplicate, "trailing document": append(encoded, []byte(` {}`)...)} {
		t.Run(name, func(t *testing.T) {
			token := signStateFixture(t, secrets.values[cfg.StateSigningSecret], payload)
			if _, err := manager.VerifyRaw(context.Background(), token, StateExpectation{Purpose: StatePurposeOAuth, ActorID: "user-1"}); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("expected closed-state rejection, got %v", err)
			}
		})
	}
}

func TestStateExpiryFutureClaimsAndReplayAreFailClosed(t *testing.T) {
	cfg := validTestConfig(t)
	clock := &fixedClock{now: time.Date(2026, 8, 9, 2, 0, 0, 0, time.UTC)}
	secrets := testSecrets(t, cfg)
	manager, _ := NewStateManager(cfg, secrets, clock, strings.NewReader(strings.Repeat("s", 64)))
	issued, _ := manager.Issue(context.Background(), StateRequest{Purpose: StatePurposeSetup, ActorID: "actor-1", ReturnKey: "setup"})
	verified, err := manager.VerifyRaw(context.Background(), issued.Reveal(), StateExpectation{Purpose: StatePurposeSetup, ActorID: "actor-1"})
	if err != nil {
		t.Fatal(err)
	}
	claimer := &memoryClaimer{}
	var accepted atomic.Int64
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if claimErr := ClaimState(context.Background(), clock, claimer, verified); claimErr == nil {
				accepted.Add(1)
			} else if !errors.Is(claimErr, ErrStateReplay) {
				t.Errorf("unexpected replay claim error: %v", claimErr)
			}
		}()
	}
	wait.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted state count=%d", accepted.Load())
	}
	clock.now = verified.ExpiresAt
	if err = ClaimState(context.Background(), clock, &memoryClaimer{}, verified); !errors.Is(err, ErrExpiredState) {
		t.Fatalf("expired verified state was claimed: %v", err)
	}
	if _, err = manager.VerifyRaw(context.Background(), issued.Reveal(), StateExpectation{Purpose: StatePurposeSetup, ActorID: "actor-1"}); !errors.Is(err, ErrExpiredState) {
		t.Fatalf("expected expiry, got %v", err)
	}
	future := statePayload{
		Version: 1, Purpose: StatePurposeSetup, ActorID: "actor-1", ReturnKey: "setup",
		Nonce:    base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("f", 32))),
		IssuedAt: clock.now.Add(31 * time.Second).Unix(), ExpiresAt: clock.now.Add(5 * time.Minute).Unix(),
	}
	payload, _ := json.Marshal(future)
	token := signStateFixture(t, secrets.values[cfg.StateSigningSecret], payload)
	if _, err = manager.VerifyRaw(context.Background(), token, StateExpectation{Purpose: StatePurposeSetup, ActorID: "actor-1"}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("future state accepted: %v", err)
	}
}

func signStateFixture(t *testing.T, key, payload []byte) string {
	t.Helper()
	body := base64.RawURLEncoding.EncodeToString(payload)
	signature := stateMAC(key, body)
	return body + "." + base64.RawURLEncoding.EncodeToString(signature)
}

type failingReader struct{ marker string }

func (f failingReader) Read([]byte) (int, error) { return 0, errors.New(f.marker) }

type memoryHandoffs struct {
	mu       sync.Mutex
	records  map[[sha256.Size]byte]HandoffRecord
	consumed map[[sha256.Size]byte]bool
}

func (s *memoryHandoffs) ConsumeHandoff(_ context.Context, digest [sha256.Size]byte, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[digest]
	if !exists || s.consumed[digest] || !now.Before(record.ExpiresAt) {
		return false, nil
	}
	s.consumed[digest] = true
	return true, nil
}

func TestHandoffStoresOnlyHashAndConsumesExactlyOnce(t *testing.T) {
	clock := &fixedClock{now: time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)}
	issued, err := IssueHandoff(clock, strings.NewReader(strings.Repeat("h", 64)), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(issued.Token.Reveal()) != 43 || strings.Contains(hex.EncodeToString(issued.Record.Digest[:]), issued.Token.Reveal()) {
		t.Fatalf("unexpected handoff material: token length=%d", len(issued.Token.Reveal()))
	}
	if err = VerifyHandoffRaw(clock, issued.Record, issued.Token.Reveal()); err != nil {
		t.Fatal(err)
	}
	if err = VerifyHandoffRaw(clock, issued.Record, strings.Repeat("A", 43)); !errors.Is(err, ErrInvalidHandoff) {
		t.Fatalf("wrong handoff accepted: %v", err)
	}
	if err = VerifyHandoffRaw(clock, issued.Record, strings.Repeat("A", 1<<20)); !errors.Is(err, ErrInvalidHandoff) {
		t.Fatalf("oversized handoff accepted: %v", err)
	}
	store := &memoryHandoffs{records: map[[sha256.Size]byte]HandoffRecord{issued.Record.Digest: issued.Record}, consumed: make(map[[sha256.Size]byte]bool)}
	var accepted atomic.Int64
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if consumeErr := ConsumeHandoffRaw(context.Background(), clock, store, issued.Token.Reveal()); consumeErr == nil {
				accepted.Add(1)
			} else if !errors.Is(consumeErr, ErrInvalidHandoff) {
				t.Errorf("consume: %v", consumeErr)
			}
		}()
	}
	wait.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("handoff accepted %d times", accepted.Load())
	}
	clock.now = issued.Record.ExpiresAt
	if err = VerifyHandoffRaw(clock, issued.Record, issued.Token.Reveal()); !errors.Is(err, ErrExpiredHandoff) {
		t.Fatalf("expired handoff accepted: %v", err)
	}
}

func TestStateAndHandoffRandomnessFailuresAreRedacted(t *testing.T) {
	cfg := validTestConfig(t)
	manager, _ := NewStateManager(cfg, testSecrets(t, cfg), &fixedClock{now: time.Now()}, failingReader{marker: "random-secret-marker"})
	_, err := manager.Issue(context.Background(), StateRequest{Purpose: StatePurposeOAuth, ActorID: "actor", ReturnKey: "home"})
	if !errors.Is(err, ErrInvalidState) || strings.Contains(err.Error(), "random-secret-marker") {
		t.Fatalf("state randomness error leaked: %v", err)
	}
	_, err = IssueHandoff(&fixedClock{now: time.Now()}, failingReader{marker: "random-secret-marker"}, 5*time.Minute)
	if !errors.Is(err, ErrInvalidHandoff) || strings.Contains(err.Error(), "random-secret-marker") {
		t.Fatalf("handoff randomness error leaked: %v", err)
	}
}
