package main

import "testing"

func TestValkeyCredentialPreservesExactSecretBytesAndFallsBack(t *testing.T) {
	t.Setenv("KUBERPLOY_TEST_VALKEY_ROLE_PASSWORD", "  exact password bytes  ")
	if got := valkeyCredential("KUBERPLOY_TEST_VALKEY_ROLE_PASSWORD", "fallback"); got != "  exact password bytes  " {
		t.Fatal("role password bytes were normalized")
	}
	t.Setenv("KUBERPLOY_TEST_VALKEY_ROLE_PASSWORD", "")
	if got := valkeyCredential("KUBERPLOY_TEST_VALKEY_ROLE_PASSWORD", "fallback"); got != "fallback" {
		t.Fatal("empty role password did not use the explicit compatibility credential")
	}
}
