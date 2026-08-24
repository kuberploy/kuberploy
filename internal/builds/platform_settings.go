package builds

import (
	"context"
	"errors"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
)

const MaximumConcurrentBuilders = 20

type BuilderPlatformSettings struct {
	Revision              int64                      `json:"revision"`
	NodeIsolation         bool                       `json:"nodeIsolation"`
	MaxConcurrentBuilders int                        `json:"maxConcurrentBuilders"`
	CheckoutResources     builder.ContainerResources `json:"checkoutResources"`
	DinDResources         builder.ContainerResources `json:"dindResources"`
	AgentResources        builder.ContainerResources `json:"agentResources"`
	UpdatedBy             string                     `json:"updatedBy,omitempty"`
	UpdatedAt             time.Time                  `json:"updatedAt,omitempty"`
}

type BuilderPlatformSettingsInput struct {
	NodeIsolation         bool                       `json:"nodeIsolation"`
	MaxConcurrentBuilders int                        `json:"maxConcurrentBuilders"`
	CheckoutResources     builder.ContainerResources `json:"checkoutResources"`
	DinDResources         builder.ContainerResources `json:"dindResources"`
	AgentResources        builder.ContainerResources `json:"agentResources"`
}

func DefaultBuilderPlatformSettings(config WorkerRuntimeConfig) BuilderPlatformSettings {
	execution := config.executionTemplate()
	return BuilderPlatformSettings{
		NodeIsolation:         config.NodeIsolation,
		MaxConcurrentBuilders: 1,
		CheckoutResources:     execution.CheckoutResources,
		DinDResources:         execution.DinDResources,
		AgentResources:        execution.AgentResources,
	}
}

func (s BuilderPlatformSettings) Validate() error {
	if s.Revision < 0 || s.MaxConcurrentBuilders < 1 || s.MaxConcurrentBuilders > MaximumConcurrentBuilders ||
		builder.ValidateContainerResources(s.CheckoutResources) != nil ||
		builder.ValidateContainerResources(s.DinDResources) != nil ||
		builder.ValidateContainerResources(s.AgentResources) != nil {
		return ErrInvalid
	}
	return nil
}

func (s BuilderPlatformSettings) Input() BuilderPlatformSettingsInput {
	return BuilderPlatformSettingsInput{NodeIsolation: s.NodeIsolation, MaxConcurrentBuilders: s.MaxConcurrentBuilders,
		CheckoutResources: s.CheckoutResources, DinDResources: s.DinDResources, AgentResources: s.AgentResources}
}

func (i BuilderPlatformSettingsInput) settings() BuilderPlatformSettings {
	return BuilderPlatformSettings{NodeIsolation: i.NodeIsolation, MaxConcurrentBuilders: i.MaxConcurrentBuilders,
		CheckoutResources: i.CheckoutResources, DinDResources: i.DinDResources, AgentResources: i.AgentResources}
}

type BuilderPlatformSettingsStore interface {
	LatestBuilderPlatformSettings(context.Context) (BuilderPlatformSettings, error)
	UpdateBuilderPlatformSettings(context.Context, string, string, string, int64, BuilderPlatformSettingsInput, time.Time) (BuilderPlatformSettings, bool, error)
}

type BuilderPlatformSettingsService struct {
	Store    BuilderPlatformSettingsStore
	Defaults BuilderPlatformSettings
	Now      func() time.Time
}

func (s *BuilderPlatformSettingsService) Current(ctx context.Context) (BuilderPlatformSettings, error) {
	if s == nil || s.Store == nil || s.Defaults.Validate() != nil {
		return BuilderPlatformSettings{}, ErrInvalid
	}
	settings, err := s.Store.LatestBuilderPlatformSettings(ctx)
	if errors.Is(err, ErrNotFound) {
		return s.Defaults, nil
	}
	return settings, err
}

func (s *BuilderPlatformSettingsService) Update(ctx context.Context, actorID, idempotencyKey, fingerprint string, expectedRevision int64, input BuilderPlatformSettingsInput) (BuilderPlatformSettings, bool, error) {
	if s == nil || s.Store == nil || s.Defaults.Validate() != nil || input.settings().Validate() != nil {
		return BuilderPlatformSettings{}, false, ErrInvalid
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	return s.Store.UpdateBuilderPlatformSettings(ctx, actorID, idempotencyKey, fingerprint, expectedRevision, input, now)
}

type BuilderPlatformSettingsReader interface {
	Current(context.Context) (BuilderPlatformSettings, error)
}
