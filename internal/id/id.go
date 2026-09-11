package id

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns a UUIDv4 without introducing a runtime UUID dependency.
func New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

// Valid reports whether value has the canonical UUID text shape used by IDs.
func Valid(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	var encoded [32]byte
	copy(encoded[0:8], value[0:8])
	copy(encoded[8:12], value[9:13])
	copy(encoded[12:16], value[14:18])
	copy(encoded[16:20], value[19:23])
	copy(encoded[20:32], value[24:36])
	var decoded [16]byte
	_, err := hex.Decode(decoded[:], encoded[:])
	return err == nil
}
