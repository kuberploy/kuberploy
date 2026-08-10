package queue

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/id"
	valkey "github.com/valkey-io/valkey-go"
)

type ValkeyOptions struct {
	Addresses                         []string
	Username, Password, Stream, Group string
	ClientName                        string
	MaxLen                            int
	ClaimIdle                         time.Duration
}
type ValkeyStream struct {
	client          valkey.Client
	stream, group   string
	maxLen          int
	claimIdleMillis int64
}

var (
	streamNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,127}$`)
	consumerPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,127}$`)
	uuidPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	kindPattern       = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,63}$`)
	streamIDPattern   = regexp.MustCompile(`^[0-9]+-[0-9]+$`)
)

func NewValkeyStream(o ValkeyOptions) (*ValkeyStream, error) {
	if len(o.Addresses) == 0 {
		return nil, errors.New("Valkey address is required")
	}
	if o.Stream == "" {
		o.Stream = "kp:v1:work:git-write"
	}
	if o.Group == "" {
		o.Group = "kuberploy-git-writers"
	}
	if o.MaxLen == 0 {
		o.MaxLen = 10000
	}
	if o.ClaimIdle == 0 {
		o.ClaimIdle = 2 * time.Minute
	}
	if o.ClientName == "" {
		o.ClientName = "kuberploy-work-stream"
	}
	if !consumerPattern.MatchString(o.ClientName) {
		return nil, errors.New("Valkey client name must be a bounded canonical identifier")
	}
	if !streamNamePattern.MatchString(o.Stream) || !streamNamePattern.MatchString(o.Group) {
		return nil, errors.New("Valkey stream and group names must be bounded canonical identifiers")
	}
	if o.MaxLen < 100 || o.MaxLen > 1_000_000 {
		return nil, errors.New("Valkey stream maximum length must be between 100 and 1000000")
	}
	if o.ClaimIdle < 10*time.Millisecond || o.ClaimIdle > time.Hour {
		return nil, errors.New("Valkey pending reclaim idle time must be between 10ms and 1h")
	}
	for _, address := range o.Addresses {
		if strings.TrimSpace(address) == "" || len(address) > 512 || strings.ContainsAny(address, "\x00\r\n") {
			return nil, errors.New("Valkey address is invalid")
		}
	}
	c, err := valkey.NewClient(valkey.ClientOption{InitAddress: o.Addresses, Username: o.Username, Password: o.Password, ClientName: o.ClientName, DisableCache: true})
	if err != nil {
		return nil, err
	}
	return &ValkeyStream{client: c, stream: o.Stream, group: o.Group, maxLen: o.MaxLen, claimIdleMillis: o.ClaimIdle.Milliseconds()}, nil
}
func (v *ValkeyStream) Close() { v.client.Close() }
func (v *ValkeyStream) Ping(ctx context.Context) error {
	return v.client.Do(ctx, v.client.B().Ping().Build()).Error()
}

const datasetIdentityKey = "kp:v1:work:dataset-id"

// DatasetIdentity returns a stable opaque identity for the current Valkey
// dataset. FLUSHDB, a replacement cluster, or loss of the persistence volume
// removes this sentinel; the first publisher then installs a fresh identity.
// PostgreSQL compares it with its durable observation before publishing work.
func (v *ValkeyStream) DatasetIdentity(ctx context.Context) (string, error) {
	identity, err := v.client.Do(ctx, v.client.B().Get().Key(datasetIdentityKey).Build()).ToString()
	if valkey.IsValkeyNil(err) {
		candidate := id.New()
		if setErr := v.client.Do(ctx, v.client.B().Set().Key(datasetIdentityKey).Value(candidate).Nx().Build()).Error(); setErr != nil && !valkey.IsValkeyNil(setErr) {
			return "", fmt.Errorf("establish Valkey dataset identity: %w", setErr)
		}
		identity, err = v.client.Do(ctx, v.client.B().Get().Key(datasetIdentityKey).Build()).ToString()
	}
	if err != nil {
		return "", fmt.Errorf("read Valkey dataset identity: %w", err)
	}
	if !uuidPattern.MatchString(identity) {
		return "", errors.New("Valkey dataset identity is invalid")
	}
	return identity, nil
}

func (v *ValkeyStream) Publish(ctx context.Context, w domain.WorkMessage) error {
	if !validWorkMessage(w, false) {
		return errors.New("refusing to publish an invalid Valkey work envelope")
	}
	cmd := v.client.B().Arbitrary("XADD", v.stream, "MAXLEN", "~", strconv.Itoa(v.maxLen), "*", "operationId", w.OperationID, "kind", w.Kind, "scopeId", w.ScopeID, "generation", strconv.FormatInt(w.Generation, 10), "traceId", w.TraceID).Build()
	return v.client.Do(ctx, cmd).Error()
}
func (v *ValkeyStream) ensureGroup(ctx context.Context) error {
	err := v.client.Do(ctx, v.client.B().Arbitrary("XGROUP", "CREATE", v.stream, v.group, "0", "MKSTREAM").Build()).Error()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}
func (v *ValkeyStream) Receive(ctx context.Context, consumer string, limit int) ([]domain.WorkMessage, error) {
	if !consumerPattern.MatchString(consumer) {
		return nil, errors.New("Valkey consumer name is invalid")
	}
	if err := v.ensureGroup(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 1000 {
		limit = 1000
	}
	reclaimed, err := v.reclaim(ctx, consumer, limit)
	if err != nil {
		return nil, err
	}
	if len(reclaimed) != 0 {
		return reclaimed, nil
	}
	cmd := v.client.B().Arbitrary("XREADGROUP", "GROUP", v.group, consumer, "COUNT", strconv.Itoa(limit), "BLOCK", "10", "STREAMS", v.stream, ">").Blocking()
	result, err := v.client.Do(ctx, cmd).AsXRead()
	if valkey.IsValkeyNil(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return v.parseAndDiscardInvalid(ctx, result[v.stream])
}
func (v *ValkeyStream) Ack(ctx context.Context, w domain.WorkMessage) error {
	if w.DeliveryID == "" {
		return nil
	}
	if !streamIDPattern.MatchString(w.DeliveryID) {
		return errors.New("Valkey delivery ID is invalid")
	}
	return v.client.Do(ctx, v.client.B().Arbitrary("XACK", v.stream, v.group, w.DeliveryID).Build()).Error()
}

func (v *ValkeyStream) reclaim(ctx context.Context, consumer string, limit int) ([]domain.WorkMessage, error) {
	command := v.client.B().Xautoclaim().Key(v.stream).Group(v.group).Consumer(consumer).
		MinIdleTime(strconv.FormatInt(v.claimIdleMillis, 10)).Start("0-0").Count(int64(limit)).Build()
	parts, err := v.client.Do(ctx, command).ToArray()
	if valkey.IsValkeyNil(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(parts) != 2 && len(parts) != 3 {
		return nil, fmt.Errorf("invalid XAUTOCLAIM response length %d", len(parts))
	}
	entries, err := parts[1].AsXRange()
	if err != nil {
		return nil, fmt.Errorf("decode XAUTOCLAIM entries: %w", err)
	}
	return v.parseAndDiscardInvalid(ctx, entries)
}

func (v *ValkeyStream) parseAndDiscardInvalid(ctx context.Context, entries []valkey.XRangeEntry) ([]domain.WorkMessage, error) {
	valid, invalid := parseWorkEntries(entries)
	if len(invalid) != 0 {
		command := v.client.B().Xack().Key(v.stream).Group(v.group).Id(invalid...).Build()
		if err := v.client.Do(ctx, command).Error(); err != nil {
			return nil, fmt.Errorf("acknowledge malformed Valkey work envelopes: %w", err)
		}
	}
	return valid, nil
}

func parseWorkEntries(entries []valkey.XRangeEntry) ([]domain.WorkMessage, []string) {
	valid := make([]domain.WorkMessage, 0, len(entries))
	invalid := make([]string, 0)
	for _, entry := range entries {
		fields := entry.FieldValues
		generation, err := strconv.ParseInt(fields["generation"], 10, 64)
		message := domain.WorkMessage{
			OperationID: fields["operationId"], Kind: fields["kind"], ScopeID: fields["scopeId"],
			Generation: generation, TraceID: fields["traceId"], DeliveryID: entry.ID,
		}
		if err != nil || len(fields) != 5 || !validWorkMessage(message, true) {
			if streamIDPattern.MatchString(entry.ID) {
				invalid = append(invalid, entry.ID)
			}
			continue
		}
		valid = append(valid, message)
	}
	return valid, invalid
}

func validWorkMessage(message domain.WorkMessage, requireDelivery bool) bool {
	if !uuidPattern.MatchString(message.OperationID) || !uuidPattern.MatchString(message.ScopeID) ||
		!kindPattern.MatchString(message.Kind) || message.Generation < 1 || message.Generation > 1_000_000_000 ||
		len(message.TraceID) < 1 || len(message.TraceID) > 128 || strings.TrimSpace(message.TraceID) != message.TraceID || strings.ContainsAny(message.TraceID, "\x00\r\n") {
		return false
	}
	return !requireDelivery || streamIDPattern.MatchString(message.DeliveryID)
}
