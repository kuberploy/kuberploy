package builds

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kuberploy/kuberploy/internal/githubapp"
)

type memoryClaim struct {
	claim     githubapp.OneTimeClaim
	createdAt time.Time
}

type memorySetupHandoff struct {
	record             SetupHandoff
	consumedAt         *time.Time
	linkIdempotencyKey string
	linkFingerprint    string
	linkedInstallation string
}

type memoryAPIIdempotency struct {
	fingerprint string
	resourceID  string
}

type memoryReleaseProjection struct {
	state             ReleaseProjectionState
	attempts          int
	availableAt       time.Time
	leaseOwner        string
	leaseUntil        time.Time
	leaseEpoch        int64
	failureCode       string
	releaseID         string
	cacheGenerationID string
	createdAt         time.Time
	updatedAt         time.Time
	completedAt       *time.Time
}

// MemoryStore is the deterministic parity implementation used by unit tests
// and local single-process development. It applies the same transaction
// boundaries as PostgreSQL under one mutex.
type MemoryStore struct {
	mu                     sync.Mutex
	claims                 map[string]memoryClaim
	installations          map[string]Installation
	installationByProvider map[string]string
	repositories           map[string]Repository
	definitions            map[string]BuildDefinition
	deliveries             map[string]DeliveryReceipt
	attempts               map[string]BuildAttempt
	serviceGeneration      map[string]int64
	outbox                 map[string]OutboxMessage
	setupAuthorizations    map[string]SetupAuthorization
	githubUserBindings     map[string]githubapp.AccountIdentity
	githubUserOwners       map[int64]string
	setupHandoffs          map[[sha256.Size]byte]memorySetupHandoff
	apiIdempotency         map[string]memoryAPIIdempotency
	releaseProjections     map[string]memoryReleaseProjection
	runtimeReadiness       map[string]SourceBuildWorkerObservation
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		claims: map[string]memoryClaim{}, installations: map[string]Installation{}, installationByProvider: map[string]string{},
		repositories: map[string]Repository{}, definitions: map[string]BuildDefinition{}, deliveries: map[string]DeliveryReceipt{},
		attempts: map[string]BuildAttempt{}, serviceGeneration: map[string]int64{}, outbox: map[string]OutboxMessage{},
		setupAuthorizations: map[string]SetupAuthorization{}, githubUserBindings: map[string]githubapp.AccountIdentity{},
		githubUserOwners: map[int64]string{}, setupHandoffs: map[[sha256.Size]byte]memorySetupHandoff{}, apiIdempotency: map[string]memoryAPIIdempotency{},
		releaseProjections: map[string]memoryReleaseProjection{},
		runtimeReadiness:   map[string]SourceBuildWorkerObservation{},
	}
}

func claimMapKey(kind, key string) string { return kind + "\x00" + key }
func providerInstallationKey(appID, installationID int64) string {
	return strings.Join([]string{strconv.FormatInt(appID, 10), strconv.FormatInt(installationID, 10)}, "\x00")
}
func serviceKey(projectID, serviceID string) string { return projectID + "\x00" + serviceID }

func (s *MemoryStore) ClaimOnce(_ context.Context, claim githubapp.OneTimeClaim) (bool, error) {
	if err := validateClaim(claim); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := claimMapKey(claim.Kind, claim.ClaimKey)
	if _, exists := s.claims[key]; exists {
		return false, nil
	}
	s.claims[key] = memoryClaim{claim: claim, createdAt: time.Now().UTC()}
	return true, nil
}

func (s *MemoryStore) PutInstallation(_ context.Context, installation Installation) error {
	if err := installation.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	providerKey := providerInstallationKey(installation.AppID, installation.GitHubInstallationID)
	if existingID, exists := s.installationByProvider[providerKey]; exists && existingID != installation.ID {
		return ErrConflict
	}
	if current, exists := s.installations[installation.ID]; exists && (current.AppID != installation.AppID || current.GitHubInstallationID != installation.GitHubInstallationID || current.Account.ID != installation.Account.ID) {
		return ErrConflict
	}
	s.installations[installation.ID] = cloneInstallation(installation)
	s.installationByProvider[providerKey] = installation.ID
	return nil
}

func (s *MemoryStore) PutRepository(_ context.Context, repository Repository) error {
	if err := repository.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	installation, exists := s.installations[repository.InstallationID]
	if !exists {
		return ErrNotFound
	}
	if repository.Identity.OwnerID != installation.Account.ID || !strings.EqualFold(repository.Identity.OwnerLogin, installation.Account.Login) {
		return ErrUnauthorized
	}
	for id, current := range s.repositories {
		if id != repository.ID && current.InstallationID == repository.InstallationID && current.Identity.ID == repository.Identity.ID {
			return ErrConflict
		}
	}
	s.repositories[repository.ID] = cloneRepository(repository)
	return nil
}

