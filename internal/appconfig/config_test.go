package appconfig_test

import (
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
)

func validConfig(t *testing.T) []byte {
	t.Helper()
	p := domain.Project{ID: "11111111-1111-4111-8111-111111111111"}
	e := domain.Environment{ID: "22222222-2222-4222-8222-222222222222"}
	a := domain.Application{ID: "33333333-3333-4333-8333-333333333333", Slug: "api"}
	d := domain.Deployment{Image: "registry.example/api@sha256:" + strings.Repeat("a", 64), Runtime: domain.DefaultWorkloadRuntime(8080, nil)}
	raw, err := gitops.RenderAppConfig(p, e, a, d)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func hasDiagnostic(diagnostics []appconfig.Diagnostic, code, pointer string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code && diagnostic.Pointer == pointer {
			return true
		}
	}
	return false
}

func TestParseAndValidateRejectsDuplicateAndCoercedYAML(t *testing.T) {
	raw := string(validConfig(t))
	duplicate := strings.Replace(raw, "  name: \"api\"\n", "  name: \"api\"\n  name: \"other\"\n", 1)
	if _, _, diagnostics := appconfig.ParseAndValidate([]byte(duplicate)); len(diagnostics) == 0 || diagnostics[0].Code != "InvalidYAML" {
		t.Fatalf("duplicate mapping key accepted: %#v", diagnostics)
	}
	coerced := strings.Replace(raw, "    replicas: 1\n", "    replicas: true\n", 1)
	_, _, diagnostics := appconfig.ParseAndValidate([]byte(coerced))
	if !hasDiagnostic(diagnostics, "SchemaViolation", "/spec/runtime/replicas") {
		t.Fatalf("boolean replica coercion should fail at exact pointer: %#v", diagnostics)
	}
}

func TestApplyFailsClosedForLockedJSONPatchAndNestedScheduling(t *testing.T) {
	raw := validConfig(t)
	locked := appconfig.Apply(raw, appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "replace", Path: "/spec/delivery/release/digest", Value: "sha256:" + strings.Repeat("b", 64)}}})
	if !hasDiagnostic(locked.Diagnostics, "LockedField", "/spec/delivery/release/digest") {
		t.Fatalf("locked delivery patch bypassed comparison: %#v changes=%#v", locked.Diagnostics, locked.Changes)
	}
	scheduling := appconfig.Apply(raw, appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "add", Path: "/spec/runtime/nodeSelector", Value: map[string]any{"node-role.kubernetes.io/control-plane": "true"}}}})
	if !hasDiagnostic(scheduling.Diagnostics, "ReservedSchedulingKey", "/spec/runtime/nodeSelector/node-role.kubernetes.io~1control-plane") {
		t.Fatalf("reserved selector pointer is not path-level: %#v", scheduling.Diagnostics)
	}
}

func TestApplyAcceptsDirectSameApplicationScheduling(t *testing.T) {
	candidate := appconfig.Apply(validConfig(t), appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{
		Op: "add", Path: "/spec/runtime/affinity", Value: map[string]any{
			"podAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{
				"topologyKey":   "kubernetes.io/hostname",
				"labelSelector": map[string]any{"matchLabels": map[string]any{"kuberploy.io/application": "33333333-3333-4333-8333-333333333333"}},
			}}},
		},
	}}})
	if len(candidate.Diagnostics) != 0 || candidate.Runtime.Affinity == nil || candidate.Runtime.Affinity.PodAffinity == nil {
		t.Fatalf("direct same-app affinity rejected: %#v", candidate.Diagnostics)
	}

	crossApp := appconfig.Apply(validConfig(t), appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{
		Op: "add", Path: "/spec/runtime/topologySpreadConstraints", Value: []any{map[string]any{
			"maxSkew": 1, "topologyKey": "kubernetes.io/hostname", "whenUnsatisfiable": "DoNotSchedule",
			"labelSelector": map[string]any{"matchLabels": map[string]any{"kuberploy.io/application": "44444444-4444-4444-8444-444444444444"}},
		}},
	}}})
	if !hasDiagnostic(crossApp.Diagnostics, "ApplicationSelectorRequired", "/spec/runtime/topologySpreadConstraints/0/labelSelector") {
		t.Fatalf("cross-app topology selector accepted: %#v", crossApp.Diagnostics)
	}
}

