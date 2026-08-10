package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/releases"
	"github.com/kuberploy/kuberploy/internal/store"
	"github.com/kuberploy/kuberploy/migrations"
)

type compatibilityView struct {
	Status  string   `json:"status"`
	Reasons []string `json:"reasons"`
}

type latestReleaseView struct {
	Tag             string                 `json:"tag"`
	Version         string                 `json:"version"`
	ManifestDigest  string                 `json:"manifestDigest"`
	PublishedAt     time.Time              `json:"publishedAt"`
	NotesURL        string                 `json:"notesUrl"`
	BreakingChanges bool                   `json:"breakingChanges"`
	Chart           domain.ManifestChart   `json:"chart"`
	Manifest        domain.ReleaseManifest `json:"manifest"`
}

type latestReleaseResponse struct {
	CurrentVersion  string            `json:"currentVersion"`
	UpdateAvailable bool              `json:"updateAvailable"`
	Compatibility   compatibilityView `json:"compatibility"`
	Release         latestReleaseView `json:"release"`
	LastCheckedAt   time.Time         `json:"lastCheckedAt"`
}

func (s *Server) latestRelease(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.releaseSnapshot(r)
	if err != nil {
		writeProblem(w, r, 503, "ReleaseCheckUnavailable", "Release check unavailable", "The canonical Kuberploy release could not be verified.")
		return
	}
	etag := `"` + snapshot.Release.ManifestDigest + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, s.releaseView(snapshot))
}

func (s *Server) releaseSnapshot(r *http.Request) (releases.Snapshot, error) {
	if s.releases == nil {
		return releases.Snapshot{}, errors.New("release checker disabled")
	}
	return s.releases.Latest(r.Context())
}

func (s *Server) releaseView(snapshot releases.Snapshot) latestReleaseResponse {
	release := snapshot.Release
	view := latestReleaseResponse{CurrentVersion: s.version, Release: latestReleaseView{Tag: release.Tag, Version: release.Version, ManifestDigest: release.ManifestDigest, PublishedAt: release.PublishedAt, NotesURL: release.Manifest.Release.NotesURL, BreakingChanges: release.Manifest.Release.BreakingChanges, Chart: release.Manifest.Artifacts.Chart, Manifest: release.Manifest}, LastCheckedAt: snapshot.LastCheckedAt}
	view.Compatibility = compatibilityView{Status: "unknown", Reasons: []string{"Kubernetes compatibility is verified by the namespaced upgrader before Helm mutation."}}
	if comparison, err := releases.CompareVersions(s.version, release.Version); err == nil {
		view.UpdateAvailable = comparison < 0
		if ok, _ := releases.SupportsUpgrade(s.version, release.Manifest.Compatibility.SupportedUpgradeFrom); !ok {
			view.Compatibility.Status = "incompatible"
			view.Compatibility.Reasons = append(view.Compatibility.Reasons, "The installed source version is outside this release's supported upgrade window.")
		}
	} else {
		view.Compatibility.Reasons = append(view.Compatibility.Reasons, "The installed build does not expose a stable semantic version.")
	}
	if supported, _ := releases.SchemaInWindow(migrations.CurrentSchema, release.Manifest.Compatibility.Database.MinimumUpgradeableSchema, release.Manifest.Compatibility.Database.CurrentSchema); !supported {
		view.Compatibility.Status = "incompatible"
		view.Compatibility.Reasons = append(view.Compatibility.Reasons, "The installed database schema is outside this release's compatibility window.")
	}
	return view
}

type createUpgradeRequest struct {
	TargetVersion  string `json:"targetVersion"`
	ManifestDigest string `json:"manifestDigest"`
}

func (s *Server) createPlatformUpgrade(w http.ResponseWriter, r *http.Request) {
	key, ok := idemKey(w, r)
	if !ok {
		return
	}
	var in createUpgradeRequest
	if !decode(w, r, &in) {
		return
	}
	in.TargetVersion = strings.TrimSpace(in.TargetVersion)
	in.ManifestDigest = strings.TrimSpace(in.ManifestDigest)
	if !releases.ValidStableVersion(in.TargetVersion) || !releases.ManifestDigestValid(in.ManifestDigest) {
		writeProblem(w, r, 422, "ValidationFailed", "Validation failed", "targetVersion and manifestDigest must identify an exact stable release.")
		return
	}
	snapshot, err := s.releaseSnapshot(r)
	if err != nil {
		writeProblem(w, r, 503, "ReleaseCheckUnavailable", "Release check unavailable", "The canonical Kuberploy release could not be verified.")
		return
	}
	if snapshot.Release.Version != in.TargetVersion || snapshot.Release.ManifestDigest != in.ManifestDigest {
		writeProblem(w, r, 409, "ReleaseSelectionStale", "Release selection is stale", "The exact target version and manifest digest no longer match the verified latest release.")
		return
	}
	comparison, compareErr := releases.CompareVersions(s.version, in.TargetVersion)
	if compareErr != nil {
		writeProblem(w, r, 409, "InstalledVersionUnknown", "Installed version is not upgradeable", "The running control plane does not expose a stable semantic version.")
		return
	}
	if comparison >= 0 {
		writeProblem(w, r, 409, "UpgradeNotNewer", "Upgrade not accepted", "The target release must be newer than the installed version.")
		return
	}
	if supported, windowErr := releases.SupportsUpgrade(s.version, snapshot.Release.Manifest.Compatibility.SupportedUpgradeFrom); windowErr != nil || !supported {
		writeProblem(w, r, 409, "UnsupportedUpgradePath", "Unsupported upgrade path", "The installed source version is outside the target release's supported upgrade window.")
		return
	}
	database := snapshot.Release.Manifest.Compatibility.Database
	if supported, windowErr := releases.SchemaInWindow(migrations.CurrentSchema, database.MinimumUpgradeableSchema, database.CurrentSchema); windowErr != nil || !supported {
		writeProblem(w, r, 409, "UnsupportedDatabaseSchema", "Unsupported upgrade path", "The installed database schema is outside the target release's compatibility window.")
		return
	}
	u := currentUser(r.Context())
	result, op, err := s.store.CreatePlatformUpgrade(r.Context(), u.ID, key, fingerprint(in), requestID(r.Context()), domain.CreatePlatformUpgrade{Release: snapshot.Release})
	if err != nil {
		if errors.Is(err, store.ErrUpgradeInProgress) {
			writeProblem(w, r, 409, "UpgradeInProgress", "Upgrade already active", "Wait for the active platform upgrade to reach a terminal state.")
			return
		}
		mappedError(w, r, err)
		return
	}
	if result.Replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/v1/operations/"+op.ID)
	w.Header().Set("X-Kuberploy-Resource-Location", "/v1/platform/upgrades/"+result.Value.ID)
	writeJSON(w, http.StatusAccepted, op)
}

func (s *Server) platformUpgrade(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.GetPlatformUpgrade(r.Context(), r.PathValue("id"))
	if err != nil {
		mappedError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}
