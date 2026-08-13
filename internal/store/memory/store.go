// Package memory provides a concurrency-safe Store for unit tests and local
// contract tests. Production binaries use PostgreSQL.
package memory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	accesspolicy "github.com/kuberploy/kuberploy/internal/access"
	"github.com/kuberploy/kuberploy/internal/appconfig"
	"github.com/kuberploy/kuberploy/internal/autodeploy"
	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
	"github.com/kuberploy/kuberploy/internal/gitprojection"
	"github.com/kuberploy/kuberploy/internal/gitpublication"
	"github.com/kuberploy/kuberploy/internal/id"
	"github.com/kuberploy/kuberploy/internal/middlewareprofiles"
	"github.com/kuberploy/kuberploy/internal/registry"
	base "github.com/kuberploy/kuberploy/internal/store"
)

type idemRecord struct{ fingerprint, typ, resourceID, operationID string }
type autoDeployCommandRecord struct {
	action, digest, policyID string
	revision                 int64
}
type outboxRecord struct {
	message   domain.WorkMessage
	published bool
	attempts  int
}

type variableSetPreviewRecord struct {
	actorID, bindingID, projectID, environmentID string
	scope, path, baseRevision, baseETag          string
	policyVersion                                string
	candidateHash                                []byte
	expiresAt                                    time.Time
	consumedAt                                   *time.Time
}

type invitationRecord struct {
	invitation  domain.UserInvitation
	displayName string
	accepted    bool
}

type Store struct {
	mu                        sync.Mutex
	bootstrapUsed             bool
	users                     map[string]domain.User
	invitations               map[string]invitationRecord
	teams                     map[string]domain.Team
	memberships               map[string]map[string]domain.TeamMember
	installations             map[string]domain.GitHubInstallation
	accessGrants              map[string]domain.AccessGrant
	serviceAccounts           map[string]domain.ServiceAccount
	serviceAccountTokens      map[string]domain.ServiceAccountToken
	serviceAccountTokenHashes map[string]string
	passwordCredentials       map[string]struct{ userID, hash string }
	sessions                  map[string]struct {
		userID   string
		revision int64
		expires  time.Time
	}
	projects            map[string]domain.Project
	environments        map[string]domain.Environment
	applications        map[string]domain.Application
	deployments         map[string]domain.Deployment
	argoObservations    map[string]domain.ArgoRolloutObservation
	deploymentInputs    map[string]domain.Deployment
	configPreviews      map[string]domain.ConfigPreviewLease
	configPreviewGit    map[string]gitprojection.WritePlan
	variableSetPreviews map[string]variableSetPreviewRecord
	gitWriteCommands    map[string]gitprojection.WriteCommand
	gitPublicationModes map[string]gitpublication.Mode
	gitPublications     map[string]gitpublication.Publication
	gitDocuments        map[string]gitprojection.Document
	operations          map[string]domain.Operation
	upgrades            map[string]domain.PlatformUpgrade
	idempotency         map[string]idemRecord
	outbox              map[string]*outboxRecord
	outboxDatasetID     string
	audits              int
	auditEvents         []domain.AuditEvent
	leases              map[string]struct {
		owner string
		until time.Time
	}
	registryTargets                   map[string]domain.RegistryTarget
	registryPolicies                  map[string]domain.ServiceRegistryPolicy
	projectRegistryPullCredentials    map[string]domain.ProjectRegistryPullCredential
	applicationRegistryPullSelections map[string]domain.ApplicationRegistryPullSelection
	registryInventories               map[string]domain.RegistryInventoryObservation
	registryCatalogs                  map[string]domain.RegistryCatalogSnapshot
	registryAuthorities               map[string]domain.RegistryProtectionSnapshot
	registryPins                      map[string]domain.RegistryArtifactReference
	registryReleases                  map[string]domain.RegistryRelease
	registryCaches                    map[string]domain.RegistryCacheGeneration
	registryPlans                     map[string]domain.RegistryCleanupPlan
	registryPlanDigests               map[string]string
	registryLeases                    map[string]registryCleanupLease
	registryRuntimeReadiness          map[string]registry.RuntimeReadinessLease
	externalDNSIntegrations           map[string]domain.ExternalDNSIntegration
	gitBindings                       map[string]gitprojection.Binding
	platformGitBindings               map[string]gitprojection.Binding
	buildAttemptAuditCatalog          base.BuildLogAttemptCatalog
	autoDeployPolicies                map[string]autodeploy.Policy
	autoDeployRevisions               map[string]map[int64]autodeploy.Revision
	autoDeployCommands                map[string]autoDeployCommandRecord
	autoDeployRuns                    map[string][]autodeploy.Run
}

type registryCleanupLease struct {
	planID string
	owner  string
	until  time.Time
}

