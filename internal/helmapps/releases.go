package helmapps

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"time"
)

type ReleaseAction string

const (
	ReleaseInitial  ReleaseAction = "initial"
	ReleaseUpdate   ReleaseAction = "update"
	ReleaseRetry    ReleaseAction = "retry"
	ReleaseDisable  ReleaseAction = "disable"
	ReleaseRollback ReleaseAction = "rollback"
)

type ReleaseTarget struct {
	ProjectID, EnvironmentID, ApplicationID string
}

func (t ReleaseTarget) Validate() error {
	if !uuidRE.MatchString(t.ProjectID) || !uuidRE.MatchString(t.EnvironmentID) ||
		!uuidRE.MatchString(t.ApplicationID) {
		return ErrInvalid
	}
	return nil
}

type ReleaseActor struct {
	ID, IdempotencyKey, RequestID string
}

func (a ReleaseActor) Validate() error {
	if !uuidRE.MatchString(a.ID) || !idempotencyRE.MatchString(a.IdempotencyKey) ||
		len(a.RequestID) < 1 || len(a.RequestID) > 128 || containsControl(a.RequestID) {
		return ErrInvalid
	}
	return nil
}

type UpsertReleaseRequest struct {
	Target     ReleaseTarget
	Actor      ReleaseActor
	Approval   ApprovalKey
	ValuesYAML []byte
}

type RetryReleaseRequest struct {
	Target ReleaseTarget
	Actor  ReleaseActor
}

type DisableReleaseRequest struct {
	Target ReleaseTarget
	Actor  ReleaseActor
}

type RollbackReleaseRequest struct {
	Target           ReleaseTarget
	Actor            ReleaseActor
	SourceRevisionID string
}

type ReleaseRevision struct {
	ID                       string        `json:"id"`
	Generation               int64         `json:"generation"`
	Target                   ReleaseTarget `json:"target"`
	ReleaseName              string        `json:"releaseName"`
	Action                   ReleaseAction `json:"action"`
	DesiredEnabled           bool          `json:"desiredEnabled"`
	ParentRevisionID         string        `json:"parentRevisionId,omitempty"`
	RollbackSourceRevisionID string        `json:"rollbackSourceRevisionId,omitempty"`
	BaseApplicationIntentID  string        `json:"baseApplicationIntentId,omitempty"`
	Approval                 ApprovalKey   `json:"approval"`
	RenderCommandID          string        `json:"renderCommandId,omitempty"`
	ValuesYAML               []byte        `json:"-"`
	ValuesDigest             string        `json:"valuesDigest"`
	IntentDigest             string        `json:"intentDigest"`
	ActorID                  string        `json:"actorId"`
	IdempotencyKey           string        `json:"-"`
	RequestID                string        `json:"requestId"`
	CreatedAt                time.Time     `json:"createdAt"`
}

