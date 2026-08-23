package helmapps

import "testing"

func TestDeriveReleasePhaseReportsDurableCascadeProgress(t *testing.T) {
	tests := []struct {
		name, cascade, observation, application string
		desiredEnabled                          bool
		want                                    ReleasePhase
	}{
		{name: "disable awaiting preflight", want: ReleasePhaseApplicationPending},
		{name: "preflight pending", cascade: "pending", want: ReleasePhaseApplicationPending},
		{name: "preflight claimed", cascade: "claimed", want: ReleasePhaseApplicationPending},
		{name: "preflight committed", cascade: "git-committed", want: ReleasePhaseApplicationCommitted},
		{name: "awaiting observation", cascade: "verified", observation: "pending", want: ReleasePhaseApplicationPending},
		{name: "observation claimed", cascade: "verified", observation: "claimed", want: ReleasePhaseApplicationPending},
		{name: "observation verified awaiting delete", cascade: "verified", observation: "verified", want: ReleasePhaseApplicationPending},
		{name: "delete committed", cascade: "verified", observation: "verified", application: "git-committed", want: ReleasePhaseApplicationCommitted},
		{name: "terminal delete remains published after readiness expiry", cascade: "verified", observation: "verified", application: "verified", want: ReleasePhasePublished},
		{name: "terminal delete remains published across activation gap", cascade: "verified", application: "verified", want: ReleasePhasePublished},
		{name: "legacy delete remains pending while cascade recovery runs", cascade: "claimed", application: "verified", want: ReleasePhaseApplicationPending},
		{name: "publish has no cascade", desiredEnabled: true, want: ReleasePhasePayloadVerified},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := ReleaseStatus{Revision: ReleaseRevision{DesiredEnabled: test.desiredEnabled},
				RenderState: "succeeded", PayloadState: "verified", ApplicationState: test.application}
			phase, _ := deriveReleasePhase(status, "", "", test.cascade, "",
				test.observation, "", "")
			if phase != test.want {
				t.Fatalf("phase=%s want=%s", phase, test.want)
			}
		})
	}

	status := ReleaseStatus{Revision: ReleaseRevision{DesiredEnabled: false}, RenderState: "succeeded",
		PayloadState: "verified"}
	for _, state := range []string{"failed", "superseded"} {
		phase, failure := deriveReleasePhase(status, "", "", "verified", "", state,
			"cascade-observer-failed", "")
		if phase != ReleasePhaseFailed || failure != "cascade-observer-failed" {
			t.Fatalf("observation %s phase=%s failure=%s", state, phase, failure)
		}
	}
	phase, failure := deriveReleasePhase(status, "", "", "failed",
		"cascade-path-absent-recovery-required", "", "", "")
	if phase != ReleasePhaseFailed || failure != "cascade-path-absent-recovery-required" {
		t.Fatalf("path-absence recovery phase=%s failure=%s", phase, failure)
	}
}

func TestDeriveReleasePhaseKeepsRecoverableProjectionReplansPending(t *testing.T) {
	status := ReleaseStatus{Revision: ReleaseRevision{DesiredEnabled: true},
		RenderState: "succeeded", PayloadState: "verified", ApplicationState: "superseded"}
	phase, failure := deriveReleasePhase(status, "", "", "", "", "", "", "projection-superseded")
	if phase != ReleasePhaseApplicationPending || failure != "" {
		t.Fatalf("application replan phase=%s failure=%s", phase, failure)
	}

	status.Revision.DesiredEnabled = false
	status.ApplicationState = ""
	phase, failure = deriveReleasePhase(status, "", "", "superseded",
		"cascade-projection-superseded", "", "", "")
	if phase != ReleasePhaseApplicationPending || failure != "" {
		t.Fatalf("cascade replan phase=%s failure=%s", phase, failure)
	}

	status.Revision.DesiredEnabled = true
	status.ApplicationState = "superseded"
	phase, failure = deriveReleasePhase(status, "", "", "", "", "", "", "invalid-command")
	if phase != ReleasePhaseFailed || failure != "invalid-command" {
		t.Fatalf("terminal application phase=%s failure=%s", phase, failure)
	}
}
