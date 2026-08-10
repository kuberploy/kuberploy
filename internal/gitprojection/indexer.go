package gitprojection

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/variables"
)

const (
	defaultMaxIndexDocuments = 10_000
	defaultMaxIndexBytes     = 32 << 20
)

// Indexer builds a complete shadow generation from the exact provider-verified
// target commit, then atomically activates it. Invalid AppConfigs become
// visible diagnostic documents and do not freeze the rest of the binding.
type Indexer struct {
	Store        Store
	Policy       AppConfigPolicyValidator
	MaxDocuments int
	MaxBytes     int
}

func (i Indexer) Index(ctx context.Context, lease ReconciliationLease, repository *PreparedRepository, now time.Time) (Binding, error) {
	if i.Store == nil || lease.Validate() != nil || repository == nil || repository.manager == nil || now.IsZero() || lease.BindingID != repository.Binding.ID {
		return Binding{}, ErrInvalid
	}
	binding, err := i.Store.Binding(ctx, repository.Binding.ID)
	if err != nil {
		return Binding{}, err
	}
	if binding.TargetHeadRevision != repository.Head.Commit || repository.Head.ValidateFor(binding) != nil {
		return Binding{}, ErrConflict
	}
	if binding.IndexedRevision != "" {
		_, ancestorErr := repository.manager.git(ctx, repository.MirrorPath, "merge-base", "--is-ancestor", binding.IndexedRevision, repository.Head.Commit)
		if ancestorErr != nil {
			_ = i.Store.SetBindingState(ctx, binding.ID, binding.TargetHeadRevision, BindingDiverged, now)
			return Binding{}, ErrDiverged
		}
	}
	return i.indexFull(ctx, lease, repository, binding, now)
}

// FullReindex is the explicit repair path after a force push, missing ancestor,
// parser-version change, or suspected corruption. Readers continue using the
// old active generation until the complete shadow generation is swapped.
func (i Indexer) FullReindex(ctx context.Context, lease ReconciliationLease, repository *PreparedRepository, now time.Time) (Binding, error) {
	if i.Store == nil || lease.Validate() != nil || repository == nil || repository.manager == nil || now.IsZero() || lease.BindingID != repository.Binding.ID {
		return Binding{}, ErrInvalid
	}
	binding, err := i.Store.Binding(ctx, repository.Binding.ID)
	if err != nil {
		return Binding{}, err
	}
	if binding.State != BindingDiverged || binding.TargetHeadRevision != repository.Head.Commit || repository.Head.ValidateFor(binding) != nil {
		return Binding{}, ErrConflict
	}
	if err = i.Store.SetBindingState(ctx, binding.ID, binding.TargetHeadRevision, BindingIndexing, now); err != nil {
		return Binding{}, err
	}
	binding.State = BindingIndexing
	result, err := i.indexFull(ctx, lease, repository, binding, now)
	if err != nil {
		_ = i.Store.SetBindingState(ctx, binding.ID, binding.TargetHeadRevision, BindingDiverged, now)
	}
	return result, err
}

func (i Indexer) indexFull(ctx context.Context, lease ReconciliationLease, repository *PreparedRepository, binding Binding, now time.Time) (Binding, error) {
	generation, err := i.Store.BeginGeneration(ctx, lease, repository.Head.Commit, binding.ParserVersion, now)
	if err != nil {
		return Binding{}, err
	}
	documents, err := i.readDocuments(ctx, repository, generation, now)
	if err != nil {
		_ = i.Store.FailGeneration(ctx, lease, generation, now)
		return Binding{}, err
	}
	if err = i.Store.PutDocuments(ctx, generation, documents); err != nil {
		_ = i.Store.FailGeneration(ctx, lease, generation, now)
		return Binding{}, err
	}
	return i.Store.ActivateGeneration(ctx, lease, generation, i.Policy, now)
}