func (s *MemoryStore) PutDefinition(_ context.Context, definition BuildDefinition) error {
	if err := definition.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	installation, installationOK := s.installations[definition.InstallationID]
	repository, repositoryOK := s.repositories[definition.RepositoryID]
	if !installationOK || !repositoryOK {
		return ErrNotFound
	}
	if repository.InstallationID != installation.ID || installation.Lifecycle == InstallationDeleted || repository.Lifecycle == RepositoryRemoved {
		return ErrUnauthorized
	}
	for id, current := range s.definitions {
		if id != definition.ID && current.ProjectID == definition.ProjectID && current.ServiceID == definition.ServiceID && current.RepositoryID == definition.RepositoryID && current.TriggerRef == definition.TriggerRef {
			return ErrConflict
		}
	}
	s.definitions[definition.ID] = cloneDefinition(definition)
	return nil
}

func (s *MemoryStore) ApplyInstallationEvent(_ context.Context, appID int64, event githubapp.InstallationEvent, now time.Time) error {
	if !validInstallationEvent(event) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, exists := s.installationByProvider[providerInstallationKey(appID, event.InstallationID)]
	if !exists {
		return ErrNotFound
	}
	installation := s.installations[id]
	if installation.Account.ID != event.Account.ID || !strings.EqualFold(installation.Account.Login, event.Account.Login) || installation.Account.Type != event.Account.Type {
		return ErrUnauthorized
	}
	installation.RepositorySelection = event.RepositorySelection
	installation.Permissions = clonePermissions(event.Permissions)
	switch event.Action {
	case "deleted":
		installation.Lifecycle = InstallationDeleted
		installation.SuspendedAt = nil
		at := now.UTC()
		installation.DeletedAt = &at
	case "suspend":
		installation.Lifecycle = InstallationSuspended
		installation.DeletedAt = nil
		at := now.UTC()
		installation.SuspendedAt = &at
	case "created", "unsuspend":
		if installation.Lifecycle == InstallationDeleted {
			return ErrUnauthorized
		}
		installation.Lifecycle, installation.SuspendedAt, installation.DeletedAt = InstallationActive, nil, nil
	case "new_permissions_accepted":
		if installation.Lifecycle == InstallationDeleted {
			return ErrUnauthorized
		}
	default:
		return ErrInvalid
	}
	installation.UpdatedAt = now.UTC()
	s.installations[id] = installation
	return nil
}

func (s *MemoryStore) ApplyRepositoryEvent(_ context.Context, appID int64, event githubapp.InstallationRepositoriesEvent, now time.Time) error {
	if !validRepositoryEvent(event) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	installationID, exists := s.installationByProvider[providerInstallationKey(appID, event.InstallationID)]
	if !exists {
		return ErrNotFound
	}
	installation := s.installations[installationID]
	if installation.Lifecycle == InstallationDeleted || installation.Account.ID != event.Account.ID || !strings.EqualFold(installation.Account.Login, event.Account.Login) || installation.Account.Type != event.Account.Type {
		return ErrUnauthorized
	}
	if event.Action == "added" {
		for _, identity := range event.Added {
			if !validRepository(identity) || identity.OwnerID != installation.Account.ID || !strings.EqualFold(identity.OwnerLogin, installation.Account.Login) {
				return ErrUnauthorized
			}
		}
		installation.RepositorySelection, installation.UpdatedAt = event.RepositorySelection, now.UTC()
		s.installations[installationID] = installation
		for _, identity := range event.Added {
			id := deterministicUUID("github-repository-v1", installationID, strconv.FormatInt(identity.ID, 10))
			created := now.UTC()
			if current, ok := s.repositories[id]; ok {
				created = current.CreatedAt
			}
			s.repositories[id] = Repository{ID: id, InstallationID: installationID, Identity: identity, Lifecycle: RepositoryActive, LastVerifiedAt: now.UTC(), CreatedAt: created, UpdatedAt: now.UTC()}
		}
		return nil
	}
	if event.Action != "removed" {
		return ErrInvalid
	}
	removeIDs := make([]string, 0, len(event.Removed))
	for _, identity := range event.Removed {
		foundID := ""
		for id, repository := range s.repositories {
			if repository.InstallationID == installationID && repository.Identity.ID == identity.ID {
				if repository.Identity.OwnerID != identity.OwnerID || repository.Identity.Name != identity.Name || !strings.EqualFold(repository.Identity.OwnerLogin, identity.OwnerLogin) {
					return ErrUnauthorized
				}
				foundID = id
			}
		}
		if foundID == "" {
			return ErrNotFound
		}
		removeIDs = append(removeIDs, foundID)
	}
	installation.RepositorySelection, installation.UpdatedAt = event.RepositorySelection, now.UTC()
	s.installations[installationID] = installation
	for _, repositoryID := range removeIDs {
		repository := s.repositories[repositoryID]
		at := now.UTC()
		repository.Lifecycle, repository.RemovedAt, repository.UpdatedAt = RepositoryRemoved, &at, at
		s.repositories[repositoryID] = repository
	}
	return nil
}