func TestApplySupportsWorkloadTypeSpecificStrategies(t *testing.T) {
	candidate := appconfig.Apply(validConfig(t), appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{
		{Op: "add", Path: "/spec/runtime/workloadType", Value: "StatefulSet"},
		{Op: "replace", Path: "/spec/runtime/strategy/type", Value: "OnDelete"},
		{Op: "add", Path: "/spec/runtime/podManagementPolicy", Value: "Parallel"},
		{Op: "add", Path: "/spec/runtime/workingDirectory", Value: "/app"},
	}})
	if len(candidate.Diagnostics) != 0 || candidate.Runtime.WorkloadType != "StatefulSet" || candidate.Runtime.WorkingDirectory != "/app" {
		t.Fatalf("valid StatefulSet runtime rejected: %#v", candidate.Diagnostics)
	}
}

func TestApplyTracksEditableArrayElement(t *testing.T) {
	candidate := appconfig.Apply(validConfig(t), appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "replace", Path: "/spec/runtime/ports/0/containerPort", Value: 9090}}})
	if len(candidate.Diagnostics) != 0 || len(candidate.Changes) != 1 || candidate.Changes[0].Pointer != "/spec/runtime/ports" {
		// Arrays are intentionally summarized at their editable root; locked
		// arrays would still fail closed because their root is not editable.
		t.Fatalf("array change not represented deterministically: diagnostics=%#v changes=%#v", candidate.Diagnostics, candidate.Changes)
	}
}

func TestApplyAllowsProbeChangesAtTheirEditableRoot(t *testing.T) {
	candidate := appconfig.Apply(validConfig(t), appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{
		Op:   "add",
		Path: "/spec/runtime/probes",
		Value: map[string]any{
			"readiness": map[string]any{
				"httpGet":       map[string]any{"path": "/ready", "port": "http"},
				"periodSeconds": 5,
			},
		},
	}}})
	if len(candidate.Diagnostics) != 0 || len(candidate.Changes) != 1 || candidate.Changes[0].Pointer != "/spec/runtime/probes" {
		t.Fatalf("probe change not represented at editable root: diagnostics=%#v changes=%#v", candidate.Diagnostics, candidate.Changes)
	}
}

func TestAppConfigSecretReferenceRequiresLockedIdentityAndIntegerVersion(t *testing.T) {
	valid := appconfig.Apply(validConfig(t), appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{
		Op: "add", Path: "/spec/runtime/env", Value: []any{map[string]any{
			"name": "DATABASE_PASSWORD", "valueFrom": map[string]any{"secretBindingRef": map[string]any{
				"bindingId": "44444444-4444-4444-8444-444444444444", "name": "database", "key": "password", "version": 3,
			}},
		}},
	}}})
	if len(valid.Diagnostics) != 0 || len(valid.Runtime.Env) != 1 || valid.Runtime.Env[0].ValueFrom == nil ||
		valid.Runtime.Env[0].ValueFrom.SecretBindingRef.BindingID != "44444444-4444-4444-8444-444444444444" ||
		valid.Runtime.Env[0].ValueFrom.SecretBindingRef.Version != 3 {
		t.Fatalf("valid exact reference failed: diagnostics=%#v runtime=%#v", valid.Diagnostics, valid.Runtime)
	}

	for name, reference := range map[string]map[string]any{
		"missing binding id": {"name": "database", "key": "password", "version": 3},
		"string version":     {"bindingId": "44444444-4444-4444-8444-444444444444", "name": "database", "key": "password", "version": "v3"},
		"zero version":       {"bindingId": "44444444-4444-4444-8444-444444444444", "name": "database", "key": "password", "version": 0},
		"invalid binding id": {"bindingId": "database", "name": "database", "key": "password", "version": 3},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := appconfig.Apply(validConfig(t), appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{
				Op: "add", Path: "/spec/runtime/env", Value: []any{map[string]any{
					"name": "DATABASE_PASSWORD", "valueFrom": map[string]any{"secretBindingRef": reference},
				}},
			}}})
			if len(candidate.Diagnostics) == 0 {
				t.Fatalf("invalid reference accepted: %#v", candidate.Runtime)
			}
		})
	}
}

