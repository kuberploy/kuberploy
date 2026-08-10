package httpapi

import (
	"regexp"
	"strings"
)

var slugRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var digestImageRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
var uuidRE = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validSlug(v string) bool { return slugRE.MatchString(v) }
func validUUID(v string) bool { return uuidRE.MatchString(v) }
func slugify(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	var b strings.Builder
	dash := false
	for _, r := range v {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
func validHostname(v string) bool {
	if len(v) == 0 || len(v) > 253 || strings.HasSuffix(v, ".") {
		return false
	}
	for _, part := range strings.Split(v, ".") {
		if !validSlug(strings.ToLower(part)) {
			return false
		}
	}
	return strings.Contains(v, ".")
}