func (s *MemoryStore) ClaimDelivery(_ context.Context, claim githubapp.OneTimeClaim, receipt DeliveryReceipt) (bool, error) {
	if err := validateClaim(claim); err != nil || claim.Kind != "github-delivery" || !claim.Permanent {
		return false, ErrInvalid
	}
	receipt.ClaimKey = claim.ClaimKey
	if err := validateReceipt(receipt); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	claimKey := claimMapKey(claim.Kind, claim.ClaimKey)
	if existing, exists := s.deliveries[claim.ClaimKey]; exists {
		if !sameReceiptIdentity(existing, receipt) {
			return false, ErrConflict
		}
		return false, nil
	}
	if _, exists := s.claims[claimKey]; exists {
		return false, ErrConflict
	}
	s.claims[claimKey] = memoryClaim{claim: claim, createdAt: receipt.ReceivedAt}
	s.deliveries[receipt.ClaimKey] = cloneReceipt(receipt)
	return true, nil
}

func (s *MemoryStore) Delivery(_ context.Context, claimKey string) (DeliveryReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt, ok := s.deliveries[claimKey]
	if !ok {
		return DeliveryReceipt{}, ErrNotFound
	}
	return cloneReceipt(receipt), nil
}

func (s *MemoryStore) PendingDeliveries(_ context.Context, now time.Time, limit int) ([]string, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	receipts := make([]DeliveryReceipt, 0)
	for _, receipt := range s.deliveries {
		if !terminalDelivery(receipt.State) && !receipt.AvailableAt.After(now.UTC()) && (receipt.LeaseOwner == "" || !receipt.LeaseUntil.After(now.UTC())) {
			receipts = append(receipts, receipt)
		}
	}
	sort.Slice(receipts, func(i, j int) bool {
		if receipts[i].ReceivedAt.Equal(receipts[j].ReceivedAt) {
			return receipts[i].ClaimKey < receipts[j].ClaimKey
		}
		return receipts[i].ReceivedAt.Before(receipts[j].ReceivedAt)
	})
	if len(receipts) > limit {
		receipts = receipts[:limit]
	}
	result := make([]string, len(receipts))
	for i := range receipts {
		result[i] = receipts[i].ClaimKey
	}
	return result, nil
}

// PurgeExpiredDeliveryPayloads removes only terminal typed payloads after the
// claim's retention deadline. Permanent claim tombstones are deliberately
// retained so a provider delivery can never be accepted twice.
func (s *MemoryStore) PurgeExpiredDeliveryPayloads(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var purged int64
	for claimKey, receipt := range s.deliveries {
		claim, exists := s.claims[claimMapKey("github-delivery", claimKey)]
		if !exists || !claim.claim.Permanent || !terminalDelivery(receipt.State) || len(receipt.TypedEvent) == 0 || claim.claim.RetainUntil.After(now.UTC()) {
			continue
		}
		receipt.TypedEvent = nil
		receipt.UpdatedAt = now.UTC()
		s.deliveries[claimKey] = receipt
		purged++
	}
	return purged, nil
}