func TestAppConfigRegistryPullMetadataIsStrictAndLocked(t *testing.T) {
	current := validConfig(t)
	private := strings.Replace(string(current), "    release:\n", "    registryPull:\n      targetId: 44444444-4444-4444-8444-444444444444\n      profileName: managed-main\n      profileRevision: 7\n    release:\n", 1)
	if _, _, diagnostics := appconfig.ParseAndValidate([]byte(private)); len(diagnostics) != 0 {
		t.Fatalf("server-derived registry pull metadata failed validation: %#v", diagnostics)
	}
	candidate := appconfig.Apply(current, appconfig.Change{Mode: "yaml", Documents: []appconfig.DocumentChange{{DocumentID: appconfig.DocumentID, RawYAML: private}}})
	if !hasDiagnostic(candidate.Diagnostics, "LockedField", "/spec/delivery/registryPull") {
		t.Fatalf("registry pull metadata was caller-editable: diagnostics=%#v changes=%#v", candidate.Diagnostics, candidate.Changes)
	}
	unsafe := strings.Replace(private, "      profileRevision: 7\n", "      profileRevision: 7\n      secretName: attacker-selected\n", 1)
	if _, _, diagnostics := appconfig.ParseAndValidate([]byte(unsafe)); !hasDiagnostic(diagnostics, "SchemaViolation", "/spec/delivery/registryPull") &&
		!hasDiagnostic(diagnostics, "SchemaViolation", "/spec/delivery/registryPull/secretName") {
		t.Fatalf("caller-selected Secret name was accepted: %#v", diagnostics)
	}
}

func TestAppConfigCustomCertificateRequiresExactBindingIdentity(t *testing.T) {
	base := string(validConfig(t))
	withReference := func(reference string) []byte {
		routes := "  routes:\n" +
			"    - host: api.example.test\n" +
			"      path: /\n" +
			"      port: http\n" +
			"      tls:\n" +
			"        mode: customCertificate\n" + reference
		return []byte(strings.Replace(base, "  runtime:\n", routes+"  runtime:\n", 1))
	}
	validReference :=
		"        secretRef:\n" +
			"          bindingId: 44444444-4444-4444-8444-444444444444\n" +
			"          name: route-certificate\n" +
			"          version: 9223372036854775807\n"
	if _, _, diagnostics := appconfig.ParseAndValidate(withReference(validReference)); len(diagnostics) != 0 {
		t.Fatalf("exact custom certificate binding identity failed validation: %#v", diagnostics)
	}

	for name, reference := range map[string]string{
		"legacy string":         "        secretRef: caller-selected-secret\n",
		"missing binding id":    "        secretRef:\n          name: route-certificate\n          version: 7\n",
		"missing reviewed name": "        secretRef:\n          bindingId: 44444444-4444-4444-8444-444444444444\n          version: 7\n",
		"missing version":       "        secretRef:\n          bindingId: 44444444-4444-4444-8444-444444444444\n          name: route-certificate\n",
		"caller target name":    "        secretRef:\n          bindingId: 44444444-4444-4444-8444-444444444444\n          name: route-certificate\n          version: 7\n          secretName: caller-selected-secret\n",
		"invalid binding id":    "        secretRef:\n          bindingId: route-certificate\n          name: route-certificate\n          version: 7\n",
		"invalid DNS name":      "        secretRef:\n          bindingId: 44444444-4444-4444-8444-444444444444\n          name: Caller_Selected_Secret\n          version: 7\n",
		"zero version":          "        secretRef:\n          bindingId: 44444444-4444-4444-8444-444444444444\n          name: route-certificate\n          version: 0\n",
		"string version":        "        secretRef:\n          bindingId: 44444444-4444-4444-8444-444444444444\n          name: route-certificate\n          version: \"7\"\n",
		"version above int64":   "        secretRef:\n          bindingId: 44444444-4444-4444-8444-444444444444\n          name: route-certificate\n          version: 9223372036854775808\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, diagnostics := appconfig.ParseAndValidate(withReference(reference)); len(diagnostics) == 0 {
				t.Fatal("invalid custom certificate identity was accepted")
			}
		})
	}
}
