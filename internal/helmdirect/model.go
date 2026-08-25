// Package helmdirect stores Helm desired state and forwards it to Argo CD.
// Argo CD, rather than Kuberploy, resolves and renders chart sources.
package helmdirect

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const MaximumValuesBytes = 256 << 10

var (
	ErrInvalid     = errors.New("Helm App input is invalid")
	ErrNotFound    = errors.New("Helm App was not found")
	ErrConflict    = errors.New("Helm App conflicts with current state")
	ErrUnavailable = errors.New("Helm App reconciliation is unavailable")
	uuidRE         = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	dnsLabelRE     = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	revisionRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+*-]{0,199}$`)
	chartRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)
)

type SourceKind string

const (
	SourceHelmRepository SourceKind = "helm-repository"
	SourceOCI            SourceKind = "oci"
	SourceGit            SourceKind = "git"
)

type Source struct {
	Kind           SourceKind `json:"kind"`
	RepositoryURL  string     `json:"repositoryUrl"`
	Chart          string     `json:"chart,omitempty"`
	TargetRevision string     `json:"targetRevision"`
	Path           string     `json:"path,omitempty"`
}

func (s Source) Normalize() (Source, error) {
	s.Kind = SourceKind(strings.TrimSpace(string(s.Kind)))
	s.RepositoryURL = strings.TrimSpace(s.RepositoryURL)
	s.Chart = strings.TrimSpace(s.Chart)
	s.TargetRevision = strings.TrimSpace(s.TargetRevision)
	s.Path = strings.Trim(strings.TrimSpace(s.Path), "/")
	if s.Kind == SourceOCI {
		s.RepositoryURL = strings.TrimPrefix(s.RepositoryURL, "oci://")
	}
	if !revisionRE.MatchString(s.TargetRevision) {
		return Source{}, ErrInvalid
	}
	switch s.Kind {
	case SourceHelmRepository:
		if !validHTTPSRepository(s.RepositoryURL) || !chartRE.MatchString(s.Chart) || s.Path != "" {
			return Source{}, ErrInvalid
		}
	case SourceOCI:
		if !validOCIRepository(s.RepositoryURL) || !chartRE.MatchString(s.Chart) || s.Path != "" {
			return Source{}, ErrInvalid
		}
	case SourceGit:
		if !validGitRepository(s.RepositoryURL) || s.Chart != "" || !validRelativePath(s.Path) {
			return Source{}, ErrInvalid
		}
	default:
		return Source{}, ErrInvalid
	}
	return s, nil
}

func validHTTPSRepository(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && len(raw) <= 2048
}

func validOCIRepository(raw string) bool {
	if strings.ContainsAny(raw, "?#@\x00\r\n") || len(raw) > 2048 {
		return false
	}
	u, err := url.Parse("https://" + raw)
	return err == nil && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func validGitRepository(raw string) bool {
	if validHTTPSRepository(raw) {
		return true
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "ssh" && u.Host != "" && u.User != nil && u.RawQuery == "" && u.Fragment == "" && len(raw) <= 2048
}

func validRelativePath(value string) bool {
	if value == "" || len(value) > 512 || strings.ContainsAny(value, "\x00\r\n\\") || strings.HasPrefix(value, "/") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func NormalizeValues(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}\n")
	}
	if len(raw) > MaximumValuesBytes || bytes.IndexByte(raw, 0) >= 0 {
		return nil, ErrInvalid
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var value map[string]any
	if err := decoder.Decode(&value); err != nil || value == nil {
		return nil, ErrInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, ErrInvalid
	}
	normalized, err := yaml.Marshal(value)
	if err != nil || len(normalized) == 0 || len(normalized) > MaximumValuesBytes {
		return nil, ErrInvalid
	}
	return normalized, nil
}

type Target struct {
	ProjectID, EnvironmentID, ApplicationID string
}

func (t Target) Validate() error {
	if !uuidRE.MatchString(t.ProjectID) || !uuidRE.MatchString(t.EnvironmentID) || !uuidRE.MatchString(t.ApplicationID) {
		return ErrInvalid
	}
	return nil
}

type Action string

const (
	ActionDeploy   Action = "deploy"
	ActionRetry    Action = "retry"
	ActionDisable  Action = "disable"
	ActionRollback Action = "rollback"
)

type State string

const (
	StatePending State = "pending"
	StateApplied State = "applied"
	StateFailed  State = "failed"
)

type Revision struct {
	ID, ParentRevisionID, RollbackSourceRevisionID string
	Generation                                     int64
	Target                                         Target
	ReleaseName, DestinationNamespace, ArgoProject string
	Source                                         Source
	ValuesYAML                                     []byte
	ValuesDigest                                   string
	Action                                         Action
	DesiredEnabled                                 bool
	State                                          State
	FailureCode                                    string
	ActorID, IdempotencyKey, RequestID             string
	CreatedAt, UpdatedAt                           time.Time
}

func Digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r Revision) Validate() error {
	_, sourceErr := r.Source.Normalize()
	if !uuidRE.MatchString(r.ID) || r.Generation < 1 || r.Target.Validate() != nil ||
		!dnsLabelRE.MatchString(r.ReleaseName) || !dnsLabelRE.MatchString(r.DestinationNamespace) ||
		!dnsLabelRE.MatchString(r.ArgoProject) || sourceErr != nil || len(r.ValuesYAML) == 0 ||
		r.ValuesDigest != Digest(r.ValuesYAML) || !uuidRE.MatchString(r.ActorID) ||
		r.IdempotencyKey == "" || r.RequestID == "" || r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return ErrInvalid
	}
	if r.DesiredEnabled != (r.Action != ActionDisable) {
		return ErrInvalid
	}
	if (r.Generation == 1) != (r.ParentRevisionID == "") ||
		(r.ParentRevisionID != "" && !uuidRE.MatchString(r.ParentRevisionID)) {
		return ErrInvalid
	}
	switch r.Action {
	case ActionDeploy:
		if r.RollbackSourceRevisionID != "" {
			return ErrInvalid
		}
	case ActionRetry:
		if r.RollbackSourceRevisionID != "" {
			return ErrInvalid
		}
	case ActionDisable:
		if r.RollbackSourceRevisionID != "" {
			return ErrInvalid
		}
	case ActionRollback:
		if !uuidRE.MatchString(r.RollbackSourceRevisionID) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	switch r.State {
	case StatePending, StateApplied:
		if r.FailureCode != "" {
			return ErrInvalid
		}
	case StateFailed:
		if r.FailureCode == "" || len(r.FailureCode) > 63 || stringsContainControl(r.FailureCode) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
