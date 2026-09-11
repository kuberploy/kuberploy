package id

import "testing"

func TestValid(t *testing.T) {
	if value := New(); !Valid(value) {
		t.Fatalf("generated ID is invalid: %q", value)
	}
	for _, value := range []string{"", "not-a-uuid", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaag"} {
		if Valid(value) {
			t.Fatalf("invalid ID accepted: %q", value)
		}
	}
}
