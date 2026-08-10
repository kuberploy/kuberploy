package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
)

type WebhookVerifier interface {
	Verify(context.Context, http.Header, io.Reader) (githubapp.WebhookEnvelope, error)
}

type GitHubProvider interface {
	VerifyInstallation(context.Context, int64, githubapp.AccountIdentity, githubapp.Permissions) (githubapp.Installation, error)
	MintInstallationToken(context.Context, githubapp.TokenRequest) (githubapp.InstallationToken, error)
	ResolveEventRef(context.Context, githubapp.InstallationToken, githubapp.PushEvent) (githubapp.ResolvedRef, error)
}

type WebhookService struct {
	Verifier      WebhookVerifier
	Provider      GitHubProvider
	Store         Store
	Owner         string
	LeaseDuration time.Duration
	Now           func() time.Time
	PushWaker     interface {
		Wake(context.Context, gitprojection.GitHubPushWake) (gitprojection.GitHubPushWakeResult, error)
	}
}

type WebhookOutcome struct {
	ClaimKey   string
	State      DeliveryState
	AttemptIDs []string
	Replay     bool
}

type receiptClaimer struct {
	store    Store
	receipt  DeliveryReceipt
	claimKey string
	replay   bool
}

func (c *receiptClaimer) ClaimOnce(ctx context.Context, claim githubapp.OneTimeClaim) (bool, error) {
	c.claimKey = claim.ClaimKey
	inserted, err := c.store.ClaimDelivery(ctx, claim, c.receipt)
	c.replay = !inserted && err == nil
	return inserted, err
}

// Handle verifies exact bytes, persists a permanent tombstone and resumable
// typed receipt, re-authorizes immutable provider identities, resolves the ref
// authoritatively, and atomically creates attempts with one outbox row each.
func (s *WebhookService) Handle(ctx context.Context, headers http.Header, body io.Reader) (WebhookOutcome, error) {
	if s == nil || s.Verifier == nil || s.Provider == nil || s.Store == nil || !validOwnerLease(s.Owner, s.LeaseDuration) {
		return WebhookOutcome{}, ErrInvalid
	}
	outcome, event, err := s.accept(ctx, headers, body)
	if err != nil || event == nil {
		return outcome, err
	}
	return s.processDelivery(ctx, outcome.ClaimKey, event, outcome.Replay)
}

// Accept authenticates and durably claims one delivery without contacting the
// provider or creating a build. HTTP receivers use this bounded path to return
// 202 immediately after the permanent tombstone and resumable typed receipt
// commit; ResumeDelivery performs provider work on a leased worker.
func (s *WebhookService) Accept(ctx context.Context, headers http.Header, body io.Reader) (WebhookOutcome, error) {
	if s == nil || s.Verifier == nil || s.Store == nil {
		return WebhookOutcome{}, ErrInvalid
	}
	outcome, _, err := s.accept(ctx, headers, body)
	return outcome, err
}

func (s *WebhookService) accept(ctx context.Context, headers http.Header, body io.Reader) (WebhookOutcome, githubapp.Event, error) {
	envelope, err := s.Verifier.Verify(ctx, headers, body)
	if err != nil {
		return WebhookOutcome{}, nil, err
	}
	// The verified raw body is needed only for closed event parsing and its
	// fingerprint. Erase this request-local copy on every exit path.
	defer clear(envelope.Body)
	event, supported, err := githubapp.ParseEvent(envelope)
	if err != nil {
		return WebhookOutcome{}, nil, err
	}
	if !supported {
		return WebhookOutcome{State: DeliveryIgnored}, nil, nil
	}
	installationID, ok := githubapp.EventInstallationID(event)
	if !ok {
		return WebhookOutcome{}, nil, githubapp.ErrInvalidWebhook
	}
	digest := sha256.Sum256(envelope.Body)
	typedEvent, err := json.Marshal(event)
	if err != nil || len(typedEvent) == 0 || len(typedEvent) > 256<<10 {
		return WebhookOutcome{}, nil, githubapp.ErrInvalidWebhook
	}
	receipt := DeliveryReceipt{
		AppID: envelope.AppID, GitHubInstallationID: installationID, DeliveryID: envelope.DeliveryID,
		Event: envelope.Event, BodySHA256: "sha256:" + hex.EncodeToString(digest[:]), TypedEvent: typedEvent, State: DeliveryClaimed,
		AvailableAt: envelope.ReceivedAt, ReceivedAt: envelope.ReceivedAt, UpdatedAt: envelope.ReceivedAt,
	}
	if push, isPush := event.(githubapp.PushEvent); isPush {
		receipt.RepositoryID, receipt.GitRef = push.Repository.ID, push.Ref
	}
	claimer := &receiptClaimer{store: s.Store, receipt: receipt}
	err = githubapp.ClaimEventDelivery(ctx, claimer, envelope, event)
	if err != nil && !errors.Is(err, githubapp.ErrWebhookReplay) {
		return WebhookOutcome{}, nil, err
	}
	if claimer.claimKey == "" {
		return WebhookOutcome{}, nil, ErrInvalid
	}
	if push, ok := event.(githubapp.PushEvent); ok && !push.Deleted && s.PushWaker != nil {
		_, wakeErr := s.PushWaker.Wake(ctx, gitprojection.GitHubPushWake{GitHubAppID: envelope.AppID,
			InstallationID: push.InstallationID, RepositoryID: push.Repository.ID, TargetRef: push.Ref,
			AfterCommit: push.UntrustedAfter, DeliveryHash: "sha256:" + claimer.claimKey, ReceivedAt: envelope.ReceivedAt})
		if wakeErr != nil {
			return WebhookOutcome{}, nil, wakeErr
		}
	}
	return WebhookOutcome{ClaimKey: claimer.claimKey, State: DeliveryClaimed, Replay: claimer.replay}, event, nil
}

