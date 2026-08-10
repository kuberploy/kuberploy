package githubapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func validPushPayload(t *testing.T) []byte {
	t.Helper()
	payload := map[string]any{
		"ref": "refs/heads/main", "after": strings.Repeat("a", 40), "created": false, "deleted": false, "forced": false,
		"repository": map[string]any{
			"id": 101, "name": "service", "full_name": "kuberploy/service",
			"owner": map[string]any{"id": 55, "login": "kuberploy", "type": "Organization"},
		},
		"installation": map[string]any{"id": 77},
		"sender":       map[string]any{"id": 88, "login": "github-actions[bot]", "type": "Bot"},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestParsePushPreservesWebhookSHAAsUntrustedOnly(t *testing.T) {
	body := validPushPayload(t)
	event, supported, err := ParseEvent(WebhookEnvelope{Event: "push", Body: body})
	if err != nil || !supported {
		t.Fatalf("supported=%t err=%v", supported, err)
	}
	push, ok := event.(PushEvent)
	if !ok || push.UntrustedAfter != strings.Repeat("a", 40) || push.Ref != "refs/heads/main" || push.Repository.ID != 101 || push.InstallationID != 77 || push.Sender.Type != "Bot" {
		t.Fatalf("push=%#v", event)
	}
}

func TestKnownWebhookEventsRejectDuplicateAndAmbiguousPayloads(t *testing.T) {
	duplicate := []byte(fmt.Sprintf(`{"ref":"refs/heads/main","ref":"refs/heads/other","after":%q,"created":false,"deleted":false,"forced":false,"repository":{"id":101,"name":"service","full_name":"kuberploy/service","owner":{"id":55,"login":"kuberploy","type":"Organization"}},"installation":{"id":77},"sender":{"id":88,"login":"octocat","type":"User"}}`, strings.Repeat("a", 40)))
	if _, supported, err := ParseEvent(WebhookEnvelope{Event: "push", Body: duplicate}); supported || !errors.Is(err, ErrInvalidWebhook) {
		t.Fatalf("duplicate JSON accepted: supported=%t err=%v", supported, err)
	}
	installationRepositories := map[string]any{
		"action": "added",
		"installation": map[string]any{
			"id": 77, "account": map[string]any{"id": 55, "login": "kuberploy", "type": "Organization"},
			"repository_selection": "selected", "permissions": map[string]any{"metadata": "read", "contents": "read"}, "suspended_at": nil,
		},
		"repository_selection": "selected",
		"repositories_added": []any{
			map[string]any{"id": 101, "name": "one", "full_name": "kuberploy/one", "owner": map[string]any{"id": 55, "login": "kuberploy", "type": "Organization"}},
			map[string]any{"id": 101, "name": "two", "full_name": "kuberploy/two", "owner": map[string]any{"id": 55, "login": "kuberploy", "type": "Organization"}},
		},
		"repositories_removed": []any{},
		"sender":               map[string]any{"id": 88, "login": "octocat", "type": "User"},
	}
	body, _ := json.Marshal(installationRepositories)
	if _, supported, err := ParseEvent(WebhookEnvelope{Event: "installation_repositories", Body: body}); supported || !errors.Is(err, ErrInvalidWebhook) {
		t.Fatalf("ambiguous repositories accepted: supported=%t err=%v", supported, err)
	}
}

func TestInstallationActionsAreTypedAndFutureActionsIgnored(t *testing.T) {
	payload := map[string]any{
		"action": "created",
		"installation": map[string]any{
			"id": 77, "account": map[string]any{"id": 55, "login": "kuberploy", "type": "Organization"},
			"repository_selection": "selected", "permissions": map[string]any{"metadata": "read", "contents": "read"}, "suspended_at": nil,
		},
		"sender": map[string]any{"id": 88, "login": "octocat", "type": "User"},
	}
	body, _ := json.Marshal(payload)
	event, supported, err := ParseEvent(WebhookEnvelope{Event: "installation", Body: body})
	if err != nil || !supported {
		t.Fatalf("created installation: event=%#v supported=%t err=%v", event, supported, err)
	}
	installation, ok := event.(InstallationEvent)
	if !ok || installation.InstallationID != 77 || installation.Permissions["contents"] != PermissionRead {
		t.Fatalf("installation=%#v", event)
	}
	payload["action"] = "future_action"
	body, _ = json.Marshal(payload)
	if event, supported, err = ParseEvent(WebhookEnvelope{Event: "installation", Body: body}); err != nil || supported || event != nil {
		t.Fatalf("future action was not safely ignored: event=%#v supported=%t err=%v", event, supported, err)
	}
}
