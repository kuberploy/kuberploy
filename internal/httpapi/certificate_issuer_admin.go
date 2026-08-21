package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/certissuers"
)

type CertificateIssuerAdminBackend interface {
	Create(context.Context, certissuers.Command, string, certissuers.Spec) (certissuers.MutationResult, error)
	Revise(context.Context, certissuers.Command, certissuers.Ref, certissuers.Spec) (certissuers.MutationResult, error)
	Deactivate(context.Context, certissuers.Command, certissuers.Ref) (certissuers.MutationResult, error)
	ReplayCreate(context.Context, certissuers.Command, string, certissuers.Spec) (certissuers.MutationResult, bool, error)
	ReplayRevise(context.Context, certissuers.Command, certissuers.Ref, certissuers.Spec) (certissuers.MutationResult, bool, error)
	ReplayDeactivate(context.Context, certissuers.Command, certissuers.Ref) (certissuers.MutationResult, bool, error)
	List(context.Context, int) ([]certissuers.Entry, error)
	Observation(context.Context, string, int64) (certissuers.Observation, error)
}

type certificateIssuerMutationRequest struct {
	Name                        string `json:"name,omitempty"`
	BaseRevision                int64  `json:"baseRevision,omitempty"`
	Environment                 string `json:"environment"`
	Email                       string `json:"email"`
	AccountPrivateKeySecretName string `json:"accountPrivateKeySecretName"`
	Solver                      struct {
		Type               string   `json:"type"`
		DNSZones           []string `json:"dnsZones,omitempty"`
		APITokenSecretName string   `json:"apiTokenSecretName,omitempty"`
		APITokenSecretKey  string   `json:"apiTokenSecretKey,omitempty"`
	} `json:"solver"`
}

type certificateIssuerDeactivateRequest struct {
	Revision int64 `json:"revision"`
}

type certificateIssuerAdminView struct {
	ID              string                           `json:"id"`
	Name            string                           `json:"name"`
	Lifecycle       certissuers.Lifecycle            `json:"lifecycle"`
	CurrentRevision int64                            `json:"currentRevision"`
	Revision        certificateIssuerRevisionView    `json:"revision"`
	Observation     certificateIssuerObservationView `json:"observation"`
	CreatedAt       time.Time                        `json:"createdAt"`
	DeactivatedAt   *time.Time                       `json:"deactivatedAt,omitempty"`
}

type certificateIssuerRevisionView struct {
	Number                      int64                  `json:"number"`
	Environment                 string                 `json:"environment"`
	Email                       string                 `json:"email"`
	AccountPrivateKeySecretName string                 `json:"accountPrivateKeySecretName"`
	Solver                      certissuers.SolverType `json:"solver"`
	DNSZones                    []string               `json:"dnsZones,omitempty"`
	APITokenSecretName          string                 `json:"apiTokenSecretName,omitempty"`
	APITokenSecretKey           string                 `json:"apiTokenSecretKey,omitempty"`
	SpecDigest                  string                 `json:"specDigest"`
	CreatedAt                   time.Time              `json:"createdAt"`
}

type certificateIssuerObservationView struct {
	State              certissuers.ObservationState `json:"state"`
	ObservedGeneration int64                        `json:"observedGeneration,omitempty"`
	Reason             string                       `json:"reason,omitempty"`
	ObservedAt         *time.Time                   `json:"observedAt,omitempty"`
	UpdatedAt          time.Time                    `json:"updatedAt"`
}

func (request certificateIssuerMutationRequest) spec() (certissuers.Spec, error) {
	server := ""
	switch request.Environment {
	case "production":
		server = certissuers.LetsEncryptProduction
	case "staging":
		server = certissuers.LetsEncryptStaging
	default:
		return certissuers.Spec{}, certissuers.ErrInvalid
	}
	spec := certissuers.Spec{ACME: certissuers.ACME{Email: request.Email, Server: server,
		AccountPrivateKeySecretName: request.AccountPrivateKeySecretName}}
	switch request.Solver.Type {
	case "http01":
		if len(request.Solver.DNSZones) != 0 || request.Solver.APITokenSecretName != "" || request.Solver.APITokenSecretKey != "" {
			return certissuers.Spec{}, certissuers.ErrInvalid
		}
		spec.HTTP01 = &certissuers.HTTP01Spec{}
	case "dns01-cloudflare":
		spec.Cloudflare = &certissuers.CloudflareDNS01Spec{DNSZones: request.Solver.DNSZones,
			APITokenSecretName: request.Solver.APITokenSecretName, APITokenSecretKey: request.Solver.APITokenSecretKey}
	default:
		return certissuers.Spec{}, certissuers.ErrInvalid
	}
	return spec, nil
}