// ResumeDelivery lets a worker continue an authenticated receipt after a
// process crash or provider rate limit. Only the closed typed event is kept;
// the signed raw payload and every credential remain absent from storage.
func (s *WebhookService) ResumeDelivery(ctx context.Context, claimKey string) (WebhookOutcome, error) {
	if s == nil || s.Provider == nil || s.Store == nil || !validOwnerLease(s.Owner, s.LeaseDuration) || !regexpHex64(claimKey) {
		return WebhookOutcome{}, ErrInvalid
	}
	receipt, err := s.Store.Delivery(ctx, claimKey)
	if err != nil {
		return WebhookOutcome{}, err
	}
	// A terminal receipt is enough to replay the durable outcome after the
	// closed typed payload reaches its retention deadline and is purged. The
	// permanent one-time claim remains the authoritative replay tombstone.
	if terminalDelivery(receipt.State) {
		return WebhookOutcome{ClaimKey: claimKey, State: receipt.State, Replay: true}, nil
	}
	event, err := decodeTypedEvent(receipt.Event, receipt.TypedEvent)
	if err != nil {
		return WebhookOutcome{}, err
	}
	return s.processDelivery(ctx, claimKey, event, true)
}

func (s *WebhookService) processDelivery(ctx context.Context, claimKey string, event githubapp.Event, replay bool) (WebhookOutcome, error) {
	stored, acquired, acquireErr := s.Store.AcquireDelivery(ctx, claimKey, s.Owner, s.now(), s.LeaseDuration)
	if acquireErr != nil {
		return WebhookOutcome{}, acquireErr
	}
	outcome := WebhookOutcome{ClaimKey: claimKey, State: stored.State, Replay: replay}
	if !acquired {
		return outcome, nil
	}
	switch typed := event.(type) {
	case githubapp.InstallationEvent:
		applyErr := s.Store.ApplyInstallationEvent(ctx, stored.AppID, typed, s.now())
		if applyErr != nil {
			return s.finishDeliveryError(ctx, outcome, applyErr)
		}
		if finishErr := s.Store.FinishDelivery(ctx, claimKey, s.Owner, DeliveryIgnored, "", s.now()); finishErr != nil {
			return outcome, finishErr
		}
		outcome.State = DeliveryIgnored
		return outcome, nil
	case githubapp.InstallationRepositoriesEvent:
		applyErr := s.Store.ApplyRepositoryEvent(ctx, stored.AppID, typed, s.now())
		if applyErr != nil {
			return s.finishDeliveryError(ctx, outcome, applyErr)
		}
		if finishErr := s.Store.FinishDelivery(ctx, claimKey, s.Owner, DeliveryIgnored, "", s.now()); finishErr != nil {
			return outcome, finishErr
		}
		outcome.State = DeliveryIgnored
		return outcome, nil
	case githubapp.PushEvent:
		if typed.Deleted {
			if finishErr := s.Store.FinishDelivery(ctx, claimKey, s.Owner, DeliveryIgnored, "", s.now()); finishErr != nil {
				return outcome, finishErr
			}
			outcome.State = DeliveryIgnored
			return outcome, nil
		}
		authorized, authorizeErr := s.Store.AuthorizePush(ctx, stored.AppID, typed.InstallationID, typed.Repository, typed.Ref)
		if authorizeErr != nil {
			return s.finishDeliveryError(ctx, outcome, authorizeErr)
		}
		if len(authorized.Definitions) == 0 {
			if finishErr := s.Store.FinishDelivery(ctx, claimKey, s.Owner, DeliveryIgnored, "", s.now()); finishErr != nil {
				return outcome, finishErr
			}
			outcome.State = DeliveryIgnored
			return outcome, nil
		}
		required := githubapp.Permissions{"metadata": githubapp.PermissionRead, "contents": githubapp.PermissionRead}
		if heartbeatErr := s.Store.HeartbeatDelivery(ctx, claimKey, s.Owner, s.now(), s.LeaseDuration); heartbeatErr != nil {
			return outcome, heartbeatErr
		}
		if _, verifyErr := s.Provider.VerifyInstallation(ctx, typed.InstallationID, authorized.Installation.Account, required); verifyErr != nil {
			return s.providerFailure(ctx, outcome, verifyErr)
		}
		token, tokenErr := s.Provider.MintInstallationToken(ctx, githubapp.TokenRequest{
			InstallationID: typed.InstallationID, Account: authorized.Installation.Account,
			Repositories: []githubapp.RepositoryIdentity{authorized.Repository.Identity}, Permissions: required,
		})
		if tokenErr != nil {
			return s.providerFailure(ctx, outcome, tokenErr)
		}
		if heartbeatErr := s.Store.HeartbeatDelivery(ctx, claimKey, s.Owner, s.now(), s.LeaseDuration); heartbeatErr != nil {
			return outcome, heartbeatErr
		}
		resolved, resolveErr := s.Provider.ResolveEventRef(ctx, token, typed)
		if resolveErr != nil {
			return s.providerFailure(ctx, outcome, resolveErr)
		}
		if resolved.Ref != typed.Ref || !commitRE.MatchString(resolved.CommitSHA) || resolved.ResolvedAt.IsZero() {
			return s.finishDeliveryError(ctx, outcome, ErrUnauthorized)
		}
		attempts, enqueueErr := s.Store.EnqueuePushBuilds(ctx, EnqueuePush{ClaimKey: claimKey, CommitSHA: resolved.CommitSHA, GitRef: resolved.Ref, ResolvedAt: resolved.ResolvedAt}, s.Owner, authorized.Definitions, s.now())
		if enqueueErr != nil {
			if errors.Is(enqueueErr, ErrUnauthorized) {
				return s.finishDeliveryError(ctx, outcome, enqueueErr)
			}
			return outcome, enqueueErr
		}
		outcome.State = DeliveryEnqueued
		outcome.AttemptIDs = make([]string, len(attempts))
		for index := range attempts {
			outcome.AttemptIDs[index] = attempts[index].ID
		}
		return outcome, nil
	default:
		return WebhookOutcome{}, githubapp.ErrInvalidWebhook
	}
}

