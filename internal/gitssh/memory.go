package gitssh

import (
	"context"
	"sync"
)

type keyRecord struct {
	metadata KeyMetadata
	envelope PrivateKeyEnvelope
}

// MemoryRepository provides atomic lifecycle storage for tests and local use.
// It intentionally exposes only public metadata.
type MemoryRepository struct {
	mu      sync.RWMutex
	records map[string][]keyRecord
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{records: make(map[string][]keyRecord)}
}

func repositoryKey(scope Scope, ownerID string) string {
	return string(scope) + "\x00" + ownerID
}

func (r *MemoryRepository) create(ctx context.Context, record keyRecord) (KeyMetadata, error) {
	if err := ctx.Err(); err != nil {
		return KeyMetadata{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	key := repositoryKey(record.metadata.Scope, record.metadata.OwnerID)
	for _, existing := range r.records[key] {
		if existing.metadata.Status == StatusActive {
			return KeyMetadata{}, ErrActiveKeyExists
		}
	}
	record.metadata.Revision = uint64(len(r.records[key]) + 1)
	record.metadata.Status = StatusActive
	record.envelope = cloneEnvelope(record.envelope)
	r.records[key] = append(r.records[key], record)
	return record.metadata, nil
}

func (r *MemoryRepository) rotate(ctx context.Context, record keyRecord) (KeyMetadata, error) {
	if err := ctx.Err(); err != nil {
		return KeyMetadata{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	key := repositoryKey(record.metadata.Scope, record.metadata.OwnerID)
	records := r.records[key]
	activeIndex := -1
	for index := range records {
		if records[index].metadata.Status == StatusActive {
			activeIndex = index
		}
	}
	if activeIndex < 0 {
		return KeyMetadata{}, ErrActiveKeyNotFound
	}
	records[activeIndex].metadata.Status = StatusRevoked
	record.metadata.Revision = uint64(len(records) + 1)
	record.metadata.Status = StatusActive
	record.envelope = cloneEnvelope(record.envelope)
	r.records[key] = append(records, record)
	return record.metadata, nil
}

func (r *MemoryRepository) revoke(ctx context.Context, scope Scope, ownerID string) (KeyMetadata, error) {
	if err := ctx.Err(); err != nil {
		return KeyMetadata{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	key := repositoryKey(scope, ownerID)
	records := r.records[key]
	for index := len(records) - 1; index >= 0; index-- {
		if records[index].metadata.Status == StatusActive {
			records[index].metadata.Status = StatusRevoked
			r.records[key] = records
			return records[index].metadata, nil
		}
	}
	return KeyMetadata{}, ErrActiveKeyNotFound
}

func (r *MemoryRepository) active(ctx context.Context, scope Scope, ownerID string) (keyRecord, error) {
	if err := validateIdentity(scope, ownerID); err != nil {
		return keyRecord{}, err
	}
	if err := ctx.Err(); err != nil {
		return keyRecord{}, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	for index := len(r.records[repositoryKey(scope, ownerID)]) - 1; index >= 0; index-- {
		record := r.records[repositoryKey(scope, ownerID)][index]
		if record.metadata.Status == StatusActive {
			record.envelope = cloneEnvelope(record.envelope)
			return record, nil
		}
	}
	return keyRecord{}, ErrActiveKeyNotFound
}

func (r *MemoryRepository) List(ctx context.Context, scope Scope, ownerID string) ([]KeyMetadata, error) {
	if err := validateIdentity(scope, ownerID); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	records := r.records[repositoryKey(scope, ownerID)]
	metadata := make([]KeyMetadata, 0, len(records))
	for _, record := range records {
		metadata = append(metadata, record.metadata)
	}
	return metadata, nil
}
