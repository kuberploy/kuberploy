package httpapi

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/kuberploy/kuberploy/internal/secrets"
)

func TestSecretValuesRejectDuplicatesAndDestroyDecodedBuffers(t *testing.T) {
	var values secretValues
	if err := json.Unmarshal([]byte(`{"token":"private UTF-8 value"}`), &values); err != nil {
		t.Fatal(err)
	}
	owned := values["token"]
	if len(owned) == 0 {
		t.Fatal("value was not decoded")
	}
	values.Destroy()
	for index, value := range owned {
		if value != 0 {
			t.Fatalf("decoded byte %d was not cleared: %d", index, value)
		}
	}
	if len(values) != 0 {
		t.Fatalf("destroy retained map entries: %#v", values)
	}

	for _, raw := range []string{
		`{"token":"first","token":"second"}`,
		`{"bad key":"private"}`,
		`{"token":""}`,
	} {
		if err := json.Unmarshal([]byte(raw), &values); !errors.Is(err, secrets.ErrInvalid) {
			t.Fatalf("input %s err=%v", raw, err)
		}
	}
}
