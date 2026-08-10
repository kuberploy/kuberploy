package middlewareprofiles

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

const Contract = "traefik-middleware-profile.v1"

var idempotencyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$`)

type Lifecycle string

const (
	Active      Lifecycle = "active"
	Deactivated Lifecycle = "deactivated"
)

type ScopeType string

const (
	ProjectScope     ScopeType = "project"
	EnvironmentScope ScopeType = "environment"
	ApplicationScope ScopeType = "application"
)

type Assignment struct {
	Scope ScopeType `json:"scope"`
	ID    string    `json:"id"`
}

func (a Assignment) Validate() error {
	if !uuidRE.MatchString(a.ID) || a.Scope != ProjectScope && a.Scope != EnvironmentScope && a.Scope != ApplicationScope {
		return ErrInvalid
	}
	return nil
}

type Profile struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Lifecycle       Lifecycle  `json:"lifecycle"`
	CurrentRevision int64      `json:"currentRevision"`
	CreatedBy       string     `json:"createdBy"`
	CreatedAt       time.Time  `json:"createdAt"`
	DeactivatedBy   string     `json:"deactivatedBy,omitempty"`
	DeactivatedAt   *time.Time `json:"deactivatedAt,omitempty"`
}
type Revision struct {
	ProfileID         string       `json:"profileId"`
	Revision          int64        `json:"revision"`
	Spec              Spec         `json:"spec"`
	SpecDigest        string       `json:"specDigest"`
	AssignmentsDigest string       `json:"assignmentsDigest"`
	CreatedBy         string       `json:"createdBy"`
	Assignments       []Assignment `json:"assignments"`
	CreatedAt         time.Time    `json:"createdAt"`
	ClonedFrom        *Ref         `json:"clonedFrom,omitempty"`
}
type Entry struct {
	Profile  Profile  `json:"profile"`
	Revision Revision `json:"revision"`
}
type Ref struct {
	ProfileID         string `json:"profileId"`
	Revision          int64  `json:"revision"`
	SpecDigest        string `json:"specDigest,omitempty"`
	AssignmentsDigest string `json:"assignmentsDigest,omitempty"`
}
type Target struct{ ProjectID, EnvironmentID, ApplicationID string }
type Command struct {
	ActorID, IdempotencyKey, RequestID string
	Now                                time.Time
}
type MutationResult struct {
	Profile  Profile
	Revision Revision
	Replay   bool
}
type AuditEvent struct {
	ActorID, Action, ProfileID, RequestID, IdempotencyKey, SpecDigest, AssignmentsDigest string
	Revision                                                                             int64
	At                                                                                   time.Time
}
type Reference struct {
	ProfileID     string `json:"profileId"`
	Revision      int64  `json:"revision"`
	ApplicationID string `json:"applicationId"`
	EnvironmentID string `json:"environmentId"`
	GitPath       string `json:"gitPath"`
	LogicalName   string `json:"logicalName"`
}

func canonical(spec Spec, assignments []Assignment) (Spec, []Assignment, string, string, error) {
	if ValidateSpec(spec) != nil || len(assignments) < 1 || len(assignments) > 256 {
		return nil, nil, "", "", ErrInvalid
	}
	a := append([]Assignment(nil), assignments...)
	for _, x := range a {
		if x.Validate() != nil {
			return nil, nil, "", "", ErrInvalid
		}
	}
	if len(SecretReferences(spec)) != 0 {
		for _, x := range a {
			if x.Scope != ApplicationScope {
				return nil, nil, "", "", ErrInvalid
			}
		}
	}
	sort.Slice(a, func(i, j int) bool {
		if a[i].Scope == a[j].Scope {
			return a[i].ID < a[j].ID
		}
		return a[i].Scope < a[j].Scope
	})
	for i := 1; i < len(a); i++ {
		if a[i] == a[i-1] {
			return nil, nil, "", "", ErrInvalid
		}
	}
	clean := cloneSpec(spec)
	specJSON, _ := json.Marshal(struct {
		Contract string `json:"contract"`
		Spec     Spec   `json:"spec"`
	}{Contract, clean})
	assignmentsJSON, _ := json.Marshal(a)
	return clean, a, digest(specJSON), digest(assignmentsJSON), nil
}
func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func validateCommand(c Command) error {
	if !uuidRE.MatchString(c.ActorID) || !idempotencyRE.MatchString(c.IdempotencyKey) || !idempotencyRE.MatchString(c.RequestID) || c.Now.IsZero() {
		return ErrInvalid
	}
	return nil
}
func commandDigest(action, profileID, name string, revision int64, spec Spec, assignments []Assignment, source *Ref) (string, error) {
	_, a, sd, ad, err := canonical(spec, assignments)
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal([]any{Contract, action, profileID, name, revision, sd, ad, a, source})
	return digest(raw), nil
}
func validateTarget(t Target) error {
	if !uuidRE.MatchString(t.ProjectID) || !uuidRE.MatchString(t.EnvironmentID) || !uuidRE.MatchString(t.ApplicationID) {
		return ErrInvalid
	}
	return nil
}
func assigned(a Assignment, t Target) bool {
	switch a.Scope {
	case ProjectScope:
		return a.ID == t.ProjectID
	case EnvironmentScope:
		return a.ID == t.EnvironmentID
	case ApplicationScope:
		return a.ID == t.ApplicationID
	}
	return false
}
func validateRef(ref Ref) error {
	if !uuidRE.MatchString(ref.ProfileID) || ref.Revision < 1 || ref.SpecDigest != "" && !digestRE.MatchString(ref.SpecDigest) || ref.AssignmentsDigest != "" && !digestRE.MatchString(ref.AssignmentsDigest) {
		return ErrInvalid
	}
	return nil
}
func DomainSecretReferences(spec Spec) []domain.SecretBindingRef { return SecretReferences(spec) }

type Resolution struct {
	Ref  Ref
	Name string
	Spec Spec
}

// MaterializedDefinition is the only reusable-profile form allowed in Git.
// The active profile is still re-resolved and compared during activation.
type MaterializedDefinition struct {
	Name       string `json:"name"`
	ProfileRef *Ref   `json:"profileRef,omitempty"`
	Spec       Spec   `json:"spec"`
}

func (r Resolution) Definition(logicalName string) map[string]any {
	return map[string]any{"name": logicalName, "profileRef": map[string]any{"profileId": r.Ref.ProfileID, "revision": r.Ref.Revision, "specDigest": r.Ref.SpecDigest, "assignmentsDigest": r.Ref.AssignmentsDigest}, "spec": cloneSpec(r.Spec)}
}
