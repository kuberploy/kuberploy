package builds

import (
	"context"
	"strings"
	"time"
)

type memoryBuilderSettingMutation struct {
	Fingerprint string
	Revision    int64
}

func (s *MemoryStore) LatestBuilderPlatformSettings(_ context.Context) (BuilderPlatformSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.builderSettings) == 0 {
		return BuilderPlatformSettings{}, ErrNotFound
	}
	return s.builderSettings[len(s.builderSettings)-1], nil
}

func (s *MemoryStore) UpdateBuilderPlatformSettings(_ context.Context, actorID, idempotencyKey, fingerprint string, expectedRevision int64, input BuilderPlatformSettingsInput, now time.Time) (BuilderPlatformSettings, bool, error) {
	if !uuidRE.MatchString(actorID) || strings.TrimSpace(idempotencyKey) != idempotencyKey || idempotencyKey == "" || len(idempotencyKey) > 128 ||
		!digestRE.MatchString(fingerprint) || expectedRevision < 0 || now.IsZero() || input.settings().Validate() != nil {
		return BuilderPlatformSettings{}, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	receiptKey := actorID + "\x00" + idempotencyKey
	if receipt, ok := s.builderSettingMutations[receiptKey]; ok {
		if receipt.Fingerprint != fingerprint || receipt.Revision < 1 || receipt.Revision > int64(len(s.builderSettings)) {
			return BuilderPlatformSettings{}, false, ErrConflict
		}
		return s.builderSettings[receipt.Revision-1], true, nil
	}
	currentRevision := int64(len(s.builderSettings))
	if expectedRevision != currentRevision {
		return BuilderPlatformSettings{}, false, ErrConflict
	}
	settings := input.settings()
	settings.Revision = currentRevision + 1
	settings.UpdatedBy = actorID
	settings.UpdatedAt = now.UTC()
	s.builderSettings = append(s.builderSettings, settings)
	s.builderSettingMutations[receiptKey] = memoryBuilderSettingMutation{Fingerprint: fingerprint, Revision: settings.Revision}
	return settings, false, nil
}