func decodeTypedEvent(name string, encoded []byte) (githubapp.Event, error) {
	switch name {
	case "push":
		var event githubapp.PushEvent
		if err := decodeClosedJSON(encoded, &event); err != nil || !validPushEvent(event) {
			return nil, ErrInvalid
		}
		return event, nil
	case "installation":
		var event githubapp.InstallationEvent
		if err := decodeClosedJSON(encoded, &event); err != nil || !validInstallationEvent(event) {
			return nil, ErrInvalid
		}
		return event, nil
	case "installation_repositories":
		var event githubapp.InstallationRepositoriesEvent
		if err := decodeClosedJSON(encoded, &event); err != nil || !validRepositoryEvent(event) {
			return nil, ErrInvalid
		}
		return event, nil
	default:
		return nil, ErrInvalid
	}
}

func (s *WebhookService) providerFailure(ctx context.Context, outcome WebhookOutcome, providerErr error) (WebhookOutcome, error) {
	if retryAt, retryable := providerRetryAt(providerErr, s.now()); retryable {
		now := s.now()
		if err := s.Store.RetryDelivery(ctx, outcome.ClaimKey, s.Owner, "github-provider-retry", now, retryAt); err != nil {
			return outcome, err
		}
		outcome.State = DeliveryClaimed
		return outcome, ErrProviderRetry
	}
	return s.finishDeliveryError(ctx, outcome, providerErr)
}

func (s *WebhookService) finishDeliveryError(ctx context.Context, outcome WebhookOutcome, cause error) (WebhookOutcome, error) {
	if errors.Is(cause, ErrNotFound) {
		now := s.now()
		if err := s.Store.RetryDelivery(ctx, outcome.ClaimKey, s.Owner, "github-installation-pending", now, now.Add(30*time.Second)); err != nil {
			return outcome, err
		}
		outcome.State = DeliveryClaimed
		return outcome, ErrProviderRetry
	}
	if err := s.Store.FinishDelivery(ctx, outcome.ClaimKey, s.Owner, DeliveryFailed, "github-authorization-failed", s.now()); err != nil {
		return outcome, err
	}
	outcome.State = DeliveryFailed
	return outcome, cause
}

func providerRetryAt(err error, now time.Time) (time.Time, bool) {
	var apiErr *githubapp.APIError
	if errors.As(err, &apiErr) && apiErr.Retryable() {
		if apiErr.RetryAt.After(now) {
			return apiErr.RetryAt.UTC(), true
		}
		return now.UTC().Add(30 * time.Second), true
	}
	if errors.Is(err, githubapp.ErrTransport) || errors.Is(err, githubapp.ErrSecretUnavailable) {
		return now.UTC().Add(30 * time.Second), true
	}
	return time.Time{}, false
}

func (s *WebhookService) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}
