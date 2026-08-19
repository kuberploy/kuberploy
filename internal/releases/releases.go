package releases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

var ErrNotModified = errors.New("release metadata not modified")

type FetchResult struct {
	Release     domain.ReleaseInfo
	ETag        string
	NotModified bool
}

// Checker fetches only the canonical Kuberploy public release source.
// cachedETag enables a conditional request without making cache storage part
// of the trust boundary.
type Checker interface {
	Latest(context.Context, string) (FetchResult, error)
}

type Snapshot struct {
	Release       domain.ReleaseInfo `json:"release"`
	UpstreamETag  string             `json:"upstreamETag"`
	LastCheckedAt time.Time          `json:"lastCheckedAt"`
}

// Cache is deliberately small and disposable. The production Valkey adapter
// retains exact manifest bytes and revalidates them on read; a cache miss or
// outage never changes release validation.
type Cache interface {
	Load(context.Context) (Snapshot, bool, error)
	Store(context.Context, Snapshot, time.Duration) error
}

type Service struct {
	checker  Checker
	cache    Cache
	ttl      time.Duration
	now      func() time.Time
	mu       sync.Mutex
	local    Snapshot
	hasLocal bool
}

func NewService(checker Checker, cache Cache, ttl time.Duration) *Service {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &Service{checker: checker, cache: cache, ttl: ttl, now: time.Now}
}

func (s *Service) Latest(ctx context.Context) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	if s.hasLocal && now.Sub(s.local.LastCheckedAt) < s.ttl {
		return s.local, nil
	}
	if !s.hasLocal && s.cache != nil {
		cacheCtx, cancel := context.WithTimeout(ctx, releaseCacheIOTimeout)
		cached, ok, err := s.cache.Load(cacheCtx)
		cancel()
		if err == nil && ok {
			s.local = cached
			s.hasLocal = true
			if now.Sub(cached.LastCheckedAt) < s.ttl {
				return cached, nil
			}
		}
	}
	etag := ""
	if s.hasLocal {
		etag = s.local.UpstreamETag
	}
	result, err := s.checker.Latest(ctx, etag)
	if err != nil {
		return Snapshot{}, err
	}
	if result.NotModified {
		if !s.hasLocal {
			return Snapshot{}, ErrNotModified
		}
		s.local.LastCheckedAt = now
	} else {
		s.local = Snapshot{Release: result.Release, UpstreamETag: result.ETag, LastCheckedAt: now}
		s.hasLocal = true
	}
	if s.cache != nil {
		cacheCtx, cancel := context.WithTimeout(ctx, releaseCacheIOTimeout)
		_ = s.cache.Store(cacheCtx, s.local, s.ttl)
		cancel()
	}
	return s.local, nil
}

var stableVersionRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var releaseVersionRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-rc\.([1-9][0-9]*))?$`)
var sha256RE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var commitRE = regexp.MustCompile(`^[a-f0-9]{40}$`)
var schemaNameRE = regexp.MustCompile(`^[0-9]{3}_[a-z0-9_]+$`)

func ValidStableVersion(v string) bool  { return stableVersionRE.MatchString(v) }
func ValidReleaseVersion(v string) bool { return releaseVersionRE.MatchString(v) }
func CompareVersions(a, b string) (int, error) {
	pa, err := versionParts(a)
	if err != nil {
		return 0, err
	}
	pb, err := versionParts(b)
	if err != nil {
		return 0, err
	}
	for i := 0; i < 3; i++ {
		if pa.core[i] < pb.core[i] {
			return -1, nil
		}
		if pa.core[i] > pb.core[i] {
			return 1, nil
		}
	}
	if pa.rc == pb.rc {
		return 0, nil
	}
	if pa.rc == 0 {
		return 1, nil
	}
	if pb.rc == 0 {
		return -1, nil
	}
	if pa.rc < pb.rc {
		return -1, nil
	}
	return 1, nil
}

type releaseVersion struct {
	core [3]uint64
	rc   uint64
}

func versionParts(v string) (releaseVersion, error) {
	v = strings.TrimPrefix(v, "v")
	m := releaseVersionRE.FindStringSubmatch(v)
	if m == nil {
		return releaseVersion{}, fmt.Errorf("invalid release semantic version %q", v)
	}
	var p releaseVersion
	for i := range p.core {
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return p, err
		}
		p.core[i] = n
	}
	if m[4] != "" {
		var err error
		p.rc, err = strconv.ParseUint(m[4], 10, 64)
		if err != nil {
			return p, err
		}
	}
	return p, nil
}
func ParseUpgradeRange(value string) (string, string, error) {
	parts := strings.Split(value, " ")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], ">=") || !strings.HasPrefix(parts[1], "<") {
		return "", "", errors.New("supportedUpgradeFrom must be exactly >=VERSION <VERSION")
	}
	minimum, maximum := strings.TrimPrefix(parts[0], ">="), strings.TrimPrefix(parts[1], "<")
	if !ValidStableVersion(minimum) || !ValidStableVersion(maximum) {
		return "", "", errors.New("supportedUpgradeFrom contains an invalid stable version")
	}
	comparison, _ := CompareVersions(minimum, maximum)
	if comparison >= 0 {
		return "", "", errors.New("supportedUpgradeFrom range is empty or reversed")
	}
	return minimum, maximum, nil
}
func SupportsUpgrade(version, value string) (bool, error) {
	minimum, maximum, err := ParseUpgradeRange(value)
	if err != nil {
		return false, err
	}
	installed := strings.TrimPrefix(version, "v")
	if parts := releaseVersionRE.FindStringSubmatch(installed); parts != nil && parts[4] != "" {
		installed = strings.Join(parts[1:4], ".")
	}
	low, err := CompareVersions(installed, minimum)
	if err != nil {
		return false, err
	}
	high, err := CompareVersions(installed, maximum)
	if err != nil {
		return false, err
	}
	return low >= 0 && high < 0, nil
}
func SchemaInWindow(installed, minimum, maximum string) (bool, error) {
	if !schemaNameRE.MatchString(installed) || !schemaNameRE.MatchString(minimum) || !schemaNameRE.MatchString(maximum) {
		return false, errors.New("database schema names must use NNN_name")
	}
	return installed >= minimum && installed <= maximum, nil
}

func ValidateManifest(m domain.ReleaseManifest) error {
	if m.SchemaVersion != "1.0.0" {
		return errors.New("manifest schemaVersion must be 1.0.0")
	}
	if !ValidReleaseVersion(m.Release.Version) || m.Release.Tag != "v"+m.Release.Version || m.Release.CreatedAt.IsZero() {
		return errors.New("manifest version must be a stable or explicit RC semantic version")
	}
	if m.Source.Repository != "kuberploy/kuberploy" || !commitRE.MatchString(m.Source.Commit) {
		return errors.New("manifest source must be an exact kuberploy/kuberploy commit")
	}
	if m.Schema != "https://raw.githubusercontent.com/kuberploy/kuberploy/"+m.Source.Commit+"/release/release-manifest.schema.json" {
		return errors.New("manifest $schema must be bound to its exact canonical source commit")
	}
	if m.Release.NotesURL != "https://github.com/kuberploy/kuberploy/releases/tag/"+m.Release.Tag {
		return errors.New("manifest notesUrl must be the canonical release tag URL")
	}
	if len(m.Release.Summary) < 1 || len(m.Release.Summary) > 500 {
		return errors.New("manifest summary must contain 1 to 500 bytes")
	}
	for name, version := range map[string]string{"kuberploy": m.Versions.Kuberploy, "api": m.Versions.API, "worker": m.Versions.Worker, "web": m.Versions.Web, "migration": m.Versions.Migration, "upgrader": m.Versions.Upgrader, "builderAgent": m.Versions.BuilderAgent, "chart": m.Versions.Chart} {
		if version != m.Release.Version {
			return fmt.Errorf("manifest %s version does not match release version", name)
		}
	}
	if _, _, err := ParseUpgradeRange(m.Compatibility.SupportedUpgradeFrom); err != nil {
		return err
	}
	if m.Compatibility.Kubernetes.Constraint != ">=1.34.0-0 <1.37.0-0" || len(m.Compatibility.Kubernetes.TestedMinors) != 3 || m.Compatibility.Kubernetes.TestedMinors[0] != "1.34" || m.Compatibility.Kubernetes.TestedMinors[1] != "1.35" || m.Compatibility.Kubernetes.TestedMinors[2] != "1.36" {
		return errors.New("manifest Kubernetes compatibility does not match the v1 contract")
	}
	database := m.Compatibility.Database
	if database.Engine != "postgresql" || !schemaNameRE.MatchString(database.CurrentSchema) || !schemaNameRE.MatchString(database.MinimumUpgradeableSchema) || database.MinimumUpgradeableSchema > database.CurrentSchema || !sha256RE.MatchString(database.MigrationSetSHA256) || database.Strategy != "prisma-migrate-deploy-with-advisory-lock" || len(database.RollbackPolicy) < 20 || len(database.RollbackPolicy) > 512 {
		return errors.New("manifest database compatibility is invalid")
	}
	wantedComponents := []string{"api", "worker", "web", "migration", "upgrader", "builder-agent"}
	if len(m.Artifacts.Images) != len(wantedComponents) {
		return errors.New("manifest must contain exactly six component images")
	}
	for i, image := range m.Artifacts.Images {
		component := wantedComponents[i]
		if image.Component != component || image.Reference != "ghcr.io/kuberploy/kuberploy-"+component || !sha256RE.MatchString(image.Digest) || len(image.Platforms) != 2 || image.Platforms[0] != "linux/amd64" || image.Platforms[1] != "linux/arm64" {
			return fmt.Errorf("manifest image %s is invalid or out of canonical order", component)
		}
	}
	chart := m.Artifacts.Chart
	if chart.Name != "kuberploy" || chart.Version != m.Release.Version || chart.OCIReference != "ghcr.io/kuberploy/charts/kuberploy:"+m.Release.Version || !sha256RE.MatchString(chart.OCIDigest) || chart.Package != "kuberploy-"+m.Release.Version+".tgz" || !sha256RE.MatchString(chart.PackageSHA256) {
		return errors.New("manifest chart artifact is invalid")
	}
	wantedCharts := []string{
		"kuberploy-argocd",
		"kuberploy-installer",
		"kuberploy-builder",
		"kuberploy-cert-manager",
		"kuberploy-edge",
		"kuberploy-external-dns",
		"kuberploy-external-secrets",
		"kuberploy-monitoring",
		"kuberploy-postgresql",
		"kuberploy-registry",
		"kuberploy-runtime",
		"kuberploy-sealed-secrets",
		"kuberploy-valkey",
	}
	if len(m.Artifacts.ComponentCharts) != len(wantedCharts) {
		return errors.New("manifest must contain exactly thirteen component charts")
	}
	for i, componentChart := range m.Artifacts.ComponentCharts {
		name := wantedCharts[i]
		if componentChart.Name != name || componentChart.Version != m.Release.Version || componentChart.OCIReference != "ghcr.io/kuberploy/charts/"+name+":"+m.Release.Version || !sha256RE.MatchString(componentChart.OCIDigest) || componentChart.Package != name+"-"+m.Release.Version+".tgz" || !sha256RE.MatchString(componentChart.PackageSHA256) {
			return fmt.Errorf("manifest component chart %s is invalid or out of canonical order", name)
		}
	}
	if m.DependencyLock.File != "DEPENDENCIES.md" || !sha256RE.MatchString(m.DependencyLock.SHA256) {
		return errors.New("manifest dependency lock is invalid")
	}
	return nil
}

// ParseExactManifest binds a parsed manifest to the exact immutable asset
// bytes. JSONB reserialization is deliberately not accepted as an identity.
func ParseExactManifest(body []byte, expectedDigest string) (domain.ReleaseManifest, error) {
	if len(body) == 0 || int64(len(body)) > maxManifestBytes || !ManifestDigestValid(expectedDigest) {
		return domain.ReleaseManifest{}, errors.New("release manifest bytes or digest are invalid")
	}
	sum := sha256.Sum256(body)
	if actual := "sha256:" + hex.EncodeToString(sum[:]); actual != expectedDigest {
		return domain.ReleaseManifest{}, errors.New("exact release manifest bytes do not match manifestDigest")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var manifest domain.ReleaseManifest
	if err := decoder.Decode(&manifest); err != nil {
		return domain.ReleaseManifest{}, fmt.Errorf("decode strict release manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return domain.ReleaseManifest{}, errors.New("release manifest must contain exactly one JSON object")
	}
	if err := ValidateManifest(manifest); err != nil {
		return domain.ReleaseManifest{}, err
	}
	return manifest, nil
}

func ManifestDigestValid(v string) bool { return sha256RE.MatchString(v) }
