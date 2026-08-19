// Package operationcache implements a disposable, revision-keyed cache for
// the safe operation-status projection returned by the API. PostgreSQL remains
// the authorization and revision authority on every read.
package operationcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	valkey "github.com/valkey-io/valkey-go"
)

const (
	cacheSchema     = "kuberploy.operation-status-cache/v1"
	cacheKeyPrefix  = "kp:v1:cache:operation-status:"
	maximumValue    = 64 << 10
	maximumCacheTTL = 2 * time.Minute
	cacheIOTimeout  = 2 * time.Second
)

var (
	uuidPattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	hexDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Identity is calculated from an authorization check and the current
// PostgreSQL row metadata. A Valkey value can only satisfy this exact identity.
type Identity struct {
	OperationID    string
	ScopeHash      string
	Generation     int64
	SourceRevision string
	bodyRevision   string
}

// NewIdentity binds a cache entry to the current principal grant revision and
// exact operation row revision. Caller-controlled scope strings never become
// Valkey keys.
func NewIdentity(actorID string, grantRevision int64, operationID string, generation int64, updatedAt time.Time) (Identity, error) {
	if !uuidPattern.MatchString(actorID) || grantRevision < 1 || !uuidPattern.MatchString(operationID) || generation < 1 || updatedAt.IsZero() {
		return Identity{}, errors.New("operation cache identity metadata is invalid")
	}
	scope := sha256.Sum256([]byte(actorID + "\x00" + strconv.FormatInt(grantRevision, 10)))
	revision := sha256.Sum256([]byte(operationID + "\x00" + strconv.FormatInt(generation, 10) + "\x00" + updatedAt.UTC().Format(time.RFC3339Nano)))
	bodyRevision := "sha256:" + hex.EncodeToString(revision[:])
	return Identity{
		OperationID: operationID, ScopeHash: hex.EncodeToString(scope[:]), Generation: generation,
		SourceRevision: bodyRevision, bodyRevision: bodyRevision,
	}, nil
}

// WithSourceRevision adds the revision of a joined PostgreSQL projection (for
// example pull-request publication state) without weakening body validation.
func (i Identity) WithSourceRevision(parts ...string) (Identity, error) {
	if !i.valid() || len(parts) == 0 || len(parts) > 8 {
		return Identity{}, errors.New("operation cache projection revision is invalid")
	}
	input := i.SourceRevision
	for _, part := range parts {
		if len(part) > 128 || strings.ContainsAny(part, "\x00\r\n") {
			return Identity{}, errors.New("operation cache projection revision is invalid")
		}
		input += "\x00" + part
	}
	digest := sha256.Sum256([]byte(input))
	i.SourceRevision = "sha256:" + hex.EncodeToString(digest[:])
	return i, nil
}

func (i Identity) publicValid() bool {
	return uuidPattern.MatchString(i.OperationID) && hexDigestPattern.MatchString(i.ScopeHash) && i.Generation >= 1 &&
		strings.HasPrefix(i.SourceRevision, "sha256:") && hexDigestPattern.MatchString(strings.TrimPrefix(i.SourceRevision, "sha256:"))
}

func (i Identity) valid() bool {
	return i.publicValid() && strings.HasPrefix(i.bodyRevision, "sha256:") && hexDigestPattern.MatchString(strings.TrimPrefix(i.bodyRevision, "sha256:"))
}

func (i Identity) key() string {
	return cacheKeyPrefix + i.ScopeHash + ":" + i.OperationID + ":" + strconv.FormatInt(i.Generation, 10) + ":" + strings.TrimPrefix(i.SourceRevision, "sha256:")
}

// MatchesOperation proves that the cached body came from the same operation
// row revision used in the key, closing races between identity and body reads.
func (i Identity) MatchesOperation(operation domain.Operation) bool {
	return safeOperation(i, operation)
}

// Cache is the narrow HTTP seam. Errors are cache misses from the caller's
// perspective and must never replace a PostgreSQL read.
type Cache interface {
	Load(context.Context, Identity) (domain.Operation, bool, error)
	Store(context.Context, Identity, domain.Operation) error
}

type Options struct {
	Addresses          []string
	Username, Password string
	TTL                time.Duration
}

type ValkeyCache struct {
	client valkey.Client
	ttl    time.Duration
	now    func() time.Time
}

func NewValkeyCache(options Options) (*ValkeyCache, error) {
	if len(options.Addresses) == 0 {
		return nil, errors.New("Valkey operation cache requires at least one address")
	}
	for _, address := range options.Addresses {
		if strings.TrimSpace(address) == "" || len(address) > 512 || strings.ContainsAny(address, "\x00\r\n") {
			return nil, errors.New("Valkey operation cache address is invalid")
		}
	}
	if options.TTL == 0 {
		options.TTL = 30 * time.Second
	}
	if options.TTL < time.Second || options.TTL > maximumCacheTTL {
		return nil, fmt.Errorf("operation cache TTL must be between 1s and %s", maximumCacheTTL)
	}
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress: options.Addresses, Username: options.Username, Password: options.Password,
		ClientName: "kuberploy-operation-cache", DisableCache: true, DisableRetry: true,
		Dialer: net.Dialer{Timeout: cacheIOTimeout}, ConnWriteTimeout: cacheIOTimeout,
	})
	if err != nil {
		return nil, err
	}
	return &ValkeyCache{client: client, ttl: options.TTL, now: time.Now}, nil
}

func (c *ValkeyCache) Close() {
	if c != nil && c.client != nil {
		c.client.Close()
	}
}

