package gitprojection

import (
	"context"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/variables"
)

// AppConfigPolicyInput is the complete exact-generation catalog presented to
// dynamic policy resolvers immediately before activation. Current contains the
// staging generation; Previous contains the formerly active generation so a
// transaction-aware resolver can safely reconcile deletion guards for paths
// removed by a direct Git push.
type AppConfigPolicyInput struct {
	Binding    Binding
	Generation Generation
	Current    []Document
	Previous   []Document
}

func (i AppConfigPolicyInput) Validate() error {
	if i.Binding.Validate() != nil || i.Generation.Validate() != nil || i.Generation.State != ProjectionStaging ||
		i.Generation.BindingID != i.Binding.ID || i.Generation.HeadRevision != i.Binding.TargetHeadRevision ||
		i.Generation.ParserVersion != i.Binding.ParserVersion || len(i.Current) > defaultMaxIndexDocuments || len(i.Previous) > defaultMaxIndexDocuments {
		return ErrInvalid
	}
	seen := map[string]struct{}{}
	for _, document := range i.Current {
		if document.Validate(i.Binding) != nil || document.Generation != i.Generation.Number || document.SourceRevision != i.Generation.HeadRevision {
			return ErrInvalid
		}
		if _, exists := seen[document.Path]; exists {
			return ErrInvalid
		}
		seen[document.Path] = struct{}{}
	}
	seen = map[string]struct{}{}
	for _, document := range i.Previous {
		if document.Validate(i.Binding) != nil || document.Generation != i.Binding.ProjectionGeneration || document.SourceRevision != i.Binding.IndexedRevision {
			return ErrInvalid
		}
		if _, exists := seen[document.Path]; exists {
			return ErrInvalid
		}
		seen[document.Path] = struct{}{}
	}
	return nil
}

// AppConfigPolicyValidation contains policy-only diagnostics keyed by exact
// AppConfig path. Schema and binding diagnostics already present on staged
// documents are retained and combined at activation.
type AppConfigPolicyValidation struct {
	Diagnostics map[string][]Diagnostic
}

func (v AppConfigPolicyValidation) ValidateFor(input AppConfigPolicyInput) error {
	if input.Validate() != nil || len(v.Diagnostics) > len(input.Current) {
		return ErrInvalid
	}
	paths := make(map[string]Document, len(input.Current))
	for _, document := range input.Current {
		if document.ApplicationID != "" {
			paths[document.Path] = document
		}
	}
	for documentPath, diagnostics := range v.Diagnostics {
		document, exists := paths[documentPath]
		if !exists || len(diagnostics) > 64-len(document.Diagnostics) {
			return ErrInvalid
		}
		for _, diagnostic := range diagnostics {
			if !validPolicyDiagnostic(diagnostic) {
				return ErrInvalid
			}
		}
	}
	return nil
}

func validPolicyDiagnostic(diagnostic Diagnostic) bool {
	return diagnostic.Code != "" && len(diagnostic.Code) <= 64 && len(diagnostic.Detail) > 0 && len(diagnostic.Detail) <= 1024 &&
		utf8.ValidString(diagnostic.Detail) && len(diagnostic.Pointer) <= 1024 && utf8.ValidString(diagnostic.Pointer)
}

// AppConfigPolicyValidator is the portable validation seam used by the memory
// projection store. Production PostgreSQL activation requires the stronger
// PostgreSQLAppConfigPolicyValidator extension below.
type AppConfigPolicyValidator interface {
	ValidateAppConfigs(context.Context, AppConfigPolicyInput) (AppConfigPolicyValidation, error)
}

// PostgreSQLAppConfigPolicyValidator runs inside the serializable generation
// activation transaction. Implementations may re-resolve metadata and update
// deletion guards through the supplied transaction, but must never perform
// provider/network I/O or read secret material.
type PostgreSQLAppConfigPolicyValidator interface {
	AppConfigPolicyValidator
	ValidateAppConfigsTx(context.Context, pgx.Tx, AppConfigPolicyInput, time.Time) (AppConfigPolicyValidation, error)
}

// SchemaOnlyAppConfigPolicyValidator is for hermetic projection/store tests.
// Production runtimes must inject a transaction-aware policy validator.
type SchemaOnlyAppConfigPolicyValidator struct{}

func (SchemaOnlyAppConfigPolicyValidator) ValidateAppConfigs(_ context.Context, input AppConfigPolicyInput) (AppConfigPolicyValidation, error) {
	if input.Validate() != nil {
		return AppConfigPolicyValidation{}, ErrInvalid
	}
	return AppConfigPolicyValidation{Diagnostics: map[string][]Diagnostic{}}, nil
}

func (validator SchemaOnlyAppConfigPolicyValidator) ValidateAppConfigsTx(ctx context.Context, _ pgx.Tx, input AppConfigPolicyInput, _ time.Time) (AppConfigPolicyValidation, error) {
	return validator.ValidateAppConfigs(ctx, input)
}

