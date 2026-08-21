package argo

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const ProtectedApplicationResourcesFinalizer = "resources-finalizer.argocd.argoproj.io"

var ErrProtectedApplicationNotReady = errors.New("protected Argo Application is not ready")

var protectedGitHubRepositoryRE = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9](?:[A-Za-z0-9-]{0,38}[A-Za-z0-9])?/[A-Za-z0-9_.-]{1,100}\.git$`)

// ProtectedApplicationExpectation is the complete server-derived semantic
// identity of one Git-managed protected Helm child Application. It deliberately
// excludes status: a degraded workload must remain safely removable.
type ProtectedApplicationExpectation struct {
	Namespace, Project, RepositoryURL, TargetRevision string
	DestinationNamespace, ClusterID                   string
	ProjectID, EnvironmentID, ApplicationID           string
	ReleaseRevisionID, PayloadPath                    string
	PayloadDigest, SpecDigest, FinalizerDigest        string
}

func NewProtectedApplicationExpectation(namespace, project, repositoryURL, targetRevision,
	destinationNamespace, clusterID, projectID, environmentID, applicationID,
	releaseRevisionID, payloadPath, payloadDigest string) (ProtectedApplicationExpectation, error) {
	expectation := ProtectedApplicationExpectation{Namespace: namespace, Project: project,
		RepositoryURL: repositoryURL, TargetRevision: targetRevision,
		DestinationNamespace: destinationNamespace, ClusterID: clusterID,
		ProjectID: projectID, EnvironmentID: environmentID, ApplicationID: applicationID,
		ReleaseRevisionID: releaseRevisionID, PayloadPath: payloadPath, PayloadDigest: payloadDigest,
		FinalizerDigest: contentDigest([]byte(ProtectedApplicationResourcesFinalizer))}
	var err error
	expectation.SpecDigest, err = protectedApplicationSpecDigest(expectation)
	if err != nil || expectation.Validate() != nil {
		return ProtectedApplicationExpectation{}, ErrInvalid
	}
	return expectation, nil
}

func (e ProtectedApplicationExpectation) Validate() error {
	if !kubeRE.MatchString(e.Namespace) ||
		!kubeRE.MatchString(e.Project) || !kubeRE.MatchString(e.DestinationNamespace) ||
		!uuidRE.MatchString(e.ClusterID) || !uuidRE.MatchString(e.ProjectID) ||
		!uuidRE.MatchString(e.EnvironmentID) || !uuidRE.MatchString(e.ApplicationID) ||
		!uuidRE.MatchString(e.ReleaseRevisionID) || !commitRE.MatchString(e.TargetRevision) ||
		!protectedGitHubRepositoryRE.MatchString(e.RepositoryURL) || !digestRE.MatchString(e.PayloadDigest) ||
		!digestRE.MatchString(e.SpecDigest) || !digestRE.MatchString(e.FinalizerDigest) {
		return ErrInvalid
	}
	if e.PayloadPath != protectedPayloadPath(e) {
		return ErrInvalid
	}
	specDigest, err := protectedApplicationSpecDigest(e)
	if err != nil || specDigest != e.SpecDigest ||
		contentDigest([]byte(ProtectedApplicationResourcesFinalizer)) != e.FinalizerDigest {
		return ErrInvalid
	}
	return nil
}

func protectedApplicationName(e ProtectedApplicationExpectation) string {
	return "kp-h-" + strings.ReplaceAll(e.ApplicationID, "-", "")
}

func protectedApplicationPath(e ProtectedApplicationExpectation) string {
	return "clusters/" + e.ClusterID + "/helm-manifests/environments/" + e.EnvironmentID +
		"/applications/" + e.ApplicationID + "/revisions/" + e.ReleaseRevisionID
}

func protectedPayloadPath(e ProtectedApplicationExpectation) string {
	return protectedApplicationPath(e) + "/release.yaml"
}

func protectedApplicationLabels(e ProtectedApplicationExpectation) map[string]string {
	return map[string]string{"app.kubernetes.io/managed-by": "kuberploy",
		"app.kubernetes.io/component": "approved-helm-application",
		"kuberploy.io/project-id":     e.ProjectID, "kuberploy.io/environment-id": e.EnvironmentID,
		"kuberploy.io/application-id": e.ApplicationID}
}

func protectedApplicationAnnotations(e ProtectedApplicationExpectation) map[string]string {
	return map[string]string{"kuberploy.io/helm-release-revision": e.ReleaseRevisionID,
		"kuberploy.io/helm-payload-revision":         e.TargetRevision,
		"kuberploy.io/helm-payload-digest":           e.PayloadDigest,
		"argocd.argoproj.io/manifest-generate-paths": e.PayloadPath}
}

func protectedApplicationLiveAnnotations(e ProtectedApplicationExpectation) map[string]string {
	annotations := protectedApplicationAnnotations(e)
	annotations["argocd.argoproj.io/tracking-id"] = PlatformRootApplicationName +
		":argoproj.io/Application:" + e.Namespace + "/" + protectedApplicationName(e)
	return annotations
}

type protectedApplicationDirectory struct {
	Recurse bool   `json:"recurse"`
	Include string `json:"include"`
}

type protectedApplicationSource struct {
	RepoURL, TargetRevision, Path string
	Directory                     protectedApplicationDirectory
}

type protectedApplicationDestination struct {
	Server, Namespace string
}

type protectedApplicationAutomated struct {
	Prune, SelfHeal, AllowEmpty bool
}

type protectedApplicationSyncPolicy struct {
	Automated   protectedApplicationAutomated
	SyncOptions []string
}

type protectedApplicationSpec struct {
	Project     string
	Source      protectedApplicationSource
	Destination protectedApplicationDestination
	SyncPolicy  protectedApplicationSyncPolicy
}

func expectedProtectedApplicationSpec(e ProtectedApplicationExpectation) protectedApplicationSpec {
	return protectedApplicationSpec{Project: e.Project,
		Source: protectedApplicationSource{RepoURL: e.RepositoryURL, TargetRevision: e.TargetRevision,
			Path: protectedApplicationPath(e), Directory: protectedApplicationDirectory{Include: "release.yaml"}},
		Destination: protectedApplicationDestination{Server: "https://kubernetes.default.svc",
			Namespace: e.DestinationNamespace},
		SyncPolicy: protectedApplicationSyncPolicy{
			Automated:   protectedApplicationAutomated{Prune: true, SelfHeal: true, AllowEmpty: false},
			SyncOptions: []string{"CreateNamespace=false", "ServerSideApply=true"}},
	}
}

func protectedApplicationSpecDigest(e ProtectedApplicationExpectation) (string, error) {
	encoded, err := json.Marshal(expectedProtectedApplicationSpec(e))
	if err != nil {
		return "", ErrInvalid
	}
	return contentDigest(encoded), nil
}

// ProtectedApplicationObservation is a bounded read-only receipt of the exact
// live child object after the platform root has reconciled its Git postimage.
type ProtectedApplicationObservation struct {
	UID, ResourceVersion, SpecDigest, FinalizerDigest string
	ObservedAt                                        time.Time
}

func (o ProtectedApplicationObservation) ValidateFor(e ProtectedApplicationExpectation, now time.Time) error {
	if e.Validate() != nil || !uuidRE.MatchString(o.UID) || o.ResourceVersion == "" ||
		len(o.ResourceVersion) > 128 || stringsContainsControl(o.ResourceVersion) ||
		o.SpecDigest != e.SpecDigest || o.FinalizerDigest != e.FinalizerDigest ||
		o.ObservedAt.IsZero() || o.ObservedAt.After(now) {
		return ErrInvalid
	}
	return nil
}

type ProtectedApplicationSource interface {
	ObserveProtectedApplication(context.Context, ProtectedApplicationExpectation, time.Time) (ProtectedApplicationObservation, error)
}

func protectedApplicationWireMatches(w protectedApplicationEnvelopeWire, e ProtectedApplicationExpectation) bool {
	if w.Metadata.Name != protectedApplicationName(e) || w.Metadata.Namespace != e.Namespace || w.Metadata.DeletionTimestamp != nil ||
		!reflect.DeepEqual(w.Metadata.Finalizers, []string{ProtectedApplicationResourcesFinalizer}) ||
		!reflect.DeepEqual(w.Metadata.Labels, protectedApplicationLabels(e)) ||
		!reflect.DeepEqual(w.Metadata.Annotations, protectedApplicationLiveAnnotations(e)) {
		return false
	}
	return true
}
