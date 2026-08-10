package domain

import "time"

// ExternalDNSIntegration contains configuration metadata only. Credential
// values, provider API endpoints, arbitrary provider payloads and controller
// observations are deliberately outside this model.
type ExternalDNSIntegration struct {
	ID                        string     `json:"id"`
	Slug                      string     `json:"slug"`
	Name                      string     `json:"name"`
	Mode                      string     `json:"mode"`
	ProviderKind              string     `json:"providerKind"`
	TXTOwnerID                string     `json:"txtOwnerId"`
	AllowedDomainSuffixes     []string   `json:"allowedDomainSuffixes"`
	SyncPolicy                string     `json:"syncPolicy"`
	DestructiveSyncConfirmed  bool       `json:"destructiveSyncConfirmed"`
	CredentialSecretRef       string     `json:"credentialSecretRef,omitempty"`
	ProviderConfigRef         string     `json:"providerConfigRef,omitempty"`
	EgressConfigRef           string     `json:"egressConfigRef,omitempty"`
	OperatorProfileRef        string     `json:"operatorProfileRef,omitempty"`
	EnvironmentIDs            []string   `json:"environmentIds"`
	RuntimeRevision           int64      `json:"runtimeRevision"`
	Lifecycle                 string     `json:"lifecycle"`
	DeactivatedBy             string     `json:"deactivatedBy,omitempty"`
	DeactivatedAt             *time.Time `json:"deactivatedAt,omitempty"`
	ProtectedGitState         string     `json:"protectedGitState"`
	ProtectedGitRevision      int64      `json:"protectedGitRevision,omitempty"`
	ProtectedGitContentDigest string     `json:"protectedGitContentDigest,omitempty"`
	ProtectedGitCommit        string     `json:"protectedGitCommit,omitempty"`
	ProtectedGitObservedAt    *time.Time `json:"protectedGitObservedAt,omitempty"`
	CreatedBy                 string     `json:"createdBy"`
	CreatedAt                 time.Time  `json:"createdAt"`
	UpdatedAt                 time.Time  `json:"updatedAt"`
}

type ExternalDNSCatalogItem struct {
	ID                    string   `json:"id"`
	Slug                  string   `json:"slug"`
	Name                  string   `json:"name"`
	Mode                  string   `json:"mode"`
	ProviderKind          string   `json:"providerKind"`
	AllowedDomainSuffixes []string `json:"allowedDomainSuffixes"`
	RuntimeRevision       int64    `json:"runtimeRevision"`
}
