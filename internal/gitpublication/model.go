package gitpublication

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalid          = errors.New("invalid Git publication")
	ErrNotFound         = errors.New("Git publication not found")
	ErrConflict         = errors.New("Git publication conflict")
	ErrProviderMismatch = errors.New("Git publication provider response mismatch")
	ErrMergeNotVisible  = errors.New("pull request merge is not visible on the authoritative target ref")
)

var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	commitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	ownerPattern  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38}[A-Za-z0-9])?$`)
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	refPattern    = regexp.MustCompile(`^refs/heads/[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

type State string

type Mode string

const (
	ModeDirect      Mode = "direct"
	ModePullRequest Mode = "pull-request"
)

const (
	StatePendingCandidate  State = "pending-candidate"
	StateWriteBaseReady    State = "write-base-ready"
	StateCandidateReady    State = "candidate-ready"
	StatePullRequestOpen   State = "pull-request-open"
	StatePullRequestClosed State = "pull-request-closed"
	StateMergePending      State = "merge-pending"
	StateMergeVerified     State = "merge-verified"
)

type PullRequestState string

const (
	PullRequestOpen   PullRequestState = "open"
	PullRequestClosed PullRequestState = "closed"
)

type Repository struct {
	InstallationID int64
	ID             int64
	Owner          string
	Name           string
}

func (r Repository) Validate() error {
	if r.InstallationID <= 0 || r.ID <= 0 || !ownerPattern.MatchString(r.Owner) || !namePattern.MatchString(r.Name) || r.Name == "." || r.Name == ".." ||
		strings.EqualFold(r.Name, ".git") || strings.HasSuffix(strings.ToLower(r.Name), ".git") {
		return ErrInvalid
	}
	return nil
}

// Publication is durable workflow state for one protected Git command. It is
// not desired state until StateMergeVerified, and even then the ordinary
// authoritative-target indexer remains responsible for advancing projections.
type Publication struct {
	OperationID        string
	BindingID          string
	Repository         Repository
	TargetRef          string
	BaseRevision       string
	WriteBaseRevision  string
	CandidateRef       string
	CandidateRevision  string
	PullRequestNumber  int64
	PullRequestURL     string
	PullRequestState   PullRequestState
	MergeRevision      string
	TargetRevision     string
	State              State
	ProviderObservedAt *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	Version            int64
}

func CandidateRef(operationID string) (string, error) {
	if !uuidPattern.MatchString(operationID) {
		return "", ErrInvalid
	}
	return "refs/heads/kuberploy/operations/" + operationID, nil
}

func NewPublication(operationID, bindingID string, repository Repository, targetRef, baseRevision string, now time.Time) (Publication, error) {
	candidateRef, err := CandidateRef(operationID)
	if err != nil {
		return Publication{}, err
	}
	value := Publication{
		OperationID: operationID, BindingID: bindingID, Repository: repository, TargetRef: targetRef,
		BaseRevision: baseRevision, CandidateRef: candidateRef, State: StatePendingCandidate,
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(), Version: 1,
	}
	return value, value.Validate()
}

func (p Publication) Validate() error {
	derived, err := CandidateRef(p.OperationID)
	if err != nil || !uuidPattern.MatchString(p.BindingID) || p.Repository.Validate() != nil || !validRef(p.TargetRef) ||
		p.CandidateRef != derived || p.CandidateRef == p.TargetRef || !commitPattern.MatchString(p.BaseRevision) ||
		p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() || p.UpdatedAt.Before(p.CreatedAt) || p.Version <= 0 {
		return ErrInvalid
	}
	observed := p.ProviderObservedAt != nil && !p.ProviderObservedAt.IsZero() && !p.ProviderObservedAt.Before(p.CreatedAt) && !p.ProviderObservedAt.After(p.UpdatedAt)
	prIdentity := p.PullRequestNumber > 0 && validPullRequestURL(p.Repository, p.PullRequestNumber, p.PullRequestURL)
	switch p.State {
	case StatePendingCandidate:
		if p.WriteBaseRevision != "" || p.CandidateRevision != "" || p.PullRequestNumber != 0 || p.PullRequestURL != "" || p.PullRequestState != "" ||
			p.MergeRevision != "" || p.TargetRevision != "" || p.ProviderObservedAt != nil {
			return ErrInvalid
		}
	case StateWriteBaseReady:
		if !commitPattern.MatchString(p.WriteBaseRevision) || p.CandidateRevision != "" || p.PullRequestNumber != 0 || p.PullRequestURL != "" ||
			p.PullRequestState != "" || p.MergeRevision != "" || p.TargetRevision != "" || p.ProviderObservedAt != nil {
			return ErrInvalid
		}
	case StateCandidateReady:
		if !commitPattern.MatchString(p.WriteBaseRevision) || !commitPattern.MatchString(p.CandidateRevision) || p.PullRequestNumber != 0 || p.PullRequestURL != "" || p.PullRequestState != "" ||
			p.MergeRevision != "" || p.TargetRevision != "" || p.ProviderObservedAt != nil {
			return ErrInvalid
		}
	case StatePullRequestOpen:
		if !commitPattern.MatchString(p.WriteBaseRevision) || !commitPattern.MatchString(p.CandidateRevision) || !prIdentity || p.PullRequestState != PullRequestOpen ||
			p.MergeRevision != "" || p.TargetRevision != "" || !observed {
			return ErrInvalid
		}
	case StatePullRequestClosed:
		if !commitPattern.MatchString(p.WriteBaseRevision) || !commitPattern.MatchString(p.CandidateRevision) || !prIdentity || p.PullRequestState != PullRequestClosed ||
			p.MergeRevision != "" || p.TargetRevision != "" || !observed {
			return ErrInvalid
		}
	case StateMergePending:
		if !commitPattern.MatchString(p.WriteBaseRevision) || !commitPattern.MatchString(p.CandidateRevision) || !prIdentity || p.PullRequestState != PullRequestClosed ||
			!commitPattern.MatchString(p.MergeRevision) || p.TargetRevision != "" || !observed {
			return ErrInvalid
		}
	case StateMergeVerified:
		if !commitPattern.MatchString(p.WriteBaseRevision) || !commitPattern.MatchString(p.CandidateRevision) || !prIdentity || p.PullRequestState != PullRequestClosed ||
			!commitPattern.MatchString(p.MergeRevision) || !commitPattern.MatchString(p.TargetRevision) || !observed {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func (p Publication) DesiredRevision() (string, bool) {
	if p.State != StateMergeVerified || commitPattern.MatchString(p.TargetRevision) == false {
		return "", false
	}
	return p.TargetRevision, true
}

func (p Publication) WithWriteBase(revision string, now time.Time) (Publication, error) {
	if !commitPattern.MatchString(revision) || now.IsZero() || now.Before(p.UpdatedAt) {
		return Publication{}, ErrInvalid
	}
	if p.State != StatePendingCandidate {
		if p.WriteBaseRevision == revision {
			return p, nil
		}
		return Publication{}, ErrConflict
	}
	next := p
	next.WriteBaseRevision, next.State, next.UpdatedAt, next.Version = revision, StateWriteBaseReady, now.UTC(), p.Version+1
	return next, next.Validate()
}

func (p Publication) WithCandidate(revision string, now time.Time) (Publication, error) {
	if !commitPattern.MatchString(revision) || now.IsZero() || now.Before(p.UpdatedAt) {
		return Publication{}, ErrInvalid
	}
	if p.State != StateWriteBaseReady {
		if p.CandidateRevision == revision {
			return p, nil
		}
		return Publication{}, ErrConflict
	}
	next := p
	next.CandidateRevision, next.State, next.UpdatedAt, next.Version = revision, StateCandidateReady, now.UTC(), p.Version+1
	return next, next.Validate()
}

func (p Publication) WithPullRequest(observation PullRequestObservation, now time.Time) (Publication, error) {
	if p.State == StatePendingCandidate || p.State == StateMergeVerified || now.IsZero() || now.Before(p.UpdatedAt) || observation.ValidateFor(p) != nil {
		return Publication{}, ErrProviderMismatch
	}
	if p.ProviderObservedAt != nil && observation.ObservedAt.Before(*p.ProviderObservedAt) {
		return Publication{}, ErrProviderMismatch
	}
	if p.MergeRevision != "" && (!observation.Merged || observation.MergeRevision != p.MergeRevision) {
		return Publication{}, ErrProviderMismatch
	}
	next := p
	next.PullRequestNumber, next.PullRequestURL = observation.Number, observation.URL
	next.PullRequestState, next.ProviderObservedAt = observation.State, timePointer(observation.ObservedAt)
	next.MergeRevision, next.TargetRevision = "", ""
	switch {
	case observation.State == PullRequestOpen && !observation.Merged:
		next.State = StatePullRequestOpen
	case observation.State == PullRequestClosed && !observation.Merged:
		next.State = StatePullRequestClosed
	case observation.State == PullRequestClosed && observation.Merged:
		next.State, next.MergeRevision = StateMergePending, observation.MergeRevision
	default:
		return Publication{}, ErrProviderMismatch
	}
	next.UpdatedAt, next.Version = now.UTC(), p.Version+1
	return next, next.Validate()
}

func (p Publication) WithVerifiedMerge(targetRevision string, now time.Time) (Publication, error) {
	if p.State != StateMergePending || !commitPattern.MatchString(targetRevision) || now.IsZero() || now.Before(p.UpdatedAt) {
		return Publication{}, ErrInvalid
	}
	next := p
	next.State, next.TargetRevision, next.UpdatedAt, next.Version = StateMergeVerified, targetRevision, now.UTC(), p.Version+1
	return next, next.Validate()
}

func ValidateTransition(previous, next Publication) error {
	if previous.Validate() != nil || next.Validate() != nil || previous.OperationID != next.OperationID ||
		previous.BindingID != next.BindingID || previous.Repository != next.Repository || previous.TargetRef != next.TargetRef ||
		previous.BaseRevision != next.BaseRevision || previous.CandidateRef != next.CandidateRef || previous.CreatedAt != next.CreatedAt ||
		next.Version != previous.Version+1 ||
		(previous.WriteBaseRevision != "" && next.WriteBaseRevision != previous.WriteBaseRevision) ||
		(previous.CandidateRevision != "" && next.CandidateRevision != previous.CandidateRevision) ||
		(previous.PullRequestNumber > 0 && (next.PullRequestNumber != previous.PullRequestNumber || next.PullRequestURL != previous.PullRequestURL)) ||
		(previous.MergeRevision != "" && next.MergeRevision != previous.MergeRevision) ||
		(previous.TargetRevision != "" && next.TargetRevision != previous.TargetRevision) {
		return ErrInvalid
	}
	validState := previous.State == StatePendingCandidate && next.State == StateWriteBaseReady ||
		previous.State == StateWriteBaseReady && next.State == StateCandidateReady ||
		previous.State == StateCandidateReady && (next.State == StatePullRequestOpen || next.State == StatePullRequestClosed || next.State == StateMergePending) ||
		previous.State == StatePullRequestOpen && (next.State == StatePullRequestOpen || next.State == StatePullRequestClosed || next.State == StateMergePending) ||
		previous.State == StatePullRequestClosed && (next.State == StatePullRequestOpen || next.State == StatePullRequestClosed || next.State == StateMergePending) ||
		previous.State == StateMergePending && (next.State == StateMergePending || next.State == StateMergeVerified) ||
		previous.State == StateMergeVerified && next.State == StateMergeVerified
	if !validState {
		return ErrInvalid
	}
	return nil
}

func validRef(value string) bool {
	return refPattern.MatchString(value) && !strings.Contains(value, "..") && !strings.Contains(value, "//") && !strings.HasSuffix(value, "/")
}

func validPullRequestURL(repository Repository, number int64, value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	want := "/" + repository.Owner + "/" + repository.Name + "/pull/" + strconv.FormatInt(number, 10)
	return parsed.EscapedPath() == want
}

func timePointer(value time.Time) *time.Time {
	copy := value.UTC()
	return &copy
}

func PullRequestTitle(operationID string) (string, error) {
	if !uuidPattern.MatchString(operationID) {
		return "", ErrInvalid
	}
	return "kuberploy: publish operation " + operationID, nil
}

func PullRequestBody(publication Publication) (string, error) {
	if publication.Validate() != nil || !commitPattern.MatchString(publication.CandidateRevision) {
		return "", ErrInvalid
	}
	return fmt.Sprintf("Kuberploy-Operation: %s\nKuberploy-Binding: %s\nKuberploy-Candidate: %s", publication.OperationID, publication.BindingID, publication.CandidateRevision), nil
}
