package httpapi

import (
	"net/http/httptest"
	"testing"
)

func TestCollectionSerializesTypedNilSliceAsEmptyArray(t *testing.T) {
	t.Parallel()

	var items []string
	recorder := httptest.NewRecorder()
	collection(recorder, items)

	if recorder.Code != 200 {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got, want := recorder.Body.String(), "{\"items\":[]}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}
