package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/kuberploy/kuberploy/internal/builds"
)

type BuilderPlatformSettingsBackend interface {
	Current(context.Context) (builds.BuilderPlatformSettings, error)
	Update(context.Context, string, string, string, int64, builds.BuilderPlatformSettingsInput) (builds.BuilderPlatformSettings, bool, error)
}

type builderPlatformSettingsRequest struct {
	Revision int64 `json:"revision"`
	builds.BuilderPlatformSettingsInput
}

func (s *Server) builderPlatformSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.builderSettings == nil {
		writeProblem(w, r, http.StatusServiceUnavailable, "BuilderSettingsUnavailable", "Builder settings unavailable", "Source builder runtime is not enabled.")
		return
	}
	if r.Method == http.MethodGet {
		settings, err := s.builderSettings.Current(r.Context())
		if err != nil {
			mappedBuilderSettingsError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, settings)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input builderPlatformSettingsRequest
	if !decode(w, r, &input) {
		return
	}
	settings, replay, err := s.builderSettings.Update(r.Context(), currentUser(r.Context()).ID, key,
		"sha256:"+fingerprint(input), input.Revision, input.BuilderPlatformSettingsInput)
	if err != nil {
		mappedBuilderSettingsError(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, settings)
}

func mappedBuilderSettingsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, builds.ErrInvalid):
		writeProblem(w, r, http.StatusUnprocessableEntity, "InvalidBuilderSettings", "Invalid builder settings", "Builder settings contain invalid concurrency, scheduling, or resource quantities.")
	case errors.Is(err, builds.ErrConflict):
		writeProblem(w, r, http.StatusConflict, "BuilderSettingsConflict", "Builder settings changed", "Reload current builder settings before saving again.")
	default:
		writeProblem(w, r, http.StatusServiceUnavailable, "BuilderSettingsUnavailable", "Builder settings unavailable", "Builder settings could not be read or saved.")
	}
}
