package domain

import "time"

// AutomationScope is the coarse bearer-token boundary. Access grants remain
// the object-level boundary, so both the token scope and the current grant must
// permit a request.
type AutomationScope string

const (
	AutomationScopeAppRead     AutomationScope = "app.read"
	AutomationScopeAppEdit     AutomationScope = "app.edit"
	AutomationScopeBuildCreate AutomationScope = "build.create"
	AutomationScopeLogsRead    AutomationScope = "logs.read"
)

type ServiceAccount struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"projectId"`
	Name       string     `json:"name"`
	Role       AccessRole `json:"role"`
	CreatedBy  string     `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	DisabledAt *time.Time `json:"disabledAt,omitempty"`
}

type CreateServiceAccount struct {
	ProjectID string
	Name      string
	Role      AccessRole
}

type ServiceAccountToken struct {
	ID               string            `json:"id"`
	ServiceAccountID string            `json:"serviceAccountId"`
	Name             string            `json:"name"`
	Prefix           string            `json:"prefix"`
	Scopes           []AutomationScope `json:"scopes"`
	ExpiresAt        time.Time         `json:"expiresAt"`
	LastUsedAt       *time.Time        `json:"lastUsedAt,omitempty"`
	RevokedAt        *time.Time        `json:"revokedAt,omitempty"`
	CreatedBy        string            `json:"createdBy"`
	CreatedAt        time.Time         `json:"createdAt"`
}

type CreateServiceAccountToken struct {
	ServiceAccountID string
	Name             string
	Prefix           string
	TokenHash        []byte
	Scopes           []AutomationScope
	ExpiresAt        time.Time
}

// AutomationPrincipal is returned only by the hashed-token authentication
// lookup. TokenHash and the raw bearer credential are never part of it.
type AutomationPrincipal struct {
	User             User
	ServiceAccountID string
	TokenID          string
	Scopes           []AutomationScope
	ExpiresAt        time.Time
}
