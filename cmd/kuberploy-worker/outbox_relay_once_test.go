package main

import (
	"bytes"
	"testing"
)

func TestOutboxRelayOnceRequiresDatabaseAuthorityBeforeNetworkUse(t *testing.T) {
	t.Setenv("KUBERPLOY_DATABASE_URL", "")
	var output bytes.Buffer
	if err := runOutboxRelayOnce(t.Context(), &output); err == nil || output.Len() != 0 {
		t.Fatalf("err=%v output=%q", err, output.String())
	}
}
