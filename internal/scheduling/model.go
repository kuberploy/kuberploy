// Package scheduling owns platform-admin scheduling profiles. Its resolver
// returns Pod-spec scheduling material only; it has no Kubernetes mutation API.
package scheduling

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
)

const (
	Contract                             = "scheduling-profile.v1"
	KarpenterDiagnosticsEnabledByDefault = false
)

var (
	ErrInvalid    = errors.New("invalid scheduling profile value")
	ErrNotFound   = errors.New("scheduling profile not found")
	ErrConflict   = errors.New("scheduling profile conflict")
	ErrInactive   = errors.New("scheduling profile is inactive")
	ErrUnassigned = errors.New("scheduling profile is not assigned to target")
	uuidRE        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	nameRE        = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	keyRE         = regexp.MustCompile(`^(?:[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?/)?[A-Za-z0-9](?:[-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	valueRE       = regexp.MustCompile(`^[A-Za-z0-9](?:[-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	idemRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,255}$`)
)

type Lifecycle string

const (
	Active      Lifecycle = "active"
	Deactivated Lifecycle = "deactivated"
)

type ScopeType string

const (
	TeamScope        ScopeType = "team"
	ProjectScope     ScopeType = "project"
	EnvironmentScope ScopeType = "environment"
)

type Assignment struct {
	Scope ScopeType `json:"scope"`
	ID    string    `json:"id"`
}

func (a Assignment) Validate() error {
	if !uuidRE.MatchString(a.ID) || (a.Scope != TeamScope && a.Scope != ProjectScope && a.Scope != EnvironmentScope) {
		return ErrInvalid
	}
	return nil
}

// Requirement and Toleration are Pod-spec fragments, never Node mutations.
type Requirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values,omitempty"`
}
type Toleration struct {
	Key               string `json:"key"`
	Operator          string `json:"operator"`
	Value             string `json:"value,omitempty"`
	Effect            string `json:"effect"`
	TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`
}
type TopologySpread struct {
	MaxSkew           int    `json:"maxSkew"`
	TopologyKey       string `json:"topologyKey"`
	WhenUnsatisfiable string `json:"whenUnsatisfiable"`
}

// PreferredNodeAffinity is a weighted, administrator-authored node preference.
// Requirements within a term are ANDed. Workload callers can only select the
// immutable profile revision; they never submit this material directly.
type PreferredNodeAffinity struct {
	Weight       int           `json:"weight"`
	Requirements []Requirement `json:"requirements"`
}

// SameApplicationPodAntiAffinity is a closed anti-affinity preset. Materialize
// derives the selector from the application UUID, so a profile cannot select a
// different workload or accept a caller-authored label selector. The projected
// domain term has no namespaces, namespaceSelector, matchLabelKeys, or
// mismatchLabelKeys surface, so Kubernetes applies it only in the Pod's own
// namespace.
type SameApplicationPodAntiAffinity struct {
	Enforcement string `json:"enforcement"`
	TopologyKey string `json:"topologyKey"`
	Weight      *int   `json:"weight,omitempty"`
}

type PodScheduling struct {
	NodeSelector                   map[string]string                `json:"nodeSelector,omitempty"`
	RequiredNodeAffinity           []Requirement                    `json:"requiredNodeAffinity,omitempty"`
	PreferredNodeAffinity          []PreferredNodeAffinity          `json:"preferredNodeAffinity,omitempty"`
	SameApplicationPodAntiAffinity []SameApplicationPodAntiAffinity `json:"sameApplicationPodAntiAffinity,omitempty"`
	Tolerations                    []Toleration                     `json:"tolerations,omitempty"`
	TopologySpread                 []TopologySpread                 `json:"topologySpread,omitempty"`
	PriorityClassName              string                           `json:"priorityClassName,omitempty"`
}

func (s PodScheduling) Validate() error {
	if len(s.NodeSelector) > 32 || len(s.RequiredNodeAffinity) > 32 || len(s.PreferredNodeAffinity) > 16 || len(s.SameApplicationPodAntiAffinity) > 16 || len(s.Tolerations) > 32 || len(s.TopologySpread) > 16 {
		return ErrInvalid
	}
	for k, v := range s.NodeSelector {
		if !keyRE.MatchString(k) || !valueRE.MatchString(v) {
			return ErrInvalid
		}
	}
	for _, r := range s.RequiredNodeAffinity {
		if validateRequirement(r) != nil {
			return ErrInvalid
		}
	}
	for _, term := range s.PreferredNodeAffinity {
		if term.Weight < 1 || term.Weight > 100 || len(term.Requirements) < 1 || len(term.Requirements) > 32 {
			return ErrInvalid
		}
		for _, r := range term.Requirements {
			if validateRequirement(r) != nil {
				return ErrInvalid
			}
		}
	}
	for _, preset := range s.SameApplicationPodAntiAffinity {
		if !keyRE.MatchString(preset.TopologyKey) {
			return ErrInvalid
		}
		switch preset.Enforcement {
		case "required":
			if preset.Weight != nil {
				return ErrInvalid
			}
		case "preferred":
			if preset.Weight != nil && (*preset.Weight < 1 || *preset.Weight > 100) {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
	}
	for _, t := range s.Tolerations {
		if t.Key == "" || !keyRE.MatchString(t.Key) || (t.Operator != "Exists" && t.Operator != "Equal") || (t.Effect != "NoSchedule" && t.Effect != "PreferNoSchedule" && t.Effect != "NoExecute") || t.Value != "" && !valueRE.MatchString(t.Value) || t.Operator == "Equal" && t.Value == "" || t.Operator == "Exists" && t.Value != "" || t.TolerationSeconds != nil && (*t.TolerationSeconds < 0 || t.Effect != "NoExecute") {
			return ErrInvalid
		}
	}
	for _, x := range s.TopologySpread {
		if x.MaxSkew < 1 || x.MaxSkew > 100 || !keyRE.MatchString(x.TopologyKey) || (x.WhenUnsatisfiable != "DoNotSchedule" && x.WhenUnsatisfiable != "ScheduleAnyway") {
			return ErrInvalid
		}
	}
	if s.PriorityClassName != "" && !nameRE.MatchString(s.PriorityClassName) {
		return ErrInvalid
	}
	return nil
}

func validateRequirement(r Requirement) error {
	if !keyRE.MatchString(r.Key) || (r.Operator != "In" && r.Operator != "NotIn" && r.Operator != "Exists" && r.Operator != "DoesNotExist" && r.Operator != "Gt" && r.Operator != "Lt") || len(r.Values) > 32 {
		return ErrInvalid
	}
	for _, v := range r.Values {
		if !valueRE.MatchString(v) {
			return ErrInvalid
		}
	}
	needsValues := r.Operator == "In" || r.Operator == "NotIn" || r.Operator == "Gt" || r.Operator == "Lt"
	if needsValues && len(r.Values) == 0 || !needsValues && len(r.Values) != 0 || (r.Operator == "Gt" || r.Operator == "Lt") && len(r.Values) != 1 {
		return ErrInvalid
	}
	if (r.Operator == "Gt" || r.Operator == "Lt") && len(r.Values) == 1 {
		if _, err := strconv.ParseInt(r.Values[0], 10, 64); err != nil {
			return ErrInvalid
		}
	}
	return nil
}

type Spec struct {
	Description string        `json:"description,omitempty"`
	Pod         PodScheduling `json:"pod"`
}

func (s Spec) Validate() error {
	if len(s.Description) > 512 {
		return ErrInvalid
	}
	return s.Pod.Validate()
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
}
type Entry struct {
	Profile  Profile  `json:"profile"`
	Revision Revision `json:"revision"`
}
type Ref struct {
	ProfileID string `json:"profileId"`
	Revision  int64  `json:"revision"`
}
type Target struct{ TeamID, ProjectID, EnvironmentID string }
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

func canonical(spec Spec, assignments []Assignment) (Spec, []Assignment, string, string, error) {
	if spec.Validate() != nil || len(assignments) < 1 || len(assignments) > 256 {
		return Spec{}, nil, "", "", ErrInvalid
	}
	spec = canonicalizeNewSchedulingFields(spec)
	if hasDuplicateNewSchedulingTerms(spec.Pod) {
		return Spec{}, nil, "", "", ErrInvalid
	}
	a := append([]Assignment(nil), assignments...)
	for _, x := range a {
		if x.Validate() != nil {
			return Spec{}, nil, "", "", ErrInvalid
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
			return Spec{}, nil, "", "", ErrInvalid
		}
	}
	b, _ := json.Marshal(struct {
		Contract string `json:"contract"`
		Spec     Spec   `json:"spec"`
	}{Contract, spec})
	c, _ := json.Marshal(a)
	return spec, a, digest(b), digest(c), nil
}

// canonicalizeNewSchedulingFields orders only the newly introduced,
// semantically unordered profile fields. Older profile fields retain their
// original digest behavior so pre-existing immutable revisions stay readable.
func canonicalizeNewSchedulingFields(spec Spec) Spec {
	spec.Pod = clonePod(spec.Pod)
	for i := range spec.Pod.PreferredNodeAffinity {
		term := &spec.Pod.PreferredNodeAffinity[i]
		for j := range term.Requirements {
			sort.Strings(term.Requirements[j].Values)
		}
		sort.Slice(term.Requirements, func(a, b int) bool {
			return requirementSortKey(term.Requirements[a]) < requirementSortKey(term.Requirements[b])
		})
	}
	sort.Slice(spec.Pod.PreferredNodeAffinity, func(i, j int) bool {
		left, _ := json.Marshal(spec.Pod.PreferredNodeAffinity[i].Requirements)
		right, _ := json.Marshal(spec.Pod.PreferredNodeAffinity[j].Requirements)
		if string(left) == string(right) {
			return spec.Pod.PreferredNodeAffinity[i].Weight < spec.Pod.PreferredNodeAffinity[j].Weight
		}
		return string(left) < string(right)
	})
	for i := range spec.Pod.SameApplicationPodAntiAffinity {
		preset := &spec.Pod.SameApplicationPodAntiAffinity[i]
		if preset.Enforcement == "preferred" && preset.Weight == nil {
			weight := 100
			preset.Weight = &weight
		}
	}
	sort.Slice(spec.Pod.SameApplicationPodAntiAffinity, func(i, j int) bool {
		left, right := spec.Pod.SameApplicationPodAntiAffinity[i], spec.Pod.SameApplicationPodAntiAffinity[j]
		if left.Enforcement != right.Enforcement {
			return left.Enforcement < right.Enforcement
		}
		if left.TopologyKey != right.TopologyKey {
			return left.TopologyKey < right.TopologyKey
		}
		return antiAffinityWeight(left) < antiAffinityWeight(right)
	})
	return spec
}

func requirementSortKey(r Requirement) string {
	b, _ := json.Marshal(r)
	return string(b)
}

func antiAffinityWeight(p SameApplicationPodAntiAffinity) int {
	if p.Weight == nil {
		return 0
	}
	return *p.Weight
}

func hasDuplicateNewSchedulingTerms(pod PodScheduling) bool {
	preferred := make(map[string]struct{}, len(pod.PreferredNodeAffinity))
	for _, term := range pod.PreferredNodeAffinity {
		b, _ := json.Marshal(term)
		key := string(b)
		if _, exists := preferred[key]; exists {
			return true
		}
		preferred[key] = struct{}{}
	}
	antiAffinity := make(map[string]struct{}, len(pod.SameApplicationPodAntiAffinity))
	for _, preset := range pod.SameApplicationPodAntiAffinity {
		b, _ := json.Marshal(preset)
		key := string(b)
		if _, exists := antiAffinity[key]; exists {
			return true
		}
		antiAffinity[key] = struct{}{}
	}
	return false
}
func digest(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }
func requestDigest(action, profileID, name string, revision int64, spec Spec, assignments []Assignment) (string, error) {
	_, a, sd, ad, e := canonical(spec, assignments)
	if e != nil {
		return "", e
	}
	b, _ := json.Marshal([]any{Contract, action, profileID, name, revision, sd, ad, a})
	return digest(b), nil
}
func validateCommand(c Command) error {
	if !uuidRE.MatchString(c.ActorID) || !idemRE.MatchString(c.IdempotencyKey) || !idemRE.MatchString(c.RequestID) || c.Now.IsZero() {
		return ErrInvalid
	}
	return nil
}
func match(a Assignment, t Target) bool {
	switch a.Scope {
	case TeamScope:
		return a.ID == t.TeamID
	case ProjectScope:
		return a.ID == t.ProjectID
	case EnvironmentScope:
		return a.ID == t.EnvironmentID
	}
	return false
}
func validateTarget(t Target) error {
	if t.TeamID != "" && !uuidRE.MatchString(t.TeamID) || !uuidRE.MatchString(t.ProjectID) || !uuidRE.MatchString(t.EnvironmentID) {
		return ErrInvalid
	}
	return nil
}
func clonePod(p PodScheduling) PodScheduling {
	b, _ := json.Marshal(p)
	var out PodScheduling
	_ = json.Unmarshal(b, &out)
	return out
}
func safeName(s string) bool { return nameRE.MatchString(s) && !strings.Contains(s, "--") }

// Resolution is the exact scheduling authority and effective Pod fragment.
// Callers persist all four identity coordinates next to the materialized
// fields so later execution boundaries can detect stale or substituted Git.
type Resolution struct {
	Ref               Ref
	SpecDigest        string
	AssignmentsDigest string
	Pod               PodScheduling
}

func (r Resolution) DomainRef() domain.SchedulingProfileRef {
	return domain.SchedulingProfileRef{ProfileID: r.Ref.ProfileID, Revision: r.Ref.Revision, SpecDigest: r.SpecDigest, AssignmentsDigest: r.AssignmentsDigest}
}
