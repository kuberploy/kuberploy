package httpapi

import (
	"errors"
	"net/http"

	"github.com/kuberploy/kuberploy/internal/imageresolution"
	"github.com/kuberploy/kuberploy/internal/store"
)

type imageResolutionPreviewRequest struct {
	EnvironmentID string `json:"environmentId"`
	ApplicationID string `json:"applicationId"`
	Image         string `json:"image"`
}

func (s *Server) previewImageResolution(w http.ResponseWriter, r *http.Request) {
	if s.imageResolution == nil {
		mappedImageResolutionError(w, r, imageresolution.ErrUnavailable)
		return
	}
	var input imageResolutionPreviewRequest
	if !decode(w, r, &input) {
		return
	}
	resolution, err := s.imageResolution.Resolve(r.Context(), currentUser(r.Context()).ID, input.ApplicationID, input.EnvironmentID, input.Image)
	if err != nil {
		mappedImageResolutionError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resolution)
}

func mappedImageResolutionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrForbidden):
		mappedError(w, r, err)
	case errors.Is(err, imageresolution.ErrInvalid):
		writeProblem(w, r, http.StatusUnprocessableEntity, "ImageReferenceInvalid", "Image reference is invalid", "Use an exact canonical registry/repository:tag or registry/repository@sha256 digest.", FieldError{Pointer: "/image", Code: "InvalidImageReference", Detail: "The registry host, repository, tag, or immutable digest is not canonical."})
	case errors.Is(err, imageresolution.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "ImageSourceNotFound", "Image source not found", "No exact authorized registry policy matches this application, environment, and repository.")
	case errors.Is(err, imageresolution.ErrConflict):
		writeProblem(w, r, http.StatusServiceUnavailable, "ImageResolutionPolicyMismatch", "Image resolution unavailable", "The registry response or operator-owned target, credential, token, or platform policy did not match exactly.")
	default:
		writeProblem(w, r, http.StatusServiceUnavailable, "ImageResolutionUnavailable", "Image resolution unavailable", "The server could not safely resolve this image reference to an immutable digest.")
	}
}