func New() *Store {
	return &Store{users: map[string]domain.User{}, sessions: map[string]struct {
		userID   string
		revision int64
		expires  time.Time
	}{}, invitations: map[string]invitationRecord{}, teams: map[string]domain.Team{}, memberships: map[string]map[string]domain.TeamMember{}, installations: map[string]domain.GitHubInstallation{}, accessGrants: map[string]domain.AccessGrant{}, serviceAccounts: map[string]domain.ServiceAccount{}, serviceAccountTokens: map[string]domain.ServiceAccountToken{}, serviceAccountTokenHashes: map[string]string{}, projects: map[string]domain.Project{}, environments: map[string]domain.Environment{}, applications: map[string]domain.Application{}, deployments: map[string]domain.Deployment{}, argoObservations: map[string]domain.ArgoRolloutObservation{}, deploymentInputs: map[string]domain.Deployment{}, configPreviews: map[string]domain.ConfigPreviewLease{}, configPreviewGit: map[string]gitprojection.WritePlan{}, variableSetPreviews: map[string]variableSetPreviewRecord{}, gitWriteCommands: map[string]gitprojection.WriteCommand{}, gitPublicationModes: map[string]gitpublication.Mode{}, gitPublications: map[string]gitpublication.Publication{}, gitDocuments: map[string]gitprojection.Document{}, operations: map[string]domain.Operation{}, upgrades: map[string]domain.PlatformUpgrade{}, idempotency: map[string]idemRecord{}, outbox: map[string]*outboxRecord{}, leases: map[string]struct {
		owner string
		until time.Time
	}{}, registryTargets: map[string]domain.RegistryTarget{}, registryPolicies: map[string]domain.ServiceRegistryPolicy{}, registryInventories: map[string]domain.RegistryInventoryObservation{}, registryCatalogs: map[string]domain.RegistryCatalogSnapshot{}, registryAuthorities: map[string]domain.RegistryProtectionSnapshot{}, registryPins: map[string]domain.RegistryArtifactReference{}, registryReleases: map[string]domain.RegistryRelease{}, registryCaches: map[string]domain.RegistryCacheGeneration{}, registryPlans: map[string]domain.RegistryCleanupPlan{}, registryPlanDigests: map[string]string{}, registryLeases: map[string]registryCleanupLease{}, registryRuntimeReadiness: map[string]registry.RuntimeReadinessLease{}, externalDNSIntegrations: map[string]domain.ExternalDNSIntegration{}, gitBindings: map[string]gitprojection.Binding{}, platformGitBindings: map[string]gitprojection.Binding{}, autoDeployPolicies: map[string]autodeploy.Policy{}, autoDeployRevisions: map[string]map[int64]autodeploy.Revision{}, autoDeployCommands: map[string]autoDeployCommandRecord{}, autoDeployRuns: map[string][]autodeploy.Run{}}
}
func (s *Store) Ping(context.Context) error { return nil }
func (s *Store) Close()                     {}
func (s *Store) BootstrapRequired(context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.bootstrapUsed, nil
}
func (s *Store) BootstrapAdmin(_ context.Context, u domain.User, passwordHash string, hash []byte, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bootstrapUsed {
		return base.ErrBootstrapConsumed
	}
	if s.passwordCredentials == nil {
		s.passwordCredentials = map[string]struct{ userID, hash string }{}
	}
	login := strings.ToLower(strings.TrimSpace(u.Login))
	if _, exists := s.passwordCredentials[login]; exists || passwordHash == "" {
		return base.ErrConflict
	}
	s.bootstrapUsed = true
	s.users[u.ID] = u
	s.passwordCredentials[login] = struct{ userID, hash string }{u.ID, passwordHash}
	grantID := id.New()
	s.accessGrants[grantID] = domain.AccessGrant{ID: grantID, SubjectUserID: u.ID, Role: domain.RolePlatformAdmin, ScopeType: domain.ScopePlatform, ScopeID: "platform", Permissions: []domain.Permission{}, Source: "bootstrap", CreatedBy: u.ID, CreatedAt: u.CreatedAt}
	s.sessions[hex.EncodeToString(hash)] = struct {
		userID   string
		revision int64
		expires  time.Time
	}{u.ID, u.GrantRevision, expires}
	return nil
}
func (s *Store) LocalCredential(_ context.Context, login string) (domain.User, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, ok := s.passwordCredentials[strings.ToLower(strings.TrimSpace(login))]
	u, userOK := s.users[credential.userID]
	if !ok || !userOK {
		return domain.User{}, "", base.ErrNotFound
	}
	return u, credential.hash, nil
}
func (s *Store) CreateLoginSession(_ context.Context, userID, expectedHash, upgradedHash string, sessionHash []byte, expires time.Time) (domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok || len(sessionHash) != 32 || !expires.After(time.Now()) {
		return domain.User{}, base.ErrNotFound
	}
	login := strings.ToLower(strings.TrimSpace(u.Login))
	credential, ok := s.passwordCredentials[login]
	if !ok || credential.userID != userID || credential.hash != expectedHash {
		return domain.User{}, base.ErrNotFound
	}
	if upgradedHash != "" {
		credential.hash = upgradedHash
		s.passwordCredentials[login] = credential
	}
	s.sessions[hex.EncodeToString(sessionHash)] = struct {
		userID   string
		revision int64
		expires  time.Time
	}{u.ID, u.GrantRevision, expires}
	return u, nil
}
func (s *Store) UserBySession(_ context.Context, hash []byte, now time.Time) (domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[hex.EncodeToString(hash)]
	if !ok || !session.expires.After(now) {
		return domain.User{}, base.ErrNotFound
	}
	u, ok := s.users[session.userID]
	if !ok || u.GrantRevision != session.revision {
		return domain.User{}, base.ErrNotFound
	}
	return u, nil
}
func (s *Store) RevokeSession(_ context.Context, hash []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, hex.EncodeToString(hash))
	return nil
}

