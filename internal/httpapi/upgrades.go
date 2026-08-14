package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/releases"
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
	installerChart, ok := installerChart(snapshot.Release.Manifest)
	if !ok {
		writeProblem(w, r, 503, "ReleaseCheckUnavailable", "Release check unavailable", "The verified release does not contain the Kuberploy installer chart.")
		return
	}
	etag := `"` + snapshot.Release.ManifestDigest + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, s.releaseView(snapshot, installerChart))
}

func (s *Server) releaseSnapshot(r *http.Request) (releases.Snapshot, error) {
	if s.releases == nil {
		return releases.Snapshot{}, errors.New("release checker disabled")
	}
	return s.releases.Latest(r.Context())
}

func installerChart(manifest domain.ReleaseManifest) (domain.ManifestChart, bool) {
	for _, chart := range manifest.Artifacts.ComponentCharts {
		if chart.Name == "kuberploy-installer" {
			return chart, true
		}
	}
	return domain.ManifestChart{}, false
}

func (s *Server) releaseView(snapshot releases.Snapshot, installerChart domain.ManifestChart) latestReleaseResponse {
	release := snapshot.Release
	view := latestReleaseResponse{CurrentVersion: s.version, Release: latestReleaseView{Tag: release.Tag, Version: release.Version, ManifestDigest: release.ManifestDigest, PublishedAt: release.PublishedAt, NotesURL: release.Manifest.Release.NotesURL, BreakingChanges: release.Manifest.Release.BreakingChanges, Chart: installerChart, Manifest: release.Manifest}, LastCheckedAt: snapshot.LastCheckedAt}
	view.Compatibility = compatibilityView{Status: "unknown", Reasons: []string{"Kubernetes and enabled Argo Application readiness are verified by the installer Helm lifecycle before Helm reports success."}}
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
