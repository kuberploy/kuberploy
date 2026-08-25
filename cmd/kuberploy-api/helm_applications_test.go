package main

import "testing"

func helmLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { value, ok := values[name]; return value, ok }
}

func TestDirectHelmApplicationsAPIIsDefaultOff(t *testing.T) {
	runtime, err := newHelmApplicationsAPIFromLookup(t.Context(), "not-a-database-url", nil, nil, helmLookup(nil))
	if err != nil || runtime != nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
}

func TestDirectHelmApplicationsAPIRejectsInvalidFeatureFlagBeforeIO(t *testing.T) {
	runtime, err := newHelmApplicationsAPIFromLookup(t.Context(), "not-a-database-url", nil, nil,
		helmLookup(map[string]string{helmApplicationsEnabledEnv: "sometimes"}))
	if err == nil || runtime != nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
}

func TestDirectHelmApplicationsAPIRequiresArgoNamespaceBeforeIO(t *testing.T) {
	runtime, err := newHelmApplicationsAPIFromLookup(t.Context(), "not-a-database-url", nil, nil,
		helmLookup(map[string]string{helmApplicationsEnabledEnv: "true"}))
	if err == nil || runtime != nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
}
