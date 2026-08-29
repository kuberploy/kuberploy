package httpapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/ratelimit"
)

const highRiskLimitTimeout = 750 * time.Millisecond

type highRiskPolicy struct {
	bucket string
	limit  int64
	window time.Duration
}

var (
	bootstrapLimit           = highRiskPolicy{bucket: "auth-bootstrap", limit: 5, window: 15 * time.Minute}
	loginLimit               = highRiskPolicy{bucket: "auth-login", limit: 30, window: 15 * time.Minute}
	invitationAcceptLimit    = highRiskPolicy{bucket: "auth-invitation-accept", limit: 30, window: 15 * time.Minute}
	invitationIssueLimit     = highRiskPolicy{bucket: "auth-invitation-issue", limit: 20, window: time.Hour}
	accessControlLimit       = highRiskPolicy{bucket: "access-control", limit: 100, window: time.Hour}
	serviceAccountLimit      = highRiskPolicy{bucket: "service-account", limit: 30, window: time.Hour}
	secretCreateLimit        = highRiskPolicy{bucket: "runtime-secret-create", limit: 30, window: time.Hour}
	secretRotateLimit        = highRiskPolicy{bucket: "runtime-secret-rotate", limit: 60, window: time.Hour}
	secretDeleteLimit        = highRiskPolicy{bucket: "runtime-secret-delete", limit: 30, window: time.Hour}
	certificateCreateLimit   = highRiskPolicy{bucket: "certificate-binding-create", limit: 20, window: time.Hour}
	certificateRotateLimit   = highRiskPolicy{bucket: "certificate-binding-rotate", limit: 30, window: time.Hour}
	certificateDeleteLimit   = highRiskPolicy{bucket: "certificate-binding-delete", limit: 20, window: time.Hour}
	githubSetupLimit         = highRiskPolicy{bucket: "github-setup", limit: 20, window: time.Hour}
	gitBindingLimit          = highRiskPolicy{bucket: "git-binding", limit: 30, window: time.Hour}
	gitSSHKeyLimit           = highRiskPolicy{bucket: "git-ssh-key", limit: 30, window: time.Hour}
	variableSetMutationLimit = highRiskPolicy{bucket: "variable-set-mutation", limit: 120, window: time.Hour}
	platformGitBindingLimit  = highRiskPolicy{bucket: "argo-platform-git-binding", limit: 10, window: time.Hour}
	buildDefinitionLimit     = highRiskPolicy{bucket: "build-definition", limit: 30, window: time.Hour}
	buildCommandLimit        = highRiskPolicy{bucket: "build-command", limit: 60, window: time.Hour}
	deploymentRollbackLimit  = highRiskPolicy{bucket: "deployment-rollback", limit: 60, window: time.Hour}
	registryTargetLimit      = highRiskPolicy{bucket: "registry-target", limit: 30, window: time.Hour}
	registryPolicyLimit      = highRiskPolicy{bucket: "registry-policy", limit: 60, window: time.Hour}
	registryPreviewLimit     = highRiskPolicy{bucket: "registry-cleanup-preview", limit: 30, window: time.Hour}
	registryExecuteLimit     = highRiskPolicy{bucket: "registry-cleanup-execute", limit: 10, window: time.Hour}
	externalDNSManageLimit   = highRiskPolicy{bucket: "external-dns-integration", limit: 30, window: time.Hour}
)

type rateLimitSubject func(*http.Request) (string, error)

func remoteRateLimitSubject(r *http.Request) (string, error) {
	value := r.RemoteAddr
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	peer, err := netip.ParseAddr(value)
	if err != nil || peer.Zone() != "" {
		return "", errors.New("request client address is unavailable")
	}
	// The managed public path is Traefik -> web Nginx -> API. Traefik
	// sanitizes X-Forwarded-For at the public boundary and Nginx appends its
	// transport peer before forwarding to the API. The first address is
	// therefore the client identity; Forwarded and X-Real-IP remain ignored.
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		value, _, _ = strings.Cut(forwarded, ",")
		value = strings.TrimSpace(value)
		if value == "" {
			return "", errors.New("request forwarded client address is unavailable")
		}
	}
	address, err := netip.ParseAddr(value)
	if err != nil || address.Zone() != "" {
		return "", errors.New("request client address is unavailable")
	}
	return "ip:" + address.Unmap().String(), nil
}

func actorRateLimitSubject(r *http.Request) (string, error) {
	user := currentUser(r.Context())
	if user.ID == "" {
		return "", errors.New("authenticated actor is unavailable")
	}
	return "user:" + user.ID, nil
}

func (s *Server) highRiskRemote(policy highRiskPolicy, next http.Handler) http.Handler {
	return s.highRiskLimit(policy, remoteRateLimitSubject, next)
}

func (s *Server) highRiskActor(policy highRiskPolicy, next http.Handler) http.Handler {
	return s.highRiskLimit(policy, actorRateLimitSubject, next)
}

func (s *Server) highRiskLimit(policy highRiskPolicy, subject rateLimitSubject, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := subject(r)
		if err != nil || s.highRiskLimiter == nil {
			rateLimitUnavailable(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), highRiskLimitTimeout)
		decision, err := s.highRiskLimiter.Allow(ctx, ratelimit.Request{
			Bucket: policy.bucket, Subject: identity, Limit: policy.limit, Window: policy.window, Cost: 1,
		})
		cancel()
		if err != nil {
			rateLimitUnavailable(w, r)
			return
		}
		w.Header().Set("RateLimit-Limit", strconv.FormatInt(policy.limit, 10))
		w.Header().Set("RateLimit-Remaining", strconv.FormatInt(decision.Remaining, 10))
		w.Header().Set("RateLimit-Reset", strconv.FormatInt(ceilSeconds(decision.RetryAfter), 10))
		if !decision.Allowed {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", strconv.FormatInt(ceilSeconds(decision.RetryAfter), 10))
			writeProblem(w, r, http.StatusTooManyRequests, "RateLimitExceeded", "Too many requests", "Retry this operation after the interval in Retry-After.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func rateLimitUnavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "1")
	writeProblem(w, r, http.StatusServiceUnavailable, "RateLimitUnavailable", "Temporarily unavailable", "Distributed abuse protection is unavailable; retry this operation later.")
}

func ceilSeconds(duration time.Duration) int64 {
	seconds := int64((duration + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}
