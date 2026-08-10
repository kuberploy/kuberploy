package appconfig_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kuberploy/kuberploy/internal/appconfig"
)

func TestAutoDeployImageMutationPreservesPinnedAdvancedIntent(t *testing.T) {
	base := validConfig(t)
	advanced := appconfig.Apply(base, appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{
		{Op: "add", Path: "/spec/routes", Value: []any{map[string]any{"id": "internal", "host": "10-0-0-1.sslip.io", "path": "/", "port": "http", "dns": map[string]any{"mode": "sslip"}, "tls": map[string]any{"mode": "httpOnly"}, "middlewareRefs": []any{"secure"}}}},
		{Op: "add", Path: "/spec/middlewares", Value: []any{map[string]any{"name": "secure", "spec": map[string]any{"headers": map[string]any{"frameDeny": true}}}}},
	}})
	if len(advanced.Diagnostics) != 0 {
		t.Fatalf("advanced config: %#v", advanced.Diagnostics)
	}
	intent, digest, diagnostics := appconfig.AutoDeployIntentTemplate(advanced.Raw)
	if len(diagnostics) != 0 || !appconfig.ValidateAutoDeployIntentTemplate(intent, digest) {
		t.Fatalf("intent=%s diagnostics=%#v", intent, diagnostics)
	}
	if bytes.Contains(intent, []byte("10-0-0-1.sslip.io")) || bytes.Contains(intent, []byte(strings.Repeat("a", 64))) {
		t.Fatalf("derived sslip host or image remained: %s", intent)
	}
	image := "registry.example/api@sha256:" + strings.Repeat("b", 64)
	candidate := appconfig.ApplyAutoDeployImage(advanced.Raw, intent, digest, image)
	if len(candidate.Diagnostics) != 0 || !bytes.Contains(candidate.Raw, []byte(strings.Repeat("b", 64))) || !bytes.Contains(candidate.Raw, []byte("frameDeny")) {
		t.Fatalf("candidate=%s diagnostics=%#v", candidate.Raw, candidate.Diagnostics)
	}
	changed := appconfig.Apply(advanced.Raw, appconfig.Change{Mode: "jsonPatch", Patch: []appconfig.PatchOperation{{Op: "replace", Path: "/spec/runtime/replicas", Value: 3}}})
	conflict := appconfig.ApplyAutoDeployImage(changed.Raw, intent, digest, image)
	if len(conflict.Diagnostics) != 1 || conflict.Diagnostics[0].Code != "AutoDeployTemplateConflict" {
		t.Fatalf("changed intent accepted: %#v", conflict.Diagnostics)
	}
}

func TestAutoDeployDependencyIntentPausesOnParentChangeButAllowsPriorImageAdvance(t *testing.T) {
	base := validConfig(t)
	intent, _, diagnostics := appconfig.AutoDeployIntentTemplate(base)
	if len(diagnostics) != 0 {
		t.Fatalf("template diagnostics=%#v", diagnostics)
	}
	parents := []appconfig.AutoDeployDependencyIntent{
		{Path: "variables/project.yaml", Present: true, BlobID: strings.Repeat("1", 40), ContentSHA256: "sha256:" + strings.Repeat("a", 64)},
		{Path: "variables/environment.yaml"},
	}
	intent, digest, err := appconfig.BindAutoDeployDependencies(intent, parents)
	if err != nil || !appconfig.ValidateAutoDeployIntentTemplate(intent, digest) {
		t.Fatalf("bind dependencies intent=%s digest=%q err=%v", intent, digest, err)
	}
	first := appconfig.ApplyAutoDeployImageWithDependencies(base, intent, digest,
		"registry.example/api@sha256:"+strings.Repeat("b", 64), parents)
	if len(first.Diagnostics) != 0 {
		t.Fatalf("first image advance diagnostics=%#v", first.Diagnostics)
	}
	second := appconfig.ApplyAutoDeployImageWithDependencies(first.Raw, intent, digest,
		"registry.example/api@sha256:"+strings.Repeat("c", 64), parents)
	if len(second.Diagnostics) != 0 {
		t.Fatalf("prior image-only advance paused policy: %#v", second.Diagnostics)
	}
	changed := append([]appconfig.AutoDeployDependencyIntent(nil), parents...)
	changed[0].BlobID = strings.Repeat("2", 40)
	changed[0].ContentSHA256 = "sha256:" + strings.Repeat("d", 64)
	conflict := appconfig.ApplyAutoDeployImageWithDependencies(first.Raw, intent, digest,
		"registry.example/api@sha256:"+strings.Repeat("e", 64), changed)
	if len(conflict.Diagnostics) != 1 || conflict.Diagnostics[0].Code != "AutoDeployTemplateConflict" {
		t.Fatalf("parent VariableSet change did not pause policy: %#v", conflict.Diagnostics)
	}
}
