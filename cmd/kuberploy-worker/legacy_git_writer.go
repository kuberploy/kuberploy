package main

import (
	"context"

	"github.com/kuberploy/kuberploy/internal/domain"
	"github.com/kuberploy/kuberploy/internal/gitops"
)

// legacyGitOperationWriter preserves the migration-only direct Git writer
// contract while returning the same closed result shape as projection mode.
// It can never synthesize a pull-request publication.
type legacyGitOperationWriter struct {
	writer *gitops.Writer
}

func (w legacyGitOperationWriter) Write(ctx context.Context, operation domain.Operation, project domain.Project, environment domain.Environment, application domain.Application, deployment domain.Deployment) (domain.GitPublicationResult, error) {
	revision, err := w.writer.Write(ctx, operation, project, environment, application, deployment)
	if err != nil {
		return domain.GitPublicationResult{}, err
	}
	return domain.GitPublicationResult{Mode: "direct", Revision: revision}, nil
}