func (s *Server) platformCertificateIssuers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.certificateIssuerAdmin == nil {
		certificateIssuerAdminUnavailable(w, r)
		return
	}
	if r.Method == http.MethodGet {
		entries, err := s.certificateIssuerAdmin.List(r.Context(), 100)
		if err != nil {
			mappedCertificateIssuerAdminError(w, r, err)
			return
		}
		items := make([]certificateIssuerAdminView, 0, len(entries))
		for _, entry := range entries {
			observation, observationErr := s.certificateIssuerAdmin.Observation(r.Context(), entry.Profile.ID, entry.Revision.Revision)
			if observationErr != nil {
				mappedCertificateIssuerAdminError(w, r, observationErr)
				return
			}
			items = append(items, certificateIssuerView(entry, observation))
		}
		collection(w, items)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input certificateIssuerMutationRequest
	if !decode(w, r, &input) {
		return
	}
	if input.BaseRevision != 0 {
		mappedCertificateIssuerAdminError(w, r, certissuers.ErrInvalid)
		return
	}
	spec, err := input.spec()
	if err != nil {
		mappedCertificateIssuerAdminError(w, r, err)
		return
	}
	command := certificateIssuerCommand(r, key)
	name := strings.TrimSpace(input.Name)
	result, found, err := s.certificateIssuerAdmin.ReplayCreate(r.Context(), command, name, spec)
	if err != nil {
		mappedCertificateIssuerAdminError(w, r, err)
		return
	}
	if !found {
		if !s.certificateIssuerMutationsReady(r.Context()) {
			certificateIssuerAdminUnavailable(w, r)
			return
		}
		result, err = s.certificateIssuerAdmin.Create(r.Context(), command, name, spec)
	}
	if err != nil {
		mappedCertificateIssuerAdminError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	observation, err := s.certificateIssuerAdmin.Observation(r.Context(), result.Profile.ID, result.Revision.Revision)
	if err != nil {
		mappedCertificateIssuerAdminError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/platform/certificate-issuers/"+result.Profile.ID)
	writeJSON(w, http.StatusCreated, certificateIssuerView(certissuers.Entry{Profile: result.Profile, Revision: result.Revision}, observation))
}

func (s *Server) platformCertificateIssuer(w http.ResponseWriter, r *http.Request) {
	if s.certificateIssuerAdmin == nil {
		certificateIssuerAdminUnavailable(w, r)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input certificateIssuerMutationRequest
	if !decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Name) != "" || input.BaseRevision < 1 {
		mappedCertificateIssuerAdminError(w, r, certissuers.ErrInvalid)
		return
	}
	spec, err := input.spec()
	if err != nil {
		mappedCertificateIssuerAdminError(w, r, err)
		return
	}
	command := certificateIssuerCommand(r, key)
	ref := certissuers.Ref{ProfileID: strings.TrimSpace(r.PathValue("id")), Revision: input.BaseRevision}
	result, found, err := s.certificateIssuerAdmin.ReplayRevise(r.Context(), command, ref, spec)
	if err != nil {
		mappedCertificateIssuerAdminError(w, r, err)
		return
	}
	if !found {
		if !s.certificateIssuerMutationsReady(r.Context()) {
			certificateIssuerAdminUnavailable(w, r)
			return
		}
		result, err = s.certificateIssuerAdmin.Revise(r.Context(), command, ref, spec)
	}
	if err != nil {
		mappedCertificateIssuerAdminError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	observation, err := s.certificateIssuerAdmin.Observation(r.Context(), result.Profile.ID, result.Revision.Revision)
	if err != nil {
		mappedCertificateIssuerAdminError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, certificateIssuerView(certissuers.Entry{Profile: result.Profile, Revision: result.Revision}, observation))
}

func (s *Server) deactivatePlatformCertificateIssuer(w http.ResponseWriter, r *http.Request) {
	if s.certificateIssuerAdmin == nil {
		certificateIssuerAdminUnavailable(w, r)
		return
	}
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var input certificateIssuerDeactivateRequest
	if !decode(w, r, &input) {
		return
	}
	command := certificateIssuerCommand(r, key)
	ref := certissuers.Ref{ProfileID: strings.TrimSpace(r.PathValue("id")), Revision: input.Revision}
	result, found, err := s.certificateIssuerAdmin.ReplayDeactivate(r.Context(), command, ref)
	if err != nil {
		mappedCertificateIssuerAdminError(w, r, err)
		return
	}
	if !found {
		if !s.certificateIssuerMutationsReady(r.Context()) {
			certificateIssuerAdminUnavailable(w, r)
			return
		}
		result, err = s.certificateIssuerAdmin.Deactivate(r.Context(), command, ref)
	}
	if err != nil {
		mappedCertificateIssuerAdminError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	observation, err := s.certificateIssuerAdmin.Observation(r.Context(), result.Profile.ID, result.Revision.Revision)
	if err != nil {
		mappedCertificateIssuerAdminError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, certificateIssuerView(certissuers.Entry{Profile: result.Profile, Revision: result.Revision}, observation))
}

func certificateIssuerCommand(r *http.Request, key string) certissuers.Command {
	return certissuers.Command{ActorID: currentUser(r.Context()).ID, IdempotencyKey: key, RequestID: requestID(r.Context()), Now: time.Now().UTC()}
}

func certificateIssuerView(entry certissuers.Entry, observation certissuers.Observation) certificateIssuerAdminView {
	environment := "production"
	if entry.Revision.Spec.ACME.Server == certissuers.LetsEncryptStaging {
		environment = "staging"
	}
	revision := certificateIssuerRevisionView{Number: entry.Revision.Revision, Environment: environment, Email: entry.Revision.Spec.ACME.Email,
		AccountPrivateKeySecretName: entry.Revision.Spec.ACME.AccountPrivateKeySecretName, Solver: entry.Revision.Solver,
		SpecDigest: entry.Revision.SpecDigest, CreatedAt: entry.Revision.CreatedAt}
	if entry.Revision.Spec.Cloudflare != nil {
		revision.DNSZones = append([]string(nil), entry.Revision.Spec.Cloudflare.DNSZones...)
		revision.APITokenSecretName = entry.Revision.Spec.Cloudflare.APITokenSecretName
		revision.APITokenSecretKey = entry.Revision.Spec.Cloudflare.APITokenSecretKey
	}
	return certificateIssuerAdminView{ID: entry.Profile.ID, Name: entry.Profile.Name, Lifecycle: entry.Profile.Lifecycle,
		CurrentRevision: entry.Profile.CurrentRevision, Revision: revision, CreatedAt: entry.Profile.CreatedAt, DeactivatedAt: entry.Profile.DeactivatedAt,
		Observation: certificateIssuerObservationView{State: observation.State, ObservedGeneration: observation.ObservedGeneration,
			Reason: observation.Reason, ObservedAt: observation.ObservedAt, UpdatedAt: observation.UpdatedAt}}
}

func (s *Server) certificateIssuerMutationsReady(ctx context.Context) bool {
	return s.certificateIssuerRuntimeReadiness != nil && s.certificateIssuerRuntimeReadiness.Probe(ctx) == nil
}

func mappedCertificateIssuerAdminError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, certissuers.ErrInvalid):
		writeProblem(w, r, http.StatusUnprocessableEntity, "CertificateIssuerInvalid", "Certificate issuer invalid", "The closed ACME issuer profile is invalid.")
	case errors.Is(err, certissuers.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "CertificateIssuerNotFound", "Certificate issuer not found", "The requested issuer profile does not exist.")
	case errors.Is(err, certissuers.ErrConflict), errors.Is(err, certissuers.ErrInactive), errors.Is(err, certissuers.ErrReferenced):
		writeProblem(w, r, http.StatusConflict, "CertificateIssuerConflict", "Certificate issuer conflict", "The exact revision, idempotency input, lifecycle, or retained route references conflict with this mutation.")
	default:
		certificateIssuerAdminUnavailable(w, r)
	}
}

func certificateIssuerAdminUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusServiceUnavailable, "CertificateIssuerManagementUnavailable", "Certificate issuer management unavailable",
		"The protected cert-manager issuer publication runtime is not configured or not freshly ready.")
}