func (c *ValkeyCache) Ping(ctx context.Context) error {
	if c == nil || c.client == nil {
		return errors.New("Valkey operation cache is not configured")
	}
	return c.client.Do(ctx, c.client.B().Ping().Build()).Error()
}

type envelope struct {
	Schema         string           `json:"schema"`
	ScopeHash      string           `json:"scopeHash"`
	OperationID    string           `json:"operationId"`
	Generation     int64            `json:"generation"`
	SourceRevision string           `json:"sourceRevision"`
	StoredAt       time.Time        `json:"storedAt"`
	ExpiresAt      time.Time        `json:"expiresAt"`
	Value          domain.Operation `json:"value"`
}

func (c *ValkeyCache) Load(ctx context.Context, identity Identity) (domain.Operation, bool, error) {
	if c == nil || c.client == nil || !identity.valid() {
		return domain.Operation{}, false, errors.New("operation cache identity is invalid")
	}
	key := identity.key()
	encoded, err := c.client.Do(ctx, c.client.B().Get().Key(key).Build()).ToString()
	if valkey.IsValkeyNil(err) {
		return domain.Operation{}, false, nil
	}
	if err != nil {
		return domain.Operation{}, false, err
	}
	value, decodeErr := decodeEnvelope([]byte(encoded), identity, c.now().UTC(), c.ttl)
	if decodeErr != nil {
		_ = c.client.Do(ctx, c.client.B().Del().Key(key).Build()).Error()
		return domain.Operation{}, false, decodeErr
	}
	return value, true, nil
}

func (c *ValkeyCache) Store(ctx context.Context, identity Identity, operation domain.Operation) error {
	if c == nil || c.client == nil || !identity.valid() {
		return errors.New("operation cache identity is invalid")
	}
	now := c.now().UTC()
	encoded, err := encodeEnvelope(identity, operation, now, c.ttl)
	if err != nil {
		return err
	}
	return c.client.Do(ctx, c.client.B().Set().Key(identity.key()).Value(string(encoded)).Ex(c.ttl).Build()).Error()
}

func encodeEnvelope(identity Identity, operation domain.Operation, now time.Time, ttl time.Duration) ([]byte, error) {
	if !identity.valid() || ttl < time.Second || ttl > maximumCacheTTL || !safeOperation(identity, operation) {
		return nil, errors.New("operation cache value is invalid")
	}
	encoded, err := json.Marshal(envelope{Schema: cacheSchema, ScopeHash: identity.ScopeHash, OperationID: identity.OperationID,
		Generation: identity.Generation, SourceRevision: identity.SourceRevision, StoredAt: now.UTC(), ExpiresAt: now.UTC().Add(ttl), Value: operation})
	if err != nil {
		return nil, err
	}
	if len(encoded) > maximumValue {
		return nil, errors.New("operation cache envelope exceeds its byte limit")
	}
	return encoded, nil
}

func decodeEnvelope(encoded []byte, identity Identity, now time.Time, ttl time.Duration) (domain.Operation, error) {
	if len(encoded) == 0 || len(encoded) > maximumValue || !identity.valid() {
		return domain.Operation{}, errors.New("operation cache envelope has an invalid size or identity")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var value envelope
	if err := decoder.Decode(&value); err != nil {
		return domain.Operation{}, fmt.Errorf("decode operation cache envelope: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return domain.Operation{}, errors.New("operation cache envelope must contain exactly one JSON document")
	}
	if value.Schema != cacheSchema || value.ScopeHash != identity.ScopeHash || value.OperationID != identity.OperationID ||
		value.Generation != identity.Generation || value.SourceRevision != identity.SourceRevision || value.StoredAt.IsZero() || value.ExpiresAt.IsZero() ||
		value.StoredAt.After(now.Add(time.Minute)) || !value.ExpiresAt.After(now) || value.ExpiresAt.Sub(value.StoredAt) != ttl || !safeOperation(identity, value.Value) {
		return domain.Operation{}, errors.New("operation cache envelope metadata is invalid")
	}
	return value.Value, nil
}

func safeOperation(identity Identity, operation domain.Operation) bool {
	if operation.ID != identity.OperationID || operation.Generation != identity.Generation || operation.CreatedAt.IsZero() || operation.UpdatedAt.IsZero() ||
		len(operation.Kind) == 0 || len(operation.Kind) > 64 || (operation.Status != "queued" && operation.Status != "running") || operation.Problem != nil ||
		operation.PullRequest != nil || !uuidPattern.MatchString(operation.RequestID) || len(operation.TargetType) == 0 || len(operation.TargetType) > 64 ||
		!uuidPattern.MatchString(operation.TargetID) || len(operation.Progress) > 64 {
		return false
	}
	for _, step := range operation.Progress {
		// Worker error/detail strings can contain provider or renderer text.
		// Cache only the closed metadata-only pending/running projection.
		if step.Detail != "" || len(step.Name) == 0 || len(step.Name) > 64 || (step.Status != "pending" && step.Status != "running") {
			return false
		}
	}
	revision := sha256.Sum256([]byte(operation.ID + "\x00" + strconv.FormatInt(operation.Generation, 10) + "\x00" + operation.UpdatedAt.UTC().Format(time.RFC3339Nano)))
	if identity.bodyRevision != "sha256:"+hex.EncodeToString(revision[:]) {
		return false
	}
	encoded, err := json.Marshal(operation)
	return err == nil && len(encoded) <= maximumValue/2
}
