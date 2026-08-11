package imagepull

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaximumDockerConfigBytes = 64 << 10

// ValidateDockerConfig validates only the closed Kubernetes image-pull subset
// of Docker's config.json. It deliberately returns no parsed credential value
// and never includes source bytes in errors. Callers own and must clear raw.
func ValidateDockerConfig(raw []byte, profile Profile) error {
	if profile.Validate() != nil || len(raw) < 1 || len(raw) > MaximumDockerConfigBytes || bytes.IndexByte(raw, 0) >= 0 {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ErrInvalid
	}
	seenAuths := false
	for decoder.More() {
		keyToken, keyErr := decoder.Token()
		key, ok := keyToken.(string)
		if keyErr != nil || !ok || key != "auths" || seenAuths {
			return ErrInvalid
		}
		seenAuths = true
		var auths json.RawMessage
		if err = decoder.Decode(&auths); err != nil || validateAuths(auths, profile.RegistryServer) != nil {
			return ErrInvalid
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || !seenAuths {
		return ErrInvalid
	}
	if token, err = decoder.Token(); err != io.EOF || token != nil {
		return ErrInvalid
	}
	return nil
}

func validateAuths(raw []byte, server string) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ErrInvalid
	}
	count := 0
	for decoder.More() {
		keyToken, keyErr := decoder.Token()
		key, ok := keyToken.(string)
		if keyErr != nil || !ok || key != server || count != 0 {
			return ErrInvalid
		}
		count++
		var entry json.RawMessage
		if err = decoder.Decode(&entry); err != nil || validateAuthEntry(entry) != nil {
			return ErrInvalid
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || count != 1 {
		return ErrInvalid
	}
	if token, err = decoder.Token(); err != io.EOF || token != nil {
		return ErrInvalid
	}
	return nil
}

func validateAuthEntry(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, keyErr := decoder.Token()
		key, ok := keyToken.(string)
		if keyErr != nil || !ok || seen[key] {
			return ErrInvalid
		}
		seen[key] = true
		if key != "auth" && key != "username" && key != "password" && key != "identitytoken" {
			return ErrInvalid
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil || !safeNonemptyJSONString(value) {
			return ErrInvalid
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return ErrInvalid
	}
	if token, err = decoder.Token(); err != io.EOF || token != nil {
		return ErrInvalid
	}
	if seen["username"] != seen["password"] {
		return ErrInvalid
	}
	if seen["identitytoken"] {
		if seen["auth"] || seen["username"] || len(seen) != 1 {
			return ErrInvalid
		}
		return nil
	}
	if !seen["auth"] && !seen["username"] {
		return ErrInvalid
	}
	if seen["auth"] && !validBasicAuthJSON(raw) {
		return ErrInvalid
	}
	return nil
}

func safeNonemptyJSONString(raw []byte) bool {
	if len(raw) < 3 || len(raw) > 16<<10 || raw[0] != '"' || raw[len(raw)-1] != '"' || !utf8.Valid(raw) {
		return false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" || len(value) > 16<<10 {
		return false
	}
	return strings.IndexFunc(value, unicode.IsControl) < 0
}

func validBasicAuthJSON(raw []byte) bool {
	var entry map[string]string
	if err := json.Unmarshal(raw, &entry); err != nil {
		return false
	}
	encoded := entry["auth"]
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) < 3 || len(decoded) > 16<<10 || base64.StdEncoding.EncodeToString(decoded) != encoded {
		clearBytes(decoded)
		return false
	}
	defer clearBytes(decoded)
	separator := bytes.IndexByte(decoded, ':')
	if separator < 1 || separator == len(decoded)-1 || !utf8.Valid(decoded) {
		return false
	}
	if strings.IndexFunc(string(decoded), unicode.IsControl) >= 0 {
		return false
	}
	username, hasUsername := entry["username"]
	password, hasPassword := entry["password"]
	if hasUsername != hasPassword {
		return false
	}
	if !hasUsername {
		return true
	}
	expected := make([]byte, 0, len(username)+1+len(password))
	expected = append(expected, username...)
	expected = append(expected, ':')
	expected = append(expected, password...)
	defer clearBytes(expected)
	return len(decoded) == len(expected) && subtle.ConstantTimeCompare(decoded, expected) == 1
}