func policyDocuments(values map[string]Document) []Document {
	result := make([]Document, 0, len(values))
	for _, document := range values {
		result = append(result, cloneDocument(document))
	}
	slices.SortFunc(result, func(left, right Document) int {
		switch {
		case left.Path < right.Path:
			return -1
		case left.Path > right.Path:
			return 1
		default:
			return 0
		}
	})
	return result
}

func applyPolicyValidation(binding Binding, documents []Document, validation AppConfigPolicyValidation) ([]Document, error) {
	result := make([]Document, len(documents))
	for index, document := range documents {
		document = cloneDocument(document)
		if diagnostics, exists := validation.Diagnostics[document.Path]; exists {
			document.Diagnostics = append(document.Diagnostics, slices.Clone(diagnostics)...)
			document.Valid = len(document.Diagnostics) == 0
		}
		if document.Validate(binding) != nil {
			return nil, ErrInvalid
		}
		// A malformed inherited VariableSet must never become active: Argo reads
		// these exact parent value files, so merely marking them diagnostic would
		// allow a direct Git push to bypass the runtime compiler.
		if document.ApplicationID == "" && !document.Valid {
			return nil, ErrInvalid
		}
		result[index] = document
	}
	if err := validateResolvedVariableDependencies(binding, result); err != nil {
		return nil, err
	}
	return result, nil
}

// applyEffectiveConfigRevisions binds each valid application document to a
// commit that contains its complete three-file values input. Git records the
// last commit that changed each individual path, but Argo consumes the project
// VariableSet, environment VariableSet, and app.yaml from one target revision.
// A parent change must therefore advance every application's effective config
// revision even when app.yaml itself is byte-identical. Conversely, an
// unrelated branch commit carries the prior effective revision forward so it
// cannot cause a broad tenant rollout.
func applyEffectiveConfigRevisions(current, previous []Document, head string) ([]Document, error) {
	if !commitRE.MatchString(head) {
		return nil, ErrInvalid
	}
	previousByPath := make(map[string]Document, len(previous))
	for _, document := range previous {
		if _, duplicate := previousByPath[document.Path]; duplicate {
			return nil, ErrInvalid
		}
		previousByPath[document.Path] = document
	}
	currentParents := make(map[string]Document, 2)
	previousParents := make(map[string]Document, 2)
	for _, document := range current {
		if document.ApplicationID == "" {
			currentParents[document.Path] = document
		}
	}
	for _, document := range previous {
		if document.ApplicationID == "" {
			previousParents[document.Path] = document
		}
	}
	parentsChanged := len(currentParents) != len(previousParents)
	if !parentsChanged {
		for path, document := range currentParents {
			prior, exists := previousParents[path]
			if !exists || !sameEffectiveInput(document, prior) {
				parentsChanged = true
				break
			}
		}
	}
	result := make([]Document, len(current))
	for index, document := range current {
		document = cloneDocument(document)
		if document.ApplicationID != "" && document.Valid {
			prior, exists := previousByPath[document.Path]
			if exists && prior.ApplicationID == document.ApplicationID && prior.Valid &&
				!parentsChanged && sameEffectiveInput(document, prior) {
				document.ConfigRevision = prior.ConfigRevision
			} else {
				document.ConfigRevision = head
			}
		}
		result[index] = document
	}
	return result, nil
}

func sameEffectiveInput(left, right Document) bool {
	return left.Path == right.Path && left.ApplicationID == right.ApplicationID &&
		left.BlobID == right.BlobID && left.ContentSHA256 == right.ContentSHA256 &&
		left.SchemaVersion == right.SchemaVersion && left.ParserVersion == right.ParserVersion
}

func validateResolvedVariableDependencies(binding Binding, documents []Document) error {
	paths, err := DependencyPaths(binding)
	if err != nil {
		return ErrInvalid
	}
	parents := [2]variables.Document{}
	seen := map[string]struct{}{}
	for _, document := range documents {
		if document.ApplicationID != "" {
			continue
		}
		index := -1
		if document.Path == paths[0] {
			index = 0
		}
		if document.Path == paths[1] {
			index = 1
		}
		if index < 0 || !document.Valid {
			return ErrInvalid
		}
		if _, duplicate := seen[document.Path]; duplicate {
			return ErrInvalid
		}
		seen[document.Path] = struct{}{}
		parsed, diagnostics := variables.ParseAndValidate(document.Raw)
		if len(diagnostics) != 0 {
			return ErrInvalid
		}
		parents[index] = parsed
	}
	for _, document := range documents {
		if document.ApplicationID == "" || !document.Valid {
			continue
		}
		_, runtime, diagnostics := appconfig.ParseAndValidate(document.Raw)
		if len(diagnostics) != 0 {
			continue
		}
		effective, problems := variables.Resolve(parents[0], parents[1], runtime.Env)
		if len(problems) != 0 {
			return ErrInvalid
		}
		runtime.Env = variables.RuntimeEnv(effective)
		if problems = domain.ValidateWorkloadRuntime(runtime); len(problems) != 0 {
			return ErrInvalid
		}
	}
	return nil
}
