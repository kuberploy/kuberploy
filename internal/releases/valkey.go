package releases

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	valkey "github.com/valkey-io/valkey-go"
)

const (
	releaseCacheKey        = "kp:v1:cache:platform-release:latest"
	releaseCacheSchema     = "kuberploy.release-cache/v1"
	maxReleaseCacheBytes   = 512 << 10
	maximumReleaseCacheTTL = 10 * time.Minute
	releaseCacheIOTimeout  = 2 * time.Second
)

type ValkeyCacheOptions struct {
	Addresses []string
	Username  string
	Password  string
}

// ValkeyCache is a disposable cross-replica release metadata cache. Exact
// immutable manifest bytes are retained and revalidated after every cache
// read; a Valkey value is never treated as a release trust authority.
type ValkeyCache struct {
	client valkey.Client
}

type releaseCacheEnvelope struct {
	Schema         string    `json:"schema"`
	ManifestDigest string    `json:"manifestDigest"`
	ManifestBytes  []byte    `json:"manifestBytes"`
	PublishedAt    time.Time `json:"publishedAt"`
	UpstreamETag   string    `json:"upstreamETag"`
	LastCheckedAt  time.Time `json:"lastCheckedAt"`
}

func NewValkeyCache(options ValkeyCacheOptions) (*ValkeyCache, error) {
	if len(options.Addresses) == 0 {
		return nil, errors.New("Valkey release cache requires at least one address")
	}
	for _, address := range options.Addresses {
		if strings.TrimSpace(address) == "" || len(address) > 512 || strings.ContainsAny(address, "\x00\r\n") {
			return nil, errors.New("Valkey release cache address is invalid")
		}
	}
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress: options.Addresses, Username: options.Username, Password: options.Password,
		ClientName: "kuberploy-release-cache", DisableCache: true, DisableRetry: true,
		Dialer: net.Dialer{Timeout: releaseCacheIOTimeout}, ConnWriteTimeout: releaseCacheIOTimeout,
	})
	if err != nil {
		return nil, err
	}
	return &ValkeyCache{client: client}, nil
}

func (c *ValkeyCache) Close() {
	if c != nil && c.client != nil {
		c.client.Close()
	}
}

func (c *ValkeyCache) Ping(ctx context.Context) error {
	if c == nil || c.client == nil {
		return errors.New("Valkey release cache is not configured")
	}
	return c.client.Do(ctx, c.client.B().Ping().Build()).Error()
}

func (c *ValkeyCache) Load(ctx context.Context) (Snapshot, bool, error) {
	if c == nil || c.client == nil {
		return Snapshot{}, false, errors.New("Valkey release cache is not configured")
	}
	encoded, err := c.client.Do(ctx, c.client.B().Get().Key(releaseCacheKey).Build()).ToString()
	if valkey.IsValkeyNil(err) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, err
	}
	snapshot, err := decodeReleaseCache([]byte(encoded), time.Now().UTC())
	if err != nil {
		// Corrupt cache data is disposable. Best-effort deletion prevents every
		// API replica from repeatedly parsing the same invalid value.
		_ = c.client.Do(ctx, c.client.B().Del().Key(releaseCacheKey).Build()).Error()
		return Snapshot{}, false, err
	}
	return snapshot, true, nil
}

func (c *ValkeyCache) Store(ctx context.Context, snapshot Snapshot, ttl time.Duration) error {
	if c == nil || c.client == nil {
		return errors.New("Valkey release cache is not configured")
	}
	if ttl < time.Second || ttl > maximumReleaseCacheTTL {
		return fmt.Errorf("release cache TTL must be between 1s and %s", maximumReleaseCacheTTL)
	}
	encoded, err := encodeReleaseCache(snapshot)
	if err != nil {
		return err
	}
	return c.client.Do(ctx, c.client.B().Set().Key(releaseCacheKey).Value(string(encoded)).Ex(ttl).Build()).Error()
}

func encodeReleaseCache(snapshot Snapshot) ([]byte, error) {
	release := snapshot.Release
	manifest, err := ParseExactManifest(release.ManifestBytes, release.ManifestDigest)
	if err != nil {
		return nil, fmt.Errorf("cache exact release manifest: %w", err)
	}
	if release.Tag != manifest.Release.Tag || release.Version != manifest.Release.Version || release.PublishedAt.IsZero() ||
		snapshot.LastCheckedAt.IsZero() || snapshot.LastCheckedAt.Location() != time.UTC || release.PublishedAt.Location() != time.UTC ||
		len(snapshot.UpstreamETag) > 512 || strings.ContainsAny(snapshot.UpstreamETag, "\x00\r\n") {
		return nil, errors.New("release cache snapshot metadata is invalid")
	}
	envelope := releaseCacheEnvelope{Schema: releaseCacheSchema, ManifestDigest: release.ManifestDigest,
		ManifestBytes: append([]byte(nil), release.ManifestBytes...), PublishedAt: release.PublishedAt,
		UpstreamETag: snapshot.UpstreamETag, LastCheckedAt: snapshot.LastCheckedAt}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxReleaseCacheBytes {
		return nil, errors.New("release cache envelope exceeds its byte limit")
	}
	return encoded, nil
}

func decodeReleaseCache(encoded []byte, now time.Time) (Snapshot, error) {
	if len(encoded) == 0 || len(encoded) > maxReleaseCacheBytes {
		return Snapshot{}, errors.New("release cache envelope has an invalid size")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var envelope releaseCacheEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return Snapshot{}, fmt.Errorf("decode release cache envelope: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Snapshot{}, errors.New("release cache envelope must contain exactly one JSON document")
	}
	if envelope.Schema != releaseCacheSchema || envelope.PublishedAt.IsZero() || envelope.LastCheckedAt.IsZero() ||
		envelope.PublishedAt.Location() != time.UTC || envelope.LastCheckedAt.Location() != time.UTC ||
		len(envelope.UpstreamETag) > 512 || strings.ContainsAny(envelope.UpstreamETag, "\x00\r\n") {
		return Snapshot{}, errors.New("release cache envelope metadata is invalid")
	}
	now = now.UTC()
	if now.IsZero() || envelope.LastCheckedAt.After(now.Add(time.Minute)) || envelope.PublishedAt.After(now.Add(time.Minute)) {
		return Snapshot{}, errors.New("release cache envelope contains a future timestamp")
	}
	manifest, err := ParseExactManifest(envelope.ManifestBytes, envelope.ManifestDigest)
	if err != nil {
		return Snapshot{}, fmt.Errorf("validate cached release manifest: %w", err)
	}
	return Snapshot{Release: domain.ReleaseInfo{Tag: manifest.Release.Tag, Version: manifest.Release.Version,
		ManifestDigest: envelope.ManifestDigest, Manifest: manifest, ManifestBytes: append([]byte(nil), envelope.ManifestBytes...),
		PublishedAt: envelope.PublishedAt}, UpstreamETag: envelope.UpstreamETag, LastCheckedAt: envelope.LastCheckedAt}, nil
}
