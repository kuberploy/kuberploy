package config

import (
	"errors"
	"os"
	"strings"
)

func Get(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
func Required(key string) (string, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return "", errors.New(key + " is required")
	}
	return v, nil
}
func List(key, fallback string) []string {
	raw := Get(key, fallback)
	parts := strings.Split(raw, ",")
	out := parts[:0]
	for _, v := range parts {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
