package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// AgentOperation is the deliberately small, safety-oriented projection of an
// OpenAPI operation. Request and response schemas remain authoritative in the
// linked OpenAPI document; this projection is only an allowlist and policy
// index for agents.
type AgentOperation struct {
	OperationID     string   `json:"operationId"`
	Method          string   `json:"method"`
	Path            string   `json:"path"`
	Summary         string   `json:"summary"`
	Tags            []string `json:"tags"`
	Permission      string   `json:"permission"`
	Effect          string   `json:"effect"`
	Risk            string   `json:"risk"`
	Idempotency     string   `json:"idempotency"`
	Confirmation    string   `json:"confirmation"`
	AutomationScope string   `json:"automationScope"`
	Asynchronous    bool     `json:"asynchronous"`
}

type agentProfile struct {
	SchemaVersion  string                  `json:"schemaVersion"`
	Title          string                  `json:"title"`
	Contract       agentContract           `json:"contract"`
	Authentication agentAuthentication     `json:"authentication"`
	Safety         agentSafety             `json:"safety"`
	Workflows      []agentWorkflowDocument `json:"workflows"`
	Operations     []AgentOperation        `json:"operations"`
}

type agentContract struct {
	OpenAPI string `json:"openapi"`
	SHA256  string `json:"sha256"`
}

type agentAuthentication struct {
	Mode                     string `json:"mode"`
	ServiceAccountsAvailable bool   `json:"serviceAccountsAvailable"`
	CSRFHeader               string `json:"csrfHeader"`
	CSRFRequiredWith         string `json:"csrfRequiredWith"`
	CSRFNotRequiredWith      string `json:"csrfNotRequiredWith"`
	Detail                   string `json:"detail"`
}

type agentSafety struct {
	UnknownOperationPolicy string `json:"unknownOperationPolicy"`
	Accepted202Meaning     string `json:"accepted202Meaning"`
	SecretMaterialPolicy   string `json:"secretMaterialPolicy"`
	MutationPolicy         string `json:"mutationPolicy"`
}

type agentWorkflowDocument struct {
	Arazzo string `json:"arazzo"`
	URL    string `json:"url"`
}

type openAPIDocument struct {
	Paths map[string]map[string]json.RawMessage `json:"paths"`
}

type openAPIOperation struct {
	OperationID     string                     `json:"operationId"`
	Summary         string                     `json:"summary"`
	Tags            []string                   `json:"tags"`
	Audience        []string                   `json:"x-kuberploy-audience"`
	Permission      string                     `json:"x-kuberploy-permission"`
	Effect          string                     `json:"x-kuberploy-effect"`
	Risk            string                     `json:"x-kuberploy-risk"`
	Idempotency     string                     `json:"x-kuberploy-idempotency"`
	Confirmation    string                     `json:"x-kuberploy-confirmation"`
	AutomationScope string                     `json:"x-kuberploy-automation-scope"`
	Responses       map[string]json.RawMessage `json:"responses"`
}

var (
	agentProfileOnce  sync.Once
	agentProfileBytes []byte
	agentProfileErr   error
)

// AgentProfileJSON returns a derived allowlist. It intentionally cannot drift
// into advertising an operation that OpenAPI has not marked for agent use.
func AgentProfileJSON() ([]byte, error) {
	agentProfileOnce.Do(func() {
		agentProfileBytes, agentProfileErr = BuildAgentProfile(OpenAPIJSON)
	})
	return append([]byte(nil), agentProfileBytes...), agentProfileErr
}

