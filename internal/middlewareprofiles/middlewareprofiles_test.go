package middlewareprofiles

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kuberploy/kuberploy/internal/id"
)

func number(value string) json.Number { return json.Number(value) }
func validCommand(actor, key string, now time.Time) Command {
	return Command{ActorID: actor, IdempotencyKey: key, RequestID: "request-" + key, Now: now}
}

func TestClosedMiddlewareValidatorRejectsSchemaBypassAndTrailingJSON(t *testing.T) {
	valid := Spec{"headers": map[string]any{"frameDeny": true, "customResponseHeaders": map[string]any{"X-Policy": "safe"}}}
	if ValidateDefinition("security", valid) != nil {
		t.Fatal("valid bounded headers rejected")
	}
	cases := []Spec{
		{"headers": map[string]any{"forwardAuth": map[string]any{"address": "http://169.254.169.254"}}},
		{"headers": map[string]any{"customRequestHeaders": map[string]any{"Authorization": "secret"}}},
		{"ipAllowList": map[string]any{"sourceRange": []any{"10.0.0.1/8"}}},
		{"redirectRegex": map[string]any{"regex": "(?=unsafe)", "replacement": "/"}},
		{"plugin": map[string]any{"unsafe": map[string]any{}}},
		{"basicAuth": map[string]any{"secret": "caller-secret"}},
	}
	for index, candidate := range cases {
		if ValidateSpec(candidate) == nil {
			t.Fatalf("unsafe case %d accepted", index)
		}
	}
	if _, err := DecodeSpec([]byte(`{"compress":{}} {"retry":{"attempts":1}}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("concatenated JSON accepted: %v", err)
	}
}

func TestBasicAuthAcceptsOnlyWriteOnlyBindingIdentity(t *testing.T) {
	ref := map[string]any{"bindingId": id.New(), "name": "admin-users", "key": "users", "version": number("2")}
	spec := Spec{"basicAuth": map[string]any{"secretBindingRef": ref, "removeHeader": true}}
	if ValidateSpec(spec) != nil {
		t.Fatal("exact BasicAuth identity rejected")
	}
	ref["key"] = "password"
	if ValidateSpec(spec) == nil {
		t.Fatal("arbitrary BasicAuth key accepted")
	}
	ref["key"] = "users"
	spec["basicAuth"].(map[string]any)["secret"] = "target-secret"
	if ValidateSpec(spec) == nil {
		t.Fatal("caller Kubernetes Secret name accepted")
	}
}

func TestMemoryProfilesAreImmutableAssignedAndReferenceGuarded(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	actor := id.New()
	project := id.New()
	environment := id.New()
	application := id.New()
	now := time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)
	spec := Spec{"rateLimit": map[string]any{"average": number("100"), "burst": number("200")}}
	created, err := store.Create(ctx, validCommand(actor, "create-profile", now), "api-limit", spec, []Assignment{{Scope: ProjectScope, ID: project}})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.Create(ctx, validCommand(actor, "create-profile", now), "api-limit", spec, []Assignment{{Scope: ProjectScope, ID: project}})
	if err != nil || !replay.Replay || replay.Profile.ID != created.Profile.ID {
		t.Fatalf("idempotent replay failed: %#v %v", replay, err)
	}
	resolver, _ := NewResolver(store)
	ref := Ref{ProfileID: created.Profile.ID, Revision: 1, SpecDigest: created.Revision.SpecDigest, AssignmentsDigest: created.Revision.AssignmentsDigest}
	if _, err = resolver.Resolve(ctx, ref, Target{ProjectID: project, EnvironmentID: environment, ApplicationID: application}); err != nil {
		t.Fatal(err)
	}
	if _, err = resolver.Resolve(ctx, ref, Target{ProjectID: id.New(), EnvironmentID: environment, ApplicationID: application}); !errors.Is(err, ErrUnassigned) {
		t.Fatalf("cross-project resolve: %v", err)
	}
	refs := []Reference{{ProfileID: created.Profile.ID, Revision: 1, ApplicationID: application, EnvironmentID: environment, GitPath: "apps/" + application + "/app.yaml", LogicalName: "api-limit"}}
	if err = store.ReplaceReferences(application, environment, refs[0].GitPath, refs); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Deactivate(ctx, validCommand(actor, "deactivate-profile", now.Add(time.Minute)), Ref{ProfileID: created.Profile.ID, Revision: 1}); !errors.Is(err, ErrReferenced) {
		t.Fatalf("referenced deactivate=%v", err)
	}
	if err = store.ReplaceReferences(application, environment, refs[0].GitPath, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Deactivate(ctx, validCommand(actor, "deactivate-profile-2", now.Add(2*time.Minute)), Ref{ProfileID: created.Profile.ID, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	catalog, err := store.Catalog(ctx, 10)
	if err != nil || len(catalog) != 0 {
		t.Fatalf("deleted profile remained in active catalog: %#v %v", catalog, err)
	}
}

func TestMemoryReferenceReplacementIsAtomicAndRequiresCurrentProfile(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	actor, project, environment, application := id.New(), id.New(), id.New(), id.New()
	now := time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)
	created, err := store.Create(ctx, validCommand(actor, "create-reference-profile", now), "api-limit", Spec{"rateLimit": map[string]any{"average": number("100"), "burst": number("200")}}, []Assignment{{Scope: ProjectScope, ID: project}})
	if err != nil {
		t.Fatal(err)
	}
	path := "apps/" + application + "/app.yaml"
	current := Reference{ProfileID: created.Profile.ID, Revision: 1, ApplicationID: application, EnvironmentID: environment, GitPath: path, LogicalName: "api-limit"}
	if err = store.ReplaceReferences(application, environment, path, []Reference{current}); err != nil {
		t.Fatal(err)
	}

	invalid := current
	invalid.LogicalName = "INVALID"
	if err = store.ReplaceReferences(application, environment, path, []Reference{invalid}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid replacement error = %v", err)
	}
	refs, err := store.References(ctx, created.Profile.ID, 10)
	if err != nil || len(refs) != 1 || refs[0] != current {
		t.Fatalf("failed replacement changed references: %#v %v", refs, err)
	}

	missing := current
	missing.ProfileID = id.New()
	if err = store.ReplaceReferences(application, environment, path, []Reference{missing}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing profile error = %v", err)
	}
	refs, err = store.References(ctx, created.Profile.ID, 10)
	if err != nil || len(refs) != 1 || refs[0] != current {
		t.Fatalf("missing profile replacement changed references: %#v %v", refs, err)
	}

	if _, err = store.Revise(ctx, validCommand(actor, "revise-reference-profile", now.Add(time.Minute)), Ref{ProfileID: created.Profile.ID, Revision: 1}, Spec{"rateLimit": map[string]any{"average": number("200"), "burst": number("400")}}, []Assignment{{Scope: ProjectScope, ID: project}}); err != nil {
		t.Fatal(err)
	}
	stale := current
	if err = store.ReplaceReferences(application, environment, path, []Reference{stale}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale profile error = %v", err)
	}
	refs, err = store.References(ctx, created.Profile.ID, 10)
	if err != nil || len(refs) != 1 || refs[0] != current {
		t.Fatalf("stale replacement changed references: %#v %v", refs, err)
	}
}
