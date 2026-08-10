package passwordauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	Memory      = uint32(19 * 1024)
	Iterations  = uint32(2)
	Parallelism = uint8(1)
	SaltLength  = 16
	KeyLength   = 32
)

var ErrInvalid = errors.New("invalid password credential")

func Validate(password string) error {
	if len(password) < 12 || len(password) > 256 || strings.IndexByte(password, 0) >= 0 {
		return ErrInvalid
	}
	return nil
}

func Hash(password string) (string, error) {
	if err := Validate(password); err != nil {
		return "", err
	}
	salt := make([]byte, SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, Iterations, Memory, Parallelism, KeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, Memory,
		Iterations, Parallelism, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func Verify(encoded, password string) (bool, bool) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false, false
	}
	var memory, iterations uint64
	var parallel uint64
	for _, field := range strings.Split(parts[3], ",") {
		pair := strings.SplitN(field, "=", 2)
		if len(pair) != 2 {
			return false, false
		}
		value, err := strconv.ParseUint(pair[1], 10, 32)
		if err != nil {
			return false, false
		}
		switch pair[0] {
		case "m":
			memory = value
		case "t":
			iterations = value
		case "p":
			parallel = value
		default:
			return false, false
		}
	}
	// Bounds prevent a corrupted database value from becoming a memory/CPU DoS.
	if memory < 8*1024 || memory > 128*1024 || iterations < 1 || iterations > 8 || parallel < 1 || parallel > 4 {
		return false, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 16 || len(salt) > 32 {
		return false, false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false, false
	}
	got := argon2.IDKey([]byte(password), salt, uint32(iterations), uint32(memory), uint8(parallel), uint32(len(want)))
	ok := subtle.ConstantTimeCompare(got, want) == 1
	needsUpgrade := ok && (memory != uint64(Memory) || iterations != uint64(Iterations) || parallel != uint64(Parallelism) || len(want) != KeyLength)
	return ok, needsUpgrade
}

func DummyVerify(password string) {
	salt := []byte("kuberploy-dummy!!")
	_ = argon2.IDKey([]byte(password), salt, Iterations, Memory, Parallelism, KeyLength)
}