// BuildAgentProfile builds and validates the agent projection from exact
// OpenAPI bytes. It is exported so contract tests and release validation can
// exercise fail-closed mutations.
func BuildAgentProfile(openAPI []byte) ([]byte, error) {
	var document openAPIDocument
	decoder := json.NewDecoder(strings.NewReader(string(openAPI)))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode OpenAPI: %w", err)
	}
	if len(document.Paths) == 0 {
		return nil, errors.New("OpenAPI paths are empty")
	}

	operations := make([]AgentOperation, 0)
	seen := map[string]struct{}{}
	for path, pathItem := range document.Paths {
		for method, raw := range pathItem {
			method = strings.ToLower(method)
			if !isHTTPMethod(method) {
				continue
			}
			var operation openAPIOperation
			if err := json.Unmarshal(raw, &operation); err != nil {
				return nil, fmt.Errorf("decode %s %s: %w", strings.ToUpper(method), path, err)
			}
			if !contains(operation.Audience, "agent") {
				continue
			}
			if operation.OperationID == "" {
				return nil, fmt.Errorf("agent operation %s %s has no operationId", strings.ToUpper(method), path)
			}
			if _, exists := seen[operation.OperationID]; exists {
				return nil, fmt.Errorf("duplicate agent operationId %q", operation.OperationID)
			}
			seen[operation.OperationID] = struct{}{}
			if operation.Permission == "" || operation.Effect == "" || operation.Risk == "" || operation.Idempotency == "" || operation.Confirmation == "" {
				return nil, fmt.Errorf("agent operation %q has incomplete safety metadata", operation.OperationID)
			}
			if !validAutomationScope(operation.AutomationScope) {
				return nil, fmt.Errorf("agent operation %q has missing or invalid automation scope %q", operation.OperationID, operation.AutomationScope)
			}
			summary := strings.TrimSpace(operation.Summary)
			if summary == "" {
				summary = operation.OperationID
			}
			operations = append(operations, AgentOperation{
				OperationID: operation.OperationID, Method: strings.ToUpper(method), Path: path,
				Summary: summary, Tags: append([]string(nil), operation.Tags...), Permission: operation.Permission,
				Effect: operation.Effect, Risk: operation.Risk, Idempotency: operation.Idempotency,
				Confirmation: operation.Confirmation, AutomationScope: operation.AutomationScope, Asynchronous: operation.Responses["202"] != nil,
			})
		}
	}
	if len(operations) == 0 {
		return nil, errors.New("OpenAPI declares no agent operations")
	}
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].Path != operations[j].Path {
			return operations[i].Path < operations[j].Path
		}
		if operations[i].Method != operations[j].Method {
			return operations[i].Method < operations[j].Method
		}
		return operations[i].OperationID < operations[j].OperationID
	})
	for i := range operations {
		sort.Strings(operations[i].Tags)
	}

	digest := sha256.Sum256(openAPI)
	profile := agentProfile{
		SchemaVersion: "1.0.0",
		Title:         "Kuberploy agent operation profile",
		Contract: agentContract{
			OpenAPI: "/openapi.json",
			SHA256:  "sha256:" + hex.EncodeToString(digest[:]),
		},
		Authentication: agentAuthentication{
			Mode:                     "cookie-session-or-bearer",
			ServiceAccountsAvailable: true,
			CSRFHeader:               "X-CSRF-Token",
			CSRFRequiredWith:         "cookieAuth on unsafe HTTP methods",
			CSRFNotRequiredWith:      "bearerAuth",
			Detail:                   "Choose exactly one OpenAPI authentication alternative. Interactive cookie sessions require the CSRF header on unsafe methods. Project-scoped service-account bearer tokens do not use CSRF and remain constrained by both their closed coarse scopes and current object-level grants.",
		},
		Safety: agentSafety{
			UnknownOperationPolicy: "deny",
			Accepted202Meaning:     "The durable operation was accepted; it does not prove Git publication, Argo CD reconciliation, or rollout health.",
			SecretMaterialPolicy:   "Never request, print, or persist secret plaintext; supported workload configuration uses versioned secret references only.",
			MutationPolicy:         "Honor permission, idempotency, confirmation, ETag, and preview-token requirements. Send CSRF only when using cookieAuth; bearerAuth mutations do not require it.",
		},
		Workflows:  []agentWorkflowDocument{{Arazzo: "1.1.0", URL: "/arazzo.yaml"}},
		Operations: operations,
	}
	result, err := json.Marshal(profile)
	if err != nil {
		return nil, fmt.Errorf("encode agent profile: %w", err)
	}
	return result, nil
}

func isHTTPMethod(method string) bool {
	switch method {
	case "get", "head", "post", "put", "patch", "delete", "options", "trace":
		return true
	default:
		return false
	}
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func validAutomationScope(scope string) bool {
	switch scope {
	case "none", "app.read", "app.edit", "build.create", "logs.read":
		return true
	default:
		return false
	}
}