func (s *MemoryStore) AcquireDelivery(_ context.Context, claimKey, owner string, now time.Time, duration time.Duration) (DeliveryReceipt, bool, error) {
	if !validOwnerLease(owner, duration) {
		return DeliveryReceipt{}, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt, exists := s.deliveries[claimKey]
	if !exists {
		return DeliveryReceipt{}, false, ErrNotFound
	}
	if terminalDelivery(receipt.State) {
		return cloneReceipt(receipt), false, nil
	}
	if receipt.AvailableAt.After(now.UTC()) || receipt.LeaseOwner != "" && receipt.LeaseUntil.After(now.UTC()) {
		return cloneReceipt(receipt), false, nil
	}
	receipt.State, receipt.LeaseOwner, receipt.LeaseUntil, receipt.UpdatedAt = DeliveryProcessing, owner, now.UTC().Add(duration), now.UTC()
	s.deliveries[claimKey] = receipt
	return cloneReceipt(receipt), true, nil
}

func (s *MemoryStore) HeartbeatDelivery(_ context.Context, claimKey, owner string, now time.Time, duration time.Duration) error {
	if !validOwnerLease(owner, duration) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt, ok := s.deliveries[claimKey]
	if !ok {
		return ErrNotFound
	}
	if receipt.State != DeliveryProcessing || receipt.LeaseOwner != owner || !receipt.LeaseUntil.After(now.UTC()) {
		return ErrLeaseLost
	}
	receipt.LeaseUntil, receipt.UpdatedAt = now.UTC().Add(duration), now.UTC()
	s.deliveries[claimKey] = receipt
	return nil
}

func (s *MemoryStore) RetryDelivery(_ context.Context, claimKey, owner, code string, now, availableAt time.Time) error {
	if validateFailureCode(code) != nil || availableAt.Before(now.UTC()) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt, ok := s.deliveries[claimKey]
	if !ok {
		return ErrNotFound
	}
	if receipt.State != DeliveryProcessing || receipt.LeaseOwner != owner || !receipt.LeaseUntil.After(now.UTC()) {
		return ErrLeaseLost
	}
	receipt.State, receipt.FailureCode, receipt.AvailableAt = DeliveryClaimed, code, availableAt.UTC()
	receipt.LeaseOwner, receipt.LeaseUntil, receipt.UpdatedAt = "", time.Time{}, now.UTC()
	s.deliveries[claimKey] = receipt
	return nil
}

func (s *MemoryStore) FinishDelivery(_ context.Context, claimKey, owner string, state DeliveryState, code string, now time.Time) error {
	if state != DeliveryIgnored && state != DeliveryFailed || state == DeliveryFailed && validateFailureCode(code) != nil || state == DeliveryIgnored && code != "" {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt, ok := s.deliveries[claimKey]
	if !ok {
		return ErrNotFound
	}
	if receipt.State != DeliveryProcessing || receipt.LeaseOwner != owner || !receipt.LeaseUntil.After(now.UTC()) {
		return ErrLeaseLost
	}
	completed := now.UTC()
	receipt.State, receipt.FailureCode, receipt.CompletedAt, receipt.UpdatedAt = state, code, &completed, completed
	receipt.LeaseOwner, receipt.LeaseUntil = "", time.Time{}
	s.deliveries[claimKey] = receipt
	return nil
}

func (s *MemoryStore) AuthorizePush(_ context.Context, appID, providerInstallationID int64, identity githubapp.RepositoryIdentity, ref string) (AuthorizedPush, error) {
	if !validRepository(identity) || !validGitRef(ref) {
		return AuthorizedPush{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	installationID, exists := s.installationByProvider[providerInstallationKey(appID, providerInstallationID)]
	if !exists {
		return AuthorizedPush{}, ErrNotFound
	}
	installation := s.installations[installationID]
	if installation.Lifecycle != InstallationActive {
		return AuthorizedPush{}, ErrUnauthorized
	}
	var repository Repository
	for _, candidate := range s.repositories {
		if candidate.InstallationID == installation.ID && candidate.Identity.ID == identity.ID {
			repository = candidate
			break
		}
	}
	if repository.ID == "" {
		return AuthorizedPush{}, ErrNotFound
	}
	if repository.Lifecycle != RepositoryActive || repository.Identity.OwnerID != identity.OwnerID || repository.Identity.Name != identity.Name || !strings.EqualFold(repository.Identity.OwnerLogin, identity.OwnerLogin) {
		return AuthorizedPush{}, ErrUnauthorized
	}
	definitions := make([]BuildDefinition, 0)
	for _, definition := range s.definitions {
		if definition.Enabled && definition.InstallationID == installation.ID && definition.RepositoryID == repository.ID && definition.TriggerRef == ref {
			definitions = append(definitions, cloneDefinition(definition))
		}
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].ID < definitions[j].ID })
	return AuthorizedPush{Installation: cloneInstallation(installation), Repository: cloneRepository(repository), Definitions: definitions}, nil
}

func (s *MemoryStore) EnqueuePushBuilds(_ context.Context, input EnqueuePush, owner string, definitions []AttemptDefinition, now time.Time) ([]BuildAttempt, error) {
	if !commitRE.MatchString(input.CommitSHA) || !validGitRef(input.GitRef) || input.ResolvedAt.IsZero() || owner == "" {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt, exists := s.deliveries[input.ClaimKey]
	if !exists {
		return nil, ErrNotFound
	}
	if receipt.State == DeliveryEnqueued {
		return s.attemptsForDeliveryLocked(input.ClaimKey), nil
	}
	if receipt.State != DeliveryProcessing || receipt.LeaseOwner != owner || !receipt.LeaseUntil.After(now.UTC()) || receipt.GitRef != input.GitRef {
		return nil, ErrLeaseLost
	}
	if len(definitions) == 0 {
		return nil, ErrInvalid
	}
	staged := make([]BuildAttempt, 0, len(definitions))
	stagedGenerations := make(map[string]int64)
	for _, requested := range definitions {
		current, ok := s.definitions[requested.Definition.ID]
		if !ok || !sameDefinition(current, requested.Definition) || !current.Enabled {
			return nil, ErrUnauthorized
		}
		installation, ok := s.installations[current.InstallationID]
		if !ok || installation.Lifecycle != InstallationActive {
			return nil, ErrUnauthorized
		}
		repository, ok := s.repositories[current.RepositoryID]
		if !ok || repository.Lifecycle != RepositoryActive || repository.InstallationID != installation.ID {
			return nil, ErrUnauthorized
		}
		if existing, ok := s.attempts[deterministicUUID("build-attempt-v1", input.ClaimKey, current.ID)]; ok {
			staged = append(staged, cloneAttempt(existing))
			continue
		}
		var existingPush *BuildAttempt
		for _, candidate := range s.attempts {
			receipt, receiptExists := s.deliveries[candidate.DeliveryClaimKey]
			if !receiptExists || len(receipt.TypedEvent) == 0 || candidate.DefinitionID != current.ID ||
				candidate.CommitSHA != input.CommitSHA || candidate.GitRef != input.GitRef {
				continue
			}
			copy := candidate
			if existingPush == nil || copy.Generation < existingPush.Generation ||
				(copy.Generation == existingPush.Generation && copy.ID < existingPush.ID) {
				existingPush = &copy
			}
		}
		if existingPush != nil {
			staged = append(staged, cloneAttempt(*existingPush))
			continue
		}
		key := serviceKey(current.ProjectID, current.ServiceID)
		generation := s.serviceGeneration[key]
		if value, ok := stagedGenerations[key]; ok {
			generation = value
		}
		generation++
		imports := s.cacheImportsLocked(current, generation)
		attempt, err := newAttemptWithExecution(current, requested.Execution, repository, input, generation, imports, now)
		if err != nil {
			return nil, err
		}
		stagedGenerations[key] = generation
		staged = append(staged, attempt)
	}
	for key, generation := range stagedGenerations {
		s.serviceGeneration[key] = generation
	}
	for _, attempt := range staged {
		if _, exists := s.attempts[attempt.ID]; !exists {
			s.attempts[attempt.ID] = cloneAttempt(attempt)
			s.outbox[attempt.ID] = OutboxMessage{AttemptID: attempt.ID, Kind: "source-build", TraceID: input.ClaimKey, AvailableAt: now.UTC()}
		}
	}
	completed := now.UTC()
	receipt.State, receipt.FailureCode, receipt.CompletedAt, receipt.UpdatedAt = DeliveryEnqueued, "", &completed, completed
	receipt.LeaseOwner, receipt.LeaseUntil = "", time.Time{}
	s.deliveries[input.ClaimKey] = receipt
	return cloneAttempts(staged), nil
}

func (s *MemoryStore) cacheImportsLocked(definition BuildDefinition, generation int64) []string {
	candidates := make([]BuildAttempt, 0)
	for _, attempt := range s.attempts {
		if attempt.ProjectID == definition.ProjectID && attempt.ServiceID == definition.ServiceID && attempt.DefinitionDigest == definition.DefinitionDigest && attempt.State == AttemptSucceeded && attempt.CacheReference != "" && attempt.Generation < generation {
			candidates = append(candidates, attempt)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Generation > candidates[j].Generation })
	if len(candidates) > definition.Spec.CacheImports {
		candidates = candidates[:definition.Spec.CacheImports]
	}
	refs := make([]string, len(candidates))
	for index, attempt := range candidates {
		refs[index] = attempt.CacheReference
	}
	sort.Strings(refs)
	return refs
}

func (s *MemoryStore) Attempt(_ context.Context, attemptID string) (BuildAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, ok := s.attempts[attemptID]
	if !ok {
		return BuildAttempt{}, ErrNotFound
	}
	return cloneAttempt(attempt), nil
}

func (s *MemoryStore) AttemptAuthorization(_ context.Context, attemptID string) (Installation, Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, ok := s.attempts[attemptID]
	if !ok {
		return Installation{}, Repository{}, ErrNotFound
	}
	definition, ok := s.definitions[attempt.DefinitionID]
	if !ok {
		return Installation{}, Repository{}, ErrNotFound
	}
	installation, installationOK := s.installations[definition.InstallationID]
	repository, repositoryOK := s.repositories[definition.RepositoryID]
	if !installationOK || !repositoryOK || !definition.Enabled || definition.DefinitionDigest != attempt.DefinitionDigest || installation.Lifecycle != InstallationActive || repository.Lifecycle != RepositoryActive {
		return Installation{}, Repository{}, ErrUnauthorized
	}
	return cloneInstallation(installation), cloneRepository(repository), nil
}

func (s *MemoryStore) ClaimNextAttempt(_ context.Context, owner string, now time.Time, duration time.Duration) (BuildAttempt, error) {
	if !validOwnerLease(owner, duration) {
		return BuildAttempt{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidates := make([]BuildAttempt, 0)
	for _, attempt := range s.attempts {
		if terminalAttempt(attempt.State) || attempt.AvailableAt.After(now.UTC()) || attempt.LeaseOwner != "" && attempt.LeaseUntil.After(now.UTC()) {
			continue
		}
		candidates = append(candidates, attempt)
	}
	if len(candidates) == 0 {
		return BuildAttempt{}, ErrNotFound
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})
	attempt := candidates[0]
	if attempt.State == AttemptQueued {
		attempt.State = AttemptPreparing
		attempt.ExecutionAttempts++
		if attempt.StartedAt == nil {
			started := now.UTC()
			attempt.StartedAt = &started
		}
	}
	if attempt.CancelRequestedAt != nil {
		attempt.State = AttemptCancelling
	}
	attempt.LeaseOwner, attempt.LeaseUntil, attempt.UpdatedAt = owner, now.UTC().Add(duration), now.UTC()
	s.attempts[attempt.ID] = attempt
	return cloneAttempt(attempt), nil
}

func (s *MemoryStore) HeartbeatAttempt(_ context.Context, attemptID, owner string, now time.Time, duration time.Duration) error {
	if !validOwnerLease(owner, duration) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, ok := s.attempts[attemptID]
	if !ok {
		return ErrNotFound
	}
	if terminalAttempt(attempt.State) || attempt.LeaseOwner != owner || !attempt.LeaseUntil.After(now.UTC()) {
		return ErrLeaseLost
	}
	attempt.LeaseUntil, attempt.UpdatedAt = now.UTC().Add(duration), now.UTC()
	s.attempts[attemptID] = attempt
	return nil
}

func (s *MemoryStore) MarkAttemptRunning(_ context.Context, attemptID, owner string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, ok := s.attempts[attemptID]
	if !ok {
		return ErrNotFound
	}
	if attempt.LeaseOwner != owner || !attempt.LeaseUntil.After(now.UTC()) || (attempt.State != AttemptPreparing && attempt.State != AttemptRunning) {
		return ErrLeaseLost
	}
	attempt.State, attempt.UpdatedAt = AttemptRunning, now.UTC()
	s.attempts[attemptID] = attempt
	return nil
}

func (s *MemoryStore) DeferAttempt(_ context.Context, attemptID, owner, code string, now, availableAt time.Time) error {
	if validateFailureCode(code) != nil || availableAt.Before(now.UTC()) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, ok := s.attempts[attemptID]
	if !ok {
		return ErrNotFound
	}
	if attempt.LeaseOwner != owner || !attempt.LeaseUntil.After(now.UTC()) || terminalAttempt(attempt.State) || attempt.CancelRequestedAt != nil {
		return ErrLeaseLost
	}
	attempt.AvailableAt, attempt.FailureCode, attempt.UpdatedAt = availableAt.UTC(), code, now.UTC()
	attempt.LeaseOwner, attempt.LeaseUntil = "", time.Time{}
	s.attempts[attempt.ID] = attempt
	return nil
}

func (s *MemoryStore) ScheduleAttemptRetry(_ context.Context, attemptID, owner, code string, now, availableAt time.Time) (bool, error) {
	if validateFailureCode(code) != nil || availableAt.Before(now.UTC()) {
		return false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, ok := s.attempts[attemptID]
	if !ok {
		return false, ErrNotFound
	}
	if attempt.LeaseOwner != owner || !attempt.LeaseUntil.After(now.UTC()) || terminalAttempt(attempt.State) {
		return false, ErrLeaseLost
	}
	if attempt.CancelRequestedAt != nil {
		attempt.State, attempt.FailureCode, attempt.AvailableAt = AttemptCancelling, code, availableAt.UTC()
		attempt.LeaseOwner, attempt.LeaseUntil, attempt.UpdatedAt = "", time.Time{}, now.UTC()
		s.attempts[attempt.ID] = attempt
		return true, nil
	}
	attempt.LeaseOwner, attempt.LeaseUntil, attempt.FailureCode, attempt.UpdatedAt = "", time.Time{}, code, now.UTC()
	if attempt.ExecutionAttempts >= attempt.MaxAttempts {
		completed := now.UTC()
		attempt.State, attempt.CompletedAt = AttemptFailed, &completed
		s.attempts[attempt.ID] = attempt
		return false, nil
	}
	attempt.State, attempt.AvailableAt = AttemptQueued, availableAt.UTC()
	s.attempts[attempt.ID] = attempt
	return true, nil
}

func (s *MemoryStore) FailAttempt(_ context.Context, attemptID, owner, code string, now time.Time) error {
	if validateFailureCode(code) != nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, ok := s.attempts[attemptID]
	if !ok {
		return ErrNotFound
	}
	if attempt.LeaseOwner != owner || !attempt.LeaseUntil.After(now.UTC()) || terminalAttempt(attempt.State) || attempt.CancelRequestedAt != nil {
		return ErrLeaseLost
	}
	completed := now.UTC()
	attempt.State, attempt.FailureCode, attempt.CompletedAt, attempt.UpdatedAt = AttemptFailed, code, &completed, completed
	attempt.LeaseOwner, attempt.LeaseUntil = "", time.Time{}
	s.attempts[attempt.ID] = attempt
	return nil
}

func (s *MemoryStore) CompleteAttempt(_ context.Context, attemptID, owner string, completion BuildCompletion, now time.Time) error {
	result := completion.Result
	if validateBuildResult(result, completion.CacheReference, completion.LogReference) != nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, ok := s.attempts[attemptID]
	if !ok {
		return ErrNotFound
	}
	if attempt.LeaseOwner != owner || !attempt.LeaseUntil.After(now.UTC()) || attempt.State != AttemptRunning || attempt.CancelRequestedAt != nil || result.OperationID != attempt.ID || result.Generation != attempt.Generation ||
		result.Image.Reference != attempt.PlanRequest.Build.Destination.Repository+"@"+result.Image.Digest || !slicesEqual(result.Image.Platforms, attempt.PlanRequest.Build.Platforms) ||
		(completion.CacheReference != "" && completion.CacheReference != cacheReference(attempt)) {
		return ErrLeaseLost
	}
	completed := now.UTC()
	attempt.State, attempt.Result, attempt.CacheReference, attempt.LogReference = AttemptSucceeded, &result, completion.CacheReference, completion.LogReference
	attempt.FailureCode, attempt.CompletedAt, attempt.UpdatedAt = "", &completed, completed
	attempt.LeaseOwner, attempt.LeaseUntil = "", time.Time{}
	s.attempts[attempt.ID] = attempt
	if _, exists := s.releaseProjections[attempt.ID]; !exists {
		s.releaseProjections[attempt.ID] = memoryReleaseProjection{
			state: ReleaseProjectionPending, availableAt: completed,
			createdAt: completed, updatedAt: completed,
		}
	}
	return nil
}

func (s *MemoryStore) RequestCancel(_ context.Context, attemptID string, now time.Time) (BuildAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, ok := s.attempts[attemptID]
	if !ok {
		return BuildAttempt{}, ErrNotFound
	}
	if terminalAttempt(attempt.State) {
		return cloneAttempt(attempt), nil
	}
	requested := now.UTC()
	attempt.CancelRequestedAt, attempt.UpdatedAt = &requested, requested
	if attempt.State == AttemptQueued && attempt.LeaseOwner == "" {
		attempt.State, attempt.CompletedAt = AttemptCancelled, &requested
	} else {
		attempt.State, attempt.AvailableAt = AttemptCancelling, requested
	}
	s.attempts[attempt.ID] = attempt
	return cloneAttempt(attempt), nil
}

func (s *MemoryStore) CompleteCancellation(_ context.Context, attemptID, owner string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt, ok := s.attempts[attemptID]
	if !ok {
		return ErrNotFound
	}
	if attempt.State != AttemptCancelling || attempt.LeaseOwner != owner || !attempt.LeaseUntil.After(now.UTC()) {
		return ErrLeaseLost
	}
	completed := now.UTC()
	attempt.State, attempt.CompletedAt, attempt.UpdatedAt = AttemptCancelled, &completed, completed
	attempt.FailureCode = ""
	attempt.LeaseOwner, attempt.LeaseUntil = "", time.Time{}
	s.attempts[attempt.ID] = attempt
	return nil
}

func (s *MemoryStore) PendingOutbox(_ context.Context, limit int) ([]OutboxMessage, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]OutboxMessage, 0)
	for _, message := range s.outbox {
		if message.PublishedAt == nil {
			result = append(result, message)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].AvailableAt.Equal(result[j].AvailableAt) {
			return result[i].AttemptID < result[j].AttemptID
		}
		return result[i].AvailableAt.Before(result[j].AvailableAt)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *MemoryStore) MarkOutboxPublished(_ context.Context, attemptID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	message, ok := s.outbox[attemptID]
	if !ok {
		return ErrNotFound
	}
	if message.PublishedAt == nil {
		published := at.UTC()
		message.PublishedAt = &published
		s.outbox[attemptID] = message
	}
	return nil
}

func (s *MemoryStore) attemptsForDeliveryLocked(claimKey string) []BuildAttempt {
	result := make([]BuildAttempt, 0)
	for _, attempt := range s.attempts {
		if attempt.DeliveryClaimKey == claimKey {
			result = append(result, cloneAttempt(attempt))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func validateClaim(claim githubapp.OneTimeClaim) error {
	if (claim.Kind != "github-state" && claim.Kind != "github-delivery") || !regexpHex64(claim.ClaimKey) || claim.RetainUntil.IsZero() || claim.Kind == "github-delivery" && !claim.Permanent || claim.Kind == "github-state" && claim.Permanent {
		return ErrInvalid
	}
	return nil
}

func validateReceipt(receipt DeliveryReceipt) error {
	if !regexpHex64(receipt.ClaimKey) || receipt.AppID <= 0 || receipt.GitHubInstallationID <= 0 || receipt.DeliveryID == "" || !digestRE.MatchString(receipt.BodySHA256) || len(receipt.TypedEvent) == 0 || len(receipt.TypedEvent) > 256<<10 || receipt.ReceivedAt.IsZero() || receipt.AvailableAt.IsZero() || receipt.State != DeliveryClaimed {
		return ErrInvalid
	}
	if receipt.Event == "push" {
		if receipt.RepositoryID <= 0 || !validGitRef(receipt.GitRef) {
			return ErrInvalid
		}
	} else if receipt.Event != "installation" && receipt.Event != "installation_repositories" || receipt.RepositoryID != 0 || receipt.GitRef != "" {
		return ErrInvalid
	}
	event, err := decodeTypedEvent(receipt.Event, receipt.TypedEvent)
	if err != nil {
		return ErrInvalid
	}
	installationID, ok := githubapp.EventInstallationID(event)
	if !ok || installationID != receipt.GitHubInstallationID {
		return ErrInvalid
	}
	if push, ok := event.(githubapp.PushEvent); ok && (push.Repository.ID != receipt.RepositoryID || push.Ref != receipt.GitRef) {
		return ErrInvalid
	}
	return nil
}

func sameReceiptIdentity(a, b DeliveryReceipt) bool {
	return a.ClaimKey == b.ClaimKey && a.AppID == b.AppID && a.GitHubInstallationID == b.GitHubInstallationID && a.DeliveryID == b.DeliveryID && a.Event == b.Event && a.BodySHA256 == b.BodySHA256 && a.RepositoryID == b.RepositoryID && a.GitRef == b.GitRef
}

func sameDefinition(a, b BuildDefinition) bool {
	return a.ID == b.ID && a.ProjectID == b.ProjectID && a.ServiceID == b.ServiceID && a.InstallationID == b.InstallationID &&
		a.RepositoryID == b.RepositoryID && a.TriggerRef == b.TriggerRef && a.Enabled == b.Enabled && a.DefinitionDigest == b.DefinitionDigest &&
		a.DefinitionGeneration == b.DefinitionGeneration && a.UpdatedAt.Equal(b.UpdatedAt)
}
func validOwnerLease(owner string, duration time.Duration) bool {
	return owner != "" && len(owner) <= 128 && !strings.ContainsAny(owner, "\x00\r\n") && duration >= 5*time.Second && duration <= 5*time.Minute
}
func regexpHex64(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}
func clonePermissions(input githubapp.Permissions) githubapp.Permissions {
	result := make(githubapp.Permissions, len(input))
	for k, v := range input {
		result[k] = v
	}
	return result
}
func cloneInstallation(input Installation) Installation {
	input.Permissions = clonePermissions(input.Permissions)
	input.SuspendedAt = cloneTimePointer(input.SuspendedAt)
	input.DeletedAt = cloneTimePointer(input.DeletedAt)
	return input
}
func cloneReceipt(input DeliveryReceipt) DeliveryReceipt {
	input.TypedEvent = append([]byte(nil), input.TypedEvent...)
	input.CompletedAt = cloneTimePointer(input.CompletedAt)
	return input
}
func cloneRepository(input Repository) Repository {
	input.RemovedAt = cloneTimePointer(input.RemovedAt)
	return input
}
func cloneTimePointer(input *time.Time) *time.Time {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}
func cloneDefinition(input BuildDefinition) BuildDefinition { return cloneJSON(input) }
func cloneAttempt(input BuildAttempt) BuildAttempt          { return cloneJSON(input) }
func cloneAttempts(input []BuildAttempt) []BuildAttempt {
	result := make([]BuildAttempt, len(input))
	for i := range input {
		result[i] = cloneAttempt(input[i])
	}
	return result
}
func cloneJSON[T any](input T) T {
	encoded, _ := json.Marshal(input)
	var output T
	_ = json.Unmarshal(encoded, &output)
	return output
}
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
