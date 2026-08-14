package certificates

import (
	"slices"
	"testing"
)

func TestReferenceSelectionsDeduplicateSharedHostPaths(t *testing.T) {
	selection := ReferenceSelection{Host: "api.example.test", Reference: Reference{
		BindingID: "77777777-7777-4777-8777-777777777777", Name: "tenant-secret", Version: 7,
	}}
	got := deduplicateReferenceSelections([]ReferenceSelection{selection, selection})
	if len(got) != 1 || got[0] != selection {
		t.Fatalf("shared-host certificate selection was rejected or changed: %#v", got)
	}
}

func TestCertificateReferenceInsertOrderIsDeterministic(t *testing.T) {
	desired := map[string]string{
		"cccccccc-cccc-4ccc-8ccc-cccccccccccc": "33333333-3333-4333-8333-333333333333",
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa": "11111111-1111-4111-8111-111111111111",
		"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb": "22222222-2222-4222-8222-222222222222",
	}
	want := []string{
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		"cccccccc-cccc-4ccc-8ccc-cccccccccccc",
	}
	for index := 0; index < 32; index++ {
		if got := sortedCertificateBindingIDs(desired); !slices.Equal(got, want) {
			t.Fatalf("nondeterministic certificate lock order: %#v", got)
		}
	}
}