func (r ReleaseRevision) Validate() error {
	if !uuidRE.MatchString(r.ID) || r.Generation < 1 || r.Target.Validate() != nil ||
		!dnsLabelRE.MatchString(r.ReleaseName) || r.Approval.Validate() != nil ||
		len(r.ValuesYAML) < 1 || len(r.ValuesYAML) > MaximumValuesSize ||
		!validDigest(r.ValuesDigest) || digestBytes(r.ValuesYAML) != r.ValuesDigest ||
		!validDigest(r.IntentDigest) || !uuidRE.MatchString(r.ActorID) ||
		!idempotencyRE.MatchString(r.IdempotencyKey) || len(r.RequestID) < 1 ||
		len(r.RequestID) > 128 || containsControl(r.RequestID) || r.CreatedAt.IsZero() {
		return ErrInvalid
	}
	if r.DesiredEnabled != (r.Action != ReleaseDisable) ||
		(r.DesiredEnabled != uuidRE.MatchString(r.RenderCommandID)) {
		return ErrInvalid
	}
	switch r.Action {
	case ReleaseInitial:
		if r.Generation != 1 || r.ParentRevisionID != "" || r.RollbackSourceRevisionID != "" || r.BaseApplicationIntentID != "" {
			return ErrInvalid
		}
	case ReleaseUpdate, ReleaseRetry:
		if r.Generation == 1 || !uuidRE.MatchString(r.ParentRevisionID) || r.RollbackSourceRevisionID != "" ||
			(r.BaseApplicationIntentID != "" && !uuidRE.MatchString(r.BaseApplicationIntentID)) {
			return ErrInvalid
		}
	case ReleaseDisable:
		if r.Generation == 1 || !uuidRE.MatchString(r.ParentRevisionID) || r.RollbackSourceRevisionID != "" ||
			!uuidRE.MatchString(r.BaseApplicationIntentID) {
			return ErrInvalid
		}
	case ReleaseRollback:
		if r.Generation == 1 || !uuidRE.MatchString(r.ParentRevisionID) ||
			!uuidRE.MatchString(r.RollbackSourceRevisionID) ||
			(r.BaseApplicationIntentID != "" && !uuidRE.MatchString(r.BaseApplicationIntentID)) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type ApprovalDocument struct {
	Approval          Approval  `json:"approval"`
	ValuesSchemaJSON  []byte    `json:"valuesSchema"`
	DefaultValuesYAML []byte    `json:"defaultValuesYaml"`
	DocumentsDigest   string    `json:"documentsDigest"`
	CreatedAt         time.Time `json:"createdAt"`
}

type ValuesPreview struct {
	Approval             ApprovalKey     `json:"approval"`
	NormalizedValuesYAML string          `json:"normalizedValuesYaml"`
	ValuesDigest         string          `json:"valuesDigest"`
	CurrentValuesDigest  string          `json:"currentValuesDigest,omitempty"`
	EffectiveValues      json.RawMessage `json:"effectiveValues"`
	ChangedPaths         []string        `json:"changedPaths"`
}

func PreviewApprovalValues(document ApprovalDocument, current, proposed []byte) (ValuesPreview, error) {
	if document.Validate() != nil {
		return ValuesPreview{}, ErrInvalid
	}
	defaults, err := ParseValues(document.DefaultValuesYAML)
	if err != nil {
		return ValuesPreview{}, err
	}
	next, err := ParseValues(proposed)
	if err != nil || validateMergedValuesSchema(document.ValuesSchemaJSON, defaults, next) != nil {
		if err != nil {
			return ValuesPreview{}, err
		}
		return ValuesPreview{}, ErrUnsafeYAML
	}
	previousValues := map[string]any{}
	currentDigest := ""
	if len(current) != 0 {
		previous, parseErr := ParseValues(current)
		if parseErr != nil {
			return ValuesPreview{}, ErrConflict
		}
		previousValues, currentDigest = previous.Values, digestBytes(previous.Raw)
	}
	effective := cloneJSONMap(defaults.Values)
	mergeJSONMaps(effective, next.Values)
	effectiveJSON, err := json.Marshal(effective)
	if err != nil || len(effectiveJSON) > MaximumValuesSize {
		return ValuesPreview{}, ErrInvalid
	}
	changed := make([]string, 0)
	collectChangedValuePaths(previousValues, next.Values, "", &changed)
	if len(changed) > 512 {
		return ValuesPreview{}, ErrInvalid
	}
	return ValuesPreview{Approval: document.Approval.ApprovalKey,
		NormalizedValuesYAML: string(next.Raw), ValuesDigest: digestBytes(next.Raw),
		CurrentValuesDigest: currentDigest, EffectiveValues: effectiveJSON,
		ChangedPaths: changed}, nil
}

func collectChangedValuePaths(previous, next any, pointer string, changed *[]string) {
	if len(*changed) > 512 {
		return
	}
	left, leftMap := previous.(map[string]any)
	right, rightMap := next.(map[string]any)
	if leftMap && rightMap {
		keys := make([]string, 0, len(left)+len(right))
		seen := make(map[string]struct{}, len(left)+len(right))
		for key := range left {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
		for key := range right {
			if _, exists := seen[key]; !exists {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			leftValue, leftExists := left[key]
			rightValue, rightExists := right[key]
			childPointer := pointer + "/" + escapeJSONPointer(key)
			if leftExists != rightExists {
				*changed = append(*changed, childPointer)
				continue
			}
			collectChangedValuePaths(leftValue, rightValue, childPointer, changed)
		}
		return
	}
	if !reflect.DeepEqual(previous, next) {
		if pointer == "" {
			pointer = "/"
		}
		*changed = append(*changed, pointer)
	}
}

func escapeJSONPointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func (d ApprovalDocument) Validate() error {
	if d.Approval.Validate() != nil || len(d.ValuesSchemaJSON) < 2 ||
		len(d.ValuesSchemaJSON) > MaximumSchemaSize || len(d.DefaultValuesYAML) < 1 ||
		len(d.DefaultValuesYAML) > MaximumValuesSize || !validDigest(d.DocumentsDigest) ||
		d.CreatedAt.IsZero() || digestBytes(d.ValuesSchemaJSON) != d.Approval.ValuesSchemaDigest {
		return ErrInvalid
	}
	var schema any
	if json.Unmarshal(d.ValuesSchemaJSON, &schema) != nil || schema == nil {
		return ErrInvalid
	}
	if _, err := ParseValues(d.DefaultValuesYAML); err != nil {
		return ErrInvalid
	}
	expected, err := approvalDocumentsDigest(d.Approval.ApprovalKey,
		d.ValuesSchemaJSON, d.DefaultValuesYAML)
	if err != nil || expected != d.DocumentsDigest {
		return ErrInvalid
	}
	return nil
}

func approvalDocumentsDigest(key ApprovalKey, schema, defaults []byte) (string, error) {
	return digestJSON(struct {
		Contract string      `json:"contract"`
		Approval ApprovalKey `json:"approval"`
		Schema   string      `json:"schemaDigest"`
		Defaults string      `json:"defaultsDigest"`
	}{"helm-approval-documents.v1", key, digestBytes(schema), digestBytes(defaults)})
}

type ReleasePhase string

const (
	ReleasePhaseRendering            ReleasePhase = "rendering"
	ReleasePhaseRenderFailed         ReleasePhase = "render-failed"
	ReleasePhasePayloadPending       ReleasePhase = "payload-pending"
	ReleasePhasePayloadCommitted     ReleasePhase = "payload-committed"
	ReleasePhasePayloadVerified      ReleasePhase = "payload-verified"
	ReleasePhaseApplicationPending   ReleasePhase = "application-pending"
	ReleasePhaseApplicationCommitted ReleasePhase = "application-committed"
	ReleasePhasePublished            ReleasePhase = "published"
	ReleasePhaseFailed               ReleasePhase = "failed"
)

type ReleaseStatus struct {
	Revision            ReleaseRevision `json:"revision"`
	Phase               ReleasePhase    `json:"phase"`
	RenderState         string          `json:"renderState,omitempty"`
	PayloadIntentID     string          `json:"payloadIntentId,omitempty"`
	PayloadState        string          `json:"payloadState,omitempty"`
	PayloadRevision     string          `json:"payloadRevision,omitempty"`
	ApplicationIntentID string          `json:"applicationIntentId,omitempty"`
	ApplicationState    string          `json:"applicationState,omitempty"`
	ApplicationRevision string          `json:"applicationRevision,omitempty"`
	FailureCode         string          `json:"failureCode,omitempty"`
}

type ReleaseService interface {
	ApprovalCatalog(context.Context, int) ([]ApprovalDocument, error)
	Upsert(context.Context, UpsertReleaseRequest, time.Time) (ReleaseRevision, bool, error)
	Retry(context.Context, RetryReleaseRequest, time.Time) (ReleaseRevision, bool, error)
	Disable(context.Context, DisableReleaseRequest, time.Time) (ReleaseRevision, bool, error)
	Rollback(context.Context, RollbackReleaseRequest, time.Time) (ReleaseRevision, bool, error)
	Head(context.Context, ReleaseTarget) (ReleaseStatus, error)
	History(context.Context, ReleaseTarget, int) ([]ReleaseStatus, error)
}

type ReleaseValuesService interface {
	ApprovalDocument(context.Context, ApprovalKey) (ApprovalDocument, error)
	PreviewValues(context.Context, ReleaseTarget, ApprovalKey, []byte) (ValuesPreview, error)
}

func containsControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}
