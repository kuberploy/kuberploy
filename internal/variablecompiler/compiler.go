// Package variablecompiler resolves exact Git-projected VariableSet snapshots
// into the effective runtime environment without changing the authoritative
// application AppConfig bytes.
package variablecompiler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/variables"
)

var ErrInvalid = errors.New("invalid projected variable dependency set")

type Resolution struct {
	Effective []variables.Effective
	Runtime   domain.WorkloadRuntime
}

type DependencyIntent struct {
	Path          string `json:"path"`
	Present       bool   `json:"present"`
	BlobID        string `json:"blobId,omitempty"`
	ContentSHA256 string `json:"contentSha256,omitempty"`
}

// CanonicalDependencyIntent is the small immutable parent-input identity used
// by auto-deploy policy provenance. It contains no values, but binds ordered
// project/environment presence, blob identity and exact content digest.
func CanonicalDependencyIntent(states []gitprojection.DependencyState, documents []gitprojection.Document) ([]DependencyIntent, string, error) {
	if len(states) != 2 {
		return nil, "", ErrInvalid
	}
	byPath := make(map[string]gitprojection.Document, len(documents))
	for _, document := range documents {
		if document.ApplicationID == "" {
			byPath[document.Path] = document
		}
	}
	intent := make([]DependencyIntent, 0, 2)
	for _, state := range states {
		entry := DependencyIntent{Path: state.Path, Present: state.Present}
		document, present := byPath[state.Path]
		if state.Present != present || state.Present && (state.BlobID != document.BlobID || !document.Valid) || !state.Present && state.BlobID != "" {
			return nil, "", ErrInvalid
		}
		if present {
			entry.BlobID, entry.ContentSHA256 = document.BlobID, document.ContentSHA256
		}
		intent = append(intent, entry)
	}
	hash := sha256.New()
	for _, entry := range intent {
		for _, value := range []string{entry.Path, entry.BlobID, entry.ContentSHA256, map[bool]string{true: "present", false: "absent"}[entry.Present]} {
			_, _ = hash.Write([]byte(value))
			_, _ = hash.Write([]byte{0})
		}
	}
	return intent, "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// Resolve requires the two server-derived dependency states in precedence
// order: project, then environment. Missing documents are explicit empty
// scopes; present documents must match their indexed state and parse exactly.
func Resolve(states []gitprojection.DependencyState, documents []gitprojection.Document, application domain.WorkloadRuntime) (Resolution, error) {
	if len(states) != 2 || states[0].Path == "" || states[1].Path == "" || states[0].Path == states[1].Path {
		return Resolution{}, ErrInvalid
	}
	byPath := make(map[string]gitprojection.Document, 2)
	for _, document := range documents {
		if document.ApplicationID != "" {
			continue
		}
		if _, duplicate := byPath[document.Path]; duplicate {
			return Resolution{}, ErrInvalid
		}
		byPath[document.Path] = document
	}
	resolved := [2]variables.Document{}
	for index, state := range states {
		document, present := byPath[state.Path]
		if !state.Present {
			if present || state.BlobID != "" {
				return Resolution{}, ErrInvalid
			}
			continue
		}
		if !present || !document.Valid || state.BlobID == "" || state.BlobID != document.BlobID {
			return Resolution{}, ErrInvalid
		}
		parsed, diagnostics := variables.ParseAndValidate(document.Raw)
		if len(diagnostics) != 0 {
			return Resolution{}, ErrInvalid
		}
		resolved[index] = parsed
	}
	effective, problems := variables.Resolve(resolved[0], resolved[1], application.Env)
	if len(problems) != 0 {
		return Resolution{}, ErrInvalid
	}
	runtime := domain.NormalizeWorkloadRuntime(application)
	runtime.Env = variables.RuntimeEnv(effective)
	if problems = domain.ValidateWorkloadRuntime(runtime); len(problems) != 0 {
		return Resolution{}, ErrInvalid
	}
	return Resolution{Effective: effective, Runtime: runtime}, nil
}

func States(paths []string, documents []gitprojection.Document) ([]gitprojection.DependencyState, error) {
	if len(paths) != 2 {
		return nil, ErrInvalid
	}
	byPath := make(map[string]gitprojection.Document, len(documents))
	for _, document := range documents {
		if document.ApplicationID != "" {
			return nil, ErrInvalid
		}
		if _, duplicate := byPath[document.Path]; duplicate {
			return nil, ErrInvalid
		}
		byPath[document.Path] = document
	}
	states := make([]gitprojection.DependencyState, 0, len(paths))
	for _, dependencyPath := range paths {
		state := gitprojection.DependencyState{Path: dependencyPath}
		if document, present := byPath[dependencyPath]; present {
			state.Present, state.BlobID = true, document.BlobID
			delete(byPath, dependencyPath)
		}
		states = append(states, state)
	}
	if len(byPath) != 0 {
		return nil, ErrInvalid
	}
	return states, nil
}
