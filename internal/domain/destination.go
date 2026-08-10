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
	idPart := strings.ReplaceAll(strings.ToLower(project.ID), "-", "")
	return namespace, "kp-p-" + idPart
}
