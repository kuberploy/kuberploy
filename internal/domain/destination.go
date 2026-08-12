package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// DeriveEnvironmentDestination makes Kubernetes and Argo CD destinations a
// function of immutable platform-owned identity. It preserves readable short
// namespaces while hashing the identity into names that would exceed the DNS
// label limit.
func DeriveEnvironmentDestination(project Project, environmentSlug string) (string, string) {
	candidate := "kp-" + project.Slug + "-" + environmentSlug
	namespace := candidate
	if len(candidate) > 63 {
		sum := sha256.Sum256([]byte(project.ID + "\x00" + environmentSlug))
		suffix := hex.EncodeToString(sum[:5])
		prefix := strings.TrimRight(candidate[:52], "-")
		namespace = prefix + "-" + suffix
	}
	// One Argo AppProject owns one environment destination. Reusing a
	// project-wide AppProject in every environment manifest makes Argo CD see
	// the same resource more than once as soon as a project has two
	// environments. The namespace is already a server-derived, globally unique
	// DNS label, so it is also the stable environment-scoped AppProject name.
	return namespace, namespace
}