func (i Indexer) readDocuments(ctx context.Context, repository *PreparedRepository, generation Generation, now time.Time) ([]Document, error) {
	maximumDocuments := i.MaxDocuments
	if maximumDocuments == 0 {
		maximumDocuments = defaultMaxIndexDocuments
	}
	maximumBytes := i.MaxBytes
	if maximumBytes == 0 {
		maximumBytes = defaultMaxIndexBytes
	}
	if maximumDocuments < 1 || maximumDocuments > defaultMaxIndexDocuments || maximumBytes < MaxDocumentBytes || maximumBytes > defaultMaxIndexBytes {
		return nil, ErrInvalid
	}
	prefix := path.Join(repository.Binding.Prefix, "apps") + "/"
	dependencies, err := DependencyPaths(repository.Binding)
	if err != nil {
		return nil, err
	}
	treeArguments := []string{"ls-tree", "-r", "-z", "--full-tree", generation.HeadRevision, "--", prefix}
	treeArguments = append(treeArguments, dependencies...)
	output, err := repository.manager.git(ctx, repository.MirrorPath, treeArguments...)
	if err != nil {
		return nil, err
	}
	entries := strings.Split(output, "\x00")
	documents := make([]Document, 0)
	totalBytes := 0
	seen := map[string]struct{}{}
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		metadata, documentPath, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 || fields[0] != "100644" || fields[1] != "blob" || !blobRE.MatchString(fields[2]) || !validRelativePath(documentPath) {
			return nil, errors.New("Git application tree contains an unsafe entry")
		}
		parts := strings.Split(strings.TrimPrefix(documentPath, prefix), "/")
		isApplication := strings.HasPrefix(documentPath, prefix) && len(parts) == 2 && uuidRE.MatchString(parts[0]) && parts[1] == "app.yaml"
		isDependency := slices.Contains(dependencies, documentPath)
		if !isApplication && !isDependency {
			// Files outside exact Kuberploy application paths are not projection
			// documents. They remain in Git but cannot influence runtime intent.
			continue
		}
		if _, duplicate := seen[documentPath]; duplicate {
			return nil, errors.New("Git tree contains a duplicate application path")
		}
		seen[documentPath] = struct{}{}
		if len(documents) >= maximumDocuments {
			return nil, errors.New("Git projection document limit exceeded")
		}
		raw, readErr := repository.manager.git(ctx, repository.MirrorPath, "cat-file", "blob", fields[2])
		if readErr != nil {
			return nil, readErr
		}
		totalBytes += len(raw)
		if len(raw) == 0 || len(raw) > MaxDocumentBytes || totalBytes > maximumBytes {
			return nil, errors.New("Git projection byte limit exceeded")
		}
		configRevision, revisionErr := repository.manager.git(ctx, repository.MirrorPath, "log", "-1", "--format=%H", generation.HeadRevision, "--", documentPath)
		configRevision = strings.TrimSpace(configRevision)
		if revisionErr != nil || !commitRE.MatchString(configRevision) {
			return nil, errors.New("Git did not return an exact config revision")
		}
		var document Document
		var documentErr error
		if isApplication {
			parsed, _, appDiagnostics := appconfig.ParseAndValidate([]byte(raw))
			diagnostics := make([]Diagnostic, 0, len(appDiagnostics)+4)
			for _, diagnostic := range appDiagnostics {
				diagnostics = append(diagnostics, Diagnostic{Code: diagnostic.Code, Detail: diagnostic.Detail, Pointer: diagnostic.Pointer})
			}
			diagnostics = append(diagnostics, bindingDiagnostics(parsed, repository.Binding, parts[0])...)
			document, documentErr = NewDocument(repository.Binding, generation.Number, parts[0], generation.HeadRevision, configRevision, fields[2], []byte(raw), parsed, diagnostics, now)
		} else {
			parsed, variableDiagnostics := variables.ParseAndValidate([]byte(raw))
			diagnostics := make([]Diagnostic, 0, len(variableDiagnostics))
			for _, diagnostic := range variableDiagnostics {
				diagnostics = append(diagnostics, Diagnostic{Code: diagnostic.Code, Detail: diagnostic.Detail, Pointer: diagnostic.Pointer})
			}
			document, documentErr = NewDependencyDocument(repository.Binding, generation.Number, documentPath, generation.HeadRevision, configRevision, fields[2], []byte(raw), parsed.Parsed, diagnostics, now)
		}
		if documentErr != nil {
			return nil, fmt.Errorf("build projected document: %w", documentErr)
		}
		documents = append(documents, document)
	}
	return documents, nil
}

func bindingDiagnostics(parsed map[string]any, binding Binding, applicationID string) []Diagnostic {
	if parsed == nil {
		return nil
	}
	wants := []struct{ pointer, value string }{
		{"/metadata/id", applicationID}, {"/spec/applicationId", applicationID},
		{"/spec/environmentId", binding.EnvironmentID}, {"/spec/projectId", binding.ProjectID},
	}
	var diagnostics []Diagnostic
	for _, want := range wants {
		if got, ok := projectedStringAt(parsed, want.pointer); !ok || got != want.value {
			diagnostics = append(diagnostics, Diagnostic{Code: "BindingMismatch", Detail: "The document identity does not match its server-owned Git binding and path.", Pointer: want.pointer})
		}
	}
	return diagnostics
}

func projectedStringAt(root map[string]any, pointer string) (string, bool) {
	var current any = root
	for _, token := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		mapping, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = mapping[token]
		if !ok {
			return "", false
		}
	}
	value, ok := current.(string)
	return value, ok
}