func ik(actor, scope, key string) string { return actor + "\x00" + scope + "\x00" + key }
func check(old idemRecord, ok bool, fingerprint string) error {
	if ok && old.fingerprint != fingerprint {
		return base.ErrIdempotencyConflict
	}
	return nil
}
func (s *Store) CreateProject(_ context.Context, actor, key, fp string, in domain.CreateProject) (base.Result[domain.Project], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := ik(actor, "projects.create", key)
	old, ok := s.idempotency[k]
	if err := check(old, ok, fp); err != nil {
		return base.Result[domain.Project]{}, err
	}
	if ok {
		target := domain.AccessTarget{Type: "platform", ID: "platform"}
		if in.TeamID != "" {
			target = domain.AccessTarget{Type: "team", ID: in.TeamID}
		}
		if err := s.authorizeLocked(actor, domain.PermissionResourcesWrite, target); err != nil {
			return base.Result[domain.Project]{}, err
		}
		return base.Result[domain.Project]{Value: s.projects[old.resourceID], Replay: true}, nil
	}
	target := domain.AccessTarget{Type: "platform", ID: "platform"}
	if in.TeamID != "" {
		target = domain.AccessTarget{Type: "team", ID: in.TeamID}
	}
	if err := s.authorizeLocked(actor, domain.PermissionResourcesWrite, target); err != nil {
		return base.Result[domain.Project]{}, err
	}
	for _, v := range s.projects {
		if v.Slug == in.Slug {
			return base.Result[domain.Project]{}, base.ErrConflict
		}
	}
	p := domain.Project{ID: id.New(), Name: in.Name, Slug: in.Slug, TeamID: in.TeamID, CreatedAt: time.Now().UTC()}
	s.projects[p.ID] = p
	s.idempotency[k] = idemRecord{fp, "project", p.ID, ""}
	s.audits++
	return base.Result[domain.Project]{Value: p}, nil
}
func (s *Store) GetProject(_ context.Context, v string) (domain.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.projects[v]
	if !ok {
		return x, base.ErrNotFound
	}
	return x, nil
}
func (s *Store) ListProjects(context.Context) ([]domain.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Project, 0, len(s.projects))
	for _, v := range s.projects {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) CreateEnvironment(_ context.Context, actor, key, fp string, in domain.CreateEnvironment) (base.Result[domain.Environment], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := ik(actor, "environments.create", key)
	old, ok := s.idempotency[k]
	if err := check(old, ok, fp); err != nil {
		return base.Result[domain.Environment]{}, err
	}
	if ok {
		replayed := s.environments[old.resourceID]
		if err := s.authorizeLocked(actor, domain.PermissionResourcesWrite, domain.AccessTarget{Type: "project", ID: replayed.ProjectID}); err != nil {
			return base.Result[domain.Environment]{}, err
		}
		return base.Result[domain.Environment]{Value: replayed, Replay: true}, nil
	}
	project, projectOK := s.projects[in.ProjectID]
	if !projectOK {
		return base.Result[domain.Environment]{}, base.ErrNotFound
	}
	if err := s.authorizeLocked(actor, domain.PermissionResourcesWrite, domain.AccessTarget{Type: "project", ID: project.ID}); err != nil {
		return base.Result[domain.Environment]{}, err
	}
	in.Namespace, in.ArgoProject = domain.DeriveEnvironmentDestination(project, in.Slug)
	if in.ProtectionPolicy == "" {
		in.ProtectionPolicy = domain.EnvironmentProtected
	}
	if in.ProtectionPolicy != domain.EnvironmentDevelopment && in.ProtectionPolicy != domain.EnvironmentProtected {
		return base.Result[domain.Environment]{}, base.ErrConflict
	}
	for _, v := range s.environments {
		if v.Namespace == in.Namespace || (v.ProjectID == in.ProjectID && v.Slug == in.Slug) {
			return base.Result[domain.Environment]{}, base.ErrConflict
		}
	}
	v := domain.Environment{ID: id.New(), ProjectID: in.ProjectID, Name: in.Name, Slug: in.Slug, Namespace: in.Namespace, ArgoProject: in.ArgoProject, ProtectionPolicy: in.ProtectionPolicy, CreatedAt: time.Now().UTC()}
	s.environments[v.ID] = v
	s.idempotency[k] = idemRecord{fp, "environment", v.ID, ""}
	s.audits++
	return base.Result[domain.Environment]{Value: v}, nil
}
func (s *Store) GetEnvironment(_ context.Context, v string) (domain.Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.environments[v]
	if !ok {
		return x, base.ErrNotFound
	}
	return x, nil
}
func (s *Store) ListEnvironments(context.Context) ([]domain.Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Environment, 0, len(s.environments))
	for _, v := range s.environments {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) CreateApplication(_ context.Context, actor, key, fp string, in domain.CreateApplication) (base.Result[domain.Application], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := ik(actor, "applications.create", key)
	old, ok := s.idempotency[k]
	if err := check(old, ok, fp); err != nil {
		return base.Result[domain.Application]{}, err
	}
	if ok {
		replayed := s.applications[old.resourceID]
		if err := s.authorizeLocked(actor, domain.PermissionResourcesWrite, domain.AccessTarget{Type: "project", ID: replayed.ProjectID}); err != nil {
			return base.Result[domain.Application]{}, err
		}
		return base.Result[domain.Application]{Value: replayed, Replay: true}, nil
	}
	project, projectOK := s.projects[in.ProjectID]
	if !projectOK {
		return base.Result[domain.Application]{}, base.ErrNotFound
	}
	if err := s.authorizeLocked(actor, domain.PermissionResourcesWrite, domain.AccessTarget{Type: "project", ID: project.ID}); err != nil {
		return base.Result[domain.Application]{}, err
	}
	for _, v := range s.applications {
		if v.ProjectID == in.ProjectID && v.Slug == in.Slug {
			return base.Result[domain.Application]{}, base.ErrConflict
		}
	}
	v := domain.Application{ID: id.New(), ProjectID: in.ProjectID, Name: in.Name, Slug: in.Slug, CreatedAt: time.Now().UTC()}
	s.applications[v.ID] = v
	s.idempotency[k] = idemRecord{fp, "application", v.ID, ""}
	s.audits++
	return base.Result[domain.Application]{Value: v}, nil
}
func (s *Store) GetApplication(_ context.Context, v string) (domain.Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.applications[v]
	if !ok {
		return x, base.ErrNotFound
	}
	return x, nil
}
func (s *Store) ListApplications(context.Context) ([]domain.Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Application, 0, len(s.applications))
	for _, v := range s.applications {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) CreateDeployment(_ context.Context, actor, key, fp, requestID string, in domain.CreateDeployment, projection *gitprojection.WritePlan, references ...*base.AppConfigReferencePlan) (base.Result[domain.Deployment], domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := ik(actor, "deployments.create", key)
	old, ok := s.idempotency[k]
	if err := check(old, ok, fp); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if ok {
		d := s.deployments[old.resourceID]
		if err := s.authorizeLocked(actor, domain.PermissionResourcesWrite, domain.AccessTarget{Type: "deployment", ID: d.ID}); err != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, err
		}
		opID := old.operationID
		if opID == "" {
			opID = d.OperationID
		}
		return base.Result[domain.Deployment]{Value: d, Replay: true}, s.operations[opID], nil
	}
	referencePlan, err := base.NormalizeAppConfigReferencePlan(projection, references)
	if err != nil || base.AppConfigUsesRuntimeSecrets(in.Runtime) && referencePlan == nil {
		if err == nil {
			err = base.ErrPreconditionFailed
		}
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	e, eok := s.environments[in.EnvironmentID]
	a, aok := s.applications[in.ApplicationID]
	project, pok := s.projects[e.ProjectID]
	if !eok || !aok || !pok {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrNotFound
	}
	if e.ProjectID != a.ProjectID {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrNotFound
	}
	target := domain.AccessTarget{Type: "deployment", TeamID: project.TeamID, ProjectID: project.ID, EnvironmentID: e.ID, Namespace: e.Namespace, ApplicationID: a.ID}
	bindings := s.bindingsLocked(actor)
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesRead) {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrNotFound
	}
	if !accesspolicy.HasPermission(bindings, target, domain.PermissionResourcesWrite) {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrForbidden
	}
	var projectionBinding gitprojection.Binding
	if projection != nil {
		if projection.EnvironmentID != in.EnvironmentID || projection.ApplicationID != in.ApplicationID {
			return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreconditionFailed
		}
		if projectionBinding, err = s.validateProjectionPlanLocked(projection); err != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, err
		}
	}
	now := time.Now().UTC()
	dID, opID := id.New(), id.New()
	generation := int64(1)
	configVersion := int64(1)
	created := now
	for _, current := range s.deployments {
		if current.EnvironmentID == in.EnvironmentID && current.ApplicationID == in.ApplicationID {
			dID = current.ID
			generation = current.Generation + 1
			configVersion = current.ConfigVersion + 1
			created = current.CreatedAt
			break
		}
	}
	op := domain.Operation{ID: opID, Kind: "deployment.git-write", Status: "queued", TargetType: "deployment", TargetID: dID, RequestID: requestID, Generation: generation, Progress: []domain.ProgressStep{{Name: "git-write", Status: "pending"}}, CreatedAt: now, UpdatedAt: now}
	runtime := domain.RuntimeForCreateDeployment(in)
	replicas, port, ordinary := domain.LegacyWorkloadFields(runtime)
	d := domain.Deployment{ID: dID, EnvironmentID: in.EnvironmentID, ApplicationID: in.ApplicationID, Image: in.Image, Replicas: replicas, Port: port, Environment: ordinary, Route: cloneRoute(in.Route), Runtime: cloneRuntime(runtime), RegistryPull: cloneRegistryPull(in.RegistryPull), State: "pending-git", OperationID: opID, Generation: generation, CreatedAt: created, UpdatedAt: now}
	configRaw, err := gitops.RenderAppConfig(project, e, a, d)
	if err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	if projection != nil {
		resolution, resolutionErr := s.resolveProjectedVariablesLocked(projectionBinding, runtime)
		if resolutionErr != nil {
			return base.Result[domain.Deployment]{}, domain.Operation{}, resolutionErr
		}
		d.Runtime = resolution.Runtime
		d.Replicas, d.Port, d.Environment = domain.LegacyWorkloadFields(d.Runtime)
	}
	parsedConfig, _, configDiagnostics := appconfig.ParseAndValidate(configRaw)
	middlewareRefs, refsErr := middlewareprofiles.AppConfigSecretReferences(parsedConfig)
	if len(configDiagnostics) != 0 || refsErr != nil || (base.AppConfigUsesRuntimeSecrets(d.Runtime) || len(middlewareRefs) != 0) && referencePlan == nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, base.ErrPreconditionFailed
	}
	d.ConfigRaw, d.ConfigVersion = configRaw, configVersion
	if err = s.putGitWriteCommandLocked(actor, opID, dID, projection, configRaw, "deploy("+in.ApplicationID+"): accept immutable release", now); err != nil {
		return base.Result[domain.Deployment]{}, domain.Operation{}, err
	}
	for oldID, oldOp := range s.operations {
		if oldOp.TargetID == dID && oldOp.Status == "queued" {
			oldOp.Status = "superseded"
			oldOp.FinishedAt = &now
			oldOp.UpdatedAt = now
			oldOp.Problem = &domain.ProblemData{Code: "Superseded", Detail: "A newer deployment release was accepted."}
			s.operations[oldID] = oldOp
		}
	}
	s.operations[opID] = op
	s.deployments[dID] = d
	s.deploymentInputs[opID] = d
	s.outbox[opID] = &outboxRecord{message: domain.WorkMessage{OperationID: opID, Kind: op.Kind, ScopeID: in.EnvironmentID, Generation: generation, TraceID: requestID}}
	s.idempotency[k] = idemRecord{fp, "deployment", dID, opID}
	s.audits++
	return base.Result[domain.Deployment]{Value: d}, op, nil
}
func clone(v map[string]string) map[string]string {
	o := map[string]string{}
	for k, x := range v {
		o[k] = x
	}
	return o
}
func cloneRoute(r *domain.Route) *domain.Route {
	if r == nil {
		return nil
	}
	v := *r
	return &v
}
func cloneRegistryPull(reference *domain.RegistryPullReference) *domain.RegistryPullReference {
	if reference == nil {
		return nil
	}
	value := *reference
	return &value
}
func cloneRuntime(runtime domain.WorkloadRuntime) domain.WorkloadRuntime {
	encoded, _ := json.Marshal(runtime)
	var cloned domain.WorkloadRuntime
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}
func (s *Store) GetDeployment(_ context.Context, v string) (domain.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.deployments[v]
	if !ok {
		return x, base.ErrNotFound
	}
	x.Environment = clone(x.Environment)
	x.Route = cloneRoute(x.Route)
	x.Runtime = cloneRuntime(x.Runtime)
	x.ConfigRaw = append([]byte(nil), x.ConfigRaw...)
	return x, nil
}
func (s *Store) GetDeploymentForOperation(_ context.Context, v string) (domain.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.deploymentInputs[v]
	if !ok {
		return x, base.ErrNotFound
	}
	x.Environment = clone(x.Environment)
	x.Route = cloneRoute(x.Route)
	x.Runtime = cloneRuntime(x.Runtime)
	x.ConfigRaw = append([]byte(nil), x.ConfigRaw...)
	return x, nil
}
func (s *Store) ListDeployments(context.Context) ([]domain.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Deployment, 0, len(s.deployments))
	for _, v := range s.deployments {
		v.Environment = clone(v.Environment)
		v.Route = cloneRoute(v.Route)
		v.Runtime = cloneRuntime(v.Runtime)
		v.ConfigRaw = append([]byte(nil), v.ConfigRaw...)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
func (s *Store) GetOperation(_ context.Context, v string) (domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.operations[v]
	if !ok {
		return x, base.ErrNotFound
	}
	return s.operationWithPublicationLocked(x), nil
}
func (s *Store) ListOperations(context.Context) ([]domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Operation, 0, len(s.operations))
	for _, v := range s.operations {
		out = append(out, s.operationWithPublicationLocked(v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}
func (s *Store) DeploymentStatus(_ context.Context, v string) (domain.DeploymentStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deployments[v]
	if !ok {
		return domain.DeploymentStatus{}, base.ErrNotFound
	}
	op := s.operations[d.OperationID]
	return s.deploymentStatusLocked(d, op, time.Now().UTC()), nil
}

const argoRolloutObservationMaxAge = 20 * time.Minute

func (s *Store) deploymentStatusLocked(d domain.Deployment, op domain.Operation, now time.Time) domain.DeploymentStatus {
	status := domain.DeploymentStatus{DeploymentID: d.ID, State: d.State, OperationID: op.ID, OperationStatus: op.Status,
		DesiredRevision: d.DesiredRevision, ObservedRevision: d.ObservedRevision, ArgoSyncStatus: "unknown", RolloutHealth: "unknown"}
	observation, ok := s.argoObservations[d.ID]
	environment, environmentOK := s.environments[d.EnvironmentID]
	application, applicationOK := s.applications[d.ApplicationID]
	if !ok || !environmentOK || !applicationOK || observation.DeploymentID != d.ID || observation.ApplicationID != d.ApplicationID ||
		observation.EnvironmentID != d.EnvironmentID || observation.ProjectID != environment.ProjectID || application.ProjectID != environment.ProjectID ||
		observation.DestinationNamespace != environment.Namespace || d.DesiredRevision == "" || observation.DesiredRevision != d.DesiredRevision ||
		observation.ObservedAt.IsZero() || observation.ObservedAt.After(now.Add(30*time.Second)) || now.Sub(observation.ObservedAt) > argoRolloutObservationMaxAge ||
		!validArgoStatus(observation.SyncStatus, observation.HealthStatus) ||
		(observation.SyncStatus == "synced" && observation.HealthStatus == "healthy" && observation.ObservedRevision != observation.DesiredRevision) {
		return status
	}
	observedAt := observation.ObservedAt.UTC()
	status.ArgoSyncStatus, status.RolloutHealth = observation.SyncStatus, observation.HealthStatus
	status.ArgoObservedRevision, status.ArgoObservedAt = observation.ObservedRevision, &observedAt
	return status
}

func validArgoStatus(syncStatus, healthStatus string) bool {
	validSync := syncStatus == "unknown" || syncStatus == "synced" || syncStatus == "out-of-sync"
	validHealth := healthStatus == "unknown" || healthStatus == "progressing" || healthStatus == "healthy" || healthStatus == "degraded" || healthStatus == "suspended" || healthStatus == "missing"
	return validSync && validHealth
}

// PutArgoRolloutObservation is an in-memory parity seam for observation tests.
// Read projection still independently verifies every deployment identity.
func (s *Store) PutArgoRolloutObservation(_ context.Context, observation domain.ArgoRolloutObservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.argoObservations[observation.DeploymentID] = observation
}

func (s *Store) PendingOutbox(_ context.Context, limit int) ([]domain.WorkMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.WorkMessage
	for _, v := range s.outbox {
		if !v.published {
			out = append(out, v.message)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
func (s *Store) ReconcileOutboxDataset(_ context.Context, datasetID string) (int64, error) {
	if len(datasetID) != 36 {
		return 0, errors.New("Valkey dataset identity is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outboxDatasetID == datasetID {
		return 0, nil
	}
	var replayed int64
	for operationID, event := range s.outbox {
		operation, ok := s.operations[operationID]
		if ok && event.published && (operation.Status == "queued" || operation.Status == "running") {
			event.published = false
			replayed++
		}
	}
	s.outboxDatasetID = datasetID
	return replayed, nil
}
func (s *Store) MarkOutboxPublished(_ context.Context, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.outbox[v]
	if !ok {
		return base.ErrNotFound
	}
	x.published = true
	return nil
}
func (s *Store) MarkOutboxFailure(_ context.Context, v, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.outbox[v]
	if !ok {
		return base.ErrNotFound
	}
	x.attempts++
	return nil
}
func (s *Store) LeasePendingOperations(_ context.Context, worker string, limit int, lease time.Duration) ([]domain.WorkMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var out []domain.WorkMessage
	for id, op := range s.operations {
		l := s.leases[id]
		if (op.Status == "queued" || (op.Status == "running" && !l.until.After(now))) && len(out) < limit {
			s.leases[id] = struct {
				owner string
				until time.Time
			}{worker, now.Add(lease)}
			out = append(out, s.outbox[id].message)
		}
	}
	return out, nil
}
func (s *Store) StartOperation(_ context.Context, v string, generation int64, worker string, lease time.Duration) (domain.Operation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if worker == "" || lease <= 0 {
		return domain.Operation{}, false, base.ErrConflict
	}
	op, ok := s.operations[v]
	if !ok {
		return op, false, base.ErrNotFound
	}
	if op.Generation != generation || terminal(op.Status) {
		return op, false, nil
	}
	now := time.Now().UTC()
	l := s.leases[v]
	if l.owner != "" && l.owner != worker && l.until.After(now) {
		return op, false, nil
	}
	if op.Kind == "deployment.git-write" {
		for _, other := range s.operations {
			if other.TargetID == op.TargetID && other.Generation < op.Generation && other.Status == "running" {
				delete(s.leases, v)
				return op, false, nil
			}
		}
		if current := s.deployments[op.TargetID]; current.Generation > op.Generation {
			op.Status = "superseded"
			op.Problem = &domain.ProblemData{Code: "Superseded", Detail: "A newer deployment release is current."}
			op.UpdatedAt = now
			op.FinishedAt = &now
			s.operations[v] = op
			delete(s.leases, v)
			return op, false, nil
		}
	}
	s.leases[v] = struct {
		owner string
		until time.Time
	}{worker, now.Add(lease)}
	op.Status = "running"
	op.UpdatedAt = now
	step := "git-write"
	if op.Kind == "platform.upgrade" {
		step = "upgrade"
		u := s.upgrades[op.TargetID]
		if u.Action == "rollback" {
			step = "rollback"
		}
		u.State = "running"
		u.UpdatedAt = now
		s.upgrades[u.ID] = u
	}
	op.Progress = []domain.ProgressStep{{Name: step, Status: "running", StartedAt: &now}}
	s.operations[v] = op
	return op, true, nil
}

func (s *Store) HeartbeatOperation(_ context.Context, v string, generation int64, worker string, lease time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if worker == "" || lease <= 0 {
		return base.ErrConflict
	}
	op, ok := s.operations[v]
	if !ok {
		return base.ErrNotFound
	}
	if op.Generation != generation {
		return base.ErrConflict
	}
	if terminal(op.Status) {
		return nil
	}
	now := time.Now().UTC()
	l := s.leases[v]
	if op.Status != "running" || l.owner != worker || !l.until.After(now) {
		return base.ErrOperationLeaseLost
	}
	s.leases[v] = struct {
		owner string
		until time.Time
	}{worker, now.Add(lease)}
	return nil
}
func terminal(v string) bool {
	return v == "succeeded" || v == "failed" || v == "cancelled" || v == "superseded"
}
func (s *Store) CompleteGitOperation(_ context.Context, v string, generation int64, worker string, result domain.GitPublicationResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[v]
	if !ok {
		return base.ErrNotFound
	}
	if op.Status == "succeeded" {
		if publicationResultMatchesOperation(result, s.operationWithPublicationLocked(op)) {
			return nil
		}
		return base.ErrConflict
	}
	if op.Generation != generation || (op.Kind != "deployment.git-write" && op.Kind != "variable-set.git-write") || op.Status != "running" {
		return base.ErrConflict
	}
	now := time.Now().UTC()
	l := s.leases[v]
	if worker == "" || l.owner != worker || !l.until.After(now) {
		return base.ErrOperationLeaseLost
	}
	mode := s.gitPublicationModes[v]
	if mode == "" {
		mode = gitpublication.ModeDirect
	}
	var pullRequest *domain.PullRequestPublication
	state, detail := "git-committed", "committed as "+result.Revision
	if mode == gitpublication.ModeDirect {
		if result.Mode != string(gitpublication.ModeDirect) || result.Revision == "" || result.CandidateRevision != "" ||
			result.PullRequestNumber != 0 || result.PullRequestURL != "" || result.PullRequestState != "" {
			return base.ErrConflict
		}
	} else if mode == gitpublication.ModePullRequest {
		publication, exists := s.gitPublications[v]
		if !exists || !publicationResultMatchesReceipt(result, publication) {
			return base.ErrConflict
		}
		pullRequest = pullRequestFromPublication(publication)
		state, detail = protectedDeploymentState(publication), "pull request created"
	} else {
		return base.ErrConflict
	}
	op.Status = "succeeded"
	op.GitRevision = result.Revision
	op.PullRequest = pullRequest
	op.UpdatedAt = now
	op.FinishedAt = &now
	op.Progress = []domain.ProgressStep{{Name: "git-write", Status: "succeeded", Detail: detail, FinishedAt: &now}}
	s.operations[v] = op
	if op.Kind == "deployment.git-write" {
		d := s.deployments[op.TargetID]
		if d.Generation == generation {
			d.State = state
			if mode == gitpublication.ModeDirect {
				d.DesiredRevision = result.Revision
			}
			d.UpdatedAt = now
			s.deployments[d.ID] = d
		}
	}
	s.audits++
	delete(s.leases, v)
	return nil
}

func (s *Store) operationWithPublicationLocked(op domain.Operation) domain.Operation {
	if publication, ok := s.gitPublications[op.ID]; ok && publication.PullRequestNumber > 0 {
		op.PullRequest = pullRequestFromPublication(publication)
	}
	return op
}

func pullRequestFromPublication(publication gitpublication.Publication) *domain.PullRequestPublication {
	return &domain.PullRequestPublication{Number: publication.PullRequestNumber, URL: publication.PullRequestURL,
		State: string(publication.PullRequestState), CandidateRevision: publication.CandidateRevision}
}

func publicationResultMatchesReceipt(result domain.GitPublicationResult, publication gitpublication.Publication) bool {
	return result.Mode == string(gitpublication.ModePullRequest) && result.Revision == "" &&
		result.CandidateRevision == publication.CandidateRevision && result.PullRequestNumber == publication.PullRequestNumber &&
		result.PullRequestURL == publication.PullRequestURL && result.PullRequestState == string(publication.PullRequestState) &&
		publication.PullRequestNumber > 0 && publication.Validate() == nil
}

func protectedDeploymentState(publication gitpublication.Publication) string {
	if publication.State == gitpublication.StateMergePending || publication.State == gitpublication.StateMergeVerified {
		return "merge-pending-index"
	}
	if publication.PullRequestState == gitpublication.PullRequestClosed {
		return "review-closed"
	}
	return "review-pending"
}

func publicationResultMatchesOperation(result domain.GitPublicationResult, operation domain.Operation) bool {
	if operation.PullRequest == nil {
		return result.Mode == string(gitpublication.ModeDirect) && result.Revision != "" && operation.GitRevision == result.Revision &&
			result.CandidateRevision == "" && result.PullRequestNumber == 0 && result.PullRequestURL == "" && result.PullRequestState == ""
	}
	return result.Mode == string(gitpublication.ModePullRequest) && result.Revision == "" && operation.GitRevision == "" &&
		result.CandidateRevision == operation.PullRequest.CandidateRevision && result.PullRequestNumber == operation.PullRequest.Number &&
		result.PullRequestURL == operation.PullRequest.URL && result.PullRequestState == operation.PullRequest.State
}
func (s *Store) FailOperation(_ context.Context, v string, generation int64, worker, code, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[v]
	if !ok {
		return base.ErrNotFound
	}
	if op.Generation != generation {
		return base.ErrConflict
	}
	if op.Status == "succeeded" {
		return nil
	}
	now := time.Now().UTC()
	if op.Status == "failed" && op.Problem != nil && op.Problem.Code == code && op.Problem.Detail == detail {
		return nil
	}
	l := s.leases[v]
	if op.Status != "running" || worker == "" || l.owner != worker || !l.until.After(now) {
		return base.ErrOperationLeaseLost
	}
	op.Status = "failed"
	op.Problem = &domain.ProblemData{Code: code, Detail: detail}
	op.FinishedAt = &now
	op.UpdatedAt = now
	step := "git-write"
	if op.Kind == "platform.upgrade" {
		step = "upgrade"
		if s.upgrades[op.TargetID].Action == "rollback" {
			step = "rollback"
		}
		u := s.upgrades[op.TargetID]
		u.State = "failed"
		if u.Result == nil {
			u.Result = map[string]any{}
		}
		u.Result["code"], u.Result["detail"] = code, detail
		u.UpdatedAt = now
		s.upgrades[u.ID] = u
	}
	op.Progress = []domain.ProgressStep{{Name: step, Status: "failed", Detail: detail, FinishedAt: &now}}
	s.operations[v] = op
	delete(s.leases, v)
	return nil
}

func (s *Store) CreatePlatformUpgrade(_ context.Context, actor, key, fp, requestID string, in domain.CreatePlatformUpgrade) (base.Result[domain.PlatformUpgrade], domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !base.ExactSHA256Matches(in.Release.ManifestBytes, in.Release.ManifestDigest) {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, base.ErrConflict
	}
	k := ik(actor, "platform-upgrades.create", key)
	old, ok := s.idempotency[k]
	if err := check(old, ok, fp); err != nil {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, err
	}
	if ok {
		u := s.upgrades[old.resourceID]
		u.ManifestBytes = append([]byte(nil), u.ManifestBytes...)
		return base.Result[domain.PlatformUpgrade]{Value: u, Replay: true}, s.operations[old.operationID], nil
	}
	for _, u := range s.upgrades {
		if u.State == "queued" || u.State == "running" {
			return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, base.ErrUpgradeInProgress
		}
	}
	now := time.Now().UTC()
	uID, opID := id.New(), id.New()
	op := domain.Operation{ID: opID, Kind: "platform.upgrade", Status: "queued", TargetType: "platform-upgrade", TargetID: uID, RequestID: requestID, Generation: 1, Progress: []domain.ProgressStep{{Name: "upgrade", Status: "pending"}}, CreatedAt: now, UpdatedAt: now}
	u := domain.PlatformUpgrade{ID: uID, Version: in.Release.Version, ManifestDigest: in.Release.ManifestDigest, Manifest: in.Release.Manifest, ManifestBytes: append([]byte(nil), in.Release.ManifestBytes...), State: "queued", OperationID: opID, Result: map[string]any{"action": "upgrade"}, Action: "upgrade", CreatedAt: now, UpdatedAt: now}
	s.operations[opID] = op
	s.upgrades[uID] = u
	s.outbox[opID] = &outboxRecord{message: domain.WorkMessage{OperationID: opID, Kind: op.Kind, ScopeID: uID, Generation: 1, TraceID: requestID}}
	s.idempotency[k] = idemRecord{fp, "platform-upgrade", uID, opID}
	s.audits++
	result := u
	result.ManifestBytes = append([]byte(nil), u.ManifestBytes...)
	return base.Result[domain.PlatformUpgrade]{Value: result}, op, nil
}

func (s *Store) CreatePlatformRollback(_ context.Context, actor, key, fp, requestID string, in domain.CreatePlatformRollback) (base.Result[domain.PlatformUpgrade], domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.HelmRevision < 1 || in.HelmRevision > 1_000_000 {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, base.ErrConflict
	}
	k := ik(actor, "platform-upgrades.rollback", key)
	old, ok := s.idempotency[k]
	if err := check(old, ok, fp); err != nil {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, err
	}
	if ok {
		u := s.upgrades[old.resourceID]
		u.ManifestBytes = append([]byte(nil), u.ManifestBytes...)
		return base.Result[domain.PlatformUpgrade]{Value: u, Replay: true}, s.operations[old.operationID], nil
	}
	for _, u := range s.upgrades {
		if u.State == "queued" || u.State == "running" {
			return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, base.ErrUpgradeInProgress
		}
	}
	source, ok := s.upgrades[in.SourceUpgradeID]
	if !ok {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, base.ErrNotFound
	}
	if source.State != "succeeded" || !base.ExactSHA256Matches(source.ManifestBytes, source.ManifestDigest) {
		return base.Result[domain.PlatformUpgrade]{}, domain.Operation{}, base.ErrConflict
	}
	now := time.Now().UTC()
	uID, opID := id.New(), id.New()
	metadata := map[string]any{"action": "rollback", "helmRevision": in.HelmRevision, "sourceUpgradeId": source.ID}
	op := domain.Operation{ID: opID, Kind: "platform.upgrade", Status: "queued", TargetType: "platform-upgrade", TargetID: uID, RequestID: requestID, Generation: 1, Progress: []domain.ProgressStep{{Name: "rollback", Status: "pending"}}, CreatedAt: now, UpdatedAt: now}
	u := domain.PlatformUpgrade{ID: uID, Version: source.Version, ManifestDigest: source.ManifestDigest, Manifest: source.Manifest, ManifestBytes: append([]byte(nil), source.ManifestBytes...), State: "queued", OperationID: opID, Result: metadata, Action: "rollback", HelmRevision: in.HelmRevision, SourceUpgradeID: source.ID, CreatedAt: now, UpdatedAt: now}
	s.operations[opID] = op
	s.upgrades[uID] = u
	s.outbox[opID] = &outboxRecord{message: domain.WorkMessage{OperationID: opID, Kind: op.Kind, ScopeID: uID, Generation: 1, TraceID: requestID}}
	s.idempotency[k] = idemRecord{fp, "platform-upgrade", uID, opID}
	s.audits++
	return base.Result[domain.PlatformUpgrade]{Value: u}, op, nil
}

func (s *Store) ListPlatformUpgrades(_ context.Context) ([]domain.PlatformUpgrade, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]domain.PlatformUpgrade, 0, len(s.upgrades))
	for _, u := range s.upgrades {
		u.ManifestBytes = append([]byte(nil), u.ManifestBytes...)
		items = append(items, u)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	return items, nil
}
func (s *Store) GetPlatformUpgrade(_ context.Context, id string) (domain.PlatformUpgrade, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.upgrades[id]
	if !ok {
		return u, base.ErrNotFound
	}
	u.ManifestBytes = append([]byte(nil), u.ManifestBytes...)
	return u, nil
}

func (s *Store) RecordUpgradeRunner(_ context.Context, operationID string, generation int64, worker, runnerRef string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runnerRef == "" {
		return base.ErrConflict
	}
	op, ok := s.operations[operationID]
	if !ok {
		return base.ErrNotFound
	}
	lease := s.leases[operationID]
	if op.Generation != generation || op.Kind != "platform.upgrade" || op.Status != "running" || worker == "" || lease.owner != worker || !lease.until.After(time.Now().UTC()) {
		return base.ErrConflict
	}
	u, ok := s.upgrades[op.TargetID]
	if !ok {
		return base.ErrNotFound
	}
	if u.RunnerRef != "" && u.RunnerRef != runnerRef {
		return base.ErrConflict
	}
	u.RunnerRef = runnerRef
	u.UpdatedAt = time.Now().UTC()
	s.upgrades[u.ID] = u
	return nil
}

func (s *Store) RequeueOperation(_ context.Context, operationID string, generation int64, worker, code, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[operationID]
	if !ok {
		return base.ErrNotFound
	}
	if op.Generation != generation || op.Kind != "platform.upgrade" && op.Kind != "deployment.git-write" && op.Kind != "variable-set.git-write" {
		return base.ErrConflict
	}
	if op.Status == "succeeded" {
		return nil
	}
	if op.Status == "queued" {
		return nil
	}
	now := time.Now().UTC()
	lease := s.leases[operationID]
	if op.Status != "running" || worker == "" || lease.owner != worker || !lease.until.After(now) {
		return base.ErrOperationLeaseLost
	}
	op.Status = "queued"
	op.Problem = nil
	op.FinishedAt = nil
	op.UpdatedAt = now
	step := "git-write"
	if op.Kind == "platform.upgrade" {
		step = "upgrade"
		if s.upgrades[op.TargetID].Action == "rollback" {
			step = "rollback"
		}
	}
	op.Progress = []domain.ProgressStep{{Name: step, Status: "pending", Detail: detail}}
	s.operations[operationID] = op
	if op.Kind == "platform.upgrade" {
		u := s.upgrades[op.TargetID]
		u.State = "queued"
		if u.Result == nil {
			u.Result = map[string]any{}
		}
		u.Result["code"], u.Result["detail"] = code, detail
		u.UpdatedAt = now
		s.upgrades[u.ID] = u
	}
	delete(s.leases, operationID)
	return nil
}
func (s *Store) CompleteUpgradeOperation(_ context.Context, operationID string, generation int64, worker, runnerRef string, result map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[operationID]
	if !ok {
		return base.ErrNotFound
	}
	if op.Status == "succeeded" {
		return nil
	}
	if op.Generation != generation || op.Kind != "platform.upgrade" || op.Status != "running" {
		return base.ErrConflict
	}
	now := time.Now().UTC()
	lease := s.leases[operationID]
	if worker == "" || lease.owner != worker || !lease.until.After(now) {
		return base.ErrOperationLeaseLost
	}
	op.Status = "succeeded"
	op.UpdatedAt = now
	op.FinishedAt = &now
	step := "upgrade"
	if s.upgrades[op.TargetID].Action == "rollback" {
		step = "rollback"
	}
	op.Progress = []domain.ProgressStep{{Name: step, Status: "succeeded", Detail: "runner completed: " + runnerRef, FinishedAt: &now}}
	s.operations[op.ID] = op
	u := s.upgrades[op.TargetID]
	u.State = "succeeded"
	u.RunnerRef = runnerRef
	if u.Result == nil {
		u.Result = map[string]any{}
	}
	for key, value := range result {
		u.Result[key] = value
	}
	u.UpdatedAt = now
	s.upgrades[u.ID] = u
	s.audits++
	delete(s.leases, operationID)
	return nil
}

func (s *Store) AuditCount() int  { s.mu.Lock(); defer s.mu.Unlock(); return s.audits }
func (s *Store) OutboxCount() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.outbox) }

// AddAuditEvent seeds the same safe projection used by the in-memory HTTP
// contract tests. Production mutation detail remains private to each store.
func (s *Store) AddAuditEvent(event domain.AuditEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditEvents = append(s.auditEvents, event)
}

func (s *Store) ListAuditEventsForActor(ctx context.Context, actor string, query domain.AuditEventQuery) ([]domain.AuditEvent, error) {
	if query.Limit < 1 || query.Limit > 100 || query.TargetType == "" != (query.TargetID == "") {
		return nil, base.ErrConflict
	}
	platform := s.Authorize(ctx, actor, domain.PermissionPlatformAdmin,
		domain.AccessTarget{Type: "platform", ID: "platform"}) == nil
	if !platform {
		if query.TargetType == "" {
			return nil, base.ErrNotFound
		}
		permission := domain.PermissionResourcesRead
		if query.TargetType == "operation" {
			permission = domain.PermissionOperationsRead
		}
		if err := s.Authorize(ctx, actor, permission,
			domain.AccessTarget{Type: query.TargetType, ID: query.TargetID}); err != nil {
			return nil, base.ErrNotFound
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.AuditEvent, 0, query.Limit)
	for index := len(s.auditEvents) - 1; index >= 0 && len(out) < query.Limit; index-- {
		event := s.auditEvents[index]
		if query.TargetType != "" && (event.TargetType != query.TargetType || event.TargetID != query.TargetID) ||
			query.Action != "" && event.Action != query.Action {
			continue
		}
		out = append(out, event)
	}
	return out, nil
}
